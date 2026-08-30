package taskstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

const StopBackgroundRunCommand = "run.stop"
const BackgroundRunStoppedBeforeStart = "background_run_stopped_before_start"

const backgroundRunSelect = `
SELECT r.task_id,r.attempt_id,r.workspace_id,r.generation,r.repository_id,r.repository_remote,r.base_oid,r.branch,
       r.instruction_sha256,r.profile,r.profile_sha256,r.image_identity,r.clone_identity,r.volume_identity,
       r.container_identity,r.endpoint_identity,r.opencode_session_id,r.opencode_message_id,r.state,r.effect_phase,
       r.cancel_epoch,r.stop_receipt_id,r.stop_requested_at,r.revision,r.created_at,r.updated_at,
       c.actor_type,c.actor_id,c.display_name,c.credential_id,c.authentication,c.request_id,
       s.actor_type,s.actor_id,s.display_name,s.credential_id,s.authentication,s.request_id
FROM background_runs r
JOIN actor_snapshots c ON c.id=r.creator_actor_snapshot_id
LEFT JOIN actor_snapshots s ON s.id=r.stop_actor_snapshot_id`

func (s *Store) GetBackgroundRun(ctx context.Context, workspaceID task.WorkspaceID, taskID task.TaskID, actor task.ActorSnapshot) (BackgroundRun, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil {
		return BackgroundRun{}, fmt.Errorf("%w: background run workspace", ErrInvalidInput)
	}
	if _, err := task.ParseTaskID(string(taskID)); err != nil || actor.Validate() != nil || actor.Type != task.ActorOpenCode {
		return BackgroundRun{}, fmt.Errorf("%w: background run identity", ErrInvalidInput)
	}
	run, err := scanBackgroundRun(s.db.QueryRowContext(ctx, backgroundRunSelect+`
WHERE r.workspace_id=? AND r.task_id=? AND c.actor_type=? AND c.actor_id=? AND c.credential_id=? AND c.authentication=?`,
		workspaceID, taskID, actor.Type, actor.ID, actor.CredentialID, actor.Authentication))
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRun{}, ErrNotFound
	}
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("read background run: %w", err)
	}
	return run, nil
}

const MaxBackgroundRunListLimit = 100

// ListBackgroundRuns applies actor authority in SQL before its bound, so runs
// owned by another plugin credential can neither consume nor escape the page.
func (s *Store) ListBackgroundRuns(ctx context.Context, workspaceID task.WorkspaceID, actor task.ActorSnapshot, limit int) ([]BackgroundRun, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil || actor.Validate() != nil || actor.Type != task.ActorOpenCode || limit < 1 || limit > MaxBackgroundRunListLimit {
		return nil, fmt.Errorf("%w: background run list", ErrInvalidInput)
	}
	rows, err := s.db.QueryContext(ctx, backgroundRunSelect+`
WHERE r.workspace_id=? AND c.actor_type=? AND c.actor_id=? AND c.credential_id=? AND c.authentication=?
ORDER BY r.created_at DESC,r.task_id DESC LIMIT ?`, workspaceID, actor.Type, actor.ID, actor.CredentialID, actor.Authentication, limit)
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
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate background run list: %w", err)
	}
	return runs, nil
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
	if run.State != BackgroundRunQueued || run.CancelEpoch != 0 || run.EffectPhase != BackgroundRunEffectAbsent {
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
		owner.CancelEpoch != 0 || attempt.TaskID != owner.ID || attempt.WorkspaceID != owner.WorkspaceID ||
		attempt.State != task.AttemptPrepared || attempt.DeliveryPhase != DeliveryPhaseNone {
		return BackgroundRunStop{}, ErrInvalidState
	}
	actorID, err := ensureActor(ctx, tx, p.Claim.Actor)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	response, _ := json.Marshal(struct {
		RunID task.TaskID        `json:"run_id"`
		State BackgroundRunState `json:"state"`
	}{run.TaskID, BackgroundRunFailed})
	now := unixMillis(p.StoppedAt)
	if _, err := tx.ExecContext(ctx, `INSERT INTO receipts(
id,workspace_id,command_kind,state,idempotency_key,request_hash,actor_snapshot_id,accepted_at,
api_contract_version,target_type,target_id,response_status,response_projection)
VALUES(?,?,?,'accepted',?,?,?,?,?,'task',?,202,?)`, p.ReceiptID, run.WorkspaceID, StopBackgroundRunCommand,
		p.Claim.Key, p.Claim.RequestHash[:], actorID, now, p.APIContractVersion, run.TaskID, string(response)); err != nil {
		return BackgroundRunStop{}, fmt.Errorf("insert background run stop receipt: %w", err)
	}
	payload, err := json.Marshal(struct {
		RunID  task.TaskID `json:"runId"`
		Reason string      `json:"reason"`
	}{run.TaskID, BackgroundRunStoppedBeforeStart})
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
WHERE id=? AND task_id=? AND workspace_id=? AND state='prepared' AND delivery_phase='none' AND revision=?`,
		BackgroundRunStoppedBeforeStart, now, attempt.ID, owner.ID, owner.WorkspaceID, attempt.Revision)
	if err != nil {
		return BackgroundRunStop{}, fmt.Errorf("terminalize background run attempt: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunStop{}, ErrInvalidState
	}
	result, err = tx.ExecContext(ctx, `UPDATE tasks SET state='failed',terminal_reason=?,latest_event_cursor=?,revision=revision+1,updated_at=?
WHERE id=? AND workspace_id=? AND state='queued' AND cancel_epoch=0 AND current_attempt_id=? AND revision=?`,
		BackgroundRunStoppedBeforeStart, taskEvent.Cursor, now, owner.ID, owner.WorkspaceID, attempt.ID, owner.Revision)
	if err != nil {
		return BackgroundRunStop{}, fmt.Errorf("terminalize background run task: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunStop{}, ErrInvalidState
	}
	result, err = tx.ExecContext(ctx, `UPDATE background_runs SET state='failed',cancel_epoch=1,
stop_receipt_id=?,stop_actor_snapshot_id=?,stop_requested_at=?,revision=revision+1,updated_at=?
WHERE task_id=? AND state='queued' AND effect_phase='absent' AND cancel_epoch=0 AND revision=?`,
		p.ReceiptID, actorID, now, now, run.TaskID, run.Revision)
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

func backgroundAttemptExists(ctx context.Context, q queryRower, attemptID task.AttemptID) (bool, error) {
	var exists int
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM background_runs WHERE attempt_id=?)`, attemptID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("inspect background run ownership: %w", err)
	}
	return exists == 1, nil
}

func scanBackgroundRun(row rowScanner) (BackgroundRun, error) {
	var run BackgroundRun
	var repositoryID, cancelEpoch int64
	var branch, stopReceipt sql.NullString
	var stopAt sql.NullInt64
	var instructionHash, profileHash []byte
	var created, updated int64
	var stopType, stopID, stopName, stopCredential, stopAuth, stopRequest sql.NullString
	err := row.Scan(&run.TaskID, &run.AttemptID, &run.WorkspaceID, &run.Generation, &repositoryID,
		&run.RepositoryRemote, &run.BaseOID, &branch, &instructionHash, &run.Profile, &profileHash,
		&run.ImageIdentity, &run.CloneIdentity, &run.VolumeIdentity, &run.ContainerIdentity, &run.EndpointIdentity,
		&run.OpenCodeSessionID, &run.OpenCodeMessageID, &run.State, &run.EffectPhase, &cancelEpoch,
		&stopReceipt, &stopAt, &run.Revision, &created, &updated,
		&run.Creator.Type, &run.Creator.ID, &run.Creator.DisplayName, &run.Creator.CredentialID, &run.Creator.Authentication, &run.Creator.RequestID,
		&stopType, &stopID, &stopName, &stopCredential, &stopAuth, &stopRequest)
	if err != nil {
		return BackgroundRun{}, err
	}
	if len(instructionHash) != 32 || len(profileHash) != 32 || repositoryID <= 0 || run.Generation <= 0 || cancelEpoch < 0 ||
		!run.State.valid() || !run.EffectPhase.valid() || !validBackgroundRunStatePhase(run.State, run.EffectPhase) || run.Creator.Validate() != nil || run.Creator.Type != task.ActorOpenCode {
		return BackgroundRun{}, ErrCorruptStore
	}
	copy(run.InstructionSHA256[:], instructionHash)
	copy(run.ProfileSHA256[:], profileHash)
	run.RepositoryID = task.RepositoryID(repositoryID)
	run.CancelEpoch = uint64(cancelEpoch)
	run.Branch = nullableString(branch)
	run.CreatedAt, run.UpdatedAt = fromUnixMillis(created), fromUnixMillis(updated)
	if cancelEpoch == 1 {
		if !stopReceipt.Valid || !stopAt.Valid || !stopType.Valid || !stopID.Valid || !stopCredential.Valid || !stopAuth.Valid || !stopRequest.Valid {
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
	return run, nil
}

func validBackgroundRunStatePhase(state BackgroundRunState, phase BackgroundRunEffectPhase) bool {
	switch state {
	case BackgroundRunQueued:
		return phase == BackgroundRunEffectAbsent
	case BackgroundRunSettingUp:
		return phase == BackgroundRunEffectProvisionStarted
	case BackgroundRunWorking, BackgroundRunNeedsYou:
		return phase == BackgroundRunEffectPromptStarted
	case BackgroundRunCanceling:
		return phase == BackgroundRunEffectStopStarted
	case BackgroundRunUncertain:
		return phase != BackgroundRunEffectAbsent
	case BackgroundRunResultReady:
		return phase == BackgroundRunEffectExportStarted || phase == BackgroundRunEffectCleanupStarted
	case BackgroundRunFailed:
		return true
	case BackgroundRunCleanupRequired:
		return phase == BackgroundRunEffectCleanupStarted
	default:
		return false
	}
}
