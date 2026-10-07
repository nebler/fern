package taskstore

import (
	"context"
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
	run, err := scanRun(s.db.QueryRowContext(ctx, runSelect+`
WHERE r.workspace_id=? AND r.profile=? AND r.effect_phase<>'cleanup_complete' AND
  (r.effect_phase<>'absent' OR NOT EXISTS (
    SELECT 1 FROM runs active WHERE active.workspace_id=r.workspace_id AND
      active.effect_phase NOT IN ('absent','cleanup_complete')))
ORDER BY CASE WHEN r.effect_phase='absent' THEN 1 ELSE 0 END,r.updated_at,r.id LIMIT 1`, workspaceID, profile))
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRun{}, ErrNotFound
	}
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("find next background run: %w", err)
	}
	return run, nil
}

// NextBackgroundRunWork pairs NextBackgroundRun with its prompt plaintext.
func (s *Store) NextBackgroundRunWork(ctx context.Context, workspaceID task.WorkspaceID, profile string) (BackgroundRunWork, error) {
	run, err := s.NextBackgroundRun(ctx, workspaceID, profile)
	if err != nil {
		return BackgroundRunWork{}, err
	}
	var prompt string
	if err := s.db.QueryRowContext(ctx, `SELECT prompt FROM runs WHERE workspace_id=? AND id=?`, run.WorkspaceID, run.TaskID).
		Scan(&prompt); err != nil {
		return BackgroundRunWork{}, fmt.Errorf("read run prompt: %w", err)
	}
	return BackgroundRunWork{Run: run, Prompt: prompt}, nil
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
	if !rundomain.Classify(p.ExpectedState, p.ExpectedPhase).TimeoutEligible {
		return BackgroundRun{}, fmt.Errorf("%w: background run timeout", ErrInvalidInput)
	}
	now := unixMillis(p.Now)
	return s.updateRun(ctx, p, `state='cleanup_required',effect_phase='cleaning',timeout_requested_at=?,last_error='attempt_timeout'`,
		[]any{now}, "request background run timeout", `timeout_requested_at IS NULL`,
		`deadline<=`+fmt.Sprint(now))
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
		rundomain.Classify(p.ExpectedState, p.ExpectedPhase).TimeoutEligible:
	default:
		return BackgroundRun{}, fmt.Errorf("%w: background run cleanup failure state", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, state, BackgroundRunEffectCleaning,
		`last_error=?`, []any{p.Error}, "mark background run cleanup required")
}

// FinalizeBackgroundRunFailure closes a cleaned run once the caller proved
// every resource absent. It performs no external I/O. A timed-out run can only
// finalize with the timeout reason.
func (s *Store) FinalizeBackgroundRunFailure(ctx context.Context, p FinalizeBackgroundRunFailureParams) (BackgroundRun, error) {
	if p.ExpectedPhase != BackgroundRunEffectCleaning || !validBoundedText(p.Reason, 1, 4096) ||
		(p.ExpectedState != BackgroundRunCanceling && p.ExpectedState != BackgroundRunCleanupRequired) {
		return BackgroundRun{}, fmt.Errorf("%w: background run finalization", ErrInvalidInput)
	}
	var predicates []string
	if p.Reason != "attempt_timeout" {
		predicates = append(predicates, `timeout_requested_at IS NULL`)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, BackgroundRunFailed, BackgroundRunEffectCleanupComplete,
		`cleanup_proof=?,last_error=?,last_evidence=?`, []any{p.CleanupProof, p.Reason, p.Evidence}, "finalize background run", predicates...)
}

func (s *Store) CompleteBackgroundRunResultCleanup(ctx context.Context, p CompleteBackgroundRunResultCleanupParams) (BackgroundRun, error) {
	if p.ExpectedState != BackgroundRunResultReady || p.ExpectedPhase != BackgroundRunEffectCleaning || !validRequiredEvidence(p.CleanupProof) {
		return BackgroundRun{}, fmt.Errorf("%w: background result cleanup", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.BackgroundRunRef, BackgroundRunResultReady, BackgroundRunEffectCleanupComplete,
		`cleanup_proof=?`, []any{p.CleanupProof}, "complete background result cleanup")
}

func (s *Store) transitionRun(ctx context.Context, ref BackgroundRunRef, state BackgroundRunState, phase BackgroundRunEffectPhase, assignments string, args []any, operation string, predicates ...string) (BackgroundRun, error) {
	if !rundomain.Classify(state, phase).Valid {
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
	run, err := updateRunTx(ctx, tx, ref, assignments, args, operation, predicates...)
	if err != nil {
		return BackgroundRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRun{}, fmt.Errorf("commit %s: %w", operation, err)
	}
	return run, nil
}

func updateRunTx(ctx context.Context, tx *sql.Tx, ref BackgroundRunRef, assignments string, args []any, operation string, predicates ...string) (BackgroundRun, error) {
	query := `UPDATE runs SET ` + assignments + `,revision=revision+1,updated_at=?
WHERE id=? AND workspace_id=? AND revision=? AND state=? AND effect_phase=?`
	for _, predicate := range predicates {
		query += ` AND ` + predicate
	}
	args = append(args, unixMillis(ref.Now), ref.TaskID, ref.WorkspaceID, ref.ExpectedRevision, ref.ExpectedState, ref.ExpectedPhase)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("%s: %w", operation, err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRun{}, ErrInvalidState
	}
	return readRun(ctx, tx, ref.WorkspaceID, ref.TaskID)
}

func validateBackgroundRunRef(ref BackgroundRunRef) error {
	if !rundomain.Classify(ref.ExpectedState, ref.ExpectedPhase).Valid {
		return fmt.Errorf("%w: background run expected state", ErrInvalidInput)
	}
	return nil
}

func validOptionalEvidence(value string) bool {
	return value == "" || validBoundedText(value, 1, 4096)
}

func validRequiredEvidence(value string) bool { return validBoundedText(value, 1, 4096) }
