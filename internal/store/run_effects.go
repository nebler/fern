package store

// This file holds the coordinator's effect transitions. Each is a
// compare-and-swap on a RunRef.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nebler/fern/internal/domain"
)

// NextRun returns the run the coordinator should advance next. It
// prefers recovery of an existing effect over consuming the workspace's one
// provisioning slot. It is a plain read: the host lease admits one coordinator
// per workspace, and every later mutation is a revision compare-and-swap.
func (s *Store) NextRun(ctx context.Context, workspaceID domain.WorkspaceID, profile string) (Run, error) {
	if profile != domain.SourceProfile {
		return Run{}, fmt.Errorf("%w: next background run", ErrInvalidInput)
	}
	run, err := scanRun(s.db.QueryRowContext(ctx, runSelect+`
WHERE r.workspace_id=? AND r.profile=? AND r.effect_phase<>'cleanup_complete' AND
  (r.effect_phase<>'absent' OR NOT EXISTS (
    SELECT 1 FROM runs active WHERE active.workspace_id=r.workspace_id AND
      active.effect_phase NOT IN ('absent','cleanup_complete')))
ORDER BY CASE WHEN r.effect_phase='absent' THEN 1 ELSE 0 END,r.updated_at,r.id LIMIT 1`, workspaceID, profile))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("find next background run: %w", err)
	}
	return run, nil
}

// NextRunWork pairs NextRun with its prompt plaintext.
func (s *Store) NextRunWork(ctx context.Context, workspaceID domain.WorkspaceID, profile string) (RunWork, error) {
	run, err := s.NextRun(ctx, workspaceID, profile)
	if err != nil {
		return RunWork{}, err
	}
	var prompt string
	if err := s.db.QueryRowContext(ctx, `SELECT prompt FROM runs WHERE workspace_id=? AND id=?`, run.WorkspaceID, run.RunID).
		Scan(&prompt); err != nil {
		return RunWork{}, fmt.Errorf("read run prompt: %w", err)
	}
	return RunWork{Run: run, Prompt: prompt}, nil
}

// StartRunProvisioning consumes the workspace's provisioning slot
// before any external I/O. The capacity index rejects a second active run.
func (s *Store) StartRunProvisioning(ctx context.Context, p RunRef) (Run, error) {
	if p.ExpectedState != domain.Queued || p.ExpectedPhase != domain.Absent {
		return Run{}, fmt.Errorf("%w: background run provisioning", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p, domain.SettingUp, domain.Provisioning, `last_error=NULL`, nil, "start background run provisioning")
}

// RecordRunRuntime commits the exact started runtime once. It is the
// only stop and cleanup authority for a started container; everything else
// provisioning creates is reconciled by inspection.
func (s *Store) RecordRunRuntime(ctx context.Context, p RecordRunRuntimeParams) (Run, error) {
	if !p.valid() {
		return Run{}, fmt.Errorf("%w: background run runtime", ErrInvalidInput)
	}
	return s.updateRun(ctx, p.RunRef,
		`observed_container_id=?,observed_container_started_at=?,runtime_epoch=?,host_port=?,last_evidence=?`,
		[]any{p.ContainerID, p.ContainerStartedAt, p.RuntimeEpoch, p.HostPort, p.Evidence}, "record background run runtime",
		`observed_container_id IS NULL`)
}

func (p RecordRunRuntimeParams) valid() bool {
	return p.ExpectedState == domain.SettingUp && p.ExpectedPhase == domain.Provisioning &&
		validBoundedText(p.ContainerID, 1, 128) &&
		validBoundedText(p.ContainerStartedAt, 1, 64) &&
		p.RuntimeEpoch > 0 &&
		p.HostPort >= 1 && p.HostPort <= 65535 &&
		validRequiredEvidence(p.Evidence)
}

// RecordRunPromptRequestAttempted is the irreversible pre-I/O fence:
// it ends provisioning before the single prompt POST. A restarted coordinator
// finds prompt_pending and only reconciles; it can never dispatch again.
func (s *Store) RecordRunPromptRequestAttempted(ctx context.Context, p RunRef) (Run, error) {
	if p.ExpectedState != domain.SettingUp || p.ExpectedPhase != domain.Provisioning {
		return Run{}, fmt.Errorf("%w: background run prompt request attempt", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p, domain.SettingUp, domain.PromptPending, `prompt_request_attempted_at=?`,
		[]any{unixMillis(p.Now)}, "record background run prompt request attempt", `prompt_request_attempted_at IS NULL`)
}

func (s *Store) RecordRunPromptAdmitted(ctx context.Context, p RecordRunEvidenceParams) (Run, error) {
	if p.ExpectedPhase != domain.PromptPending || !validRequiredEvidence(p.Evidence) {
		return Run{}, fmt.Errorf("%w: background run prompt admission", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.RunRef, domain.Working, domain.Admitted,
		`last_evidence=?`, []any{p.Evidence}, "record background run prompt admission")
}

func (s *Store) RecordRunPromptUncertain(ctx context.Context, p RecordRunEvidenceParams) (Run, error) {
	if p.ExpectedPhase != domain.PromptPending || !validRequiredEvidence(p.Evidence) {
		return Run{}, fmt.Errorf("%w: uncertain background run prompt", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.RunRef, domain.Uncertain, domain.PromptPending,
		`last_evidence=?`, []any{p.Evidence}, "record uncertain background run prompt")
}

// RecordRunWorkObservation records positive bounded evidence only.
// Callers must not invoke it for an empty active/pending observation.
func (s *Store) RecordRunWorkObservation(ctx context.Context, p RecordRunEvidenceParams, state domain.State) (Run, error) {
	if p.ExpectedPhase != domain.Admitted || !admittedState(p.ExpectedState) || !admittedState(state) || !validRequiredEvidence(p.Evidence) {
		return Run{}, fmt.Errorf("%w: background run work observation", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.RunRef, state, domain.Admitted,
		`last_evidence=?`, []any{p.Evidence}, "record background run work observation")
}

// RequestRunTimeout commits a system-owned stop without manufacturing
// a plugin receipt once the run deadline has passed. Parent terminalization
// remains coupled to cleanup finality.
func (s *Store) RequestRunTimeout(ctx context.Context, p RunRef) (_ Run, err error) {
	if !domain.Classify(p.ExpectedState, p.ExpectedPhase).TimeoutEligible {
		return Run{}, fmt.Errorf("%w: background run timeout", ErrInvalidInput)
	}
	now := unixMillis(p.Now)
	return s.updateRun(ctx, p, `state='cleanup_required',effect_phase='cleaning',timeout_requested_at=?,last_error='run_timeout'`,
		[]any{now}, "request background run timeout", `timeout_requested_at IS NULL`,
		`deadline<=`+fmt.Sprint(now))
}

// MarkRunCleanupRequired records a failed effect. An executing run
// moves to cleanup; a run already cleaning keeps its phase so the next pass
// retries, and only a user stop's canceling becomes cleanup_required.
func (s *Store) MarkRunCleanupRequired(ctx context.Context, p MarkRunCleanupRequiredParams) (Run, error) {
	if !validBoundedText(p.Error, 1, 4096) {
		return Run{}, fmt.Errorf("%w: background run cleanup failure", ErrInvalidInput)
	}
	state := domain.CleanupRequired
	switch {
	case p.ExpectedPhase == domain.Cleaning && p.ExpectedState == domain.ResultReady:
		state = domain.ResultReady
	case p.ExpectedPhase == domain.Cleaning,
		domain.Classify(p.ExpectedState, p.ExpectedPhase).TimeoutEligible:
	default:
		return Run{}, fmt.Errorf("%w: background run cleanup failure state", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.RunRef, state, domain.Cleaning,
		`last_error=?`, []any{p.Error}, "mark background run cleanup required")
}

// FinalizeRunFailure closes a cleaned run once the caller proved
// every resource absent. It performs no external I/O. A timed-out run can only
// finalize with the timeout reason.
func (s *Store) FinalizeRunFailure(ctx context.Context, p FinalizeRunFailureParams) (Run, error) {
	if p.ExpectedPhase != domain.Cleaning || !validBoundedText(p.Reason, 1, 4096) ||
		(p.ExpectedState != domain.Canceling && p.ExpectedState != domain.CleanupRequired) {
		return Run{}, fmt.Errorf("%w: background run finalization", ErrInvalidInput)
	}
	var predicates []string
	if p.Reason != "run_timeout" {
		predicates = append(predicates, `timeout_requested_at IS NULL`)
	}
	return s.transitionRun(ctx, p.RunRef, domain.Failed, domain.CleanupComplete,
		`cleanup_proof=?,last_error=?,last_evidence=?`, []any{p.CleanupProof, p.Reason, p.Evidence}, "finalize background run", predicates...)
}

func (s *Store) CompleteRunResultCleanup(ctx context.Context, p CompleteRunResultCleanupParams) (Run, error) {
	if p.ExpectedState != domain.ResultReady || p.ExpectedPhase != domain.Cleaning || !validRequiredEvidence(p.CleanupProof) {
		return Run{}, fmt.Errorf("%w: background result cleanup", ErrInvalidInput)
	}
	return s.transitionRun(ctx, p.RunRef, domain.ResultReady, domain.CleanupComplete,
		`cleanup_proof=?`, []any{p.CleanupProof}, "complete background result cleanup")
}

func (s *Store) transitionRun(ctx context.Context, ref RunRef, state domain.State, phase domain.Phase, assignments string, args []any, operation string, predicates ...string) (Run, error) {
	if !domain.Classify(state, phase).Valid {
		return Run{}, fmt.Errorf("%w: background run transition", ErrInvalidInput)
	}
	if len(args) > 0 {
		if evidence, ok := args[len(args)-1].(string); ok && !validOptionalEvidence(evidence) {
			return Run{}, fmt.Errorf("%w: background run evidence", ErrInvalidInput)
		}
	}
	return s.updateRun(ctx, ref, `state=?,effect_phase=?,`+assignments, append([]any{state, phase}, args...), operation, predicates...)
}

// updateRun is the revision compare-and-swap behind every coordinator
// transition. Extra predicates guard one-way fields the revision alone does
// not describe.
func (s *Store) updateRun(ctx context.Context, ref RunRef, assignments string, args []any, operation string, predicates ...string) (_ Run, err error) {
	if err := validateRunRef(ref); err != nil {
		return Run{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return Run{}, err
	}
	defer release()
	defer rollback(tx, &err)
	run, err := updateRunTx(ctx, tx, ref, assignments, args, operation, predicates...)
	if err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, fmt.Errorf("commit %s: %w", operation, err)
	}
	return run, nil
}

func updateRunTx(ctx context.Context, tx *sql.Tx, ref RunRef, assignments string, args []any, operation string, predicates ...string) (Run, error) {
	query := `UPDATE runs SET ` + assignments + `,revision=revision+1,updated_at=?
WHERE id=? AND workspace_id=? AND revision=? AND state=? AND effect_phase=?`
	for _, predicate := range predicates {
		query += ` AND ` + predicate
	}
	args = append(args, unixMillis(ref.Now), ref.RunID, ref.WorkspaceID, ref.ExpectedRevision, ref.ExpectedState, ref.ExpectedPhase)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return Run{}, fmt.Errorf("%s: %w", operation, err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return Run{}, ErrInvalidState
	}
	return readRun(ctx, tx, ref.WorkspaceID, ref.RunID)
}

// admittedState reports whether state is one an admitted prompt may report:
// working, needs_you, or uncertain.
func admittedState(state domain.State) bool {
	return domain.Classify(state, domain.Admitted).Valid
}

// matches reports whether run is still at the exact revision, state, and
// phase the ref pinned.
func (ref RunRef) matches(run Run) bool {
	return run.Revision == ref.ExpectedRevision && run.State == ref.ExpectedState && run.EffectPhase == ref.ExpectedPhase
}

func validateRunRef(ref RunRef) error {
	if !domain.Classify(ref.ExpectedState, ref.ExpectedPhase).Valid {
		return fmt.Errorf("%w: background run expected state", ErrInvalidInput)
	}
	return nil
}

func validOptionalEvidence(value string) bool {
	return value == "" || validBoundedText(value, 1, 4096)
}

func validRequiredEvidence(value string) bool { return validBoundedText(value, 1, 4096) }

// RecordRunWriterFence durably records, once per seal, that the
// exact writer no longer runs. A runtime_stopped fence must name the run's
// committed runtime (a schema CHECK); the other kinds require that no runtime
// was ever committed.
func (s *Store) RecordRunWriterFence(ctx context.Context, p RecordRunWriterFenceParams) (Run, error) {
	if p.ExpectedState != domain.Canceling || p.ExpectedPhase != domain.Sealing {
		return Run{}, fmt.Errorf("%w: writer fence revision", ErrInvalidInput)
	}
	var stoppedAt any
	if p.StoppedAt != nil {
		stoppedAt = unixMillis(*p.StoppedAt)
	}
	predicate := `observed_container_id IS NULL`
	if p.Kind == WriterFenceRuntimeStopped {
		predicate = `observed_container_id IS NOT NULL`
	}
	return s.updateRun(ctx, p.RunRef,
		`writer_fence_kind=?,writer_fence_container_id=?,writer_fence_started_at=?,writer_fence_token=?,writer_fence_stopped_at=?,last_evidence=?`,
		[]any{p.Kind, nullIfEmpty(p.ContainerID), nullIfEmpty(p.ContainerStartedAt), nullIfEmpty(p.RuntimeToken), stoppedAt,
			"writer_fence:" + string(p.Kind)},
		"record writer fence", `seal_receipt_id IS NOT NULL`, `writer_fence_kind IS NULL`, predicate)
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
