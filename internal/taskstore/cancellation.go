package taskstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

const (
	CancelTaskCommand          = "task.cancel"
	maxCancellationReasonBytes = 500
	CancellationTerminalReason = "cancellation_acknowledged"
)

// RequestCancellation durably fences a task and its current attempt. It
// performs no external delivery reconciliation or OpenCode interruption.
func (s *Store) RequestCancellation(ctx context.Context, p RequestCancellationParams) (_ Cancellation, err error) {
	if err := validateCancellation(p); err != nil {
		return Cancellation{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return Cancellation{}, fmt.Errorf("begin task cancellation: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	existing, found, err := receiptByKey(ctx, tx, p.Claim.Scope.WorkspaceID, p.Claim.Scope.CommandKind, p.Claim.Key)
	if err != nil {
		return Cancellation{}, err
	}
	if found {
		existingClaim := task.IdempotencyClaim{
			Scope: task.IdempotencyScope{WorkspaceID: existing.WorkspaceID, CommandKind: existing.CommandKind},
			Key:   existing.IdempotencyKey, RequestHash: existing.RequestHash, Actor: existing.Actor,
		}
		disposition, classifyErr := task.ClassifyIdempotency(&existingClaim, p.Claim)
		if classifyErr != nil {
			return Cancellation{}, fmt.Errorf("classify cancellation idempotency: %w", classifyErr)
		}
		switch disposition {
		case task.IdempotencyReplay:
			result, getErr := cancellationByTask(ctx, tx, existing.TargetID)
			if getErr != nil {
				return Cancellation{}, getErr
			}
			if result.Receipt.ID != existing.ID {
				return Cancellation{}, fmt.Errorf("%w: cancellation receipt linkage", ErrCorruptStore)
			}
			if err := tx.Commit(); err != nil {
				return Cancellation{}, fmt.Errorf("finish cancellation replay: %w", err)
			}
			result.Replayed = true
			return result, nil
		case task.IdempotencyOwnerMismatch:
			return Cancellation{}, ErrIdempotencyOwnerMismatch
		case task.IdempotencyConflict:
			return Cancellation{}, &ConflictError{ReceiptID: existing.ID, TargetID: existing.TargetID}
		default:
			return Cancellation{}, fmt.Errorf("%w: unexpected idempotency disposition", ErrCorruptStore)
		}
	}

	owner, err := getTask(ctx, tx, p.TaskID)
	if errors.Is(err, ErrNotFound) {
		return Cancellation{}, &NotFoundError{Kind: "task", ID: string(p.TaskID)}
	}
	if err != nil {
		return Cancellation{}, err
	}
	if owner.WorkspaceID != p.Claim.Scope.WorkspaceID {
		return Cancellation{}, &NotFoundError{Kind: "task", ID: string(p.TaskID)}
	}
	if owner.CancelEpoch != 0 {
		return Cancellation{}, &CancellationAlreadyRequestedError{TaskID: owner.ID, ReceiptID: owner.CancellationReceiptID}
	}
	if owner.State.Terminal() {
		return Cancellation{}, &TerminalTaskError{TaskID: owner.ID, State: owner.State}
	}
	if err := task.AllowTaskTransition(owner.State, task.TaskCancelRequested); err != nil {
		return Cancellation{}, fmt.Errorf("%w: task cannot request cancellation", ErrInvalidState)
	}
	attempt, err := getAttempt(ctx, tx, owner.CurrentAttemptID)
	if err != nil {
		return Cancellation{}, err
	}
	if attempt.TaskID != owner.ID || attempt.WorkspaceID != owner.WorkspaceID {
		return Cancellation{}, fmt.Errorf("%w: cancellation attempt ownership", ErrCorruptStore)
	}
	if p.Now.Before(owner.CreatedAt) || p.Now.Before(attempt.CreatedAt) {
		return Cancellation{}, fmt.Errorf("%w: cancellation precedes task", ErrInvalidInput)
	}

	effect, transitionAttempt, err := cancellationDisposition(attempt.State)
	if err != nil {
		return Cancellation{}, err
	}
	actorID, err := ensureActor(ctx, tx, p.Claim.Actor)
	if err != nil {
		return Cancellation{}, err
	}
	nowMS := unixMillis(p.Now)
	reasonValue := any(nil)
	if p.Reason != "" {
		reasonValue = p.Reason
	}
	response, err := json.Marshal(struct {
		ReceiptID      task.ReceiptID                `json:"receiptId"`
		TaskID         task.TaskID                   `json:"taskId"`
		AttemptID      task.AttemptID                `json:"attemptId"`
		AttemptEventID task.EventID                  `json:"attemptEventId"`
		TaskEventID    task.EventID                  `json:"taskEventId"`
		CancelEpoch    uint64                        `json:"cancelEpoch"`
		Disposition    CancellationEffectDisposition `json:"effectDisposition"`
	}{p.ReceiptID, owner.ID, attempt.ID, p.AttemptEventID, p.TaskEventID, 1, effect})
	if err != nil {
		return Cancellation{}, fmt.Errorf("encode cancellation receipt projection: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO receipts(
    id,workspace_id,command_kind,state,idempotency_key,request_hash,actor_snapshot_id,
    accepted_at,api_contract_version,target_type,target_id,response_status,response_projection
) VALUES(?,?,?,'accepted',?,?,?,?,?,'task',?,202,?)`,
		p.ReceiptID, owner.WorkspaceID, p.Claim.Scope.CommandKind, p.Claim.Key, p.Claim.RequestHash[:],
		actorID, nowMS, p.APIContractVersion, owner.ID, string(response)); err != nil {
		return Cancellation{}, fmt.Errorf("insert cancellation receipt: %w", err)
	}
	payload, err := json.Marshal(struct {
		CancelEpoch uint64                        `json:"cancelEpoch"`
		Reason      string                        `json:"reason,omitempty"`
		Disposition CancellationEffectDisposition `json:"effectDisposition"`
	}{1, p.Reason, effect})
	if err != nil {
		return Cancellation{}, fmt.Errorf("encode cancellation event: %w", err)
	}
	attemptEvent, err := insertAttemptEvent(ctx, tx, p.AttemptEventID, attempt, "attempt.cancel_requested", nowMS, actorID, payload)
	if err != nil {
		return Cancellation{}, err
	}
	taskEvent, err := insertTaskEvent(ctx, tx, p.TaskEventID, owner, "task.cancel_requested", nowMS, actorID, payload)
	if err != nil {
		return Cancellation{}, err
	}
	if attemptEvent.Cursor >= taskEvent.Cursor {
		return Cancellation{}, fmt.Errorf("%w: cancellation event ordering", ErrCorruptStore)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE tasks
SET state='cancel_requested',cancel_epoch=1,cancel_actor_snapshot_id=?,cancel_reason=?,cancel_requested_at=?,
    cancel_receipt_id=?,cancel_attempt_id=?,cancel_attempt_event_id=?,cancel_task_event_id=?,cancel_effect_disposition=?,
    latest_event_cursor=?,revision=revision+1,updated_at=?
WHERE id=? AND workspace_id=? AND cancel_epoch=0 AND state=? AND current_attempt_id=? AND revision=?`,
		actorID, reasonValue, nowMS, p.ReceiptID, attempt.ID, p.AttemptEventID, p.TaskEventID, effect,
		taskEvent.Cursor, nowMS, owner.ID, owner.WorkspaceID, owner.State, attempt.ID, owner.Revision)
	if err != nil {
		return Cancellation{}, fmt.Errorf("fence task cancellation: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return Cancellation{}, fmt.Errorf("%w: task cancellation lost", ErrInvalidState)
	}
	if transitionAttempt {
		result, err = tx.ExecContext(ctx, `
UPDATE attempts
SET state='cancel_requested',delivery_claim_owner=NULL,delivery_claim_expires_at=NULL,
    revision=revision+1,updated_at=?
WHERE id=? AND task_id=? AND workspace_id=? AND state=? AND revision=?`,
			nowMS, attempt.ID, owner.ID, owner.WorkspaceID, attempt.State, attempt.Revision)
		if err != nil {
			return Cancellation{}, fmt.Errorf("fence attempt cancellation: %w", err)
		}
		if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
			return Cancellation{}, fmt.Errorf("%w: attempt cancellation lost", ErrInvalidState)
		}
	}

	stored, err := cancellationByTask(ctx, tx, owner.ID)
	if err != nil {
		return Cancellation{}, err
	}
	if stored.AttemptEvent.Cursor >= stored.TaskEvent.Cursor || stored.Task.LatestEventCursor != stored.TaskEvent.Cursor {
		return Cancellation{}, fmt.Errorf("%w: cancellation event ordering", ErrCorruptStore)
	}
	if err := tx.Commit(); err != nil {
		return Cancellation{}, fmt.Errorf("commit task cancellation: %w", err)
	}
	return stored, nil
}

// InspectCancellation returns durable cancellation intent and the exact current
// attempt without exposing a mutation surface. It is suitable for restart
// reconciliation after cancellation events identify the task.
func (s *Store) InspectCancellation(ctx context.Context, taskID task.TaskID) (Cancellation, error) {
	if _, err := task.ParseTaskID(string(taskID)); err != nil {
		return Cancellation{}, fmt.Errorf("%w: task ID", ErrInvalidInput)
	}
	return cancellationByTask(ctx, s.db, taskID)
}

// FindPendingCancellation returns one durable cancellation fence for restart
// processing. Cancellation work is ordered by its task event cursor.
func (s *Store) FindPendingCancellation(ctx context.Context, workspaceID task.WorkspaceID) (Cancellation, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil {
		return Cancellation{}, fmt.Errorf("%w: workspace ID", ErrInvalidInput)
	}
	var taskID task.TaskID
	err := s.db.QueryRowContext(ctx, `
SELECT id FROM tasks
WHERE workspace_id=? AND state='cancel_requested' AND cancel_epoch=1
ORDER BY latest_event_cursor ASC LIMIT 1`, workspaceID).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return Cancellation{}, &NotFoundError{Kind: "pending cancellation", ID: string(workspaceID)}
	}
	if err != nil {
		return Cancellation{}, fmt.Errorf("find pending cancellation: %w", err)
	}
	return cancellationByTask(ctx, s.db, taskID)
}

// AcknowledgeCancellation atomically closes the exact current cancellation
// after the coordinator has completed and proved the disposition persisted by
// RequestCancellation. It performs no external effect.
func (s *Store) AcknowledgeCancellation(ctx context.Context, p AcknowledgeCancellationParams) (_ CancellationAcknowledgment, err error) {
	if err := validateCancellationAcknowledgment(p); err != nil {
		return CancellationAcknowledgment{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return CancellationAcknowledgment{}, fmt.Errorf("begin cancellation acknowledgment: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	owner, err := getTask(ctx, tx, p.TaskID)
	if errors.Is(err, ErrNotFound) {
		return CancellationAcknowledgment{}, &NotFoundError{Kind: "task", ID: string(p.TaskID)}
	}
	if err != nil {
		return CancellationAcknowledgment{}, err
	}
	attempt, err := getAttempt(ctx, tx, p.AttemptID)
	if errors.Is(err, ErrNotFound) {
		return CancellationAcknowledgment{}, &NotFoundError{Kind: "attempt", ID: string(p.AttemptID)}
	}
	if err != nil {
		return CancellationAcknowledgment{}, err
	}
	if attempt.TaskID != owner.ID || attempt.WorkspaceID != owner.WorkspaceID || owner.CurrentAttemptID != attempt.ID || owner.CancellationAttemptID != attempt.ID {
		return CancellationAcknowledgment{}, fmt.Errorf("%w: cancellation acknowledgment ownership", ErrInvalidState)
	}

	if owner.State == task.TaskCanceled && attempt.State == task.AttemptCanceled && attempt.CancellationAckAt != nil {
		stored, replayErr := cancellationAcknowledgmentReplay(ctx, tx, owner, attempt, p)
		if replayErr != nil {
			return CancellationAcknowledgment{}, replayErr
		}
		if err := tx.Commit(); err != nil {
			return CancellationAcknowledgment{}, fmt.Errorf("finish cancellation acknowledgment replay: %w", err)
		}
		stored.Replayed = true
		return stored, nil
	}
	if attempt.Revision != p.ExpectedAttemptRevision {
		return CancellationAcknowledgment{}, &StaleRevisionError{AttemptID: attempt.ID, Expected: p.ExpectedAttemptRevision, Actual: attempt.Revision}
	}
	if owner.Revision != p.ExpectedTaskRevision {
		return CancellationAcknowledgment{}, &StaleTaskRevisionError{TaskID: owner.ID, Expected: p.ExpectedTaskRevision, Actual: owner.Revision}
	}
	if owner.CancelEpoch != p.CancelEpoch || owner.CancelEpoch != 1 || owner.State != task.TaskCancelRequested || owner.CancellationEffect != p.Disposition {
		return CancellationAcknowledgment{}, fmt.Errorf("%w: cancellation fence or disposition changed", ErrInvalidState)
	}
	if attempt.CancellationAckAt != nil || attempt.DeliveryClaimOwner != nil || attempt.DeliveryClaimExpiresAt != nil {
		return CancellationAcknowledgment{}, fmt.Errorf("%w: cancellation attempt still has authority", ErrInvalidState)
	}
	if p.Disposition == CancellationEffectNoneTerminal {
		if !attempt.State.Terminal() {
			return CancellationAcknowledgment{}, fmt.Errorf("%w: terminal cancellation outcome not present", ErrInvalidState)
		}
	} else if attempt.State != task.AttemptCancelRequested {
		return CancellationAcknowledgment{}, &StateError{AttemptID: attempt.ID, State: attempt.State, Required: task.AttemptCancelRequested}
	}
	if owner.CancellationRequestedAt == nil || p.Now.Before(*owner.CancellationRequestedAt) || p.Now.Before(attempt.CreatedAt) {
		return CancellationAcknowledgment{}, fmt.Errorf("%w: cancellation acknowledgment precedes request", ErrInvalidInput)
	}

	actorID, err := ensureActor(ctx, tx, p.Actor)
	if err != nil {
		return CancellationAcknowledgment{}, err
	}
	payload, err := cancellationAcknowledgmentPayload(p)
	if err != nil {
		return CancellationAcknowledgment{}, err
	}
	nowMS := unixMillis(p.Now)
	attemptEvent, err := insertAttemptEvent(ctx, tx, p.AttemptEventID, attempt, "attempt.canceled", nowMS, actorID, payload)
	if err != nil {
		return CancellationAcknowledgment{}, err
	}
	taskEvent, err := insertTaskEvent(ctx, tx, p.TaskEventID, owner, "task.canceled", nowMS, actorID, payload)
	if err != nil {
		return CancellationAcknowledgment{}, err
	}
	if attemptEvent.Cursor >= taskEvent.Cursor {
		return CancellationAcknowledgment{}, fmt.Errorf("%w: cancellation acknowledgment event ordering", ErrCorruptStore)
	}

	result, err := tx.ExecContext(ctx, `
UPDATE attempts
SET state='canceled',delivery_claim_owner=NULL,delivery_claim_expires_at=NULL,cancellation_ack_at=?,
    recovery_reason=NULL,terminal_reason=?,revision=revision+1,updated_at=?
WHERE id=? AND task_id=? AND workspace_id=? AND state=? AND cancellation_ack_at IS NULL
  AND delivery_claim_owner IS NULL AND delivery_claim_expires_at IS NULL AND revision=?`,
		nowMS, CancellationTerminalReason, nowMS, attempt.ID, owner.ID, owner.WorkspaceID, attempt.State, p.ExpectedAttemptRevision)
	if err != nil {
		return CancellationAcknowledgment{}, fmt.Errorf("acknowledge canceled attempt: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return CancellationAcknowledgment{}, fmt.Errorf("%w: cancellation attempt changed", ErrInvalidState)
	}
	result, err = tx.ExecContext(ctx, `
UPDATE tasks
SET state='canceled',terminal_reason=?,latest_event_cursor=?,revision=revision+1,updated_at=?
WHERE id=? AND workspace_id=? AND state='cancel_requested' AND cancel_epoch=?
  AND current_attempt_id=? AND cancel_attempt_id=? AND cancel_effect_disposition=? AND revision=?`,
		CancellationTerminalReason, taskEvent.Cursor, nowMS, owner.ID, owner.WorkspaceID, p.CancelEpoch,
		attempt.ID, attempt.ID, p.Disposition, p.ExpectedTaskRevision)
	if err != nil {
		return CancellationAcknowledgment{}, fmt.Errorf("acknowledge canceled task: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return CancellationAcknowledgment{}, fmt.Errorf("%w: cancellation task changed", ErrInvalidState)
	}

	storedAttempt, err := getAttempt(ctx, tx, attempt.ID)
	if err != nil {
		return CancellationAcknowledgment{}, err
	}
	storedTask, err := getTask(ctx, tx, owner.ID)
	if err != nil {
		return CancellationAcknowledgment{}, err
	}
	if storedAttempt.CancellationAckAt == nil || !storedAttempt.CancellationAckAt.Equal(p.Now) || storedTask.LatestEventCursor != taskEvent.Cursor {
		return CancellationAcknowledgment{}, fmt.Errorf("%w: cancellation acknowledgment projection", ErrCorruptStore)
	}
	if err := tx.Commit(); err != nil {
		return CancellationAcknowledgment{}, fmt.Errorf("commit cancellation acknowledgment: %w", err)
	}
	return CancellationAcknowledgment{Task: storedTask, Attempt: storedAttempt, AttemptEvent: attemptEvent, TaskEvent: taskEvent, Disposition: p.Disposition}, nil
}

func cancellationAcknowledgmentReplay(ctx context.Context, q queryRower, owner Task, attempt Attempt, p AcknowledgeCancellationParams) (CancellationAcknowledgment, error) {
	if owner.CancelEpoch != p.CancelEpoch || owner.CancellationEffect != p.Disposition ||
		attempt.Revision != p.ExpectedAttemptRevision+1 || owner.Revision != p.ExpectedTaskRevision+1 ||
		attempt.CancellationAckAt == nil || !attempt.CancellationAckAt.Equal(p.Now) {
		return CancellationAcknowledgment{}, fmt.Errorf("%w: cancellation acknowledgment already closed", ErrInvalidState)
	}
	attemptEvent, err := scanEvent(q.QueryRowContext(ctx, eventSelect+` WHERE e.id=?`, p.AttemptEventID))
	if errors.Is(err, sql.ErrNoRows) {
		return CancellationAcknowledgment{}, fmt.Errorf("%w: cancellation acknowledgment replay event", ErrInvalidState)
	}
	if err != nil {
		return CancellationAcknowledgment{}, fmt.Errorf("read cancellation acknowledgment attempt event: %w", err)
	}
	taskEvent, err := scanEvent(q.QueryRowContext(ctx, eventSelect+` WHERE e.id=?`, p.TaskEventID))
	if errors.Is(err, sql.ErrNoRows) {
		return CancellationAcknowledgment{}, fmt.Errorf("%w: cancellation acknowledgment replay event", ErrInvalidState)
	}
	if err != nil {
		return CancellationAcknowledgment{}, fmt.Errorf("read cancellation acknowledgment task event: %w", err)
	}
	wantPayload, err := cancellationAcknowledgmentPayload(p)
	if err != nil {
		return CancellationAcknowledgment{}, err
	}
	if attemptEvent.Type != "attempt.canceled" || attemptEvent.TaskID != owner.ID || attemptEvent.AttemptID != attempt.ID ||
		taskEvent.Type != "task.canceled" || taskEvent.TaskID != owner.ID || taskEvent.AttemptID != "" ||
		attemptEvent.Cursor >= taskEvent.Cursor || taskEvent.Cursor != owner.LatestEventCursor ||
		!attemptEvent.OccurredAt.Equal(p.Now) || !taskEvent.OccurredAt.Equal(p.Now) ||
		attemptEvent.Actor != p.Actor || taskEvent.Actor != p.Actor ||
		!bytes.Equal(attemptEvent.Payload, wantPayload) || !bytes.Equal(taskEvent.Payload, wantPayload) {
		return CancellationAcknowledgment{}, fmt.Errorf("%w: cancellation acknowledgment replay differs", ErrInvalidState)
	}
	return CancellationAcknowledgment{Task: owner, Attempt: attempt, AttemptEvent: attemptEvent, TaskEvent: taskEvent, Disposition: p.Disposition}, nil
}

func cancellationAcknowledgmentPayload(p AcknowledgeCancellationParams) (json.RawMessage, error) {
	base, err := deliveryEvidencePayload("", p.EvidencePayload, p.EvidenceSHA256)
	if err != nil {
		return nil, err
	}
	taskJSON, _ := json.Marshal(p.TaskID)
	attemptJSON, _ := json.Marshal(p.AttemptID)
	dispositionJSON, _ := json.Marshal(p.Disposition)
	payload := make([]byte, 0, len(base)+len(taskJSON)+len(attemptJSON)+len(dispositionJSON)+180)
	payload = append(payload, `{"taskId":`...)
	payload = append(payload, taskJSON...)
	payload = append(payload, `,"attemptId":`...)
	payload = append(payload, attemptJSON...)
	payload = append(payload, fmt.Sprintf(`,"cancelEpoch":%d,"expectedAttemptRevision":%d,"expectedTaskRevision":%d,"disposition":`, p.CancelEpoch, p.ExpectedAttemptRevision, p.ExpectedTaskRevision)...)
	payload = append(payload, dispositionJSON...)
	payload = append(payload, `,"terminalReason":"`...)
	payload = append(payload, CancellationTerminalReason...)
	payload = append(payload, `",`...)
	payload = append(payload, base[1:]...)
	if !json.Valid(payload) {
		return nil, fmt.Errorf("%w: encoded cancellation acknowledgment", ErrCorruptStore)
	}
	return payload, nil
}

func validateCancellationAcknowledgment(p AcknowledgeCancellationParams) error {
	if _, err := task.ParseTaskID(string(p.TaskID)); err != nil {
		return fmt.Errorf("%w: task ID", ErrInvalidInput)
	}
	if err := validateAttemptAndEvents(p.AttemptID, p.AttemptEventID, p.TaskEventID); err != nil {
		return err
	}
	if p.CancelEpoch != 1 || p.ExpectedAttemptRevision < 1 || p.ExpectedTaskRevision < 1 || !p.Disposition.valid() {
		return fmt.Errorf("%w: cancellation epoch, revisions, or disposition", ErrInvalidInput)
	}
	if err := validExactTimestamp(p.Now); err != nil {
		return err
	}
	if err := p.Actor.Validate(); err != nil || (p.Actor.Type != task.ActorSystem && p.Actor.Type != task.ActorRecovery) {
		return fmt.Errorf("%w: cancellation acknowledgment actor", ErrInvalidInput)
	}
	return validateDeliveryEvidence(p.EvidencePayload, p.EvidenceSHA256)
}

func cancellationByTask(ctx context.Context, q queryRower, taskID task.TaskID) (Cancellation, error) {
	owner, err := getTask(ctx, q, taskID)
	if err != nil {
		return Cancellation{}, err
	}
	if owner.CancelEpoch != 1 || owner.CancellationReceiptID == "" || owner.CancellationAttemptID != owner.CurrentAttemptID || !owner.CancellationEffect.valid() {
		if owner.CancelEpoch == 0 {
			return Cancellation{}, &NotFoundError{Kind: "cancellation", ID: string(taskID)}
		}
		return Cancellation{}, fmt.Errorf("%w: cancellation projection", ErrCorruptStore)
	}
	attempt, err := getAttempt(ctx, q, owner.CurrentAttemptID)
	if err != nil {
		return Cancellation{}, err
	}
	receipt, err := scanReceipt(q.QueryRowContext(ctx, receiptSelect+` WHERE r.id=?`, owner.CancellationReceiptID))
	if err != nil {
		return Cancellation{}, fmt.Errorf("read cancellation receipt: %w", err)
	}
	attemptEvent, err := scanEvent(q.QueryRowContext(ctx, eventSelect+` WHERE e.id=?`, owner.CancellationAttemptEventID))
	if err != nil {
		return Cancellation{}, fmt.Errorf("read cancellation attempt event: %w", err)
	}
	taskEvent, err := scanEvent(q.QueryRowContext(ctx, eventSelect+` WHERE e.id=?`, owner.CancellationTaskEventID))
	if err != nil {
		return Cancellation{}, fmt.Errorf("read cancellation task event: %w", err)
	}
	if receipt.TargetID != owner.ID || receipt.CommandKind != CancelTaskCommand ||
		attemptEvent.Type != "attempt.cancel_requested" || attemptEvent.TaskID != owner.ID || attemptEvent.AttemptID != attempt.ID ||
		taskEvent.Type != "task.cancel_requested" || taskEvent.TaskID != owner.ID || taskEvent.AttemptID != "" ||
		attemptEvent.Cursor >= taskEvent.Cursor {
		return Cancellation{}, fmt.Errorf("%w: cancellation ownership", ErrCorruptStore)
	}
	return Cancellation{Task: owner, Attempt: attempt, Receipt: receipt, AttemptEvent: attemptEvent, TaskEvent: taskEvent, Disposition: owner.CancellationEffect}, nil
}

func cancellationDisposition(state task.AttemptState) (CancellationEffectDisposition, bool, error) {
	if state.Terminal() {
		return CancellationEffectNoneTerminal, false, nil
	}
	if err := task.AllowAttemptTransition(state, task.AttemptCancelRequested); err != nil {
		return "", false, fmt.Errorf("%w: attempt cannot request cancellation", ErrInvalidState)
	}
	switch state {
	case task.AttemptPrepared:
		return CancellationEffectNonePrepared, true, nil
	case task.AttemptDelivering:
		return CancellationEffectReconcileDelivery, true, nil
	case task.AttemptAdmitted, task.AttemptRunning, task.AttemptInputRequired, task.AttemptUncertain, task.AttemptRecoveryRequired:
		return CancellationEffectInterrupt, true, nil
	default:
		return "", false, fmt.Errorf("%w: attempt cancellation state", ErrInvalidState)
	}
}

func validateCancellation(p RequestCancellationParams) error {
	if _, err := task.ParseTaskID(string(p.TaskID)); err != nil {
		return fmt.Errorf("%w: task ID", ErrInvalidInput)
	}
	if _, err := task.ParseReceiptID(string(p.ReceiptID)); err != nil {
		return fmt.Errorf("%w: receipt ID", ErrInvalidInput)
	}
	if _, err := task.ParseEventID(string(p.AttemptEventID)); err != nil {
		return fmt.Errorf("%w: attempt event ID", ErrInvalidInput)
	}
	if _, err := task.ParseEventID(string(p.TaskEventID)); err != nil || p.AttemptEventID == p.TaskEventID {
		return fmt.Errorf("%w: task event ID", ErrInvalidInput)
	}
	if err := p.Claim.Validate(); err != nil || p.Claim.Scope.CommandKind != CancelTaskCommand {
		return fmt.Errorf("%w: idempotency claim", ErrInvalidInput)
	}
	if p.Reason != "" && !validBoundedText(p.Reason, 1, maxCancellationReasonBytes) {
		return fmt.Errorf("%w: cancellation reason", ErrInvalidInput)
	}
	if err := validExactTimestamp(p.Now); err != nil {
		return err
	}
	if !validBoundedText(p.APIContractVersion, 1, 64) {
		return fmt.Errorf("%w: API contract version", ErrInvalidInput)
	}
	return nil
}
