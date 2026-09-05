// Package runapi exposes the plugin-authenticated Background Run boundary.
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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nebler/fern/internal/jsoncanon"
	"github.com/nebler/fern/internal/pluginauth"
	runidentity "github.com/nebler/fern/internal/run"
	"github.com/nebler/fern/internal/runcommand"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	PathPrefix             = "/fern/api/runs"
	PluginOpenCodeProfile  = taskstore.BackgroundRunSourceProfile
	APIContractVersion     = runcommand.APIContractVersion
	maxCreateBodyBytes     = 32 << 10
	maxEmptyBodyBytes      = 16
	backgroundRunListLimit = 100
)

type Store interface {
	runcommand.Store
	ListBackgroundRuns(context.Context, task.WorkspaceID, task.ActorSnapshot, int) ([]taskstore.BackgroundRun, error)
	GetBackgroundRunExport(context.Context, task.ArtifactExportID) (taskstore.BackgroundRunExport, error)
	GetBackgroundRunResult(context.Context, task.WorkspaceID, task.TaskID, task.ActorSnapshot) (taskstore.BackgroundRunResultProjection, error)
}

var _ Store = (*taskstore.Store)(nil)

// BaseVerifier proves an exact object is a commit reachable from the
// configured checkout's HEAD or origin tracking refs. It performs no mutation.
type BaseVerifier = runcommand.BaseVerifier

type ActorResolver func(context.Context) (task.ActorSnapshot, error)

type RetentionVerifier interface {
	Verify(context.Context, taskstore.Result) error
}

type Config struct {
	WorkspaceID                 task.WorkspaceID
	RepositoryID                task.RepositoryID
	RepositoryRemote            string
	BackgroundImageIdentity     string
	BackgroundEnvironmentSHA256 [32]byte
	AvailableProfile            string
	Store                       Store
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
	commands *runcommand.Service
}

func New(config Config) (*Handler, error) {
	if config.Store == nil || config.Generator == nil || config.ActorResolver == nil || config.BaseVerifier == nil || config.RetentionVerifier == nil || config.Now == nil ||
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
	if !canonicalRepositoryRemote(config.RepositoryRemote) {
		return nil, errors.New("canonical background run repository remote is required")
	}
	if (config.AvailableProfile == PluginOpenCodeProfile) != (config.BackgroundImageIdentity != "") ||
		(config.AvailableProfile != "" && config.AvailableProfile != PluginOpenCodeProfile) {
		return nil, errors.New("qualified background image and profile must be configured together")
	}
	commands, err := newCommands(config)
	if err != nil {
		return nil, err
	}
	return &Handler{config: config, commands: commands}, nil
}

func newCommands(config Config) (*runcommand.Service, error) {
	return runcommand.New(runcommand.Config{
		WorkspaceID: config.WorkspaceID, RepositoryID: config.RepositoryID, RepositoryRemote: config.RepositoryRemote,
		BackgroundImageIdentity: config.BackgroundImageIdentity, BackgroundEnvironmentSHA256: config.BackgroundEnvironmentSHA256,
		AvailableProfile: config.AvailableProfile, Store: config.Store, Generator: config.Generator,
		BaseVerifier: config.BaseVerifier, Now: config.Now, AttemptTimeout: config.AttemptTimeout,
		Agent: config.Agent, ModelProvider: config.ModelProvider, Model: config.Model,
		Wake: config.Wake, SealPolicyVersion: config.SealPolicyVersion,
	})
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.EscapedPath() != r.URL.Path {
		writeError(w, http.StatusNotFound, "not_found", "The requested run was not found.")
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
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "The method is not allowed for this resource.")
		}
		return
	}
	if !strings.HasPrefix(r.URL.Path, PathPrefix+"/") {
		writeError(w, http.StatusNotFound, "not_found", "The requested run was not found.")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, PathPrefix+"/"), "/")
	id, err := task.ParseTaskID(parts[0])
	if err != nil || len(parts) > 2 {
		writeError(w, http.StatusNotFound, "not_found", "The requested run was not found.")
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
		writeError(w, http.StatusNotFound, "not_found", "The requested run was not found.")
	}
}

type sealProjection struct {
	RunID         task.TaskID        `json:"run_id"`
	State         runidentity.State  `json:"state"`
	ResultPhase   string             `json:"result_phase"`
	SealRequestID task.SealRequestID `json:"seal_request_id"`
	Committed     bool               `json:"committed"`
}

func (h *Handler) seal(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.TaskID) {
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
	writeJSON(w, http.StatusAccepted, sealProjection{admission.RunID, admission.State, admission.ResultPhase, admission.SealRequestID, admission.Committed})
}

type resultResponse struct {
	RunID     task.TaskID              `json:"run_id"`
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
	ID         task.RetainedArtifactID `json:"id"`
	Format     string                  `json:"format"`
	SHA        string                  `json:"sha256"`
	BundleSHA  string                  `json:"bundle_sha256"`
	BundleSize int64                   `json:"bundle_size"`
	Manifest   string                  `json:"manifest_sha256"`
}

type retentionResponse struct {
	Verified        bool `json:"verified"`
	Reconstructable bool `json:"reconstructable"`
}

type cleanupResponse struct {
	Complete bool `json:"complete"`
}

func (h *Handler) result(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.TaskID) {
	if !noQuery(r) || !noBody(r) {
		writeError(w, 400, "invalid_query", "This run operation does not accept query parameters.")
		return
	}
	run, err := h.config.Store.GetBackgroundRun(r.Context(), h.config.WorkspaceID, id, actor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if run.State != taskstore.BackgroundRunResultReady {
		if run.ArtifactExportID != "" {
			if export, exportErr := h.config.Store.GetBackgroundRunExport(r.Context(), run.ArtifactExportID); exportErr == nil && export.State == taskstore.BackgroundRunExportRecoveryRequired {
				writeError(w, http.StatusServiceUnavailable, "recovery_required", "The retained result requires recovery.")
				return
			}
		}
		writeError(w, http.StatusConflict, "not_ready", "The retained result is not ready.")
		return
	}
	projection, err := h.config.Store.GetBackgroundRunResult(r.Context(), h.config.WorkspaceID, id, actor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	retained := h.config.RetentionVerifier.Verify(r.Context(), projection.Result) == nil
	digest := func(value [32]byte) string { return hex.EncodeToString(value[:]) }
	writeJSON(w, http.StatusOK, resultResponse{RunID: id, State: "result_ready",
		Result: retainedResultResponse{
			projection.Result.ID, projection.Result.Outcome, run.RepositoryRemote, projection.Result.BaseSHA, projection.Result.ResultCommit, projection.Result.TreeOID, projection.Result.ManifestEntries, digest(projection.Result.ManifestSHA256)},
		Artifact: retainedArtifactResponse{
			projection.Artifact.ID, "git_bundle_v1", digest(projection.Artifact.ManifestSHA256), digest(projection.Artifact.BundleSHA256), projection.Artifact.BundleBytes, digest(projection.Artifact.ManifestSHA256)},
		Retention: retentionResponse{retained, retained},
		Cleanup:   cleanupResponse{run.EffectPhase == taskstore.BackgroundRunEffectCleanupComplete},
	})
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) (task.ActorSnapshot, bool) {
	authorization, exists := pluginauth.RequestAuthorizationFromContext(r.Context())
	actor, err := h.config.ActorResolver(r.Context())
	if err != nil || actor.Validate() != nil || !exists || actor.Type != task.ActorOpenCode || actor.ID != authorization.Credential.ID ||
		actor.CredentialID != authorization.Credential.ID || actor.Authentication != "fern_plugin_bearer" {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Plugin authentication is required.")
		return task.ActorSnapshot{}, false
	}
	return actor, true
}

func (h *Handler) requireScope(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, scope string) bool {
	authorization, ok := pluginauth.RequestAuthorizationFromContext(r.Context())
	if actor.Type != task.ActorOpenCode || !ok || actor.ID != authorization.Credential.ID || !authorization.HasScope(scope) {
		writeError(w, http.StatusForbidden, "forbidden", "The plugin credential lacks the required scope.")
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
	RunID     task.TaskID `json:"run_id"`
	Committed bool        `json:"committed"`
}

type stopResponse struct {
	RunID task.TaskID       `json:"run_id"`
	State runidentity.State `json:"state"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot) {
	if !noQuery(r) || !exactJSON(r) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request is not valid.")
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
	admission, err := h.commands.Create(r.Context(), actor, key, runcommand.CreateInput{
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
	writeJSON(w, http.StatusAccepted, createResponse{admission.RunID, admission.Committed})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot) {
	if !noQuery(r) || !noBody(r) {
		writeError(w, 400, "invalid_query", "Run listing does not accept query parameters.")
		return
	}
	runs, err := h.config.Store.ListBackgroundRuns(r.Context(), h.config.WorkspaceID, actor, backgroundRunListLimit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]runView, 0, len(runs))
	for _, run := range runs {
		views = append(views, view(run))
	}
	writeJSON(w, 200, struct {
		Runs []runView `json:"runs"`
	}{views})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.TaskID) {
	if !noQuery(r) || !noBody(r) {
		writeError(w, 400, "invalid_query", "Run reads do not accept query parameters.")
		return
	}
	run, err := h.config.Store.GetBackgroundRun(r.Context(), h.config.WorkspaceID, id, actor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, 200, view(run))
}

func (h *Handler) stop(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.TaskID) {
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
	writeJSON(w, http.StatusAccepted, stopResponse{result.RunID, result.State})
}

type runView struct {
	ID         task.TaskID                  `json:"id"`
	State      taskstore.BackgroundRunState `json:"state"`
	Repository string                       `json:"repository"`
	Head       task.GitOID                  `json:"head"`
	Branch     *string                      `json:"branch"`
}

func view(run taskstore.BackgroundRun) runView {
	return runView{run.TaskID, run.State, run.RepositoryRemote, run.BaseOID, run.Branch}
}
func decodeStrict(w http.ResponseWriter, r *http.Request, limit int64, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	payload, err := io.ReadAll(r.Body)
	if err != nil || jsoncanon.Check(payload, 3) != nil {
		writeError(w, 400, "invalid_json", "The JSON body is not valid.")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, 400, "invalid_json", "The JSON body is not valid.")
		return false
	}
	return true
}
func idempotencyKey(w http.ResponseWriter, r *http.Request) (task.IdempotencyKey, bool) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 {
		writeError(w, 400, "invalid_idempotency_key", "A valid Idempotency-Key is required.")
		return "", false
	}
	key, err := task.ParseIdempotencyKey(values[0])
	if err != nil {
		writeError(w, 400, "invalid_idempotency_key", "A valid Idempotency-Key is required.")
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
		writeError(w, 400, "invalid_request", "This run operation does not accept query parameters.")
		return false
	}
	if !exactJSON(r) {
		writeError(w, 400, "invalid_request", "Content-Type must be application/json.")
		return false
	}
	var value struct{}
	r.Body = http.MaxBytesReader(w, r.Body, maxEmptyBodyBytes)
	payload, err := io.ReadAll(r.Body)
	if err != nil || len(bytes.TrimSpace(payload)) < 2 || bytes.TrimSpace(payload)[0] != '{' || jsoncanon.Check(payload, 3) != nil {
		writeError(w, 400, "invalid_json", "The JSON body must be an empty object.")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		writeError(w, 400, "invalid_json", "The JSON body must be an empty object.")
		return false
	}
	return true
}
func canonicalRepositoryRemote(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || parsed.Path == "/" || strings.HasSuffix(parsed.Path, "/") || strings.HasSuffix(strings.ToLower(parsed.Path), ".git") {
		return false
	}
	return value == "https://"+strings.ToLower(parsed.Host)+parsed.EscapedPath()
}
func methodNotAllowed(w http.ResponseWriter, method string) {
	w.Header().Set("Allow", method)
	writeError(w, 405, "method_not_allowed", "The method is not allowed for this resource.")
}
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runcommand.ErrInvalidCreate):
		writeError(w, http.StatusBadRequest, "invalid_run", "Repository, base, branch, instruction, or profile is not valid for this Fern workspace.")
	case errors.Is(err, runcommand.ErrInvalidBase):
		writeError(w, http.StatusBadRequest, "invalid_base", "base_oid must be an exact lowercase SHA-1 commit identity.")
	case errors.Is(err, runcommand.ErrProfileUnavailable):
		writeError(w, http.StatusServiceUnavailable, "profile_unavailable", fmt.Sprintf("Profile %s requires a configured image qualified for exact source commit 39fb919a054190498f6d5b7985bde231f93ad7a6.", PluginOpenCodeProfile))
	case errors.Is(err, runcommand.ErrBaseUnavailable):
		writeError(w, http.StatusUnprocessableEntity, "base_unavailable", "base_oid is not an exact commit reachable from an allowed configured-repository ref.")
	case errors.Is(err, runcommand.ErrReplayConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", "Idempotency-Key was already used for another request.")
	case errors.Is(err, taskstore.ErrNotFound):
		writeError(w, 404, "not_found", "The requested run was not found.")
	case errors.Is(err, taskstore.ErrIdempotencyConflict), errors.Is(err, taskstore.ErrInvalidState):
		writeError(w, 409, "conflict", "The run command conflicts with durable state.")
	default:
		writeError(w, 500, "internal_error", "The run command could not be completed.")
	}
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{code, message}})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type GitBaseVerifier struct {
	repository, git string
	timeout         time.Duration
}

func NewGitBaseVerifier(repository, git string, timeout time.Duration) (*GitBaseVerifier, error) {
	if !filepath.IsAbs(repository) || filepath.Clean(repository) != repository || !filepath.IsAbs(git) || filepath.Clean(git) != git || timeout <= 0 || timeout > time.Minute {
		return nil, errors.New("valid configured repository verifier is required")
	}
	repositoryInfo, repositoryErr := os.Stat(repository)
	gitInfo, gitErr := os.Stat(git)
	if repositoryErr != nil || !repositoryInfo.IsDir() || gitErr != nil || gitInfo.IsDir() || gitInfo.Mode()&0o111 == 0 {
		return nil, errors.New("configured repository and Git executable must exist")
	}
	return &GitBaseVerifier{repository: repository, git: git, timeout: timeout}, nil
}

func (v *GitBaseVerifier) Verify(parent context.Context, oid task.GitOID) error {
	ctx, cancel := context.WithTimeout(parent, v.timeout)
	defer cancel()
	objectType, err := v.command(ctx, "cat-file", "-t", string(oid))
	if err != nil || !bytes.Equal(objectType, []byte("commit\n")) {
		if err != nil {
			return err
		}
		return errors.New("base object is not exactly a commit")
	}
	output, err := v.output(ctx, "for-each-ref", "--format=%(refname)", "refs/remotes/origin/")
	if err != nil {
		return err
	}
	refs := []string{"HEAD"}
	for _, ref := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.HasPrefix(ref, "refs/remotes/origin/") && !strings.ContainsAny(ref, "\x00\r") {
			refs = append(refs, ref)
		}
	}
	for _, ref := range refs {
		if v.run(ctx, "merge-base", "--is-ancestor", string(oid), ref) == nil {
			return nil
		}
	}
	return errors.New("base commit is not reachable from an allowed ref")
}
func (v *GitBaseVerifier) run(ctx context.Context, args ...string) error {
	_, err := v.command(ctx, args...)
	return err
}
func (v *GitBaseVerifier) output(ctx context.Context, args ...string) (string, error) {
	value, err := v.command(ctx, args...)
	return string(value), err
}
func (v *GitBaseVerifier) command(ctx context.Context, args ...string) ([]byte, error) {
	base := []string{"--no-pager", "--no-replace-objects", "-C", v.repository}
	command := exec.CommandContext(ctx, v.git, append(base, args...)...)
	command.Env = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "HOME=/", "LANG=C", "LC_ALL=C", "PATH=/usr/bin:/bin"}
	var output bytes.Buffer
	command.Stdout = &limitedWriter{writer: &output, remaining: 64 << 10}
	command.Stderr = &limitedWriter{remaining: 64 << 10}
	err := command.Run()
	return output.Bytes(), err
}

type limitedWriter struct {
	writer    io.Writer
	remaining int
}

func (w *limitedWriter) Write(value []byte) (int, error) {
	original := len(value)
	if len(value) > w.remaining {
		value = value[:w.remaining]
	}
	w.remaining -= len(value)
	if w.writer != nil {
		_, _ = w.writer.Write(value)
	}
	return original, nil
}
