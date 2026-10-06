package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
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
	if profile != BackgroundRunSourceProfile {
		return BackgroundRun{}, fmt.Errorf("%w: next background run", ErrInvalidInput)
	}
	run, err := scanBackgroundRun(s.db.QueryRowContext(ctx, backgroundRunSelect+`
JOIN tasks t ON t.id=r.task_id AND t.workspace_id=r.workspace_id AND t.current_attempt_id=r.attempt_id
JOIN attempts a ON a.id=r.attempt_id AND a.task_id=r.task_id AND a.workspace_id=r.workspace_id AND a.sequence=r.generation
WHERE r.workspace_id=? AND r.profile=? AND r.effect_phase<>'cleanup_complete' AND
  (r.effect_phase<>'absent' OR NOT EXISTS (
    SELECT 1 FROM background_runs active WHERE active.workspace_id=r.workspace_id AND active.profile=? AND
      active.effect_phase NOT IN ('absent','cleanup_complete')))
ORDER BY CASE WHEN r.effect_phase='absent' THEN 1 ELSE 0 END,r.updated_at,r.task_id LIMIT 1`, workspaceID, profile, profile))
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
	return s.transitionRun(ctx, p, BackgroundRunSettingUp, BackgroundRunEffectProvisioning, `last_error=NULL`, nil, "start background run provisioning")
}

// RecordBackgroundRunRuntime commits the exact started runtime once. It is the
// only stop and cleanup authority for a started container; everything else
// provisioning creates is reconciled by inspection.
func (s *Store) RecordBackgroundRunRuntime(ctx context.Context, p RecordBackgroundRunRuntimeParams) (BackgroundRun, error) {
	if !validBoundedText(p.ContainerID, 1, 128) || !validBoundedText(p.ContainerStartedAt, 1, 64) ||
		p.RuntimeEpoch <= 0 || p.HostPort < 1 || p.HostPort > 65535 || !validRequiredEvidence(p.Evidence) ||
		p.ExpectedState != BackgroundRunSettingUp || p.ExpectedPhase != BackgroundRunEffectProvisioning {
		return BackgroundRun{}, fmt.Errorf("%w: background run runtime", ErrInvalidInput)
	}
	return s.updateRun(ctx, p.BackgroundRunRef,
		`observed_container_id=?,observed_container_started_at=?,runtime_epoch=?,host_port=?,last_evidence=?`,
		[]any{p.ContainerID, p.ContainerStartedAt, p.RuntimeEpoch, p.HostPort, p.Evidence}, "record background run runtime",
		`observed_container_id IS NULL`)
}

// RecordBackgroundRunPromptRequestAttempted is the irreversible pre-I/O fence:
// it ends provisioning before the single prompt POST. A restarted coordinator
// finds prompt_pending and only reconciles; it can never dispatch again.
func (s *Store) RecordBackgroundRunPromptRequestAttempted(ctx context.Context, p BackgroundRunRef) (BackgroundRun, error) {
	if p.ExpectedState != BackgroundRunSettingUp || p.ExpectedPhase != BackgroundRunEffectProvisioning {
		return BackgroundRun{}, fmt.Errorf("%w: background run prompt request attempt", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p, BackgroundRunSettingUp, BackgroundRunEffectPromptPending, `prompt_request_attempted_at=?`,
		[]any{unixMillis(p.Now)}, "record background run prompt request attempt", `prompt_request_attempted_at IS NULL`)
}

func (s *Store) RecordBackgroundRunPromptAdmitted(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectPromptPending || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run prompt admission", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, BackgroundRunWorking, BackgroundRunEffectAdmitted,
		`last_evidence=?`, []any{p.Evidence}, "record background run prompt admission")
}

func (s *Store) RecordBackgroundRunPromptUncertain(ctx context.Context, p RecordBackgroundRunEvidenceParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectPromptPending || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: uncertain background run prompt", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, BackgroundRunUncertain, BackgroundRunEffectPromptPending,
		`last_evidence=?`, []any{p.Evidence}, "record uncertain background run prompt")
}

// RecordBackgroundRunWorkObservation records positive bounded evidence only.
// Callers must not invoke it for an empty active/pending observation.
func (s *Store) RecordBackgroundRunWorkObservation(ctx context.Context, p RecordBackgroundRunEvidenceParams, state BackgroundRunState) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectAdmitted ||
		(p.ExpectedState != BackgroundRunWorking && p.ExpectedState != BackgroundRunNeedsYou && p.ExpectedState != BackgroundRunUncertain) ||
		(state != BackgroundRunWorking && state != BackgroundRunNeedsYou && state != BackgroundRunUncertain) || !validRequiredEvidence(p.Evidence) {
		return BackgroundRun{}, fmt.Errorf("%w: background run work observation", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, state, BackgroundRunEffectAdmitted,
		`last_evidence=?`, []any{p.Evidence}, "record background run work observation")
}

// RequestBackgroundRunTimeout commits a system-owned stop without manufacturing
// a plugin receipt once the attempt deadline has passed. Parent terminalization
// remains coupled to cleanup finality.
func (s *Store) RequestBackgroundRunTimeout(ctx context.Context, p BackgroundRunRef) (_ BackgroundRun, err error) {
	if !rundomain.Classify(rundomain.State(p.ExpectedState), rundomain.Phase(p.ExpectedPhase)).TimeoutEligible {
		return BackgroundRun{}, fmt.Errorf("%w: background run timeout", ErrInvalidInput)
	}
	now := unixMillis(p.Now)
	return s.updateRun(ctx, p, `state='cleanup_required',effect_phase='cleaning',timeout_requested_at=?,last_error='attempt_timeout'`,
		[]any{now}, "request background run timeout", `timeout_requested_at IS NULL`,
		`EXISTS (SELECT 1 FROM attempts a WHERE a.id=background_runs.attempt_id AND a.state='prepared' AND a.deadline<=`+fmt.Sprint(now)+`)`)
}

// MarkBackgroundRunCleanupRequired records a failed effect. An executing run
// moves to cleanup; a run already cleaning keeps its phase so the next pass
// retries, and only a user stop's canceling becomes cleanup_required.
func (s *Store) MarkBackgroundRunCleanupRequired(ctx context.Context, p MarkBackgroundRunCleanupRequiredParams) (BackgroundRun, error) {
	if !validBoundedText(p.Error, 1, 4096) {
		return BackgroundRun{}, fmt.Errorf("%w: background run cleanup failure", ErrInvalidInput)
	}
	state := BackgroundRunCleanupRequired
	switch {
	case p.ExpectedPhase == BackgroundRunEffectCleaning && p.ExpectedState == BackgroundRunResultReady:
		state = BackgroundRunResultReady
	case p.ExpectedPhase == BackgroundRunEffectCleaning,
		rundomain.Classify(rundomain.State(p.ExpectedState), rundomain.Phase(p.ExpectedPhase)).TimeoutEligible:
	default:
		return BackgroundRun{}, fmt.Errorf("%w: background run cleanup failure state", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, state, BackgroundRunEffectCleaning,
		`last_error=?`, []any{p.Error}, "mark background run cleanup required")
}

// FinalizeBackgroundRunFailure atomically closes a cleaned run and its exact
// parent task/attempt once the caller proved every resource absent. It performs
// no external I/O.
func (s *Store) FinalizeBackgroundRunFailure(ctx context.Context, p FinalizeBackgroundRunFailureParams) (_ BackgroundRun, err error) {
	if p.ExpectedPhase != BackgroundRunEffectCleaning ||
		(p.ExpectedState != BackgroundRunCanceling && p.ExpectedState != BackgroundRunCleanupRequired) {
		return BackgroundRun{}, fmt.Errorf("%w: background run finalization", ErrInvalidInput)
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRun{}, err
	}
	defer release()
	defer rollback(tx, &err)

	// The final UPDATE pins the run revision, so the stop receipt and timeout
	// actor read here are the ones the transition commits against.
	run, err := readBackgroundRunExact(ctx, tx, p.WorkspaceID, p.TaskID)
	if err != nil {
		return BackgroundRun{}, err
	}
	owner, err := getTask(ctx, tx, run.TaskID)
	if err != nil {
		return BackgroundRun{}, err
	}
	attempt, err := getAttempt(ctx, tx, p.AttemptID)
	if err != nil {
		return BackgroundRun{}, err
	}
	if owner.WorkspaceID != run.WorkspaceID || attempt.TaskID != owner.ID || attempt.WorkspaceID != owner.WorkspaceID || attempt.Sequence != p.Generation {
		return BackgroundRun{}, ErrInvalidState
	}
	if run.TimeoutRequestedAt != nil && p.Reason != "attempt_timeout" {
		return BackgroundRun{}, ErrInvalidState
	}
	now := unixMillis(p.Now)
	result, err := tx.ExecContext(ctx, `UPDATE attempts SET state='failed',terminal_reason=?,revision=revision+1,updated_at=?
WHERE id=? AND task_id=? AND workspace_id=? AND state='prepared' AND revision=?`, p.Reason, now,
		attempt.ID, owner.ID, owner.WorkspaceID, attempt.Revision)
	if err != nil {
		return BackgroundRun{}, err
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRun{}, ErrInvalidState
	}
	result, err = tx.ExecContext(ctx, `UPDATE tasks SET state='failed',terminal_reason=?,revision=revision+1,updated_at=?
WHERE id=? AND workspace_id=? AND state='queued' AND current_attempt_id=? AND revision=?`, p.Reason, now,
		owner.ID, owner.WorkspaceID, attempt.ID, owner.Revision)
	if err != nil {
		return BackgroundRun{}, err
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRun{}, ErrInvalidState
	}
	result, err = tx.ExecContext(ctx, `UPDATE background_runs SET state='failed',effect_phase='cleanup_complete',cleanup_proof=?,
last_evidence=?,last_error=?,revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND state=? AND effect_phase=? AND revision=?`,
		p.CleanupProof, p.Evidence, p.Reason, now, p.TaskID, p.AttemptID, p.WorkspaceID, p.Generation, p.ExpectedState, p.ExpectedPhase,
		p.ExpectedRevision)
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("finalize background run: %w", err)
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
	if p.ExpectedState != BackgroundRunResultReady || p.ExpectedPhase != BackgroundRunEffectCleaning || !validRequiredEvidence(p.CleanupProof) {
		return BackgroundRun{}, fmt.Errorf("%w: background result cleanup", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, BackgroundRunResultReady, BackgroundRunEffectCleanupComplete,
		`cleanup_proof=?`, []any{p.CleanupProof}, "complete background result cleanup")
}

func (s *Store) transitionRun(ctx context.Context, ref BackgroundRunRef, state BackgroundRunState, phase BackgroundRunEffectPhase, assignments string, args []any, operation string, predicates ...string) (BackgroundRun, error) {
	if !validBackgroundRunStatePhase(BackgroundRunSourceProfile, state, phase) {
		return BackgroundRun{}, fmt.Errorf("%w: background run transition", ErrInvalidInput)
	}
	if len(args) > 0 {
		if evidence, ok := args[len(args)-1].(string); ok && !validOptionalEvidence(evidence) {
			return BackgroundRun{}, fmt.Errorf("%w: background run evidence", ErrInvalidInput)
		}
	}
	return s.updateRun(ctx, ref, `state=?,effect_phase=?,`+assignments, append([]any{state, phase}, args...), operation, predicates...)
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
	query := `UPDATE background_runs SET ` + assignments + `,revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND revision=? AND state=? AND effect_phase=?`
	for _, predicate := range predicates {
		query += ` AND ` + predicate
	}
	args = append(args, unixMillis(ref.Now), ref.TaskID, ref.AttemptID, ref.WorkspaceID, ref.Generation,
		ref.ExpectedRevision, ref.ExpectedState, ref.ExpectedPhase)
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
	if !validBackgroundRunStatePhase(BackgroundRunSourceProfile, ref.ExpectedState, ref.ExpectedPhase) {
		return fmt.Errorf("%w: background run expected state", ErrInvalidInput)
	}
	return nil
}

func validOptionalEvidence(value string) bool {
	return value == "" || validBoundedText(value, 1, 4096)
}

func validRequiredEvidence(value string) bool { return validBoundedText(value, 1, 4096) }
