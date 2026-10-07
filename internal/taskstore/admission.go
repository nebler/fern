package taskstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

const CreateBackgroundRunCommand = "run.create"

// AdmitBackgroundRun atomically claims an idempotency key and creates the run
// and its receipt. It performs no external effects. IDs, the prompt, and the
// environment selection come from Fern itself (runapi validated the HTTP
// input), so only command authority is checked; schema CHECKs bound what is
// persisted.
func (s *Store) AdmitBackgroundRun(ctx context.Context, p AdmitBackgroundRunParams) (_ Admission, err error) {
	if p.Claim.Scope.CommandKind != CreateBackgroundRunCommand || p.Claim.Actor.Type != task.ActorOpenCode ||
		p.Profile != BackgroundRunSourceProfile {
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

	var workspaceState WorkspaceState
	var repositoryID int64
	var repositoryFullName string
	if err := tx.QueryRowContext(ctx, `SELECT state,repository_id,repository_full_name FROM workspaces WHERE id=?`, workspaceID).
		Scan(&workspaceState, &repositoryID, &repositoryFullName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Admission{}, fmt.Errorf("%w: workspace", ErrNotFound)
		}
		return Admission{}, fmt.Errorf("read workspace: %w", err)
	}
	if workspaceState != WorkspaceActive {
		return Admission{}, ErrWorkspaceUnavailable
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
		backgroundRunResourceSpecVersion, p.OpenCodeSessionID, p.OpenCodeMessageID, encodeActor(p.Claim.Actor),
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
