package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nebler/fern/internal/task"
)

const backgroundRunControlSelect = `SELECT receipt_id,task_id,attempt_id,workspace_id,run_generation,command_kind,state,
writer_generation,container_id,container_started_at,runtime_epoch,runtime_token,opencode_session_id,opencode_message_id,instruction,
attempted_at,completed_at,last_error,claim_owner,claim_expires_at,claim_generation,revision,created_at,updated_at
FROM background_run_controls`

func (s *Store) AdmitBackgroundRunControl(ctx context.Context, p AdmitBackgroundRunControlParams) (_ BackgroundRunControlAdmission, err error) {
	if _, parseErr := task.ParseWorkspaceID(string(p.WorkspaceID)); parseErr != nil || p.WorkspaceID != p.Claim.Scope.WorkspaceID ||
		p.Claim.Validate() != nil || (p.Claim.Scope.CommandKind != InterruptBackgroundRunCommand && p.Claim.Scope.CommandKind != SteerBackgroundRunCommand) ||
		(p.Claim.Actor.Type != task.ActorDevice && p.Claim.Actor.Type != task.ActorOperator) || !validBoundedText(p.APIContractVersion, 1, 64) ||
		validExactTimestamp(p.RequestedAt) != nil {
		return BackgroundRunControlAdmission{}, fmt.Errorf("%w: background run control", ErrInvalidInput)
	}
	if _, parseErr := task.ParseTaskID(string(p.TaskID)); parseErr != nil {
		return BackgroundRunControlAdmission{}, fmt.Errorf("%w: background run control task", ErrInvalidInput)
	}
	if _, parseErr := task.ParseReceiptID(string(p.ReceiptID)); parseErr != nil {
		return BackgroundRunControlAdmission{}, fmt.Errorf("%w: background run control receipt", ErrInvalidInput)
	}
	if p.Claim.Scope.CommandKind == SteerBackgroundRunCommand {
		if _, parseErr := task.ParseOpenCodeMessageID(string(p.OpenCodeMessageID)); parseErr != nil || !validControlInstruction(p.Instruction) {
			return BackgroundRunControlAdmission{}, fmt.Errorf("%w: steer identity or instruction", ErrInvalidInput)
		}
	} else if p.OpenCodeMessageID != "" || p.Instruction != "" {
		return BackgroundRunControlAdmission{}, fmt.Errorf("%w: interrupt payload", ErrInvalidInput)
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRunControlAdmission{}, err
	}
	defer release()
	defer rollback(tx, &err)
	existing, found, err := receiptByKey(ctx, tx, p.WorkspaceID, p.Claim.Scope.CommandKind, p.Claim.Key)
	if err != nil {
		return BackgroundRunControlAdmission{}, err
	}
	if found {
		disposition, classifyErr := task.ClassifyIdempotency(&task.IdempotencyClaim{
			Scope: task.IdempotencyScope{WorkspaceID: existing.WorkspaceID, CommandKind: existing.CommandKind}, Key: existing.IdempotencyKey,
			RequestHash: existing.RequestHash, Actor: existing.Actor,
		}, p.Claim)
		if classifyErr != nil {
			return BackgroundRunControlAdmission{}, classifyErr
		}
		if disposition == task.IdempotencyOwnerMismatch {
			return BackgroundRunControlAdmission{}, ErrNotFound
		}
		if disposition != task.IdempotencyReplay || existing.TargetID != p.TaskID {
			return BackgroundRunControlAdmission{}, ErrIdempotencyConflict
		}
		control, readErr := getBackgroundRunControl(ctx, tx, existing.ID)
		if readErr != nil {
			return BackgroundRunControlAdmission{}, readErr
		}
		if err := tx.Commit(); err != nil {
			return BackgroundRunControlAdmission{}, err
		}
		return BackgroundRunControlAdmission{Control: control, Receipt: existing, Replayed: true}, nil
	}
	run, err := readBackgroundRunExact(ctx, tx, p.WorkspaceID, p.TaskID)
	if err != nil {
		return BackgroundRunControlAdmission{}, err
	}
	ownership, err := getBackgroundRunOwnership(ctx, tx, p.WorkspaceID, p.TaskID)
	if err != nil {
		return BackgroundRunControlAdmission{}, err
	}
	if ownership.Mode != BackgroundRunAgentOwned || ownership.Phase != BackgroundRunOwnershipAgentActive || run.CancelEpoch != 0 ||
		run.EffectPhase != BackgroundRunEffectPromptAdmitted || (run.State != BackgroundRunWorking && run.State != BackgroundRunNeedsYou && run.State != BackgroundRunUncertain) {
		return BackgroundRunControlAdmission{}, ErrInvalidState
	}
	var activeControls int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM background_run_controls WHERE task_id=? AND state IN ('requested','attempted')`, run.TaskID).Scan(&activeControls); err != nil {
		return BackgroundRunControlAdmission{}, err
	}
	if activeControls != 0 {
		return BackgroundRunControlAdmission{}, ErrInvalidState
	}
	containerID, startedAt, runtimeEpoch, runtimeToken, sessionID := ownership.ContainerID, ownership.ContainerStartedAt, ownership.RuntimeEpoch, ownership.RuntimeToken, ownership.OpenCodeSessionID
	if containerID == "" {
		containerID, startedAt, runtimeEpoch, sessionID = run.ObservedContainerID, run.ObservedContainerStartedAt, run.RuntimeEpoch, run.OpenCodeSessionID
		digest := sha256.Sum256([]byte(containerID + "\x00" + startedAt))
		runtimeToken = hex.EncodeToString(digest[:])
	}
	started, parseErr := time.Parse(time.RFC3339Nano, startedAt)
	digest := sha256.Sum256([]byte(containerID + "\x00" + startedAt))
	if containerID == "" || parseErr != nil || started.UnixNano() != runtimeEpoch || runtimeToken != hex.EncodeToString(digest[:]) || sessionID == "" {
		return BackgroundRunControlAdmission{}, ErrInvalidState
	}
	actorID, err := ensureActor(ctx, tx, p.Claim.Actor)
	if err != nil {
		return BackgroundRunControlAdmission{}, err
	}
	now := unixMillis(p.RequestedAt)
	projection, _ := json.Marshal(struct {
		RunID     task.TaskID               `json:"run_id"`
		ControlID task.ReceiptID            `json:"control_id"`
		State     BackgroundRunControlState `json:"state"`
	}{p.TaskID, p.ReceiptID, BackgroundRunControlRequested})
	if _, err := tx.ExecContext(ctx, `INSERT INTO receipts(
id,workspace_id,command_kind,state,idempotency_key,request_hash,actor_snapshot_id,accepted_at,api_contract_version,target_type,target_id,response_status,response_projection)
VALUES(?,?,?,'accepted',?,?,?,?,?,'task',?,202,?)`, p.ReceiptID, p.WorkspaceID, p.Claim.Scope.CommandKind, p.Claim.Key,
		p.Claim.RequestHash[:], actorID, now, p.APIContractVersion, p.TaskID, string(projection)); err != nil {
		return BackgroundRunControlAdmission{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO background_run_controls(
receipt_id,task_id,attempt_id,workspace_id,run_generation,command_kind,state,writer_generation,container_id,container_started_at,
runtime_epoch,runtime_token,opencode_session_id,opencode_message_id,instruction,revision,created_at,updated_at)
VALUES(?,?,?,?,?,?,'requested',?,?,?,?,?,?,?,?,1,?,?)`, p.ReceiptID, run.TaskID, run.AttemptID, run.WorkspaceID, run.Generation,
		p.Claim.Scope.CommandKind, ownership.WriterGeneration, containerID, startedAt, runtimeEpoch, runtimeToken, sessionID,
		nullableValue(string(p.OpenCodeMessageID)), nullableValue(p.Instruction), now, now); err != nil {
		return BackgroundRunControlAdmission{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE background_runs SET claim_owner=NULL,claim_expires_at=NULL,revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND revision=? AND cancel_epoch=0 AND effect_phase='prompt_admitted'`,
		now, run.TaskID, run.AttemptID, run.WorkspaceID, run.Generation, run.Revision)
	if err != nil {
		return BackgroundRunControlAdmission{}, err
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunControlAdmission{}, ErrInvalidState
	}
	control, err := getBackgroundRunControl(ctx, tx, p.ReceiptID)
	if err != nil {
		return BackgroundRunControlAdmission{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRunControlAdmission{}, err
	}
	receipt := Receipt{ID: p.ReceiptID, WorkspaceID: p.WorkspaceID, CommandKind: p.Claim.Scope.CommandKind, State: ReceiptAccepted,
		IdempotencyKey: p.Claim.Key, RequestHash: p.Claim.RequestHash, Actor: p.Claim.Actor, AcceptedAt: p.RequestedAt,
		APIContractVersion: p.APIContractVersion, TargetType: "task", TargetID: p.TaskID, ResponseStatus: 202, ResponseProjection: projection}
	return BackgroundRunControlAdmission{Control: control, Receipt: receipt}, nil
}

func (s *Store) ClaimNextBackgroundRunControl(ctx context.Context, p ClaimNextBackgroundRunControlParams) (_ BackgroundRunControl, err error) {
	if _, parseErr := task.ParseWorkspaceID(string(p.WorkspaceID)); parseErr != nil || !validBoundedText(p.ClaimOwner, 1, 128) ||
		validExactTimestamp(p.Now) != nil || p.LeaseDuration <= 0 || p.LeaseDuration > maxBackgroundRunLease {
		return BackgroundRunControl{}, fmt.Errorf("%w: background run control claim", ErrInvalidInput)
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRunControl{}, err
	}
	defer release()
	defer rollback(tx, &err)
	now, expiry := unixMillis(p.Now), unixMillis(p.Now.Add(p.LeaseDuration))
	var receiptID task.ReceiptID
	err = tx.QueryRowContext(ctx, `SELECT c.receipt_id FROM background_run_controls c
JOIN background_run_ownerships o ON o.task_id=c.task_id AND o.workspace_id=c.workspace_id
JOIN background_runs r ON r.task_id=c.task_id AND r.workspace_id=c.workspace_id
WHERE c.workspace_id=? AND c.state IN ('requested','attempted') AND (c.claim_owner=? OR c.claim_owner IS NULL OR c.claim_expires_at<=?) AND
o.mode='agent_owned' AND o.phase='agent_active' AND o.writer_generation=c.writer_generation AND r.cancel_epoch=0 AND r.effect_phase='prompt_admitted'
ORDER BY c.created_at,c.receipt_id LIMIT 1`, p.WorkspaceID, p.ClaimOwner, now).Scan(&receiptID)
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRunControl{}, ErrNotFound
	}
	if err != nil {
		return BackgroundRunControl{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE background_run_controls SET claim_owner=?,claim_expires_at=?,
claim_generation=claim_generation+CASE WHEN claim_owner=? THEN 0 ELSE 1 END,revision=revision+1,updated_at=?
WHERE receipt_id=? AND (claim_owner=? OR claim_owner IS NULL OR claim_expires_at<=?)`, p.ClaimOwner, expiry, p.ClaimOwner, now,
		receiptID, p.ClaimOwner, now)
	if err != nil {
		return BackgroundRunControl{}, err
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunControl{}, ErrInvalidState
	}
	control, err := getBackgroundRunControl(ctx, tx, receiptID)
	if err != nil {
		return BackgroundRunControl{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRunControl{}, err
	}
	return control, nil
}

func (s *Store) ReadClaimedBackgroundRunControlWork(ctx context.Context, claim BackgroundRunControlClaim) (BackgroundRunControlWork, error) {
	if err := validateBackgroundRunControlClaim(claim); err != nil {
		return BackgroundRunControlWork{}, err
	}
	control, err := scanBackgroundRunControl(s.db.QueryRowContext(ctx, backgroundRunControlSelect+`
 WHERE receipt_id=? AND workspace_id=? AND revision=? AND state=? AND claim_owner=? AND claim_generation=? AND claim_expires_at>?`,
		claim.ReceiptID, claim.WorkspaceID, claim.ExpectedRevision, claim.ExpectedState, claim.ClaimOwner, claim.ClaimGeneration, unixMillis(claim.Now)))
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRunControlWork{}, ErrInvalidState
	}
	if err != nil {
		return BackgroundRunControlWork{}, err
	}
	run, err := readBackgroundRunExact(ctx, s.db, control.WorkspaceID, control.TaskID)
	if err != nil {
		return BackgroundRunControlWork{}, err
	}
	ownership, err := getBackgroundRunOwnership(ctx, s.db, control.WorkspaceID, control.TaskID)
	if err != nil {
		return BackgroundRunControlWork{}, err
	}
	return BackgroundRunControlWork{Run: run, Ownership: ownership, Control: control}, nil
}

func (s *Store) MarkBackgroundRunControlAttempted(ctx context.Context, claim BackgroundRunControlClaim) (BackgroundRunControl, error) {
	if err := validateBackgroundRunControlClaim(claim); err != nil || claim.ExpectedState != BackgroundRunControlRequested {
		return BackgroundRunControl{}, fmt.Errorf("%w: control attempt", ErrInvalidInput)
	}
	now := unixMillis(claim.Now)
	result, err := s.db.ExecContext(ctx, `UPDATE background_run_controls SET state='attempted',attempted_at=?,revision=revision+1,updated_at=?
WHERE receipt_id=? AND workspace_id=? AND revision=? AND state='requested' AND claim_owner=? AND claim_generation=? AND claim_expires_at>?`,
		now, now, claim.ReceiptID, claim.WorkspaceID, claim.ExpectedRevision, claim.ClaimOwner, claim.ClaimGeneration, now)
	if err != nil {
		return BackgroundRunControl{}, err
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunControl{}, ErrInvalidState
	}
	return getBackgroundRunControl(ctx, s.db, claim.ReceiptID)
}

func (s *Store) CompleteBackgroundRunControl(ctx context.Context, claim BackgroundRunControlClaim, state BackgroundRunControlState, detail string) (BackgroundRunControl, error) {
	if err := validateBackgroundRunControlClaim(claim); err != nil || claim.ExpectedState != BackgroundRunControlAttempted ||
		(state != BackgroundRunControlSucceeded && state != BackgroundRunControlUncertain && state != BackgroundRunControlConflict) || !validOptionalBounded(detail, 4096) {
		return BackgroundRunControl{}, fmt.Errorf("%w: control completion", ErrInvalidInput)
	}
	now := unixMillis(claim.Now)
	result, err := s.db.ExecContext(ctx, `UPDATE background_run_controls SET state=?,completed_at=?,last_error=?,claim_owner=NULL,claim_expires_at=NULL,revision=revision+1,updated_at=?
WHERE receipt_id=? AND workspace_id=? AND revision=? AND state='attempted' AND claim_owner=? AND claim_generation=? AND claim_expires_at>?`,
		state, now, nullableValue(detail), now, claim.ReceiptID, claim.WorkspaceID, claim.ExpectedRevision, claim.ClaimOwner, claim.ClaimGeneration, now)
	if err != nil {
		return BackgroundRunControl{}, err
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunControl{}, ErrInvalidState
	}
	return getBackgroundRunControl(ctx, s.db, claim.ReceiptID)
}

func (s *Store) LatestBackgroundRunControl(ctx context.Context, workspaceID task.WorkspaceID, taskID task.TaskID) (BackgroundRunControl, error) {
	value, err := scanBackgroundRunControl(s.db.QueryRowContext(ctx, backgroundRunControlSelect+` WHERE workspace_id=? AND task_id=? ORDER BY created_at DESC,receipt_id DESC LIMIT 1`, workspaceID, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRunControl{}, ErrNotFound
	}
	return value, err
}

func getBackgroundRunControl(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, receiptID task.ReceiptID) (BackgroundRunControl, error) {
	value, err := scanBackgroundRunControl(query.QueryRowContext(ctx, backgroundRunControlSelect+` WHERE receipt_id=?`, receiptID))
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRunControl{}, ErrNotFound
	}
	return value, err
}

func scanBackgroundRunControl(scanner interface{ Scan(...any) error }) (BackgroundRunControl, error) {
	var value BackgroundRunControl
	var messageID, instruction, lastError, claimOwner sql.NullString
	var attemptedAt, completedAt, claimExpiresAt sql.NullInt64
	var createdAt, updatedAt int64
	err := scanner.Scan(&value.ReceiptID, &value.TaskID, &value.AttemptID, &value.WorkspaceID, &value.RunGeneration, &value.CommandKind,
		&value.State, &value.WriterGeneration, &value.ContainerID, &value.ContainerStartedAt, &value.RuntimeEpoch, &value.RuntimeToken,
		&value.OpenCodeSessionID, &messageID, &instruction, &attemptedAt, &completedAt, &lastError, &claimOwner, &claimExpiresAt,
		&value.ClaimGeneration, &value.Revision, &createdAt, &updatedAt)
	if err != nil {
		return BackgroundRunControl{}, err
	}
	value.OpenCodeMessageID, value.Instruction, value.LastError = task.OpenCodeMessageID(nullableText(messageID)), nullableText(instruction), nullableText(lastError)
	value.AttemptedAt, value.CompletedAt, value.ClaimExpiresAt = nullableTime(attemptedAt), nullableTime(completedAt), nullableTime(claimExpiresAt)
	value.ClaimOwner, value.CreatedAt, value.UpdatedAt = nullableText(claimOwner), fromUnixMillis(createdAt), fromUnixMillis(updatedAt)
	started, parseErr := time.Parse(time.RFC3339Nano, value.ContainerStartedAt)
	digest := sha256.Sum256([]byte(value.ContainerID + "\x00" + value.ContainerStartedAt))
	if parseErr != nil || started.UnixNano() != value.RuntimeEpoch || value.RuntimeToken != hex.EncodeToString(digest[:]) ||
		(value.CommandKind != InterruptBackgroundRunCommand && value.CommandKind != SteerBackgroundRunCommand) || !value.State.valid() {
		return BackgroundRunControl{}, ErrCorruptStore
	}
	return value, nil
}

func (state BackgroundRunControlState) valid() bool {
	switch state {
	case BackgroundRunControlRequested, BackgroundRunControlAttempted, BackgroundRunControlSucceeded, BackgroundRunControlUncertain, BackgroundRunControlConflict:
		return true
	default:
		return false
	}
}

func validateBackgroundRunControlClaim(claim BackgroundRunControlClaim) error {
	if _, err := task.ParseWorkspaceID(string(claim.WorkspaceID)); err != nil {
		return err
	}
	if _, err := task.ParseReceiptID(string(claim.ReceiptID)); err != nil || claim.ExpectedRevision < 1 || !claim.ExpectedState.valid() ||
		!validBoundedText(claim.ClaimOwner, 1, 128) || claim.ClaimGeneration < 1 || validExactTimestamp(claim.Now) != nil {
		return ErrInvalidInput
	}
	return nil
}

func validControlInstruction(value string) bool {
	if !validBoundedText(value, 1, 16*1024) || utf8.RuneCountInString(value) > 4000 || strings.TrimSpace(value) == "" {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) && character != '\n' && character != '\t' {
			return false
		}
	}
	return true
}
