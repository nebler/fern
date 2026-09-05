package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/nebler/fern/internal/task"
)

const (
	CreateBackgroundRunCommand = "run.create"
)

// AdmitBackgroundRun atomically claims an idempotency key and creates the
// Background Run's task, sequence-1 attempt, receipt, and initial events. It
// performs no external effects.
func (s *Store) AdmitBackgroundRun(ctx context.Context, p AdmitBackgroundRunParams) (_ Admission, err error) {
	if err := validateAdmission(p); err != nil {
		return Admission{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return Admission{}, fmt.Errorf("begin task admission: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	existing, found, err := receiptByKey(ctx, tx, p.Claim.Scope.WorkspaceID, p.Claim.Scope.CommandKind, p.Claim.Key)
	if err != nil {
		return Admission{}, err
	}
	if found {
		existingClaim := task.IdempotencyClaim{
			Scope: task.IdempotencyScope{WorkspaceID: existing.WorkspaceID, CommandKind: existing.CommandKind},
			Key:   existing.IdempotencyKey, RequestHash: existing.RequestHash, Actor: existing.Actor,
		}
		disposition, classifyErr := task.ClassifyIdempotency(&existingClaim, p.Claim)
		if classifyErr != nil {
			return Admission{}, fmt.Errorf("classify idempotency: %w", classifyErr)
		}
		switch disposition {
		case task.IdempotencyReplay:
			storedTask, getErr := getTask(ctx, tx, existing.TargetID)
			if getErr != nil {
				return Admission{}, getErr
			}
			storedAttempt, getErr := getAttempt(ctx, tx, storedTask.CurrentAttemptID)
			if getErr != nil {
				return Admission{}, getErr
			}
			taskEvent, attemptEvent, getErr := admissionEvents(ctx, tx, storedTask.ID, storedAttempt.ID)
			if getErr != nil {
				return Admission{}, getErr
			}
			if err := tx.Commit(); err != nil {
				return Admission{}, fmt.Errorf("finish admission replay: %w", err)
			}
			return Admission{Task: storedTask, Attempt: storedAttempt, Receipt: existing, TaskEvent: taskEvent, AttemptEvent: attemptEvent, Replayed: true}, nil
		case task.IdempotencyOwnerMismatch:
			return Admission{}, ErrIdempotencyOwnerMismatch
		case task.IdempotencyConflict:
			return Admission{}, &ConflictError{ReceiptID: existing.ID, TargetID: existing.TargetID}
		default:
			return Admission{}, fmt.Errorf("%w: unexpected idempotency disposition", ErrCorruptStore)
		}
	}

	var workspaceState WorkspaceState
	var repositoryID int64
	var imageDigest, openCodeProtocol string
	if err := tx.QueryRowContext(ctx, `SELECT state,repository_id,image_digest,opencode_protocol FROM workspaces WHERE id=?`, p.Claim.Scope.WorkspaceID).Scan(&workspaceState, &repositoryID, &imageDigest, &openCodeProtocol); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Admission{}, fmt.Errorf("%w: workspace", ErrNotFound)
		}
		return Admission{}, fmt.Errorf("read workspace: %w", err)
	}
	if workspaceState != WorkspaceActive {
		return Admission{}, ErrWorkspaceUnavailable
	}
	if task.RepositoryID(repositoryID) != p.RepositoryID {
		return Admission{}, ErrRepositoryMismatch
	}

	actorID, err := ensureActor(ctx, tx, p.Claim.Actor)
	if err != nil {
		return Admission{}, err
	}
	promptHash := sha256.Sum256([]byte(p.Prompt))
	attemptImage, attemptProtocol := p.BackgroundRun.ImageIdentity, p.BackgroundRun.Profile
	acceptedMS := unixMillis(p.AcceptedAt)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tasks(
    id,workspace_id,title,prompt,prompt_sha256,repository_id,base_ref,base_sha,
    object_format,state,cancel_epoch,current_attempt_id,actor_snapshot_id,latest_event_cursor,
    revision,created_at,updated_at
) VALUES(?,?,?,?,?,?,?,?,?,'queued',0,?,?,0,1,?,?)`,
		p.TaskID, p.Claim.Scope.WorkspaceID, p.Title, p.Prompt, promptHash[:], p.RepositoryID,
		p.BaseRef, p.BaseSHA, p.ObjectFormat, p.AttemptID, actorID, acceptedMS, acceptedMS); err != nil {
		return Admission{}, fmt.Errorf("insert task: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO attempts(
    id,task_id,workspace_id,sequence,state,delivery_phase,opencode_session_id,opencode_message_id,prompt_sha256,
    base_sha,image_digest,opencode_protocol,execution_contract_version,agent,
    model_provider,model,budget_snapshot,deadline,revision,created_at,updated_at
) VALUES(?, ?, ?, 1, 'prepared', 'none', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		p.AttemptID, p.TaskID, p.Claim.Scope.WorkspaceID, p.OpenCodeSessionID, p.OpenCodeMessageID, promptHash[:], p.BaseSHA,
		attemptImage, attemptProtocol, p.ExecutionContractVersion, p.Agent, p.ModelProvider, p.Model,
		string(p.BudgetSnapshot), unixMillis(p.Deadline), acceptedMS, acceptedMS); err != nil {
		return Admission{}, fmt.Errorf("insert attempt: %w", err)
	}
	response, err := json.Marshal(struct {
		RunID     task.TaskID `json:"run_id"`
		Committed bool        `json:"committed"`
	}{p.TaskID, true})
	if err != nil {
		return Admission{}, fmt.Errorf("encode receipt projection: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO receipts(
    id,workspace_id,command_kind,state,idempotency_key,request_hash,actor_snapshot_id,
    accepted_at,api_contract_version,target_type,target_id,response_status,response_projection
) VALUES(?,?,?,'accepted',?,?,?,?,?,'task',?,202,?)`,
		p.ReceiptID, p.Claim.Scope.WorkspaceID, p.Claim.Scope.CommandKind, p.Claim.Key,
		p.Claim.RequestHash[:], actorID, acceptedMS, p.APIContractVersion, p.TaskID, string(response)); err != nil {
		return Admission{}, fmt.Errorf("insert receipt: %w", err)
	}
	branch := any(nil)
	if p.BackgroundRun.Branch != "" {
		branch = p.BackgroundRun.Branch
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO background_runs(
    task_id,attempt_id,workspace_id,generation,repository_id,repository_remote,base_oid,branch,
    instruction_sha256,profile,profile_sha256,environment_sha256,resource_spec_version,image_identity,clone_identity,volume_identity,
    container_identity,endpoint_identity,opencode_session_id,opencode_message_id,state,effect_phase,
    creator_actor_snapshot_id,revision,created_at,updated_at
) VALUES(?,?,?,1,?,?,?,?,?,?,?,?,9,?,?,?,?,?,?,?,'queued','absent',?,1,?,?)`,
		p.TaskID, p.AttemptID, p.Claim.Scope.WorkspaceID, p.RepositoryID, p.BackgroundRun.RepositoryRemote,
		p.BaseSHA, branch, p.BackgroundRun.InstructionSHA256[:], p.BackgroundRun.Profile,
		p.BackgroundRun.ProfileSHA256[:], p.BackgroundRun.EnvironmentSHA256[:], p.BackgroundRun.ImageIdentity, p.BackgroundRun.CloneIdentity,
		p.BackgroundRun.VolumeIdentity, p.BackgroundRun.ContainerIdentity, p.BackgroundRun.EndpointIdentity,
		p.OpenCodeSessionID, p.OpenCodeMessageID, actorID, acceptedMS, acceptedMS); err != nil {
		return Admission{}, fmt.Errorf("insert background run: %w", err)
	}
	taskPayload := []byte(`{}`)
	result, err := tx.ExecContext(ctx, `
INSERT INTO events(
    id,workspace_id,task_id,attempt_id,entity_type,entity_id,type,version,occurred_at,actor_snapshot_id,payload
) VALUES(?, ?, ?, NULL, 'task', ?, 'task.accepted', 1, ?, ?, ?)`,
		p.TaskEventID, p.Claim.Scope.WorkspaceID, p.TaskID, p.TaskID, acceptedMS, actorID, string(taskPayload))
	if err != nil {
		return Admission{}, fmt.Errorf("insert acceptance event: %w", err)
	}
	taskCursor, err := result.LastInsertId()
	if err != nil || taskCursor <= 0 {
		return Admission{}, fmt.Errorf("read acceptance cursor: %w", err)
	}
	attemptPayload := []byte(`{"sequence":1}`)
	result, err = tx.ExecContext(ctx, `
INSERT INTO events(
    id,workspace_id,task_id,attempt_id,entity_type,entity_id,type,version,occurred_at,actor_snapshot_id,payload
) VALUES(?, ?, ?, ?, 'attempt', ?, 'attempt.prepared', 1, ?, ?, ?)`,
		p.AttemptEventID, p.Claim.Scope.WorkspaceID, p.TaskID, p.AttemptID, p.AttemptID, acceptedMS, actorID, string(attemptPayload))
	if err != nil {
		return Admission{}, fmt.Errorf("insert prepared event: %w", err)
	}
	attemptCursor, err := result.LastInsertId()
	if err != nil || attemptCursor <= taskCursor {
		return Admission{}, fmt.Errorf("read prepared cursor: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET current_attempt_id=?,latest_event_cursor=? WHERE id=?`, p.AttemptID, attemptCursor, p.TaskID); err != nil {
		return Admission{}, fmt.Errorf("link current attempt and events: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Admission{}, fmt.Errorf("commit task admission: %w", err)
	}

	actor := p.Claim.Actor
	storedTask := Task{
		ID: p.TaskID, WorkspaceID: p.Claim.Scope.WorkspaceID, Title: p.Title, Prompt: p.Prompt,
		PromptSHA256: promptHash, RepositoryID: p.RepositoryID, BaseRef: p.BaseRef, BaseSHA: p.BaseSHA,
		ObjectFormat: p.ObjectFormat, State: task.TaskQueued, CurrentAttemptID: p.AttemptID, Actor: actor,
		LatestEventCursor: task.Cursor(attemptCursor), Revision: 1,
		CreatedAt: fromUnixMillis(acceptedMS), UpdatedAt: fromUnixMillis(acceptedMS),
	}
	storedAttempt := Attempt{
		ID: p.AttemptID, TaskID: p.TaskID, WorkspaceID: p.Claim.Scope.WorkspaceID, Sequence: 1, State: task.AttemptPrepared, DeliveryPhase: DeliveryPhaseNone,
		OpenCodeSessionID: p.OpenCodeSessionID, OpenCodeMessageID: p.OpenCodeMessageID,
		PromptSHA256: promptHash, BaseSHA: p.BaseSHA, ImageDigest: attemptImage, OpenCodeProtocol: attemptProtocol,
		ExecutionContractVersion: p.ExecutionContractVersion, Agent: p.Agent, ModelProvider: p.ModelProvider,
		Model: p.Model, BudgetSnapshot: append(json.RawMessage(nil), p.BudgetSnapshot...), Deadline: fromUnixMillis(unixMillis(p.Deadline)),
		Revision: 1, CreatedAt: fromUnixMillis(acceptedMS), UpdatedAt: fromUnixMillis(acceptedMS),
	}
	receipt := Receipt{
		ID: p.ReceiptID, WorkspaceID: p.Claim.Scope.WorkspaceID, CommandKind: p.Claim.Scope.CommandKind,
		State: ReceiptAccepted, IdempotencyKey: p.Claim.Key, RequestHash: p.Claim.RequestHash, Actor: actor,
		AcceptedAt: fromUnixMillis(acceptedMS), APIContractVersion: p.APIContractVersion,
		TargetType: "task", TargetID: p.TaskID, ResponseStatus: 202, ResponseProjection: response,
	}
	taskEvent := Event{
		ID: p.TaskEventID, Cursor: task.Cursor(taskCursor), WorkspaceID: p.Claim.Scope.WorkspaceID,
		TaskID: p.TaskID, EntityType: "task", EntityID: string(p.TaskID), Type: "task.accepted",
		Version: 1, OccurredAt: fromUnixMillis(acceptedMS), Actor: actor, Payload: taskPayload,
	}
	attemptEvent := Event{
		ID: p.AttemptEventID, Cursor: task.Cursor(attemptCursor), WorkspaceID: p.Claim.Scope.WorkspaceID,
		TaskID: p.TaskID, AttemptID: p.AttemptID, EntityType: "attempt", EntityID: string(p.AttemptID), Type: "attempt.prepared",
		Version: 1, OccurredAt: fromUnixMillis(acceptedMS), Actor: actor, Payload: attemptPayload,
	}
	return Admission{Task: storedTask, Attempt: storedAttempt, Receipt: receipt, TaskEvent: taskEvent, AttemptEvent: attemptEvent}, nil
}

func validateAdmission(p AdmitBackgroundRunParams) error {
	if _, err := task.ParseTaskID(string(p.TaskID)); err != nil {
		return fmt.Errorf("%w: task ID: %v", ErrInvalidInput, err)
	}
	if _, err := task.ParseAttemptID(string(p.AttemptID)); err != nil {
		return fmt.Errorf("%w: attempt ID: %v", ErrInvalidInput, err)
	}
	if _, err := task.ParseReceiptID(string(p.ReceiptID)); err != nil {
		return fmt.Errorf("%w: receipt ID: %v", ErrInvalidInput, err)
	}
	if _, err := task.ParseEventID(string(p.TaskEventID)); err != nil {
		return fmt.Errorf("%w: task event ID: %v", ErrInvalidInput, err)
	}
	if _, err := task.ParseEventID(string(p.AttemptEventID)); err != nil || p.AttemptEventID == p.TaskEventID {
		return fmt.Errorf("%w: attempt event ID: %v", ErrInvalidInput, err)
	}
	if _, err := task.ParseOpenCodeSessionID(string(p.OpenCodeSessionID)); err != nil {
		return fmt.Errorf("%w: OpenCode session ID: %v", ErrInvalidInput, err)
	}
	if _, err := task.ParseOpenCodeMessageID(string(p.OpenCodeMessageID)); err != nil {
		return fmt.Errorf("%w: OpenCode message ID: %v", ErrInvalidInput, err)
	}
	if err := p.Claim.Validate(); err != nil || p.Claim.Scope.CommandKind != CreateBackgroundRunCommand || p.BackgroundRun == nil {
		return fmt.Errorf("%w: idempotency claim: %v", ErrInvalidInput, err)
	}
	if !canonicalBackgroundRemote(p.BackgroundRun.RepositoryRemote) ||
		(p.BackgroundRun.Branch != "" && !validBoundedText(p.BackgroundRun.Branch, 1, 255)) ||
		p.BackgroundRun.Profile != BackgroundRunSourceProfile || p.BackgroundRun.InstructionSHA256 != sha256.Sum256([]byte(p.Prompt)) ||
		p.BackgroundRun.ProfileSHA256 != sha256.Sum256([]byte(p.BackgroundRun.Profile)) || p.BackgroundRun.EnvironmentSHA256 == ([32]byte{}) || p.Claim.Actor.Type != task.ActorOpenCode ||
		!validBackgroundImageIdentity(p.BackgroundRun.ImageIdentity) || !validBoundedText(p.BackgroundRun.CloneIdentity, 1, 256) ||
		!validBoundedText(p.BackgroundRun.VolumeIdentity, 1, 256) || !validBoundedText(p.BackgroundRun.ContainerIdentity, 1, 256) ||
		!validBoundedText(p.BackgroundRun.EndpointIdentity, 1, 256) || !canonicalBackgroundIdentities(p) {
		return fmt.Errorf("%w: background run intent", ErrInvalidInput)
	}
	if !validBoundedText(p.Title, 1, 200) || !utf8.ValidString(p.Prompt) || len(p.Prompt) < 1 || len(p.Prompt) > 64*1024 {
		return fmt.Errorf("%w: title or prompt", ErrInvalidInput)
	}
	if p.RepositoryID == 0 || uint64(p.RepositoryID) > math.MaxInt64 {
		return fmt.Errorf("%w: repository ID", ErrInvalidInput)
	}
	if !validBoundedText(p.BaseRef, 1, 255) {
		return fmt.Errorf("%w: base ref", ErrInvalidInput)
	}
	if _, err := task.ParseGitOID(string(p.BaseSHA)); err != nil {
		return fmt.Errorf("%w: base SHA: %v", ErrInvalidInput, err)
	}
	if p.ObjectFormat != "sha1" || !validBoundedText(p.APIContractVersion, 1, 64) {
		return fmt.Errorf("%w: object or API contract version", ErrInvalidInput)
	}
	if !validBoundedText(p.ExecutionContractVersion, 1, 128) || !validBoundedText(p.Agent, 1, 128) ||
		!validBoundedText(p.ModelProvider, 1, 128) || !validBoundedText(p.Model, 1, 256) {
		return fmt.Errorf("%w: execution or model selection", ErrInvalidInput)
	}
	if len(p.BudgetSnapshot) < 1 || len(p.BudgetSnapshot) > 16*1024 || !json.Valid(p.BudgetSnapshot) {
		return fmt.Errorf("%w: budget snapshot", ErrInvalidInput)
	}
	if err := validTimestamp(p.AcceptedAt); err != nil {
		return err
	}
	if err := validTimestamp(p.Deadline); err != nil || unixMillis(p.Deadline) <= unixMillis(p.AcceptedAt) {
		return fmt.Errorf("%w: attempt deadline", ErrInvalidInput)
	}
	return nil
}

func validBackgroundImageIdentity(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[7:] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func canonicalBackgroundRemote(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && strings.EqualFold(parsed.Host, "github.com") && parsed.User == nil && parsed.RawQuery == "" &&
		parsed.Fragment == "" && parsed.RawPath == "" && parsed.Path != "/" && !strings.HasSuffix(parsed.Path, "/") &&
		!strings.HasSuffix(strings.ToLower(parsed.Path), ".git") && value == "https://"+strings.ToLower(parsed.Host)+parsed.EscapedPath()
}

func canonicalBackgroundIdentities(p AdmitBackgroundRunParams) bool {
	compact := strings.ReplaceAll(strings.TrimPrefix(string(p.TaskID), "tsk_"), "-", "")
	return p.BackgroundRun.CloneIdentity == "run-"+compact+"-g1-clone" &&
		p.BackgroundRun.VolumeIdentity == "fern-run-"+compact+"-g1-opencode" &&
		p.BackgroundRun.ContainerIdentity == "fern-run-"+compact+"-g1" &&
		p.BackgroundRun.EndpointIdentity == "run-"+compact+"-g1-endpoint"
}

func ensureActor(ctx context.Context, tx *sql.Tx, actor task.ActorSnapshot) (int64, error) {
	_, err := tx.ExecContext(ctx, `
INSERT INTO actor_snapshots(actor_type,actor_id,display_name,credential_id,authentication,request_id)
VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		actor.Type, actor.ID, actor.DisplayName, actor.CredentialID, actor.Authentication, actor.RequestID)
	if err != nil {
		return 0, fmt.Errorf("insert actor snapshot: %w", err)
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `
SELECT id FROM actor_snapshots
WHERE actor_type=? AND actor_id=? AND display_name=? AND credential_id=? AND authentication=? AND request_id=?`,
		actor.Type, actor.ID, actor.DisplayName, actor.CredentialID, actor.Authentication, actor.RequestID).Scan(&id); err != nil {
		return 0, fmt.Errorf("read actor snapshot: %w", err)
	}
	return id, nil
}

func receiptByKey(ctx context.Context, tx *sql.Tx, workspaceID task.WorkspaceID, kind string, key task.IdempotencyKey) (Receipt, bool, error) {
	row := tx.QueryRowContext(ctx, receiptSelect+` WHERE r.workspace_id=? AND r.command_kind=? AND r.idempotency_key=?`, workspaceID, kind, key)
	receipt, err := scanReceipt(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, fmt.Errorf("read idempotency receipt: %w", err)
	}
	return receipt, true, nil
}
