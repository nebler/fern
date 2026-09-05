// Package runapi exposes the plugin-authenticated Background Run boundary.
package runapi

import (
	"bytes"
	"context"
	"crypto/sha256"
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

	"github.com/nebler/fern/internal/backgroundopencode"
	"github.com/nebler/fern/internal/jsoncanon"
	"github.com/nebler/fern/internal/pluginauth"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	PathPrefix             = "/fern/api/runs"
	PluginOpenCodeProfile  = taskstore.BackgroundRunSourceProfile
	APIContractVersion     = "fern.background-run.v1"
	maxCreateBodyBytes     = 32 << 10
	maxEmptyBodyBytes      = 16
	maxInstructionRunes    = 4000
	backgroundRunListLimit = 100
)

type Store interface {
	AdmitBackgroundRun(context.Context, taskstore.AdmitBackgroundRunParams) (taskstore.Admission, error)
	FindReceiptByIdempotency(context.Context, task.WorkspaceID, string, task.IdempotencyKey) (taskstore.Receipt, bool, error)
	GetBackgroundRun(context.Context, task.WorkspaceID, task.TaskID, task.ActorSnapshot) (taskstore.BackgroundRun, error)
	ListBackgroundRuns(context.Context, task.WorkspaceID, task.ActorSnapshot, int) ([]taskstore.BackgroundRun, error)
	StopBackgroundRun(context.Context, taskstore.StopBackgroundRunParams) (taskstore.BackgroundRunStop, error)
	OpenBackgroundRun(context.Context, taskstore.OpenBackgroundRunParams) (taskstore.BackgroundRunOpen, error)
	SealBackgroundRun(context.Context, taskstore.SealBackgroundRunParams) (taskstore.BackgroundRunSealAdmission, error)
	GetBackgroundRunOwners(context.Context, task.WorkspaceID, task.TaskID, task.ActorSnapshot) (taskstore.Task, taskstore.Attempt, error)
	GetBackgroundRunExport(context.Context, task.ArtifactExportID) (taskstore.BackgroundRunExport, error)
	GetBackgroundRunResult(context.Context, task.WorkspaceID, task.TaskID, task.ActorSnapshot) (taskstore.BackgroundRunResultProjection, error)
}

var _ Store = (*taskstore.Store)(nil)

// BaseVerifier proves an exact object is a commit reachable from the
// configured checkout's HEAD or origin tracking refs. It performs no mutation.
type BaseVerifier interface {
	Verify(context.Context, task.GitOID) error
}

type ActorResolver func(context.Context) (task.ActorSnapshot, error)

type RouteResolver interface {
	ActiveOrigin(taskstore.BackgroundRun) (string, bool)
}

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
	BudgetSnapshot              json.RawMessage
	Wake                        func()
	Route                       RouteResolver
	RetentionVerifier           RetentionVerifier
	SealPolicyVersion           string
}

type Handler struct{ config Config }

func New(config Config) (*Handler, error) {
	if config.Store == nil || config.Generator == nil || config.ActorResolver == nil || config.BaseVerifier == nil || config.RetentionVerifier == nil || config.Now == nil ||
		config.AttemptTimeout <= 0 || config.RepositoryID == 0 || config.RepositoryRemote == "" ||
		config.Agent == "" || config.ModelProvider == "" || config.Model == "" || config.BackgroundEnvironmentSHA256 == ([32]byte{}) ||
		len(config.BudgetSnapshot) == 0 || !json.Valid(config.BudgetSnapshot) {
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
	if (config.AvailableProfile == PluginOpenCodeProfile) != (config.Route != nil) {
		return nil, errors.New("qualified background route and profile must be configured together")
	}
	config.BudgetSnapshot = append(json.RawMessage(nil), config.BudgetSnapshot...)
	return &Handler{config: config}, nil
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
			if !h.requireScope(w, r, "run:create") {
				return
			}
			h.create(w, r, actor)
		case http.MethodGet:
			if !h.requireScope(w, r, "run:read") {
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
		if !h.requireScope(w, r, "run:read") {
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
		if !h.requireScope(w, r, "run:stop") {
			return
		}
		h.stop(w, r, actor, id)
	case "open":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if !h.requireScope(w, r, "run:open") {
			return
		}
		h.open(w, r, actor, id)
	case "result":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		if !h.requireScope(w, r, "run:result") {
			return
		}
		h.result(w, r, actor, id)
	case "seal":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if !h.requireScope(w, r, "run:result") {
			return
		}
		h.seal(w, r, actor, id)
	default:
		writeError(w, http.StatusNotFound, "not_found", "The requested run was not found.")
	}
}

type sealProjection struct {
	RunID         task.TaskID         `json:"run_id"`
	State         BackgroundSealState `json:"state"`
	ResultPhase   string              `json:"result_phase"`
	SealRequestID task.SealRequestID  `json:"seal_request_id"`
	Committed     bool                `json:"committed"`
}

type BackgroundSealState string

func (h *Handler) seal(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.TaskID) {
	if !validateEmptyMutation(w, r) {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	claim := task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: h.config.WorkspaceID, CommandKind: taskstore.SealBackgroundRunCommand},
		Key: key, RequestHash: commandHash(taskstore.SealBackgroundRunCommand, struct {
			RunID task.TaskID `json:"run_id"`
		}{id}), Actor: actor}
	run, err := h.config.Store.GetBackgroundRun(r.Context(), h.config.WorkspaceID, id, actor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	owner, attempt, err := h.config.Store.GetBackgroundRunOwners(r.Context(), h.config.WorkspaceID, id, actor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	ids, err := h.config.Generator.GenerateBackgroundSealIDs()
	if err != nil {
		writeError(w, 500, "internal_error", "The run could not be sealed.")
		return
	}
	now := h.config.Now().UTC().Truncate(time.Millisecond)
	admission, err := h.config.Store.SealBackgroundRun(r.Context(), taskstore.SealBackgroundRunParams{
		WorkspaceID: h.config.WorkspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID, Generation: run.Generation,
		ExpectedRunRevision: run.Revision, ExpectedTaskRevision: owner.Revision, ExpectedAttemptRevision: attempt.Revision,
		SealRequestID: ids.SealRequestID, ReceiptID: ids.ReceiptID, ExportID: ids.ArtifactExportID,
		ArtifactID: ids.RetainedArtifactID, MaterializationID: ids.MaterializationID, ResultID: ids.ResultID,
		ResultEventID: ids.ResultEventID, TaskEventID: ids.TaskEventID, Claim: claim, CommitEpochSeconds: now.Unix(),
		PolicyVersion: h.config.SealPolicyVersion, APIContractVersion: APIContractVersion, AcceptedAt: now,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if admission.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	state, phase := BackgroundSealState("canceling"), "seal_requested"
	if admission.Run.State == taskstore.BackgroundRunResultReady {
		state, phase = "result_ready", "ready"
	}
	if !admission.Replayed && h.config.Wake != nil {
		h.config.Wake()
	}
	writeJSON(w, http.StatusAccepted, sealProjection{admission.Run.TaskID, state, phase, admission.Request.ID, true})
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
	writeJSON(w, http.StatusOK, struct {
		RunID  task.TaskID `json:"run_id"`
		State  string      `json:"state"`
		Result struct {
			ID         task.ResultID      `json:"id"`
			Outcome    task.ResultOutcome `json:"outcome"`
			Repository string             `json:"repository"`
			Base       task.GitOID        `json:"base_oid"`
			Commit     task.GitOID        `json:"result_commit"`
			Tree       task.GitOID        `json:"tree_oid"`
			Entries    int                `json:"manifest_entries"`
			Manifest   string             `json:"manifest_sha256"`
		} `json:"result"`
		Artifact struct {
			ID         task.RetainedArtifactID `json:"id"`
			Format     string                  `json:"format"`
			SHA        string                  `json:"sha256"`
			BundleSHA  string                  `json:"bundle_sha256"`
			BundleSize int64                   `json:"bundle_size"`
			Manifest   string                  `json:"manifest_sha256"`
		} `json:"artifact"`
		Retention struct {
			Verified        bool `json:"verified"`
			Reconstructable bool `json:"reconstructable"`
		} `json:"retention"`
		Cleanup struct {
			Complete bool `json:"complete"`
		} `json:"cleanup"`
	}{RunID: id, State: "result_ready",
		Result: struct {
			ID         task.ResultID      `json:"id"`
			Outcome    task.ResultOutcome `json:"outcome"`
			Repository string             `json:"repository"`
			Base       task.GitOID        `json:"base_oid"`
			Commit     task.GitOID        `json:"result_commit"`
			Tree       task.GitOID        `json:"tree_oid"`
			Entries    int                `json:"manifest_entries"`
			Manifest   string             `json:"manifest_sha256"`
		}{
			projection.Result.ID, projection.Result.Outcome, run.RepositoryRemote, projection.Result.BaseSHA, projection.Result.ResultCommit, projection.Result.TreeOID, projection.Result.ManifestEntries, digest(projection.Result.ManifestSHA256)},
		Artifact: struct {
			ID         task.RetainedArtifactID `json:"id"`
			Format     string                  `json:"format"`
			SHA        string                  `json:"sha256"`
			BundleSHA  string                  `json:"bundle_sha256"`
			BundleSize int64                   `json:"bundle_size"`
			Manifest   string                  `json:"manifest_sha256"`
		}{
			projection.Artifact.ID, "git_bundle_v1", digest(projection.Artifact.ManifestSHA256), digest(projection.Artifact.BundleSHA256), projection.Artifact.BundleBytes, digest(projection.Artifact.ManifestSHA256)},
		Retention: struct {
			Verified        bool `json:"verified"`
			Reconstructable bool `json:"reconstructable"`
		}{retained, retained},
		Cleanup: struct {
			Complete bool `json:"complete"`
		}{run.EffectPhase == taskstore.BackgroundRunEffectCleanupComplete},
	})
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) (task.ActorSnapshot, bool) {
	authorization, exists := pluginauth.RequestAuthorizationFromContext(r.Context())
	actor, err := h.config.ActorResolver(r.Context())
	if !exists || err != nil || actor.Validate() != nil || actor.Type != task.ActorOpenCode ||
		actor.ID != authorization.Credential.ID || actor.CredentialID != authorization.Credential.ID || actor.Authentication != "fern_plugin_bearer" {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Plugin authentication is required.")
		return task.ActorSnapshot{}, false
	}
	return actor, true
}

func (h *Handler) requireScope(w http.ResponseWriter, r *http.Request, scope string) bool {
	authorization, ok := pluginauth.RequestAuthorizationFromContext(r.Context())
	if !ok || !authorization.HasScope(scope) {
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
	if !decodeStrict(w, r, maxCreateBodyBytes, &input) || input.Repository != h.config.RepositoryRemote ||
		!validInstruction(input.Instruction) ||
		input.Profile != PluginOpenCodeProfile || (input.Branch != nil && !validText(*input.Branch, 1, 255)) {
		writeError(w, http.StatusBadRequest, "invalid_run", "Repository, base, branch, instruction, or profile is not valid for this Fern workspace.")
		return
	}
	base, err := task.ParseGitOID(input.BaseOID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_base", "base_oid must be an exact lowercase SHA-1 commit identity.")
		return
	}
	requestHash := createHash(input)
	claim := task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: h.config.WorkspaceID, CommandKind: taskstore.CreateBackgroundRunCommand}, Key: key, RequestHash: requestHash, Actor: actor}
	if h.replayCreate(w, r, claim) {
		return
	}
	if h.config.AvailableProfile != PluginOpenCodeProfile {
		writeError(w, http.StatusServiceUnavailable, "profile_unavailable",
			fmt.Sprintf("Profile %s requires a configured image qualified for exact source commit 39fb919a054190498f6d5b7985bde231f93ad7a6.", PluginOpenCodeProfile))
		return
	}
	if err := h.config.BaseVerifier.Verify(r.Context(), base); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "base_unavailable", "base_oid is not an exact commit reachable from an allowed configured-repository ref.")
		return
	}
	ids, err := h.config.Generator.GenerateAdmissionIDs()
	if err != nil {
		writeError(w, 500, "internal_error", "The run could not be committed.")
		return
	}
	now := h.config.Now().UTC().Truncate(time.Millisecond)
	if now.IsZero() || now.UnixMilli() < 0 {
		writeError(w, 500, "internal_error", "The run could not be committed.")
		return
	}
	branch := ""
	if input.Branch != nil {
		branch = *input.Branch
	}
	profileHash := sha256.Sum256([]byte(input.Profile))
	compact := strings.ReplaceAll(strings.TrimPrefix(string(ids.TaskID), "tsk_"), "-", "")
	intent := &taskstore.BackgroundRunIntent{RepositoryRemote: input.Repository, Branch: branch,
		InstructionSHA256: sha256.Sum256([]byte(input.Instruction)), Profile: input.Profile, ProfileSHA256: profileHash,
		EnvironmentSHA256: h.config.BackgroundEnvironmentSHA256,
		ImageIdentity:     h.config.BackgroundImageIdentity,
		CloneIdentity:     "run-" + compact + "-g1-clone", VolumeIdentity: "fern-run-" + compact + "-g1-opencode",
		ContainerIdentity: "fern-run-" + compact + "-g1", EndpointIdentity: "run-" + compact + "-g1-endpoint"}
	admission, err := h.config.Store.AdmitBackgroundRun(r.Context(), taskstore.AdmitBackgroundRunParams{
		TaskID: ids.TaskID, AttemptID: ids.AttemptID, ReceiptID: ids.ReceiptID, TaskEventID: ids.TaskEventID,
		AttemptEventID: ids.AttemptEventID, OpenCodeSessionID: ids.OpenCodeSessionID, OpenCodeMessageID: ids.OpenCodeMessageID,
		Claim: claim, Title: "Background Run", Prompt: input.Instruction, RepositoryID: h.config.RepositoryID,
		BaseRef: displayBase(input), BaseSHA: base, ObjectFormat: "sha1", ExecutionContractVersion: APIContractVersion,
		Agent: h.config.Agent, ModelProvider: h.config.ModelProvider, Model: h.config.Model,
		BudgetSnapshot: h.config.BudgetSnapshot, Deadline: now.Add(h.config.AttemptTimeout), APIContractVersion: APIContractVersion,
		AcceptedAt: now, BackgroundRun: intent,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if admission.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	if h.config.Wake != nil {
		h.config.Wake()
	}
	writeJSON(w, http.StatusAccepted, struct {
		RunID     task.TaskID `json:"run_id"`
		Committed bool        `json:"committed"`
	}{admission.Task.ID, true})
}

func (h *Handler) replayCreate(w http.ResponseWriter, r *http.Request, claim task.IdempotencyClaim) bool {
	receipt, found, err := h.config.Store.FindReceiptByIdempotency(r.Context(), h.config.WorkspaceID, taskstore.CreateBackgroundRunCommand, claim.Key)
	if err != nil {
		writeStoreError(w, err)
		return true
	}
	if !found {
		return false
	}
	disposition, err := task.ClassifyIdempotency(&task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: receipt.WorkspaceID, CommandKind: receipt.CommandKind}, Key: receipt.IdempotencyKey, RequestHash: receipt.RequestHash, Actor: receipt.Actor}, claim)
	if err != nil {
		writeError(w, 500, "internal_error", "The run could not be read.")
		return true
	}
	if disposition == task.IdempotencyOwnerMismatch {
		writeError(w, 404, "not_found", "The requested run was not found.")
		return true
	}
	if disposition != task.IdempotencyReplay {
		writeError(w, 409, "idempotency_conflict", "Idempotency-Key was already used for another request.")
		return true
	}
	run, err := h.config.Store.GetBackgroundRun(r.Context(), h.config.WorkspaceID, receipt.TargetID, claim.Actor)
	if err != nil {
		writeStoreError(w, err)
		return true
	}
	w.Header().Set("Idempotency-Replayed", "true")
	writeJSON(w, http.StatusAccepted, struct {
		RunID     task.TaskID `json:"run_id"`
		Committed bool        `json:"committed"`
	}{run.TaskID, true})
	return true
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
	claim := task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: h.config.WorkspaceID, CommandKind: taskstore.StopBackgroundRunCommand}, Key: key,
		RequestHash: commandHash(taskstore.StopBackgroundRunCommand, struct {
			RunID task.TaskID `json:"run_id"`
		}{id}), Actor: actor}
	if h.replayStop(w, r, id, claim) {
		return
	}
	receiptID, err := h.config.Generator.ReceiptID()
	if err != nil {
		writeError(w, 500, "internal_error", "The run could not be stopped.")
		return
	}
	attemptEventID, err := h.config.Generator.EventID()
	if err != nil {
		writeError(w, 500, "internal_error", "The run could not be stopped.")
		return
	}
	taskEventID, err := h.config.Generator.EventID()
	if err != nil {
		writeError(w, 500, "internal_error", "The run could not be stopped.")
		return
	}
	now := h.config.Now().UTC().Truncate(time.Millisecond)
	result, err := h.config.Store.StopBackgroundRun(r.Context(), taskstore.StopBackgroundRunParams{WorkspaceID: h.config.WorkspaceID,
		TaskID: id, ReceiptID: receiptID, AttemptEventID: attemptEventID, TaskEventID: taskEventID, Claim: claim,
		APIContractVersion: APIContractVersion, StoppedAt: now})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	if h.config.Wake != nil {
		h.config.Wake()
	}
	writeJSON(w, http.StatusAccepted, struct {
		RunID task.TaskID                  `json:"run_id"`
		State taskstore.BackgroundRunState `json:"state"`
	}{id, result.Run.State})
}

func (h *Handler) replayStop(w http.ResponseWriter, r *http.Request, id task.TaskID, claim task.IdempotencyClaim) bool {
	receipt, found, err := h.config.Store.FindReceiptByIdempotency(r.Context(), h.config.WorkspaceID, taskstore.StopBackgroundRunCommand, claim.Key)
	if err != nil {
		writeStoreError(w, err)
		return true
	}
	if !found {
		return false
	}
	disposition, err := task.ClassifyIdempotency(&task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: receipt.WorkspaceID, CommandKind: receipt.CommandKind}, Key: receipt.IdempotencyKey, RequestHash: receipt.RequestHash, Actor: receipt.Actor}, claim)
	if err != nil {
		writeError(w, 500, "internal_error", "The run could not be stopped.")
		return true
	}
	if disposition == task.IdempotencyOwnerMismatch {
		writeError(w, 404, "not_found", "The requested run was not found.")
		return true
	}
	if disposition != task.IdempotencyReplay || receipt.TargetID != id {
		writeError(w, 409, "idempotency_conflict", "Idempotency-Key was already used for another request.")
		return true
	}
	run, err := h.config.Store.GetBackgroundRun(r.Context(), h.config.WorkspaceID, id, claim.Actor)
	if err != nil || run.StopReceiptID != receipt.ID {
		if err != nil {
			writeStoreError(w, err)
		} else {
			writeError(w, 500, "internal_error", "The run could not be stopped.")
		}
		return true
	}
	var committed struct {
		RunID task.TaskID                  `json:"run_id"`
		State taskstore.BackgroundRunState `json:"state"`
	}
	if json.Unmarshal(receipt.ResponseProjection, &committed) != nil || committed.RunID != id ||
		(committed.State != taskstore.BackgroundRunFailed && committed.State != taskstore.BackgroundRunCanceling) {
		writeError(w, 500, "internal_error", "The run could not be stopped.")
		return true
	}
	w.Header().Set("Idempotency-Replayed", "true")
	writeJSON(w, http.StatusAccepted, struct {
		RunID task.TaskID                  `json:"run_id"`
		State taskstore.BackgroundRunState `json:"state"`
	}{id, committed.State})
	return true
}

type openProjection struct {
	RunID task.TaskID `json:"run_id"`
	URL   string      `json:"url"`
}

func (h *Handler) open(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.TaskID) {
	if !validateEmptyMutation(w, r) {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	claim := task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: h.config.WorkspaceID, CommandKind: taskstore.OpenBackgroundRunCommand}, Key: key,
		RequestHash: commandHash(taskstore.OpenBackgroundRunCommand, struct {
			RunID task.TaskID `json:"run_id"`
		}{id}), Actor: actor}
	if h.replayOpen(w, r, id, claim) {
		return
	}
	run, err := h.config.Store.GetBackgroundRun(r.Context(), h.config.WorkspaceID, id, claim.Actor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	projection, ok := h.openProjection(run)
	if !ok {
		writeError(w, http.StatusConflict, "not_ready", "The disposable Background Run route is not ready.")
		return
	}
	receiptID, err := h.config.Generator.ReceiptID()
	if err != nil {
		writeError(w, 500, "internal_error", "The run could not be opened.")
		return
	}
	now := h.config.Now().UTC().Truncate(time.Millisecond)
	result, err := h.config.Store.OpenBackgroundRun(r.Context(), taskstore.OpenBackgroundRunParams{WorkspaceID: h.config.WorkspaceID,
		TaskID: id, ReceiptID: receiptID, Claim: claim, URL: projection.URL, APIContractVersion: APIContractVersion, OpenedAt: now})
	if err != nil {
		if errors.Is(err, taskstore.ErrInvalidState) {
			writeError(w, http.StatusConflict, "not_ready", "The disposable Background Run route is not ready.")
			return
		}
		writeStoreError(w, err)
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
		if json.Unmarshal(result.Receipt.ResponseProjection, &projection) != nil {
			writeError(w, 500, "internal_error", "The run could not be opened.")
			return
		}
	}
	if active, ready := h.openProjection(result.Run); !ready || active != projection {
		writeError(w, http.StatusConflict, "not_ready", "The disposable Background Run route is not ready.")
		return
	}
	writeJSON(w, http.StatusOK, projection)
}

func (h *Handler) replayOpen(w http.ResponseWriter, r *http.Request, id task.TaskID, claim task.IdempotencyClaim) bool {
	receipt, found, err := h.config.Store.FindReceiptByIdempotency(r.Context(), h.config.WorkspaceID, taskstore.OpenBackgroundRunCommand, claim.Key)
	if err != nil {
		writeStoreError(w, err)
		return true
	}
	if !found {
		return false
	}
	disposition, err := task.ClassifyIdempotency(&task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: receipt.WorkspaceID, CommandKind: receipt.CommandKind}, Key: receipt.IdempotencyKey, RequestHash: receipt.RequestHash, Actor: receipt.Actor}, claim)
	if err != nil {
		writeError(w, 500, "internal_error", "The run could not be opened.")
		return true
	}
	if disposition == task.IdempotencyOwnerMismatch {
		writeError(w, 404, "not_found", "The requested run was not found.")
		return true
	}
	if disposition != task.IdempotencyReplay || receipt.TargetID != id {
		writeError(w, 409, "idempotency_conflict", "Idempotency-Key was already used for another request.")
		return true
	}
	run, err := h.config.Store.GetBackgroundRun(r.Context(), h.config.WorkspaceID, id, claim.Actor)
	if err != nil {
		writeStoreError(w, err)
		return true
	}
	expected, ready := h.openProjection(run)
	var committed openProjection
	if !ready || json.Unmarshal(receipt.ResponseProjection, &committed) != nil || committed != expected || committed.RunID != id {
		writeError(w, http.StatusConflict, "not_ready", "The disposable Background Run route is not ready.")
		return true
	}
	w.Header().Set("Idempotency-Replayed", "true")
	writeJSON(w, http.StatusOK, committed)
	return true
}

func (h *Handler) openProjection(run taskstore.BackgroundRun) (openProjection, bool) {
	active := run.State == taskstore.BackgroundRunSettingUp || run.State == taskstore.BackgroundRunWorking ||
		run.State == taskstore.BackgroundRunNeedsYou || run.State == taskstore.BackgroundRunUncertain
	readyPhase := run.EffectPhase == taskstore.BackgroundRunEffectSessionObserved || run.EffectPhase == taskstore.BackgroundRunEffectPromptIntent ||
		run.EffectPhase == taskstore.BackgroundRunEffectPromptAdmitted
	if h.config.Route == nil || !active || !readyPhase || run.SessionObservedAt == nil || run.CancelEpoch != 0 {
		return openProjection{}, false
	}
	origin, active := h.config.Route.ActiveOrigin(run)
	if !active {
		return openProjection{}, false
	}
	trusted, err := backgroundopencode.ParseTrustedOrigin(origin)
	if err != nil {
		return openProjection{}, false
	}
	deepLink, err := backgroundopencode.DeepLink(trusted, string(run.OpenCodeSessionID))
	if err != nil {
		return openProjection{}, false
	}
	return openProjection{RunID: run.TaskID, URL: deepLink}, true
}

func (h *Handler) notReady(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.TaskID, mutation bool) {
	if !noQuery(r) || (!mutation && !noBody(r)) {
		writeError(w, 400, "invalid_query", "This run operation does not accept query parameters.")
		return
	}
	if mutation {
		if !validateEmptyMutation(w, r) {
			return
		}
		if _, ok := idempotencyKey(w, r); !ok {
			return
		}
	}
	if _, err := h.config.Store.GetBackgroundRun(r.Context(), h.config.WorkspaceID, id, actor); err != nil {
		writeStoreError(w, err)
		return
	}
	writeError(w, http.StatusConflict, "not_ready", "The disposable Background Run environment is not available in this Fern build.")
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
func displayBase(input createInput) string {
	if input.Branch != nil {
		return *input.Branch
	}
	return input.BaseOID
}

func createHash(input createInput) task.RequestHash {
	return commandHash(taskstore.CreateBackgroundRunCommand, input)
}
func commandHash(kind string, value any) task.RequestHash {
	encoded, _ := json.Marshal(value)
	return task.RequestHash(sha256.Sum256(append(append([]byte(kind), '\n'), encoded...)))
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
func validInstruction(value string) bool {
	if len(value) < 1 || len(value) > 16*1024 || utf8.RuneCountInString(value) > maxInstructionRunes || !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) && char != '\n' && char != '\t' {
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
