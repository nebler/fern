package taskstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

// SealAuthorizedResult is the only transition that consumes a user seal
// request. Collection is read-only; this transaction inserts the result,
// supersedes (not succeeds) the OpenCode attempt, completes the task, and
// closes the request together.
func (s *Store) SealAuthorizedResult(ctx context.Context, p SealAuthorizedResultParams) (_ SealedResult, err error) {
	p.Result.CompletionAuthority = SealAuthorityUser
	p.Result.SealRequestID = p.SealRequestID
	if _, parseErr := task.ParseSealRequestID(string(p.SealRequestID)); parseErr != nil ||
		!validBoundedText(p.ClaimOwner, 1, 64) || p.ExpectedClaimRevision < 1 || p.Result.Authorizer == nil {
		return SealedResult{}, fmt.Errorf("%w: authorized seal identity", ErrInvalidInput)
	}
	if authErr := p.Result.Authorizer.Validate(); authErr != nil ||
		(p.Result.Authorizer.Type != task.ActorDevice && p.Result.Authorizer.Type != task.ActorOperator) {
		return SealedResult{}, fmt.Errorf("%w: authorized seal actor", ErrInvalidInput)
	}
	manifest, err := validateResultMaterial(p.Result)
	if err != nil {
		return SealedResult{}, err
	}
	p.Result.Manifest = manifest
	payload, err := resultSealPayload(p.Result)
	if err != nil {
		return SealedResult{}, err
	}

	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return SealedResult{}, fmt.Errorf("begin authorized result seal: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	request, err := getSealRequest(ctx, tx, p.SealRequestID)
	if err != nil {
		return SealedResult{}, err
	}
	if request.State == SealRequestCompleted {
		return finishAuthorizedSealReplay(ctx, tx, request)
	}
	if !sealLeaseValid(request, p.ClaimOwner, p.ExpectedClaimRevision, p.Result.SealedAt) {
		return SealedResult{}, ErrLeaseConflict
	}
	preview, err := getSealPreview(ctx, tx, request.TaskID, false)
	if err != nil {
		return SealedResult{}, err
	}
	work := SealRequestWork{Request: request, Preview: preview}
	if !sealRequestMatchesPreview(work) {
		return SealedResult{}, fmt.Errorf("%w: authorized ownership changed", ErrStaleRevision)
	}
	owner, attempt := preview.Task, preview.Attempt
	if request.ResultID != p.Result.ResultID || request.ResultEventID != p.Result.ResultEventID || request.TaskEventID != p.Result.TaskEventID ||
		request.TaskID != p.Result.TaskID || request.AttemptID != p.Result.AttemptID ||
		request.ExpectedTaskRevision != p.Result.ExpectedTaskRevision || request.ExpectedAttemptRevision != p.Result.ExpectedAttemptRevision ||
		request.RepositoryID != p.Result.RepositoryID || request.BaseSHA != p.Result.BaseSHA ||
		request.ExpectedResultCommit != p.Result.ResultCommit || request.ExpectedTreeOID != p.Result.TreeOID ||
		request.ExpectedOutcome != p.Result.Outcome || request.ExpectedManifestEntries != len(p.Result.Manifest) ||
		request.ExpectedManifestSHA256 != p.Result.ManifestSHA256 || request.ExpectedWorktreeClean != p.Result.WorktreeClean ||
		request.Authorizer != *p.Result.Authorizer {
		return SealedResult{}, fmt.Errorf("%w: collected snapshot differs from authorization", ErrInvalidState)
	}
	if attempt.OpenCodeSessionID != p.Result.OpenCodeSessionID || attempt.OpenCodeMessageID != p.Result.OpenCodeMessageID ||
		p.Result.CollectedAt.Before(request.AcceptedAt) || p.Result.CollectedAt.Before(attempt.UpdatedAt) || p.Result.SealedAt.Before(p.Result.CollectedAt) {
		return SealedResult{}, fmt.Errorf("%w: authorized result identity or time", ErrInvalidInput)
	}

	creatorID, err := ensureActor(ctx, tx, p.Result.Actor)
	if err != nil {
		return SealedResult{}, err
	}
	authorizerID, err := ensureActor(ctx, tx, *p.Result.Authorizer)
	if err != nil {
		return SealedResult{}, err
	}
	sealedMS := unixMillis(p.Result.SealedAt)
	resultEvent, err := insertAttemptEvent(ctx, tx, p.Result.ResultEventID, attempt, "attempt.result_sealed", sealedMS, creatorID, payload)
	if err != nil {
		return SealedResult{}, err
	}
	taskEvent, err := insertTaskEvent(ctx, tx, p.Result.TaskEventID, owner, "task.completed", sealedMS, creatorID, payload)
	if err != nil {
		return SealedResult{}, err
	}
	for i, entry := range p.Result.Manifest {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO result_manifest(result_id,ordinal,path_base64,change_kind,old_mode,new_mode,old_blob_oid,new_blob_oid,old_size,new_size)
VALUES(?,?,?,?,?,?,?,?,?,?)`, p.Result.ResultID, i, entry.PathBase64, entry.ChangeKind, entry.OldMode, entry.NewMode,
			entry.OldBlobOID, entry.NewBlobOID, entry.OldSize, entry.NewSize); err != nil {
			return SealedResult{}, fmt.Errorf("insert authorized manifest entry %d: %w", i, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO results(
 id,task_id,attempt_id,workspace_id,state,outcome,repository_id,base_sha,result_commit,tree_oid,
 worktree_clean,manifest_entries,manifest_sha256,opencode_session_id,opencode_message_id,evidence_sha256,
 policy_version,collected_at,sealed_at,creator_actor_snapshot_id,sealed_event_id,completed_event_id,revision,created_at,updated_at,
 completion_authority,seal_request_id,authorizer_actor_snapshot_id)
VALUES(?,?,?,?,'sealed',?,?,?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,1,?,?, 'user_seal',?,?)`,
		p.Result.ResultID, owner.ID, attempt.ID, owner.WorkspaceID, p.Result.Outcome, p.Result.RepositoryID, p.Result.BaseSHA,
		p.Result.ResultCommit, p.Result.TreeOID, len(p.Result.Manifest), p.Result.ManifestSHA256[:], p.Result.OpenCodeSessionID,
		p.Result.OpenCodeMessageID, p.Result.EvidenceSHA256[:], p.Result.PolicyVersion, unixMillis(p.Result.CollectedAt), sealedMS,
		creatorID, resultEvent.ID, taskEvent.ID, sealedMS, sealedMS, request.ID, authorizerID); err != nil {
		return SealedResult{}, fmt.Errorf("insert user-authorized result: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE attempts SET state='superseded',sealed_result_id=?,delivery_claim_owner=NULL,delivery_claim_expires_at=NULL,
 revision=revision+1,updated_at=?
WHERE id=? AND task_id=? AND workspace_id=? AND state=? AND sealed_result_id IS NULL AND revision=?`,
		p.Result.ResultID, sealedMS, attempt.ID, owner.ID, owner.WorkspaceID, attempt.State, request.ExpectedAttemptRevision)
	if err != nil {
		return SealedResult{}, fmt.Errorf("supersede user-sealed attempt: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return SealedResult{}, fmt.Errorf("%w: authorized attempt changed", ErrInvalidState)
	}
	result, err = tx.ExecContext(ctx, `
UPDATE tasks SET state='completed',sealed_result_id=?,latest_event_cursor=?,revision=revision+1,updated_at=?
WHERE id=? AND workspace_id=? AND state=? AND cancel_epoch=0 AND current_attempt_id=? AND sealed_result_id IS NULL AND revision=?`,
		p.Result.ResultID, taskEvent.Cursor, sealedMS, owner.ID, owner.WorkspaceID, owner.State, attempt.ID, request.ExpectedTaskRevision)
	if err != nil {
		return SealedResult{}, fmt.Errorf("complete user-sealed task: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return SealedResult{}, fmt.Errorf("%w: authorized task changed", ErrInvalidState)
	}
	result, err = tx.ExecContext(ctx, `
UPDATE seal_requests SET state='completed',claim_owner=NULL,claim_expires_at=NULL,completed_at=?
WHERE id=? AND state='claimed' AND claim_owner=? AND claim_revision=?`, sealedMS, request.ID, p.ClaimOwner, p.ExpectedClaimRevision)
	if err != nil {
		return SealedResult{}, fmt.Errorf("complete seal request: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return SealedResult{}, ErrLeaseConflict
	}

	storedResult, err := getResult(ctx, tx, p.Result.ResultID)
	if err != nil {
		return SealedResult{}, err
	}
	storedAttempt, err := getAttempt(ctx, tx, attempt.ID)
	if err != nil {
		return SealedResult{}, err
	}
	storedTask, err := getTask(ctx, tx, owner.ID)
	if err != nil {
		return SealedResult{}, err
	}
	if storedAttempt.State != task.AttemptSuperseded || storedTask.State != task.TaskCompleted ||
		resultEvent.Cursor >= taskEvent.Cursor || storedTask.LatestEventCursor != taskEvent.Cursor {
		return SealedResult{}, fmt.Errorf("%w: authorized seal projection", ErrCorruptStore)
	}
	if err := tx.Commit(); err != nil {
		return SealedResult{}, fmt.Errorf("commit authorized result seal: %w", err)
	}
	return SealedResult{Result: storedResult, Manifest: p.Result.Manifest, Task: storedTask, Attempt: storedAttempt,
		ResultEvent: resultEvent, TaskEvent: taskEvent}, nil
}

func finishAuthorizedSealReplay(ctx context.Context, tx *sql.Tx, request SealRequest) (SealedResult, error) {
	result, err := getResult(ctx, tx, request.ResultID)
	if err != nil {
		return SealedResult{}, err
	}
	if result.CompletionAuthority != SealAuthorityUser || result.SealRequestID != request.ID {
		return SealedResult{}, fmt.Errorf("%w: completed seal request result", ErrCorruptStore)
	}
	manifest, err := getResultManifest(ctx, tx, result.ID)
	if err != nil {
		return SealedResult{}, err
	}
	owner, err := getTask(ctx, tx, request.TaskID)
	if err != nil {
		return SealedResult{}, err
	}
	attempt, err := getAttempt(ctx, tx, request.AttemptID)
	if err != nil {
		return SealedResult{}, err
	}
	resultEvent, err := scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.id=?`, request.ResultEventID))
	if err != nil {
		return SealedResult{}, err
	}
	taskEvent, err := scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.id=?`, request.TaskEventID))
	if err != nil {
		return SealedResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return SealedResult{}, fmt.Errorf("finish authorized seal replay: %w", err)
	}
	return SealedResult{Result: result, Manifest: manifest, Task: owner, Attempt: attempt, ResultEvent: resultEvent, TaskEvent: taskEvent, Replayed: true}, nil
}
