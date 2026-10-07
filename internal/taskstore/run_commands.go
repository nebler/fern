package taskstore

// This file holds the idempotent run commands: create, stop, and seal. Each
// classifies its receipt, then commits the receipt and run transition together.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	rundomain "github.com/nebler/fern/internal/run"
	"github.com/nebler/fern/internal/task"
)

// Receipt command kinds.
const (
	CreateBackgroundRunCommand = "run.create"
	StopBackgroundRunCommand   = "run.stop"
	SealBackgroundRunCommand   = "run.seal"
)

// BackgroundRunStoppedBeforeStart is the last_error of a run stopped while queued.
const BackgroundRunStoppedBeforeStart = "background_run_stopped_before_start"

// AdmitBackgroundRun atomically claims an idempotency key and creates the run
// and its receipt. It performs no external effects. IDs, the prompt, and the
// environment selection come from Fern itself (runapi validated the HTTP
// input), so only command authority is checked; schema CHECKs bound what is
// persisted.
func (s *Store) AdmitBackgroundRun(ctx context.Context, p AdmitBackgroundRunParams) (_ Admission, err error) {
	if p.Claim.Scope.CommandKind != CreateBackgroundRunCommand || p.Claim.Actor.Type != task.ActorOpenCode ||
		p.Profile != rundomain.SourceProfile {
		return Admission{}, fmt.Errorf("%w: background run admission", ErrInvalidInput)
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return Admission{}, fmt.Errorf("begin run admission: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	workspaceID := p.Claim.Scope.WorkspaceID
	existing, found, err := receiptByKey(ctx, tx, workspaceID, CreateBackgroundRunCommand, p.Claim.Key)
	if err != nil {
		return Admission{}, err
	}
	if found {
		switch existing.classify(p.Claim) {
		case task.IdempotencyReplay:
			run, getErr := readRun(ctx, tx, workspaceID, existing.RunID)
			if getErr != nil {
				return Admission{}, getErr
			}
			if err := tx.Commit(); err != nil {
				return Admission{}, fmt.Errorf("finish admission replay: %w", err)
			}
			return Admission{Run: run, Receipt: existing, Replayed: true}, nil
		case task.IdempotencyOwnerMismatch:
			return Admission{}, ErrIdempotencyOwnerMismatch
		case task.IdempotencyConflict:
			return Admission{}, &ConflictError{ReceiptID: existing.ID, RunID: existing.RunID}
		default:
			return Admission{}, fmt.Errorf("%w: unexpected idempotency disposition", ErrCorruptStore)
		}
	}

	var repositoryID int64
	var repositoryFullName string
	if err := tx.QueryRowContext(ctx, `SELECT repository_id,repository_full_name FROM workspaces WHERE id=?`, workspaceID).
		Scan(&repositoryID, &repositoryFullName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Admission{}, fmt.Errorf("%w: workspace", ErrNotFound)
		}
		return Admission{}, fmt.Errorf("read workspace: %w", err)
	}
	if task.RepositoryID(repositoryID) != p.RepositoryID || p.RepositoryRemote != "https://github.com/"+repositoryFullName {
		return Admission{}, ErrRepositoryMismatch
	}

	branch := any(nil)
	if p.Branch != "" {
		branch = p.Branch
	}
	acceptedMS := unixMillis(p.AcceptedAt)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO runs(
    id,workspace_id,repository_id,repository_remote,base_oid,branch,prompt,agent,model_provider,model,
    deadline,profile,image_identity,environment_sha256,resource_spec_version,opencode_session_id,opencode_message_id,
    creator_actor,state,effect_phase,revision,created_at,updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'queued','absent',1,?,?)`,
		p.RunID, workspaceID, p.RepositoryID, p.RepositoryRemote, p.BaseSHA, branch, p.Prompt,
		p.Agent, p.ModelProvider, p.Model, unixMillis(p.Deadline), p.Profile, p.ImageIdentity, p.EnvironmentSHA256[:],
		rundomain.ResourceSpecVersion, p.OpenCodeSessionID, p.OpenCodeMessageID, encodeActor(p.Claim.Actor),
		acceptedMS, acceptedMS); err != nil {
		return Admission{}, fmt.Errorf("insert run: %w", err)
	}
	receipt, err := insertReceipt(ctx, tx, p.Claim, p.RunID, p.APIContractVersion, p.AcceptedAt, struct {
		RunID     task.RunID `json:"run_id"`
		Committed bool       `json:"committed"`
	}{p.RunID, true})
	if err != nil {
		return Admission{}, err
	}
	run, err := readRun(ctx, tx, workspaceID, p.RunID)
	if err != nil {
		return Admission{}, err
	}
	if err := tx.Commit(); err != nil {
		return Admission{}, fmt.Errorf("commit run admission: %w", err)
	}
	return Admission{Run: run, Receipt: receipt}, nil
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
			if existing.RunID != p.RunID {
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

	run, err := readOwnedRun(ctx, tx, p.WorkspaceID, p.RunID, p.Claim.Actor)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	queuedStop := run.EffectPhase == rundomain.Absent
	activeStop := run.EffectPhase == rundomain.Provisioning || run.EffectPhase == rundomain.PromptPending || run.EffectPhase == rundomain.Admitted
	if run.StopReceiptID != 0 || (!queuedStop && !activeStop) {
		return BackgroundRunStop{}, ErrInvalidState
	}
	stopState := rundomain.Failed
	if activeStop {
		stopState = rundomain.Canceling
	}
	receipt, err := insertReceipt(ctx, tx, p.Claim, run.RunID, p.APIContractVersion, p.StoppedAt, struct {
		RunID task.RunID      `json:"run_id"`
		State rundomain.State `json:"state"`
	}{run.RunID, stopState})
	if err != nil {
		return BackgroundRunStop{}, err
	}
	ref := BackgroundRunRef{WorkspaceID: run.WorkspaceID, RunID: run.RunID, ExpectedRevision: run.Revision,
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

// SealBackgroundRun atomically wins against stop and timeout: it is a
// compare-and-swap on the admitted run's revision, and a sealed run can no
// longer be stopped or timed out. Once input validation has completed, caller
// cancellation cannot split the receipt from the seal.
func (s *Store) SealBackgroundRun(ctx context.Context, p SealBackgroundRunParams) (_ BackgroundRunSealAdmission, err error) {
	if p.WorkspaceID != p.Claim.Scope.WorkspaceID || p.Claim.Scope.CommandKind != SealBackgroundRunCommand || p.Claim.Actor.Type != task.ActorOpenCode {
		return BackgroundRunSealAdmission{}, fmt.Errorf("%w: background seal authority", ErrInvalidInput)
	}
	ctx = context.WithoutCancel(ctx)
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRunSealAdmission{}, fmt.Errorf("begin background run seal: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	existing, found, err := receiptByKey(ctx, tx, p.WorkspaceID, SealBackgroundRunCommand, p.Claim.Key)
	if err != nil {
		return BackgroundRunSealAdmission{}, err
	}
	if found {
		switch existing.classify(p.Claim) {
		case task.IdempotencyOwnerMismatch:
			return BackgroundRunSealAdmission{}, ErrNotFound
		case task.IdempotencyConflict:
			return BackgroundRunSealAdmission{}, ErrIdempotencyConflict
		case task.IdempotencyReplay:
			if existing.RunID != p.RunID {
				return BackgroundRunSealAdmission{}, ErrIdempotencyConflict
			}
			run, getErr := readOwnedRun(ctx, tx, p.WorkspaceID, existing.RunID, p.Claim.Actor)
			if getErr != nil || run.Seal == nil || run.Seal.ReceiptID != existing.ID {
				return BackgroundRunSealAdmission{}, fmt.Errorf("%w: background seal replay", ErrCorruptStore)
			}
			if err := tx.Commit(); err != nil {
				return BackgroundRunSealAdmission{}, err
			}
			return BackgroundRunSealAdmission{Run: run, Receipt: existing, Replayed: true}, nil
		default:
			return BackgroundRunSealAdmission{}, ErrCorruptStore
		}
	}

	run, err := readOwnedRun(ctx, tx, p.WorkspaceID, p.RunID, p.Claim.Actor)
	if err != nil {
		return BackgroundRunSealAdmission{}, err
	}
	if run.Revision != p.ExpectedRunRevision || !sealable(run) {
		return BackgroundRunSealAdmission{}, ErrInvalidState
	}
	receipt, err := insertReceipt(ctx, tx, p.Claim, run.RunID, p.APIContractVersion, p.AcceptedAt, struct {
		RunID    task.RunID    `json:"run_id"`
		ResultID task.ResultID `json:"result_id"`
	}{run.RunID, p.ResultID})
	if err != nil {
		return BackgroundRunSealAdmission{}, err
	}
	ref := BackgroundRunRef{WorkspaceID: run.WorkspaceID, RunID: run.RunID, ExpectedRevision: run.Revision,
		ExpectedState: run.State, ExpectedPhase: run.EffectPhase, Now: p.AcceptedAt}
	stored, err := updateRunTx(ctx, tx, ref, `state='canceling',effect_phase='sealing',
seal_receipt_id=?,seal_requested_at=?,seal_policy_version=?,result_id=?`,
		[]any{receipt.ID, unixMillis(p.AcceptedAt), p.PolicyVersion, p.ResultID}, "bind background seal",
		`stop_receipt_id IS NULL`, `timeout_requested_at IS NULL`, `seal_receipt_id IS NULL`)
	if err != nil {
		return BackgroundRunSealAdmission{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRunSealAdmission{}, fmt.Errorf("commit background run seal: %w", err)
	}
	return BackgroundRunSealAdmission{Run: stored, Receipt: receipt}, nil
}

// sealable reports whether run has an admitted prompt and no stop, timeout, or
// earlier seal has claimed it.
func sealable(run BackgroundRun) bool {
	claimed := run.Seal != nil || run.TimeoutRequestedAt != nil || run.StopReceiptID != 0
	return run.EffectPhase == rundomain.Admitted && admittedState(run.State) && !claimed
}
