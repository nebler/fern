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

// actorRecord is the stored JSON form of an actor snapshot. Actors are
// attribution on the row that names them, not an interned table.
type actorRecord struct {
	Type           task.ActorType `json:"type"`
	ID             string         `json:"id"`
	DisplayName    string         `json:"display_name"`
	CredentialID   string         `json:"credential_id"`
	Authentication string         `json:"authentication"`
	RequestID      string         `json:"request_id"`
}

func encodeActor(actor task.ActorSnapshot) string {
	encoded, _ := json.Marshal(actorRecord(actor))
	return string(encoded)
}

func decodeActor(value string) (task.ActorSnapshot, error) {
	var record actorRecord
	if err := json.Unmarshal([]byte(value), &record); err != nil {
		return task.ActorSnapshot{}, fmt.Errorf("%w: actor: %v", ErrCorruptStore, err)
	}
	return task.ActorSnapshot(record), nil
}

// ownedBy restricts a run query (aliased r) to one authenticated authority;
// display name and request ID are not authority.
const ownedBy = ` AND json_extract(r.creator_actor,'$.type')=? AND json_extract(r.creator_actor,'$.id')=?
AND json_extract(r.creator_actor,'$.credential_id')=? AND json_extract(r.creator_actor,'$.authentication')=?`

func ownerArgs(actor task.ActorSnapshot) []any {
	return []any{actor.Type, actor.ID, actor.CredentialID, actor.Authentication}
}

const taskSelect = `
SELECT t.id,t.workspace_id,t.title,t.prompt,t.prompt_sha256,t.repository_id,t.base_ref,t.base_sha,
       t.object_format,t.state,t.terminal_reason,
       t.current_attempt_id,t.sealed_result_id,t.revision,t.created_at,t.updated_at
FROM tasks t`

const attemptSelect = `
SELECT id,task_id,workspace_id,sequence,state,opencode_session_id,opencode_message_id,prompt_sha256,base_sha,
       image_digest,opencode_protocol,execution_contract_version,agent,model_provider,model,
       deadline,terminal_reason,sealed_result_id,revision,created_at,updated_at
FROM attempts`

const receiptSelect = `
SELECT r.id,r.workspace_id,r.command_kind,r.state,r.idempotency_key,r.request_hash,r.accepted_at,
       r.api_contract_version,r.target_type,r.target_id,r.response_status,r.response_projection,r.actor
FROM receipts r`

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
		&t.CurrentAttemptID, &sealedResultID, &t.Revision, &createdAt, &updatedAt,
	)
	if err != nil {
		return Task{}, err
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
	var terminalReason, sealedResultID sql.NullString
	err := row.Scan(
		&a.ID, &a.TaskID, &a.WorkspaceID, &a.Sequence, &a.State, &a.OpenCodeSessionID, &a.OpenCodeMessageID,
		&promptHash, &a.BaseSHA, &a.ImageDigest, &a.OpenCodeProtocol, &a.ExecutionContractVersion,
		&a.Agent, &a.ModelProvider, &a.Model, &deadline,
		&terminalReason, &sealedResultID, &a.Revision, &createdAt, &updatedAt,
	)
	if err != nil {
		return Attempt{}, err
	}
	copy(a.PromptSHA256[:], promptHash)
	a.Deadline, a.CreatedAt, a.UpdatedAt = fromUnixMillis(deadline), fromUnixMillis(createdAt), fromUnixMillis(updatedAt)
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
	var response, actor string
	err := row.Scan(
		&r.ID, &r.WorkspaceID, &r.CommandKind, &r.State, &r.IdempotencyKey, &requestHash, &acceptedAt,
		&r.APIContractVersion, &r.TargetType, &r.TargetID, &r.ResponseStatus, &response, &actor,
	)
	if err != nil {
		return Receipt{}, err
	}
	if r.Actor, err = decodeActor(actor); err != nil {
		return Receipt{}, err
	}
	copy(r.RequestHash[:], requestHash)
	r.AcceptedAt = fromUnixMillis(acceptedAt)
	r.ResponseProjection = json.RawMessage(response)
	return r, nil
}
