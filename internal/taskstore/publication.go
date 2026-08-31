package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nebler/fern/internal/task"
)

const PublishResultCommand = "result.publish"

const publicationSelect = `
SELECT p.id,p.operation_id,p.admission_receipt_id,p.result_id,p.verification_id,p.task_id,p.attempt_id,p.workspace_id,p.state,p.effect_phase,
 p.installation_id,p.repository_id,p.repository_full_name,p.base_ref,p.base_sha,p.result_commit,p.branch,p.expected_remote_old_sha,
 p.broker_policy_version,p.broker_policy_sha256,p.observed_remote_sha,p.pr_number,p.pr_url,p.pr_state,p.pr_draft,
 p.pr_repository_id,p.pr_repository_full_name,p.pr_base_repository_id,p.pr_base_repository_full_name,p.pr_base_ref,p.pr_base_sha,
 p.pr_head_repository_id,p.pr_head_repository_full_name,p.pr_head_repository_owner,p.pr_head_repository_name,p.pr_head_ref,p.pr_head_sha,
 p.reason,p.latest_event_id,p.revision,p.created_at,p.updated_at,
 a.actor_type,a.actor_id,a.display_name,a.credential_id,a.authentication,a.request_id
FROM publications p JOIN actor_snapshots a ON a.id=p.requester_actor_snapshot_id`

func (s *Store) GetPublication(ctx context.Context, id task.PublicationID) (Publication, error) {
	if _, err := task.ParsePublicationID(string(id)); err != nil {
		return Publication{}, fmt.Errorf("%w: publication ID", ErrInvalidInput)
	}
	return getPublication(ctx, s.db, id)
}

func (s *Store) InspectPublication(ctx context.Context, id task.PublicationID) (PublicationRecord, error) {
	p, err := s.GetPublication(ctx, id)
	if err != nil {
		return PublicationRecord{}, err
	}
	e, err := getJournalEvent(ctx, s.db, p.LatestEventID)
	if err != nil {
		return PublicationRecord{}, err
	}
	return PublicationRecord{Publication: p, Event: e}, nil
}

func (s *Store) FindPreparedPublication(ctx context.Context, workspaceID task.WorkspaceID) (PublicationRecord, error) {
	return s.findPublication(ctx, workspaceID, `p.admission_receipt_id IS NOT NULL AND p.state='prepared' AND p.effect_phase='none'`, "prepared publication")
}

// FindPublicationWork returns mutation or read-only reconciliation work. The
// row's phase determines the only permitted coordinator action.
func (s *Store) FindPublicationWork(ctx context.Context, workspaceID task.WorkspaceID) (_ PublicationWork, err error) {
	if _, parseErr := task.ParseWorkspaceID(string(workspaceID)); parseErr != nil {
		return PublicationWork{}, fmt.Errorf("%w: workspace ID", ErrInvalidInput)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PublicationWork{}, fmt.Errorf("begin publication work read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var id task.PublicationID
	err = tx.QueryRowContext(ctx, `
SELECT p.id FROM publications p JOIN results r ON r.id=p.result_id JOIN tasks t ON t.id=p.task_id
JOIN attempts a ON a.id=p.attempt_id JOIN verifications v ON v.id=p.verification_id
WHERE p.workspace_id=? AND p.admission_receipt_id IS NOT NULL AND p.state IN ('prepared','running','uncertain') AND r.state='sealed' AND
 t.current_attempt_id=a.id AND t.sealed_result_id=r.id AND t.state='completed' AND t.cancel_epoch=0 AND
 (a.state='succeeded' OR (a.state='superseded' AND r.completion_authority='user_seal')) AND
 a.sealed_result_id=r.id AND v.state='succeeded' AND v.result_id=r.id AND
 v.verified_commit=r.result_commit AND `+resultConsumerSourcePredicate+`
 ORDER BY p.updated_at,p.id LIMIT 1`, workspaceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return PublicationWork{}, &NotFoundError{Kind: "publication work", ID: string(workspaceID)}
	}
	if err != nil {
		return PublicationWork{}, fmt.Errorf("find publication work: %w", err)
	}
	p, err := getPublication(ctx, tx, id)
	if err != nil {
		return PublicationWork{}, err
	}
	owner, err := getTask(ctx, tx, p.TaskID)
	if err != nil {
		return PublicationWork{}, err
	}
	attempt, err := getAttempt(ctx, tx, p.AttemptID)
	if err != nil {
		return PublicationWork{}, err
	}
	result, err := getResult(ctx, tx, p.ResultID)
	if err != nil {
		return PublicationWork{}, err
	}
	verification, err := getVerification(ctx, tx, p.VerificationID)
	if err != nil {
		return PublicationWork{}, err
	}
	event, err := getJournalEvent(ctx, tx, p.LatestEventID)
	if err != nil {
		return PublicationWork{}, err
	}
	if owner.ID != p.TaskID || attempt.ID != p.AttemptID || attempt.TaskID != owner.ID || result.ID != p.ResultID ||
		result.TaskID != owner.ID || result.AttemptID != attempt.ID || verification.ID != p.VerificationID ||
		verification.ResultID != result.ID || verification.TaskID != owner.ID || verification.AttemptID != attempt.ID ||
		event.EntityType != "publication" || event.EntityID != string(p.ID) || event.EntityRevision != p.Revision {
		return PublicationWork{}, ErrCorruptStore
	}
	if err := tx.Commit(); err != nil {
		return PublicationWork{}, fmt.Errorf("finish publication work read: %w", err)
	}
	return PublicationWork{Publication: p, Task: owner, Attempt: attempt, Result: result, Verification: verification, Event: event}, nil
}

func (s *Store) FindUncertainPublication(ctx context.Context, workspaceID task.WorkspaceID) (PublicationRecord, error) {
	return s.findPublication(ctx, workspaceID, `p.admission_receipt_id IS NOT NULL AND p.state='uncertain'`, "uncertain publication")
}

func (s *Store) findPublication(ctx context.Context, workspaceID task.WorkspaceID, predicate, kind string) (PublicationRecord, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil {
		return PublicationRecord{}, fmt.Errorf("%w: workspace ID", ErrInvalidInput)
	}
	var id task.PublicationID
	err := s.db.QueryRowContext(ctx, `
SELECT p.id FROM publications p JOIN results r ON r.id=p.result_id JOIN tasks t ON t.id=p.task_id
JOIN attempts a ON a.id=p.attempt_id JOIN verifications v ON v.id=p.verification_id
WHERE p.workspace_id=? AND `+predicate+` AND r.state='sealed' AND t.current_attempt_id=a.id AND
 t.sealed_result_id=r.id AND t.state='completed' AND t.cancel_epoch=0 AND
 (a.state='succeeded' OR (a.state='superseded' AND r.completion_authority='user_seal')) AND a.sealed_result_id=r.id AND
 v.state='succeeded' AND v.result_id=r.id AND v.verified_commit=r.result_commit AND
 `+resultConsumerSourcePredicate+` ORDER BY p.updated_at,p.id LIMIT 1`, workspaceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return PublicationRecord{}, &NotFoundError{Kind: kind, ID: string(workspaceID)}
	}
	if err != nil {
		return PublicationRecord{}, fmt.Errorf("find publication: %w", err)
	}
	return s.InspectPublication(ctx, id)
}

// AdmitPublication atomically claims a command receipt and prepares one
// publication from current durable ownership. No tuple authority is accepted
// from the caller and no external effect occurs in this transaction.
func (s *Store) AdmitPublication(ctx context.Context, p AdmitPublicationParams) (_ PublicationAdmission, err error) {
	if err := validateAdmitPublication(p); err != nil {
		return PublicationAdmission{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return PublicationAdmission{}, fmt.Errorf("begin publication admission: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	existing, found, err := receiptByKey(ctx, tx, p.Claim.Scope.WorkspaceID, p.Claim.Scope.CommandKind, p.Claim.Key)
	if err != nil {
		return PublicationAdmission{}, err
	}
	if found {
		existingClaim := task.IdempotencyClaim{
			Scope: task.IdempotencyScope{WorkspaceID: existing.WorkspaceID, CommandKind: existing.CommandKind},
			Key:   existing.IdempotencyKey, RequestHash: existing.RequestHash, Actor: existing.Actor,
		}
		disposition, classifyErr := task.ClassifyIdempotency(&existingClaim, p.Claim)
		if classifyErr != nil {
			return PublicationAdmission{}, fmt.Errorf("classify publication idempotency: %w", classifyErr)
		}
		switch disposition {
		case task.IdempotencyReplay:
			publication, getErr := publicationByAdmissionReceipt(ctx, tx, existing.ID)
			if getErr != nil {
				return PublicationAdmission{}, getErr
			}
			event, getErr := publicationAdmissionEvent(ctx, tx, publication.ID)
			if getErr != nil {
				return PublicationAdmission{}, getErr
			}
			if err := tx.Commit(); err != nil {
				return PublicationAdmission{}, fmt.Errorf("finish publication admission replay: %w", err)
			}
			return PublicationAdmission{Publication: publication, Receipt: existing, Event: event, Replayed: true}, nil
		case task.IdempotencyOwnerMismatch:
			return PublicationAdmission{}, ErrIdempotencyOwnerMismatch
		case task.IdempotencyConflict:
			return PublicationAdmission{}, &ConflictError{ReceiptID: existing.ID, TargetID: existing.TargetID}
		default:
			return PublicationAdmission{}, fmt.Errorf("%w: unexpected publication idempotency disposition", ErrCorruptStore)
		}
	}

	result, err := getResult(ctx, tx, p.ResultID)
	if errors.Is(err, ErrNotFound) {
		return PublicationAdmission{}, &NotFoundError{Kind: "result", ID: string(p.ResultID)}
	}
	if err != nil {
		return PublicationAdmission{}, err
	}
	if result.WorkspaceID != p.Claim.Scope.WorkspaceID {
		return PublicationAdmission{}, &NotFoundError{Kind: "result", ID: string(p.ResultID)}
	}
	owner, err := getTask(ctx, tx, result.TaskID)
	if err != nil {
		return PublicationAdmission{}, err
	}
	attempt, err := getAttempt(ctx, tx, result.AttemptID)
	if err != nil {
		return PublicationAdmission{}, err
	}
	workspace, err := scanWorkspace(tx.QueryRowContext(ctx, workspaceSelect+` WHERE id=?`, result.WorkspaceID))
	if err != nil {
		return PublicationAdmission{}, err
	}
	verification, err := getVerification(ctx, tx, p.VerificationID)
	if errors.Is(err, ErrNotFound) {
		return PublicationAdmission{}, fmt.Errorf("%w: expected verification", ErrInvalidState)
	}
	if err != nil {
		return PublicationAdmission{}, err
	}
	validAttempt := attempt.State == task.AttemptSucceeded ||
		(attempt.State == task.AttemptSuperseded && result.CompletionAuthority == SealAuthorityUser)
	if workspace.State != WorkspaceActive || workspace.GitHubAuthority != GitHubAuthorityAppBroker ||
		result.State != task.ResultSealed || result.Outcome != task.ResultChanged || result.ManifestEntries < 1 || !result.WorktreeClean ||
		owner.ID != result.TaskID || owner.WorkspaceID != result.WorkspaceID || owner.RepositoryID != result.RepositoryID ||
		owner.CurrentAttemptID != attempt.ID || owner.SealedResultID != result.ID || owner.State != task.TaskCompleted || owner.CancelEpoch != 0 ||
		attempt.ID != result.AttemptID || attempt.TaskID != owner.ID || attempt.WorkspaceID != owner.WorkspaceID ||
		attempt.SealedResultID != result.ID || !validAttempt ||
		verification.ID != p.VerificationID || verification.ResultID != result.ID || verification.TaskID != owner.ID ||
		verification.AttemptID != attempt.ID || verification.WorkspaceID != workspace.ID ||
		verification.State != VerificationSucceeded || verification.VerifiedCommit != result.ResultCommit {
		return PublicationAdmission{}, fmt.Errorf("%w: result is not eligible for publication", ErrInvalidState)
	}
	var publications int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM publications WHERE result_id=?`, result.ID).Scan(&publications); err != nil {
		return PublicationAdmission{}, fmt.Errorf("inspect existing result publication: %w", err)
	}
	if publications != 0 {
		return PublicationAdmission{}, fmt.Errorf("%w: result already has a publication", ErrInvalidState)
	}

	tuple := task.PublicationTuple{
		OperationID: p.OperationID, InstallationID: workspace.InstallationID, RepositoryID: workspace.RepositoryID,
		RepositoryFullName: workspace.RepositoryFullName, WorkspaceName: workspace.Name, BaseRef: owner.BaseRef,
		BaseSHA: owner.BaseSHA, ResultCommit: result.ResultCommit, Branch: task.PublicationBranch(workspace.Name, p.OperationID),
	}
	resultTuple := task.ResultTuple{RepositoryTuple: task.RepositoryTuple{RepositoryID: result.RepositoryID, BaseSHA: result.BaseSHA},
		ResultCommit: result.ResultCommit, Outcome: result.Outcome, ManifestEntries: result.ManifestEntries, WorktreeClean: result.WorktreeClean}
	verificationTuple := task.VerificationTuple{State: task.VerificationState(verification.State), VerifiedCommit: verification.VerifiedCommit}
	if tuple.ValidateAgainst(workspace.RepositoryID, task.RepositoryTuple{RepositoryID: owner.RepositoryID, BaseSHA: owner.BaseSHA}, resultTuple, verificationTuple) != nil {
		return PublicationAdmission{}, fmt.Errorf("%w: derived publication tuple", ErrCorruptStore)
	}

	actorID, err := ensureActor(ctx, tx, p.Claim.Actor)
	if err != nil {
		return PublicationAdmission{}, err
	}
	response, err := json.Marshal(struct {
		PublicationID  task.PublicationID  `json:"publicationId"`
		ResultID       task.ResultID       `json:"resultId"`
		VerificationID task.VerificationID `json:"verificationId"`
	}{p.PublicationID, result.ID, verification.ID})
	if err != nil {
		return PublicationAdmission{}, fmt.Errorf("encode publication receipt projection: %w", err)
	}
	acceptedMS := unixMillis(p.AcceptedAt)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO receipts(id,workspace_id,command_kind,state,idempotency_key,request_hash,actor_snapshot_id,
 accepted_at,api_contract_version,target_type,target_id,response_status,response_projection)
VALUES(?,?,?,'accepted',?,?,?,?,?,'task',?,202,?)`, p.ReceiptID, workspace.ID, PublishResultCommand,
		p.Claim.Key, p.Claim.RequestHash[:], actorID, acceptedMS, p.APIContractVersion, owner.ID, string(response)); err != nil {
		return PublicationAdmission{}, fmt.Errorf("insert publication receipt: %w", err)
	}
	evidence := json.RawMessage(`{"authority":"authenticated_http"}`)
	evidenceHash := sha256.Sum256(evidence)
	detail := struct {
		ReceiptID               task.ReceiptID        `json:"receiptId"`
		ResultID                task.ResultID         `json:"resultId"`
		VerificationID          task.VerificationID   `json:"verificationId"`
		Tuple                   task.PublicationTuple `json:"tuple"`
		BrokerPolicyVersion     string                `json:"brokerPolicyVersion"`
		BrokerPolicySHA256      string                `json:"brokerPolicySha256"`
		ExpectedTaskRevision    int64                 `json:"expectedTaskRevision"`
		ExpectedAttemptRevision int64                 `json:"expectedAttemptRevision"`
	}{p.ReceiptID, result.ID, verification.ID, tuple, p.BrokerPolicyVersion, digestString(p.BrokerPolicySHA256), owner.Revision, attempt.Revision}
	payload, err := journalPayload(detail, evidence, evidenceHash)
	if err != nil {
		return PublicationAdmission{}, err
	}
	event, err := insertJournalEvent(ctx, tx, p.EventID, "publication", string(p.PublicationID), "publication.prepared", "",
		string(PublicationPrepared), 1, p.AcceptedAt, actorID, result, evidenceHash, payload)
	if err != nil {
		return PublicationAdmission{}, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO publications(id,operation_id,admission_receipt_id,result_id,verification_id,task_id,attempt_id,workspace_id,state,effect_phase,
 installation_id,repository_id,repository_full_name,base_ref,base_sha,result_commit,branch,expected_remote_old_sha,
 broker_policy_version,broker_policy_sha256,latest_event_id,requester_actor_snapshot_id,revision,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,'prepared','none',?,?,?,?,?,?,?,NULL,?,?,?,?,1,?,?)`, p.PublicationID, p.OperationID, p.ReceiptID,
		result.ID, verification.ID, owner.ID, attempt.ID, workspace.ID, tuple.InstallationID, tuple.RepositoryID,
		tuple.RepositoryFullName, tuple.BaseRef, tuple.BaseSHA, tuple.ResultCommit, tuple.Branch,
		p.BrokerPolicyVersion, p.BrokerPolicySHA256[:], event.ID, actorID, acceptedMS, acceptedMS); err != nil {
		return PublicationAdmission{}, fmt.Errorf("insert admitted publication: %w", err)
	}
	publication, err := getPublication(ctx, tx, p.PublicationID)
	if err != nil {
		return PublicationAdmission{}, err
	}
	if err := tx.Commit(); err != nil {
		return PublicationAdmission{}, fmt.Errorf("commit publication admission: %w", err)
	}
	receipt := Receipt{ID: p.ReceiptID, WorkspaceID: workspace.ID, CommandKind: PublishResultCommand, State: ReceiptAccepted,
		IdempotencyKey: p.Claim.Key, RequestHash: p.Claim.RequestHash, Actor: p.Claim.Actor, AcceptedAt: fromUnixMillis(acceptedMS),
		APIContractVersion: p.APIContractVersion, TargetType: "task", TargetID: owner.ID, ResponseStatus: 202, ResponseProjection: response}
	return PublicationAdmission{Publication: publication, Receipt: receipt, Event: event}, nil
}

func publicationByAdmissionReceipt(ctx context.Context, q queryRower, receiptID task.ReceiptID) (Publication, error) {
	var id task.PublicationID
	if err := q.QueryRowContext(ctx, `SELECT id FROM publications WHERE admission_receipt_id=?`, receiptID).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return Publication{}, ErrCorruptStore
	} else if err != nil {
		return Publication{}, fmt.Errorf("read receipt publication: %w", err)
	}
	publication, err := getPublication(ctx, q, id)
	if err != nil || publication.AdmissionReceiptID != receiptID {
		return Publication{}, fmt.Errorf("%w: publication receipt link", ErrCorruptStore)
	}
	return publication, nil
}

func publicationAdmissionEvent(ctx context.Context, q queryRower, id task.PublicationID) (JournalEvent, error) {
	event, err := scanJournalEvent(q.QueryRowContext(ctx, journalEventSelect+`
 WHERE e.entity_type='publication' AND e.entity_id=? AND e.entity_revision=1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return JournalEvent{}, ErrCorruptStore
	}
	if err != nil {
		return JournalEvent{}, fmt.Errorf("read publication admission event: %w", err)
	}
	if event.Type != "publication.prepared" || event.FromState != "" || event.ToState != string(PublicationPrepared) {
		return JournalEvent{}, fmt.Errorf("%w: publication admission event", ErrCorruptStore)
	}
	return event, nil
}

func validateAdmitPublication(p AdmitPublicationParams) error {
	if _, err := task.ParsePublicationID(string(p.PublicationID)); err != nil {
		return fmt.Errorf("%w: publication ID", ErrInvalidInput)
	}
	if _, err := task.ParsePublicationOperationID(string(p.OperationID)); err != nil {
		return fmt.Errorf("%w: publication operation ID", ErrInvalidInput)
	}
	if _, err := task.ParseReceiptID(string(p.ReceiptID)); err != nil {
		return fmt.Errorf("%w: publication receipt ID", ErrInvalidInput)
	}
	if _, err := task.ParseEventID(string(p.EventID)); err != nil {
		return fmt.Errorf("%w: publication event ID", ErrInvalidInput)
	}
	if _, err := task.ParseResultID(string(p.ResultID)); err != nil {
		return fmt.Errorf("%w: result ID", ErrInvalidInput)
	}
	if _, err := task.ParseVerificationID(string(p.VerificationID)); err != nil {
		return fmt.Errorf("%w: verification ID", ErrInvalidInput)
	}
	if err := p.Claim.Validate(); err != nil || p.Claim.Scope.CommandKind != PublishResultCommand {
		return fmt.Errorf("%w: publication idempotency claim", ErrInvalidInput)
	}
	if !validBoundedText(p.BrokerPolicyVersion, 1, 128) || !validBoundedText(p.APIContractVersion, 1, 64) {
		return fmt.Errorf("%w: publication policy or API contract", ErrInvalidInput)
	}
	if err := validExactTimestamp(p.AcceptedAt); err != nil {
		return err
	}
	return nil
}

func (s *Store) PreparePublication(ctx context.Context, p PreparePublicationParams) (_ PublicationRecord, err error) {
	if err := validatePreparePublication(p); err != nil {
		return PublicationRecord{}, err
	}
	detail := struct {
		ResultID            task.ResultID         `json:"resultId"`
		VerificationID      task.VerificationID   `json:"verificationId"`
		Tuple               task.PublicationTuple `json:"tuple"`
		BrokerPolicyVersion string                `json:"brokerPolicyVersion"`
		BrokerPolicySHA256  string                `json:"brokerPolicySha256"`
		TaskRevision        int64                 `json:"expectedTaskRevision"`
		AttemptRevision     int64                 `json:"expectedAttemptRevision"`
	}{p.ResultID, p.VerificationID, p.Tuple, p.BrokerPolicyVersion, digestString(p.BrokerPolicySHA256),
		p.ExpectedTaskRevision, p.ExpectedAttemptRevision}
	payload, err := journalPayload(detail, p.EvidencePayload, p.EvidenceSHA256)
	if err != nil {
		return PublicationRecord{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return PublicationRecord{}, err
	}
	defer release()
	defer rollback(tx, &err)
	if existing, readErr := getPublication(ctx, tx, p.PublicationID); readErr == nil {
		return finishPublicationReplay(ctx, tx, existing, p.EventID, "publication.prepared", "", string(PublicationPrepared),
			1, p.PreparedAt, p.Actor, payload)
	} else if !errors.Is(readErr, ErrNotFound) {
		return PublicationRecord{}, readErr
	}
	result, owner, _, err := journalSource(ctx, tx, p.ResultID, p.ExpectedTaskRevision, p.ExpectedAttemptRevision)
	if err != nil {
		return PublicationRecord{}, err
	}
	verification, err := getVerification(ctx, tx, p.VerificationID)
	if err != nil {
		return PublicationRecord{}, err
	}
	workspace, err := scanWorkspace(tx.QueryRowContext(ctx, workspaceSelect+` WHERE id=?`, result.WorkspaceID))
	if err != nil {
		return PublicationRecord{}, err
	}
	resultTuple := task.ResultTuple{RepositoryTuple: task.RepositoryTuple{RepositoryID: result.RepositoryID, BaseSHA: result.BaseSHA},
		ResultCommit: result.ResultCommit, Outcome: result.Outcome, ManifestEntries: result.ManifestEntries, WorktreeClean: result.WorktreeClean}
	verificationTuple := task.VerificationTuple{State: task.VerificationState(verification.State), VerifiedCommit: verification.VerifiedCommit}
	if workspace.State != WorkspaceActive || workspace.GitHubAuthority != GitHubAuthorityAppBroker || owner.BaseRef != p.Tuple.BaseRef ||
		p.Tuple.ValidateAgainst(workspace.RepositoryID, task.RepositoryTuple{RepositoryID: owner.RepositoryID, BaseSHA: owner.BaseSHA}, resultTuple, verificationTuple) != nil ||
		p.Tuple.InstallationID != workspace.InstallationID || p.Tuple.RepositoryFullName != workspace.RepositoryFullName {
		return PublicationRecord{}, fmt.Errorf("%w: publication tuple", ErrInvalidState)
	}
	actorID, err := ensureActor(ctx, tx, p.Actor)
	if err != nil {
		return PublicationRecord{}, err
	}
	event, err := insertJournalEvent(ctx, tx, p.EventID, "publication", string(p.PublicationID), "publication.prepared", "",
		string(PublicationPrepared), 1, p.PreparedAt, actorID, result, p.EvidenceSHA256, payload)
	if err != nil {
		return PublicationRecord{}, err
	}
	var expectedOld any
	if p.Tuple.ExpectedRemoteOldSHA != "" {
		expectedOld = p.Tuple.ExpectedRemoteOldSHA
	}
	at := unixMillis(p.PreparedAt)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO publications(id,operation_id,result_id,verification_id,task_id,attempt_id,workspace_id,state,effect_phase,
 installation_id,repository_id,repository_full_name,base_ref,base_sha,result_commit,branch,expected_remote_old_sha,
 broker_policy_version,broker_policy_sha256,latest_event_id,requester_actor_snapshot_id,revision,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,'prepared','none',?,?,?,?,?,?,?,?,?,?,?,?,1,?,?)`, p.PublicationID, p.Tuple.OperationID, result.ID,
		verification.ID, result.TaskID, result.AttemptID, result.WorkspaceID, p.Tuple.InstallationID, p.Tuple.RepositoryID,
		p.Tuple.RepositoryFullName, p.Tuple.BaseRef, p.Tuple.BaseSHA, p.Tuple.ResultCommit, p.Tuple.Branch, expectedOld,
		p.BrokerPolicyVersion, p.BrokerPolicySHA256[:], event.ID, actorID, at, at); err != nil {
		return PublicationRecord{}, fmt.Errorf("prepare publication: %w", err)
	}
	return finishPublication(ctx, tx, p.PublicationID, event)
}

func (s *Store) AdvancePublication(ctx context.Context, p AdvancePublicationParams) (_ PublicationRecord, err error) {
	if err := validateJournalTransition(p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision, p.EventID,
		p.AdvancedAt, p.EvidencePayload, p.EvidenceSHA256, p.Actor); err != nil {
		return PublicationRecord{}, err
	}
	validEdge := (p.From == PublicationPhaseNone && p.To == PublicationPhasePushStarted && p.ObservedRemoteSHA == "") ||
		(p.From == PublicationPhasePushStarted && p.To == PublicationPhasePushObserved) ||
		(p.From == PublicationPhasePushObserved && p.To == PublicationPhasePRCreateStarted && p.ObservedRemoteSHA == "")
	if !validEdge || (p.To == PublicationPhasePushObserved && p.ObservedRemoteSHA == "") {
		return PublicationRecord{}, fmt.Errorf("%w: publication phase edge", ErrInvalidInput)
	}
	if p.ObservedRemoteSHA != "" {
		if _, err := task.ParseGitOID(string(p.ObservedRemoteSHA)); err != nil {
			return PublicationRecord{}, fmt.Errorf("%w: observed remote SHA", ErrInvalidInput)
		}
	}
	detail := struct {
		From                    PublicationPhase `json:"from"`
		To                      PublicationPhase `json:"to"`
		ObservedRemoteSHA       task.GitOID      `json:"observedRemoteSha,omitempty"`
		ExpectedRevision        int64            `json:"expectedRevision"`
		ExpectedTaskRevision    int64            `json:"expectedTaskRevision"`
		ExpectedAttemptRevision int64            `json:"expectedAttemptRevision"`
	}{p.From, p.To, p.ObservedRemoteSHA, p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision}
	payload, err := journalPayload(detail, p.EvidencePayload, p.EvidenceSHA256)
	if err != nil {
		return PublicationRecord{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return PublicationRecord{}, err
	}
	defer release()
	defer rollback(tx, &err)
	publication, err := getPublication(ctx, tx, p.PublicationID)
	if err != nil {
		return PublicationRecord{}, err
	}
	if publication.Revision == p.ExpectedRevision+1 && publication.State == PublicationRunning && publication.EffectPhase == p.To {
		e, eventErr := getJournalEvent(ctx, tx, p.EventID)
		if eventErr != nil {
			return PublicationRecord{}, eventErr
		}
		return finishPublicationReplay(ctx, tx, publication, p.EventID, "publication.phase_advanced", e.FromState,
			string(PublicationRunning), publication.Revision, p.AdvancedAt, p.Actor, payload)
	}
	if err := checkPublicationSource(ctx, tx, publication, p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision); err != nil {
		return PublicationRecord{}, err
	}
	if publication.EffectPhase != p.From || (p.From == PublicationPhaseNone && publication.State != PublicationPrepared) ||
		(p.From != PublicationPhaseNone && publication.State != PublicationRunning && publication.State != PublicationUncertain) {
		return PublicationRecord{}, fmt.Errorf("%w: publication phase/state differs", ErrInvalidState)
	}
	if p.To == PublicationPhasePushObserved && p.ObservedRemoteSHA != publication.Tuple.ResultCommit {
		return PublicationRecord{}, fmt.Errorf("%w: pushed remote SHA differs from result", ErrInvalidState)
	}
	if p.AdvancedAt.Before(publication.UpdatedAt) {
		return PublicationRecord{}, fmt.Errorf("%w: publication timestamp regressed", ErrInvalidInput)
	}
	actorID, err := ensureActor(ctx, tx, p.Actor)
	if err != nil {
		return PublicationRecord{}, err
	}
	result, _ := getResult(ctx, tx, publication.ResultID)
	event, err := insertJournalEvent(ctx, tx, p.EventID, "publication", string(publication.ID), "publication.phase_advanced",
		string(publication.State), string(PublicationRunning), publication.Revision+1, p.AdvancedAt, actorID, result, p.EvidenceSHA256, payload)
	if err != nil {
		return PublicationRecord{}, err
	}
	var remote any
	if p.ObservedRemoteSHA != "" {
		remote = p.ObservedRemoteSHA
	} else if publication.ObservedRemoteSHA != "" {
		remote = publication.ObservedRemoteSHA
	}
	if _, err := tx.ExecContext(ctx, `UPDATE publications SET state='running',effect_phase=?,observed_remote_sha=?,reason=NULL,
latest_event_id=?,revision=revision+1,updated_at=? WHERE id=? AND state=? AND effect_phase=? AND revision=?`, p.To, remote,
		event.ID, unixMillis(p.AdvancedAt), publication.ID, publication.State, p.From, p.ExpectedRevision); err != nil {
		return PublicationRecord{}, fmt.Errorf("advance publication phase: %w", err)
	}
	return finishPublication(ctx, tx, publication.ID, event)
}

func (s *Store) CompletePublication(ctx context.Context, p CompletePublicationParams) (_ PublicationRecord, err error) {
	if err := validateJournalTransition(p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision, p.EventID,
		p.CompletedAt, p.EvidencePayload, p.EvidenceSHA256, p.Actor); err != nil {
		return PublicationRecord{}, err
	}
	detail := struct {
		Observation             task.PublicationObservation `json:"observation"`
		ExpectedRevision        int64                       `json:"expectedRevision"`
		ExpectedTaskRevision    int64                       `json:"expectedTaskRevision"`
		ExpectedAttemptRevision int64                       `json:"expectedAttemptRevision"`
	}{p.Observation, p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision}
	payload, err := journalPayload(detail, p.EvidencePayload, p.EvidenceSHA256)
	if err != nil {
		return PublicationRecord{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return PublicationRecord{}, err
	}
	defer release()
	defer rollback(tx, &err)
	publication, err := getPublication(ctx, tx, p.PublicationID)
	if err != nil {
		return PublicationRecord{}, err
	}
	if publication.Revision == p.ExpectedRevision+1 && publication.State == PublicationPublished {
		e, eventErr := getJournalEvent(ctx, tx, p.EventID)
		if eventErr != nil {
			return PublicationRecord{}, eventErr
		}
		return finishPublicationReplay(ctx, tx, publication, p.EventID, "publication.published", e.FromState,
			string(PublicationPublished), publication.Revision, p.CompletedAt, p.Actor, payload)
	}
	if err := checkPublicationSource(ctx, tx, publication, p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision); err != nil {
		return PublicationRecord{}, err
	}
	if publication.State != PublicationRunning && publication.State != PublicationUncertain {
		return PublicationRecord{}, fmt.Errorf("%w: publication cannot complete from %s", ErrInvalidState, publication.State)
	}
	if publication.EffectPhase != PublicationPhasePushObserved && publication.EffectPhase != PublicationPhasePRCreateStarted {
		return PublicationRecord{}, fmt.Errorf("%w: publication effect phase lacks push proof", ErrInvalidState)
	}
	if err := p.Observation.ValidateAgainst(publication.Tuple); err != nil {
		return PublicationRecord{}, fmt.Errorf("%w: publication observation: %v", ErrInvalidInput, err)
	}
	actorID, err := ensureActor(ctx, tx, p.Actor)
	if err != nil {
		return PublicationRecord{}, err
	}
	result, _ := getResult(ctx, tx, publication.ResultID)
	event, err := insertJournalEvent(ctx, tx, p.EventID, "publication", string(publication.ID), "publication.published",
		string(publication.State), string(PublicationPublished), publication.Revision+1, p.CompletedAt, actorID, result, p.EvidenceSHA256, payload)
	if err != nil {
		return PublicationRecord{}, err
	}
	pr := p.Observation.PullRequest
	if _, err := tx.ExecContext(ctx, `UPDATE publications SET state='published',observed_remote_sha=?,pr_number=?,pr_url=?,pr_state=?,pr_draft=?,
pr_repository_id=?,pr_repository_full_name=?,pr_base_repository_id=?,pr_base_repository_full_name=?,pr_base_ref=?,pr_base_sha=?,
pr_head_repository_id=?,pr_head_repository_full_name=?,pr_head_repository_owner=?,pr_head_repository_name=?,pr_head_ref=?,pr_head_sha=?,
reason=NULL,latest_event_id=?,revision=revision+1,updated_at=? WHERE id=? AND state=? AND revision=?`,
		p.Observation.RemoteSHA, pr.Number, pr.URL, pr.State, boolInt(pr.Draft), pr.RepositoryID, pr.RepositoryFullName,
		pr.BaseRepositoryID, pr.BaseRepositoryFullName, pr.BaseRef, pr.BaseSHA, pr.HeadRepositoryID, pr.HeadRepositoryFullName,
		pr.HeadRepositoryOwner, pr.HeadRepositoryName, pr.HeadRef, pr.HeadSHA, event.ID, unixMillis(p.CompletedAt),
		publication.ID, publication.State, p.ExpectedRevision); err != nil {
		return PublicationRecord{}, fmt.Errorf("complete publication: %w", err)
	}
	return finishPublication(ctx, tx, publication.ID, event)
}

func (s *Store) RecoverPublication(ctx context.Context, p RecoverPublicationParams) (_ PublicationRecord, err error) {
	if err := validateJournalTransition(p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision, p.EventID,
		p.RecoveredAt, p.EvidencePayload, p.EvidenceSHA256, p.Actor); err != nil {
		return PublicationRecord{}, err
	}
	if p.State != PublicationUncertain && p.State != PublicationRecoveryRequired && p.State != PublicationFailed && p.State != PublicationConflict {
		return PublicationRecord{}, fmt.Errorf("%w: publication recovery state", ErrInvalidInput)
	}
	if !validBoundedText(p.Reason, 1, 1000) {
		return PublicationRecord{}, fmt.Errorf("%w: publication recovery reason", ErrInvalidInput)
	}
	detail := struct {
		State                   PublicationState `json:"state"`
		Reason                  string           `json:"reason,omitempty"`
		ExpectedRevision        int64            `json:"expectedRevision"`
		ExpectedTaskRevision    int64            `json:"expectedTaskRevision"`
		ExpectedAttemptRevision int64            `json:"expectedAttemptRevision"`
	}{p.State, p.Reason, p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision}
	payload, err := journalPayload(detail, p.EvidencePayload, p.EvidenceSHA256)
	if err != nil {
		return PublicationRecord{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return PublicationRecord{}, err
	}
	defer release()
	defer rollback(tx, &err)
	publication, err := getPublication(ctx, tx, p.PublicationID)
	if err != nil {
		return PublicationRecord{}, err
	}
	if publication.Revision == p.ExpectedRevision+1 && publication.State == p.State {
		e, eventErr := getJournalEvent(ctx, tx, p.EventID)
		if eventErr != nil {
			return PublicationRecord{}, eventErr
		}
		return finishPublicationReplay(ctx, tx, publication, p.EventID, "publication."+string(p.State), e.FromState,
			string(p.State), publication.Revision, p.RecoveredAt, p.Actor, payload)
	}
	if err := checkPublicationSource(ctx, tx, publication, p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision); err != nil {
		return PublicationRecord{}, err
	}
	if publication.State != PublicationPrepared && publication.State != PublicationRunning && publication.State != PublicationUncertain {
		return PublicationRecord{}, fmt.Errorf("%w: publication cannot recover from %s", ErrInvalidState, publication.State)
	}
	if publication.State == PublicationPrepared && p.State == PublicationUncertain {
		return PublicationRecord{}, fmt.Errorf("%w: no publication mutation can be uncertain before push", ErrInvalidState)
	}
	actorID, err := ensureActor(ctx, tx, p.Actor)
	if err != nil {
		return PublicationRecord{}, err
	}
	result, _ := getResult(ctx, tx, publication.ResultID)
	event, err := insertJournalEvent(ctx, tx, p.EventID, "publication", string(publication.ID), "publication."+string(p.State),
		string(publication.State), string(p.State), publication.Revision+1, p.RecoveredAt, actorID, result, p.EvidenceSHA256, payload)
	if err != nil {
		return PublicationRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE publications SET state=?,reason=?,latest_event_id=?,revision=revision+1,updated_at=?
WHERE id=? AND state=? AND revision=?`, p.State, p.Reason, event.ID, unixMillis(p.RecoveredAt), publication.ID,
		publication.State, p.ExpectedRevision); err != nil {
		return PublicationRecord{}, fmt.Errorf("recover publication: %w", err)
	}
	return finishPublication(ctx, tx, publication.ID, event)
}

func checkPublicationSource(ctx context.Context, q queryRower, p Publication, expected, taskRevision, attemptRevision int64) error {
	if p.Revision != expected {
		return &StaleJournalRevisionError{Kind: "publication", ID: string(p.ID), Expected: expected, Actual: p.Revision}
	}
	result, _, _, err := journalSource(ctx, q, p.ResultID, taskRevision, attemptRevision)
	if err != nil {
		return err
	}
	verification, err := getVerification(ctx, q, p.VerificationID)
	if err != nil {
		return err
	}
	if verification.State != VerificationSucceeded || verification.ResultID != result.ID || verification.VerifiedCommit != result.ResultCommit ||
		result.ResultCommit != p.Tuple.ResultCommit || result.RepositoryID != p.Tuple.RepositoryID || result.BaseSHA != p.Tuple.BaseSHA {
		return fmt.Errorf("%w: publication source tuple differs", ErrInvalidState)
	}
	return nil
}

func finishPublication(ctx context.Context, tx *sql.Tx, id task.PublicationID, event JournalEvent) (PublicationRecord, error) {
	p, err := getPublication(ctx, tx, id)
	if err != nil {
		return PublicationRecord{}, err
	}
	if p.LatestEventID != event.ID || p.Revision != event.EntityRevision {
		return PublicationRecord{}, fmt.Errorf("%w: publication event pairing", ErrCorruptStore)
	}
	if err := tx.Commit(); err != nil {
		return PublicationRecord{}, fmt.Errorf("commit publication: %w", err)
	}
	return PublicationRecord{Publication: p, Event: event}, nil
}

func finishPublicationReplay(ctx context.Context, tx *sql.Tx, p Publication, eventID task.EventID, eventType, from, to string,
	revision int64, at time.Time, actor task.ActorSnapshot, payload []byte,
) (PublicationRecord, error) {
	e, err := exactJournalEvent(ctx, tx, eventID, p.LatestEventID, "publication", string(p.ID), eventType, from, to, revision, at, actor, payload)
	if err != nil {
		return PublicationRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return PublicationRecord{}, err
	}
	return PublicationRecord{Publication: p, Event: e, Replayed: true}, nil
}

func getPublication(ctx context.Context, q queryRower, id task.PublicationID) (Publication, error) {
	p, err := scanPublication(q.QueryRowContext(ctx, publicationSelect+` WHERE p.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Publication{}, ErrNotFound
	}
	if err != nil {
		return Publication{}, fmt.Errorf("read publication: %w", err)
	}
	return p, nil
}

func scanPublication(row rowScanner) (Publication, error) {
	var p Publication
	var installationID, repositoryID int64
	var admissionReceiptID, expectedOld, remote, reason sql.NullString
	var policyHash []byte
	var number, draft, prRepoID, baseRepoID, headRepoID sql.NullInt64
	var url, state, prRepoName, baseRepoName, baseRef, baseSHA, headRepoName, headOwner, headName, headRef, headSHA sql.NullString
	var created, updated int64
	if err := row.Scan(&p.ID, &p.OperationID, &admissionReceiptID, &p.ResultID, &p.VerificationID, &p.TaskID, &p.AttemptID, &p.WorkspaceID,
		&p.State, &p.EffectPhase, &installationID, &repositoryID, &p.Tuple.RepositoryFullName, &p.Tuple.BaseRef, &p.Tuple.BaseSHA,
		&p.Tuple.ResultCommit, &p.Tuple.Branch, &expectedOld, &p.BrokerPolicyVersion, &policyHash, &remote, &number, &url, &state,
		&draft, &prRepoID, &prRepoName, &baseRepoID, &baseRepoName, &baseRef, &baseSHA, &headRepoID, &headRepoName, &headOwner,
		&headName, &headRef, &headSHA, &reason, &p.LatestEventID, &p.Revision, &created, &updated,
		&p.Requester.Type, &p.Requester.ID, &p.Requester.DisplayName, &p.Requester.CredentialID, &p.Requester.Authentication, &p.Requester.RequestID); err != nil {
		return Publication{}, err
	}
	if installationID <= 0 || repositoryID <= 0 || len(policyHash) != sha256.Size {
		return Publication{}, ErrCorruptStore
	}
	p.Tuple.OperationID, p.Tuple.InstallationID, p.Tuple.RepositoryID = p.OperationID, task.InstallationID(installationID), task.RepositoryID(repositoryID)
	if admissionReceiptID.Valid {
		p.AdmissionReceiptID = task.ReceiptID(admissionReceiptID.String)
	}
	p.Tuple.ExpectedRemoteOldSHA = task.GitOID(expectedOld.String)
	p.ObservedRemoteSHA, p.Reason = task.GitOID(remote.String), reason.String
	copy(p.BrokerPolicySHA256[:], policyHash)
	if number.Valid {
		observation := task.PublicationObservation{RemoteSHA: p.ObservedRemoteSHA, PullRequest: task.PullRequestObservation{
			RepositoryID: task.RepositoryID(prRepoID.Int64), RepositoryFullName: prRepoName.String,
			Number: task.PullRequestNumber(number.Int64), URL: url.String, State: state.String, Draft: draft.Int64 == 1,
			BaseRepositoryID: task.RepositoryID(baseRepoID.Int64), BaseRepositoryFullName: baseRepoName.String,
			BaseRef: baseRef.String, BaseSHA: task.GitOID(baseSHA.String), HeadRepositoryID: task.RepositoryID(headRepoID.Int64),
			HeadRepositoryFullName: headRepoName.String, HeadRepositoryOwner: headOwner.String, HeadRepositoryName: headName.String,
			HeadRef: headRef.String, HeadSHA: task.GitOID(headSHA.String),
		}}
		p.Observation = &observation
	}
	p.CreatedAt, p.UpdatedAt = fromUnixMillis(created), fromUnixMillis(updated)
	return p, nil
}

func validatePreparePublication(p PreparePublicationParams) error {
	if _, err := task.ParsePublicationID(string(p.PublicationID)); err != nil {
		return fmt.Errorf("%w: publication ID", ErrInvalidInput)
	}
	if _, err := task.ParseResultID(string(p.ResultID)); err != nil {
		return fmt.Errorf("%w: result ID", ErrInvalidInput)
	}
	if _, err := task.ParseVerificationID(string(p.VerificationID)); err != nil {
		return fmt.Errorf("%w: verification ID", ErrInvalidInput)
	}
	if p.ExpectedTaskRevision < 1 || p.ExpectedAttemptRevision < 1 || !validBoundedText(p.BrokerPolicyVersion, 1, 128) {
		return fmt.Errorf("%w: publication policy or revisions", ErrInvalidInput)
	}
	if err := validExactTimestamp(p.PreparedAt); err != nil {
		return err
	}
	if err := p.Actor.Validate(); err != nil {
		return fmt.Errorf("%w: publication requester", ErrInvalidInput)
	}
	if _, err := task.ParseEventID(string(p.EventID)); err != nil {
		return fmt.Errorf("%w: publication event ID", ErrInvalidInput)
	}
	return validateDeliveryEvidence(p.EvidencePayload, p.EvidenceSHA256)
}
