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

	"github.com/nebler/fern/internal/backgroundroute"
	"github.com/nebler/fern/internal/domain"
	"github.com/nebler/fern/internal/pluginauth"
	"github.com/nebler/fern/internal/safeio"
	"github.com/nebler/fern/internal/store"
)

const (
	PathPrefix             = "/fern/api/runs"
	maxCreateBodyBytes     = 32 << 10
	maxEmptyBodyBytes      = 16
	backgroundRunListLimit = 100
)

type Store interface {
	AdmitBackgroundRun(context.Context, store.AdmitBackgroundRunParams) (store.Admission, error)
	FindReceiptByIdempotency(context.Context, domain.WorkspaceID, string, domain.IdempotencyKey) (store.Receipt, bool, error)
	GetBackgroundRun(context.Context, domain.WorkspaceID, domain.RunID, domain.ActorSnapshot) (store.BackgroundRun, error)
	StopBackgroundRun(context.Context, store.StopBackgroundRunParams) (store.BackgroundRunStop, error)
	SealBackgroundRun(context.Context, store.SealBackgroundRunParams) (store.BackgroundRunSealAdmission, error)
	ListBackgroundRuns(context.Context, domain.WorkspaceID, domain.ActorSnapshot, int) ([]store.BackgroundRun, error)
	GetBackgroundRunResult(context.Context, domain.WorkspaceID, domain.RunID, domain.ActorSnapshot) (store.BackgroundRunResultProjection, error)
}

var _ Store = (*store.Store)(nil)

// Route issues short-lived OpenCode attachment credentials for live runs.
type Route interface {
	IssueAttachment(store.BackgroundRun) (backgroundroute.Attachment, bool, error)
	ActiveOrigin(store.BackgroundRun) (string, bool)
}

type ActorResolver func(context.Context) (domain.ActorSnapshot, error)

type RetentionVerifier interface {
	Verify(context.Context, store.BackgroundRunResultProjection) error
}

type Config struct {
	WorkspaceID                 domain.WorkspaceID
	RepositoryID                domain.RepositoryID
	RepositoryRemote            string
	BackgroundImageIdentity     string
	BackgroundEnvironmentSHA256 [32]byte
	Store                       Store
	Route                       Route
	Generator                   *domain.Generator
	ActorResolver               ActorResolver
	BaseVerifier                BaseVerifier
	Now                         func() time.Time
	RunTimeout                  time.Duration
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
	result domain.ResultID
	bundle [32]byte
}

func New(config Config) (*Handler, error) {
	_, workspaceErr := domain.ParseWorkspaceID(string(config.WorkspaceID))
	checks := []struct {
		field string
		ok    bool
	}{
		{"WorkspaceID", workspaceErr == nil},
		{"RepositoryID", config.RepositoryID != 0},
		{"RepositoryRemote", domain.ValidateGitHubRemote(config.RepositoryRemote) == nil},
		{"BackgroundImageIdentity", config.BackgroundImageIdentity != ""},
		{"BackgroundEnvironmentSHA256", config.BackgroundEnvironmentSHA256 != [32]byte{}},
		{"Store", config.Store != nil},
		{"Route", config.Route != nil},
		{"Generator", config.Generator != nil},
		{"ActorResolver", config.ActorResolver != nil},
		{"BaseVerifier", config.BaseVerifier != nil},
		{"RetentionVerifier", config.RetentionVerifier != nil},
		{"Now", config.Now != nil},
		{"RunTimeout", config.RunTimeout > 0},
		{"Agent", config.Agent != ""},
		{"ModelProvider", config.ModelProvider != ""},
		{"Model", config.Model != ""},
		{"SealPolicyVersion", config.SealPolicyVersion != ""},
	}
	for _, check := range checks {
		if !check.ok {
			return nil, fmt.Errorf("background run API configuration: %s is required", check.field)
		}
	}
	return &Handler{config: config, commands: &service{config: config}}, nil
}

// route is one operation on a run resource. Collection operations ignore the
// run ID.
type route struct {
	method string
	scope  string
	serve  func(*Handler, http.ResponseWriter, *http.Request, domain.ActorSnapshot, domain.RunID)
}

// routes maps a resource below PathPrefix to its operations: "" is the
// collection, "{id}" one run, and "{id}/<action>" an action on it.
var routes = map[string][]route{
	"":            {{http.MethodGet, "run:read", (*Handler).list}, {http.MethodPost, "run:create", (*Handler).create}},
	"{id}":        {{http.MethodGet, "run:read", (*Handler).get}},
	"{id}/attach": {{http.MethodGet, "run:attach", (*Handler).attach}},
	"{id}/stop":   {{http.MethodPost, "run:stop", (*Handler).stop}},
	"{id}/result": {{http.MethodGet, "run:result", (*Handler).result}},
	// Sealing turns the run's snapshot into its retained result, so it is
	// granted with reading that result rather than as a separate capability.
	"{id}/seal": {{http.MethodPost, "run:result", (*Handler).seal}},
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.EscapedPath() != r.URL.Path {
		writeNotFound(w)
		return
	}
	actor, authorization, ok := h.authorize(r)
	if !ok {
		WriteError(w, http.StatusUnauthorized, "unauthenticated", "Plugin authentication is required.")
		return
	}
	resource, id, ok := parseRunPath(r.URL.Path)
	operations, known := routes[resource]
	if !ok || !known {
		writeNotFound(w)
		return
	}
	allowed := make([]string, 0, len(operations))
	for _, operation := range operations {
		if operation.method == r.Method {
			if requireScope(w, actor, authorization, operation.scope) {
				operation.serve(h, w, r, actor, id)
			}
			return
		}
		allowed = append(allowed, operation.method)
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "The method is not allowed for this resource.")
}

// parseRunPath splits a request path into its routes key and run ID.
func parseRunPath(path string) (resource string, id domain.RunID, ok bool) {
	rest, ok := strings.CutPrefix(path, PathPrefix)
	if !ok || rest == "" {
		return "", "", ok
	}
	rest, ok = strings.CutPrefix(rest, "/")
	if !ok {
		return "", "", false
	}
	runID, action, hasAction := strings.Cut(rest, "/")
	id, err := domain.ParseRunID(runID)
	if err != nil {
		return "", "", false
	}
	if hasAction {
		return "{id}/" + action, id, true
	}
	return "{id}", id, true
}

func writeNotFound(w http.ResponseWriter) {
	WriteError(w, http.StatusNotFound, "not_found", "The requested run was not found.")
}

type sealProjection struct {
	RunID       domain.RunID    `json:"run_id"`
	State       domain.State    `json:"state"`
	ResultPhase string          `json:"result_phase"`
	ResultID    domain.ResultID `json:"result_id"`
	Committed   bool            `json:"committed"`
}

func (h *Handler) seal(w http.ResponseWriter, r *http.Request, actor domain.ActorSnapshot, id domain.RunID) {
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
	RunID     domain.RunID             `json:"run_id"`
	State     string                   `json:"state"`
	Result    retainedResultResponse   `json:"result"`
	Artifact  retainedArtifactResponse `json:"artifact"`
	Retention retentionResponse        `json:"retention"`
	Cleanup   cleanupResponse          `json:"cleanup"`
}

type retainedResultResponse struct {
	ID         domain.ResultID      `json:"id"`
	Outcome    domain.ResultOutcome `json:"outcome"`
	Repository string               `json:"repository"`
	Base       domain.GitOID        `json:"base_oid"`
	Commit     domain.GitOID        `json:"result_commit"`
	Tree       domain.GitOID        `json:"tree_oid"`
	Entries    int                  `json:"manifest_entries"`
	Manifest   string               `json:"manifest_sha256"`
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

func (h *Handler) result(w http.ResponseWriter, r *http.Request, actor domain.ActorSnapshot, id domain.RunID) {
	if !noQuery(r) || !noBody(r) {
		WriteError(w, http.StatusBadRequest, "invalid_query", "This run operation does not accept query parameters.")
		return
	}
	run, err := h.config.Store.GetBackgroundRun(r.Context(), h.config.WorkspaceID, id, actor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if run.State != domain.ResultReady {
		// A sealing run records its last failed export pass as last_error.
		if run.EffectPhase == domain.Sealing && run.LastError != "" {
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
		Cleanup:   cleanupResponse{run.EffectPhase == domain.CleanupComplete},
	})
}

// verifyRetained runs the full retention verification (bundle copy, unbundle,
// fsck) once per immutable retained tuple instead of on every result read.
func (h *Handler) verifyRetained(ctx context.Context, projection store.BackgroundRunResultProjection) bool {
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
func (h *Handler) authorize(r *http.Request) (domain.ActorSnapshot, pluginauth.RequestAuthorization, bool) {
	actor, err := h.config.ActorResolver(r.Context())
	if err != nil || actor.Validate() != nil {
		return domain.ActorSnapshot{}, pluginauth.RequestAuthorization{}, false
	}
	if actor.Type == domain.ActorOperator {
		return actor, pluginauth.RequestAuthorization{}, true
	}
	authorization, ok := pluginauth.RequestAuthorizationFromContext(r.Context())
	return actor, authorization, ok && pluginBearerActor(actor, authorization.Credential.ID)
}

// pluginBearerActor reports whether actor is the OpenCode plugin identity
// authenticated by the bearer credential credentialID.
func pluginBearerActor(actor domain.ActorSnapshot, credentialID string) bool {
	return actor.Type == domain.ActorOpenCode &&
		actor.ID == credentialID &&
		actor.CredentialID == credentialID &&
		actor.Authentication == "fern_plugin_bearer"
}

// operatorScopes are the operations the operator may perform without a plugin
// credential: workspace-wide discovery and attachment, never run mutation.
var operatorScopes = map[string]bool{"run:read": true, "run:attach": true}

// requireScope checks an authorized actor: the operator against
// operatorScopes, a plugin against its credential's scopes.
func requireScope(w http.ResponseWriter, actor domain.ActorSnapshot, authorization pluginauth.RequestAuthorization, scope string) bool {
	switch {
	case actor.Type == domain.ActorOperator && !operatorScopes[scope]:
		WriteError(w, http.StatusForbidden, "forbidden", "The operator cannot perform this run operation.")
		return false
	case actor.Type != domain.ActorOperator && !authorization.HasScope(scope):
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
	RunID     domain.RunID `json:"run_id"`
	Committed bool         `json:"committed"`
}

type stopResponse struct {
	RunID domain.RunID `json:"run_id"`
	State domain.State `json:"state"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request, actor domain.ActorSnapshot, _ domain.RunID) {
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

func (h *Handler) list(w http.ResponseWriter, r *http.Request, actor domain.ActorSnapshot, _ domain.RunID) {
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

func (h *Handler) get(w http.ResponseWriter, r *http.Request, actor domain.ActorSnapshot, id domain.RunID) {
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
	RunID     domain.RunID             `json:"run_id"`
	URL       string                   `json:"url"`
	SessionID domain.OpenCodeSessionID `json:"session_id"`
	Username  string                   `json:"username"`
	Password  string                   `json:"password"`
	ExpiresAt time.Time                `json:"expires_at"`
}

// attach mints a short-lived OpenCode credential. Durable readiness alone
// never grants access: the route manager must also issue it, and it owns
// expiry and runtime fencing. The response carries a secret and is no-store.
func (h *Handler) attach(w http.ResponseWriter, r *http.Request, actor domain.ActorSnapshot, id domain.RunID) {
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

func attachmentReady(run store.BackgroundRun) bool {
	active := run.State == domain.SettingUp || run.State == domain.Working ||
		run.State == domain.NeedsYou || run.State == domain.Uncertain
	// Provisioning reconciles the session before the prompt fence ends it.
	ready := run.EffectPhase == domain.PromptPending || run.EffectPhase == domain.Admitted
	return active && ready && run.StopReceiptID == 0
}

func (h *Handler) stop(w http.ResponseWriter, r *http.Request, actor domain.ActorSnapshot, id domain.RunID) {
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
	ID         domain.RunID  `json:"id"`
	State      domain.State  `json:"state"`
	Repository string        `json:"repository"`
	Head       domain.GitOID `json:"head"`
	Branch     *string       `json:"branch"`
	// Attachable is advisory, not a reservation; attach re-checks readiness.
	Attachable bool `json:"attachable"`
}

func (h *Handler) view(run store.BackgroundRun) runView {
	_, active := h.config.Route.ActiveOrigin(run)
	return runView{run.RunID, run.State, run.RepositoryRemote, run.BaseOID, run.Branch, active && attachmentReady(run)}
}
func decodeStrict(w http.ResponseWriter, r *http.Request, limit int64, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	payload, err := io.ReadAll(r.Body)
	if err != nil || safeio.CheckJSON(payload, 3) != nil {
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
func idempotencyKey(w http.ResponseWriter, r *http.Request) (domain.IdempotencyKey, bool) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 {
		WriteError(w, http.StatusBadRequest, "invalid_idempotency_key", "A valid Idempotency-Key is required.")
		return "", false
	}
	key, err := domain.ParseIdempotencyKey(values[0])
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
	if err != nil || len(bytes.TrimSpace(payload)) < 2 || bytes.TrimSpace(payload)[0] != '{' || safeio.CheckJSON(payload, 3) != nil {
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
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInvalidCreate):
		WriteError(w, http.StatusBadRequest, "invalid_run", "Repository, base, branch, instruction, or profile is not valid for this Fern workspace.")
	case errors.Is(err, errInvalidBase):
		WriteError(w, http.StatusBadRequest, "invalid_base", "base_oid must be an exact lowercase SHA-1 commit identity.")
	case errors.Is(err, errBaseUnavailable):
		WriteError(w, http.StatusUnprocessableEntity, "base_unavailable", "base_oid is not an exact commit reachable from an allowed configured-repository ref.")
	case errors.Is(err, errReplayConflict):
		WriteError(w, http.StatusConflict, "idempotency_conflict", "Idempotency-Key was already used for another request.")
	case errors.Is(err, store.ErrNotFound):
		writeNotFound(w)
	case errors.Is(err, store.ErrIdempotencyConflict), errors.Is(err, store.ErrInvalidState):
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
