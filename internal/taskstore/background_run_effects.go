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

const maxBackgroundRunLease = 5 * time.Minute

// ClaimNextBackgroundRun commits effect authority before any external I/O. It
// prefers recovery of an existing effect over consuming the workspace's one
// provisioning slot.
func (s *Store) ClaimNextBackgroundRun(ctx context.Context, p ClaimNextBackgroundRunParams) (_ BackgroundRun, err error) {
	if err := validateBackgroundClaimRequest(p.WorkspaceID, p.ClaimOwner, p.Profile, p.ImageIdentity, p.Now, p.LeaseDuration); err != nil {
		return BackgroundRun{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("begin background run claim: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	now, expiry := unixMillis(p.Now), unixMillis(p.Now.Add(p.LeaseDuration))
	var taskID task.TaskID
	var attemptID task.AttemptID
	var generation, revision int64
	var state BackgroundRunState
	var phase BackgroundRunEffectPhase
	var cancelEpochValue int64
	err = tx.QueryRowContext(ctx, `SELECT r.task_id,r.attempt_id,r.generation,r.revision,r.state,r.effect_phase,r.cancel_epoch
FROM background_runs r
JOIN tasks t ON t.id=r.task_id AND t.workspace_id=r.workspace_id AND t.current_attempt_id=r.attempt_id
JOIN attempts a ON a.id=r.attempt_id AND a.task_id=r.task_id AND a.workspace_id=r.workspace_id AND a.sequence=r.generation
JOIN background_run_ownerships o ON o.task_id=r.task_id AND o.attempt_id=r.attempt_id AND o.workspace_id=r.workspace_id AND o.run_generation=r.generation
WHERE r.workspace_id=? AND r.profile=? AND r.state<>'failed' AND
	NOT (r.state='result_ready' AND r.effect_phase='cleanup_complete') AND
	o.mode='agent_owned' AND
	NOT EXISTS (SELECT 1 FROM background_run_controls c WHERE c.task_id=r.task_id AND c.state IN ('requested','attempted')) AND
  (r.claim_owner=? OR r.claim_owner IS NULL OR r.claim_expires_at<=?) AND
  ((r.state='queued' AND r.cancel_epoch=0 AND NOT EXISTS (
      SELECT 1 FROM background_runs active WHERE active.workspace_id=r.workspace_id AND active.profile=? AND
		active.effect_phase NOT IN ('absent','cleanup_complete','pre_effect_failed')
	    )) OR r.state IN ('setting_up','working','needs_you','uncertain','canceling','cleanup_required','result_ready'))
ORDER BY CASE WHEN r.state='queued' THEN 1 ELSE 0 END,r.updated_at,r.task_id LIMIT 1`, p.WorkspaceID, p.Profile, p.ClaimOwner, now, p.Profile).
		Scan(&taskID, &attemptID, &generation, &revision, &state, &phase, &cancelEpochValue)
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRun{}, ErrNotFound
	}
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("find background run claim: %w", err)
	}
	cancelEpoch := uint64(cancelEpochValue)

	newState, newPhase := state, phase
	provisionStarted := any(nil)
	if state == BackgroundRunQueued {
		newState, newPhase = BackgroundRunSettingUp, BackgroundRunEffectProvisionIntent
		provisionStarted = now
	}
	result, err := tx.ExecContext(ctx, `UPDATE background_runs SET state=?,effect_phase=?,claim_owner=?,claim_expires_at=?,
claim_generation=claim_generation+CASE WHEN claim_owner=? THEN 0 ELSE 1 END,provision_intent_at=COALESCE(provision_intent_at,?),revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND revision=? AND cancel_epoch=? AND
(claim_owner=? OR claim_owner IS NULL OR claim_expires_at<=?)`, newState, newPhase, p.ClaimOwner, expiry, p.ClaimOwner, provisionStarted, now,
		taskID, attemptID, p.WorkspaceID, generation, revision, cancelEpoch, p.ClaimOwner, now)
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("claim background run: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRun{}, ErrInvalidState
	}
	run, err := readBackgroundRunExact(ctx, tx, p.WorkspaceID, taskID)
	if err != nil {
		return BackgroundRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRun{}, fmt.Errorf("commit background run claim: %w", err)
	}
	return run, nil
}

// ClaimActiveBackgroundRun claims one exact recoverable run after restart.
func (s *Store) ClaimActiveBackgroundRun(ctx context.Context, p ClaimBackgroundRunParams) (BackgroundRun, error) {
	return s.claimExactBackgroundRun(ctx, p, false)
}

// ClaimBackgroundRunStop claims only an already committed stop request.
func (s *Store) ClaimBackgroundRunStop(ctx context.Context, p ClaimBackgroundRunParams) (BackgroundRun, error) {
	return s.claimExactBackgroundRun(ctx, p, true)
}

func (s *Store) claimExactBackgroundRun(ctx context.Context, p ClaimBackgroundRunParams, stopping bool) (_ BackgroundRun, err error) {
	if err := validateBackgroundClaimRequest(p.WorkspaceID, p.ClaimOwner, p.Profile, p.ImageIdentity, p.Now, p.LeaseDuration); err != nil ||
		p.TaskID == "" || p.AttemptID == "" || p.Generation <= 0 || p.ExpectedRevision <= 0 || p.CancelEpoch > 1 ||
		!validBackgroundRunStatePhase(p.Profile, p.ExpectedState, p.ExpectedPhase) {
		return BackgroundRun{}, fmt.Errorf("%w: exact background run claim", ErrInvalidInput)
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRun{}, err
	}
	defer release()
	defer rollback(tx, &err)
	now, expiry := unixMillis(p.Now), unixMillis(p.Now.Add(p.LeaseDuration))
	states := "('setting_up','working','needs_you','uncertain','cleanup_required','result_ready')"
	databaseState, databasePhase := p.ExpectedState, p.ExpectedPhase
	if p.ExpectedState == BackgroundRunCanceling && p.ExpectedPhase == BackgroundRunEffectSealIntent {
		databaseState, databasePhase = BackgroundRunCleanupRequired, BackgroundRunEffectStopIntent
	}
	if p.ExpectedState == BackgroundRunResultReady && p.ExpectedPhase == BackgroundRunEffectArtifactCommitted {
		databaseState, databasePhase = BackgroundRunResultReady, BackgroundRunEffectWriterInactive
	}
	if stopping {
		states = "('canceling','cleanup_required')"
	}
	query := `UPDATE background_runs SET claim_owner=?,claim_expires_at=?,claim_generation=claim_generation+1,
revision=revision+1,updated_at=? WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND revision=? AND
cancel_epoch=? AND profile=? AND image_identity=? AND state=? AND effect_phase=? AND state IN ` + states + ` AND (claim_owner IS NULL OR claim_expires_at<=?)`
	result, err := tx.ExecContext(ctx, query, p.ClaimOwner, expiry, now, p.TaskID, p.AttemptID, p.WorkspaceID,
		p.Generation, p.ExpectedRevision, p.CancelEpoch, p.Profile, p.ImageIdentity, databaseState, databasePhase, now)
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("claim exact background run: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRun{}, ErrInvalidState
	}
	run, err := readBackgroundRunExact(ctx, tx, p.WorkspaceID, p.TaskID)
	if err != nil {
		return BackgroundRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRun{}, err
	}
	return run, nil
}

func (s *Store) ReadClaimedBackgroundRun(ctx context.Context, p BackgroundRunClaim) (BackgroundRun, error) {
	if err := validateBackgroundRunClaim(p); err != nil {
		return BackgroundRun{}, err
	}
	databaseState, databasePhase := databaseBackgroundStatePhase(p.ExpectedState, p.ExpectedPhase)
	run, err := scanBackgroundRun(s.db.QueryRowContext(ctx, backgroundRunSelect+`
WHERE r.task_id=? AND r.attempt_id=? AND r.workspace_id=? AND r.generation=? AND r.revision=? AND r.state=? AND r.effect_phase=? AND r.cancel_epoch=? AND
r.claim_owner=? AND r.claim_generation=? AND r.claim_expires_at>?`, p.TaskID, p.AttemptID, p.WorkspaceID, p.Generation,
		p.ExpectedRevision, databaseState, databasePhase, p.CancelEpoch, p.ClaimOwner, p.ClaimGeneration, unixMillis(p.Now)))
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRun{}, ErrInvalidState
	}
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("read claimed background run: %w", err)
	}
	return run, nil
}

// ReadClaimedBackgroundRunWork returns task plaintext only after rechecking the
// complete active run claim and digest binding.
func (s *Store) ReadClaimedBackgroundRunWork(ctx context.Context, p BackgroundRunClaim) (BackgroundRunWork, error) {
	run, err := s.ReadClaimedBackgroundRun(ctx, p)
	if err != nil {
		return BackgroundRunWork{}, err
	}
	return s.readBackgroundRunWork(ctx, run)
}

func (s *Store) ClaimNextBackgroundRunWork(ctx context.Context, p ClaimNextBackgroundRunParams) (BackgroundRunWork, error) {
	run, err := s.ClaimNextBackgroundRun(ctx, p)
	if err != nil {
		return BackgroundRunWork{}, err
	}
	return s.ReadClaimedBackgroundRunWork(ctx, BackgroundRunClaim{WorkspaceID: run.WorkspaceID, TaskID: run.TaskID,
		AttemptID: run.AttemptID, Generation: run.Generation, ClaimOwner: run.ClaimOwner, ClaimGeneration: run.ClaimGeneration,
		ExpectedRevision: run.Revision, ExpectedState: run.State, ExpectedPhase: run.EffectPhase, CancelEpoch: run.CancelEpoch, Now: p.Now})
}

func (s *Store) readBackgroundRunWork(ctx context.Context, run BackgroundRun) (BackgroundRunWork, error) {
	owner, err := getTask(ctx, s.db, run.TaskID)
	if err != nil {
		return BackgroundRunWork{}, err
	}
	attempt, err := getAttempt(ctx, s.db, run.AttemptID)
	if err != nil {
		return BackgroundRunWork{}, err
	}
	digest := sha256.Sum256([]byte(owner.Prompt))
	if owner.WorkspaceID != run.WorkspaceID || owner.CurrentAttemptID != attempt.ID || attempt.TaskID != run.TaskID ||
		attempt.WorkspaceID != run.WorkspaceID || attempt.Sequence != run.Generation || digest != run.InstructionSHA256 ||
		owner.PromptSHA256 != digest || attempt.PromptSHA256 != digest || !attempt.Deadline.After(attempt.CreatedAt) {
		return BackgroundRunWork{}, ErrCorruptStore
	}
	return BackgroundRunWork{Run: run, Prompt: owner.Prompt, Deadline: attempt.Deadline, AttemptCreated: attempt.CreatedAt,
		AttemptTimeout: attempt.Deadline.Sub(attempt.CreatedAt), Agent: attempt.Agent, ModelProvider: attempt.ModelProvider, Model: attempt.Model}, nil
}

func (s *Store) RenewBackgroundRunClaim(ctx context.Context, p RenewBackgroundRunClaimParams) (BackgroundRun, error) {
	if err := validateBackgroundRunClaim(p.BackgroundRunClaim); err != nil || p.LeaseDuration <= 0 || p.LeaseDuration > maxBackgroundRunLease {
		return BackgroundRun{}, fmt.Errorf("%w: background run claim renewal", ErrInvalidInput)
	}
	return s.updateClaimedRun(ctx, p.BackgroundRunClaim,
		`claim_expires_at=?`, []any{unixMillis(p.Now.Add(p.LeaseDuration))}, "renew background run claim")
}

func (s *Store) ReleaseBackgroundRunClaim(ctx context.Context, p BackgroundRunClaim) (BackgroundRun, error) {
	return s.updateClaimedRun(ctx, p, `claim_owner=NULL,claim_expires_at=NULL`, nil, "release background run claim")
}

func (s *Store) RecordBackgroundRunCloneObserved(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectProvisionIntent || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run clone observation", ErrInvalidInput)
	}
	return s.transitionClaimedRun(ctx, p.BackgroundRunClaim, BackgroundRunSettingUp, BackgroundRunEffectCloneObserved,
		`clone_observed_at=?,clone_evidence=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence, p.Evidence}, "record background run clone observation")
}

func (s *Store) RecordBackgroundRunVolumeObserved(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectCloneObserved || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run volume observation", ErrInvalidInput)
	}
	return s.transitionClaimedRun(ctx, p.BackgroundRunClaim, BackgroundRunSettingUp, BackgroundRunEffectVolumeObserved,
		`volume_observed_at=?,volume_evidence=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence, p.Evidence}, "record background run volume observation")
}

func (s *Store) RecordBackgroundRunContainerObserved(ctx context.Context, p RecordBackgroundRunContainerObservedParams) (BackgroundRun, error) {
	if !validBoundedText(p.ContainerID, 1, 128) || !validBoundedText(p.ContainerStartedAt, 1, 64) ||
		p.RuntimeEpoch <= 0 || p.HostPort < 1 || p.HostPort > 65535 || !validRequiredEvidence(p.Evidence) || p.ExpectedPhase != BackgroundRunEffectVolumeObserved {
		return BackgroundRun{}, fmt.Errorf("%w: background run observation", ErrInvalidInput)
	}
	return s.transitionClaimedRun(ctx, p.BackgroundRunClaim, BackgroundRunSettingUp, BackgroundRunEffectContainerObserved,
		`observed_container_id=?,observed_container_started_at=?,runtime_epoch=?,host_port=?,container_observed_at=?,last_evidence=?`,
		[]any{p.ContainerID, p.ContainerStartedAt, p.RuntimeEpoch, p.HostPort, unixMillis(p.Now), p.Evidence}, "record background run provision observation")
}

func (s *Store) RecordBackgroundRunHealthObserved(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectContainerObserved || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run health observation", ErrInvalidInput)
	}
	return s.transitionClaimedRun(ctx, p.BackgroundRunClaim, BackgroundRunSettingUp, BackgroundRunEffectHealthObserved,
		`health_observed_at=?,health_evidence=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence, p.Evidence}, "record background run health observation")
}

func (s *Store) RecordBackgroundRunReady(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectHealthObserved || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run readiness", ErrInvalidInput)
	}
	return s.transitionClaimedRun(ctx, p.BackgroundRunClaim, BackgroundRunSettingUp, BackgroundRunEffectReady,
		`ready_at=?,ready_evidence=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence, p.Evidence}, "record background run readiness")
}

func (s *Store) RecordBackgroundRunSessionObserved(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectReady || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run session observation", ErrInvalidInput)
	}
	return s.transitionClaimedRun(ctx, p.BackgroundRunClaim, BackgroundRunSettingUp, BackgroundRunEffectSessionObserved,
		`session_observed_at=?,session_evidence=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence, p.Evidence}, "record background run session observation")
}

func (s *Store) RecordBackgroundRunPromptIntent(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectSessionObserved || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run prompt intent", ErrInvalidInput)
	}
	return s.transitionClaimedRun(ctx, p.BackgroundRunClaim, BackgroundRunUncertain, BackgroundRunEffectPromptIntent,
		`prompt_intent_at=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence}, "record background run prompt intent")
}

// RecordBackgroundRunPromptRequestAttempted is the irreversible pre-I/O fence.
// A takeover can observe it but no claimant can set or change it twice.
func (s *Store) RecordBackgroundRunPromptRequestAttempted(ctx context.Context, p BackgroundRunClaim) (BackgroundRun, error) {
	if p.ExpectedState != BackgroundRunUncertain || p.ExpectedPhase != BackgroundRunEffectPromptIntent {
		return BackgroundRun{}, fmt.Errorf("%w: background run prompt request attempt", ErrInvalidInput)
	}
	current, err := s.ReadClaimedBackgroundRun(ctx, p)
	if err != nil {
		return BackgroundRun{}, err
	}
	if current.PromptRequestAttemptedAt != nil {
		return BackgroundRun{}, ErrInvalidState
	}
	return s.updateClaimedRun(ctx, p, `prompt_request_attempted_at=?`, []any{unixMillis(p.Now)}, "record background run prompt request attempt")
}

func (s *Store) RecordBackgroundRunPromptAdmitted(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectPromptIntent || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run prompt admission", ErrInvalidInput)
	}
	return s.transitionClaimedRun(ctx, p.BackgroundRunClaim, BackgroundRunWorking, BackgroundRunEffectPromptAdmitted,
		`prompt_admitted_at=?,prompt_evidence=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence, p.Evidence}, "record background run prompt admission")
}

func (s *Store) RecordBackgroundRunPromptUncertain(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectPromptIntent || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: uncertain background run prompt", ErrInvalidInput)
	}
	return s.transitionClaimedRun(ctx, p.BackgroundRunClaim, BackgroundRunUncertain, BackgroundRunEffectPromptIntent,
		`last_evidence=?`, []any{p.Evidence}, "record uncertain background run prompt")
}

// RecordBackgroundRunWorkObservation records positive bounded evidence only.
// Callers must not invoke it for an empty active/pending observation.
func (s *Store) RecordBackgroundRunWorkObservation(ctx context.Context, p RecordBackgroundRunEvidenceParams, state BackgroundRunState) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectPromptAdmitted ||
		(p.ExpectedState != BackgroundRunWorking && p.ExpectedState != BackgroundRunNeedsYou && p.ExpectedState != BackgroundRunUncertain) ||
		(state != BackgroundRunWorking && state != BackgroundRunNeedsYou && state != BackgroundRunUncertain) || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run work observation", ErrInvalidInput)
	}
	return s.transitionClaimedRun(ctx, p.BackgroundRunClaim, state, BackgroundRunEffectPromptAdmitted,
		`last_evidence=?`, []any{p.Evidence}, "record background run work observation")
}

// RequestBackgroundRunTimeout commits a system-owned stop without manufacturing
// a plugin receipt. Parent terminalization remains coupled to cleanup finality.
func (s *Store) RequestBackgroundRunTimeout(ctx context.Context, p RequestBackgroundRunTimeoutParams) (_ BackgroundRun, err error) {
	if err := validateBackgroundRunClaim(p.BackgroundRunClaim); err != nil || p.Actor.Validate() != nil || p.Actor.Type != task.ActorSystem {
		return BackgroundRun{}, fmt.Errorf("%w: background run timeout", ErrInvalidInput)
	}
	if _, parseErr := task.ParseEventID(string(p.AttemptEventID)); parseErr != nil {
		return BackgroundRun{}, fmt.Errorf("%w: background run timeout attempt event", ErrInvalidInput)
	}
	if _, parseErr := task.ParseEventID(string(p.TaskEventID)); parseErr != nil || p.TaskEventID == p.AttemptEventID {
		return BackgroundRun{}, fmt.Errorf("%w: background run timeout task event", ErrInvalidInput)
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
	if run.AttemptID != p.AttemptID || run.Generation != p.Generation || run.Revision != p.ExpectedRevision ||
		run.State != p.ExpectedState || run.EffectPhase != p.ExpectedPhase || run.CancelEpoch != 0 ||
		run.ClaimOwner != p.ClaimOwner || run.ClaimGeneration != p.ClaimGeneration || run.ClaimExpiresAt == nil || !run.ClaimExpiresAt.After(p.Now) {
		return BackgroundRun{}, ErrInvalidState
	}
	owner, err := getTask(ctx, tx, run.TaskID)
	if err != nil {
		return BackgroundRun{}, err
	}
	attempt, err := getAttempt(ctx, tx, run.AttemptID)
	if err != nil {
		return BackgroundRun{}, err
	}
	if p.Now.Before(attempt.Deadline) || owner.State != task.TaskQueued || attempt.State != task.AttemptPrepared {
		return BackgroundRun{}, ErrInvalidState
	}
	actorID, err := ensureActor(ctx, tx, p.Actor)
	if err != nil {
		return BackgroundRun{}, err
	}
	now := unixMillis(p.Now)
	payload := json.RawMessage(`{"reason":"attempt_timeout"}`)
	attemptEvent, err := insertAttemptEvent(ctx, tx, p.AttemptEventID, attempt, "attempt.timeout_requested", now, actorID, payload)
	if err != nil {
		return BackgroundRun{}, err
	}
	taskEvent, err := insertTaskEvent(ctx, tx, p.TaskEventID, owner, "task.timeout_requested", now, actorID, payload)
	if err != nil || attemptEvent.Cursor >= taskEvent.Cursor {
		return BackgroundRun{}, fmt.Errorf("insert background run timeout events: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET latest_event_cursor=?,revision=revision+1,updated_at=?
WHERE id=? AND workspace_id=? AND state='queued' AND current_attempt_id=? AND revision=?`, taskEvent.Cursor, now,
		owner.ID, owner.WorkspaceID, attempt.ID, owner.Revision)
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("project background run timeout event: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRun{}, ErrInvalidState
	}
	result, err = tx.ExecContext(ctx, `UPDATE background_runs SET state='cleanup_required',effect_phase='stop_intent',
timeout_requested_at=?,timeout_actor_snapshot_id=?,stop_intent_at=COALESCE(stop_intent_at,?),last_error='attempt_timeout',
claim_owner=NULL,claim_expires_at=NULL,revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND revision=? AND state=? AND effect_phase=? AND cancel_epoch=0 AND
claim_owner=? AND claim_generation=? AND claim_expires_at>?`, now, actorID, now, now, run.TaskID, run.AttemptID, run.WorkspaceID,
		run.Generation, run.Revision, run.State, run.EffectPhase, run.ClaimOwner, run.ClaimGeneration, now)
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("request background run timeout: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRun{}, ErrInvalidState
	}
	stored, err := readBackgroundRunExact(ctx, tx, run.WorkspaceID, run.TaskID)
	if err != nil {
		return BackgroundRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRun{}, err
	}
	return stored, nil
}

func (s *Store) RecordBackgroundRunResultReady(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	return BackgroundRun{}, fmt.Errorf("%w: retained result commit is the only result-ready authority", ErrInvalidState)
}

func (s *Store) RecordBackgroundRunWriterInactive(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectStopIntent || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run writer inactivity", ErrInvalidInput)
	}
	return s.transitionClaimedRun(ctx, p.BackgroundRunClaim, p.ExpectedState, BackgroundRunEffectWriterInactive,
		`writer_inactive_at=?,writer_inactive_evidence=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence, p.Evidence}, "record background run writer inactivity")
}

func (s *Store) RequestBackgroundRunResultCleanup(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedState != BackgroundRunResultReady || p.ExpectedPhase != BackgroundRunEffectArtifactCommitted || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background result cleanup intent", ErrInvalidInput)
	}
	return s.updateClaimedRetainedRun(ctx, p.BackgroundRunClaim, `result_authority_phase='cleanup',last_evidence=?`, []any{p.Evidence}, "request background result cleanup")
}

func (s *Store) updateClaimedRetainedRun(ctx context.Context, claim BackgroundRunClaim, assignments string, args []any, operation string) (_ BackgroundRun, err error) {
	if err := validateBackgroundRunClaim(claim); err != nil {
		return BackgroundRun{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRun{}, err
	}
	defer release()
	defer rollback(tx, &err)
	query := `UPDATE background_runs SET ` + assignments + `,revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND claim_owner=? AND claim_generation=? AND
claim_expires_at>? AND revision=? AND state='result_ready' AND effect_phase='writer_inactive' AND
result_authority_phase='artifact_committed' AND cancel_epoch=0`
	args = append(args, unixMillis(claim.Now), claim.TaskID, claim.AttemptID, claim.WorkspaceID, claim.Generation,
		claim.ClaimOwner, claim.ClaimGeneration, unixMillis(claim.Now), claim.ExpectedRevision)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("%s: %w", operation, err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRun{}, ErrInvalidState
	}
	run, err := readBackgroundRunExact(ctx, tx, claim.WorkspaceID, claim.TaskID)
	if err != nil {
		return BackgroundRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRun{}, err
	}
	return run, nil
}

func (s *Store) RecordBackgroundRunRouteRemoved(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	return s.recordBackgroundRunRemoval(ctx, p, BackgroundRunEffectWriterInactive, BackgroundRunEffectRouteRemoved, "route_removed_at", "route_removed_evidence", "route removal")
}

func (s *Store) RecordBackgroundRunContainerRemoved(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	return s.recordBackgroundRunRemoval(ctx, p, BackgroundRunEffectRouteRemoved, BackgroundRunEffectContainerRemoved, "container_removed_at", "container_removed_evidence", "container removal")
}

func (s *Store) RecordBackgroundRunVolumeRemoved(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	return s.recordBackgroundRunRemoval(ctx, p, BackgroundRunEffectContainerRemoved, BackgroundRunEffectVolumeRemoved, "volume_removed_at", "volume_removed_evidence", "volume removal")
}

func (s *Store) RecordBackgroundRunCloneRemoved(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	return s.recordBackgroundRunRemoval(ctx, p, BackgroundRunEffectVolumeRemoved, BackgroundRunEffectCloneRemoved, "clone_removed_at", "clone_removed_evidence", "clone removal")
}

func (s *Store) recordBackgroundRunRemoval(ctx context.Context, p RecordBackgroundRunEvidenceParams, from, to BackgroundRunEffectPhase, timestamp, evidenceColumn, operation string) (BackgroundRun, error) {
	if p.ExpectedPhase != from || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run %s", ErrInvalidInput, operation)
	}
	return s.transitionClaimedRun(ctx, p.BackgroundRunClaim, p.ExpectedState, to,
		timestamp+`=?,`+evidenceColumn+`=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence, p.Evidence}, "record background run "+operation)
}

func (s *Store) MarkBackgroundRunCleanupRequired(ctx context.Context, p MarkBackgroundRunCleanupRequiredParams) (BackgroundRun, error) {
	if !validBoundedText(p.Error, 1, 4096) {
		return BackgroundRun{}, fmt.Errorf("%w: background run cleanup failure", ErrInvalidInput)
	}
	if p.ExpectedState == BackgroundRunResultReady && p.ExpectedPhase == BackgroundRunEffectArtifactCommitted {
		return s.updateClaimedRetainedRun(ctx, p.BackgroundRunClaim, `last_error=?,claim_owner=NULL,claim_expires_at=NULL`, []any{p.Error}, "retain failed background result cleanup")
	}
	if cleanupEffectPhase(p.ExpectedPhase) {
		state := p.ExpectedState
		if state == BackgroundRunCanceling {
			state = BackgroundRunCleanupRequired
		}
		if state != BackgroundRunCleanupRequired && state != BackgroundRunResultReady {
			return BackgroundRun{}, fmt.Errorf("%w: background run cleanup failure state", ErrInvalidInput)
		}
		return s.transitionClaimedRun(ctx, p.BackgroundRunClaim, state, p.ExpectedPhase,
			`last_error=?,claim_owner=NULL,claim_expires_at=NULL`, []any{p.Error}, "retain failed background run cleanup phase")
	}
	validState := p.ExpectedState == BackgroundRunSettingUp || p.ExpectedState == BackgroundRunWorking ||
		p.ExpectedState == BackgroundRunNeedsYou || p.ExpectedState == BackgroundRunUncertain
	if !validState || p.ExpectedPhase == BackgroundRunEffectAbsent {
		return BackgroundRun{}, fmt.Errorf("%w: background run cleanup failure state", ErrInvalidInput)
	}
	return s.transitionClaimedRun(ctx, p.BackgroundRunClaim, BackgroundRunCleanupRequired, BackgroundRunEffectStopIntent,
		`stop_intent_at=?,last_error=?,claim_owner=NULL,claim_expires_at=NULL`, []any{unixMillis(p.Now), p.Error}, "mark background run cleanup required")
}

// FinalizeBackgroundRunFailure atomically closes an active run and its exact
// parent task/attempt after cleanup, or before any effect with explicit absence
// proof. It performs no external I/O.
func (s *Store) FinalizeBackgroundRunFailure(ctx context.Context, p FinalizeBackgroundRunFailureParams) (_ BackgroundRun, err error) {
	preEffect := p.ExpectedPhase == BackgroundRunEffectProvisionIntent
	cleaned := p.ExpectedPhase == BackgroundRunEffectCloneRemoved
	if err := validateBackgroundRunClaim(p.BackgroundRunClaim); err != nil || (!preEffect && !cleaned) ||
		!validBoundedText(p.Reason, 1, 1000) || !validRequiredEvidence(p.Evidence) || !validRequiredEvidence(p.CleanupProof) ||
		p.Actor.Validate() != nil || p.AttemptEventID == p.TaskEventID {
		return BackgroundRun{}, fmt.Errorf("%w: background run finalization", ErrInvalidInput)
	}
	if _, parseErr := task.ParseEventID(string(p.AttemptEventID)); parseErr != nil {
		return BackgroundRun{}, fmt.Errorf("%w: attempt event", ErrInvalidInput)
	}
	if _, parseErr := task.ParseEventID(string(p.TaskEventID)); parseErr != nil {
		return BackgroundRun{}, fmt.Errorf("%w: task event", ErrInvalidInput)
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
	if run.AttemptID != p.AttemptID || run.Generation != p.Generation || run.Revision != p.ExpectedRevision ||
		run.State != p.ExpectedState || run.EffectPhase != p.ExpectedPhase || run.CancelEpoch != p.CancelEpoch ||
		run.ClaimOwner != p.ClaimOwner || run.ClaimGeneration != p.ClaimGeneration || run.ClaimExpiresAt == nil || !run.ClaimExpiresAt.After(p.Now) {
		return BackgroundRun{}, ErrInvalidState
	}
	owner, err := getTask(ctx, tx, run.TaskID)
	if err != nil {
		return BackgroundRun{}, err
	}
	attempt, err := getAttempt(ctx, tx, run.AttemptID)
	if err != nil {
		return BackgroundRun{}, err
	}
	if owner.WorkspaceID != run.WorkspaceID || owner.CurrentAttemptID != attempt.ID || owner.State != task.TaskQueued ||
		attempt.TaskID != owner.ID || attempt.WorkspaceID != owner.WorkspaceID || attempt.Sequence != run.Generation || attempt.State != task.AttemptPrepared {
		return BackgroundRun{}, ErrInvalidState
	}
	if run.TimeoutRequestedAt != nil {
		if run.TimeoutActor == nil || p.Actor != *run.TimeoutActor || p.Reason != "attempt_timeout" {
			return BackgroundRun{}, ErrInvalidState
		}
	}
	actorID, err := ensureActor(ctx, tx, p.Actor)
	if err != nil {
		return BackgroundRun{}, err
	}
	now := unixMillis(p.Now)
	payload, err := json.Marshal(struct {
		RunID         task.TaskID    `json:"runId"`
		Reason        string         `json:"reason"`
		Evidence      string         `json:"evidence"`
		CleanupProof  string         `json:"cleanupProof"`
		StopReceiptID task.ReceiptID `json:"stopReceiptId,omitempty"`
	}{run.TaskID, p.Reason, p.Evidence, p.CleanupProof, run.StopReceiptID})
	if err != nil {
		return BackgroundRun{}, err
	}
	attemptEvent, err := insertAttemptEvent(ctx, tx, p.AttemptEventID, attempt, "attempt.failed", now, actorID, payload)
	if err != nil {
		return BackgroundRun{}, err
	}
	taskEvent, err := insertTaskEvent(ctx, tx, p.TaskEventID, owner, "task.failed", now, actorID, payload)
	if err != nil || attemptEvent.Cursor >= taskEvent.Cursor {
		return BackgroundRun{}, fmt.Errorf("insert background run finalization events: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE attempts SET state='failed',terminal_reason=?,revision=revision+1,updated_at=?
WHERE id=? AND task_id=? AND workspace_id=? AND state='prepared' AND revision=?`, p.Reason, now,
		attempt.ID, owner.ID, owner.WorkspaceID, attempt.Revision)
	if err != nil {
		return BackgroundRun{}, err
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRun{}, ErrInvalidState
	}
	result, err = tx.ExecContext(ctx, `UPDATE tasks SET state='failed',terminal_reason=?,latest_event_cursor=?,revision=revision+1,updated_at=?
WHERE id=? AND workspace_id=? AND state='queued' AND current_attempt_id=? AND revision=?`, p.Reason, taskEvent.Cursor, now,
		owner.ID, owner.WorkspaceID, attempt.ID, owner.Revision)
	if err != nil {
		return BackgroundRun{}, err
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRun{}, ErrInvalidState
	}
	phase := BackgroundRunEffectCleanupComplete
	assignments := `state='failed',effect_phase='cleanup_complete',cleanup_completed_at=?,cleanup_proof=?,last_evidence=?,last_error=?,claim_owner=NULL,claim_expires_at=NULL`
	args := []any{now, p.CleanupProof, p.Evidence, p.Reason}
	if preEffect {
		phase = BackgroundRunEffectPreEffectFailed
		assignments = `state='failed',effect_phase='pre_effect_failed',absence_proof=?,last_evidence=?,last_error=?,claim_owner=NULL,claim_expires_at=NULL`
		args = []any{p.CleanupProof, p.Evidence, p.Reason}
	}
	query := `UPDATE background_runs SET ` + assignments + `,revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND state=? AND effect_phase=? AND cancel_epoch=? AND
claim_owner=? AND claim_generation=? AND claim_expires_at>? AND revision=?`
	args = append(args, now, run.TaskID, run.AttemptID, run.WorkspaceID, run.Generation, p.ExpectedState, p.ExpectedPhase,
		p.CancelEpoch, p.ClaimOwner, p.ClaimGeneration, now, p.ExpectedRevision)
	result, err = tx.ExecContext(ctx, query, args...)
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("finalize background run %s: %w", phase, err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRun{}, ErrInvalidState
	}
	stored, err := readBackgroundRunExact(ctx, tx, run.WorkspaceID, run.TaskID)
	if err != nil {
		return BackgroundRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRun{}, err
	}
	return stored, nil
}

func (s *Store) CompleteBackgroundRunResultCleanup(ctx context.Context, p CompleteBackgroundRunResultCleanupParams) (BackgroundRun, error) {
	if p.ExpectedState != BackgroundRunResultReady || p.ExpectedPhase != BackgroundRunEffectCloneRemoved || !validRequiredEvidence(p.CleanupProof) {
		return BackgroundRun{}, fmt.Errorf("%w: background result cleanup", ErrInvalidInput)
	}
	return s.transitionClaimedRun(ctx, p.BackgroundRunClaim, BackgroundRunResultReady, BackgroundRunEffectCleanupComplete,
		`cleanup_completed_at=?,cleanup_proof=?,claim_owner=NULL,claim_expires_at=NULL`, []any{unixMillis(p.Now), p.CleanupProof}, "complete background result cleanup")
}

func (s *Store) transitionClaimedRun(ctx context.Context, claim BackgroundRunClaim, state BackgroundRunState, phase BackgroundRunEffectPhase, assignments string, args []any, operation string) (BackgroundRun, error) {
	if err := validateBackgroundRunClaim(claim); err != nil || !state.valid() || !phase.valid() || !validBackgroundRunStatePhase(BackgroundRunSourceProfile, state, phase) {
		return BackgroundRun{}, fmt.Errorf("%w: background run transition", ErrInvalidInput)
	}
	if len(args) > 0 {
		if evidence, ok := args[len(args)-1].(string); ok && !validOptionalEvidence(evidence) {
			return BackgroundRun{}, fmt.Errorf("%w: background run evidence", ErrInvalidInput)
		}
	}
	return s.updateClaimedRun(ctx, claim, `state=?,effect_phase=?,`+assignments, append([]any{state, phase}, args...), operation)
}

func (s *Store) updateClaimedRun(ctx context.Context, claim BackgroundRunClaim, assignments string, args []any, operation string) (_ BackgroundRun, err error) {
	if err := validateBackgroundRunClaim(claim); err != nil {
		return BackgroundRun{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRun{}, err
	}
	defer release()
	defer rollback(tx, &err)
	databaseState, databasePhase := databaseBackgroundStatePhase(claim.ExpectedState, claim.ExpectedPhase)
	query := `UPDATE background_runs SET ` + assignments + `,revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND claim_owner=? AND claim_generation=? AND
claim_expires_at>? AND revision=? AND state=? AND effect_phase=? AND cancel_epoch=?`
	args = append(args, unixMillis(claim.Now), claim.TaskID, claim.AttemptID, claim.WorkspaceID, claim.Generation,
		claim.ClaimOwner, claim.ClaimGeneration, unixMillis(claim.Now), claim.ExpectedRevision, databaseState, databasePhase, claim.CancelEpoch)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("%s: %w", operation, err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRun{}, ErrInvalidState
	}
	run, err := readBackgroundRunExact(ctx, tx, claim.WorkspaceID, claim.TaskID)
	if err != nil {
		return BackgroundRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRun{}, fmt.Errorf("commit %s: %w", operation, err)
	}
	return run, nil
}

func databaseBackgroundStatePhase(state BackgroundRunState, phase BackgroundRunEffectPhase) (BackgroundRunState, BackgroundRunEffectPhase) {
	if state == BackgroundRunCanceling {
		switch phase {
		case BackgroundRunEffectSealIntent:
			return BackgroundRunCleanupRequired, BackgroundRunEffectStopIntent
		case BackgroundRunEffectExporting:
			return BackgroundRunCleanupRequired, BackgroundRunEffectWriterInactive
		}
	}
	if state == BackgroundRunResultReady && phase == BackgroundRunEffectArtifactCommitted {
		return BackgroundRunResultReady, BackgroundRunEffectWriterInactive
	}
	return state, phase
}

func readBackgroundRunExact(ctx context.Context, q queryRower, workspaceID task.WorkspaceID, taskID task.TaskID) (BackgroundRun, error) {
	run, err := scanBackgroundRun(q.QueryRowContext(ctx, backgroundRunSelect+` WHERE r.workspace_id=? AND r.task_id=?`, workspaceID, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRun{}, ErrNotFound
	}
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("read exact background run: %w", err)
	}
	return run, nil
}

func validateBackgroundClaimRequest(workspaceID task.WorkspaceID, owner, profile, imageIdentity string, now time.Time, duration time.Duration) error {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil || !validBoundedText(owner, 1, 128) ||
		profile != BackgroundRunSourceProfile || !validBackgroundImageIdentity(imageIdentity) || duration <= 0 || duration > maxBackgroundRunLease ||
		unixMillis(now.Add(duration)) <= unixMillis(now) || validExactTimestamp(now) != nil {
		return fmt.Errorf("%w: background run claim", ErrInvalidInput)
	}
	return nil
}

func validateBackgroundRunClaim(claim BackgroundRunClaim) error {
	if _, err := task.ParseWorkspaceID(string(claim.WorkspaceID)); err != nil {
		return fmt.Errorf("%w: background run workspace", ErrInvalidInput)
	}
	if _, err := task.ParseTaskID(string(claim.TaskID)); err != nil {
		return fmt.Errorf("%w: background run task", ErrInvalidInput)
	}
	if _, err := task.ParseAttemptID(string(claim.AttemptID)); err != nil || claim.Generation <= 0 || claim.ClaimGeneration <= 0 ||
		claim.ExpectedRevision <= 0 || claim.CancelEpoch > 1 || !validBoundedText(claim.ClaimOwner, 1, 128) || validExactTimestamp(claim.Now) != nil {
		return fmt.Errorf("%w: background run claim fence", ErrInvalidInput)
	}
	if !claim.ExpectedState.valid() || !claim.ExpectedPhase.valid() || !validBackgroundRunStatePhase(BackgroundRunSourceProfile, claim.ExpectedState, claim.ExpectedPhase) {
		return fmt.Errorf("%w: background run expected state", ErrInvalidInput)
	}
	return nil
}

func validOptionalEvidence(value string) bool {
	return value == "" || validBoundedText(value, 1, 4096)
}

func validRequiredEvidence(value string) bool { return validBoundedText(value, 1, 4096) }
