package taskstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/nebler/fern/internal/gitref"
	"github.com/nebler/fern/internal/task"
)

const maxManifestEntries = 10000

const resultSelect = `
SELECT r.id,r.task_id,r.attempt_id,r.workspace_id,r.state,r.outcome,r.repository_id,r.base_sha,
       r.result_commit,r.tree_oid,r.worktree_clean,r.manifest_entries,r.manifest_sha256,
       r.opencode_session_id,r.opencode_message_id,r.policy_version,
       r.collected_at,r.sealed_at,r.revision,r.created_at,r.updated_at,r.completion_authority,
       r.source_kind,r.retained_artifact_id,r.artifact_export_id,r.materialization_id
FROM results r`

func (s *Store) GetResult(ctx context.Context, id task.ResultID) (Result, error) {
	return getResult(ctx, s.db, id)
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
	var manifestHash []byte
	var retainedArtifactID, artifactExportID, materializationID sql.NullString
	err := row.Scan(&r.ID, &r.TaskID, &r.AttemptID, &r.WorkspaceID, &r.State, &r.Outcome, &repositoryID, &r.BaseSHA,
		&r.ResultCommit, &r.TreeOID, &clean, &r.ManifestEntries, &manifestHash, &r.OpenCodeSessionID, &r.OpenCodeMessageID,
		&r.PolicyVersion, &collectedAt, &sealedAt, &r.Revision, &createdAt, &updatedAt, &r.CompletionAuthority,
		&r.SourceKind, &retainedArtifactID, &artifactExportID, &materializationID)
	if err != nil {
		return Result{}, err
	}
	r.RepositoryID = task.RepositoryID(repositoryID)
	r.WorktreeClean = true
	copy(r.ManifestSHA256[:], manifestHash)
	r.CollectedAt, r.SealedAt = fromUnixMillis(collectedAt), fromUnixMillis(sealedAt)
	r.CreatedAt, r.UpdatedAt = fromUnixMillis(createdAt), fromUnixMillis(updatedAt)
	r.RetainedArtifactID = task.RetainedArtifactID(retainedArtifactID.String)
	r.ArtifactExportID = task.ArtifactExportID(artifactExportID.String)
	r.MaterializationID = task.MaterializationID(materializationID.String)
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
