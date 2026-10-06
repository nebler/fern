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

	"github.com/nebler/fern/internal/task"
)

const backgroundRunExportSelect = `SELECT id,seal_request_id,workspace_id,task_id,attempt_id,generation,artifact_id,
materialization_id,result_id,state,phase,repository_id,base_sha,opencode_session_id,opencode_message_id,
result_commit,tree_oid,outcome,result_manifest_json,result_manifest_entries,result_manifest_sha256,artifact_manifest_json,
artifact_manifest_sha256,cas_locator,bundle_sha256,bundle_size,collected_at,recovery_reason,revision,created_at,updated_at
FROM background_run_exports`

func (s *Store) GetBackgroundRunExport(ctx context.Context, id task.ArtifactExportID) (BackgroundRunExport, error) {
	if _, err := task.ParseArtifactExportID(string(id)); err != nil {
		return BackgroundRunExport{}, fmt.Errorf("%w: background export", ErrInvalidInput)
	}
	return getBackgroundRunExport(ctx, s.db, id)
}

func getBackgroundRunExport(ctx context.Context, q queryRower, id task.ArtifactExportID) (BackgroundRunExport, error) {
	return scanBackgroundRunExport(q.QueryRowContext(ctx, backgroundRunExportSelect+` WHERE id=?`, id))
}

func scanBackgroundRunExport(row rowScanner) (BackgroundRunExport, error) {
	var value BackgroundRunExport
	var resultCommit, treeOID, outcome, resultManifestJSON, artifactManifestJSON, casLocator, recoveryReason sql.NullString
	var resultEntries, bundleSize, collectedAt sql.NullInt64
	var resultManifestHash, artifactManifestHash, bundleHash []byte
	var repositoryID, createdAt, updatedAt int64
	err := row.Scan(&value.ID, &value.SealRequestID, &value.WorkspaceID, &value.TaskID, &value.AttemptID, &value.Generation,
		&value.ArtifactID, &value.MaterializationID, &value.ResultID, &value.State, &value.Phase,
		&repositoryID, &value.BaseSHA, &value.OpenCodeSessionID, &value.OpenCodeMessageID, &resultCommit, &treeOID, &outcome, &resultManifestJSON,
		&resultEntries, &resultManifestHash, &artifactManifestJSON, &artifactManifestHash, &casLocator, &bundleHash, &bundleSize,
		&collectedAt, &recoveryReason, &value.Revision, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRunExport{}, ErrNotFound
	}
	if err != nil {
		return BackgroundRunExport{}, fmt.Errorf("read background export: %w", err)
	}
	if repositoryID <= 0 || value.Generation <= 0 || value.Revision <= 0 {
		return BackgroundRunExport{}, ErrCorruptStore
	}
	value.RepositoryID = task.RepositoryID(repositoryID)
	value.ResultCommit, value.TreeOID, value.Outcome = task.GitOID(nullableText(resultCommit)), task.GitOID(nullableText(treeOID)), task.ResultOutcome(nullableText(outcome))
	value.RecoveryReason = nullableText(recoveryReason)
	value.BundleBytes = bundleSize.Int64
	value.CollectedAt = nullableTime(collectedAt)
	value.CreatedAt, value.UpdatedAt = fromUnixMillis(createdAt), fromUnixMillis(updatedAt)
	if resultManifestJSON.Valid {
		if err := json.Unmarshal([]byte(resultManifestJSON.String), &value.ResultManifest); err != nil || len(resultManifestHash) != 32 || int64(len(value.ResultManifest)) != resultEntries.Int64 {
			return BackgroundRunExport{}, ErrCorruptStore
		}
		copy(value.ChangesSHA256[:], resultManifestHash)
	}
	if artifactManifestJSON.Valid {
		if len(artifactManifestHash) != 32 {
			return BackgroundRunExport{}, ErrCorruptStore
		}
		value.ArtifactManifest = json.RawMessage(artifactManifestJSON.String)
		copy(value.ArtifactManifestSHA256[:], artifactManifestHash)
		value.CASLocator = nullableText(casLocator)
		if value.CASLocator != "sha256:"+hex.EncodeToString(value.ArtifactManifestSHA256[:]) {
			return BackgroundRunExport{}, ErrCorruptStore
		}
	}
	if bundleHash != nil {
		if len(bundleHash) != 32 || !bundleSize.Valid {
			return BackgroundRunExport{}, ErrCorruptStore
		}
		copy(value.BundleSHA256[:], bundleHash)
	}
	return value, nil
}

// StartBackgroundRunExport moves a prepared or recovery-required export to
// running and projects the run into exporting. Restarting a running export is
// allowed: the coordinator replays durable phases after a crash.
func (s *Store) StartBackgroundRunExport(ctx context.Context, ref BackgroundRunExportRef) (_ BackgroundRunExport, err error) {
	if err := validateExportRef(ref); err != nil {
		return BackgroundRunExport{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRunExport{}, err
	}
	defer release()
	defer rollback(tx, &err)
	now := unixMillis(ref.Now)
	result, err := tx.ExecContext(ctx, `UPDATE background_run_exports SET state='running',recovery_reason=NULL,revision=revision+1,updated_at=?
WHERE id=? AND task_id=? AND attempt_id=? AND generation=? AND revision=? AND phase=? AND state IN ('prepared','running','recovery_required')`,
		now, ref.ExportID, ref.TaskID, ref.AttemptID, ref.Generation, ref.ExpectedRevision, ref.ExpectedPhase)
	if err != nil {
		return BackgroundRunExport{}, fmt.Errorf("start background export: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunExport{}, ErrInvalidState
	}
	export, err := getBackgroundRunExport(ctx, tx, ref.ExportID)
	if err != nil {
		return BackgroundRunExport{}, err
	}
	projection, err := tx.ExecContext(ctx, `UPDATE background_runs SET result_authority_phase='exporting',revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND generation=? AND artifact_export_id=? AND result_authority_phase IN ('writer_inactive','exporting')`,
		now, ref.TaskID, ref.AttemptID, ref.Generation, ref.ExportID)
	if err != nil {
		return BackgroundRunExport{}, fmt.Errorf("project background export start: %w", err)
	}
	if changed, changeErr := projection.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunExport{}, ErrInvalidState
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRunExport{}, err
	}
	return export, nil
}

func (s *Store) RecordBackgroundRunSnapshotStarted(ctx context.Context, ref BackgroundRunExportRef) (BackgroundRunExport, error) {
	return s.advanceBackgroundExport(ctx, ref, BackgroundRunExportPhasePrepared, BackgroundRunExportPhaseSnapshotStarted, "", nil, nil)
}

func (s *Store) SelectBackgroundRunSnapshot(ctx context.Context, p SelectBackgroundRunSnapshotParams) (BackgroundRunExport, error) {
	manifest, err := validateManifest(p.ResultManifest)
	if err != nil {
		return BackgroundRunExport{}, err
	}
	encodedManifest, err := json.Marshal(manifest)
	if err != nil {
		return BackgroundRunExport{}, err
	}
	// Retained changes use taskartifact's nested canonical ChangeEntry encoding;
	// ResultManifest is its lossless relational projection, not a second digest
	// authority with a different JSON shape.
	if p.ChangesSHA256 == ([32]byte{}) || sha256.Sum256(p.ArtifactManifest) != p.ArtifactManifestSHA256 ||
		!safeArtifactManifest(p.ArtifactManifest) || validExactTimestamp(p.CollectedAt) != nil {
		return BackgroundRunExport{}, fmt.Errorf("%w: selected background snapshot", ErrInvalidInput)
	}
	if _, err := task.ParseGitOID(string(p.ResultCommit)); err != nil {
		return BackgroundRunExport{}, fmt.Errorf("%w: result commit", ErrInvalidInput)
	}
	if _, err := task.ParseGitOID(string(p.TreeOID)); err != nil {
		return BackgroundRunExport{}, fmt.Errorf("%w: result tree", ErrInvalidInput)
	}
	if _, err := task.ParseOpenCodeSessionID(string(p.OpenCodeSessionID)); err != nil {
		return BackgroundRunExport{}, fmt.Errorf("%w: snapshot OpenCode session", ErrInvalidInput)
	}
	if _, err := task.ParseOpenCodeMessageID(string(p.OpenCodeMessageID)); err != nil {
		return BackgroundRunExport{}, fmt.Errorf("%w: snapshot OpenCode message", ErrInvalidInput)
	}
	if (p.Outcome != task.ResultChanged && p.Outcome != task.ResultNoChanges) || (p.Outcome == task.ResultChanged && len(manifest) == 0) ||
		(p.Outcome == task.ResultNoChanges && len(manifest) != 0) || p.ResultCommit == "" {
		return BackgroundRunExport{}, fmt.Errorf("%w: result tuple", ErrInvalidInput)
	}
	p.ResultManifest = manifest
	replay := func(value BackgroundRunExport) bool {
		requested, _ := json.Marshal(p.ResultManifest)
		stored, _ := json.Marshal(value.ResultManifest)
		return value.ResultCommit == p.ResultCommit && value.TreeOID == p.TreeOID && value.Outcome == p.Outcome &&
			value.OpenCodeSessionID == p.OpenCodeSessionID && value.OpenCodeMessageID == p.OpenCodeMessageID &&
			value.ChangesSHA256 == p.ChangesSHA256 && bytes.Equal(stored, requested) &&
			value.ArtifactManifestSHA256 == p.ArtifactManifestSHA256 && bytes.Equal(value.ArtifactManifest, p.ArtifactManifest) &&
			value.CollectedAt != nil && value.CollectedAt.Equal(p.CollectedAt)
	}
	return s.advanceBackgroundExport(ctx, p.BackgroundRunExportRef, BackgroundRunExportPhaseSnapshotStarted,
		BackgroundRunExportPhaseSnapshotSelected,
		`result_commit=?,tree_oid=?,outcome=?,result_manifest_json=?,result_manifest_entries=?,result_manifest_sha256=?,
artifact_manifest_json=?,artifact_manifest_sha256=?,cas_locator=?,opencode_session_id=?,opencode_message_id=?,collected_at=?`,
		[]any{p.ResultCommit, p.TreeOID, p.Outcome, string(encodedManifest), len(manifest), p.ChangesSHA256[:],
			string(p.ArtifactManifest), p.ArtifactManifestSHA256[:], "sha256:" + hex.EncodeToString(p.ArtifactManifestSHA256[:]),
			p.OpenCodeSessionID, p.OpenCodeMessageID, unixMillis(p.CollectedAt)}, replay)
}

func (s *Store) RecordBackgroundRunBundleWriteStarted(ctx context.Context, ref BackgroundRunExportRef) (BackgroundRunExport, error) {
	return s.advanceBackgroundExport(ctx, ref, BackgroundRunExportPhaseSnapshotSelected, BackgroundRunExportPhaseBundleWriteStarted, "", nil, nil)
}

// RecordBackgroundRunBundleVerified commits the caller's verified digest/size
// evidence at the export revision. It does not read or verify bundle bytes.
func (s *Store) RecordBackgroundRunBundleVerified(ctx context.Context, p RecordBackgroundRunBundleVerifiedParams) (BackgroundRunExport, error) {
	if p.BundleSHA256 == ([32]byte{}) || p.BundleBytes < 0 {
		return BackgroundRunExport{}, fmt.Errorf("%w: verified bundle", ErrInvalidInput)
	}
	replay := func(value BackgroundRunExport) bool {
		return value.BundleSHA256 == p.BundleSHA256 && value.BundleBytes == p.BundleBytes
	}
	return s.advanceBackgroundExport(ctx, p.BackgroundRunExportRef, BackgroundRunExportPhaseBundleWriteStarted,
		BackgroundRunExportPhaseBundleVerified, `bundle_sha256=?,bundle_size=?`, []any{p.BundleSHA256[:], p.BundleBytes}, replay)
}

func (s *Store) RecordBackgroundRunCASInstallStarted(ctx context.Context, ref BackgroundRunExportRef) (BackgroundRunExport, error) {
	return s.advanceBackgroundExport(ctx, ref, BackgroundRunExportPhaseBundleVerified, BackgroundRunExportPhaseCASInstallStarted, "", nil, nil)
}

func (s *Store) RecordBackgroundRunCASInstalled(ctx context.Context, ref BackgroundRunExportRef) (BackgroundRunExport, error) {
	return s.advanceBackgroundExport(ctx, ref, BackgroundRunExportPhaseCASInstallStarted, BackgroundRunExportPhaseCASInstalled, "", nil, nil)
}

func (s *Store) RecordBackgroundRunMaterializeStarted(ctx context.Context, ref BackgroundRunExportRef) (BackgroundRunExport, error) {
	return s.advanceBackgroundExport(ctx, ref, BackgroundRunExportPhaseCASInstalled, BackgroundRunExportPhaseMaterializeStarted, "", nil, nil)
}

func (s *Store) MarkBackgroundRunExportRecoveryRequired(ctx context.Context, ref BackgroundRunExportRef, reason string) (BackgroundRunExport, error) {
	if !validBoundedText(reason, 1, 1000) {
		return BackgroundRunExport{}, fmt.Errorf("%w: export recovery reason", ErrInvalidInput)
	}
	replay := func(value BackgroundRunExport) bool {
		return value.State == BackgroundRunExportRecoveryRequired && value.RecoveryReason == reason
	}
	return s.updateBackgroundExport(ctx, ref, ref.ExpectedPhase, `state='recovery_required',recovery_reason=?`, []any{reason}, replay)
}

func (s *Store) advanceBackgroundExport(ctx context.Context, ref BackgroundRunExportRef, from, to BackgroundRunExportPhase,
	assignments string, args []any, replay func(BackgroundRunExport) bool) (BackgroundRunExport, error) {
	if ref.ExpectedPhase != from {
		return BackgroundRunExport{}, fmt.Errorf("%w: background export phase", ErrInvalidInput)
	}
	return s.updateBackgroundExport(ctx, ref, to, assignments, args, replay)
}

func (s *Store) updateBackgroundExport(ctx context.Context, ref BackgroundRunExportRef, to BackgroundRunExportPhase,
	assignments string, args []any, replay func(BackgroundRunExport) bool) (_ BackgroundRunExport, err error) {
	if err := validateExportRef(ref); err != nil {
		return BackgroundRunExport{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRunExport{}, err
	}
	defer release()
	defer rollback(tx, &err)
	current, err := getBackgroundRunExport(ctx, tx, ref.ExportID)
	if err != nil {
		return BackgroundRunExport{}, err
	}
	if current.TaskID == ref.TaskID && current.AttemptID == ref.AttemptID && current.Generation == ref.Generation &&
		current.Phase == to && current.Revision == ref.ExpectedRevision+1 && (replay == nil || replay(current)) {
		if err := tx.Commit(); err != nil {
			return BackgroundRunExport{}, err
		}
		return current, nil
	}
	set := `phase=?,`
	values := []any{to}
	if assignments != "" {
		set += assignments + `,`
		values = append(values, args...)
	}
	values = append(values, unixMillis(ref.Now), ref.ExportID, ref.TaskID, ref.AttemptID, ref.Generation,
		ref.ExpectedRevision, ref.ExpectedPhase)
	result, err := tx.ExecContext(ctx, `UPDATE background_run_exports SET `+set+`revision=revision+1,updated_at=?
WHERE id=? AND task_id=? AND attempt_id=? AND generation=? AND revision=? AND phase=? AND state='running'`, values...)
	if err != nil {
		return BackgroundRunExport{}, fmt.Errorf("advance background export: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunExport{}, ErrInvalidState
	}
	stored, err := getBackgroundRunExport(ctx, tx, ref.ExportID)
	if err != nil {
		return BackgroundRunExport{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRunExport{}, err
	}
	return stored, nil
}

func validateExportRef(p BackgroundRunExportRef) error {
	if _, err := task.ParseArtifactExportID(string(p.ExportID)); err != nil {
		return fmt.Errorf("%w: export ID", ErrInvalidInput)
	}
	if _, err := task.ParseTaskID(string(p.TaskID)); err != nil {
		return fmt.Errorf("%w: export task", ErrInvalidInput)
	}
	if _, err := task.ParseAttemptID(string(p.AttemptID)); err != nil || p.Generation <= 0 || p.ExpectedRevision <= 0 ||
		validExactTimestamp(p.Now) != nil {
		return fmt.Errorf("%w: export revision", ErrInvalidInput)
	}
	return nil
}

func safeArtifactManifest(value json.RawMessage) bool {
	if len(value) < 2 || len(value) > 4*1024*1024 || !json.Valid(value) || value[0] != '{' || sha256.Sum256(value) == ([32]byte{}) {
		return false
	}
	var decoded any
	if json.Unmarshal(value, &decoded) != nil {
		return false
	}
	forbidden := map[string]bool{"host_path": true, "remote_url": true, "prompt": true, "environment": true,
		"credential": true, "credentials": true, "cookie": true, "cookies": true, "authorization": true, "actor_auth": true,
		"opencode_output": true, "raw_output": true}
	var inspect func(any) bool
	inspect = func(node any) bool {
		switch typed := node.(type) {
		case map[string]any:
			for key, child := range typed {
				if forbidden[strings.ToLower(key)] || !inspect(child) {
					return false
				}
			}
		case []any:
			for _, child := range typed {
				if !inspect(child) {
					return false
				}
			}
		}
		return true
	}
	return inspect(decoded)
}
