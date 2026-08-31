package taskstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

func (s *Store) SealBackgroundRun(ctx context.Context, p SealBackgroundRunParams) (BackgroundRunSealAdmission, error) {
	return s.AdmitBackgroundRunSeal(ctx, p)
}

// AdmitBackgroundRunSeal atomically wins against stop and timeout. Once input
// validation has completed, caller cancellation cannot split its durable tuple.
func (s *Store) AdmitBackgroundRunSeal(ctx context.Context, p SealBackgroundRunParams) (_ BackgroundRunSealAdmission, err error) {
	if err := validateBackgroundRunSeal(p); err != nil {
		return BackgroundRunSealAdmission{}, err
	}
	ctx = context.WithoutCancel(ctx)
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRunSealAdmission{}, fmt.Errorf("begin background run seal: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	existing, found, err := receiptByKey(ctx, tx, p.WorkspaceID, SealBackgroundRunCommand, p.Claim.Key)
	if err != nil {
		return BackgroundRunSealAdmission{}, err
	}
	if found {
		disposition, classifyErr := task.ClassifyIdempotency(&task.IdempotencyClaim{
			Scope: task.IdempotencyScope{WorkspaceID: existing.WorkspaceID, CommandKind: existing.CommandKind},
			Key:   existing.IdempotencyKey, RequestHash: existing.RequestHash, Actor: existing.Actor,
		}, p.Claim)
		if classifyErr != nil {
			return BackgroundRunSealAdmission{}, classifyErr
		}
		switch disposition {
		case task.IdempotencyOwnerMismatch:
			return BackgroundRunSealAdmission{}, ErrNotFound
		case task.IdempotencyConflict:
			return BackgroundRunSealAdmission{}, ErrIdempotencyConflict
		case task.IdempotencyReplay:
			request, getErr := getBackgroundRunSealRequestByReceipt(ctx, tx, existing.ID)
			if getErr != nil || request.TaskID != p.TaskID {
				return BackgroundRunSealAdmission{}, fmt.Errorf("%w: background seal replay", ErrCorruptStore)
			}
			run, getErr := readBackgroundRunExact(ctx, tx, request.WorkspaceID, request.TaskID)
			if getErr != nil {
				return BackgroundRunSealAdmission{}, getErr
			}
			export, getErr := getBackgroundRunExport(ctx, tx, request.ExportID)
			if getErr != nil {
				return BackgroundRunSealAdmission{}, getErr
			}
			if err := tx.Commit(); err != nil {
				return BackgroundRunSealAdmission{}, err
			}
			return BackgroundRunSealAdmission{Run: run, Request: request, Export: export, Receipt: existing, Replayed: true}, nil
		default:
			return BackgroundRunSealAdmission{}, ErrCorruptStore
		}
	}

	run, err := getBackgroundRunOwned(ctx, tx, p.WorkspaceID, p.TaskID, p.Claim.Actor)
	if err != nil {
		return BackgroundRunSealAdmission{}, err
	}
	if run.AttemptID != p.AttemptID || run.Generation != p.Generation || run.Revision != p.ExpectedRunRevision ||
		run.CancelEpoch != 0 || run.EffectPhase != BackgroundRunEffectPromptAdmitted ||
		(run.State != BackgroundRunWorking && run.State != BackgroundRunNeedsYou && run.State != BackgroundRunUncertain) ||
		run.BackgroundSealRequestID != "" || run.TimeoutRequestedAt != nil || run.StopReceiptID != "" {
		return BackgroundRunSealAdmission{}, ErrInvalidState
	}
	owner, err := getTask(ctx, tx, run.TaskID)
	if err != nil {
		return BackgroundRunSealAdmission{}, err
	}
	attempt, err := getAttempt(ctx, tx, run.AttemptID)
	if err != nil {
		return BackgroundRunSealAdmission{}, err
	}
	if owner.Revision != p.ExpectedTaskRevision || attempt.Revision != p.ExpectedAttemptRevision {
		return BackgroundRunSealAdmission{}, ErrStaleRevision
	}
	if owner.WorkspaceID != run.WorkspaceID || owner.CurrentAttemptID != attempt.ID || owner.State != task.TaskQueued ||
		owner.CancelEpoch != 0 || owner.SealedResultID != "" || attempt.TaskID != owner.ID || attempt.WorkspaceID != owner.WorkspaceID ||
		attempt.Sequence != run.Generation || attempt.State != task.AttemptPrepared || attempt.SealedResultID != "" {
		return BackgroundRunSealAdmission{}, ErrInvalidState
	}
	actorID, err := ensureActor(ctx, tx, p.Claim.Actor)
	if err != nil {
		return BackgroundRunSealAdmission{}, err
	}
	now := unixMillis(p.AcceptedAt)
	response, err := json.Marshal(struct {
		RunID             task.TaskID             `json:"run_id"`
		SealRequestID     task.SealRequestID      `json:"seal_request_id"`
		ExportID          task.ArtifactExportID   `json:"export_id"`
		ArtifactID        task.RetainedArtifactID `json:"artifact_id"`
		MaterializationID task.MaterializationID  `json:"materialization_id"`
		ResultID          task.ResultID           `json:"result_id"`
	}{run.TaskID, p.SealRequestID, p.ExportID, p.ArtifactID, p.MaterializationID, p.ResultID})
	if err != nil {
		return BackgroundRunSealAdmission{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO receipts(
id,workspace_id,command_kind,state,idempotency_key,request_hash,actor_snapshot_id,accepted_at,
api_contract_version,target_type,target_id,response_status,response_projection)
VALUES(?,?,?,'accepted',?,?,?,?,?,'task',?,202,?)`, p.ReceiptID, p.WorkspaceID, SealBackgroundRunCommand,
		p.Claim.Key, p.Claim.RequestHash[:], actorID, now, p.APIContractVersion, p.TaskID, string(response)); err != nil {
		return BackgroundRunSealAdmission{}, fmt.Errorf("insert background seal receipt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO background_run_seal_requests(
id,receipt_id,workspace_id,task_id,attempt_id,generation,expected_run_revision,expected_task_revision,expected_attempt_revision,
idempotency_key,request_hash,owner_actor_snapshot_id,export_id,artifact_id,materialization_id,result_id,result_event_id,task_event_id,
commit_epoch_seconds,policy_version,accepted_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.SealRequestID, p.ReceiptID, p.WorkspaceID, p.TaskID, p.AttemptID, p.Generation, p.ExpectedRunRevision,
		p.ExpectedTaskRevision, p.ExpectedAttemptRevision, p.Claim.Key, p.Claim.RequestHash[:], actorID, p.ExportID,
		p.ArtifactID, p.MaterializationID, p.ResultID, p.ResultEventID, p.TaskEventID, p.CommitEpochSeconds, p.PolicyVersion, now); err != nil {
		return BackgroundRunSealAdmission{}, fmt.Errorf("insert background seal request: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO background_run_exports(
id,seal_request_id,workspace_id,task_id,attempt_id,generation,artifact_id,materialization_id,result_id,state,phase,
repository_id,base_sha,opencode_session_id,opencode_message_id,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'prepared','prepared',?,?,?,?,1,?,?)`,
		p.ExportID, p.SealRequestID, p.WorkspaceID, p.TaskID, p.AttemptID, p.Generation, p.ArtifactID,
		p.MaterializationID, p.ResultID, run.RepositoryID, run.BaseOID, run.OpenCodeSessionID, run.OpenCodeMessageID, now, now); err != nil {
		return BackgroundRunSealAdmission{}, fmt.Errorf("prepare background export: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO artifact_materializations(
id,seal_request_id,export_id,artifact_id,result_id,state,revision,created_at,updated_at)
VALUES(?,?,?,?,?,'prepared',1,?,?)`, p.MaterializationID, p.SealRequestID, p.ExportID, p.ArtifactID, p.ResultID, now, now); err != nil {
		return BackgroundRunSealAdmission{}, fmt.Errorf("prepare artifact materialization: %w", err)
	}
	update, err := tx.ExecContext(ctx, `UPDATE background_runs SET state='cleanup_required',effect_phase='stop_intent',
stop_intent_at=COALESCE(stop_intent_at,?),background_seal_request_id=?,artifact_export_id=?,retained_artifact_id=?,
materialization_id=?,retained_result_id=?,result_authority_phase='seal_intent',claim_owner=NULL,claim_expires_at=NULL,
revision=revision+1,updated_at=? WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND revision=? AND
state IN ('working','needs_you','uncertain') AND effect_phase='prompt_admitted' AND cancel_epoch=0 AND stop_receipt_id IS NULL AND timeout_requested_at IS NULL`,
		now, p.SealRequestID, p.ExportID, p.ArtifactID, p.MaterializationID, p.ResultID, now,
		p.TaskID, p.AttemptID, p.WorkspaceID, p.Generation, p.ExpectedRunRevision)
	if err != nil {
		return BackgroundRunSealAdmission{}, fmt.Errorf("bind background seal: %w", err)
	}
	if changed, changeErr := update.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunSealAdmission{}, ErrInvalidState
	}
	request, err := getBackgroundRunSealRequest(ctx, tx, p.SealRequestID)
	if err != nil {
		return BackgroundRunSealAdmission{}, err
	}
	export, err := getBackgroundRunExport(ctx, tx, p.ExportID)
	if err != nil {
		return BackgroundRunSealAdmission{}, err
	}
	storedRun, err := readBackgroundRunExact(ctx, tx, p.WorkspaceID, p.TaskID)
	if err != nil {
		return BackgroundRunSealAdmission{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRunSealAdmission{}, fmt.Errorf("commit background run seal: %w", err)
	}
	return BackgroundRunSealAdmission{Run: storedRun, Request: request, Export: export, Receipt: Receipt{
		ID: p.ReceiptID, WorkspaceID: p.WorkspaceID, CommandKind: SealBackgroundRunCommand, State: ReceiptAccepted,
		IdempotencyKey: p.Claim.Key, RequestHash: p.Claim.RequestHash, Actor: p.Claim.Actor, AcceptedAt: fromUnixMillis(now),
		APIContractVersion: p.APIContractVersion, TargetType: "task", TargetID: p.TaskID, ResponseStatus: 202,
		ResponseProjection: response,
	}}, nil
}

func validateBackgroundRunSeal(p SealBackgroundRunParams) error {
	if _, err := task.ParseWorkspaceID(string(p.WorkspaceID)); err != nil || p.WorkspaceID != p.Claim.Scope.WorkspaceID {
		return fmt.Errorf("%w: background seal workspace", ErrInvalidInput)
	}
	if _, err := task.ParseTaskID(string(p.TaskID)); err != nil {
		return fmt.Errorf("%w: background seal task", ErrInvalidInput)
	}
	if _, err := task.ParseAttemptID(string(p.AttemptID)); err != nil {
		return fmt.Errorf("%w: background seal attempt", ErrInvalidInput)
	}
	if _, err := task.ParseSealRequestID(string(p.SealRequestID)); err != nil {
		return fmt.Errorf("%w: background seal request", ErrInvalidInput)
	}
	if _, err := task.ParseReceiptID(string(p.ReceiptID)); err != nil {
		return fmt.Errorf("%w: background seal receipt", ErrInvalidInput)
	}
	if _, err := task.ParseArtifactExportID(string(p.ExportID)); err != nil {
		return fmt.Errorf("%w: background export", ErrInvalidInput)
	}
	if _, err := task.ParseRetainedArtifactID(string(p.ArtifactID)); err != nil {
		return fmt.Errorf("%w: retained artifact", ErrInvalidInput)
	}
	if _, err := task.ParseMaterializationID(string(p.MaterializationID)); err != nil {
		return fmt.Errorf("%w: materialization", ErrInvalidInput)
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
	if p.Generation <= 0 || p.ExpectedRunRevision <= 0 || p.ExpectedTaskRevision <= 0 || p.ExpectedAttemptRevision <= 0 ||
		p.CommitEpochSeconds < 0 || !validBoundedText(p.PolicyVersion, 1, 128) || !validBoundedText(p.APIContractVersion, 1, 64) ||
		p.Claim.Validate() != nil || p.Claim.Scope.CommandKind != SealBackgroundRunCommand || p.Claim.Actor.Type != task.ActorOpenCode {
		return fmt.Errorf("%w: background seal authority", ErrInvalidInput)
	}
	return validExactTimestamp(p.AcceptedAt)
}

func (s *Store) GetBackgroundRunSealRequest(ctx context.Context, id task.SealRequestID) (BackgroundRunSealRequest, error) {
	if _, err := task.ParseSealRequestID(string(id)); err != nil {
		return BackgroundRunSealRequest{}, fmt.Errorf("%w: background seal request", ErrInvalidInput)
	}
	return getBackgroundRunSealRequest(ctx, s.db, id)
}

const backgroundRunSealRequestSelect = `SELECT q.id,q.receipt_id,q.workspace_id,q.task_id,q.attempt_id,q.generation,
q.expected_run_revision,q.expected_task_revision,q.expected_attempt_revision,q.idempotency_key,q.request_hash,
q.export_id,q.artifact_id,q.materialization_id,q.result_id,q.result_event_id,q.task_event_id,q.commit_epoch_seconds,
q.policy_version,q.accepted_at,a.actor_type,a.actor_id,a.display_name,a.credential_id,a.authentication,a.request_id
FROM background_run_seal_requests q JOIN actor_snapshots a ON a.id=q.owner_actor_snapshot_id`

func getBackgroundRunSealRequest(ctx context.Context, q queryRower, id task.SealRequestID) (BackgroundRunSealRequest, error) {
	return scanBackgroundRunSealRequest(q.QueryRowContext(ctx, backgroundRunSealRequestSelect+` WHERE q.id=?`, id))
}

func getBackgroundRunSealRequestByReceipt(ctx context.Context, q queryRower, id task.ReceiptID) (BackgroundRunSealRequest, error) {
	return scanBackgroundRunSealRequest(q.QueryRowContext(ctx, backgroundRunSealRequestSelect+` WHERE q.receipt_id=?`, id))
}

func scanBackgroundRunSealRequest(row rowScanner) (BackgroundRunSealRequest, error) {
	var request BackgroundRunSealRequest
	var requestHash []byte
	var accepted int64
	err := row.Scan(&request.ID, &request.ReceiptID, &request.WorkspaceID, &request.TaskID, &request.AttemptID, &request.Generation,
		&request.ExpectedRunRevision, &request.ExpectedTaskRevision, &request.ExpectedAttemptRevision, &request.IdempotencyKey, &requestHash,
		&request.ExportID, &request.ArtifactID, &request.MaterializationID, &request.ResultID, &request.ResultEventID, &request.TaskEventID,
		&request.CommitEpochSeconds, &request.PolicyVersion, &accepted, &request.Owner.Type, &request.Owner.ID, &request.Owner.DisplayName,
		&request.Owner.CredentialID, &request.Owner.Authentication, &request.Owner.RequestID)
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRunSealRequest{}, ErrNotFound
	}
	if err != nil {
		return BackgroundRunSealRequest{}, fmt.Errorf("read background seal request: %w", err)
	}
	if len(requestHash) != 32 || request.Owner.Validate() != nil || request.Owner.Type != task.ActorOpenCode {
		return BackgroundRunSealRequest{}, ErrCorruptStore
	}
	copy(request.RequestHash[:], requestHash)
	request.AcceptedAt = fromUnixMillis(accepted)
	return request, nil
}
