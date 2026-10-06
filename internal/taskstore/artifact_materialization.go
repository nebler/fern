package taskstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

func (s *Store) GetArtifactMaterialization(ctx context.Context, id task.MaterializationID) (ArtifactMaterialization, error) {
	return getArtifactMaterialization(ctx, s.db, id)
}

func getArtifactMaterialization(ctx context.Context, q queryRower, id task.MaterializationID) (ArtifactMaterialization, error) {
	var value ArtifactMaterialization
	var resultCommit, treeOID sql.NullString
	var proof []byte
	var createdAt, updatedAt int64
	err := q.QueryRowContext(ctx, `SELECT id,seal_request_id,export_id,artifact_id,result_id,state,result_commit,tree_oid,
proof_sha256,revision,created_at,updated_at FROM artifact_materializations WHERE id=?`, id).
		Scan(&value.ID, &value.SealRequestID, &value.ExportID, &value.ArtifactID, &value.ResultID, &value.State,
			&resultCommit, &treeOID, &proof, &value.Revision, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactMaterialization{}, ErrNotFound
	}
	if err != nil {
		return ArtifactMaterialization{}, fmt.Errorf("read artifact materialization: %w", err)
	}
	value.ResultCommit, value.TreeOID = task.GitOID(nullableText(resultCommit)), task.GitOID(nullableText(treeOID))
	value.CreatedAt, value.UpdatedAt = fromUnixMillis(createdAt), fromUnixMillis(updatedAt)
	copy(value.ProofSHA256[:], proof)
	return value, nil
}
