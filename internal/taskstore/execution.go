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

// FindExecutionAttempt returns the workspace's current post-admission work.
// The returned revisions and exact OpenCode IDs are inputs to
// RecordExecutionProjection, which remains authoritative after this read.
func (s *Store) FindExecutionAttempt(ctx context.Context, workspaceID task.WorkspaceID) (DeliveryWork, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil {
		return DeliveryWork{}, fmt.Errorf("%w: workspace ID", ErrInvalidInput)
	}
	var attemptID task.AttemptID
	err := s.db.QueryRowContext(ctx, `
SELECT a.id
FROM attempts a JOIN tasks t ON t.id=a.task_id AND t.workspace_id=a.workspace_id
WHERE a.workspace_id=? AND t.current_attempt_id=a.id AND t.cancel_epoch=0 AND
      ((a.state IN ('admitted','running') AND t.state='running') OR
       (a.state='input_required' AND t.state='input_required'))
ORDER BY a.id LIMIT 1`, workspaceID).Scan(&attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return DeliveryWork{}, &NotFoundError{Kind: "execution attempt", ID: string(workspaceID)}
	}
	if err != nil {
		return DeliveryWork{}, fmt.Errorf("find execution attempt: %w", err)
	}
	return s.InspectExecutionAttempt(ctx, attemptID)
}

func (s *Store) InspectExecutionAttempt(ctx context.Context, attemptID task.AttemptID) (DeliveryWork, error) {
	if _, err := task.ParseAttemptID(string(attemptID)); err != nil {
		return DeliveryWork{}, fmt.Errorf("%w: attempt ID", ErrInvalidInput)
	}
	attempt, err := getAttempt(ctx, s.db, attemptID)
	if errors.Is(err, ErrNotFound) {
		return DeliveryWork{}, &NotFoundError{Kind: "attempt", ID: string(attemptID)}
	}
	if err != nil {
		return DeliveryWork{}, err
	}
	if attempt.State != task.AttemptAdmitted && attempt.State != task.AttemptRunning && attempt.State != task.AttemptInputRequired {
		return DeliveryWork{}, &StateError{AttemptID: attempt.ID, State: attempt.State, Required: task.AttemptAdmitted}
	}
	owner, err := getTask(ctx, s.db, attempt.TaskID)
	if err != nil {
		return DeliveryWork{}, err
	}
	wantTaskState := task.TaskRunning
	if attempt.State == task.AttemptInputRequired {
		wantTaskState = task.TaskInputRequired
	}
	if owner.CurrentAttemptID != attempt.ID || owner.CancelEpoch != 0 || owner.State != wantTaskState {
		return DeliveryWork{}, fmt.Errorf("%w: execution attempt is not current", ErrInvalidState)
	}
	return DeliveryWork{Task: owner, Attempt: attempt}, nil
}

// RecordExecutionProjection records one fully decoded, bounded OpenCode
// projection. It never performs an OpenCode or repository effect.
func (s *Store) RecordExecutionProjection(ctx context.Context, p RecordExecutionProjectionParams) (_ ExecutionProjection, err error) {
	if err := validateExecutionProjection(p); err != nil {
		return ExecutionProjection{}, err
	}
	payload, err := executionProjectionPayload(p)
	if err != nil {
		return ExecutionProjection{}, err
	}
	targetAttempt, targetTask, attemptEventType, taskEventType, err := executionProjectionTargets(p.ExpectedState, p.Outcome)
	if err != nil {
		return ExecutionProjection{}, err
	}

	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return ExecutionProjection{}, fmt.Errorf("begin execution projection: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	attempt, owner, err := deliveryRows(ctx, tx, p.AttemptID)
	if err != nil {
		return ExecutionProjection{}, err
	}
	if owner.ID != p.TaskID {
		return ExecutionProjection{}, fmt.Errorf("%w: exact task identity differs", ErrInvalidState)
	}
	if attempt.State == targetAttempt && attempt.Revision == p.ExpectedAttemptRevision+1 &&
		owner.State == targetTask && owner.Revision == p.ExpectedTaskRevision+1 {
		replayed, replayErr := executionProjectionReplay(ctx, tx, owner, attempt, p, payload, attemptEventType, taskEventType)
		if replayErr != nil {
			return ExecutionProjection{}, replayErr
		}
		if err := tx.Commit(); err != nil {
			return ExecutionProjection{}, fmt.Errorf("finish execution projection replay: %w", err)
		}
		replayed.Replayed = true
		return replayed, nil
	}
	if attempt.Revision != p.ExpectedAttemptRevision {
		return ExecutionProjection{}, &StaleRevisionError{AttemptID: attempt.ID, Expected: p.ExpectedAttemptRevision, Actual: attempt.Revision}
	}
	if owner.Revision != p.ExpectedTaskRevision {
		return ExecutionProjection{}, &StaleTaskRevisionError{TaskID: owner.ID, Expected: p.ExpectedTaskRevision, Actual: owner.Revision}
	}
	if attempt.State != p.ExpectedState {
		return ExecutionProjection{}, &StateError{AttemptID: attempt.ID, State: attempt.State, Required: p.ExpectedState}
	}
	wantTaskState := task.TaskRunning
	if p.ExpectedState == task.AttemptInputRequired {
		wantTaskState = task.TaskInputRequired
	}
	if owner.CurrentAttemptID != attempt.ID || owner.CancelEpoch != 0 || owner.State != wantTaskState {
		return ExecutionProjection{}, fmt.Errorf("%w: execution attempt is not current and unfenced", ErrInvalidState)
	}
	if attempt.OpenCodeSessionID != p.OpenCodeSessionID || attempt.OpenCodeMessageID != p.OpenCodeMessageID {
		return ExecutionProjection{}, fmt.Errorf("%w: exact OpenCode identity differs", ErrInvalidState)
	}
	if attempt.DeliveryPhase != DeliveryPhasePromptStarted || attempt.AdmittedAt == nil ||
		attempt.DeliveryClaimOwner != nil || attempt.DeliveryClaimExpiresAt != nil {
		return ExecutionProjection{}, fmt.Errorf("%w: attempt lacks admitted execution proof", ErrInvalidState)
	}
	if p.ObservedAt.Before(*attempt.AdmittedAt) || p.ObservedAt.Before(attempt.UpdatedAt) {
		return ExecutionProjection{}, fmt.Errorf("%w: projection observation precedes current attempt", ErrInvalidInput)
	}

	actorID, err := ensureActor(ctx, tx, p.Actor)
	if err != nil {
		return ExecutionProjection{}, err
	}
	nowMS := unixMillis(p.ObservedAt)
	attemptEvent, err := insertAttemptEvent(ctx, tx, p.AttemptEventID, attempt, attemptEventType, nowMS, actorID, payload)
	if err != nil {
		return ExecutionProjection{}, err
	}
	taskEvent, err := insertTaskEvent(ctx, tx, p.TaskEventID, owner, taskEventType, nowMS, actorID, payload)
	if err != nil {
		return ExecutionProjection{}, err
	}

	var recoveryReason, terminalReason any
	if targetAttempt == task.AttemptRecoveryRequired {
		recoveryReason = p.Reason
	}
	if targetAttempt == task.AttemptFailed {
		terminalReason = p.Reason
	}
	result, err := tx.ExecContext(ctx, `
UPDATE attempts SET state=?,recovery_reason=?,terminal_reason=?,revision=revision+1,updated_at=?
WHERE id=? AND task_id=? AND state=? AND revision=? AND opencode_session_id=? AND opencode_message_id=?`,
		targetAttempt, recoveryReason, terminalReason, nowMS, attempt.ID, owner.ID, p.ExpectedState,
		p.ExpectedAttemptRevision, p.OpenCodeSessionID, p.OpenCodeMessageID)
	if err != nil {
		return ExecutionProjection{}, fmt.Errorf("record execution attempt projection: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return ExecutionProjection{}, fmt.Errorf("%w: execution attempt changed", ErrInvalidState)
	}
	var taskTerminalReason any
	if targetTask == task.TaskFailed {
		taskTerminalReason = p.Reason
	}
	result, err = tx.ExecContext(ctx, `
UPDATE tasks SET state=?,terminal_reason=?,latest_event_cursor=?,revision=revision+1,updated_at=?
WHERE id=? AND state=? AND cancel_epoch=0 AND current_attempt_id=? AND revision=?`,
		targetTask, taskTerminalReason, taskEvent.Cursor, nowMS, owner.ID, wantTaskState, attempt.ID, p.ExpectedTaskRevision)
	if err != nil {
		return ExecutionProjection{}, fmt.Errorf("record execution task projection: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return ExecutionProjection{}, fmt.Errorf("%w: execution task changed", ErrInvalidState)
	}
	return finishExecutionProjection(ctx, tx, owner.ID, attempt.ID, attemptEvent, taskEvent)
}

func executionProjectionTargets(from task.AttemptState, outcome ExecutionProjectionOutcome) (task.AttemptState, task.TaskState, string, string, error) {
	to := task.AttemptState(outcome)
	if err := task.AllowAttemptTransition(from, to); err != nil {
		return "", "", "", "", fmt.Errorf("%w: execution projection %s -> %s", ErrInvalidState, from, outcome)
	}
	taskState := task.TaskRunning
	taskEvent := "task." + string(outcome)
	switch outcome {
	case ExecutionRunning:
		if from != task.AttemptAdmitted && from != task.AttemptInputRequired {
			return "", "", "", "", fmt.Errorf("%w: running proof source", ErrInvalidState)
		}
	case ExecutionInputRequired:
		taskState = task.TaskInputRequired
	case ExecutionRecoveryRequired:
		taskState = task.TaskRecoveryRequired
	case ExecutionFailed:
		taskState = task.TaskFailed
	case ExecutionSucceeded:
		taskEvent = "task.execution_succeeded"
	default:
		return "", "", "", "", fmt.Errorf("%w: execution outcome", ErrInvalidInput)
	}
	return to, taskState, "attempt." + string(outcome), taskEvent, nil
}

func finishExecutionProjection(ctx context.Context, tx *sql.Tx, taskID task.TaskID, attemptID task.AttemptID, attemptEvent, taskEvent Event) (ExecutionProjection, error) {
	attempt, err := getAttempt(ctx, tx, attemptID)
	if err != nil {
		return ExecutionProjection{}, err
	}
	owner, err := getTask(ctx, tx, taskID)
	if err != nil {
		return ExecutionProjection{}, err
	}
	if attemptEvent.Cursor >= taskEvent.Cursor || owner.LatestEventCursor != taskEvent.Cursor {
		return ExecutionProjection{}, fmt.Errorf("%w: execution event ordering", ErrCorruptStore)
	}
	if err := tx.Commit(); err != nil {
		return ExecutionProjection{}, fmt.Errorf("commit execution projection: %w", err)
	}
	return ExecutionProjection{Task: owner, Attempt: attempt, AttemptEvent: attemptEvent, TaskEvent: taskEvent}, nil
}

func executionProjectionReplay(ctx context.Context, q queryRower, owner Task, attempt Attempt, p RecordExecutionProjectionParams, payload []byte, attemptType, taskType string) (ExecutionProjection, error) {
	attemptEvent, err := scanEvent(q.QueryRowContext(ctx, eventSelect+` WHERE e.id=?`, p.AttemptEventID))
	if err != nil {
		return ExecutionProjection{}, fmt.Errorf("%w: execution projection replay attempt event", ErrInvalidState)
	}
	taskEvent, err := scanEvent(q.QueryRowContext(ctx, eventSelect+` WHERE e.id=?`, p.TaskEventID))
	if err != nil {
		return ExecutionProjection{}, fmt.Errorf("%w: execution projection replay task event", ErrInvalidState)
	}
	if attemptEvent.Type != attemptType || taskEvent.Type != taskType || attemptEvent.TaskID != owner.ID ||
		attemptEvent.AttemptID != attempt.ID || taskEvent.TaskID != owner.ID || taskEvent.AttemptID != "" ||
		attemptEvent.Cursor >= taskEvent.Cursor || owner.LatestEventCursor != taskEvent.Cursor ||
		!attemptEvent.OccurredAt.Equal(p.ObservedAt) || !taskEvent.OccurredAt.Equal(p.ObservedAt) ||
		attemptEvent.Actor != p.Actor || taskEvent.Actor != p.Actor ||
		!bytes.Equal(attemptEvent.Payload, payload) || !bytes.Equal(taskEvent.Payload, payload) {
		return ExecutionProjection{}, fmt.Errorf("%w: execution projection replay differs", ErrInvalidState)
	}
	return ExecutionProjection{Task: owner, Attempt: attempt, AttemptEvent: attemptEvent, TaskEvent: taskEvent}, nil
}

func executionProjectionPayload(p RecordExecutionProjectionParams) (json.RawMessage, error) {
	base, err := deliveryEvidencePayload(p.Reason, p.EvidencePayload, p.EvidenceSHA256)
	if err != nil {
		return nil, err
	}
	taskJSON, _ := json.Marshal(p.TaskID)
	fromJSON, _ := json.Marshal(p.ExpectedState)
	toJSON, _ := json.Marshal(p.Outcome)
	sessionJSON, _ := json.Marshal(p.OpenCodeSessionID)
	messageJSON, _ := json.Marshal(p.OpenCodeMessageID)
	payload := make([]byte, 0, len(base)+220)
	payload = append(payload, `{"taskId":`...)
	payload = append(payload, taskJSON...)
	payload = append(payload, `,"attemptId":`...)
	attemptJSON, _ := json.Marshal(p.AttemptID)
	payload = append(payload, attemptJSON...)
	payload = append(payload, fmt.Sprintf(`,"expectedAttemptRevision":%d,"expectedTaskRevision":%d,"from":`, p.ExpectedAttemptRevision, p.ExpectedTaskRevision)...)
	payload = append(payload, fromJSON...)
	payload = append(payload, `,"to":`...)
	payload = append(payload, toJSON...)
	payload = append(payload, `,"opencodeSessionId":`...)
	payload = append(payload, sessionJSON...)
	payload = append(payload, `,"opencodeMessageId":`...)
	payload = append(payload, messageJSON...)
	payload = append(payload, ',')
	payload = append(payload, base[1:]...)
	return payload, nil
}

func validateExecutionProjection(p RecordExecutionProjectionParams) error {
	if _, err := task.ParseTaskID(string(p.TaskID)); err != nil {
		return fmt.Errorf("%w: task ID", ErrInvalidInput)
	}
	if err := validateAttemptAndEvents(p.AttemptID, p.AttemptEventID, p.TaskEventID); err != nil {
		return err
	}
	if p.ExpectedAttemptRevision < 1 || p.ExpectedTaskRevision < 1 ||
		(p.ExpectedState != task.AttemptAdmitted && p.ExpectedState != task.AttemptRunning && p.ExpectedState != task.AttemptInputRequired) {
		return fmt.Errorf("%w: execution state or revisions", ErrInvalidInput)
	}
	if _, err := task.ParseOpenCodeSessionID(string(p.OpenCodeSessionID)); err != nil {
		return fmt.Errorf("%w: OpenCode session ID", ErrInvalidInput)
	}
	if _, err := task.ParseOpenCodeMessageID(string(p.OpenCodeMessageID)); err != nil {
		return fmt.Errorf("%w: OpenCode message ID", ErrInvalidInput)
	}
	if p.Outcome != ExecutionRunning && p.Outcome != ExecutionInputRequired && p.Outcome != ExecutionRecoveryRequired && p.Outcome != ExecutionFailed && p.Outcome != ExecutionSucceeded {
		return fmt.Errorf("%w: execution outcome", ErrInvalidInput)
	}
	if p.Outcome == ExecutionRecoveryRequired || p.Outcome == ExecutionFailed {
		if !validBoundedText(p.Reason, 1, 1000) {
			return fmt.Errorf("%w: execution reason", ErrInvalidInput)
		}
	} else if p.Reason != "" {
		return fmt.Errorf("%w: unexpected execution reason", ErrInvalidInput)
	}
	if err := validExactTimestamp(p.ObservedAt); err != nil {
		return err
	}
	if err := p.Actor.Validate(); err != nil || (p.Actor.Type != task.ActorSystem && p.Actor.Type != task.ActorRecovery) {
		return fmt.Errorf("%w: execution actor", ErrInvalidInput)
	}
	return validateDeliveryEvidence(p.EvidencePayload, p.EvidenceSHA256)
}
