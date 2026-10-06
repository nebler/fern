package taskstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nebler/fern/internal/run"
	"github.com/nebler/fern/internal/task"
)

const StopBackgroundRunCommand = "run.stop"
const BackgroundRunStoppedBeforeStart = "background_run_stopped_before_start"

const backgroundRunResourceSpecVersion = run.ResourceSpecVersion

const backgroundRunSelect = `
SELECT r.task_id,r.attempt_id,r.workspace_id,r.generation,r.writer_generation,r.repository_id,r.repository_remote,r.base_oid,r.branch,
       r.instruction_sha256,r.profile,r.profile_sha256,r.environment_sha256,r.resource_spec_version,r.image_identity,r.clone_identity,r.volume_identity,
       r.container_identity,r.endpoint_identity,r.opencode_session_id,r.opencode_message_id,r.state,r.effect_phase,
       r.stop_receipt_id,r.stop_requested_at,r.observed_container_id,r.observed_container_started_at,r.runtime_epoch,r.host_port,
       r.last_evidence,r.last_error,r.prompt_request_attempted_at,r.timeout_requested_at,r.cleanup_proof,
       r.revision,r.created_at,r.updated_at,r.background_seal_request_id,r.artifact_export_id,r.retained_artifact_id,
       r.materialization_id,r.retained_result_id,
       c.actor_type,c.actor_id,c.display_name,c.credential_id,c.authentication,c.request_id,
       s.actor_type,s.actor_id,s.display_name,s.credential_id,s.authentication,s.request_id,
       x.actor_type,x.actor_id,x.display_name,x.credential_id,x.authentication,x.request_id
FROM background_runs r
JOIN actor_snapshots c ON c.id=r.creator_actor_snapshot_id
LEFT JOIN actor_snapshots s ON s.id=r.stop_actor_snapshot_id
LEFT JOIN actor_snapshots x ON x.id=r.timeout_actor_snapshot_id`

func (s *Store) GetBackgroundRun(ctx context.Context, workspaceID task.WorkspaceID, taskID task.TaskID, actor task.ActorSnapshot) (BackgroundRun, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil {
		return BackgroundRun{}, fmt.Errorf("%w: background run workspace", ErrInvalidInput)
	}
	if _, err := task.ParseTaskID(string(taskID)); err != nil || actor.Validate() != nil || !backgroundRunReader(actor.Type) {
		return BackgroundRun{}, fmt.Errorf("%w: background run identity", ErrInvalidInput)
	}
	query := backgroundRunSelect + ` WHERE r.workspace_id=? AND r.task_id=?`
	arguments := []any{workspaceID, taskID}
	if actor.Type == task.ActorOpenCode {
		query += ` AND c.actor_type=? AND c.actor_id=? AND c.credential_id=? AND c.authentication=?`
		arguments = append(arguments, actor.Type, actor.ID, actor.CredentialID, actor.Authentication)
	}
	run, err := scanBackgroundRun(s.db.QueryRowContext(ctx, query, arguments...))
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRun{}, ErrNotFound
	}
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("read background run: %w", err)
	}
	return run, nil
}

// ReadBackgroundRunLifecycle returns the (state, phase) of one run for the
// trusted in-process coordinator. It exposes no task plaintext and authorizes
// no actor.
func (s *Store) ReadBackgroundRunLifecycle(ctx context.Context, workspaceID task.WorkspaceID, taskID task.TaskID) (BackgroundRunState, BackgroundRunEffectPhase, error) {
	run, err := scanBackgroundRun(s.db.QueryRowContext(ctx, backgroundRunSelect+` WHERE r.workspace_id=? AND r.task_id=?`, workspaceID, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("read background run lifecycle: %w", err)
	}
	return run.State, run.EffectPhase, nil
}

// GetBackgroundRunOwners returns the exact parent revisions only after the
// same ownership-hiding check used by GetBackgroundRun.
func (s *Store) GetBackgroundRunOwners(ctx context.Context, workspaceID task.WorkspaceID, taskID task.TaskID, actor task.ActorSnapshot) (Task, Attempt, error) {
	run, err := s.GetBackgroundRun(ctx, workspaceID, taskID, actor)
	if err != nil {
		return Task{}, Attempt{}, err
	}
	owner, err := getTask(ctx, s.db, run.TaskID)
	if err != nil {
		return Task{}, Attempt{}, err
	}
	attempt, err := getAttempt(ctx, s.db, run.AttemptID)
	return owner, attempt, err
}

const MaxBackgroundRunListLimit = 100

// ListBackgroundRuns applies plugin ownership in SQL before its bound. Trusted
// operator/device actors receive the workspace-wide operator projection.
func (s *Store) ListBackgroundRuns(ctx context.Context, workspaceID task.WorkspaceID, actor task.ActorSnapshot, limit int) ([]BackgroundRun, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil || actor.Validate() != nil || !backgroundRunReader(actor.Type) || limit < 1 || limit > MaxBackgroundRunListLimit {
		return nil, fmt.Errorf("%w: background run list", ErrInvalidInput)
	}
	query := backgroundRunSelect + ` WHERE r.workspace_id=?`
	arguments := []any{workspaceID}
	if actor.Type == task.ActorOpenCode {
		query += ` AND c.actor_type=? AND c.actor_id=? AND c.credential_id=? AND c.authentication=?`
		arguments = append(arguments, actor.Type, actor.ID, actor.CredentialID, actor.Authentication)
	}
	query += ` ORDER BY r.created_at DESC,r.task_id DESC LIMIT ?`
	arguments = append(arguments, limit)
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("list background runs: %w", err)
	}
	defer rows.Close()
	runs := make([]BackgroundRun, 0)
	for rows.Next() {
		run, scanErr := scanBackgroundRun(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan background run list: %w", scanErr)
		}
		if len(runs) == cap(runs) {
			// Grow only after a successful scan, without exceeding the query bound.
			// Starting at one avoids reserving excess space for sparse lists.
			grown := make([]BackgroundRun, len(runs), min(limit, max(1, 2*cap(runs))))
			copy(grown, runs)
			runs = grown
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate background run list: %w", err)
	}
	return runs, nil
}

func backgroundRunReader(actorType task.ActorType) bool {
	return actorType == task.ActorOpenCode || actorType == task.ActorDevice || actorType == task.ActorOperator
}

func (s *Store) StopBackgroundRun(ctx context.Context, p StopBackgroundRunParams) (_ BackgroundRunStop, err error) {
	if err := validateBackgroundRunStop(p); err != nil {
		return BackgroundRunStop{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRunStop{}, fmt.Errorf("begin background run stop: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	existing, found, err := receiptByKey(ctx, tx, p.Claim.Scope.WorkspaceID, p.Claim.Scope.CommandKind, p.Claim.Key)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	if found {
		disposition, classifyErr := task.ClassifyIdempotency(&task.IdempotencyClaim{
			Scope: task.IdempotencyScope{WorkspaceID: existing.WorkspaceID, CommandKind: existing.CommandKind},
			Key:   existing.IdempotencyKey, RequestHash: existing.RequestHash, Actor: existing.Actor,
		}, p.Claim)
		if classifyErr != nil {
			return BackgroundRunStop{}, classifyErr
		}
		switch disposition {
		case task.IdempotencyReplay:
			if existing.TargetID != p.TaskID {
				return BackgroundRunStop{}, ErrIdempotencyConflict
			}
			run, getErr := getBackgroundRunOwned(ctx, tx, p.WorkspaceID, existing.TargetID, p.Claim.Actor)
			if getErr != nil || run.StopReceiptID != existing.ID {
				return BackgroundRunStop{}, fmt.Errorf("%w: background run stop replay", ErrCorruptStore)
			}
			if err := tx.Commit(); err != nil {
				return BackgroundRunStop{}, err
			}
			return BackgroundRunStop{Run: run, Receipt: existing, Replayed: true}, nil
		case task.IdempotencyOwnerMismatch:
			return BackgroundRunStop{}, ErrNotFound
		case task.IdempotencyConflict:
			return BackgroundRunStop{}, ErrIdempotencyConflict
		default:
			return BackgroundRunStop{}, ErrCorruptStore
		}
	}

	run, err := getBackgroundRunOwned(ctx, tx, p.WorkspaceID, p.TaskID, p.Claim.Actor)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	queuedStop := run.EffectPhase == BackgroundRunEffectAbsent
	activeStop := run.EffectPhase == BackgroundRunEffectProvisioning || run.EffectPhase == BackgroundRunEffectPromptPending || run.EffectPhase == BackgroundRunEffectAdmitted
	if run.StopReceiptID != "" || (!queuedStop && !activeStop) {
		return BackgroundRunStop{}, ErrInvalidState
	}
	owner, err := getTask(ctx, tx, run.TaskID)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	attempt, err := getAttempt(ctx, tx, run.AttemptID)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	if owner.WorkspaceID != p.WorkspaceID || owner.CurrentAttemptID != attempt.ID || owner.State != task.TaskQueued ||
		attempt.TaskID != owner.ID || attempt.WorkspaceID != owner.WorkspaceID ||
		attempt.ID != run.AttemptID || attempt.Sequence != run.Generation || attempt.State != task.AttemptPrepared {
		return BackgroundRunStop{}, ErrInvalidState
	}
	actorID, err := ensureActor(ctx, tx, p.Claim.Actor)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	stopState := BackgroundRunFailed
	if activeStop {
		stopState = BackgroundRunCanceling
	}
	response, _ := json.Marshal(struct {
		RunID task.TaskID        `json:"run_id"`
		State BackgroundRunState `json:"state"`
	}{run.TaskID, stopState})
	now := unixMillis(p.StoppedAt)
	if _, err := tx.ExecContext(ctx, `INSERT INTO receipts(
id,workspace_id,command_kind,state,idempotency_key,request_hash,actor_snapshot_id,accepted_at,
api_contract_version,target_type,target_id,response_status,response_projection)
VALUES(?,?,?,'accepted',?,?,?,?,?,'task',?,202,?)`, p.ReceiptID, run.WorkspaceID, StopBackgroundRunCommand,
		p.Claim.Key, p.Claim.RequestHash[:], actorID, now, p.APIContractVersion, run.TaskID, string(response)); err != nil {
		return BackgroundRunStop{}, fmt.Errorf("insert background run stop receipt: %w", err)
	}
	if activeStop {
		result, updateErr := tx.ExecContext(ctx, `UPDATE background_runs SET state='canceling',effect_phase='cleaning',
stop_receipt_id=?,stop_actor_snapshot_id=?,stop_requested_at=?,revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND stop_receipt_id IS NULL AND revision=? AND
effect_phase IN ('provisioning','prompt_pending','admitted')`, p.ReceiptID, actorID, now, now,
			run.TaskID, run.AttemptID, run.WorkspaceID, run.Generation, run.Revision)
		if updateErr != nil {
			return BackgroundRunStop{}, fmt.Errorf("request active background run stop: %w", updateErr)
		}
		if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
			return BackgroundRunStop{}, ErrInvalidState
		}
		stored, getErr := getBackgroundRunOwned(ctx, tx, p.WorkspaceID, run.TaskID, p.Claim.Actor)
		if getErr != nil {
			return BackgroundRunStop{}, getErr
		}
		if err := tx.Commit(); err != nil {
			return BackgroundRunStop{}, fmt.Errorf("commit active background run stop: %w", err)
		}
		return BackgroundRunStop{Run: stored, Receipt: Receipt{ID: p.ReceiptID, WorkspaceID: run.WorkspaceID,
			CommandKind: StopBackgroundRunCommand, State: ReceiptAccepted, IdempotencyKey: p.Claim.Key,
			RequestHash: p.Claim.RequestHash, Actor: p.Claim.Actor, AcceptedAt: fromUnixMillis(now),
			APIContractVersion: p.APIContractVersion, TargetType: "task", TargetID: run.TaskID,
			ResponseStatus: 202, ResponseProjection: response}}, nil
	}
	payload, err := json.Marshal(struct {
		RunID         task.TaskID    `json:"runId"`
		Reason        string         `json:"reason"`
		StopReceiptID task.ReceiptID `json:"stopReceiptId"`
	}{run.TaskID, BackgroundRunStoppedBeforeStart, p.ReceiptID})
	if err != nil {
		return BackgroundRunStop{}, fmt.Errorf("encode background run stop event: %w", err)
	}
	attemptEvent, err := insertAttemptEvent(ctx, tx, p.AttemptEventID, attempt, "attempt.failed", now, actorID, payload)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	taskEvent, err := insertTaskEvent(ctx, tx, p.TaskEventID, owner, "task.failed", now, actorID, payload)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	if attemptEvent.Cursor >= taskEvent.Cursor {
		return BackgroundRunStop{}, ErrCorruptStore
	}
	result, err := tx.ExecContext(ctx, `UPDATE attempts SET state='failed',terminal_reason=?,revision=revision+1,updated_at=?
WHERE id=? AND task_id=? AND workspace_id=? AND state='prepared' AND revision=?`,
		BackgroundRunStoppedBeforeStart, now, attempt.ID, owner.ID, owner.WorkspaceID, attempt.Revision)
	if err != nil {
		return BackgroundRunStop{}, fmt.Errorf("terminalize background run attempt: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunStop{}, ErrInvalidState
	}
	result, err = tx.ExecContext(ctx, `UPDATE tasks SET state='failed',terminal_reason=?,latest_event_cursor=?,revision=revision+1,updated_at=?
WHERE id=? AND workspace_id=? AND state='queued' AND current_attempt_id=? AND revision=?`,
		BackgroundRunStoppedBeforeStart, taskEvent.Cursor, now, owner.ID, owner.WorkspaceID, attempt.ID, owner.Revision)
	if err != nil {
		return BackgroundRunStop{}, fmt.Errorf("terminalize background run task: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunStop{}, ErrInvalidState
	}
	result, err = tx.ExecContext(ctx, `UPDATE background_runs SET state='failed',effect_phase='cleanup_complete',
stop_receipt_id=?,stop_actor_snapshot_id=?,stop_requested_at=?,cleanup_proof='queued:no_effect_claim',last_error=?,revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND state='queued' AND effect_phase='absent' AND stop_receipt_id IS NULL AND revision=?`,
		p.ReceiptID, actorID, now, BackgroundRunStoppedBeforeStart, now, run.TaskID, run.AttemptID, run.WorkspaceID, run.Generation, run.Revision)
	if err != nil {
		return BackgroundRunStop{}, fmt.Errorf("fence background run stop: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunStop{}, ErrInvalidState
	}
	stored, err := getBackgroundRunOwned(ctx, tx, p.WorkspaceID, run.TaskID, p.Claim.Actor)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRunStop{}, fmt.Errorf("commit background run stop: %w", err)
	}
	return BackgroundRunStop{Run: stored, Receipt: Receipt{ID: p.ReceiptID, WorkspaceID: run.WorkspaceID,
		CommandKind: StopBackgroundRunCommand, State: ReceiptAccepted, IdempotencyKey: p.Claim.Key,
		RequestHash: p.Claim.RequestHash, Actor: p.Claim.Actor, AcceptedAt: fromUnixMillis(now),
		APIContractVersion: p.APIContractVersion, TargetType: "task", TargetID: run.TaskID,
		ResponseStatus: 202, ResponseProjection: response}}, nil
}

func validateBackgroundRunStop(p StopBackgroundRunParams) error {
	if _, err := task.ParseWorkspaceID(string(p.WorkspaceID)); err != nil || p.WorkspaceID != p.Claim.Scope.WorkspaceID {
		return fmt.Errorf("%w: run workspace", ErrInvalidInput)
	}
	if _, err := task.ParseTaskID(string(p.TaskID)); err != nil {
		return fmt.Errorf("%w: run ID", ErrInvalidInput)
	}
	if _, err := task.ParseReceiptID(string(p.ReceiptID)); err != nil {
		return fmt.Errorf("%w: receipt ID", ErrInvalidInput)
	}
	if _, err := task.ParseEventID(string(p.AttemptEventID)); err != nil {
		return fmt.Errorf("%w: attempt event ID", ErrInvalidInput)
	}
	if _, err := task.ParseEventID(string(p.TaskEventID)); err != nil || p.TaskEventID == p.AttemptEventID {
		return fmt.Errorf("%w: task event ID", ErrInvalidInput)
	}
	if err := p.Claim.Validate(); err != nil || p.Claim.Scope.CommandKind != StopBackgroundRunCommand || p.Claim.Actor.Type != task.ActorOpenCode {
		return fmt.Errorf("%w: run stop claim", ErrInvalidInput)
	}
	if !validBoundedText(p.APIContractVersion, 1, 64) {
		return fmt.Errorf("%w: API contract version", ErrInvalidInput)
	}
	return validExactTimestamp(p.StoppedAt)
}

func getBackgroundRunOwned(ctx context.Context, q queryRower, workspaceID task.WorkspaceID, taskID task.TaskID, actor task.ActorSnapshot) (BackgroundRun, error) {
	run, err := scanBackgroundRun(q.QueryRowContext(ctx, backgroundRunSelect+`
WHERE r.workspace_id=? AND r.task_id=? AND c.actor_type=? AND c.actor_id=? AND c.credential_id=? AND c.authentication=?`,
		workspaceID, taskID, actor.Type, actor.ID, actor.CredentialID, actor.Authentication))
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRun{}, ErrNotFound
	}
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("read owned background run: %w", err)
	}
	return run, nil
}

func scanBackgroundRun(row rowScanner) (BackgroundRun, error) {
	var run BackgroundRun
	var repositoryID int64
	var branch, stopReceipt, containerID, containerStarted, evidence, lastError, cleanupProof sql.NullString
	var sealRequestID, artifactExportID, retainedArtifactID, materializationID, retainedResultID sql.NullString
	var stopAt, runtimeEpoch, hostPort, promptAttempted, timeoutRequested sql.NullInt64
	var instructionHash, profileHash, environmentHash []byte
	var created, updated int64
	var stopType, stopID, stopName, stopCredential, stopAuth, stopRequest sql.NullString
	var timeoutType, timeoutID, timeoutName, timeoutCredential, timeoutAuth, timeoutRequest sql.NullString
	err := row.Scan(&run.TaskID, &run.AttemptID, &run.WorkspaceID, &run.Generation, &run.WriterGeneration, &repositoryID,
		&run.RepositoryRemote, &run.BaseOID, &branch, &instructionHash, &run.Profile, &profileHash, &environmentHash, &run.ResourceSpecVersion,
		&run.ImageIdentity, &run.CloneIdentity, &run.VolumeIdentity, &run.ContainerIdentity, &run.EndpointIdentity,
		&run.OpenCodeSessionID, &run.OpenCodeMessageID, &run.State, &run.EffectPhase,
		&stopReceipt, &stopAt, &containerID, &containerStarted, &runtimeEpoch, &hostPort,
		&evidence, &lastError, &promptAttempted, &timeoutRequested, &cleanupProof,
		&run.Revision, &created, &updated, &sealRequestID, &artifactExportID, &retainedArtifactID, &materializationID, &retainedResultID,
		&run.Creator.Type, &run.Creator.ID, &run.Creator.DisplayName, &run.Creator.CredentialID, &run.Creator.Authentication, &run.Creator.RequestID,
		&stopType, &stopID, &stopName, &stopCredential, &stopAuth, &stopRequest,
		&timeoutType, &timeoutID, &timeoutName, &timeoutCredential, &timeoutAuth, &timeoutRequest)
	if err != nil {
		return BackgroundRun{}, err
	}
	if len(instructionHash) != 32 || len(profileHash) != 32 || len(environmentHash) != 32 || bytes.Equal(environmentHash, make([]byte, 32)) ||
		run.ResourceSpecVersion != backgroundRunResourceSpecVersion || repositoryID <= 0 || run.Generation <= 0 || run.WriterGeneration != 1 ||
		!validBackgroundRunStatePhase(run.Profile, run.State, run.EffectPhase) || run.Creator.Validate() != nil || run.Creator.Type != task.ActorOpenCode {
		return BackgroundRun{}, ErrCorruptStore
	}
	copy(run.InstructionSHA256[:], instructionHash)
	copy(run.ProfileSHA256[:], profileHash)
	copy(run.EnvironmentSHA256[:], environmentHash)
	run.RepositoryID = task.RepositoryID(repositoryID)
	run.Branch = nullableString(branch)
	run.CreatedAt, run.UpdatedAt = fromUnixMillis(created), fromUnixMillis(updated)
	run.ObservedContainerID = nullableText(containerID)
	run.ObservedContainerStartedAt = nullableText(containerStarted)
	run.RuntimeEpoch = runtimeEpoch.Int64
	run.HostPort = int(hostPort.Int64)
	run.LastEvidence = nullableText(evidence)
	run.LastError = nullableText(lastError)
	run.PromptRequestAttemptedAt = nullableTime(promptAttempted)
	run.TimeoutRequestedAt = nullableTime(timeoutRequested)
	run.CleanupProof = nullableText(cleanupProof)
	run.BackgroundSealRequestID = task.SealRequestID(nullableText(sealRequestID))
	run.ArtifactExportID = task.ArtifactExportID(nullableText(artifactExportID))
	run.RetainedArtifactID = task.RetainedArtifactID(nullableText(retainedArtifactID))
	run.MaterializationID = task.MaterializationID(nullableText(materializationID))
	run.RetainedResultID = task.ResultID(nullableText(retainedResultID))
	if stopReceipt.Valid {
		if !stopAt.Valid || !stopType.Valid || !stopID.Valid || !stopCredential.Valid || !stopAuth.Valid || !stopRequest.Valid {
			return BackgroundRun{}, ErrCorruptStore
		}
		run.StopReceiptID = task.ReceiptID(stopReceipt.String)
		run.StopRequestedAt = nullableTime(stopAt)
		actor := task.ActorSnapshot{Type: task.ActorType(stopType.String), ID: stopID.String, DisplayName: stopName.String,
			CredentialID: stopCredential.String, Authentication: stopAuth.String, RequestID: stopRequest.String}
		if actor.Validate() != nil {
			return BackgroundRun{}, ErrCorruptStore
		}
		run.StopActor = &actor
	}
	if timeoutRequested.Valid {
		if !timeoutType.Valid || !timeoutID.Valid || !timeoutCredential.Valid || !timeoutAuth.Valid || !timeoutRequest.Valid {
			return BackgroundRun{}, ErrCorruptStore
		}
		actor := task.ActorSnapshot{Type: task.ActorType(timeoutType.String), ID: timeoutID.String, DisplayName: timeoutName.String,
			CredentialID: timeoutCredential.String, Authentication: timeoutAuth.String, RequestID: timeoutRequest.String}
		if actor.Validate() != nil || actor.Type != task.ActorSystem {
			return BackgroundRun{}, ErrCorruptStore
		}
		run.TimeoutActor = &actor
	} else if timeoutType.Valid || timeoutID.Valid || timeoutCredential.Valid || timeoutAuth.Valid || timeoutRequest.Valid {
		return BackgroundRun{}, ErrCorruptStore
	}
	return run, nil
}

func validBackgroundRunStatePhase(profile string, state BackgroundRunState, phase BackgroundRunEffectPhase) bool {
	if profile != BackgroundRunSourceProfile {
		return false
	}
	return run.Classify(run.State(state), run.Phase(phase)).Valid
}

func nullableText(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}
