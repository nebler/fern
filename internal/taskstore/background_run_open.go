package taskstore

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/nebler/fern/internal/task"
)

const OpenBackgroundRunCommand = "run.open"

// OpenBackgroundRun records a replayable read projection. The transaction has
// no external effect and does not change the run lifecycle.
func (s *Store) OpenBackgroundRun(ctx context.Context, p OpenBackgroundRunParams) (_ BackgroundRunOpen, err error) {
	if err := validateBackgroundRunOpen(p); err != nil {
		return BackgroundRunOpen{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRunOpen{}, err
	}
	defer release()
	defer rollback(tx, &err)
	existing, found, err := receiptByKey(ctx, tx, p.WorkspaceID, OpenBackgroundRunCommand, p.Claim.Key)
	if err != nil {
		return BackgroundRunOpen{}, err
	}
	if found {
		disposition, classifyErr := task.ClassifyIdempotency(&task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: existing.WorkspaceID, CommandKind: existing.CommandKind}, Key: existing.IdempotencyKey, RequestHash: existing.RequestHash, Actor: existing.Actor}, p.Claim)
		if classifyErr != nil {
			return BackgroundRunOpen{}, classifyErr
		}
		switch disposition {
		case task.IdempotencyReplay:
			if existing.TargetID != p.TaskID {
				return BackgroundRunOpen{}, ErrIdempotencyConflict
			}
			run, getErr := getBackgroundRunOwned(ctx, tx, p.WorkspaceID, p.TaskID, p.Claim.Actor)
			if getErr != nil {
				return BackgroundRunOpen{}, getErr
			}
			if err := tx.Commit(); err != nil {
				return BackgroundRunOpen{}, err
			}
			return BackgroundRunOpen{Run: run, Receipt: existing, Replayed: true}, nil
		case task.IdempotencyOwnerMismatch:
			return BackgroundRunOpen{}, ErrNotFound
		case task.IdempotencyConflict:
			return BackgroundRunOpen{}, ErrIdempotencyConflict
		default:
			return BackgroundRunOpen{}, ErrCorruptStore
		}
	}
	run, err := getBackgroundRunOwned(ctx, tx, p.WorkspaceID, p.TaskID, p.Claim.Actor)
	if err != nil {
		return BackgroundRunOpen{}, err
	}
	if !backgroundRunOpenEligible(run) {
		return BackgroundRunOpen{}, ErrInvalidState
	}
	actorID, err := ensureActor(ctx, tx, p.Claim.Actor)
	if err != nil {
		return BackgroundRunOpen{}, err
	}
	projection, _ := json.Marshal(struct {
		RunID task.TaskID `json:"run_id"`
		URL   string      `json:"url"`
	}{run.TaskID, p.URL})
	now := unixMillis(p.OpenedAt)
	if _, err := tx.ExecContext(ctx, `INSERT INTO receipts(
id,workspace_id,command_kind,state,idempotency_key,request_hash,actor_snapshot_id,accepted_at,
api_contract_version,target_type,target_id,response_status,response_projection)
VALUES(?,?,?,'accepted',?,?,?,?,?,'task',?,200,?)`, p.ReceiptID, p.WorkspaceID, OpenBackgroundRunCommand,
		p.Claim.Key, p.Claim.RequestHash[:], actorID, now, p.APIContractVersion, p.TaskID, string(projection)); err != nil {
		return BackgroundRunOpen{}, fmt.Errorf("insert background run open receipt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRunOpen{}, err
	}
	receipt := Receipt{ID: p.ReceiptID, WorkspaceID: p.WorkspaceID, CommandKind: OpenBackgroundRunCommand,
		State: ReceiptAccepted, IdempotencyKey: p.Claim.Key, RequestHash: p.Claim.RequestHash, Actor: p.Claim.Actor,
		AcceptedAt: fromUnixMillis(now), APIContractVersion: p.APIContractVersion, TargetType: "task", TargetID: p.TaskID,
		ResponseStatus: 200, ResponseProjection: projection}
	return BackgroundRunOpen{Run: run, Receipt: receipt}, nil
}

func backgroundRunOpenEligible(run BackgroundRun) bool {
	active := run.State == BackgroundRunSettingUp || run.State == BackgroundRunWorking || run.State == BackgroundRunNeedsYou || run.State == BackgroundRunUncertain
	phase := run.EffectPhase == BackgroundRunEffectSessionObserved || run.EffectPhase == BackgroundRunEffectPromptIntent || run.EffectPhase == BackgroundRunEffectPromptAdmitted
	return active && phase && run.SessionObservedAt != nil && run.CancelEpoch == 0
}

func validateBackgroundRunOpen(p OpenBackgroundRunParams) error {
	if _, err := task.ParseWorkspaceID(string(p.WorkspaceID)); err != nil || p.WorkspaceID != p.Claim.Scope.WorkspaceID {
		return fmt.Errorf("%w: run workspace", ErrInvalidInput)
	}
	if _, err := task.ParseTaskID(string(p.TaskID)); err != nil {
		return fmt.Errorf("%w: run ID", ErrInvalidInput)
	}
	if _, err := task.ParseReceiptID(string(p.ReceiptID)); err != nil {
		return fmt.Errorf("%w: receipt ID", ErrInvalidInput)
	}
	if err := p.Claim.Validate(); err != nil || p.Claim.Scope.CommandKind != OpenBackgroundRunCommand || p.Claim.Actor.Type != task.ActorOpenCode {
		return fmt.Errorf("%w: run open claim", ErrInvalidInput)
	}
	parsed, err := url.Parse(p.URL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Host == "" || parsed.Fragment != "" || parsed.RawQuery != "" || len(p.URL) > 4096 {
		return fmt.Errorf("%w: run open URL", ErrInvalidInput)
	}
	if !validBoundedText(p.APIContractVersion, 1, 64) {
		return fmt.Errorf("%w: API contract version", ErrInvalidInput)
	}
	if err := validExactTimestamp(p.OpenedAt); err != nil {
		return err
	}
	return nil
}
