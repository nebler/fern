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

const taskSelect = `
SELECT t.id,t.workspace_id,t.title,t.prompt,t.prompt_sha256,t.repository_id,t.base_ref,t.base_sha,
       t.object_format,t.state,t.terminal_reason,
       t.current_attempt_id,t.sealed_result_id,t.latest_event_cursor,t.revision,t.created_at,t.updated_at,
       ` + actorColumns + `
FROM tasks t JOIN actor_snapshots a ON a.id=t.actor_snapshot_id`

const attemptSelect = `
SELECT id,task_id,workspace_id,sequence,state,delivery_phase,opencode_session_id,opencode_message_id,prompt_sha256,base_sha,
       image_digest,opencode_protocol,execution_contract_version,agent,model_provider,model,
       deadline,delivery_claim_owner,delivery_claim_expires_at,delivery_started_at,admitted_at,
       opencode_log_aggregate_id,opencode_log_seq,recovery_reason,terminal_reason,
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
	var repositoryID int64
	var createdAt, updatedAt int64
	var terminalReason, sealedResultID sql.NullString
	err := row.Scan(
		&t.ID, &t.WorkspaceID, &t.Title, &t.Prompt, &promptHash, &repositoryID, &t.BaseRef, &t.BaseSHA,
		&t.ObjectFormat, &t.State, &terminalReason,
		&t.CurrentAttemptID, &sealedResultID, &t.LatestEventCursor, &t.Revision, &createdAt, &updatedAt,
		&t.Actor.Type, &t.Actor.ID, &t.Actor.DisplayName, &t.Actor.CredentialID, &t.Actor.Authentication, &t.Actor.RequestID,
	)
	if err != nil {
		return Task{}, err
	}
	if len(promptHash) != len(t.PromptSHA256) || repositoryID <= 0 {
		return Task{}, ErrCorruptStore
	}
	copy(t.PromptSHA256[:], promptHash)
	t.RepositoryID = task.RepositoryID(repositoryID)
	t.TerminalReason = nullableString(terminalReason)
	t.CreatedAt, t.UpdatedAt = fromUnixMillis(createdAt), fromUnixMillis(updatedAt)
	if sealedResultID.Valid {
		t.SealedResultID = task.ResultID(sealedResultID.String)
	}
	return t, nil
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
	var deadline, createdAt, updatedAt int64
	var claimOwner, logAggregateID, recoveryReason, terminalReason, sealedResultID sql.NullString
	var claimExpiresAt, deliveryStartedAt, admittedAt sql.NullInt64
	err := row.Scan(
		&a.ID, &a.TaskID, &a.WorkspaceID, &a.Sequence, &a.State, &a.DeliveryPhase, &a.OpenCodeSessionID, &a.OpenCodeMessageID,
		&promptHash, &a.BaseSHA, &a.ImageDigest, &a.OpenCodeProtocol, &a.ExecutionContractVersion,
		&a.Agent, &a.ModelProvider, &a.Model, &deadline, &claimOwner, &claimExpiresAt,
		&deliveryStartedAt, &admittedAt, &logAggregateID, &a.OpenCodeLogSeq,
		&recoveryReason, &terminalReason, &sealedResultID, &a.Revision, &createdAt, &updatedAt,
	)
	if err != nil {
		return Attempt{}, err
	}
	if len(promptHash) != len(a.PromptSHA256) || a.Sequence <= 0 || !a.State.Valid() || !a.DeliveryPhase.valid() {
		return Attempt{}, ErrCorruptStore
	}
	copy(a.PromptSHA256[:], promptHash)
	a.Deadline, a.CreatedAt, a.UpdatedAt = fromUnixMillis(deadline), fromUnixMillis(createdAt), fromUnixMillis(updatedAt)
	a.DeliveryClaimExpiresAt = nullableTime(claimExpiresAt)
	a.DeliveryStartedAt = nullableTime(deliveryStartedAt)
	a.AdmittedAt = nullableTime(admittedAt)
	a.DeliveryClaimOwner = nullableString(claimOwner)
	a.OpenCodeLogAggregateID = nullableString(logAggregateID)
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
