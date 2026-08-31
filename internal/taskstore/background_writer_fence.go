package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

func (s *Store) RecordBackgroundRunWriterFence(ctx context.Context, p RecordBackgroundRunWriterFenceParams) (_ BackgroundRun, err error) {
	if err := validateBackgroundRunClaim(p.BackgroundRunClaim); err != nil || p.ExpectedState != BackgroundRunCanceling ||
		p.ExpectedPhase != BackgroundRunEffectSealIntent || p.CancelEpoch != 0 {
		return BackgroundRun{}, fmt.Errorf("%w: writer fence claim", ErrInvalidInput)
	}
	if _, parseErr := task.ParseSealRequestID(string(p.SealRequestID)); parseErr != nil {
		return BackgroundRun{}, fmt.Errorf("%w: writer fence seal", ErrInvalidInput)
	}
	if _, parseErr := task.ParseArtifactExportID(string(p.ExportID)); parseErr != nil {
		return BackgroundRun{}, fmt.Errorf("%w: writer fence export", ErrInvalidInput)
	}
	if !validWriterFenceShape(p) {
		return BackgroundRun{}, fmt.Errorf("%w: writer fence proof", ErrInvalidInput)
	}
	digest, digestErr := WriterFenceProofDigest(p)
	if digestErr != nil || digest != p.ProofSHA256 {
		return BackgroundRun{}, fmt.Errorf("%w: writer fence digest", ErrInvalidInput)
	}

	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRun{}, err
	}
	defer release()
	defer rollback(tx, &err)
	run, err := readBackgroundRunExact(ctx, tx, p.WorkspaceID, p.TaskID)
	if err != nil {
		return BackgroundRun{}, err
	}
	if run.ResultAuthorityPhase == "writer_inactive" {
		fence, fenceErr := getWriterFence(ctx, tx, p.SealRequestID)
		if fenceErr == nil && writerFenceMatches(fence, p) {
			if err := tx.Commit(); err != nil {
				return BackgroundRun{}, err
			}
			return run, nil
		}
		return BackgroundRun{}, ErrInvalidState
	}
	if run.AttemptID != p.AttemptID || run.Generation != p.Generation || run.Revision != p.ExpectedRevision ||
		run.EffectPhase != BackgroundRunEffectSealIntent || run.ClaimOwner != p.ClaimOwner || run.ClaimGeneration != p.ClaimGeneration ||
		run.ClaimExpiresAt == nil || !run.ClaimExpiresAt.After(p.Now) || run.BackgroundSealRequestID != p.SealRequestID || run.ArtifactExportID != p.ExportID {
		return BackgroundRun{}, ErrLeaseConflict
	}
	if p.Kind == WriterFenceRuntimeStopped && (p.ContainerID != run.ObservedContainerID || p.ContainerStartedAt != run.ObservedContainerStartedAt || p.RuntimeEpoch != run.RuntimeEpoch) {
		return BackgroundRun{}, ErrInvalidState
	}
	if (p.Kind == WriterFenceNeverCreated || p.Kind == WriterFenceNeverStarted) && run.ObservedContainerID != "" {
		return BackgroundRun{}, ErrInvalidState
	}
	var stoppedAt any
	if p.StoppedAt != nil {
		stoppedAt = unixMillis(*p.StoppedAt)
	}
	var containerID, started any
	var runtime any
	if p.ContainerID != "" {
		containerID = p.ContainerID
	}
	if p.ContainerStartedAt != "" {
		started = p.ContainerStartedAt
	}
	if p.RuntimeEpoch > 0 {
		runtime = p.RuntimeEpoch
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO background_run_writer_fences(
seal_request_id,export_id,task_id,attempt_id,generation,kind,container_id,container_started_at,runtime_epoch,stopped_at,proof_sha256,recorded_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, p.SealRequestID, p.ExportID, p.TaskID, p.AttemptID, p.Generation, p.Kind,
		containerID, started, runtime, stoppedAt, p.ProofSHA256[:], unixMillis(p.Now)); err != nil {
		return BackgroundRun{}, fmt.Errorf("insert writer fence: %w", err)
	}
	evidence := "writer_fence:sha256:" + hex.EncodeToString(p.ProofSHA256[:])
	result, err := tx.ExecContext(ctx, `UPDATE background_runs SET effect_phase='writer_inactive',writer_inactive_at=?,
writer_inactive_evidence=?,last_evidence=?,result_authority_phase='writer_inactive',claim_owner=NULL,claim_expires_at=NULL,revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND revision=? AND state='cleanup_required' AND
effect_phase='stop_intent' AND result_authority_phase='seal_intent' AND claim_owner=? AND claim_generation=? AND claim_expires_at>?`,
		unixMillis(p.Now), evidence, evidence, unixMillis(p.Now), p.TaskID, p.AttemptID, p.WorkspaceID, p.Generation,
		p.ExpectedRevision, p.ClaimOwner, p.ClaimGeneration, unixMillis(p.Now))
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("record writer inactivity: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRun{}, ErrLeaseConflict
	}
	stored, err := readBackgroundRunExact(ctx, tx, p.WorkspaceID, p.TaskID)
	if err != nil {
		return BackgroundRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRun{}, err
	}
	return stored, nil
}

// WriterFenceProofDigest computes the canonical structured proof digest. The
// digest excludes ProofSHA256 itself, avoiding self-referential authority.
func WriterFenceProofDigest(p RecordBackgroundRunWriterFenceParams) ([32]byte, error) {
	if !validWriterFenceShape(p) {
		return [32]byte{}, fmt.Errorf("%w: writer fence proof", ErrInvalidInput)
	}
	proof := struct {
		SealRequestID      task.SealRequestID    `json:"sealRequestId"`
		ExportID           task.ArtifactExportID `json:"exportId"`
		TaskID             task.TaskID           `json:"taskId"`
		AttemptID          task.AttemptID        `json:"attemptId"`
		Generation         int64                 `json:"generation"`
		Kind               WriterFenceKind       `json:"kind"`
		ContainerID        string                `json:"containerId,omitempty"`
		ContainerStartedAt string                `json:"containerStartedAt,omitempty"`
		RuntimeEpoch       int64                 `json:"runtimeEpoch,omitempty"`
		StoppedAtMillis    *int64                `json:"stoppedAtMillis,omitempty"`
	}{SealRequestID: p.SealRequestID, ExportID: p.ExportID, TaskID: p.TaskID, AttemptID: p.AttemptID,
		Generation: p.Generation, Kind: p.Kind, ContainerID: p.ContainerID, ContainerStartedAt: p.ContainerStartedAt, RuntimeEpoch: p.RuntimeEpoch}
	if p.StoppedAt != nil {
		value := unixMillis(*p.StoppedAt)
		proof.StoppedAtMillis = &value
	}
	encoded, _ := json.Marshal(proof)
	return sha256.Sum256(encoded), nil
}

func validWriterFenceShape(p RecordBackgroundRunWriterFenceParams) bool {
	switch p.Kind {
	case WriterFenceNeverCreated:
		return p.ContainerID == "" && p.ContainerStartedAt == "" && p.RuntimeEpoch == 0 && p.StoppedAt == nil
	case WriterFenceNeverStarted:
		return validBoundedText(p.ContainerID, 1, 128) && p.ContainerStartedAt == "" && p.RuntimeEpoch == 0 && p.StoppedAt == nil
	case WriterFenceRuntimeStopped:
		return validBoundedText(p.ContainerID, 1, 128) && validBoundedText(p.ContainerStartedAt, 1, 64) && p.RuntimeEpoch > 0 &&
			p.StoppedAt != nil && validExactTimestamp(*p.StoppedAt) == nil && !p.StoppedAt.After(p.Now)
	default:
		return false
	}
}

func (s *Store) GetBackgroundRunWriterFence(ctx context.Context, id task.SealRequestID) (WriterFence, error) {
	if _, err := task.ParseSealRequestID(string(id)); err != nil {
		return WriterFence{}, fmt.Errorf("%w: writer fence", ErrInvalidInput)
	}
	return getWriterFence(ctx, s.db, id)
}

func getWriterFence(ctx context.Context, q queryRower, id task.SealRequestID) (WriterFence, error) {
	var value WriterFence
	var containerID, started sql.NullString
	var runtime, stoppedAt, recordedAt sql.NullInt64
	var proof []byte
	err := q.QueryRowContext(ctx, `SELECT seal_request_id,export_id,task_id,attempt_id,generation,kind,container_id,
container_started_at,runtime_epoch,stopped_at,proof_sha256,recorded_at FROM background_run_writer_fences WHERE seal_request_id=?`, id).
		Scan(&value.SealRequestID, &value.ExportID, &value.TaskID, &value.AttemptID, &value.Generation, &value.Kind, &containerID,
			&started, &runtime, &stoppedAt, &proof, &recordedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WriterFence{}, ErrNotFound
	}
	if err != nil {
		return WriterFence{}, fmt.Errorf("read writer fence: %w", err)
	}
	if len(proof) != 32 {
		return WriterFence{}, ErrCorruptStore
	}
	value.ContainerID, value.ContainerStartedAt, value.RuntimeEpoch = nullableText(containerID), nullableText(started), runtime.Int64
	value.StoppedAt, value.RecordedAt = nullableTime(stoppedAt), fromUnixMillis(recordedAt.Int64)
	copy(value.ProofSHA256[:], proof)
	return value, nil
}

func writerFenceMatches(value WriterFence, p RecordBackgroundRunWriterFenceParams) bool {
	return value.SealRequestID == p.SealRequestID && value.ExportID == p.ExportID && value.TaskID == p.TaskID &&
		value.AttemptID == p.AttemptID && value.Generation == p.Generation && value.Kind == p.Kind && value.ContainerID == p.ContainerID &&
		value.ContainerStartedAt == p.ContainerStartedAt && value.RuntimeEpoch == p.RuntimeEpoch && value.ProofSHA256 == p.ProofSHA256 &&
		((value.StoppedAt == nil && p.StoppedAt == nil) || (value.StoppedAt != nil && p.StoppedAt != nil && value.StoppedAt.Equal(*p.StoppedAt)))
}
