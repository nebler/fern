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
	"strings"
	"time"

	"github.com/nebler/fern/internal/task"
)

const maxDeliveryEvidenceBytes = 16 * 1024

func deliveryRows(ctx context.Context, tx *sql.Tx, attemptID task.AttemptID) (Attempt, Task, error) {
	attempt, err := getAttempt(ctx, tx, attemptID)
	if errors.Is(err, ErrNotFound) {
		return Attempt{}, Task{}, &NotFoundError{Kind: "attempt", ID: string(attemptID)}
	}
	if err != nil {
		return Attempt{}, Task{}, err
	}
	if background, backgroundErr := backgroundAttemptExists(ctx, tx, attempt.ID); backgroundErr != nil {
		return Attempt{}, Task{}, backgroundErr
	} else if background {
		return Attempt{}, Task{}, &NotFoundError{Kind: "attempt", ID: string(attemptID)}
	}
	owner, err := getTask(ctx, tx, attempt.TaskID)
	if err != nil {
		return Attempt{}, Task{}, err
	}
	if owner.WorkspaceID != attempt.WorkspaceID {
		return Attempt{}, Task{}, fmt.Errorf("%w: attempt workspace ownership", ErrCorruptStore)
	}
	return attempt, owner, nil
}

func insertAttemptEvent(ctx context.Context, tx *sql.Tx, id task.EventID, attempt Attempt, eventType string, occurredAt, actorID int64, payload []byte) (Event, error) {
	return insertTaskStoreEvent(ctx, tx, id, attempt.WorkspaceID, attempt.TaskID, attempt.ID, "attempt", string(attempt.ID), eventType, occurredAt, actorID, payload)
}

func insertTaskEvent(ctx context.Context, tx *sql.Tx, id task.EventID, owner Task, eventType string, occurredAt, actorID int64, payload []byte) (Event, error) {
	return insertTaskStoreEvent(ctx, tx, id, owner.WorkspaceID, owner.ID, "", "task", string(owner.ID), eventType, occurredAt, actorID, payload)
}

func insertTaskStoreEvent(ctx context.Context, tx *sql.Tx, id task.EventID, workspaceID task.WorkspaceID, taskID task.TaskID, attemptID task.AttemptID, entityType, entityID, eventType string, occurredAt, actorID int64, payload []byte) (Event, error) {
	var attemptValue any
	if attemptID != "" {
		attemptValue = attemptID
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO events(id,workspace_id,task_id,attempt_id,entity_type,entity_id,type,version,occurred_at,actor_snapshot_id,payload)
VALUES(?,?,?,?,?,?,?,1,?,?,?)`, id, workspaceID, taskID, attemptValue, entityType, entityID, eventType, occurredAt, actorID, string(payload))
	if err != nil {
		return Event{}, fmt.Errorf("insert %s event: %w", eventType, err)
	}
	cursor, err := result.LastInsertId()
	if err != nil || cursor <= 0 {
		return Event{}, fmt.Errorf("read %s event cursor: %w", eventType, err)
	}
	event, err := scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.id=?`, id))
	if err != nil {
		return Event{}, fmt.Errorf("read %s event: %w", eventType, err)
	}
	if int64(event.Cursor) != cursor {
		return Event{}, fmt.Errorf("%w: event cursor changed", ErrCorruptStore)
	}
	return event, nil
}

func validateAttemptAndEvents(attemptID task.AttemptID, attemptEventID, taskEventID task.EventID) error {
	if _, err := task.ParseAttemptID(string(attemptID)); err != nil {
		return fmt.Errorf("%w: attempt ID", ErrInvalidInput)
	}
	if _, err := task.ParseEventID(string(attemptEventID)); err != nil {
		return fmt.Errorf("%w: attempt event ID", ErrInvalidInput)
	}
	if _, err := task.ParseEventID(string(taskEventID)); err != nil || attemptEventID == taskEventID {
		return fmt.Errorf("%w: task event ID", ErrInvalidInput)
	}
	return nil
}

func validExactTimestamp(value time.Time) error {
	if err := validTimestamp(value); err != nil || !value.Equal(fromUnixMillis(unixMillis(value))) {
		return fmt.Errorf("%w: timestamp must be exact Unix milliseconds", ErrInvalidInput)
	}
	return nil
}

func validateDeliveryEvidence(payload json.RawMessage, expected [32]byte) error {
	trimmed := bytes.TrimSpace(payload)
	if len(payload) < 2 || len(payload) > maxDeliveryEvidenceBytes || !json.Valid(payload) || len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return fmt.Errorf("%w: evidence must be a bounded JSON object", ErrInvalidInput)
	}
	if actual := sha256.Sum256(payload); actual != expected {
		return fmt.Errorf("%w: evidence hash", ErrInvalidInput)
	}
	var decoded any
	if json.Unmarshal(payload, &decoded) != nil || containsSensitiveEvidenceKey(decoded) {
		return fmt.Errorf("%w: evidence contains a sensitive raw field", ErrInvalidInput)
	}
	return nil
}

func deliveryEvidencePayload(reason string, evidence json.RawMessage, evidenceHash [32]byte) (json.RawMessage, error) {
	if err := validateDeliveryEvidence(evidence, evidenceHash); err != nil {
		return nil, err
	}
	var encoded bytes.Buffer
	encoded.WriteByte('{')
	if reason != "" {
		reasonJSON, err := json.Marshal(reason)
		if err != nil {
			return nil, fmt.Errorf("encode evidence reason: %w", err)
		}
		encoded.WriteString(`"reason":`)
		encoded.Write(reasonJSON)
		encoded.WriteByte(',')
	}
	encoded.WriteString(`"evidence":`)
	encoded.Write(evidence)
	encoded.WriteString(`,"evidenceSha256":"sha256:`)
	encoded.WriteString(hex.EncodeToString(evidenceHash[:]))
	encoded.WriteString(`"}`)
	if !json.Valid(encoded.Bytes()) {
		return nil, fmt.Errorf("%w: encoded evidence", ErrCorruptStore)
	}
	return encoded.Bytes(), nil
}

var sensitiveEvidenceKeyReplacer = strings.NewReplacer("_", "", "-", "", ".", "")

func containsSensitiveEvidenceKey(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			normalized := sensitiveEvidenceKeyReplacer.Replace(strings.ToLower(key))
			switch normalized {
			case "prompt", "rawprompt", "credential", "credentials", "authorization", "token", "cookie", "setcookie", "body", "rawbody", "requestbody", "responsebody":
				return true
			}
			if containsSensitiveEvidenceKey(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if containsSensitiveEvidenceKey(child) {
				return true
			}
		}
	}
	return false
}
