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

	"github.com/nebler/fern/internal/task"
)

func (s *Store) CommitBackgroundRunRetainedResult(ctx context.Context, p CommitBackgroundRunRetainedResultParams) (_ BackgroundRunRetainedResult, err error) {
	if err := validateRetainedResultCommit(p); err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	ctx = context.WithoutCancel(ctx)
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	defer release()
	defer rollback(tx, &err)

	if existing, getErr := getResult(ctx, tx, p.ResultID); getErr == nil {
		value, replayErr := retainedResultReplay(ctx, tx, existing, p)
		if replayErr != nil {
			return BackgroundRunRetainedResult{}, replayErr
		}
		if err := tx.Commit(); err != nil {
			return BackgroundRunRetainedResult{}, err
		}
		value.Replayed = true
		return value, nil
	} else if !errors.Is(getErr, ErrNotFound) {
		return BackgroundRunRetainedResult{}, getErr
	}

	export, err := getBackgroundRunExport(ctx, tx, p.ExportID)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	request, err := getBackgroundRunSealRequest(ctx, tx, export.SealRequestID)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	materialization, err := getArtifactMaterialization(ctx, tx, p.MaterializationID)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	fence, err := getWriterFence(ctx, tx, request.ID)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	run, err := readBackgroundRunExact(ctx, tx, export.WorkspaceID, export.TaskID)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	owner, err := getTask(ctx, tx, export.TaskID)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	attempt, err := getAttempt(ctx, tx, export.AttemptID)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	if export.TaskID != p.TaskID || export.AttemptID != p.AttemptID || export.Generation != p.Generation || export.Revision != p.ExpectedRevision ||
		export.Phase != BackgroundRunExportPhaseMaterialized || export.State != BackgroundRunExportRunning || export.ClaimOwner != p.ClaimOwner ||
		export.ClaimGeneration != p.ClaimGeneration || export.ClaimExpiresAt == nil || !export.ClaimExpiresAt.After(p.Now) ||
		export.ArtifactID != p.ArtifactID || export.MaterializationID != p.MaterializationID || export.ResultID != p.ResultID ||
		request.ArtifactID != p.ArtifactID || request.MaterializationID != p.MaterializationID || request.ResultID != p.ResultID ||
		request.ResultEventID != p.ResultEventID || request.TaskEventID != p.TaskEventID || request.ExportID != p.ExportID ||
		materialization.State != ArtifactMaterializationReady || materialization.ExportID != p.ExportID || materialization.ArtifactID != p.ArtifactID ||
		materialization.ResultID != p.ResultID || materialization.ResultCommit != export.ResultCommit || materialization.TreeOID != export.TreeOID ||
		fence.ExportID != p.ExportID || fence.TaskID != p.TaskID || fence.AttemptID != p.AttemptID || fence.Generation != p.Generation ||
		run.ResultAuthorityPhase != "exporting" || run.EffectPhase != BackgroundRunEffectExporting || run.BackgroundSealRequestID != request.ID ||
		run.ArtifactExportID != p.ExportID || run.RetainedArtifactID != p.ArtifactID || run.MaterializationID != p.MaterializationID ||
		run.RetainedResultID != p.ResultID || run.CancelEpoch != 0 || owner.Revision != request.ExpectedTaskRevision ||
		attempt.Revision != request.ExpectedAttemptRevision || owner.State != task.TaskQueued || owner.CancelEpoch != 0 || owner.SealedResultID != "" ||
		owner.CurrentAttemptID != attempt.ID || attempt.State != task.AttemptPrepared || attempt.SealedResultID != "" {
		return BackgroundRunRetainedResult{}, ErrInvalidState
	}
	if export.Outcome == task.ResultNoChanges && (export.ResultCommit != export.BaseSHA || len(export.ResultManifest) != 0) {
		return BackgroundRunRetainedResult{}, ErrInvalidState
	}
	if export.Outcome == task.ResultChanged && (export.ResultCommit == export.BaseSHA || len(export.ResultManifest) == 0) {
		return BackgroundRunRetainedResult{}, ErrInvalidState
	}
	if export.CollectedAt == nil || p.SealedAt.Before(*export.CollectedAt) || p.SealedAt.Before(request.AcceptedAt) {
		return BackgroundRunRetainedResult{}, ErrInvalidInput
	}

	sealParams := SealResultParams{ResultID: p.ResultID, TaskID: p.TaskID, AttemptID: p.AttemptID,
		ExpectedAttemptRevision: request.ExpectedAttemptRevision, ExpectedTaskRevision: request.ExpectedTaskRevision,
		ResultEventID: p.ResultEventID, TaskEventID: p.TaskEventID, RepositoryID: export.RepositoryID, BaseSHA: export.BaseSHA,
		ResultCommit: export.ResultCommit, TreeOID: export.TreeOID, Outcome: export.Outcome, WorktreeClean: true,
		Manifest: export.ResultManifest, ManifestSHA256: export.ChangesSHA256, OpenCodeSessionID: run.OpenCodeSessionID,
		OpenCodeMessageID: run.OpenCodeMessageID, EvidencePayload: p.EvidencePayload, EvidenceSHA256: p.EvidenceSHA256,
		PolicyVersion: request.PolicyVersion, CollectedAt: *export.CollectedAt, SealedAt: p.SealedAt, Actor: p.Actor,
		CompletionAuthority: SealAuthorityUser}
	// Persistent results hash the relational ManifestEntry JSON. Retained
	// results instead preserve taskartifact's canonical ChangeEntry digest as
	// the cross-layer authority, while validating the relational projection
	// independently with the existing closed-schema validator.
	validationParams := sealParams
	encodedProjection, err := json.Marshal(sealParams.Manifest)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	validationParams.ManifestSHA256 = sha256.Sum256(encodedProjection)
	manifest, err := validateResultMaterial(validationParams)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	if sealParams.ManifestSHA256 == ([32]byte{}) {
		return BackgroundRunRetainedResult{}, fmt.Errorf("%w: retained changes digest", ErrInvalidInput)
	}
	sealParams.Manifest = manifest
	payload, err := resultSealPayload(sealParams)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	var payloadObject map[string]any
	if json.Unmarshal(payload, &payloadObject) != nil {
		return BackgroundRunRetainedResult{}, ErrCorruptStore
	}
	payloadObject["sourceKind"] = string(ResultSourceRetainedArtifact)
	payloadObject["sealRequestId"] = request.ID
	payloadObject["artifactExportId"] = export.ID
	payloadObject["retainedArtifactId"] = export.ArtifactID
	payloadObject["materializationId"] = export.MaterializationID
	payloadObject["commitEpochSeconds"] = request.CommitEpochSeconds
	payload, err = json.Marshal(payloadObject)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	actorID, err := ensureActor(ctx, tx, p.Actor)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	sealedMS := unixMillis(p.SealedAt)
	resultEvent, err := insertAttemptEvent(ctx, tx, p.ResultEventID, attempt, "attempt.result_sealed", sealedMS, actorID, payload)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	taskEvent, err := insertTaskEvent(ctx, tx, p.TaskEventID, owner, "task.completed", sealedMS, actorID, payload)
	if err != nil || resultEvent.Cursor >= taskEvent.Cursor {
		return BackgroundRunRetainedResult{}, fmt.Errorf("insert retained result events: %w", err)
	}
	for index, entry := range export.ResultManifest {
		if _, err := tx.ExecContext(ctx, `INSERT INTO result_manifest(
result_id,ordinal,path_base64,change_kind,old_mode,new_mode,old_blob_oid,new_blob_oid,old_size,new_size)
VALUES(?,?,?,?,?,?,?,?,?,?)`, p.ResultID, index, entry.PathBase64, entry.ChangeKind, entry.OldMode, entry.NewMode,
			entry.OldBlobOID, entry.NewBlobOID, entry.OldSize, entry.NewSize); err != nil {
			return BackgroundRunRetainedResult{}, fmt.Errorf("insert retained result manifest: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO retained_artifacts(
id,seal_request_id,export_id,materialization_id,result_id,workspace_id,task_id,attempt_id,generation,manifest_json,
manifest_sha256,changes_sha256,cas_locator,bundle_sha256,bundle_size,base_sha,result_commit,tree_oid,opencode_session_id,opencode_message_id,committed_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,

		p.ArtifactID, request.ID, p.ExportID, p.MaterializationID, p.ResultID, export.WorkspaceID, p.TaskID, p.AttemptID,
		p.Generation, string(export.ArtifactManifest), export.ArtifactManifestSHA256[:], export.ChangesSHA256[:], export.CASLocator, export.BundleSHA256[:], export.BundleBytes,
		export.BaseSHA, export.ResultCommit, export.TreeOID, export.OpenCodeSessionID, export.OpenCodeMessageID, sealedMS); err != nil {
		return BackgroundRunRetainedResult{}, fmt.Errorf("insert retained artifact: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO results(
id,task_id,attempt_id,workspace_id,state,outcome,repository_id,base_sha,result_commit,tree_oid,worktree_clean,
manifest_entries,manifest_sha256,opencode_session_id,opencode_message_id,evidence_sha256,policy_version,collected_at,sealed_at,
creator_actor_snapshot_id,sealed_event_id,completed_event_id,revision,created_at,updated_at,completion_authority,
source_kind,retained_artifact_id,artifact_export_id,materialization_id)
VALUES(?,?,?,?,'sealed',?,?,?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,1,?,?,'user_seal','retained_artifact',?,?,?)`,
		p.ResultID, p.TaskID, p.AttemptID, export.WorkspaceID, export.Outcome, export.RepositoryID, export.BaseSHA,
		export.ResultCommit, export.TreeOID, len(export.ResultManifest), export.ChangesSHA256[:], run.OpenCodeSessionID,
		run.OpenCodeMessageID, p.EvidenceSHA256[:], request.PolicyVersion, unixMillis(*export.CollectedAt), sealedMS, actorID,
		p.ResultEventID, p.TaskEventID, sealedMS, sealedMS, p.ArtifactID, p.ExportID, p.MaterializationID); err != nil {
		return BackgroundRunRetainedResult{}, fmt.Errorf("insert retained result: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE attempts SET state='superseded',sealed_result_id=?,revision=revision+1,updated_at=?
WHERE id=? AND task_id=? AND workspace_id=? AND state='prepared' AND sealed_result_id IS NULL AND revision=?`,
		p.ResultID, sealedMS, p.AttemptID, p.TaskID, export.WorkspaceID, request.ExpectedAttemptRevision)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunRetainedResult{}, ErrInvalidState
	}
	result, err = tx.ExecContext(ctx, `UPDATE tasks SET state='completed',sealed_result_id=?,latest_event_cursor=?,revision=revision+1,updated_at=?
WHERE id=? AND workspace_id=? AND state='queued' AND cancel_epoch=0 AND current_attempt_id=? AND sealed_result_id IS NULL AND revision=?`,
		p.ResultID, taskEvent.Cursor, sealedMS, p.TaskID, export.WorkspaceID, p.AttemptID, request.ExpectedTaskRevision)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunRetainedResult{}, ErrInvalidState
	}
	result, err = tx.ExecContext(ctx, `UPDATE background_runs SET state='result_ready',result_authority_phase='artifact_committed',
claim_owner=NULL,claim_expires_at=NULL,revision=revision+1,updated_at=? WHERE task_id=? AND attempt_id=? AND generation=? AND
state='cleanup_required' AND effect_phase='writer_inactive' AND result_authority_phase='exporting' AND artifact_export_id=? AND retained_artifact_id=?`,
		sealedMS, p.TaskID, p.AttemptID, p.Generation, p.ExportID, p.ArtifactID)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunRetainedResult{}, ErrInvalidState
	}
	result, err = tx.ExecContext(ctx, `UPDATE background_run_exports SET state='completed',phase='completed',claim_owner=NULL,
claim_expires_at=NULL,revision=revision+1,updated_at=? WHERE id=? AND revision=? AND phase='materialized' AND state='running' AND
claim_owner=? AND claim_generation=? AND claim_expires_at>?`, sealedMS, p.ExportID, p.ExpectedRevision, p.ClaimOwner, p.ClaimGeneration, unixMillis(p.Now))
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunRetainedResult{}, ErrLeaseConflict
	}

	storedResult, err := getResult(ctx, tx, p.ResultID)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	storedTask, _ := getTask(ctx, tx, p.TaskID)
	storedAttempt, _ := getAttempt(ctx, tx, p.AttemptID)
	storedRun, _ := readBackgroundRunExact(ctx, tx, export.WorkspaceID, p.TaskID)
	storedExport, _ := getBackgroundRunExport(ctx, tx, p.ExportID)
	artifact, _ := getRetainedArtifact(ctx, tx, p.ArtifactID)
	if err := tx.Commit(); err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	return BackgroundRunRetainedResult{SealedResult: SealedResult{Result: storedResult, Manifest: export.ResultManifest,
		Task: storedTask, Attempt: storedAttempt, ResultEvent: resultEvent, TaskEvent: taskEvent}, Run: storedRun,
		SealRequest: request, Export: storedExport, Artifact: artifact, Materialization: materialization}, nil
}

func validateRetainedResultCommit(p CommitBackgroundRunRetainedResultParams) error {
	if err := validateExportClaim(p.BackgroundRunExportClaim); err != nil || p.ExpectedPhase != BackgroundRunExportPhaseMaterialized {
		return fmt.Errorf("%w: retained result export claim", ErrInvalidInput)
	}
	if _, err := task.ParseMaterializationID(string(p.MaterializationID)); err != nil {
		return fmt.Errorf("%w: materialization", ErrInvalidInput)
	}
	if _, err := task.ParseRetainedArtifactID(string(p.ArtifactID)); err != nil {
		return fmt.Errorf("%w: retained artifact", ErrInvalidInput)
	}
	if _, err := task.ParseResultID(string(p.ResultID)); err != nil {
		return fmt.Errorf("%w: retained result", ErrInvalidInput)
	}
	if _, err := task.ParseEventID(string(p.ResultEventID)); err != nil {
		return fmt.Errorf("%w: result event", ErrInvalidInput)
	}
	if _, err := task.ParseEventID(string(p.TaskEventID)); err != nil || p.TaskEventID == p.ResultEventID {
		return fmt.Errorf("%w: task event", ErrInvalidInput)
	}
	if p.Actor.Validate() != nil || (p.Actor.Type != task.ActorSystem && p.Actor.Type != task.ActorRecovery) || validExactTimestamp(p.SealedAt) != nil {
		return fmt.Errorf("%w: retained result actor or time", ErrInvalidInput)
	}
	return validateDeliveryEvidence(p.EvidencePayload, p.EvidenceSHA256)
}

func (s *Store) GetRetainedArtifact(ctx context.Context, id task.RetainedArtifactID) (RetainedArtifact, error) {
	if _, err := task.ParseRetainedArtifactID(string(id)); err != nil {
		return RetainedArtifact{}, fmt.Errorf("%w: retained artifact", ErrInvalidInput)
	}
	return getRetainedArtifact(ctx, s.db, id)
}

func getRetainedArtifact(ctx context.Context, q queryRower, id task.RetainedArtifactID) (RetainedArtifact, error) {
	var value RetainedArtifact
	var manifest string
	var manifestHash, changesHash, bundleHash []byte
	var casLocator string
	var committedAt int64
	err := q.QueryRowContext(ctx, `SELECT id,seal_request_id,export_id,materialization_id,result_id,workspace_id,task_id,attempt_id,
generation,manifest_json,manifest_sha256,changes_sha256,cas_locator,bundle_sha256,bundle_size,base_sha,result_commit,tree_oid,
opencode_session_id,opencode_message_id,committed_at FROM retained_artifacts WHERE id=?`, id).
		Scan(&value.ID, &value.SealRequestID, &value.ExportID, &value.MaterializationID, &value.ResultID, &value.WorkspaceID,
			&value.TaskID, &value.AttemptID, &value.Generation, &manifest, &manifestHash, &changesHash, &casLocator, &bundleHash, &value.BundleBytes,
			&value.BaseSHA, &value.ResultCommit, &value.TreeOID, &value.OpenCodeSessionID, &value.OpenCodeMessageID, &committedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RetainedArtifact{}, ErrNotFound
	}
	if err != nil {
		return RetainedArtifact{}, fmt.Errorf("read retained artifact: %w", err)
	}
	value.Manifest = json.RawMessage(manifest)
	if len(manifestHash) != 32 || len(changesHash) != 32 || len(bundleHash) != 32 || !safeArtifactManifest(value.Manifest) {
		return RetainedArtifact{}, ErrCorruptStore
	}
	copy(value.ManifestSHA256[:], manifestHash)
	copy(value.ChangesSHA256[:], changesHash)
	copy(value.BundleSHA256[:], bundleHash)
	value.CASLocator = casLocator
	if value.CASLocator != "sha256:"+hex.EncodeToString(value.ManifestSHA256[:]) {
		return RetainedArtifact{}, ErrCorruptStore
	}
	value.CommittedAt = fromUnixMillis(committedAt)
	return value, nil
}

func (s *Store) ReferencedArtifactManifestSHA256(ctx context.Context) ([][32]byte, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT manifest_sha256 FROM retained_artifacts ORDER BY manifest_sha256`)
	if err != nil {
		return nil, fmt.Errorf("list referenced artifact manifests: %w", err)
	}
	defer rows.Close()
	values := make([][32]byte, 0)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil || len(raw) != 32 {
			return nil, ErrCorruptStore
		}
		var value [32]byte
		copy(value[:], raw)
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func (s *Store) ListReferencedArtifactManifestSHA256(ctx context.Context) ([][32]byte, error) {
	return s.ReferencedArtifactManifestSHA256(ctx)
}

func retainedResultReplay(ctx context.Context, tx *sql.Tx, result Result, p CommitBackgroundRunRetainedResultParams) (BackgroundRunRetainedResult, error) {
	if result.SourceKind != ResultSourceRetainedArtifact || result.ID != p.ResultID || result.TaskID != p.TaskID || result.AttemptID != p.AttemptID ||
		result.RetainedArtifactID != p.ArtifactID || result.ArtifactExportID != p.ExportID || result.MaterializationID != p.MaterializationID ||
		result.SealedEventID != p.ResultEventID || result.CompletedEventID != p.TaskEventID || result.EvidenceSHA256 != p.EvidenceSHA256 ||
		result.Creator != p.Actor || !result.SealedAt.Equal(p.SealedAt) {
		return BackgroundRunRetainedResult{}, fmt.Errorf("%w: retained result replay differs", ErrInvalidState)
	}
	artifact, err := getRetainedArtifact(ctx, tx, p.ArtifactID)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	export, err := getBackgroundRunExport(ctx, tx, p.ExportID)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	request, err := getBackgroundRunSealRequest(ctx, tx, export.SealRequestID)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	materialization, err := getArtifactMaterialization(ctx, tx, p.MaterializationID)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	run, err := readBackgroundRunExact(ctx, tx, export.WorkspaceID, p.TaskID)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	owner, _ := getTask(ctx, tx, p.TaskID)
	attempt, _ := getAttempt(ctx, tx, p.AttemptID)
	manifest, _ := getResultManifest(ctx, tx, p.ResultID)
	resultEvent, _ := scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.id=?`, p.ResultEventID))
	taskEvent, _ := scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.id=?`, p.TaskEventID))
	if export.State != BackgroundRunExportCompleted || export.Phase != BackgroundRunExportPhaseCompleted ||
		run.EffectPhase != BackgroundRunEffectArtifactCommitted || owner.SealedResultID != p.ResultID || attempt.SealedResultID != p.ResultID ||
		!bytes.Equal(artifact.Manifest, export.ArtifactManifest) {
		return BackgroundRunRetainedResult{}, ErrCorruptStore
	}
	return BackgroundRunRetainedResult{SealedResult: SealedResult{Result: result, Manifest: manifest, Task: owner, Attempt: attempt,
		ResultEvent: resultEvent, TaskEvent: taskEvent}, Run: run, SealRequest: request, Export: export, Artifact: artifact,
		Materialization: materialization}, nil
}
