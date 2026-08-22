package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/nebler/fern/internal/task"
)

const verificationSelect = `
SELECT id,result_id,task_id,attempt_id,workspace_id,state,policy_name,policy_sha256,verified_commit,
 working_directory,timeout_millis,output_limit_bytes,runner_name,runner_version,image_digest,environment_sha256,
 effect_attempt,started_at,ended_at,outcome,exit_code,signal,
 stdout_byte_count,stdout_retained_bytes,stdout_sha256,stdout_truncated,
 stderr_byte_count,stderr_retained_bytes,stderr_sha256,stderr_truncated,
 reason,latest_event_id,revision,created_at,updated_at
FROM verifications`

func (s *Store) GetVerification(ctx context.Context, id task.VerificationID) (Verification, error) {
	if _, err := task.ParseVerificationID(string(id)); err != nil {
		return Verification{}, fmt.Errorf("%w: verification ID", ErrInvalidInput)
	}
	return getVerification(ctx, s.db, id)
}

func (s *Store) InspectVerification(ctx context.Context, id task.VerificationID) (VerificationRecord, error) {
	v, err := s.GetVerification(ctx, id)
	if err != nil {
		return VerificationRecord{}, err
	}
	e, err := getJournalEvent(ctx, s.db, v.LatestEventID)
	if err != nil {
		return VerificationRecord{}, err
	}
	return VerificationRecord{Verification: v, Event: e}, nil
}

func (s *Store) FindPreparedVerification(ctx context.Context, workspaceID task.WorkspaceID) (VerificationRecord, error) {
	return s.findVerification(ctx, workspaceID, VerificationPrepared)
}

func (s *Store) FindRunningVerification(ctx context.Context, workspaceID task.WorkspaceID) (VerificationRecord, error) {
	return s.findVerification(ctx, workspaceID, VerificationRunning)
}

// FindResultAwaitingVerification returns one exact sealed result and its
// current task/attempt owners only when no verification of any state exists.
// PrepareVerification rechecks ownership and revisions before insertion.
func (s *Store) FindResultAwaitingVerification(ctx context.Context, workspaceID task.WorkspaceID) (VerificationSource, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil {
		return VerificationSource{}, fmt.Errorf("%w: workspace ID", ErrInvalidInput)
	}
	var resultID task.ResultID
	err := s.db.QueryRowContext(ctx, `
SELECT r.id FROM results r
JOIN tasks t ON t.id=r.task_id AND t.workspace_id=r.workspace_id
JOIN attempts a ON a.id=r.attempt_id AND a.task_id=r.task_id AND a.workspace_id=r.workspace_id
WHERE r.workspace_id=? AND r.state='sealed' AND t.current_attempt_id=a.id AND t.sealed_result_id=r.id AND
 t.state='completed' AND t.cancel_epoch=0 AND a.state='succeeded' AND a.sealed_result_id=r.id AND
 NOT EXISTS (SELECT 1 FROM verifications v WHERE v.result_id=r.id)
ORDER BY r.updated_at,r.id LIMIT 1`, workspaceID).Scan(&resultID)
	if errors.Is(err, sql.ErrNoRows) {
		return VerificationSource{}, &NotFoundError{Kind: "result awaiting verification", ID: string(workspaceID)}
	}
	if err != nil {
		return VerificationSource{}, fmt.Errorf("find result awaiting verification: %w", err)
	}
	result, err := getResult(ctx, s.db, resultID)
	if err != nil {
		return VerificationSource{}, err
	}
	owner, err := getTask(ctx, s.db, result.TaskID)
	if err != nil {
		return VerificationSource{}, err
	}
	attempt, err := getAttempt(ctx, s.db, result.AttemptID)
	if err != nil {
		return VerificationSource{}, err
	}
	if owner.CurrentAttemptID != attempt.ID || owner.SealedResultID != result.ID || attempt.SealedResultID != result.ID {
		return VerificationSource{}, fmt.Errorf("%w: verification source ownership changed", ErrInvalidState)
	}
	return VerificationSource{Result: result, Task: owner, Attempt: attempt}, nil
}

func (s *Store) findVerification(ctx context.Context, workspaceID task.WorkspaceID, state VerificationState) (VerificationRecord, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil {
		return VerificationRecord{}, fmt.Errorf("%w: workspace ID", ErrInvalidInput)
	}
	var id task.VerificationID
	err := s.db.QueryRowContext(ctx, `
SELECT v.id FROM verifications v
JOIN results r ON r.id=v.result_id JOIN tasks t ON t.id=v.task_id JOIN attempts a ON a.id=v.attempt_id
WHERE v.workspace_id=? AND v.state=? AND r.state='sealed' AND r.result_commit=v.verified_commit AND
 t.current_attempt_id=a.id AND t.sealed_result_id=r.id AND t.state='completed' AND t.cancel_epoch=0 AND
 a.state='succeeded' AND a.sealed_result_id=r.id ORDER BY v.updated_at,v.id LIMIT 1`, workspaceID, state).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return VerificationRecord{}, &NotFoundError{Kind: string(state) + " verification", ID: string(workspaceID)}
	}
	if err != nil {
		return VerificationRecord{}, fmt.Errorf("find verification: %w", err)
	}
	return s.InspectVerification(ctx, id)
}

func (s *Store) PrepareVerification(ctx context.Context, p PrepareVerificationParams) (_ VerificationRecord, err error) {
	if err := validatePrepareVerification(p); err != nil {
		return VerificationRecord{}, err
	}
	detail := struct {
		ResultID          task.ResultID `json:"resultId"`
		PolicyName        string        `json:"policyName"`
		PolicySHA256      string        `json:"policySha256"`
		VerifiedCommit    task.GitOID   `json:"verifiedCommit"`
		WorkingDirectory  string        `json:"workingDirectory"`
		TimeoutMillis     int64         `json:"timeoutMillis"`
		OutputLimitBytes  int64         `json:"outputLimitBytes"`
		RunnerName        string        `json:"runnerName"`
		RunnerVersion     string        `json:"runnerVersion"`
		ImageDigest       string        `json:"imageDigest"`
		EnvironmentSHA256 string        `json:"environmentSha256"`
		TaskRevision      int64         `json:"expectedTaskRevision"`
		AttemptRevision   int64         `json:"expectedAttemptRevision"`
	}{p.ResultID, p.PolicyName, digestString(p.PolicySHA256), p.VerifiedCommit, p.WorkingDirectory,
		p.Timeout.Milliseconds(), p.OutputLimitBytes, p.RunnerName, p.RunnerVersion, p.ImageDigest,
		digestString(p.EnvironmentSHA256), p.ExpectedTaskRevision, p.ExpectedAttemptRevision}
	payload, err := journalPayload(detail, p.EvidencePayload, p.EvidenceSHA256)
	if err != nil {
		return VerificationRecord{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return VerificationRecord{}, fmt.Errorf("begin verification preparation: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	if existing, readErr := getVerification(ctx, tx, p.VerificationID); readErr == nil {
		return finishVerificationReplay(ctx, tx, existing, p.EventID, "verification.prepared", "", string(VerificationPrepared),
			1, p.PreparedAt, p.Actor, payload)
	} else if !errors.Is(readErr, ErrNotFound) {
		return VerificationRecord{}, readErr
	}
	result, _, _, err := journalSource(ctx, tx, p.ResultID, p.ExpectedTaskRevision, p.ExpectedAttemptRevision)
	if err != nil {
		return VerificationRecord{}, err
	}
	if result.ResultCommit != p.VerifiedCommit {
		return VerificationRecord{}, fmt.Errorf("%w: verification commit differs from result", ErrInvalidState)
	}
	actorID, err := ensureActor(ctx, tx, p.Actor)
	if err != nil {
		return VerificationRecord{}, err
	}
	event, err := insertJournalEvent(ctx, tx, p.EventID, "verification", string(p.VerificationID), "verification.prepared", "",
		string(VerificationPrepared), 1, p.PreparedAt, actorID, result, p.EvidenceSHA256, payload)
	if err != nil {
		return VerificationRecord{}, err
	}
	at := unixMillis(p.PreparedAt)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO verifications(id,result_id,task_id,attempt_id,workspace_id,state,policy_name,policy_sha256,verified_commit,
 working_directory,timeout_millis,output_limit_bytes,runner_name,runner_version,image_digest,environment_sha256,
 effect_attempt,latest_event_id,revision,created_at,updated_at)
VALUES(?,?,?,?,?,'prepared',?,?,?,?,?,?,?,?,?,?,0,?,1,?,?)`, p.VerificationID, result.ID, result.TaskID, result.AttemptID,
		result.WorkspaceID, p.PolicyName, p.PolicySHA256[:], p.VerifiedCommit, p.WorkingDirectory, p.Timeout.Milliseconds(),
		p.OutputLimitBytes, p.RunnerName, p.RunnerVersion, p.ImageDigest, p.EnvironmentSHA256[:], event.ID, at, at); err != nil {
		return VerificationRecord{}, fmt.Errorf("prepare verification: %w", err)
	}
	return finishVerification(ctx, tx, p.VerificationID, event)
}

func (s *Store) AdvanceVerification(ctx context.Context, p AdvanceVerificationParams) (_ VerificationRecord, err error) {
	if err := validateJournalTransition(p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision, p.EventID,
		p.StartedAt, p.EvidencePayload, p.EvidenceSHA256, p.Actor); err != nil {
		return VerificationRecord{}, err
	}
	detail := struct {
		EffectAttempt           int   `json:"effectAttempt"`
		ExpectedRevision        int64 `json:"expectedRevision"`
		ExpectedTaskRevision    int64 `json:"expectedTaskRevision"`
		ExpectedAttemptRevision int64 `json:"expectedAttemptRevision"`
	}{1, p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision}
	payload, err := journalPayload(detail, p.EvidencePayload, p.EvidenceSHA256)
	if err != nil {
		return VerificationRecord{}, err
	}
	return s.transitionVerification(ctx, p.VerificationID, p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision,
		p.EventID, VerificationPrepared, VerificationRunning, p.StartedAt, p.Actor, p.EvidenceSHA256, payload,
		func(ctx context.Context, tx *sql.Tx, v Verification, event JournalEvent) error {
			_, err := tx.ExecContext(ctx, `UPDATE verifications SET state='running',effect_attempt=1,started_at=?,latest_event_id=?,revision=revision+1,updated_at=?
WHERE id=? AND state='prepared' AND revision=?`, unixMillis(p.StartedAt), event.ID, unixMillis(p.StartedAt), v.ID, p.ExpectedRevision)
			return err
		})
}

func (s *Store) CompleteVerification(ctx context.Context, p CompleteVerificationParams) (_ VerificationRecord, err error) {
	if err := validateCompleteVerification(p); err != nil {
		return VerificationRecord{}, err
	}
	detail := verificationCompletionDetail(p.State, p.Outcome, p.ExitCode, p.Signal, p.Stdout, p.Stderr, p.Reason,
		p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision)
	payload, err := journalPayload(detail, p.EvidencePayload, p.EvidenceSHA256)
	if err != nil {
		return VerificationRecord{}, err
	}
	return s.transitionVerification(ctx, p.VerificationID, p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision,
		p.EventID, VerificationRunning, p.State, p.EndedAt, p.Actor, p.EvidenceSHA256, payload,
		func(ctx context.Context, tx *sql.Tx, v Verification, event JournalEvent) error {
			var exit any
			if p.ExitCode != nil {
				exit = *p.ExitCode
			}
			var signal any
			if p.Signal != "" {
				signal = p.Signal
			}
			var reason any
			if p.Reason != "" {
				reason = p.Reason
			}
			_, err := tx.ExecContext(ctx, `UPDATE verifications SET state=?,ended_at=?,outcome=?,exit_code=?,signal=?,
stdout_byte_count=?,stdout_retained_bytes=?,stdout_sha256=?,stdout_truncated=?,stderr_byte_count=?,stderr_retained_bytes=?,stderr_sha256=?,stderr_truncated=?,
reason=?,latest_event_id=?,revision=revision+1,updated_at=? WHERE id=? AND state='running' AND revision=?`,
				p.State, unixMillis(p.EndedAt), p.Outcome, exit, signal, p.Stdout.ByteCount, p.Stdout.RetainedBytes, p.Stdout.SHA256[:], boolInt(p.Stdout.Truncated),
				p.Stderr.ByteCount, p.Stderr.RetainedBytes, p.Stderr.SHA256[:], boolInt(p.Stderr.Truncated), reason, event.ID,
				unixMillis(p.EndedAt), v.ID, p.ExpectedRevision)
			return err
		})
}

func (s *Store) RecoverVerification(ctx context.Context, p RecoverVerificationParams) (_ VerificationRecord, err error) {
	if !validBoundedText(p.Reason, 1, 1000) {
		return VerificationRecord{}, fmt.Errorf("%w: recovery reason", ErrInvalidInput)
	}
	if err := validateJournalTransition(p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision, p.EventID,
		p.RecoveredAt, p.EvidencePayload, p.EvidenceSHA256, p.Actor); err != nil {
		return VerificationRecord{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return VerificationRecord{}, err
	}
	defer release()
	defer rollback(tx, &err)
	v, err := getVerification(ctx, tx, p.VerificationID)
	if err != nil {
		return VerificationRecord{}, err
	}
	if v.State == VerificationRecoveryRequired && v.Revision == p.ExpectedRevision+1 {
		e, eventErr := getJournalEvent(ctx, tx, p.EventID)
		if eventErr != nil {
			return VerificationRecord{}, eventErr
		}
		detail := struct {
			Reason                  string              `json:"reason"`
			Outcome                 string              `json:"outcome,omitempty"`
			Stdout                  *VerificationOutput `json:"stdout,omitempty"`
			Stderr                  *VerificationOutput `json:"stderr,omitempty"`
			ExpectedRevision        int64               `json:"expectedRevision"`
			ExpectedTaskRevision    int64               `json:"expectedTaskRevision"`
			ExpectedAttemptRevision int64               `json:"expectedAttemptRevision"`
		}{p.Reason, p.Outcome, p.Stdout, p.Stderr, p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision}
		payload, payloadErr := journalPayload(detail, p.EvidencePayload, p.EvidenceSHA256)
		if payloadErr != nil {
			return VerificationRecord{}, payloadErr
		}
		return finishVerificationReplay(ctx, tx, v, p.EventID, "verification.recovery_required", e.FromState,
			string(VerificationRecoveryRequired), v.Revision, p.RecoveredAt, p.Actor, payload)
	}
	if v.State != VerificationPrepared && v.State != VerificationRunning {
		return VerificationRecord{}, fmt.Errorf("%w: verification cannot recover from %s", ErrInvalidState, v.State)
	}
	if v.State == VerificationPrepared && (p.Outcome != "" || p.Stdout != nil || p.Stderr != nil) {
		return VerificationRecord{}, fmt.Errorf("%w: unstarted verification has output", ErrInvalidInput)
	}
	if v.State == VerificationRunning && (p.Outcome == "" || p.Outcome == "passed" || p.Stdout == nil || p.Stderr == nil ||
		!validVerificationOutput(*p.Stdout, v.OutputLimitBytes) || !validVerificationOutput(*p.Stderr, v.OutputLimitBytes)) {
		return VerificationRecord{}, fmt.Errorf("%w: running verification recovery outcome", ErrInvalidInput)
	}
	detail := struct {
		Reason                  string              `json:"reason"`
		Outcome                 string              `json:"outcome,omitempty"`
		Stdout                  *VerificationOutput `json:"stdout,omitempty"`
		Stderr                  *VerificationOutput `json:"stderr,omitempty"`
		ExpectedRevision        int64               `json:"expectedRevision"`
		ExpectedTaskRevision    int64               `json:"expectedTaskRevision"`
		ExpectedAttemptRevision int64               `json:"expectedAttemptRevision"`
	}{p.Reason, p.Outcome, p.Stdout, p.Stderr, p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision}
	payload, err := journalPayload(detail, p.EvidencePayload, p.EvidenceSHA256)
	if err != nil {
		return VerificationRecord{}, err
	}
	if err := checkVerificationSource(ctx, tx, v, p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision); err != nil {
		return VerificationRecord{}, err
	}
	actorID, err := ensureActor(ctx, tx, p.Actor)
	if err != nil {
		return VerificationRecord{}, err
	}
	result, _ := getResult(ctx, tx, v.ResultID)
	event, err := insertJournalEvent(ctx, tx, p.EventID, "verification", string(v.ID), "verification.recovery_required",
		string(v.State), string(VerificationRecoveryRequired), v.Revision+1, p.RecoveredAt, actorID, result, p.EvidenceSHA256, payload)
	if err != nil {
		return VerificationRecord{}, err
	}
	if v.State == VerificationPrepared {
		_, err = tx.ExecContext(ctx, `UPDATE verifications SET state='recovery_required',reason=?,latest_event_id=?,revision=revision+1,updated_at=? WHERE id=? AND state='prepared' AND revision=?`,
			p.Reason, event.ID, unixMillis(p.RecoveredAt), v.ID, p.ExpectedRevision)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE verifications SET state='recovery_required',ended_at=?,outcome=?,
stdout_byte_count=?,stdout_retained_bytes=?,stdout_sha256=?,stdout_truncated=?,stderr_byte_count=?,stderr_retained_bytes=?,stderr_sha256=?,stderr_truncated=?,
reason=?,latest_event_id=?,revision=revision+1,updated_at=? WHERE id=? AND state='running' AND revision=?`,
			unixMillis(p.RecoveredAt), p.Outcome, p.Stdout.ByteCount, p.Stdout.RetainedBytes, p.Stdout.SHA256[:], boolInt(p.Stdout.Truncated),
			p.Stderr.ByteCount, p.Stderr.RetainedBytes, p.Stderr.SHA256[:], boolInt(p.Stderr.Truncated), p.Reason, event.ID,
			unixMillis(p.RecoveredAt), v.ID, p.ExpectedRevision)
	}
	if err != nil {
		return VerificationRecord{}, fmt.Errorf("recover verification: %w", err)
	}
	return finishVerification(ctx, tx, v.ID, event)
}

type verificationUpdater func(context.Context, *sql.Tx, Verification, JournalEvent) error

func (s *Store) transitionVerification(ctx context.Context, id task.VerificationID, expected, taskRevision, attemptRevision int64,
	eventID task.EventID, from, to VerificationState, at time.Time, actor task.ActorSnapshot, evidenceHash [32]byte,
	payload []byte, update verificationUpdater,
) (_ VerificationRecord, err error) {
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return VerificationRecord{}, err
	}
	defer release()
	defer rollback(tx, &err)
	v, err := getVerification(ctx, tx, id)
	if err != nil {
		return VerificationRecord{}, err
	}
	if v.Revision == expected+1 && v.State == to {
		return finishVerificationReplay(ctx, tx, v, eventID, "verification."+string(to), string(from), string(to),
			v.Revision, at, actor, payload)
	}
	if v.State != from {
		return VerificationRecord{}, fmt.Errorf("%w: verification is %s, requires %s", ErrInvalidState, v.State, from)
	}
	if err := checkVerificationSource(ctx, tx, v, expected, taskRevision, attemptRevision); err != nil {
		return VerificationRecord{}, err
	}
	if at.Before(v.UpdatedAt) {
		return VerificationRecord{}, fmt.Errorf("%w: verification timestamp regressed", ErrInvalidInput)
	}
	actorID, err := ensureActor(ctx, tx, actor)
	if err != nil {
		return VerificationRecord{}, err
	}
	result, _ := getResult(ctx, tx, v.ResultID)
	event, err := insertJournalEvent(ctx, tx, eventID, "verification", string(v.ID), "verification."+string(to), string(from),
		string(to), v.Revision+1, at, actorID, result, evidenceHash, payload)
	if err != nil {
		return VerificationRecord{}, err
	}
	if err := update(ctx, tx, v, event); err != nil {
		return VerificationRecord{}, fmt.Errorf("transition verification: %w", err)
	}
	return finishVerification(ctx, tx, id, event)
}

func checkVerificationSource(ctx context.Context, q queryRower, v Verification, expected, taskRevision, attemptRevision int64) error {
	if v.Revision != expected {
		return &StaleJournalRevisionError{Kind: "verification", ID: string(v.ID), Expected: expected, Actual: v.Revision}
	}
	result, _, _, err := journalSource(ctx, q, v.ResultID, taskRevision, attemptRevision)
	if err != nil {
		return err
	}
	if result.ID != v.ResultID || result.TaskID != v.TaskID || result.AttemptID != v.AttemptID || result.ResultCommit != v.VerifiedCommit {
		return fmt.Errorf("%w: verification result tuple differs", ErrInvalidState)
	}
	return nil
}

func finishVerification(ctx context.Context, tx *sql.Tx, id task.VerificationID, event JournalEvent) (VerificationRecord, error) {
	v, err := getVerification(ctx, tx, id)
	if err != nil {
		return VerificationRecord{}, err
	}
	if v.LatestEventID != event.ID || v.Revision != event.EntityRevision {
		return VerificationRecord{}, fmt.Errorf("%w: verification event pairing", ErrCorruptStore)
	}
	if err := tx.Commit(); err != nil {
		return VerificationRecord{}, fmt.Errorf("commit verification: %w", err)
	}
	return VerificationRecord{Verification: v, Event: event}, nil
}

func finishVerificationReplay(ctx context.Context, tx *sql.Tx, v Verification, eventID task.EventID, eventType, from, to string,
	revision int64, at time.Time, actor task.ActorSnapshot, payload []byte,
) (VerificationRecord, error) {
	e, err := exactJournalEvent(ctx, tx, eventID, v.LatestEventID, "verification", string(v.ID), eventType, from, to, revision, at, actor, payload)
	if err != nil {
		return VerificationRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return VerificationRecord{}, err
	}
	return VerificationRecord{Verification: v, Event: e, Replayed: true}, nil
}

func getVerification(ctx context.Context, q queryRower, id task.VerificationID) (Verification, error) {
	v, err := scanVerification(q.QueryRowContext(ctx, verificationSelect+` WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Verification{}, ErrNotFound
	}
	if err != nil {
		return Verification{}, fmt.Errorf("read verification: %w", err)
	}
	return v, nil
}

func scanVerification(row rowScanner) (Verification, error) {
	var v Verification
	var policyHash, environmentHash []byte
	var started, ended sql.NullInt64
	var outcome, signal, reason sql.NullString
	var exit sql.NullInt64
	var stdoutCount, stdoutRetained, stdoutTruncated, stderrCount, stderrRetained, stderrTruncated sql.NullInt64
	var stdoutHash, stderrHash []byte
	var timeoutMS, created, updated int64
	if err := row.Scan(&v.ID, &v.ResultID, &v.TaskID, &v.AttemptID, &v.WorkspaceID, &v.State, &v.PolicyName, &policyHash,
		&v.VerifiedCommit, &v.WorkingDirectory, &timeoutMS, &v.OutputLimitBytes, &v.RunnerName, &v.RunnerVersion, &v.ImageDigest,
		&environmentHash, &v.EffectAttempt, &started, &ended, &outcome, &exit, &signal,
		&stdoutCount, &stdoutRetained, &stdoutHash, &stdoutTruncated, &stderrCount, &stderrRetained, &stderrHash, &stderrTruncated,
		&reason, &v.LatestEventID, &v.Revision, &created, &updated); err != nil {
		return Verification{}, err
	}
	if len(policyHash) != sha256.Size || len(environmentHash) != sha256.Size {
		return Verification{}, ErrCorruptStore
	}
	copy(v.PolicySHA256[:], policyHash)
	copy(v.EnvironmentSHA256[:], environmentHash)
	v.Timeout = time.Duration(timeoutMS) * time.Millisecond
	v.StartedAt, v.EndedAt = nullableTime(started), nullableTime(ended)
	v.Outcome, v.Signal, v.Reason = outcome.String, signal.String, reason.String
	if exit.Valid {
		n := int(exit.Int64)
		v.ExitCode = &n
	}
	if outcome.Valid {
		if len(stdoutHash) != sha256.Size || len(stderrHash) != sha256.Size || !stdoutCount.Valid || !stdoutRetained.Valid || !stdoutTruncated.Valid ||
			!stderrCount.Valid || !stderrRetained.Valid || !stderrTruncated.Valid {
			return Verification{}, ErrCorruptStore
		}
		v.Stdout = &VerificationOutput{ByteCount: stdoutCount.Int64, RetainedBytes: stdoutRetained.Int64, Truncated: stdoutTruncated.Int64 == 1}
		v.Stderr = &VerificationOutput{ByteCount: stderrCount.Int64, RetainedBytes: stderrRetained.Int64, Truncated: stderrTruncated.Int64 == 1}
		copy(v.Stdout.SHA256[:], stdoutHash)
		copy(v.Stderr.SHA256[:], stderrHash)
	}
	v.CreatedAt, v.UpdatedAt = fromUnixMillis(created), fromUnixMillis(updated)
	return v, nil
}

func validatePrepareVerification(p PrepareVerificationParams) error {
	if _, err := task.ParseVerificationID(string(p.VerificationID)); err != nil {
		return fmt.Errorf("%w: verification ID", ErrInvalidInput)
	}
	if _, err := task.ParseResultID(string(p.ResultID)); err != nil {
		return fmt.Errorf("%w: result ID", ErrInvalidInput)
	}
	if p.ExpectedTaskRevision < 1 || p.ExpectedAttemptRevision < 1 || !validPolicyName(p.PolicyName) ||
		!validBoundedText(p.RunnerName, 1, 128) || !validBoundedText(p.RunnerVersion, 1, 128) || !validBoundedText(p.ImageDigest, 1, 256) ||
		p.Timeout <= 0 || p.Timeout > time.Hour || p.Timeout%time.Millisecond != 0 || p.OutputLimitBytes < 1 || p.OutputLimitBytes > 1<<20 {
		return fmt.Errorf("%w: verification policy", ErrInvalidInput)
	}
	if _, err := task.ParseGitOID(string(p.VerifiedCommit)); err != nil {
		return fmt.Errorf("%w: verified commit", ErrInvalidInput)
	}
	if strings.IndexByte(p.WorkingDirectory, 0) >= 0 || filepath.IsAbs(p.WorkingDirectory) ||
		(p.WorkingDirectory != "" && (filepath.Clean(p.WorkingDirectory) != p.WorkingDirectory || p.WorkingDirectory == ".." || strings.HasPrefix(p.WorkingDirectory, ".."+string(filepath.Separator)))) || len(p.WorkingDirectory) > 4096 {
		return fmt.Errorf("%w: verification working directory", ErrInvalidInput)
	}
	if err := validExactTimestamp(p.PreparedAt); err != nil {
		return err
	}
	if err := p.Actor.Validate(); err != nil || (p.Actor.Type != task.ActorSystem && p.Actor.Type != task.ActorRecovery) {
		return fmt.Errorf("%w: verification actor", ErrInvalidInput)
	}
	if _, err := task.ParseEventID(string(p.EventID)); err != nil {
		return fmt.Errorf("%w: verification event ID", ErrInvalidInput)
	}
	return validateDeliveryEvidence(p.EvidencePayload, p.EvidenceSHA256)
}

func validateCompleteVerification(p CompleteVerificationParams) error {
	if err := validateJournalTransition(p.ExpectedRevision, p.ExpectedTaskRevision, p.ExpectedAttemptRevision, p.EventID,
		p.EndedAt, p.EvidencePayload, p.EvidenceSHA256, p.Actor); err != nil {
		return err
	}
	if (p.State != VerificationSucceeded && p.State != VerificationFailed) || !validVerificationOutcome(p.Outcome) ||
		!validVerificationOutput(p.Stdout, 1<<20) || !validVerificationOutput(p.Stderr, 1<<20) {
		return fmt.Errorf("%w: verification completion", ErrInvalidInput)
	}
	if p.ExitCode != nil && (*p.ExitCode < 0 || *p.ExitCode > 255) {
		return fmt.Errorf("%w: verification exit code", ErrInvalidInput)
	}
	if p.Signal != "" && !validBoundedText(p.Signal, 1, 64) {
		return fmt.Errorf("%w: verification signal", ErrInvalidInput)
	}
	if p.State == VerificationSucceeded {
		if p.Outcome != "passed" || p.ExitCode == nil || *p.ExitCode != 0 || p.Signal != "" || p.Reason != "" {
			return fmt.Errorf("%w: successful verification outcome", ErrInvalidInput)
		}
	} else if p.Outcome == "passed" || !validBoundedText(p.Reason, 1, 1000) {
		return fmt.Errorf("%w: failed verification outcome", ErrInvalidInput)
	}
	if p.Outcome == "exit_nonzero" && (p.ExitCode == nil || *p.ExitCode == 0 || p.Signal != "") {
		return fmt.Errorf("%w: nonzero-exit verification outcome", ErrInvalidInput)
	}
	if p.Outcome == "signaled" && p.Signal == "" {
		return fmt.Errorf("%w: signaled verification outcome", ErrInvalidInput)
	}
	return nil
}

func validVerificationOutput(o VerificationOutput, limit int64) bool {
	return o.ByteCount >= 0 && o.RetainedBytes >= 0 && o.RetainedBytes <= o.ByteCount && o.RetainedBytes <= limit &&
		((!o.Truncated && o.RetainedBytes == o.ByteCount) || (o.Truncated && o.ByteCount > o.RetainedBytes && o.RetainedBytes == limit))
}

func validVerificationOutcome(v string) bool {
	switch v {
	case "passed", "exit_nonzero", "signaled", "timeout", "canceled", "start_failed", "integrity_failure", "runner_failure":
		return true
	default:
		return false
	}
}

func validPolicyName(v string) bool {
	if len(v) < 1 || len(v) > 64 || v[0] < 'a' || v[0] > 'z' {
		return false
	}
	for i := 1; i < len(v); i++ {
		c := v[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func verificationCompletionDetail(state VerificationState, outcome string, exit *int, signal string, stdout, stderr VerificationOutput,
	reason string, expected, expectedTask, expectedAttempt int64,
) any {
	return struct {
		State                   VerificationState  `json:"state"`
		Outcome                 string             `json:"outcome"`
		ExitCode                *int               `json:"exitCode"`
		Signal                  string             `json:"signal"`
		Stdout                  VerificationOutput `json:"stdout"`
		Stderr                  VerificationOutput `json:"stderr"`
		Reason                  string             `json:"reason"`
		ExpectedRevision        int64              `json:"expectedRevision"`
		ExpectedTaskRevision    int64              `json:"expectedTaskRevision"`
		ExpectedAttemptRevision int64              `json:"expectedAttemptRevision"`
	}{state, outcome, exit, signal, stdout, stderr, reason, expected, expectedTask, expectedAttempt}
}

func digestString(v [32]byte) string { return "sha256:" + fmt.Sprintf("%x", v[:]) }
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
