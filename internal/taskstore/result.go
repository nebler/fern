package taskstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

const maxManifestEntries = 10000

const resultSelect = `
SELECT r.id,r.task_id,r.attempt_id,r.workspace_id,r.state,r.outcome,r.repository_id,r.base_sha,
       r.result_commit,r.tree_oid,r.worktree_clean,r.manifest_entries,r.manifest_sha256,
       r.opencode_session_id,r.opencode_message_id,r.evidence_sha256,r.policy_version,
       r.collected_at,r.sealed_at,r.sealed_event_id,r.completed_event_id,r.revision,r.created_at,r.updated_at,
       a.actor_type,a.actor_id,a.display_name,a.credential_id,a.authentication,a.request_id
FROM results r JOIN actor_snapshots a ON a.id=r.creator_actor_snapshot_id`

// FindSucceededUnsealedAttempt returns current successful execution whose task
// is still eligible for result collection. SealResult rechecks every field.
func (s *Store) FindSucceededUnsealedAttempt(ctx context.Context, workspaceID task.WorkspaceID) (DeliveryWork, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil {
		return DeliveryWork{}, fmt.Errorf("%w: workspace ID", ErrInvalidInput)
	}
	var attemptID task.AttemptID
	err := s.db.QueryRowContext(ctx, `
SELECT a.id FROM attempts a JOIN tasks t ON t.id=a.task_id AND t.workspace_id=a.workspace_id
WHERE a.workspace_id=? AND a.state='succeeded' AND a.sealed_result_id IS NULL AND
      t.current_attempt_id=a.id AND t.state='running' AND t.cancel_epoch=0 AND t.sealed_result_id IS NULL
ORDER BY a.updated_at,a.id LIMIT 1`, workspaceID).Scan(&attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return DeliveryWork{}, &NotFoundError{Kind: "succeeded unsealed attempt", ID: string(workspaceID)}
	}
	if err != nil {
		return DeliveryWork{}, fmt.Errorf("find succeeded unsealed attempt: %w", err)
	}
	attempt, err := getAttempt(ctx, s.db, attemptID)
	if err != nil {
		return DeliveryWork{}, err
	}
	owner, err := getTask(ctx, s.db, attempt.TaskID)
	if err != nil {
		return DeliveryWork{}, err
	}
	return DeliveryWork{Task: owner, Attempt: attempt}, nil
}

func (s *Store) GetResult(ctx context.Context, id task.ResultID) (Result, error) {
	if _, err := task.ParseResultID(string(id)); err != nil {
		return Result{}, fmt.Errorf("%w: result ID", ErrInvalidInput)
	}
	return getResult(ctx, s.db, id)
}

func (s *Store) GetResultManifest(ctx context.Context, id task.ResultID) ([]ManifestEntry, error) {
	if _, err := task.ParseResultID(string(id)); err != nil {
		return nil, fmt.Errorf("%w: result ID", ErrInvalidInput)
	}
	if _, err := getResult(ctx, s.db, id); err != nil {
		return nil, err
	}
	return getResultManifest(ctx, s.db, id)
}

// SealResult atomically inserts one immutable result and manifest, binds it to
// the exact current successful attempt, and completes the unfenced task.
func (s *Store) SealResult(ctx context.Context, p SealResultParams) (_ SealedResult, err error) {
	manifest, err := validateSealResult(p)
	if err != nil {
		return SealedResult{}, err
	}
	p.Manifest = manifest
	payload, err := resultSealPayload(p)
	if err != nil {
		return SealedResult{}, err
	}

	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return SealedResult{}, fmt.Errorf("begin result seal: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	storedResult, resultErr := getResult(ctx, tx, p.ResultID)
	if resultErr == nil {
		replay, replayErr := resultSealReplay(ctx, tx, storedResult, p, payload)
		if replayErr != nil {
			return SealedResult{}, replayErr
		}
		if err := tx.Commit(); err != nil {
			return SealedResult{}, fmt.Errorf("finish result seal replay: %w", err)
		}
		replay.Replayed = true
		return replay, nil
	}
	if !errors.Is(resultErr, ErrNotFound) {
		return SealedResult{}, resultErr
	}

	attempt, owner, err := deliveryRows(ctx, tx, p.AttemptID)
	if err != nil {
		return SealedResult{}, err
	}
	if owner.ID != p.TaskID || owner.CurrentAttemptID != attempt.ID {
		return SealedResult{}, fmt.Errorf("%w: result attempt is not current", ErrInvalidState)
	}
	if attempt.Revision != p.ExpectedAttemptRevision {
		return SealedResult{}, &StaleRevisionError{AttemptID: attempt.ID, Expected: p.ExpectedAttemptRevision, Actual: attempt.Revision}
	}
	if owner.Revision != p.ExpectedTaskRevision {
		return SealedResult{}, &StaleTaskRevisionError{TaskID: owner.ID, Expected: p.ExpectedTaskRevision, Actual: owner.Revision}
	}
	if attempt.State != task.AttemptSucceeded || owner.State != task.TaskRunning || owner.CancelEpoch != 0 ||
		attempt.SealedResultID != "" || owner.SealedResultID != "" {
		return SealedResult{}, fmt.Errorf("%w: result source is not successful and unsealed", ErrInvalidState)
	}
	if owner.RepositoryID != p.RepositoryID || owner.BaseSHA != p.BaseSHA || attempt.BaseSHA != p.BaseSHA {
		return SealedResult{}, ErrRepositoryMismatch
	}
	if attempt.OpenCodeSessionID != p.OpenCodeSessionID || attempt.OpenCodeMessageID != p.OpenCodeMessageID {
		return SealedResult{}, fmt.Errorf("%w: exact OpenCode identity differs", ErrInvalidState)
	}
	if p.CollectedAt.Before(attempt.UpdatedAt) || p.SealedAt.Before(p.CollectedAt) {
		return SealedResult{}, fmt.Errorf("%w: result collection time", ErrInvalidInput)
	}

	actorID, err := ensureActor(ctx, tx, p.Actor)
	if err != nil {
		return SealedResult{}, err
	}
	sealedMS := unixMillis(p.SealedAt)
	resultEvent, err := insertAttemptEvent(ctx, tx, p.ResultEventID, attempt, "attempt.result_sealed", sealedMS, actorID, payload)
	if err != nil {
		return SealedResult{}, err
	}
	taskEvent, err := insertTaskEvent(ctx, tx, p.TaskEventID, owner, "task.completed", sealedMS, actorID, payload)
	if err != nil {
		return SealedResult{}, err
	}
	for i, entry := range p.Manifest {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO result_manifest(result_id,ordinal,path_base64,change_kind,old_mode,new_mode,old_blob_oid,new_blob_oid,old_size,new_size)
VALUES(?,?,?,?,?,?,?,?,?,?)`, p.ResultID, i, entry.PathBase64, entry.ChangeKind, entry.OldMode, entry.NewMode,
			entry.OldBlobOID, entry.NewBlobOID, entry.OldSize, entry.NewSize); err != nil {
			return SealedResult{}, fmt.Errorf("insert result manifest entry %d: %w", i, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO results(
 id,task_id,attempt_id,workspace_id,state,outcome,repository_id,base_sha,result_commit,tree_oid,
 worktree_clean,manifest_entries,manifest_sha256,opencode_session_id,opencode_message_id,evidence_sha256,
 policy_version,collected_at,sealed_at,creator_actor_snapshot_id,sealed_event_id,completed_event_id,revision,created_at,updated_at
) VALUES(?,?,?,?,'sealed',?,?,?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,1,?,?)`,
		p.ResultID, owner.ID, attempt.ID, owner.WorkspaceID, p.Outcome, p.RepositoryID, p.BaseSHA, p.ResultCommit, p.TreeOID,
		len(p.Manifest), p.ManifestSHA256[:], p.OpenCodeSessionID, p.OpenCodeMessageID, p.EvidenceSHA256[:],
		p.PolicyVersion, unixMillis(p.CollectedAt), sealedMS, actorID, resultEvent.ID, taskEvent.ID, sealedMS, sealedMS); err != nil {
		return SealedResult{}, fmt.Errorf("insert sealed result: %w", err)
	}
	update, err := tx.ExecContext(ctx, `
UPDATE attempts SET sealed_result_id=?,revision=revision+1,updated_at=?
WHERE id=? AND task_id=? AND state='succeeded' AND sealed_result_id IS NULL AND revision=?`,
		p.ResultID, sealedMS, attempt.ID, owner.ID, p.ExpectedAttemptRevision)
	if err != nil {
		return SealedResult{}, fmt.Errorf("bind result to attempt: %w", err)
	}
	if changed, changeErr := update.RowsAffected(); changeErr != nil || changed != 1 {
		return SealedResult{}, fmt.Errorf("%w: result attempt changed", ErrInvalidState)
	}
	update, err = tx.ExecContext(ctx, `
UPDATE tasks SET state='completed',sealed_result_id=?,latest_event_cursor=?,revision=revision+1,updated_at=?
WHERE id=? AND state='running' AND cancel_epoch=0 AND current_attempt_id=? AND sealed_result_id IS NULL AND revision=?`,
		p.ResultID, taskEvent.Cursor, sealedMS, owner.ID, attempt.ID, p.ExpectedTaskRevision)
	if err != nil {
		return SealedResult{}, fmt.Errorf("complete result task: %w", err)
	}
	if changed, changeErr := update.RowsAffected(); changeErr != nil || changed != 1 {
		return SealedResult{}, fmt.Errorf("%w: result task changed", ErrInvalidState)
	}

	result, err := getResult(ctx, tx, p.ResultID)
	if err != nil {
		return SealedResult{}, err
	}
	storedAttempt, err := getAttempt(ctx, tx, attempt.ID)
	if err != nil {
		return SealedResult{}, err
	}
	storedTask, err := getTask(ctx, tx, owner.ID)
	if err != nil {
		return SealedResult{}, err
	}
	if resultEvent.Cursor >= taskEvent.Cursor || storedTask.LatestEventCursor != taskEvent.Cursor {
		return SealedResult{}, fmt.Errorf("%w: result seal event ordering", ErrCorruptStore)
	}
	if err := tx.Commit(); err != nil {
		return SealedResult{}, fmt.Errorf("commit result seal: %w", err)
	}
	return SealedResult{Result: result, Manifest: p.Manifest, Task: storedTask, Attempt: storedAttempt, ResultEvent: resultEvent, TaskEvent: taskEvent}, nil
}

func getResult(ctx context.Context, q queryRower, id task.ResultID) (Result, error) {
	r, err := scanResult(q.QueryRowContext(ctx, resultSelect+` WHERE r.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, ErrNotFound
	}
	if err != nil {
		return Result{}, fmt.Errorf("read result: %w", err)
	}
	return r, nil
}

func scanResult(row rowScanner) (Result, error) {
	var r Result
	var repositoryID, clean, collectedAt, sealedAt, createdAt, updatedAt int64
	var manifestHash, evidenceHash []byte
	err := row.Scan(&r.ID, &r.TaskID, &r.AttemptID, &r.WorkspaceID, &r.State, &r.Outcome, &repositoryID, &r.BaseSHA,
		&r.ResultCommit, &r.TreeOID, &clean, &r.ManifestEntries, &manifestHash, &r.OpenCodeSessionID, &r.OpenCodeMessageID,
		&evidenceHash, &r.PolicyVersion, &collectedAt, &sealedAt, &r.SealedEventID, &r.CompletedEventID,
		&r.Revision, &createdAt, &updatedAt, &r.Creator.Type, &r.Creator.ID, &r.Creator.DisplayName,
		&r.Creator.CredentialID, &r.Creator.Authentication, &r.Creator.RequestID)
	if err != nil {
		return Result{}, err
	}
	if repositoryID <= 0 || clean != 1 || len(manifestHash) != 32 || len(evidenceHash) != 32 ||
		r.State != task.ResultSealed || r.Revision != 1 {
		return Result{}, ErrCorruptStore
	}
	r.RepositoryID = task.RepositoryID(repositoryID)
	r.WorktreeClean = true
	copy(r.ManifestSHA256[:], manifestHash)
	copy(r.EvidenceSHA256[:], evidenceHash)
	r.CollectedAt, r.SealedAt = fromUnixMillis(collectedAt), fromUnixMillis(sealedAt)
	r.CreatedAt, r.UpdatedAt = fromUnixMillis(createdAt), fromUnixMillis(updatedAt)
	return r, nil
}

func getResultManifest(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, id task.ResultID) ([]ManifestEntry, error) {
	rows, err := q.QueryContext(ctx, `
SELECT path_base64,change_kind,old_mode,new_mode,old_blob_oid,new_blob_oid,old_size,new_size
FROM result_manifest WHERE result_id=? ORDER BY ordinal`, id)
	if err != nil {
		return nil, fmt.Errorf("read result manifest: %w", err)
	}
	defer rows.Close()
	entries := make([]ManifestEntry, 0)
	for rows.Next() {
		var e ManifestEntry
		var oldMode, newMode, oldBlob, newBlob sql.NullString
		var oldSize, newSize sql.NullInt64
		if err := rows.Scan(&e.PathBase64, &e.ChangeKind, &oldMode, &newMode, &oldBlob, &newBlob, &oldSize, &newSize); err != nil {
			return nil, fmt.Errorf("scan result manifest: %w", err)
		}
		e.OldMode, e.NewMode = nullableString(oldMode), nullableString(newMode)
		e.OldBlobOID, e.NewBlobOID = nullableString(oldBlob), nullableString(newBlob)
		e.OldSize, e.NewSize = nullableInt64(oldSize), nullableInt64(newSize)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read result manifest: %w", err)
	}
	return entries, nil
}

func nullableInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func validateSealResult(p SealResultParams) ([]ManifestEntry, error) {
	if _, err := task.ParseResultID(string(p.ResultID)); err != nil {
		return nil, fmt.Errorf("%w: result ID", ErrInvalidInput)
	}
	if _, err := task.ParseTaskID(string(p.TaskID)); err != nil {
		return nil, fmt.Errorf("%w: task ID", ErrInvalidInput)
	}
	if err := validateAttemptAndEvents(p.AttemptID, p.ResultEventID, p.TaskEventID); err != nil {
		return nil, err
	}
	if p.ExpectedAttemptRevision < 1 || p.ExpectedTaskRevision < 1 {
		return nil, fmt.Errorf("%w: result revisions", ErrInvalidInput)
	}
	if _, err := task.ParseGitOID(string(p.BaseSHA)); err != nil {
		return nil, fmt.Errorf("%w: base SHA", ErrInvalidInput)
	}
	if _, err := task.ParseGitOID(string(p.ResultCommit)); err != nil {
		return nil, fmt.Errorf("%w: result commit", ErrInvalidInput)
	}
	if _, err := task.ParseGitOID(string(p.TreeOID)); err != nil {
		return nil, fmt.Errorf("%w: tree OID", ErrInvalidInput)
	}
	if _, err := task.ParseOpenCodeSessionID(string(p.OpenCodeSessionID)); err != nil {
		return nil, fmt.Errorf("%w: OpenCode session ID", ErrInvalidInput)
	}
	if _, err := task.ParseOpenCodeMessageID(string(p.OpenCodeMessageID)); err != nil {
		return nil, fmt.Errorf("%w: OpenCode message ID", ErrInvalidInput)
	}
	if p.RepositoryID == 0 || !p.WorktreeClean || !validBoundedText(p.PolicyVersion, 1, 128) {
		return nil, fmt.Errorf("%w: repository, cleanliness, or policy", ErrInvalidInput)
	}
	if err := validExactTimestamp(p.CollectedAt); err != nil {
		return nil, err
	}
	if err := validExactTimestamp(p.SealedAt); err != nil || p.SealedAt.Before(p.CollectedAt) {
		return nil, fmt.Errorf("%w: seal timestamp", ErrInvalidInput)
	}
	if err := p.Actor.Validate(); err != nil || (p.Actor.Type != task.ActorSystem && p.Actor.Type != task.ActorRecovery) {
		return nil, fmt.Errorf("%w: result actor", ErrInvalidInput)
	}
	if err := validateDeliveryEvidence(p.EvidencePayload, p.EvidenceSHA256); err != nil {
		return nil, err
	}
	manifest, err := validateManifest(p.Manifest)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode result manifest: %w", err)
	}
	if sha256.Sum256(encoded) != p.ManifestSHA256 {
		return nil, fmt.Errorf("%w: manifest digest", ErrInvalidInput)
	}
	tuple := task.ResultTuple{RepositoryTuple: task.RepositoryTuple{RepositoryID: p.RepositoryID, BaseSHA: p.BaseSHA},
		ResultCommit: p.ResultCommit, Outcome: p.Outcome, ManifestEntries: len(manifest), WorktreeClean: p.WorktreeClean}
	if err := tuple.ValidateAgainst(task.RepositoryTuple{RepositoryID: p.RepositoryID, BaseSHA: p.BaseSHA}); err != nil {
		return nil, fmt.Errorf("%w: result tuple: %v", ErrInvalidInput, err)
	}
	return manifest, nil
}

func validateManifest(input []ManifestEntry) ([]ManifestEntry, error) {
	if len(input) > maxManifestEntries {
		return nil, fmt.Errorf("%w: manifest entry count", ErrInvalidInput)
	}
	entries := append([]ManifestEntry{}, input...)
	var previous []byte
	for i, entry := range entries {
		path, err := base64.StdEncoding.DecodeString(entry.PathBase64)
		if err != nil || base64.StdEncoding.EncodeToString(path) != entry.PathBase64 || !validGitPath(path) {
			return nil, fmt.Errorf("%w: manifest path %d", ErrInvalidInput, i)
		}
		if i > 0 && bytes.Compare(previous, path) >= 0 {
			return nil, fmt.Errorf("%w: manifest paths are not strictly sorted", ErrInvalidInput)
		}
		previous = append(previous[:0], path...)
		if err := validateManifestEntry(entry); err != nil {
			return nil, fmt.Errorf("%w: manifest entry %d", ErrInvalidInput, i)
		}
	}
	return entries, nil
}

func validGitPath(path []byte) bool {
	if len(path) == 0 || len(path) > 4096 || path[0] == '/' || path[len(path)-1] == '/' || bytes.IndexByte(path, 0) >= 0 {
		return false
	}
	for _, part := range bytes.Split(path, []byte{'/'}) {
		if len(part) == 0 || bytes.Equal(part, []byte(".")) || bytes.Equal(part, []byte("..")) {
			return false
		}
	}
	return true
}

func validateManifestEntry(e ManifestEntry) error {
	validMode := func(v *string) bool {
		return v != nil && (*v == "100644" || *v == "100755" || *v == "120000")
	}
	validBlob := func(v *string) bool {
		if v == nil {
			return false
		}
		_, err := task.ParseGitOID(*v)
		return err == nil
	}
	validSize := func(v *int64) bool { return v != nil && *v >= 0 }
	oldPresent := validMode(e.OldMode) && validBlob(e.OldBlobOID) && validSize(e.OldSize)
	newPresent := validMode(e.NewMode) && validBlob(e.NewBlobOID) && validSize(e.NewSize)
	oldAbsent := e.OldMode == nil && e.OldBlobOID == nil && e.OldSize == nil
	newAbsent := e.NewMode == nil && e.NewBlobOID == nil && e.NewSize == nil
	switch e.ChangeKind {
	case "added":
		if !oldAbsent || !newPresent {
			return ErrInvalidInput
		}
	case "deleted":
		if !oldPresent || !newAbsent {
			return ErrInvalidInput
		}
	case "modified":
		if !oldPresent || !newPresent {
			return ErrInvalidInput
		}
	default:
		return ErrInvalidInput
	}
	return nil
}

func resultSealPayload(p SealResultParams) (json.RawMessage, error) {
	base, err := deliveryEvidencePayload("", p.EvidencePayload, p.EvidenceSHA256)
	if err != nil {
		return nil, err
	}
	type proof struct {
		ResultID                task.ResultID          `json:"resultId"`
		TaskID                  task.TaskID            `json:"taskId"`
		AttemptID               task.AttemptID         `json:"attemptId"`
		ExpectedAttemptRevision int64                  `json:"expectedAttemptRevision"`
		ExpectedTaskRevision    int64                  `json:"expectedTaskRevision"`
		RepositoryID            task.RepositoryID      `json:"repositoryId"`
		BaseSHA                 task.GitOID            `json:"baseSha"`
		ResultCommit            task.GitOID            `json:"resultCommit"`
		TreeOID                 task.GitOID            `json:"treeOid"`
		Outcome                 task.ResultOutcome     `json:"outcome"`
		Clean                   bool                   `json:"clean"`
		ManifestEntries         int                    `json:"manifestEntries"`
		ManifestSHA256          string                 `json:"manifestSha256"`
		OpenCodeSessionID       task.OpenCodeSessionID `json:"opencodeSessionId"`
		OpenCodeMessageID       task.OpenCodeMessageID `json:"opencodeMessageId"`
		EvidenceSHA256          string                 `json:"evidenceSha256"`
		PolicyVersion           string                 `json:"policyVersion"`
		CollectedAtMillis       int64                  `json:"collectedAtMillis"`
	}
	encoded, err := json.Marshal(proof{p.ResultID, p.TaskID, p.AttemptID, p.ExpectedAttemptRevision, p.ExpectedTaskRevision,
		p.RepositoryID, p.BaseSHA, p.ResultCommit, p.TreeOID, p.Outcome, p.WorktreeClean, len(p.Manifest),
		"sha256:" + hex.EncodeToString(p.ManifestSHA256[:]), p.OpenCodeSessionID, p.OpenCodeMessageID,
		"sha256:" + hex.EncodeToString(p.EvidenceSHA256[:]), p.PolicyVersion, unixMillis(p.CollectedAt)})
	if err != nil {
		return nil, fmt.Errorf("encode result proof: %w", err)
	}
	// Replace the proof's digest-only evidence field with the exact sanitized
	// evidence object while retaining the digest in the same canonical payload.
	encoded = encoded[:len(encoded)-1]
	encoded = append(encoded, `,"evidence":`...)
	var evidenceEnvelope map[string]json.RawMessage
	if err := json.Unmarshal(base, &evidenceEnvelope); err != nil {
		return nil, fmt.Errorf("%w: result evidence envelope", ErrCorruptStore)
	}
	encoded = append(encoded, evidenceEnvelope["evidence"]...)
	encoded = append(encoded, '}')
	return encoded, nil
}

func resultSealReplay(ctx context.Context, q interface {
	queryRower
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, result Result, p SealResultParams, payload []byte) (SealedResult, error) {
	owner, err := getTask(ctx, q, result.TaskID)
	if err != nil {
		return SealedResult{}, err
	}
	attempt, err := getAttempt(ctx, q, result.AttemptID)
	if err != nil {
		return SealedResult{}, err
	}
	manifest, err := getResultManifest(ctx, q, result.ID)
	if err != nil {
		return SealedResult{}, err
	}
	resultEvent, err := scanEvent(q.QueryRowContext(ctx, eventSelect+` WHERE e.id=?`, p.ResultEventID))
	if err != nil {
		return SealedResult{}, fmt.Errorf("%w: result seal replay event", ErrInvalidState)
	}
	taskEvent, err := scanEvent(q.QueryRowContext(ctx, eventSelect+` WHERE e.id=?`, p.TaskEventID))
	if err != nil {
		return SealedResult{}, fmt.Errorf("%w: result seal replay event", ErrInvalidState)
	}
	encodedManifest, _ := json.Marshal(manifest)
	requestedManifest, _ := json.Marshal(p.Manifest)
	if result.TaskID != p.TaskID || result.AttemptID != p.AttemptID || result.RepositoryID != p.RepositoryID ||
		result.BaseSHA != p.BaseSHA || result.ResultCommit != p.ResultCommit || result.TreeOID != p.TreeOID ||
		result.Outcome != p.Outcome || !result.WorktreeClean || result.ManifestSHA256 != p.ManifestSHA256 ||
		result.OpenCodeSessionID != p.OpenCodeSessionID || result.OpenCodeMessageID != p.OpenCodeMessageID ||
		result.EvidenceSHA256 != p.EvidenceSHA256 || result.PolicyVersion != p.PolicyVersion ||
		!result.CollectedAt.Equal(p.CollectedAt) || !result.SealedAt.Equal(p.SealedAt) || result.Creator != p.Actor ||
		result.SealedEventID != p.ResultEventID || result.CompletedEventID != p.TaskEventID ||
		attempt.Revision != p.ExpectedAttemptRevision+1 || owner.Revision != p.ExpectedTaskRevision+1 ||
		attempt.SealedResultID != result.ID || owner.SealedResultID != result.ID || owner.State != task.TaskCompleted ||
		!bytes.Equal(encodedManifest, requestedManifest) || resultEvent.Type != "attempt.result_sealed" ||
		taskEvent.Type != "task.completed" || resultEvent.Cursor >= taskEvent.Cursor || owner.LatestEventCursor != taskEvent.Cursor ||
		resultEvent.Actor != p.Actor || taskEvent.Actor != p.Actor || !bytes.Equal(resultEvent.Payload, payload) ||
		!bytes.Equal(taskEvent.Payload, payload) || !resultEvent.OccurredAt.Equal(p.SealedAt) || !taskEvent.OccurredAt.Equal(p.SealedAt) {
		return SealedResult{}, fmt.Errorf("%w: result seal replay differs", ErrInvalidState)
	}
	return SealedResult{Result: result, Manifest: manifest, Task: owner, Attempt: attempt, ResultEvent: resultEvent, TaskEvent: taskEvent}, nil
}
