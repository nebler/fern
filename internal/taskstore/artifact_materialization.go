package taskstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

func (s *Store) GetArtifactMaterialization(ctx context.Context, id task.MaterializationID) (ArtifactMaterialization, error) {
	if _, err := task.ParseMaterializationID(string(id)); err != nil {
		return ArtifactMaterialization{}, fmt.Errorf("%w: materialization", ErrInvalidInput)
	}
	return getArtifactMaterialization(ctx, s.db, id)
}

func getArtifactMaterialization(ctx context.Context, q queryRower, id task.MaterializationID) (ArtifactMaterialization, error) {
	var value ArtifactMaterialization
	var resultCommit, treeOID, recoveryReason sql.NullString
	var proof []byte
	var createdAt, updatedAt int64
	err := q.QueryRowContext(ctx, `SELECT id,seal_request_id,export_id,artifact_id,result_id,state,result_commit,tree_oid,
proof_sha256,recovery_reason,revision,created_at,updated_at FROM artifact_materializations WHERE id=?`, id).
		Scan(&value.ID, &value.SealRequestID, &value.ExportID, &value.ArtifactID, &value.ResultID, &value.State,
			&resultCommit, &treeOID, &proof, &recoveryReason, &value.Revision, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactMaterialization{}, ErrNotFound
	}
	if err != nil {
		return ArtifactMaterialization{}, fmt.Errorf("read artifact materialization: %w", err)
	}
	value.ResultCommit, value.TreeOID = task.GitOID(nullableText(resultCommit)), task.GitOID(nullableText(treeOID))
	value.RecoveryReason = nullableText(recoveryReason)
	value.CreatedAt, value.UpdatedAt = fromUnixMillis(createdAt), fromUnixMillis(updatedAt)
	if value.State == ArtifactMaterializationReady {
		if len(proof) != 32 {
			return ArtifactMaterialization{}, ErrCorruptStore
		}
		copy(value.ProofSHA256[:], proof)
	}
	return value, nil
}

func (s *Store) RecordArtifactMaterializationReady(ctx context.Context, p RecordArtifactMaterializationReadyParams) (_ BackgroundRunExport, err error) {
	if err := validateExportRef(p.BackgroundRunExportRef); err != nil || p.ExpectedPhase != BackgroundRunExportPhaseMaterializeStarted ||
		p.ProofSHA256 == ([32]byte{}) {
		return BackgroundRunExport{}, fmt.Errorf("%w: artifact materialization proof", ErrInvalidInput)
	}
	if _, parseErr := task.ParseMaterializationID(string(p.MaterializationID)); parseErr != nil {
		return BackgroundRunExport{}, fmt.Errorf("%w: materialization", ErrInvalidInput)
	}
	if _, parseErr := task.ParseRetainedArtifactID(string(p.ArtifactID)); parseErr != nil {
		return BackgroundRunExport{}, fmt.Errorf("%w: artifact", ErrInvalidInput)
	}
	if _, parseErr := task.ParseResultID(string(p.ResultID)); parseErr != nil {
		return BackgroundRunExport{}, fmt.Errorf("%w: result", ErrInvalidInput)
	}
	if _, parseErr := task.ParseGitOID(string(p.ResultCommit)); parseErr != nil {
		return BackgroundRunExport{}, fmt.Errorf("%w: materialized commit", ErrInvalidInput)
	}
	if _, parseErr := task.ParseGitOID(string(p.TreeOID)); parseErr != nil {
		return BackgroundRunExport{}, fmt.Errorf("%w: materialized tree", ErrInvalidInput)
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRunExport{}, err
	}
	defer release()
	defer rollback(tx, &err)
	export, err := getBackgroundRunExport(ctx, tx, p.ExportID)
	if err != nil {
		return BackgroundRunExport{}, err
	}
	materialization, err := getArtifactMaterialization(ctx, tx, p.MaterializationID)
	if err != nil {
		return BackgroundRunExport{}, err
	}
	if export.Phase == BackgroundRunExportPhaseMaterialized && materialization.State == ArtifactMaterializationReady &&
		export.Revision == p.ExpectedRevision+1 && materialization.ArtifactID == p.ArtifactID && materialization.ResultID == p.ResultID &&
		materialization.ResultCommit == p.ResultCommit && materialization.TreeOID == p.TreeOID && materialization.ProofSHA256 == p.ProofSHA256 {
		if err := tx.Commit(); err != nil {
			return BackgroundRunExport{}, err
		}
		return export, nil
	}
	// Both UPDATEs below pin their rows (export revision and phase,
	// materialization state and identity); only the export's selected tuple
	// needs comparing here.
	if export.MaterializationID != p.MaterializationID || export.ArtifactID != p.ArtifactID || export.ResultID != p.ResultID ||
		export.ResultCommit != p.ResultCommit || export.TreeOID != p.TreeOID {
		return BackgroundRunExport{}, ErrInvalidState
	}
	now := unixMillis(p.Now)
	result, err := tx.ExecContext(ctx, `UPDATE artifact_materializations SET state='ready',result_commit=?,tree_oid=?,proof_sha256=?,
revision=revision+1,updated_at=? WHERE id=? AND export_id=? AND artifact_id=? AND result_id=? AND state='prepared' AND revision=?`,
		p.ResultCommit, p.TreeOID, p.ProofSHA256[:], now, p.MaterializationID, p.ExportID, p.ArtifactID, p.ResultID, materialization.Revision)
	if err != nil {
		return BackgroundRunExport{}, fmt.Errorf("accept artifact materialization: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunExport{}, ErrInvalidState
	}
	result, err = tx.ExecContext(ctx, `UPDATE background_run_exports SET phase='materialized',revision=revision+1,updated_at=?
WHERE id=? AND task_id=? AND attempt_id=? AND generation=? AND revision=? AND phase='materialize_started' AND state='running'`,
		now, p.ExportID, p.TaskID, p.AttemptID, p.Generation, p.ExpectedRevision)
	if err != nil {
		return BackgroundRunExport{}, fmt.Errorf("complete artifact materialization: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunExport{}, ErrInvalidState
	}
	stored, err := getBackgroundRunExport(ctx, tx, p.ExportID)
	if err != nil {
		return BackgroundRunExport{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRunExport{}, err
	}
	return stored, nil
}
