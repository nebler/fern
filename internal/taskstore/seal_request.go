package taskstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

const sealRequestSelect = `
SELECT q.id,q.receipt_id,q.workspace_id,q.task_id,q.attempt_id,q.state,q.completion_authority,
 q.expected_workspace_revision,q.expected_task_revision,q.expected_attempt_revision,q.repository_id,q.base_sha,
 q.expected_result_commit,q.expected_tree_oid,q.expected_outcome,q.expected_manifest_entries,q.expected_manifest_sha256,q.expected_worktree_clean,
 q.idempotency_key,q.request_hash,q.result_id,q.result_event_id,q.task_event_id,q.claim_owner,q.claim_expires_at,
 q.claim_revision,q.accepted_at,q.completed_at,q.rejected_at,q.rejected_reason,
 a.actor_type,a.actor_id,a.display_name,a.credential_id,a.authentication,a.request_id
FROM seal_requests q JOIN actor_snapshots a ON a.id=q.authorizer_actor_snapshot_id`

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
