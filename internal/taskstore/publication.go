package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nebler/fern/internal/task"
)

const publicationSelect = `
SELECT p.id,p.operation_id,p.result_id,p.verification_id,p.task_id,p.attempt_id,p.workspace_id,p.state,p.effect_phase,
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
	return s.findPublication(ctx, workspaceID, `p.state='prepared' AND p.effect_phase='none'`, "prepared publication")
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
WHERE p.workspace_id=? AND p.state IN ('prepared','running','uncertain') AND r.state='sealed' AND
 t.current_attempt_id=a.id AND t.sealed_result_id=r.id AND t.state='completed' AND t.cancel_epoch=0 AND
 a.state='succeeded' AND a.sealed_result_id=r.id AND v.state='succeeded' AND v.result_id=r.id AND
 v.verified_commit=r.result_commit ORDER BY p.updated_at,p.id LIMIT 1`, workspaceID).Scan(&id)
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
	return s.findPublication(ctx, workspaceID, `p.state='uncertain'`, "uncertain publication")
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
 t.sealed_result_id=r.id AND t.state='completed' AND t.cancel_epoch=0 AND a.state='succeeded' AND a.sealed_result_id=r.id AND
 v.state='succeeded' AND v.result_id=r.id AND v.verified_commit=r.result_commit ORDER BY p.updated_at,p.id LIMIT 1`, workspaceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return PublicationRecord{}, &NotFoundError{Kind: kind, ID: string(workspaceID)}
	}
	if err != nil {
		return PublicationRecord{}, fmt.Errorf("find publication: %w", err)
	}
	return s.InspectPublication(ctx, id)
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
	if workspace.State != WorkspaceActive || owner.BaseRef != p.Tuple.BaseRef ||
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
	var expectedOld, remote, reason sql.NullString
	var policyHash []byte
	var number, draft, prRepoID, baseRepoID, headRepoID sql.NullInt64
	var url, state, prRepoName, baseRepoName, baseRef, baseSHA, headRepoName, headOwner, headName, headRef, headSHA sql.NullString
	var created, updated int64
	if err := row.Scan(&p.ID, &p.OperationID, &p.ResultID, &p.VerificationID, &p.TaskID, &p.AttemptID, &p.WorkspaceID,
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
