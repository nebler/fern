package taskstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nebler/fern/internal/task"
)

const journalEventSelect = `
SELECT e.id,e.workspace_id,e.task_id,e.attempt_id,e.result_id,e.entity_type,e.entity_id,e.type,
       e.from_state,e.to_state,e.entity_revision,e.occurred_at,e.evidence_sha256,e.payload,
       a.actor_type,a.actor_id,a.display_name,a.credential_id,a.authentication,a.request_id
FROM journal_events e JOIN actor_snapshots a ON a.id=e.actor_snapshot_id`

type journalEnvelope struct {
	Detail         json.RawMessage `json:"detail"`
	Evidence       json.RawMessage `json:"evidence"`
	EvidenceSHA256 string          `json:"evidenceSha256"`
}

func journalPayload(detail any, evidence json.RawMessage, evidenceHash [32]byte) (json.RawMessage, error) {
	if err := validateDeliveryEvidence(evidence, evidenceHash); err != nil {
		return nil, err
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return nil, fmt.Errorf("encode journal detail: %w", err)
	}
	payload, err := json.Marshal(journalEnvelope{
		Detail: detailJSON, Evidence: evidence,
		EvidenceSHA256: "sha256:" + hex.EncodeToString(evidenceHash[:]),
	})
	if err != nil || len(payload) > maxDeliveryEvidenceBytes {
		return nil, fmt.Errorf("%w: journal payload", ErrInvalidInput)
	}
	return payload, nil
}

func insertJournalEvent(ctx context.Context, tx *sql.Tx, id task.EventID, entityType, entityID, eventType, from, to string,
	revision int64, at time.Time, actorID int64, result Result, evidenceHash [32]byte, payload json.RawMessage,
) (JournalEvent, error) {
	if _, err := task.ParseEventID(string(id)); err != nil {
		return JournalEvent{}, fmt.Errorf("%w: journal event ID", ErrInvalidInput)
	}
	var fromValue any
	if from != "" {
		fromValue = from
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO journal_events(id,workspace_id,task_id,attempt_id,result_id,entity_type,entity_id,type,from_state,to_state,
 entity_revision,occurred_at,actor_snapshot_id,evidence_sha256,payload)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, result.WorkspaceID, result.TaskID, result.AttemptID, result.ID,
		entityType, entityID, eventType, fromValue, to, revision, unixMillis(at), actorID, evidenceHash[:], string(payload)); err != nil {
		return JournalEvent{}, fmt.Errorf("insert %s event: %w", entityType, err)
	}
	return getJournalEvent(ctx, tx, id)
}

func (s *Store) GetJournalEvent(ctx context.Context, id task.EventID) (JournalEvent, error) {
	if _, err := task.ParseEventID(string(id)); err != nil {
		return JournalEvent{}, fmt.Errorf("%w: journal event ID", ErrInvalidInput)
	}
	return getJournalEvent(ctx, s.db, id)
}

func getJournalEvent(ctx context.Context, q queryRower, id task.EventID) (JournalEvent, error) {
	e, err := scanJournalEvent(q.QueryRowContext(ctx, journalEventSelect+` WHERE e.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return JournalEvent{}, ErrNotFound
	}
	if err != nil {
		return JournalEvent{}, fmt.Errorf("read journal event: %w", err)
	}
	return e, nil
}

func scanJournalEvent(row rowScanner) (JournalEvent, error) {
	var e JournalEvent
	var from sql.NullString
	var occurred int64
	var evidence []byte
	var payload string
	if err := row.Scan(&e.ID, &e.WorkspaceID, &e.TaskID, &e.AttemptID, &e.ResultID, &e.EntityType, &e.EntityID,
		&e.Type, &from, &e.ToState, &e.EntityRevision, &occurred, &evidence, &payload,
		&e.Actor.Type, &e.Actor.ID, &e.Actor.DisplayName, &e.Actor.CredentialID, &e.Actor.Authentication, &e.Actor.RequestID); err != nil {
		return JournalEvent{}, err
	}
	if len(evidence) != sha256.Size || !json.Valid([]byte(payload)) || e.EntityRevision < 1 {
		return JournalEvent{}, ErrCorruptStore
	}
	e.FromState = from.String
	e.OccurredAt = fromUnixMillis(occurred)
	copy(e.EvidenceSHA256[:], evidence)
	e.Payload = json.RawMessage(payload)
	return e, nil
}

func exactJournalEvent(ctx context.Context, q queryRower, id, latest task.EventID, entityType, entityID, eventType, from, to string,
	revision int64, at time.Time, actor task.ActorSnapshot, payload []byte,
) (JournalEvent, error) {
	if id != latest {
		return JournalEvent{}, fmt.Errorf("%w: replay event is not latest", ErrInvalidState)
	}
	e, err := getJournalEvent(ctx, q, id)
	if err != nil || e.EntityType != entityType || e.EntityID != entityID || e.Type != eventType || e.FromState != from ||
		e.ToState != to || e.EntityRevision != revision || !e.OccurredAt.Equal(at) || e.Actor != actor || !bytes.Equal(e.Payload, payload) {
		return JournalEvent{}, fmt.Errorf("%w: journal replay differs", ErrInvalidState)
	}
	return e, nil
}

func journalSource(ctx context.Context, q queryRower, resultID task.ResultID, expectedTaskRevision, expectedAttemptRevision int64) (Result, Task, Attempt, error) {
	result, err := getResult(ctx, q, resultID)
	if err != nil {
		return Result{}, Task{}, Attempt{}, err
	}
	owner, err := getTask(ctx, q, result.TaskID)
	if err != nil {
		return Result{}, Task{}, Attempt{}, err
	}
	attempt, err := getAttempt(ctx, q, result.AttemptID)
	if err != nil {
		return Result{}, Task{}, Attempt{}, err
	}
	if owner.Revision != expectedTaskRevision {
		return Result{}, Task{}, Attempt{}, &StaleTaskRevisionError{TaskID: owner.ID, Expected: expectedTaskRevision, Actual: owner.Revision}
	}
	if attempt.Revision != expectedAttemptRevision {
		return Result{}, Task{}, Attempt{}, &StaleRevisionError{AttemptID: attempt.ID, Expected: expectedAttemptRevision, Actual: attempt.Revision}
	}
	if result.State != task.ResultSealed || owner.CurrentAttemptID != attempt.ID || owner.SealedResultID != result.ID ||
		owner.State != task.TaskCompleted || owner.CancelEpoch != 0 || attempt.State != task.AttemptSucceeded || attempt.SealedResultID != result.ID {
		return Result{}, Task{}, Attempt{}, fmt.Errorf("%w: sealed result is not currently owned and unfenced", ErrInvalidState)
	}
	return result, owner, attempt, nil
}

func validateJournalTransition(expectedRevision, expectedTaskRevision, expectedAttemptRevision int64, eventID task.EventID,
	at time.Time, evidence json.RawMessage, evidenceHash [32]byte, actor task.ActorSnapshot,
) error {
	if expectedRevision < 1 || expectedTaskRevision < 1 || expectedAttemptRevision < 1 {
		return fmt.Errorf("%w: journal revisions", ErrInvalidInput)
	}
	if _, err := task.ParseEventID(string(eventID)); err != nil {
		return fmt.Errorf("%w: journal event ID", ErrInvalidInput)
	}
	if err := validExactTimestamp(at); err != nil {
		return err
	}
	if err := actor.Validate(); err != nil || (actor.Type != task.ActorSystem && actor.Type != task.ActorRecovery) {
		return fmt.Errorf("%w: journal transition actor", ErrInvalidInput)
	}
	return validateDeliveryEvidence(evidence, evidenceHash)
}
