package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	rundomain "github.com/nebler/fern/internal/run"
	"github.com/nebler/fern/internal/task"
)

// NextBackgroundRun returns the run the coordinator should advance next. It
// prefers recovery of an existing effect over consuming the workspace's one
// provisioning slot. It is a plain read: the host lease admits one coordinator
// per workspace, and every later mutation is a revision compare-and-swap.
func (s *Store) NextBackgroundRun(ctx context.Context, workspaceID task.WorkspaceID, profile string) (BackgroundRun, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil || profile != BackgroundRunSourceProfile {
		return BackgroundRun{}, fmt.Errorf("%w: next background run", ErrInvalidInput)
	}
	run, err := scanBackgroundRun(s.db.QueryRowContext(ctx, backgroundRunSelect+`
JOIN tasks t ON t.id=r.task_id AND t.workspace_id=r.workspace_id AND t.current_attempt_id=r.attempt_id
JOIN attempts a ON a.id=r.attempt_id AND a.task_id=r.task_id AND a.workspace_id=r.workspace_id AND a.sequence=r.generation
WHERE r.workspace_id=? AND r.profile=? AND r.state<>'failed' AND
	NOT (r.state='result_ready' AND r.effect_phase='cleanup_complete') AND
  ((r.state='queued' AND NOT EXISTS (
      SELECT 1 FROM background_runs active WHERE active.workspace_id=r.workspace_id AND active.profile=? AND
		active.effect_phase NOT IN ('absent','cleanup_complete','pre_effect_failed')
	    )) OR r.state IN ('setting_up','working','needs_you','uncertain','canceling','cleanup_required','result_ready'))
ORDER BY CASE WHEN r.state='queued' THEN 1 ELSE 0 END,r.updated_at,r.task_id LIMIT 1`, workspaceID, profile, profile))
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRun{}, ErrNotFound
	}
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("find next background run: %w", err)
	}
	return run, nil
}

// NextBackgroundRunWork pairs NextBackgroundRun with its task plaintext after
// rechecking the digest binding.
func (s *Store) NextBackgroundRunWork(ctx context.Context, workspaceID task.WorkspaceID, profile string) (BackgroundRunWork, error) {
	run, err := s.NextBackgroundRun(ctx, workspaceID, profile)
	if err != nil {
		return BackgroundRunWork{}, err
	}
	return s.readBackgroundRunWork(ctx, run)
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

// StartBackgroundRunProvisioning consumes the workspace's provisioning slot
// before any external I/O. The capacity index rejects a second active run.
func (s *Store) StartBackgroundRunProvisioning(ctx context.Context, p BackgroundRunRef) (BackgroundRun, error) {
	if p.ExpectedState != BackgroundRunQueued || p.ExpectedPhase != BackgroundRunEffectAbsent {
		return BackgroundRun{}, fmt.Errorf("%w: background run provisioning", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p, BackgroundRunSettingUp, BackgroundRunEffectProvisionIntent,
		`provision_intent_at=?`, []any{unixMillis(p.Now)}, "start background run provisioning")
}

func (s *Store) RecordBackgroundRunCloneObserved(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectProvisionIntent || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run clone observation", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, BackgroundRunSettingUp, BackgroundRunEffectCloneObserved,
		`clone_observed_at=?,clone_evidence=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence, p.Evidence}, "record background run clone observation")
}

func (s *Store) RecordBackgroundRunVolumeObserved(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectCloneObserved || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run volume observation", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, BackgroundRunSettingUp, BackgroundRunEffectVolumeObserved,
		`volume_observed_at=?,volume_evidence=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence, p.Evidence}, "record background run volume observation")
}

func (s *Store) RecordBackgroundRunContainerObserved(ctx context.Context, p RecordBackgroundRunContainerObservedParams) (BackgroundRun, error) {
	if !validBoundedText(p.ContainerID, 1, 128) || !validBoundedText(p.ContainerStartedAt, 1, 64) ||
		p.RuntimeEpoch <= 0 || p.HostPort < 1 || p.HostPort > 65535 || !validRequiredEvidence(p.Evidence) || p.ExpectedPhase != BackgroundRunEffectVolumeObserved {
		return BackgroundRun{}, fmt.Errorf("%w: background run observation", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, BackgroundRunSettingUp, BackgroundRunEffectContainerObserved,
		`observed_container_id=?,observed_container_started_at=?,runtime_epoch=?,host_port=?,container_observed_at=?,last_evidence=?`,
		[]any{p.ContainerID, p.ContainerStartedAt, p.RuntimeEpoch, p.HostPort, unixMillis(p.Now), p.Evidence}, "record background run provision observation")
}

func (s *Store) RecordBackgroundRunHealthObserved(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectContainerObserved || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run health observation", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, BackgroundRunSettingUp, BackgroundRunEffectHealthObserved,
		`health_observed_at=?,health_evidence=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence, p.Evidence}, "record background run health observation")
}

func (s *Store) RecordBackgroundRunReady(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectHealthObserved || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run readiness", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, BackgroundRunSettingUp, BackgroundRunEffectReady,
		`ready_at=?,ready_evidence=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence, p.Evidence}, "record background run readiness")
}

func (s *Store) RecordBackgroundRunSessionObserved(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectReady || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run session observation", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, BackgroundRunSettingUp, BackgroundRunEffectSessionObserved,
		`session_observed_at=?,session_evidence=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence, p.Evidence}, "record background run session observation")
}

func (s *Store) RecordBackgroundRunPromptIntent(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectSessionObserved || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run prompt intent", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, BackgroundRunUncertain, BackgroundRunEffectPromptIntent,
		`prompt_intent_at=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence}, "record background run prompt intent")
}

// RecordBackgroundRunPromptRequestAttempted is the irreversible pre-I/O fence.
// A restarted coordinator can observe it but can never set or change it twice.
func (s *Store) RecordBackgroundRunPromptRequestAttempted(ctx context.Context, p BackgroundRunRef) (BackgroundRun, error) {
	if p.ExpectedState != BackgroundRunUncertain || p.ExpectedPhase != BackgroundRunEffectPromptIntent {
		return BackgroundRun{}, fmt.Errorf("%w: background run prompt request attempt", ErrInvalidInput)
	}
	return s.updateRun(ctx, p, `prompt_request_attempted_at=?`, []any{unixMillis(p.Now)}, "record background run prompt request attempt",
		`prompt_request_attempted_at IS NULL`)
}

func (s *Store) RecordBackgroundRunPromptAdmitted(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectPromptIntent || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run prompt admission", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, BackgroundRunWorking, BackgroundRunEffectPromptAdmitted,
		`prompt_admitted_at=?,prompt_evidence=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence, p.Evidence}, "record background run prompt admission")
}

func (s *Store) RecordBackgroundRunPromptUncertain(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectPromptIntent || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: uncertain background run prompt", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, BackgroundRunUncertain, BackgroundRunEffectPromptIntent,
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
	return s.transitionRun(ctx, p.BackgroundRunRef, state, BackgroundRunEffectPromptAdmitted,
		`last_evidence=?`, []any{p.Evidence}, "record background run work observation")
}

// RequestBackgroundRunTimeout commits a system-owned stop without manufacturing
// a plugin receipt. Parent terminalization remains coupled to cleanup finality.
func (s *Store) RequestBackgroundRunTimeout(ctx context.Context, p RequestBackgroundRunTimeoutParams) (_ BackgroundRun, err error) {
	if err := validateBackgroundRunRef(p.BackgroundRunRef); err != nil || p.Actor.Validate() != nil || p.Actor.Type != task.ActorSystem {
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
		run.State != p.ExpectedState || run.EffectPhase != p.ExpectedPhase ||
		run.TimeoutRequestedAt != nil || !rundomain.Classify(rundomain.State(run.State), rundomain.Phase(run.EffectPhase)).TimeoutEligible {
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
revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND revision=? AND state=? AND effect_phase=?`,
		now, actorID, now, now, run.TaskID, run.AttemptID, run.WorkspaceID, run.Generation, run.Revision, run.State, run.EffectPhase)
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

func (s *Store) RecordBackgroundRunWriterInactive(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectStopIntent || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run writer inactivity", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, p.ExpectedState, BackgroundRunEffectWriterInactive,
		`writer_inactive_at=?,writer_inactive_evidence=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence, p.Evidence}, "record background run writer inactivity")
}

func (s *Store) RequestBackgroundRunResultCleanup(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedState != BackgroundRunResultReady || p.ExpectedPhase != BackgroundRunEffectArtifactCommitted || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background result cleanup intent", ErrInvalidInput)
	}
	return s.updateRetainedRun(ctx, p.BackgroundRunRef, `result_authority_phase='cleanup',last_evidence=?`, []any{p.Evidence}, "request background result cleanup")
}

func (s *Store) updateRetainedRun(ctx context.Context, ref BackgroundRunRef, assignments string, args []any, operation string) (_ BackgroundRun, err error) {
	if err := validateBackgroundRunRef(ref); err != nil {
		return BackgroundRun{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRun{}, err
	}
	defer release()
	defer rollback(tx, &err)
	query := `UPDATE background_runs SET ` + assignments + `,revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND revision=? AND state='result_ready' AND
effect_phase='writer_inactive' AND result_authority_phase='artifact_committed'`
	args = append(args, unixMillis(ref.Now), ref.TaskID, ref.AttemptID, ref.WorkspaceID, ref.Generation, ref.ExpectedRevision)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("%s: %w", operation, err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRun{}, ErrInvalidState
	}
	run, err := readBackgroundRunExact(ctx, tx, ref.WorkspaceID, ref.TaskID)
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
	return s.transitionRun(ctx, p.BackgroundRunRef, p.ExpectedState, to,
		timestamp+`=?,`+evidenceColumn+`=?,last_evidence=?`, []any{unixMillis(p.Now), p.Evidence, p.Evidence}, "record background run "+operation)
}

func (s *Store) MarkBackgroundRunCleanupRequired(ctx context.Context, p MarkBackgroundRunCleanupRequiredParams) (BackgroundRun, error) {
	if !validBoundedText(p.Error, 1, 4096) {
		return BackgroundRun{}, fmt.Errorf("%w: background run cleanup failure", ErrInvalidInput)
	}
	if p.ExpectedState == BackgroundRunResultReady && p.ExpectedPhase == BackgroundRunEffectArtifactCommitted {
		return s.updateRetainedRun(ctx, p.BackgroundRunRef, `last_error=?`, []any{p.Error}, "retain failed background result cleanup")
	}
	lifecycle := rundomain.Classify(rundomain.State(p.ExpectedState), rundomain.Phase(p.ExpectedPhase))
	if lifecycle.CleanupStep {
		state := p.ExpectedState
		if state == BackgroundRunCanceling {
			state = BackgroundRunCleanupRequired
		}
		if state != BackgroundRunCleanupRequired && state != BackgroundRunResultReady {
			return BackgroundRun{}, fmt.Errorf("%w: background run cleanup failure state", ErrInvalidInput)
		}
		return s.transitionRun(ctx, p.BackgroundRunRef, state, p.ExpectedPhase,
			`last_error=?`, []any{p.Error}, "retain failed background run cleanup phase")
	}
	validState := p.ExpectedState == BackgroundRunSettingUp || p.ExpectedState == BackgroundRunWorking ||
		p.ExpectedState == BackgroundRunNeedsYou || p.ExpectedState == BackgroundRunUncertain
	if !validState || p.ExpectedPhase == BackgroundRunEffectAbsent {
		return BackgroundRun{}, fmt.Errorf("%w: background run cleanup failure state", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, BackgroundRunCleanupRequired, BackgroundRunEffectStopIntent,
		`stop_intent_at=?,last_error=?`, []any{unixMillis(p.Now), p.Error}, "mark background run cleanup required")
}

// FinalizeBackgroundRunFailure atomically closes an active run and its exact
// parent task/attempt after cleanup, or before any effect with explicit absence
// proof. It performs no external I/O.
func (s *Store) FinalizeBackgroundRunFailure(ctx context.Context, p FinalizeBackgroundRunFailureParams) (_ BackgroundRun, err error) {
	preEffect := p.ExpectedPhase == BackgroundRunEffectProvisionIntent
	cleaned := p.ExpectedPhase == BackgroundRunEffectCloneRemoved
	if err := validateBackgroundRunRef(p.BackgroundRunRef); err != nil || (!preEffect && !cleaned) ||
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
		run.State != p.ExpectedState || run.EffectPhase != p.ExpectedPhase {
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
	assignments := `state='failed',effect_phase='cleanup_complete',cleanup_completed_at=?,cleanup_proof=?,last_evidence=?,last_error=?`
	args := []any{now, p.CleanupProof, p.Evidence, p.Reason}
	if preEffect {
		phase = BackgroundRunEffectPreEffectFailed
		assignments = `state='failed',effect_phase='pre_effect_failed',absence_proof=?,last_evidence=?,last_error=?`
		args = []any{p.CleanupProof, p.Evidence, p.Reason}
	}
	query := `UPDATE background_runs SET ` + assignments + `,revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND state=? AND effect_phase=? AND revision=?`
	args = append(args, now, run.TaskID, run.AttemptID, run.WorkspaceID, run.Generation, p.ExpectedState, p.ExpectedPhase,
		p.ExpectedRevision)
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
	return s.transitionRun(ctx, p.BackgroundRunRef, BackgroundRunResultReady, BackgroundRunEffectCleanupComplete,
		`cleanup_completed_at=?,cleanup_proof=?`, []any{unixMillis(p.Now), p.CleanupProof}, "complete background result cleanup")
}

func (s *Store) transitionRun(ctx context.Context, ref BackgroundRunRef, state BackgroundRunState, phase BackgroundRunEffectPhase, assignments string, args []any, operation string) (BackgroundRun, error) {
	if !validBackgroundRunStatePhase(BackgroundRunSourceProfile, state, phase) {
		return BackgroundRun{}, fmt.Errorf("%w: background run transition", ErrInvalidInput)
	}
	if len(args) > 0 {
		if evidence, ok := args[len(args)-1].(string); ok && !validOptionalEvidence(evidence) {
			return BackgroundRun{}, fmt.Errorf("%w: background run evidence", ErrInvalidInput)
		}
	}
	return s.updateRun(ctx, ref, `state=?,effect_phase=?,`+assignments, append([]any{state, phase}, args...), operation)
}

// updateRun is the revision compare-and-swap behind every coordinator
// transition. Extra predicates guard one-way fields the revision alone does
// not describe.
func (s *Store) updateRun(ctx context.Context, ref BackgroundRunRef, assignments string, args []any, operation string, predicates ...string) (_ BackgroundRun, err error) {
	if err := validateBackgroundRunRef(ref); err != nil {
		return BackgroundRun{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRun{}, err
	}
	defer release()
	defer rollback(tx, &err)
	databaseState, databasePhase := databaseBackgroundStatePhase(ref.ExpectedState, ref.ExpectedPhase)
	query := `UPDATE background_runs SET ` + assignments + `,revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND revision=? AND state=? AND effect_phase=?`
	for _, predicate := range predicates {
		query += ` AND ` + predicate
	}
	args = append(args, unixMillis(ref.Now), ref.TaskID, ref.AttemptID, ref.WorkspaceID, ref.Generation,
		ref.ExpectedRevision, databaseState, databasePhase)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("%s: %w", operation, err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRun{}, ErrInvalidState
	}
	run, err := readBackgroundRunExact(ctx, tx, ref.WorkspaceID, ref.TaskID)
	if err != nil {
		return BackgroundRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRun{}, fmt.Errorf("commit %s: %w", operation, err)
	}
	return run, nil
}

// databaseBackgroundStatePhase maps the in-memory tuple projected by
// scanBackgroundRun from result_authority_phase back to the stored tuple.
func databaseBackgroundStatePhase(state BackgroundRunState, phase BackgroundRunEffectPhase) (BackgroundRunState, BackgroundRunEffectPhase) {
	if state == BackgroundRunCanceling && phase == BackgroundRunEffectSealIntent {
		return BackgroundRunCleanupRequired, BackgroundRunEffectStopIntent
	}
	// Exporting projects only the phase, so the reachable in-memory tuple is
	// (cleanup_required, exporting); the stored state stays cleanup_required.
	if (state == BackgroundRunCleanupRequired || state == BackgroundRunCanceling) && phase == BackgroundRunEffectExporting {
		return BackgroundRunCleanupRequired, BackgroundRunEffectWriterInactive
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

func validateBackgroundRunRef(ref BackgroundRunRef) error {
	if _, err := task.ParseWorkspaceID(string(ref.WorkspaceID)); err != nil {
		return fmt.Errorf("%w: background run workspace", ErrInvalidInput)
	}
	if _, err := task.ParseTaskID(string(ref.TaskID)); err != nil {
		return fmt.Errorf("%w: background run task", ErrInvalidInput)
	}
	if _, err := task.ParseAttemptID(string(ref.AttemptID)); err != nil || ref.Generation <= 0 ||
		ref.ExpectedRevision <= 0 || validExactTimestamp(ref.Now) != nil {
		return fmt.Errorf("%w: background run revision", ErrInvalidInput)
	}
	if !validBackgroundRunStatePhase(BackgroundRunSourceProfile, ref.ExpectedState, ref.ExpectedPhase) {
		return fmt.Errorf("%w: background run expected state", ErrInvalidInput)
	}
	return nil
}

func validOptionalEvidence(value string) bool {
	return value == "" || validBoundedText(value, 1, 4096)
}

func validRequiredEvidence(value string) bool { return validBoundedText(value, 1, 4096) }
