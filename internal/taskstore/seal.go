package taskstore

import (
	"context"
	"fmt"
	"time"

	"github.com/nebler/fern/internal/task"
)

const SealBackgroundRunCommand = "run.seal"

// Seal is the durable seal admission recorded on its run. Its receipt holds
// the requesting actor, idempotency key, and request hash.
type Seal struct {
	ReceiptID     int64
	ResultID      task.ResultID
	RequestedAt   time.Time
	PolicyVersion string
}

// CommitEpochSeconds is the normalized Git commit time of the retained
// snapshot, fixed at seal admission so every export pass is deterministic.
func (s Seal) CommitEpochSeconds() int64 { return s.RequestedAt.Unix() }

type SealBackgroundRunParams struct {
	WorkspaceID         task.WorkspaceID
	RunID               task.RunID
	ExpectedRunRevision int64
	ResultID            task.ResultID
	Claim               task.IdempotencyClaim
	PolicyVersion       string
	APIContractVersion  string
	AcceptedAt          time.Time
}

type BackgroundRunSealAdmission struct {
	Run      BackgroundRun
	Receipt  Receipt
	Replayed bool
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
	if run.Revision != p.ExpectedRunRevision || run.EffectPhase != BackgroundRunEffectAdmitted ||
		(run.State != BackgroundRunWorking && run.State != BackgroundRunNeedsYou && run.State != BackgroundRunUncertain) ||
		run.Seal != nil || run.TimeoutRequestedAt != nil || run.StopReceiptID != 0 {
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

type WriterFenceKind string

const (
	WriterFenceNeverCreated   WriterFenceKind = "never_created"
	WriterFenceNeverStarted   WriterFenceKind = "never_started"
	WriterFenceRuntimeStopped WriterFenceKind = "runtime_stopped"
)

// WriterFence is the once-recorded proof that a sealed run's exact writer no
// longer runs. Export reads the clone only under it.
type WriterFence struct {
	Kind               WriterFenceKind
	ContainerID        string
	ContainerStartedAt string
	RuntimeToken       string
	StoppedAt          *time.Time
}

type RecordBackgroundRunWriterFenceParams struct {
	BackgroundRunRef
	WriterFence
}

// RecordBackgroundRunWriterFence durably records, once per seal, that the
// exact writer no longer runs. A runtime_stopped fence must name the run's
// committed runtime (a schema CHECK); the other kinds require that no runtime
// was ever committed.
func (s *Store) RecordBackgroundRunWriterFence(ctx context.Context, p RecordBackgroundRunWriterFenceParams) (BackgroundRun, error) {
	if p.ExpectedState != BackgroundRunCanceling || p.ExpectedPhase != BackgroundRunEffectSealing {
		return BackgroundRun{}, fmt.Errorf("%w: writer fence revision", ErrInvalidInput)
	}
	var stoppedAt any
	if p.StoppedAt != nil {
		stoppedAt = unixMillis(*p.StoppedAt)
	}
	predicate := `observed_container_id IS NULL`
	if p.Kind == WriterFenceRuntimeStopped {
		predicate = `observed_container_id IS NOT NULL`
	}
	return s.updateRun(ctx, p.BackgroundRunRef,
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
