package taskstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nebler/fern/internal/task"
)

type rowScanner interface {
	Scan(...any) error
}

const actorColumns = `a.actor_type,a.actor_id,a.display_name,a.credential_id,a.authentication,a.request_id`
const cancellationActorColumns = `ca.actor_type,ca.actor_id,ca.display_name,ca.credential_id,ca.authentication,ca.request_id`

const taskSelect = `
SELECT t.id,t.workspace_id,t.title,t.prompt,t.prompt_sha256,t.repository_id,t.base_ref,t.base_sha,
       t.object_format,t.state,t.terminal_reason,t.cancel_epoch,t.cancel_reason,t.cancel_requested_at,t.cancel_receipt_id,
       t.cancel_attempt_id,t.cancel_attempt_event_id,t.cancel_task_event_id,t.cancel_effect_disposition,
       t.current_attempt_id,t.sealed_result_id,t.latest_event_cursor,t.revision,t.created_at,t.updated_at,
       ` + actorColumns + `,` + cancellationActorColumns + `
FROM tasks t JOIN actor_snapshots a ON a.id=t.actor_snapshot_id
LEFT JOIN actor_snapshots ca ON ca.id=t.cancel_actor_snapshot_id`

const attemptSelect = `
SELECT id,task_id,workspace_id,sequence,state,delivery_phase,opencode_session_id,opencode_message_id,prompt_sha256,base_sha,
       image_digest,opencode_protocol,execution_contract_version,agent,model_provider,model,budget_snapshot,
       deadline,delivery_claim_owner,delivery_claim_expires_at,delivery_started_at,admitted_at,
       opencode_log_aggregate_id,opencode_log_seq,cancellation_ack_at,recovery_reason,terminal_reason,
        sealed_result_id,revision,created_at,updated_at
FROM attempts`

const receiptSelect = `
SELECT r.id,r.workspace_id,r.command_kind,r.state,r.idempotency_key,r.request_hash,r.accepted_at,
       r.api_contract_version,r.target_type,r.target_id,r.response_status,r.response_projection,
       ` + actorColumns + `
FROM receipts r JOIN actor_snapshots a ON a.id=r.actor_snapshot_id`

const eventSelect = `
SELECT e.id,e.cursor,e.workspace_id,e.task_id,e.attempt_id,e.entity_type,e.entity_id,e.type,e.version,e.occurred_at,e.payload,
       ` + actorColumns + `
FROM events e JOIN actor_snapshots a ON a.id=e.actor_snapshot_id`

func (s *Store) GetTask(ctx context.Context, id task.TaskID) (Task, error) {
	if _, err := task.ParseTaskID(string(id)); err != nil {
		return Task{}, fmt.Errorf("%w: task ID", ErrInvalidInput)
	}
	return getTask(ctx, s.db, id)
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getTask(ctx context.Context, q queryRower, id task.TaskID) (Task, error) {
	t, err := scanTask(q.QueryRowContext(ctx, taskSelect+` WHERE t.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, fmt.Errorf("read task: %w", err)
	}
	return t, nil
}

func scanTask(row rowScanner) (Task, error) {
	var t Task
	var promptHash []byte
	var repositoryID, cancelEpoch int64
	var createdAt, updatedAt int64
	var terminalReason, cancelReason, cancelReceiptID, cancelAttemptID, cancelAttemptEventID, cancelTaskEventID, cancelEffect, sealedResultID sql.NullString
	var cancelRequestedAt sql.NullInt64
	var cancelActorType, cancelActorID, cancelDisplayName, cancelCredentialID, cancelAuthentication, cancelRequestID sql.NullString
	err := row.Scan(
		&t.ID, &t.WorkspaceID, &t.Title, &t.Prompt, &promptHash, &repositoryID, &t.BaseRef, &t.BaseSHA,
		&t.ObjectFormat, &t.State, &terminalReason, &cancelEpoch, &cancelReason, &cancelRequestedAt, &cancelReceiptID,
		&cancelAttemptID, &cancelAttemptEventID, &cancelTaskEventID, &cancelEffect,
		&t.CurrentAttemptID, &sealedResultID, &t.LatestEventCursor, &t.Revision, &createdAt, &updatedAt,
		&t.Actor.Type, &t.Actor.ID, &t.Actor.DisplayName, &t.Actor.CredentialID, &t.Actor.Authentication, &t.Actor.RequestID,
		&cancelActorType, &cancelActorID, &cancelDisplayName, &cancelCredentialID, &cancelAuthentication, &cancelRequestID,
	)
	if err != nil {
		return Task{}, err
	}
	if len(promptHash) != len(t.PromptSHA256) || repositoryID <= 0 || cancelEpoch < 0 {
		return Task{}, ErrCorruptStore
	}
	copy(t.PromptSHA256[:], promptHash)
	t.RepositoryID = task.RepositoryID(repositoryID)
	t.TerminalReason = nullableString(terminalReason)
	t.CancelEpoch = uint64(cancelEpoch)
	if cancelEpoch == 1 {
		if !cancelActorType.Valid || !cancelActorID.Valid || !cancelCredentialID.Valid || !cancelAuthentication.Valid || !cancelRequestID.Valid ||
			!cancelRequestedAt.Valid || !cancelReceiptID.Valid || !cancelAttemptID.Valid || !cancelAttemptEventID.Valid || !cancelTaskEventID.Valid || !cancelEffect.Valid {
			return Task{}, ErrCorruptStore
		}
		actor := task.ActorSnapshot{Type: task.ActorType(cancelActorType.String), ID: cancelActorID.String, DisplayName: cancelDisplayName.String, CredentialID: cancelCredentialID.String, Authentication: cancelAuthentication.String, RequestID: cancelRequestID.String}
		if err := actor.Validate(); err != nil {
			return Task{}, ErrCorruptStore
		}
		t.CancellationActor = &actor
		t.CancellationReason = nullableString(cancelReason)
		t.CancellationRequestedAt = nullableTime(cancelRequestedAt)
		t.CancellationReceiptID = task.ReceiptID(cancelReceiptID.String)
		t.CancellationAttemptID = task.AttemptID(cancelAttemptID.String)
		t.CancellationAttemptEventID = task.EventID(cancelAttemptEventID.String)
		t.CancellationTaskEventID = task.EventID(cancelTaskEventID.String)
		t.CancellationEffect = CancellationEffectDisposition(cancelEffect.String)
		if !t.CancellationEffect.valid() {
			return Task{}, ErrCorruptStore
		}
	}
	t.CreatedAt, t.UpdatedAt = fromUnixMillis(createdAt), fromUnixMillis(updatedAt)
	if sealedResultID.Valid {
		t.SealedResultID = task.ResultID(sealedResultID.String)
	}
	return t, nil
}

func (s *Store) GetAttempt(ctx context.Context, id task.AttemptID) (Attempt, error) {
	if _, err := task.ParseAttemptID(string(id)); err != nil {
		return Attempt{}, fmt.Errorf("%w: attempt ID", ErrInvalidInput)
	}
	return getAttempt(ctx, s.db, id)
}

func getAttempt(ctx context.Context, q queryRower, id task.AttemptID) (Attempt, error) {
	a, err := scanAttempt(q.QueryRowContext(ctx, attemptSelect+` WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{}, ErrNotFound
	}
	if err != nil {
		return Attempt{}, fmt.Errorf("read attempt: %w", err)
	}
	return a, nil
}

func scanAttempt(row rowScanner) (Attempt, error) {
	var a Attempt
	var promptHash []byte
	var budget string
	var deadline, createdAt, updatedAt int64
	var claimOwner, logAggregateID, recoveryReason, terminalReason, sealedResultID sql.NullString
	var claimExpiresAt, deliveryStartedAt, admittedAt, cancellationAckAt sql.NullInt64
	err := row.Scan(
		&a.ID, &a.TaskID, &a.WorkspaceID, &a.Sequence, &a.State, &a.DeliveryPhase, &a.OpenCodeSessionID, &a.OpenCodeMessageID,
		&promptHash, &a.BaseSHA, &a.ImageDigest, &a.OpenCodeProtocol, &a.ExecutionContractVersion,
		&a.Agent, &a.ModelProvider, &a.Model, &budget, &deadline, &claimOwner, &claimExpiresAt,
		&deliveryStartedAt, &admittedAt, &logAggregateID, &a.OpenCodeLogSeq, &cancellationAckAt,
		&recoveryReason, &terminalReason, &sealedResultID, &a.Revision, &createdAt, &updatedAt,
	)
	if err != nil {
		return Attempt{}, err
	}
	if len(promptHash) != len(a.PromptSHA256) || !json.Valid([]byte(budget)) || a.Sequence <= 0 || !a.State.Valid() || !a.DeliveryPhase.valid() {
		return Attempt{}, ErrCorruptStore
	}
	copy(a.PromptSHA256[:], promptHash)
	a.BudgetSnapshot = json.RawMessage(budget)
	a.Deadline, a.CreatedAt, a.UpdatedAt = fromUnixMillis(deadline), fromUnixMillis(createdAt), fromUnixMillis(updatedAt)
	a.DeliveryClaimExpiresAt = nullableTime(claimExpiresAt)
	a.DeliveryStartedAt = nullableTime(deliveryStartedAt)
	a.AdmittedAt = nullableTime(admittedAt)
	a.DeliveryClaimOwner = nullableString(claimOwner)
	a.OpenCodeLogAggregateID = nullableString(logAggregateID)
	a.CancellationAckAt = nullableTime(cancellationAckAt)
	a.RecoveryReason = nullableString(recoveryReason)
	a.TerminalReason = nullableString(terminalReason)
	if sealedResultID.Valid {
		a.SealedResultID = task.ResultID(sealedResultID.String)
	}
	return a, nil
}

func nullableString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func nullableTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromUnixMillis(v.Int64)
	return &t
}

func (s *Store) GetReceipt(ctx context.Context, id task.ReceiptID) (Receipt, error) {
	if _, err := task.ParseReceiptID(string(id)); err != nil {
		return Receipt{}, fmt.Errorf("%w: receipt ID", ErrInvalidInput)
	}
	r, err := scanReceipt(s.db.QueryRowContext(ctx, receiptSelect+` WHERE r.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, ErrNotFound
	}
	if err != nil {
		return Receipt{}, fmt.Errorf("read receipt: %w", err)
	}
	return r, nil
}

// FindReceiptByIdempotency returns the durable command receipt for one exact
// workspace/kind/key scope. Ownership and request-hash classification remains
// in the command transition that consumes this read.
func (s *Store) FindReceiptByIdempotency(ctx context.Context, workspaceID task.WorkspaceID, commandKind string, key task.IdempotencyKey) (Receipt, bool, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil {
		return Receipt{}, false, fmt.Errorf("%w: workspace ID", ErrInvalidInput)
	}
	if (task.IdempotencyScope{WorkspaceID: workspaceID, CommandKind: commandKind}).Validate() != nil {
		return Receipt{}, false, fmt.Errorf("%w: command kind", ErrInvalidInput)
	}
	if _, err := task.ParseIdempotencyKey(string(key)); err != nil {
		return Receipt{}, false, fmt.Errorf("%w: idempotency key", ErrInvalidInput)
	}
	receipt, err := scanReceipt(s.db.QueryRowContext(ctx, receiptSelect+` WHERE r.workspace_id=? AND r.command_kind=? AND r.idempotency_key=?`, workspaceID, commandKind, key))
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, fmt.Errorf("find idempotency receipt: %w", err)
	}
	return receipt, true, nil
}

func scanReceipt(row rowScanner) (Receipt, error) {
	var r Receipt
	var requestHash []byte
	var acceptedAt int64
	var response string
	err := row.Scan(
		&r.ID, &r.WorkspaceID, &r.CommandKind, &r.State, &r.IdempotencyKey, &requestHash, &acceptedAt,
		&r.APIContractVersion, &r.TargetType, &r.TargetID, &r.ResponseStatus, &response,
		&r.Actor.Type, &r.Actor.ID, &r.Actor.DisplayName, &r.Actor.CredentialID, &r.Actor.Authentication, &r.Actor.RequestID,
	)
	if err != nil {
		return Receipt{}, err
	}
	if len(requestHash) != len(r.RequestHash) || !json.Valid([]byte(response)) {
		return Receipt{}, ErrCorruptStore
	}
	copy(r.RequestHash[:], requestHash)
	r.AcceptedAt = fromUnixMillis(acceptedAt)
	r.ResponseProjection = json.RawMessage(response)
	return r, nil
}

// ListEvents returns workspace events after an exclusive cursor, bounded by a
// watermark captured before the page query.
func (s *Store) ListEvents(ctx context.Context, workspaceID task.WorkspaceID, after task.Cursor, limit int) (EventPage, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil {
		return EventPage{}, fmt.Errorf("%w: workspace ID", ErrInvalidInput)
	}
	if err := after.Validate(); err != nil {
		return EventPage{}, fmt.Errorf("%w: after cursor", ErrInvalidInput)
	}
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 {
		return EventPage{}, fmt.Errorf("%w: event limit", ErrInvalidInput)
	}
	var watermark task.Cursor
	if err := s.db.QueryRowContext(ctx, `SELECT coalesce(max(cursor),0) FROM events WHERE workspace_id=?`, workspaceID).Scan(&watermark); err != nil {
		return EventPage{}, fmt.Errorf("read event watermark: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, eventSelect+`
WHERE e.workspace_id=? AND e.cursor>? AND e.cursor<=?
ORDER BY e.cursor ASC LIMIT ?`, workspaceID, after, watermark, limit)
	if err != nil {
		return EventPage{}, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	page := EventPage{NextCursor: after, Watermark: watermark}
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return EventPage{}, fmt.Errorf("read event: %w", err)
		}
		page.Events = append(page.Events, event)
		page.NextCursor = event.Cursor
	}
	if err := rows.Err(); err != nil {
		return EventPage{}, fmt.Errorf("list events: %w", err)
	}
	page.CaughtUp = page.NextCursor >= page.Watermark
	return page, nil
}

func admissionEvents(ctx context.Context, q queryRower, taskID task.TaskID, attemptID task.AttemptID) (Event, Event, error) {
	taskEvent, err := scanEvent(q.QueryRowContext(ctx, eventSelect+` WHERE e.task_id=? AND e.type='task.accepted' ORDER BY e.cursor ASC LIMIT 1`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, Event{}, fmt.Errorf("%w: accepted task has no event", ErrCorruptStore)
	}
	if err != nil {
		return Event{}, Event{}, fmt.Errorf("read acceptance event: %w", err)
	}
	attemptEvent, err := scanEvent(q.QueryRowContext(ctx, eventSelect+` WHERE e.task_id=? AND e.attempt_id=? AND e.type='attempt.prepared' ORDER BY e.cursor ASC LIMIT 1`, taskID, attemptID))
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, Event{}, fmt.Errorf("%w: prepared attempt has no event", ErrCorruptStore)
	}
	if err != nil {
		return Event{}, Event{}, fmt.Errorf("read prepared event: %w", err)
	}
	if taskEvent.Cursor >= attemptEvent.Cursor {
		return Event{}, Event{}, fmt.Errorf("%w: admission event ordering", ErrCorruptStore)
	}
	return taskEvent, attemptEvent, nil
}

func scanEvent(row rowScanner) (Event, error) {
	var e Event
	var taskID, attemptID sql.NullString
	var occurredAt int64
	var payload string
	err := row.Scan(
		&e.ID, &e.Cursor, &e.WorkspaceID, &taskID, &attemptID, &e.EntityType, &e.EntityID, &e.Type, &e.Version, &occurredAt, &payload,
		&e.Actor.Type, &e.Actor.ID, &e.Actor.DisplayName, &e.Actor.CredentialID, &e.Actor.Authentication, &e.Actor.RequestID,
	)
	if err != nil {
		return Event{}, err
	}
	if taskID.Valid {
		e.TaskID = task.TaskID(taskID.String)
	}
	if attemptID.Valid {
		e.AttemptID = task.AttemptID(attemptID.String)
	}
	if err := e.Cursor.ValidateEvent(); err != nil || !json.Valid([]byte(payload)) {
		return Event{}, ErrCorruptStore
	}
	e.OccurredAt = fromUnixMillis(occurredAt)
	e.Payload = json.RawMessage(payload)
	return e, nil
}
