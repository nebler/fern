package runapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nebler/fern/internal/backgroundroute"
	"github.com/nebler/fern/internal/gitref"
	"github.com/nebler/fern/internal/pluginauth"
	runidentity "github.com/nebler/fern/internal/run"
	"github.com/nebler/fern/internal/strictjson"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	PathPrefix             = "/fern/api/runs"
	PluginOpenCodeProfile  = taskstore.BackgroundRunSourceProfile
	maxCreateBodyBytes     = 32 << 10
	maxEmptyBodyBytes      = 16
	backgroundRunListLimit = 100
)

type Store interface {
	AdmitBackgroundRun(context.Context, taskstore.AdmitBackgroundRunParams) (taskstore.Admission, error)
	FindReceiptByIdempotency(context.Context, task.WorkspaceID, string, task.IdempotencyKey) (taskstore.Receipt, bool, error)
	GetBackgroundRun(context.Context, task.WorkspaceID, task.RunID, task.ActorSnapshot) (taskstore.BackgroundRun, error)
	StopBackgroundRun(context.Context, taskstore.StopBackgroundRunParams) (taskstore.BackgroundRunStop, error)
	SealBackgroundRun(context.Context, taskstore.SealBackgroundRunParams) (taskstore.BackgroundRunSealAdmission, error)
	ListBackgroundRuns(context.Context, task.WorkspaceID, task.ActorSnapshot, int) ([]taskstore.BackgroundRun, error)
	GetBackgroundRunResult(context.Context, task.WorkspaceID, task.RunID, task.ActorSnapshot) (taskstore.BackgroundRunResultProjection, error)
}

var _ Store = (*taskstore.Store)(nil)

// Route issues short-lived OpenCode attachment credentials for live runs.
type Route interface {
	IssueAttachment(taskstore.BackgroundRun) (backgroundroute.Attachment, bool, error)
	ActiveOrigin(taskstore.BackgroundRun) (string, bool)
}

type ActorResolver func(context.Context) (task.ActorSnapshot, error)

type RetentionVerifier interface {
	Verify(context.Context, taskstore.BackgroundRunResultProjection) error
}

type Config struct {
	WorkspaceID                 task.WorkspaceID
	RepositoryID                task.RepositoryID
	RepositoryRemote            string
	BackgroundImageIdentity     string
	BackgroundEnvironmentSHA256 [32]byte
	AvailableProfile            string
	Store                       Store
	Route                       Route
	Generator                   *task.Generator
	ActorResolver               ActorResolver
	BaseVerifier                BaseVerifier
	Now                         func() time.Time
	AttemptTimeout              time.Duration
	Agent                       string
	ModelProvider               string
	Model                       string
	Wake                        func()
	RetentionVerifier           RetentionVerifier
	SealPolicyVersion           string
}

type Handler struct {
	config   Config
	commands *service
	// retained caches successful retention verifications for the process
	// lifetime. Retained artifacts are content-addressed and immutable, so a
	// verified (result, bundle digest) tuple stays verified; failures
	// are never cached.
	retained sync.Map // map[retainedKey]struct{}
}

type retainedKey struct {
	result task.ResultID
	bundle [32]byte
}

func New(config Config) (*Handler, error) {
	if config.Store == nil || config.Route == nil || config.Generator == nil || config.ActorResolver == nil || config.BaseVerifier == nil || config.RetentionVerifier == nil || config.Now == nil ||
		config.AttemptTimeout <= 0 || config.RepositoryID == 0 || config.RepositoryRemote == "" ||
		config.Agent == "" || config.ModelProvider == "" || config.Model == "" || config.BackgroundEnvironmentSHA256 == ([32]byte{}) {
		return nil, errors.New("valid background run API configuration is required")
	}
	if !validText(config.SealPolicyVersion, 1, 128) {
		return nil, errors.New("valid background seal policy is required")
	}
	if _, err := task.ParseWorkspaceID(string(config.WorkspaceID)); err != nil {
		return nil, errors.New("valid background run workspace is required")
	}
	if gitref.ValidateGitHubRemote(config.RepositoryRemote) != nil {
		return nil, errors.New("canonical background run repository remote is required")
	}
	if (config.AvailableProfile == PluginOpenCodeProfile) != (config.BackgroundImageIdentity != "") ||
		(config.AvailableProfile != "" && config.AvailableProfile != PluginOpenCodeProfile) {
		return nil, errors.New("qualified background image and profile must be configured together")
	}
	return &Handler{config: config, commands: &service{config: config}}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.EscapedPath() != r.URL.Path {
		WriteError(w, http.StatusNotFound, "not_found", "The requested run was not found.")
		return
	}
	actor, ok := h.authorize(w, r)
	if !ok {
		return
	}
	if r.URL.Path == PathPrefix {
		switch r.Method {
		case http.MethodPost:
			if !h.requireScope(w, r, actor, "run:create") {
				return
			}
			h.create(w, r, actor)
		case http.MethodGet:
			if !h.requireScope(w, r, actor, "run:read") {
				return
			}
			h.list(w, r, actor)
		default:
			w.Header().Set("Allow", "GET, POST")
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "The method is not allowed for this resource.")
		}
		return
	}
	if !strings.HasPrefix(r.URL.Path, PathPrefix+"/") {
		WriteError(w, http.StatusNotFound, "not_found", "The requested run was not found.")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, PathPrefix+"/"), "/")
	id, err := task.ParseRunID(parts[0])
	if err != nil || len(parts) > 2 {
		WriteError(w, http.StatusNotFound, "not_found", "The requested run was not found.")
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		if !h.requireScope(w, r, actor, "run:read") {
			return
		}
		h.get(w, r, actor, id)
		return
	}
	switch parts[1] {
	case "attach":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		if !h.requireScope(w, r, actor, "run:attach") {
			return
		}
		h.attach(w, r, actor, id)
	case "stop":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if !h.requireScope(w, r, actor, "run:stop") {
			return
		}
		h.stop(w, r, actor, id)
	case "result":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		if !h.requireScope(w, r, actor, "run:result") {
			return
		}
		h.result(w, r, actor, id)
	case "seal":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if !h.requireScope(w, r, actor, "run:result") {
			return
		}
		h.seal(w, r, actor, id)
	default:
		WriteError(w, http.StatusNotFound, "not_found", "The requested run was not found.")
	}
}

type sealProjection struct {
	RunID       task.RunID        `json:"run_id"`
	State       runidentity.State `json:"state"`
	ResultPhase string            `json:"result_phase"`
	ResultID    task.ResultID     `json:"result_id"`
	Committed   bool              `json:"committed"`
}

func (h *Handler) seal(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.RunID) {
	if !validateEmptyMutation(w, r) {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	admission, err := h.commands.Seal(r.Context(), actor, key, id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if admission.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	WriteJSON(w, http.StatusAccepted, sealProjection{admission.RunID, admission.State, admission.ResultPhase, admission.ResultID, admission.Committed})
}

type resultResponse struct {
	RunID     task.RunID               `json:"run_id"`
	State     string                   `json:"state"`
	Result    retainedResultResponse   `json:"result"`
	Artifact  retainedArtifactResponse `json:"artifact"`
	Retention retentionResponse        `json:"retention"`
	Cleanup   cleanupResponse          `json:"cleanup"`
}

type retainedResultResponse struct {
	ID         task.ResultID      `json:"id"`
	Outcome    task.ResultOutcome `json:"outcome"`
	Repository string             `json:"repository"`
	Base       task.GitOID        `json:"base_oid"`
	Commit     task.GitOID        `json:"result_commit"`
	Tree       task.GitOID        `json:"tree_oid"`
	Entries    int                `json:"manifest_entries"`
	Manifest   string             `json:"manifest_sha256"`
}

type retainedArtifactResponse struct {
	Format     string `json:"format"`
	SHA        string `json:"sha256"`
	BundleSHA  string `json:"bundle_sha256"`
	BundleSize int64  `json:"bundle_size"`
	Manifest   string `json:"manifest_sha256"`
}

type retentionResponse struct {
	Verified        bool `json:"verified"`
	Reconstructable bool `json:"reconstructable"`
}

type cleanupResponse struct {
	Complete bool `json:"complete"`
}

func (h *Handler) result(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.RunID) {
	if !noQuery(r) || !noBody(r) {
		WriteError(w, http.StatusBadRequest, "invalid_query", "This run operation does not accept query parameters.")
		return
	}
	run, err := h.config.Store.GetBackgroundRun(r.Context(), h.config.WorkspaceID, id, actor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if run.State != taskstore.BackgroundRunResultReady {
		// A sealing run records its last failed export pass as last_error.
		if run.EffectPhase == taskstore.BackgroundRunEffectSealing && run.LastError != "" {
			WriteError(w, http.StatusServiceUnavailable, "recovery_required", "The retained result requires recovery.")
			return
		}
		WriteError(w, http.StatusConflict, "not_ready", "The retained result is not ready.")
		return
	}
	projection, err := h.config.Store.GetBackgroundRunResult(r.Context(), h.config.WorkspaceID, id, actor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	retained := h.verifyRetained(r.Context(), projection)
	digest := func(value [32]byte) string { return hex.EncodeToString(value[:]) }
	WriteJSON(w, http.StatusOK, resultResponse{RunID: id, State: "result_ready",
		Result: retainedResultResponse{
			projection.Result.ID, projection.Result.Outcome, run.RepositoryRemote, projection.Result.BaseSHA, projection.Result.ResultCommit, projection.Result.TreeOID, projection.Result.ChangeCount, digest(projection.Result.ChangesSHA256)},
		Artifact: retainedArtifactResponse{
			"git_bundle_v1", digest(projection.Result.ManifestSHA256), digest(projection.Result.BundleSHA256), projection.Result.BundleBytes, digest(projection.Result.ManifestSHA256)},
		Retention: retentionResponse{retained, retained},
		Cleanup:   cleanupResponse{run.EffectPhase == taskstore.BackgroundRunEffectCleanupComplete},
	})
}

// verifyRetained runs the full retention verification (bundle copy, unbundle,
// fsck) once per immutable retained tuple instead of on every result read.
func (h *Handler) verifyRetained(ctx context.Context, projection taskstore.BackgroundRunResultProjection) bool {
	key := retainedKey{result: projection.Result.ID, bundle: projection.Result.BundleSHA256}
	if _, ok := h.retained.Load(key); ok {
		return true
	}
	if h.config.RetentionVerifier.Verify(ctx, projection) != nil {
		return false
	}
	h.retained.Store(key, struct{}{})
	return true
}

// authorize accepts the ingress-authenticated actor: the loopback operator, or
// an OpenCode plugin whose identity matches its bearer authorization.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) (task.ActorSnapshot, bool) {
	authorization, exists := pluginauth.RequestAuthorizationFromContext(r.Context())
	actor, err := h.config.ActorResolver(r.Context())
	if err == nil && actor.Type == task.ActorOperator && actor.Validate() == nil {
		return actor, true
	}
	if err != nil || actor.Validate() != nil || !exists || actor.Type != task.ActorOpenCode || actor.ID != authorization.Credential.ID ||
		actor.CredentialID != authorization.Credential.ID || actor.Authentication != "fern_plugin_bearer" {
		WriteError(w, http.StatusUnauthorized, "unauthenticated", "Plugin authentication is required.")
		return task.ActorSnapshot{}, false
	}
	return actor, true
}

// operatorScopes are the operations the operator may perform without a plugin
// credential: workspace-wide discovery and attachment, never run mutation.
var operatorScopes = map[string]bool{"run:read": true, "run:attach": true}

func (h *Handler) requireScope(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, scope string) bool {
	if actor.Type == task.ActorOperator {
		if !operatorScopes[scope] {
			WriteError(w, http.StatusForbidden, "forbidden", "The operator cannot perform this run operation.")
			return false
		}
		return true
	}
	authorization, ok := pluginauth.RequestAuthorizationFromContext(r.Context())
	if actor.Type != task.ActorOpenCode || !ok || actor.ID != authorization.Credential.ID || !authorization.HasScope(scope) {
		WriteError(w, http.StatusForbidden, "forbidden", "The plugin credential lacks the required scope.")
		return false
	}
	return true
}

type createInput struct {
	Repository  string  `json:"repository"`
	BaseOID     string  `json:"base_oid"`
	Branch      *string `json:"branch"`
	Instruction string  `json:"instruction"`
	Profile     string  `json:"profile"`
}

type createResponse struct {
	RunID     task.RunID `json:"run_id"`
	Committed bool       `json:"committed"`
}

type stopResponse struct {
	RunID task.RunID        `json:"run_id"`
	State runidentity.State `json:"state"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot) {
	if !noQuery(r) || !exactJSON(r) {
		WriteError(w, http.StatusBadRequest, "invalid_request", "The request is not valid.")
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var input createInput
	if !decodeStrict(w, r, maxCreateBodyBytes, &input) {
		return
	}
	admission, err := h.commands.Create(r.Context(), actor, key, createIntent{
		Repository: input.Repository, BaseOID: input.BaseOID, Branch: input.Branch,
		Instruction: input.Instruction, Profile: input.Profile,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if admission.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	WriteJSON(w, http.StatusAccepted, createResponse{admission.RunID, admission.Committed})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot) {
	if !noQuery(r) || !noBody(r) {
		WriteError(w, http.StatusBadRequest, "invalid_query", "Run listing does not accept query parameters.")
		return
	}
	runs, err := h.config.Store.ListBackgroundRuns(r.Context(), h.config.WorkspaceID, actor, backgroundRunListLimit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]runView, 0, len(runs))
	for _, run := range runs {
		views = append(views, h.view(run))
	}
	WriteJSON(w, http.StatusOK, struct {
		Runs []runView `json:"runs"`
	}{views})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.RunID) {
	if !noQuery(r) || !noBody(r) {
		WriteError(w, http.StatusBadRequest, "invalid_query", "Run reads do not accept query parameters.")
		return
	}
	run, err := h.config.Store.GetBackgroundRun(r.Context(), h.config.WorkspaceID, id, actor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, h.view(run))
}

type attachResponse struct {
	RunID     task.RunID             `json:"run_id"`
	URL       string                 `json:"url"`
	SessionID task.OpenCodeSessionID `json:"session_id"`
	Username  string                 `json:"username"`
	Password  string                 `json:"password"`
	ExpiresAt time.Time              `json:"expires_at"`
}

// attach mints a short-lived OpenCode credential. Durable readiness alone
// never grants access: the route manager must also issue it, and it owns
// expiry and runtime fencing. The response carries a secret and is no-store.
func (h *Handler) attach(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.RunID) {
	if !noQuery(r) || !noBody(r) {
		WriteError(w, http.StatusBadRequest, "invalid_query", "Run attachment does not accept query parameters.")
		return
	}
	run, err := h.config.Store.GetBackgroundRun(r.Context(), h.config.WorkspaceID, id, actor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !attachmentReady(run) {
		WriteError(w, http.StatusConflict, "not_ready", "The OpenCode session is not ready for attachment.")
		return
	}
	attachment, issued, err := h.config.Route.IssueAttachment(run)
	if err != nil {
		WriteError(w, http.StatusServiceUnavailable, "unavailable", "The OpenCode attachment could not be issued.")
		return
	}
	if !issued {
		WriteError(w, http.StatusConflict, "not_ready", "The OpenCode session is not ready for attachment.")
		return
	}
	WriteJSON(w, http.StatusOK, attachResponse{RunID: run.RunID, URL: attachment.Origin, SessionID: run.OpenCodeSessionID,
		Username: attachment.Username, Password: attachment.Password, ExpiresAt: attachment.ExpiresAt})
}

func attachmentReady(run taskstore.BackgroundRun) bool {
	active := run.State == taskstore.BackgroundRunSettingUp || run.State == taskstore.BackgroundRunWorking ||
		run.State == taskstore.BackgroundRunNeedsYou || run.State == taskstore.BackgroundRunUncertain
	// Provisioning reconciles the session before the prompt fence ends it.
	ready := run.EffectPhase == taskstore.BackgroundRunEffectPromptPending || run.EffectPhase == taskstore.BackgroundRunEffectAdmitted
	return active && ready && run.StopReceiptID == 0
}

func (h *Handler) stop(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.RunID) {
	if !validateEmptyMutation(w, r) {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	result, err := h.commands.Stop(r.Context(), actor, key, id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	WriteJSON(w, http.StatusAccepted, stopResponse{result.RunID, result.State})
}

type runView struct {
	ID         task.RunID                   `json:"id"`
	State      taskstore.BackgroundRunState `json:"state"`
	Repository string                       `json:"repository"`
	Head       task.GitOID                  `json:"head"`
	Branch     *string                      `json:"branch"`
	// Attachable is advisory, not a reservation; attach re-checks readiness.
	Attachable bool `json:"attachable"`
}

func (h *Handler) view(run taskstore.BackgroundRun) runView {
	_, active := h.config.Route.ActiveOrigin(run)
	return runView{run.RunID, run.State, run.RepositoryRemote, run.BaseOID, run.Branch, active && attachmentReady(run)}
}
func decodeStrict(w http.ResponseWriter, r *http.Request, limit int64, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	payload, err := io.ReadAll(r.Body)
	if err != nil || strictjson.Check(payload, 3) != nil {
		WriteError(w, http.StatusBadRequest, "invalid_json", "The JSON body is not valid.")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_json", "The JSON body is not valid.")
		return false
	}
	return true
}
func idempotencyKey(w http.ResponseWriter, r *http.Request) (task.IdempotencyKey, bool) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 {
		WriteError(w, http.StatusBadRequest, "invalid_idempotency_key", "A valid Idempotency-Key is required.")
		return "", false
	}
	key, err := task.ParseIdempotencyKey(values[0])
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_idempotency_key", "A valid Idempotency-Key is required.")
		return "", false
	}
	return key, true
}
func exactJSON(r *http.Request) bool {
	return len(r.Header.Values("Content-Type")) == 1 && r.Header.Get("Content-Type") == "application/json"
}
func noQuery(r *http.Request) bool { return r.URL.RawQuery == "" }
func noBody(r *http.Request) bool  { return r.Body == nil || r.ContentLength == 0 }
func validText(value string, min, max int) bool {
	if len(value) < min || len(value) > max || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}
func validateEmptyMutation(w http.ResponseWriter, r *http.Request) bool {
	if !noQuery(r) {
		WriteError(w, http.StatusBadRequest, "invalid_request", "This run operation does not accept query parameters.")
		return false
	}
	if !exactJSON(r) {
		WriteError(w, http.StatusBadRequest, "invalid_request", "Content-Type must be application/json.")
		return false
	}
	var value struct{}
	r.Body = http.MaxBytesReader(w, r.Body, maxEmptyBodyBytes)
	payload, err := io.ReadAll(r.Body)
	if err != nil || len(bytes.TrimSpace(payload)) < 2 || bytes.TrimSpace(payload)[0] != '{' || strictjson.Check(payload, 3) != nil {
		WriteError(w, http.StatusBadRequest, "invalid_json", "The JSON body must be an empty object.")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_json", "The JSON body must be an empty object.")
		return false
	}
	return true
}
func methodNotAllowed(w http.ResponseWriter, method string) {
	w.Header().Set("Allow", method)
	WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "The method is not allowed for this resource.")
}
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInvalidCreate):
		WriteError(w, http.StatusBadRequest, "invalid_run", "Repository, base, branch, instruction, or profile is not valid for this Fern workspace.")
	case errors.Is(err, errInvalidBase):
		WriteError(w, http.StatusBadRequest, "invalid_base", "base_oid must be an exact lowercase SHA-1 commit identity.")
	case errors.Is(err, errProfileUnavailable):
		WriteError(w, http.StatusServiceUnavailable, "profile_unavailable", fmt.Sprintf("Profile %s is unavailable: no background image qualified for it is configured.", PluginOpenCodeProfile))
	case errors.Is(err, errBaseUnavailable):
		WriteError(w, http.StatusUnprocessableEntity, "base_unavailable", "base_oid is not an exact commit reachable from an allowed configured-repository ref.")
	case errors.Is(err, errReplayConflict):
		WriteError(w, http.StatusConflict, "idempotency_conflict", "Idempotency-Key was already used for another request.")
	case errors.Is(err, taskstore.ErrNotFound):
		WriteError(w, http.StatusNotFound, "not_found", "The requested run was not found.")
	case errors.Is(err, taskstore.ErrIdempotencyConflict), errors.Is(err, taskstore.ErrInvalidState):
		WriteError(w, http.StatusConflict, "conflict", "The run command conflicts with durable state.")
	default:
		WriteError(w, http.StatusInternalServerError, "internal_error", "The run command could not be completed.")
	}
}

// WriteError writes Fern's JSON error envelope {"error":{"code","message"}}.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	type errorBody struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	WriteJSON(w, status, struct {
		Error errorBody `json:"error"`
	}{errorBody{code, message}})
}

// WriteJSON writes value as a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
