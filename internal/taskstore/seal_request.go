package taskstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/nebler/fern/internal/task"
)

const SealTaskCommand = "task.seal"

const sealRequestSelect = `
SELECT q.id,q.receipt_id,q.workspace_id,q.task_id,q.attempt_id,q.state,q.completion_authority,
 q.expected_workspace_revision,q.expected_task_revision,q.expected_attempt_revision,q.repository_id,q.base_sha,
 q.expected_result_commit,q.expected_tree_oid,q.expected_outcome,q.expected_manifest_entries,q.expected_manifest_sha256,q.expected_worktree_clean,
 q.idempotency_key,q.request_hash,q.result_id,q.result_event_id,q.task_event_id,q.claim_owner,q.claim_expires_at,
 q.claim_revision,q.accepted_at,q.completed_at,q.rejected_at,q.rejected_reason,
 a.actor_type,a.actor_id,a.display_name,a.credential_id,a.authentication,a.request_id
FROM seal_requests q JOIN actor_snapshots a ON a.id=q.authorizer_actor_snapshot_id`

func (s *Store) GetSealPreview(ctx context.Context, taskID task.TaskID) (SealPreview, error) {
	if _, err := task.ParseTaskID(string(taskID)); err != nil {
		return SealPreview{}, fmt.Errorf("%w: task ID", ErrInvalidInput)
	}
	return getSealPreview(ctx, s.db, taskID, true)
}

func getSealPreview(ctx context.Context, q queryRower, taskID task.TaskID, requireEligible bool) (SealPreview, error) {
	owner, err := getTask(ctx, q, taskID)
	if err != nil {
		return SealPreview{}, err
	}
	attempt, err := getAttempt(ctx, q, owner.CurrentAttemptID)
	if err != nil {
		return SealPreview{}, err
	}
	workspace, err := scanWorkspace(q.QueryRowContext(ctx, workspaceSelect+` WHERE id=?`, owner.WorkspaceID))
	if err != nil {
		return SealPreview{}, err
	}
	preview := SealPreview{Workspace: workspace, Task: owner, Attempt: attempt}
	if attempt.TaskID != owner.ID || attempt.WorkspaceID != owner.WorkspaceID || owner.RepositoryID != workspace.RepositoryID || owner.BaseSHA != attempt.BaseSHA {
		return SealPreview{}, fmt.Errorf("%w: seal preview ownership", ErrCorruptStore)
	}
	if requireEligible && !sealPreviewEligible(preview) {
		return SealPreview{}, fmt.Errorf("%w: task is not eligible for a user seal", ErrInvalidState)
	}
	return preview, nil
}

func sealPreviewEligible(preview SealPreview) bool {
	return preview.Workspace.State == WorkspaceActive && preview.Task.CancelEpoch == 0 && preview.Task.SealedResultID == "" &&
		(preview.Task.State == task.TaskRunning || preview.Task.State == task.TaskInputRequired) &&
		preview.Attempt.SealedResultID == "" &&
		(preview.Attempt.State == task.AttemptAdmitted || preview.Attempt.State == task.AttemptRunning || preview.Attempt.State == task.AttemptInputRequired)
}

// RequestSeal atomically records the exact user-authorized snapshot and its
// idempotent receipt. It does not pause a workspace, inspect Git, or create a
// result.
func (s *Store) RequestSeal(ctx context.Context, p RequestSealParams) (_ SealAdmission, err error) {
	if err := validateRequestSeal(p); err != nil {
		return SealAdmission{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return SealAdmission{}, fmt.Errorf("begin seal request: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	existing, found, err := receiptByKey(ctx, tx, p.Claim.Scope.WorkspaceID, p.Claim.Scope.CommandKind, p.Claim.Key)
	if err != nil {
		return SealAdmission{}, err
	}
	if found {
		existingClaim := task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: existing.WorkspaceID, CommandKind: existing.CommandKind},
			Key: existing.IdempotencyKey, RequestHash: existing.RequestHash, Actor: existing.Actor}
		disposition, classifyErr := task.ClassifyIdempotency(&existingClaim, p.Claim)
		if classifyErr != nil {
			return SealAdmission{}, fmt.Errorf("classify seal idempotency: %w", classifyErr)
		}
		switch disposition {
		case task.IdempotencyReplay:
			request, getErr := sealRequestByReceipt(ctx, tx, existing.ID)
			if getErr != nil {
				return SealAdmission{}, getErr
			}
			preview, getErr := getSealPreview(ctx, tx, request.TaskID, false)
			if getErr != nil {
				return SealAdmission{}, getErr
			}
			if err := tx.Commit(); err != nil {
				return SealAdmission{}, fmt.Errorf("finish seal request replay: %w", err)
			}
			return SealAdmission{Request: request, Receipt: existing, Preview: preview, Replayed: true}, nil
		case task.IdempotencyOwnerMismatch:
			return SealAdmission{}, ErrIdempotencyOwnerMismatch
		case task.IdempotencyConflict:
			return SealAdmission{}, &ConflictError{ReceiptID: existing.ID, TargetID: existing.TargetID}
		default:
			return SealAdmission{}, fmt.Errorf("%w: unexpected seal idempotency disposition", ErrCorruptStore)
		}
	}

	preview, err := getSealPreview(ctx, tx, p.TaskID, true)
	if errors.Is(err, ErrNotFound) {
		return SealAdmission{}, &NotFoundError{Kind: "task", ID: string(p.TaskID)}
	}
	if err != nil {
		return SealAdmission{}, err
	}
	if preview.Workspace.ID != p.Claim.Scope.WorkspaceID {
		return SealAdmission{}, &NotFoundError{Kind: "task", ID: string(p.TaskID)}
	}
	if preview.Workspace.Revision != p.ExpectedWorkspaceRevision || preview.Task.Revision != p.ExpectedTaskRevision ||
		preview.Attempt.Revision != p.ExpectedAttemptRevision {
		return SealAdmission{}, ErrStaleRevision
	}
	if preview.Task.RepositoryID != p.RepositoryID || preview.Task.BaseSHA != p.BaseSHA {
		return SealAdmission{}, ErrRepositoryMismatch
	}

	actorID, err := ensureActor(ctx, tx, p.Claim.Actor)
	if err != nil {
		return SealAdmission{}, err
	}
	response, err := json.Marshal(struct {
		ReceiptID     task.ReceiptID     `json:"receiptId"`
		SealRequestID task.SealRequestID `json:"sealRequestId"`
		TaskID        task.TaskID        `json:"taskId"`
		AttemptID     task.AttemptID     `json:"attemptId"`
		State         SealRequestState   `json:"state"`
	}{p.ReceiptID, p.SealRequestID, preview.Task.ID, preview.Attempt.ID, SealRequestPending})
	if err != nil {
		return SealAdmission{}, fmt.Errorf("encode seal receipt projection: %w", err)
	}
	acceptedMS := unixMillis(p.AcceptedAt)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO receipts(id,workspace_id,command_kind,state,idempotency_key,request_hash,actor_snapshot_id,
 accepted_at,api_contract_version,target_type,target_id,response_status,response_projection)
VALUES(?,?,?,'accepted',?,?,?,?,?,'task',?,202,?)`, p.ReceiptID, preview.Workspace.ID, SealTaskCommand,
		p.Claim.Key, p.Claim.RequestHash[:], actorID, acceptedMS, p.APIContractVersion, preview.Task.ID, string(response)); err != nil {
		return SealAdmission{}, fmt.Errorf("insert seal receipt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO seal_requests(
 id,receipt_id,workspace_id,task_id,attempt_id,state,completion_authority,
 expected_workspace_revision,expected_task_revision,expected_attempt_revision,repository_id,base_sha,
 expected_result_commit,expected_tree_oid,expected_outcome,expected_manifest_entries,expected_manifest_sha256,expected_worktree_clean,
 idempotency_key,request_hash,authorizer_actor_snapshot_id,result_id,result_event_id,task_event_id,
 claim_revision,accepted_at)
VALUES(?,?,?,?,?,'pending','user_seal',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?)`,
		p.SealRequestID, p.ReceiptID, preview.Workspace.ID, preview.Task.ID, preview.Attempt.ID,
		p.ExpectedWorkspaceRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision, p.RepositoryID, p.BaseSHA,
		p.ExpectedResultCommit, p.ExpectedTreeOID, p.ExpectedOutcome, p.ExpectedManifestEntries, p.ExpectedManifestSHA256[:],
		p.ExpectedWorktreeClean, p.Claim.Key, p.Claim.RequestHash[:], actorID, p.ResultID, p.ResultEventID, p.TaskEventID, acceptedMS); err != nil {
		return SealAdmission{}, fmt.Errorf("insert seal request: %w", err)
	}
	request, err := getSealRequest(ctx, tx, p.SealRequestID)
	if err != nil {
		return SealAdmission{}, err
	}
	receipt, err := receiptByID(ctx, tx, p.ReceiptID)
	if err != nil {
		return SealAdmission{}, err
	}
	if err := tx.Commit(); err != nil {
		return SealAdmission{}, fmt.Errorf("commit seal request: %w", err)
	}
	return SealAdmission{Request: request, Receipt: receipt, Preview: preview}, nil
}

func (s *Store) GetSealRequest(ctx context.Context, id task.SealRequestID) (SealRequest, error) {
	if _, err := task.ParseSealRequestID(string(id)); err != nil {
		return SealRequest{}, fmt.Errorf("%w: seal request ID", ErrInvalidInput)
	}
	return getSealRequest(ctx, s.db, id)
}

func (s *Store) GetSealRequestByReceipt(ctx context.Context, id task.ReceiptID) (SealRequest, error) {
	if _, err := task.ParseReceiptID(string(id)); err != nil {
		return SealRequest{}, fmt.Errorf("%w: receipt ID", ErrInvalidInput)
	}
	return sealRequestByReceipt(ctx, s.db, id)
}

func (s *Store) ClaimSealRequest(ctx context.Context, p ClaimSealRequestParams) (_ SealRequestWork, err error) {
	if _, parseErr := task.ParseWorkspaceID(string(p.WorkspaceID)); parseErr != nil || !validBoundedText(p.ClaimOwner, 1, 64) ||
		validExactTimestamp(p.Now) != nil || validExactTimestamp(p.LeaseExpiresAt) != nil || !p.LeaseExpiresAt.After(p.Now) {
		return SealRequestWork{}, fmt.Errorf("%w: seal claim", ErrInvalidInput)
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return SealRequestWork{}, fmt.Errorf("begin seal claim: %w", err)
	}
	defer release()
	defer rollback(tx, &err)
	var id task.SealRequestID
	err = tx.QueryRowContext(ctx, `
SELECT id FROM seal_requests WHERE workspace_id=? AND
 (state='pending' OR (state='claimed' AND claim_expires_at<=?))
ORDER BY accepted_at,id LIMIT 1`, p.WorkspaceID, unixMillis(p.Now)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return SealRequestWork{}, &NotFoundError{Kind: "pending seal request", ID: string(p.WorkspaceID)}
	}
	if err != nil {
		return SealRequestWork{}, fmt.Errorf("find seal request claim: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE seal_requests SET state='claimed',claim_owner=?,claim_expires_at=?,claim_revision=claim_revision+1
WHERE id=? AND (state='pending' OR (state='claimed' AND claim_expires_at<=?))`,
		p.ClaimOwner, unixMillis(p.LeaseExpiresAt), id, unixMillis(p.Now))
	if err != nil {
		return SealRequestWork{}, fmt.Errorf("claim seal request: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return SealRequestWork{}, ErrLeaseConflict
	}
	request, err := getSealRequest(ctx, tx, id)
	if err != nil {
		return SealRequestWork{}, err
	}
	preview, err := getSealPreview(ctx, tx, request.TaskID, false)
	if err != nil {
		return SealRequestWork{}, err
	}
	if err := tx.Commit(); err != nil {
		return SealRequestWork{}, fmt.Errorf("commit seal claim: %w", err)
	}
	return SealRequestWork{Request: request, Preview: preview}, nil
}

func (s *Store) InspectClaimedSealRequest(ctx context.Context, id task.SealRequestID, owner string, claimRevision int64) (SealRequestWork, error) {
	if _, err := task.ParseSealRequestID(string(id)); err != nil || !validBoundedText(owner, 1, 64) || claimRevision < 1 {
		return SealRequestWork{}, fmt.Errorf("%w: claimed seal request", ErrInvalidInput)
	}
	request, err := getSealRequest(ctx, s.db, id)
	if err != nil {
		return SealRequestWork{}, err
	}
	if request.State != SealRequestClaimed || request.ClaimOwner != owner || request.ClaimRevision != claimRevision {
		return SealRequestWork{}, ErrLeaseConflict
	}
	preview, err := getSealPreview(ctx, s.db, request.TaskID, false)
	if err != nil {
		return SealRequestWork{}, err
	}
	return SealRequestWork{Request: request, Preview: preview}, nil
}

func (s *Store) RejectSealRequest(ctx context.Context, p RejectSealRequestParams) (SealRequest, error) {
	if _, err := task.ParseSealRequestID(string(p.SealRequestID)); err != nil || !validBoundedText(p.ClaimOwner, 1, 64) ||
		p.ExpectedClaimRevision < 1 || !validBoundedText(p.Reason, 1, 1000) || validExactTimestamp(p.RejectedAt) != nil {
		return SealRequest{}, fmt.Errorf("%w: seal rejection", ErrInvalidInput)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE seal_requests SET state='rejected',claim_owner=NULL,claim_expires_at=NULL,rejected_at=?,rejected_reason=?
WHERE id=? AND state='claimed' AND claim_owner=? AND claim_revision=?`, unixMillis(p.RejectedAt), p.Reason,
		p.SealRequestID, p.ClaimOwner, p.ExpectedClaimRevision)
	if err != nil {
		return SealRequest{}, fmt.Errorf("reject seal request: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return SealRequest{}, ErrLeaseConflict
	}
	return s.GetSealRequest(ctx, p.SealRequestID)
}

func validateRequestSeal(p RequestSealParams) error {
	if _, err := task.ParseSealRequestID(string(p.SealRequestID)); err != nil {
		return fmt.Errorf("%w: seal request ID", ErrInvalidInput)
	}
	if _, err := task.ParseReceiptID(string(p.ReceiptID)); err != nil {
		return fmt.Errorf("%w: receipt ID", ErrInvalidInput)
	}
	if _, err := task.ParseResultID(string(p.ResultID)); err != nil {
		return fmt.Errorf("%w: result ID", ErrInvalidInput)
	}
	if _, err := task.ParseTaskID(string(p.TaskID)); err != nil {
		return fmt.Errorf("%w: task ID", ErrInvalidInput)
	}
	if err := validateAttemptAndEvents("att_01890a5d-ac00-7000-8000-000000000000", p.ResultEventID, p.TaskEventID); err != nil {
		// validateAttemptAndEvents also validates an attempt ID; use a fixed valid
		// sentinel because the current attempt is selected transactionally.
		return err
	}
	if err := p.Claim.Validate(); err != nil || p.Claim.Scope.CommandKind != SealTaskCommand {
		return fmt.Errorf("%w: seal idempotency claim", ErrInvalidInput)
	}
	if p.Claim.Actor.Type != task.ActorDevice && p.Claim.Actor.Type != task.ActorOperator {
		return fmt.Errorf("%w: seal authorizer", ErrInvalidInput)
	}
	if p.ExpectedWorkspaceRevision < 1 || p.ExpectedTaskRevision < 1 || p.ExpectedAttemptRevision < 1 ||
		p.RepositoryID == 0 || uint64(p.RepositoryID) > math.MaxInt64 || p.ExpectedManifestEntries < 0 || p.ExpectedManifestEntries > maxManifestEntries {
		return fmt.Errorf("%w: seal revisions or tuple", ErrInvalidInput)
	}
	for name, oid := range map[string]task.GitOID{"base SHA": p.BaseSHA, "result commit": p.ExpectedResultCommit, "tree OID": p.ExpectedTreeOID} {
		if _, err := task.ParseGitOID(string(oid)); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidInput, name)
		}
	}
	tuple := task.ResultTuple{RepositoryTuple: task.RepositoryTuple{RepositoryID: p.RepositoryID, BaseSHA: p.BaseSHA},
		ResultCommit: p.ExpectedResultCommit, Outcome: p.ExpectedOutcome, ManifestEntries: p.ExpectedManifestEntries, WorktreeClean: p.ExpectedWorktreeClean}
	if err := tuple.ValidateAgainst(tuple.RepositoryTuple); err != nil {
		return fmt.Errorf("%w: authorized result tuple", ErrInvalidInput)
	}
	if !validBoundedText(p.APIContractVersion, 1, 64) || validExactTimestamp(p.AcceptedAt) != nil {
		return fmt.Errorf("%w: seal API contract or timestamp", ErrInvalidInput)
	}
	return nil
}

func getSealRequest(ctx context.Context, q queryRower, id task.SealRequestID) (SealRequest, error) {
	request, err := scanSealRequest(q.QueryRowContext(ctx, sealRequestSelect+` WHERE q.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return SealRequest{}, ErrNotFound
	}
	if err != nil {
		return SealRequest{}, fmt.Errorf("read seal request: %w", err)
	}
	return request, nil
}

func sealRequestByReceipt(ctx context.Context, q queryRower, receiptID task.ReceiptID) (SealRequest, error) {
	request, err := scanSealRequest(q.QueryRowContext(ctx, sealRequestSelect+` WHERE q.receipt_id=?`, receiptID))
	if errors.Is(err, sql.ErrNoRows) {
		return SealRequest{}, ErrCorruptStore
	}
	if err != nil {
		return SealRequest{}, fmt.Errorf("read seal request receipt: %w", err)
	}
	return request, nil
}

func scanSealRequest(row rowScanner) (SealRequest, error) {
	var request SealRequest
	var repositoryID int64
	var manifestHash, requestHash []byte
	var claimOwner, rejectedReason sql.NullString
	var claimExpiresAt, completedAt, rejectedAt sql.NullInt64
	var acceptedAt int64
	err := row.Scan(&request.ID, &request.ReceiptID, &request.WorkspaceID, &request.TaskID, &request.AttemptID,
		&request.State, &request.CompletionAuthority, &request.ExpectedWorkspaceRevision, &request.ExpectedTaskRevision,
		&request.ExpectedAttemptRevision, &repositoryID, &request.BaseSHA, &request.ExpectedResultCommit, &request.ExpectedTreeOID,
		&request.ExpectedOutcome, &request.ExpectedManifestEntries, &manifestHash, &request.ExpectedWorktreeClean, &request.IdempotencyKey, &requestHash,
		&request.ResultID, &request.ResultEventID, &request.TaskEventID, &claimOwner, &claimExpiresAt, &request.ClaimRevision,
		&acceptedAt, &completedAt, &rejectedAt, &rejectedReason, &request.Authorizer.Type, &request.Authorizer.ID,
		&request.Authorizer.DisplayName, &request.Authorizer.CredentialID, &request.Authorizer.Authentication, &request.Authorizer.RequestID)
	if err != nil {
		return SealRequest{}, err
	}
	if repositoryID <= 0 || len(manifestHash) != 32 || len(requestHash) != 32 || request.CompletionAuthority != SealAuthorityUser ||
		(request.State != SealRequestPending && request.State != SealRequestClaimed && request.State != SealRequestCompleted && request.State != SealRequestRejected) ||
		request.Authorizer.Validate() != nil {
		return SealRequest{}, ErrCorruptStore
	}
	request.RepositoryID = task.RepositoryID(repositoryID)
	copy(request.ExpectedManifestSHA256[:], manifestHash)
	copy(request.RequestHash[:], requestHash)
	request.AcceptedAt = fromUnixMillis(acceptedAt)
	request.ClaimOwner = claimOwner.String
	request.ClaimExpiresAt = nullableTime(claimExpiresAt)
	request.CompletedAt = nullableTime(completedAt)
	request.RejectedAt = nullableTime(rejectedAt)
	request.RejectedReason = rejectedReason.String
	return request, nil
}

func receiptByID(ctx context.Context, q queryRower, id task.ReceiptID) (Receipt, error) {
	receipt, err := scanReceipt(q.QueryRowContext(ctx, receiptSelect+` WHERE r.id=?`, id))
	if err != nil {
		return Receipt{}, fmt.Errorf("read receipt: %w", err)
	}
	return receipt, nil
}

func sealRequestMatchesPreview(work SealRequestWork) bool {
	request, preview := work.Request, work.Preview
	return sealPreviewEligible(preview) && request.WorkspaceID == preview.Workspace.ID && request.TaskID == preview.Task.ID &&
		request.AttemptID == preview.Attempt.ID && request.ExpectedWorkspaceRevision == preview.Workspace.Revision &&
		request.ExpectedTaskRevision == preview.Task.Revision && request.ExpectedAttemptRevision == preview.Attempt.Revision &&
		request.RepositoryID == preview.Task.RepositoryID && request.BaseSHA == preview.Task.BaseSHA && request.BaseSHA == preview.Attempt.BaseSHA
}

func sealLeaseValid(request SealRequest, owner string, revision int64, now time.Time) bool {
	return request.State == SealRequestClaimed && request.ClaimOwner == owner && request.ClaimRevision == revision &&
		request.ClaimExpiresAt != nil && request.ClaimExpiresAt.After(now)
}
