package taskstore

import (
	"context"
	"fmt"

	"github.com/nebler/fern/internal/run"
	"github.com/nebler/fern/internal/task"
)

const StopBackgroundRunCommand = "run.stop"
const BackgroundRunStoppedBeforeStart = "background_run_stopped_before_start"

const backgroundRunResourceSpecVersion = run.ResourceSpecVersion

// GetBackgroundRun reads one run. Plugin actors see only runs they created;
// a foreign run reads as not found.
func (s *Store) GetBackgroundRun(ctx context.Context, workspaceID task.WorkspaceID, taskID task.TaskID, actor task.ActorSnapshot) (BackgroundRun, error) {
	if !backgroundRunReader(actor.Type) {
		return BackgroundRun{}, fmt.Errorf("%w: background run reader", ErrInvalidInput)
	}
	if actor.Type == task.ActorOpenCode {
		return readOwnedRun(ctx, s.db, workspaceID, taskID, actor)
	}
	return readRun(ctx, s.db, workspaceID, taskID)
}

// ReadBackgroundRunLifecycle returns the (state, phase) of one run for the
// trusted in-process coordinator. It authorizes no actor.
func (s *Store) ReadBackgroundRunLifecycle(ctx context.Context, workspaceID task.WorkspaceID, taskID task.TaskID) (BackgroundRunState, BackgroundRunEffectPhase, error) {
	var state BackgroundRunState
	var phase BackgroundRunEffectPhase
	if err := s.db.QueryRowContext(ctx, `SELECT state,effect_phase FROM runs WHERE workspace_id=? AND id=?`, workspaceID, taskID).
		Scan(&state, &phase); err != nil {
		return "", "", fmt.Errorf("read run lifecycle: %w", err)
	}
	return state, phase, nil
}

const MaxBackgroundRunListLimit = 100

// ListBackgroundRuns applies plugin ownership in SQL before its bound. Trusted
// operator/device actors receive the workspace-wide operator projection.
func (s *Store) ListBackgroundRuns(ctx context.Context, workspaceID task.WorkspaceID, actor task.ActorSnapshot, limit int) ([]BackgroundRun, error) {
	if !backgroundRunReader(actor.Type) || limit < 1 || limit > MaxBackgroundRunListLimit {
		return nil, fmt.Errorf("%w: background run list", ErrInvalidInput)
	}
	query := runSelect + ` WHERE r.workspace_id=?`
	arguments := []any{workspaceID}
	if actor.Type == task.ActorOpenCode {
		query += ownedBy
		arguments = append(arguments, ownerArgs(actor)...)
	}
	query += ` ORDER BY r.created_at DESC,r.id DESC LIMIT ?`
	arguments = append(arguments, limit)
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("list background runs: %w", err)
	}
	defer rows.Close()
	runs := make([]BackgroundRun, 0)
	for rows.Next() {
		run, scanErr := scanRun(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan background run list: %w", scanErr)
		}
		if len(runs) == cap(runs) {
			// Grow only after a successful scan, without exceeding the query bound.
			// Starting at one avoids reserving excess space for sparse lists.
			grown := make([]BackgroundRun, len(runs), min(limit, max(1, 2*cap(runs))))
			copy(grown, runs)
			runs = grown
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate background run list: %w", err)
	}
	return runs, nil
}

func backgroundRunReader(actorType task.ActorType) bool {
	return actorType == task.ActorOpenCode || actorType == task.ActorDevice || actorType == task.ActorOperator
}

// StopBackgroundRun commits a user stop. A queued run becomes terminal at
// once; an executing run moves to cleaning. It is a compare-and-swap on the
// run revision, so exactly one of a racing stop and seal wins.
func (s *Store) StopBackgroundRun(ctx context.Context, p StopBackgroundRunParams) (_ BackgroundRunStop, err error) {
	if p.WorkspaceID != p.Claim.Scope.WorkspaceID || p.Claim.Scope.CommandKind != StopBackgroundRunCommand || p.Claim.Actor.Type != task.ActorOpenCode {
		return BackgroundRunStop{}, fmt.Errorf("%w: run stop claim", ErrInvalidInput)
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRunStop{}, fmt.Errorf("begin background run stop: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	existing, found, err := receiptByKey(ctx, tx, p.WorkspaceID, StopBackgroundRunCommand, p.Claim.Key)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	if found {
		switch existing.classify(p.Claim) {
		case task.IdempotencyReplay:
			if existing.RunID != p.TaskID {
				return BackgroundRunStop{}, ErrIdempotencyConflict
			}
			run, getErr := readOwnedRun(ctx, tx, p.WorkspaceID, existing.RunID, p.Claim.Actor)
			if getErr != nil || run.StopReceiptID != existing.ID {
				return BackgroundRunStop{}, fmt.Errorf("%w: background run stop replay", ErrCorruptStore)
			}
			if err := tx.Commit(); err != nil {
				return BackgroundRunStop{}, err
			}
			return BackgroundRunStop{Run: run, Receipt: existing, Replayed: true}, nil
		case task.IdempotencyOwnerMismatch:
			return BackgroundRunStop{}, ErrNotFound
		case task.IdempotencyConflict:
			return BackgroundRunStop{}, ErrIdempotencyConflict
		default:
			return BackgroundRunStop{}, ErrCorruptStore
		}
	}

	run, err := readOwnedRun(ctx, tx, p.WorkspaceID, p.TaskID, p.Claim.Actor)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	queuedStop := run.EffectPhase == BackgroundRunEffectAbsent
	activeStop := run.EffectPhase == BackgroundRunEffectProvisioning || run.EffectPhase == BackgroundRunEffectPromptPending || run.EffectPhase == BackgroundRunEffectAdmitted
	if run.StopReceiptID != 0 || (!queuedStop && !activeStop) {
		return BackgroundRunStop{}, ErrInvalidState
	}
	stopState := BackgroundRunFailed
	if activeStop {
		stopState = BackgroundRunCanceling
	}
	receipt, err := insertReceipt(ctx, tx, p.Claim, run.TaskID, p.APIContractVersion, p.StoppedAt, struct {
		RunID task.TaskID        `json:"run_id"`
		State BackgroundRunState `json:"state"`
	}{run.TaskID, stopState})
	if err != nil {
		return BackgroundRunStop{}, err
	}
	ref := BackgroundRunRef{WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, ExpectedRevision: run.Revision,
		ExpectedState: run.State, ExpectedPhase: run.EffectPhase, Now: p.StoppedAt}
	now := unixMillis(p.StoppedAt)
	var stored BackgroundRun
	if activeStop {
		stored, err = updateRunTx(ctx, tx, ref, `state='canceling',effect_phase='cleaning',stop_receipt_id=?,stop_requested_at=?`,
			[]any{receipt.ID, now}, "request active background run stop", `stop_receipt_id IS NULL`)
	} else {
		stored, err = updateRunTx(ctx, tx, ref, `state='failed',effect_phase='cleanup_complete',stop_receipt_id=?,stop_requested_at=?,
cleanup_proof='queued:no_effect_claim',last_error=?`, []any{receipt.ID, now, BackgroundRunStoppedBeforeStart},
			"stop queued background run", `stop_receipt_id IS NULL`)
	}
	if err != nil {
		return BackgroundRunStop{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRunStop{}, fmt.Errorf("commit background run stop: %w", err)
	}
	return BackgroundRunStop{Run: stored, Receipt: receipt}, nil
}
