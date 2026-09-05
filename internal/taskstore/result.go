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

	"github.com/nebler/fern/internal/gitref"
	"github.com/nebler/fern/internal/task"
)

const maxManifestEntries = 10000

const resultSelect = `
SELECT r.id,r.task_id,r.attempt_id,r.workspace_id,r.state,r.outcome,r.repository_id,r.base_sha,
       r.result_commit,r.tree_oid,r.worktree_clean,r.manifest_entries,r.manifest_sha256,
	       r.opencode_session_id,r.opencode_message_id,r.evidence_sha256,r.policy_version,
	       r.collected_at,r.sealed_at,r.sealed_event_id,r.completed_event_id,r.revision,r.created_at,r.updated_at,
	       a.actor_type,a.actor_id,a.display_name,a.credential_id,a.authentication,a.request_id,
	       r.completion_authority,r.seal_request_id,
	       aa.actor_type,aa.actor_id,aa.display_name,aa.credential_id,aa.authentication,aa.request_id,
	       r.source_kind,r.retained_artifact_id,r.artifact_export_id,r.materialization_id
	FROM results r JOIN actor_snapshots a ON a.id=r.creator_actor_snapshot_id
	LEFT JOIN actor_snapshots aa ON aa.id=r.authorizer_actor_snapshot_id`

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
	var sealRequestID sql.NullString
	var retainedArtifactID, artifactExportID, materializationID sql.NullString
	var authorizerType, authorizerID, authorizerDisplayName, authorizerCredentialID, authorizerAuthentication, authorizerRequestID sql.NullString
	err := row.Scan(&r.ID, &r.TaskID, &r.AttemptID, &r.WorkspaceID, &r.State, &r.Outcome, &repositoryID, &r.BaseSHA,
		&r.ResultCommit, &r.TreeOID, &clean, &r.ManifestEntries, &manifestHash, &r.OpenCodeSessionID, &r.OpenCodeMessageID,
		&evidenceHash, &r.PolicyVersion, &collectedAt, &sealedAt, &r.SealedEventID, &r.CompletedEventID,
		&r.Revision, &createdAt, &updatedAt, &r.Creator.Type, &r.Creator.ID, &r.Creator.DisplayName,
		&r.Creator.CredentialID, &r.Creator.Authentication, &r.Creator.RequestID, &r.CompletionAuthority, &sealRequestID,
		&authorizerType, &authorizerID, &authorizerDisplayName, &authorizerCredentialID, &authorizerAuthentication, &authorizerRequestID,
		&r.SourceKind, &retainedArtifactID, &artifactExportID, &materializationID)
	if err != nil {
		return Result{}, err
	}
	if repositoryID <= 0 || clean != 1 || len(manifestHash) != 32 || len(evidenceHash) != 32 ||
		r.State != task.ResultSealed || r.Revision != 1 ||
		(r.CompletionAuthority != SealAuthorityExecutionSuccess && r.CompletionAuthority != SealAuthorityUser) ||
		(r.SourceKind != ResultSourcePersistentWorkspace && r.SourceKind != ResultSourceRetainedArtifact) {
		return Result{}, ErrCorruptStore
	}
	r.RepositoryID = task.RepositoryID(repositoryID)
	r.WorktreeClean = true
	copy(r.ManifestSHA256[:], manifestHash)
	copy(r.EvidenceSHA256[:], evidenceHash)
	r.CollectedAt, r.SealedAt = fromUnixMillis(collectedAt), fromUnixMillis(sealedAt)
	r.CreatedAt, r.UpdatedAt = fromUnixMillis(createdAt), fromUnixMillis(updatedAt)
	if r.SourceKind == ResultSourceRetainedArtifact {
		if !retainedArtifactID.Valid || !artifactExportID.Valid || !materializationID.Valid {
			return Result{}, ErrCorruptStore
		}
		r.RetainedArtifactID = task.RetainedArtifactID(retainedArtifactID.String)
		r.ArtifactExportID = task.ArtifactExportID(artifactExportID.String)
		r.MaterializationID = task.MaterializationID(materializationID.String)
	} else if retainedArtifactID.Valid || artifactExportID.Valid || materializationID.Valid {
		return Result{}, ErrCorruptStore
	}
	if r.SourceKind == ResultSourceRetainedArtifact {
		if r.CompletionAuthority != SealAuthorityUser || sealRequestID.Valid || authorizerType.Valid {
			return Result{}, ErrCorruptStore
		}
	} else if r.CompletionAuthority == SealAuthorityExecutionSuccess {
		if sealRequestID.Valid || authorizerType.Valid {
			return Result{}, ErrCorruptStore
		}
	} else {
		if !sealRequestID.Valid || !authorizerType.Valid || !authorizerID.Valid || !authorizerCredentialID.Valid || !authorizerAuthentication.Valid || !authorizerRequestID.Valid {
			return Result{}, ErrCorruptStore
		}
		r.SealRequestID = task.SealRequestID(sealRequestID.String)
		authorizer := task.ActorSnapshot{Type: task.ActorType(authorizerType.String), ID: authorizerID.String, DisplayName: authorizerDisplayName.String,
			CredentialID: authorizerCredentialID.String, Authentication: authorizerAuthentication.String, RequestID: authorizerRequestID.String}
		if err := authorizer.Validate(); err != nil {
			return Result{}, ErrCorruptStore
		}
		r.Authorizer = &authorizer
	}
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

func validateResultMaterial(p resultMaterial) ([]ManifestEntry, error) {
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
		if err != nil || base64.StdEncoding.EncodeToString(path) != entry.PathBase64 || !gitref.ValidPathBytes(path) {
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

func resultSealPayload(p resultMaterial) (json.RawMessage, error) {
	base, err := deliveryEvidencePayload("", p.EvidencePayload, p.EvidenceSHA256)
	if err != nil {
		return nil, err
	}
	type proof struct {
		ResultID                task.ResultID           `json:"resultId"`
		TaskID                  task.TaskID             `json:"taskId"`
		AttemptID               task.AttemptID          `json:"attemptId"`
		ExpectedAttemptRevision int64                   `json:"expectedAttemptRevision"`
		ExpectedTaskRevision    int64                   `json:"expectedTaskRevision"`
		RepositoryID            task.RepositoryID       `json:"repositoryId"`
		BaseSHA                 task.GitOID             `json:"baseSha"`
		ResultCommit            task.GitOID             `json:"resultCommit"`
		TreeOID                 task.GitOID             `json:"treeOid"`
		Outcome                 task.ResultOutcome      `json:"outcome"`
		Clean                   bool                    `json:"clean"`
		ManifestEntries         int                     `json:"manifestEntries"`
		ManifestSHA256          string                  `json:"manifestSha256"`
		OpenCodeSessionID       task.OpenCodeSessionID  `json:"opencodeSessionId"`
		OpenCodeMessageID       task.OpenCodeMessageID  `json:"opencodeMessageId"`
		EvidenceSHA256          string                  `json:"evidenceSha256"`
		PolicyVersion           string                  `json:"policyVersion"`
		CollectedAtMillis       int64                   `json:"collectedAtMillis"`
		CompletionAuthority     SealCompletionAuthority `json:"completionAuthority"`
	}
	encoded, err := json.Marshal(proof{p.ResultID, p.TaskID, p.AttemptID, p.ExpectedAttemptRevision, p.ExpectedTaskRevision,
		p.RepositoryID, p.BaseSHA, p.ResultCommit, p.TreeOID, p.Outcome, p.WorktreeClean, len(p.Manifest),
		"sha256:" + hex.EncodeToString(p.ManifestSHA256[:]), p.OpenCodeSessionID, p.OpenCodeMessageID,
		"sha256:" + hex.EncodeToString(p.EvidenceSHA256[:]), p.PolicyVersion, unixMillis(p.CollectedAt),
		p.CompletionAuthority})
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
