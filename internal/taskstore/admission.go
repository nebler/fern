package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	runidentity "github.com/nebler/fern/internal/run"
	"github.com/nebler/fern/internal/task"
)

const CreateBackgroundRunCommand = "run.create"

// AdmitBackgroundRun atomically claims an idempotency key and creates the
// Background Run's task, sequence-1 attempt, receipt, and initial events. It
// performs no external effects.
func (s *Store) AdmitBackgroundRun(ctx context.Context, p AdmitBackgroundRunParams) (_ Admission, err error) {
	if err := validateAdmission(p); err != nil {
		return Admission{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return Admission{}, fmt.Errorf("begin task admission: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	existing, found, err := receiptByKey(ctx, tx, p.Claim.Scope.WorkspaceID, p.Claim.Scope.CommandKind, p.Claim.Key)
	if err != nil {
		return Admission{}, err
	}
	if found {
		switch existing.classify(p.Claim) {
		case task.IdempotencyReplay:
			storedTask, getErr := getTask(ctx, tx, existing.TargetID)
			if getErr != nil {
				return Admission{}, getErr
			}
			storedAttempt, getErr := getAttempt(ctx, tx, storedTask.CurrentAttemptID)
			if getErr != nil {
				return Admission{}, getErr
			}
			if err := tx.Commit(); err != nil {
				return Admission{}, fmt.Errorf("finish admission replay: %w", err)
			}
			return Admission{Task: storedTask, Attempt: storedAttempt, Receipt: existing, Replayed: true}, nil
		case task.IdempotencyOwnerMismatch:
			return Admission{}, ErrIdempotencyOwnerMismatch
		case task.IdempotencyConflict:
			return Admission{}, &ConflictError{ReceiptID: existing.ID, TargetID: existing.TargetID}
		default:
			return Admission{}, fmt.Errorf("%w: unexpected idempotency disposition", ErrCorruptStore)
		}
	}

	var workspaceState WorkspaceState
	var repositoryID int64
	var repositoryFullName string
	if err := tx.QueryRowContext(ctx, `SELECT state,repository_id,repository_full_name FROM workspaces WHERE id=?`, p.Claim.Scope.WorkspaceID).Scan(&workspaceState, &repositoryID, &repositoryFullName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Admission{}, fmt.Errorf("%w: workspace", ErrNotFound)
		}
		return Admission{}, fmt.Errorf("read workspace: %w", err)
	}
	if workspaceState != WorkspaceActive {
		return Admission{}, ErrWorkspaceUnavailable
	}
	if task.RepositoryID(repositoryID) != p.RepositoryID || p.BackgroundRun.RepositoryRemote != "https://github.com/"+repositoryFullName {
		return Admission{}, ErrRepositoryMismatch
	}

	actor := encodeActor(p.Claim.Actor)
	promptHash := sha256.Sum256([]byte(p.Prompt))
	attemptImage, attemptProtocol := p.BackgroundRun.ImageIdentity, p.BackgroundRun.Profile
	acceptedMS := unixMillis(p.AcceptedAt)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tasks(
    id,workspace_id,title,prompt,prompt_sha256,repository_id,base_ref,base_sha,
    object_format,state,current_attempt_id,revision,created_at,updated_at
) VALUES(?,?,?,?,?,?,?,?,?,'queued',?,1,?,?)`,
		p.TaskID, p.Claim.Scope.WorkspaceID, p.Title, p.Prompt, promptHash[:], p.RepositoryID,
		p.BaseRef, p.BaseSHA, p.ObjectFormat, p.AttemptID, acceptedMS, acceptedMS); err != nil {
		return Admission{}, fmt.Errorf("insert task: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO attempts(
    id,task_id,workspace_id,sequence,state,opencode_session_id,opencode_message_id,prompt_sha256,
    base_sha,image_digest,opencode_protocol,execution_contract_version,agent,
    model_provider,model,deadline,revision,created_at,updated_at
) VALUES(?, ?, ?, 1, 'prepared', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		p.AttemptID, p.TaskID, p.Claim.Scope.WorkspaceID, p.OpenCodeSessionID, p.OpenCodeMessageID, promptHash[:], p.BaseSHA,
		attemptImage, attemptProtocol, p.ExecutionContractVersion, p.Agent, p.ModelProvider, p.Model,
		unixMillis(p.Deadline), acceptedMS, acceptedMS); err != nil {
		return Admission{}, fmt.Errorf("insert attempt: %w", err)
	}
	response, err := json.Marshal(struct {
		RunID     task.TaskID `json:"run_id"`
		Committed bool        `json:"committed"`
	}{p.TaskID, true})
	if err != nil {
		return Admission{}, fmt.Errorf("encode receipt projection: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO receipts(
    id,workspace_id,command_kind,state,idempotency_key,request_hash,actor,
    accepted_at,api_contract_version,target_type,target_id,response_status,response_projection
) VALUES(?,?,?,'accepted',?,?,?,?,?,'task',?,202,?)`,
		p.ReceiptID, p.Claim.Scope.WorkspaceID, p.Claim.Scope.CommandKind, p.Claim.Key,
		p.Claim.RequestHash[:], actor, acceptedMS, p.APIContractVersion, p.TaskID, string(response)); err != nil {
		return Admission{}, fmt.Errorf("insert receipt: %w", err)
	}
	branch := any(nil)
	if p.BackgroundRun.Branch != "" {
		branch = p.BackgroundRun.Branch
	}
	resources := runidentity.NewResources(p.TaskID)
	profileHash := sha256.Sum256([]byte(p.BackgroundRun.Profile))
	if _, err := tx.ExecContext(ctx, `
INSERT INTO background_runs(
    task_id,attempt_id,workspace_id,generation,repository_id,repository_remote,base_oid,branch,
    instruction_sha256,profile,profile_sha256,environment_sha256,resource_spec_version,image_identity,clone_identity,volume_identity,
    container_identity,endpoint_identity,opencode_session_id,opencode_message_id,state,effect_phase,
    creator_actor,revision,created_at,updated_at
) VALUES(?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'queued','absent',?,1,?,?)`,
		p.TaskID, p.AttemptID, p.Claim.Scope.WorkspaceID, p.RepositoryID, p.BackgroundRun.RepositoryRemote,
		p.BaseSHA, branch, promptHash[:], p.BackgroundRun.Profile,
		profileHash[:], p.BackgroundRun.EnvironmentSHA256[:], backgroundRunResourceSpecVersion, p.BackgroundRun.ImageIdentity, resources.Clone(),
		resources.Volume(), resources.Container(), resources.Endpoint(),
		p.OpenCodeSessionID, p.OpenCodeMessageID, actor, acceptedMS, acceptedMS); err != nil {
		return Admission{}, fmt.Errorf("insert background run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Admission{}, fmt.Errorf("commit task admission: %w", err)
	}

	storedTask := Task{
		ID: p.TaskID, WorkspaceID: p.Claim.Scope.WorkspaceID, Title: p.Title, Prompt: p.Prompt,
		PromptSHA256: promptHash, RepositoryID: p.RepositoryID, BaseRef: p.BaseRef, BaseSHA: p.BaseSHA,
		ObjectFormat: p.ObjectFormat, State: task.TaskQueued, CurrentAttemptID: p.AttemptID, Revision: 1,
		CreatedAt: fromUnixMillis(acceptedMS), UpdatedAt: fromUnixMillis(acceptedMS),
	}
	storedAttempt := Attempt{
		ID: p.AttemptID, TaskID: p.TaskID, WorkspaceID: p.Claim.Scope.WorkspaceID, Sequence: 1, State: task.AttemptPrepared,
		OpenCodeSessionID: p.OpenCodeSessionID, OpenCodeMessageID: p.OpenCodeMessageID,
		PromptSHA256: promptHash, BaseSHA: p.BaseSHA, ImageDigest: attemptImage, OpenCodeProtocol: attemptProtocol,
		ExecutionContractVersion: p.ExecutionContractVersion, Agent: p.Agent, ModelProvider: p.ModelProvider,
		Model: p.Model, Deadline: fromUnixMillis(unixMillis(p.Deadline)),
		Revision: 1, CreatedAt: fromUnixMillis(acceptedMS), UpdatedAt: fromUnixMillis(acceptedMS),
	}
	receipt := Receipt{
		ID: p.ReceiptID, WorkspaceID: p.Claim.Scope.WorkspaceID, CommandKind: p.Claim.Scope.CommandKind,
		State: ReceiptAccepted, IdempotencyKey: p.Claim.Key, RequestHash: p.Claim.RequestHash, Actor: p.Claim.Actor,
		AcceptedAt: fromUnixMillis(acceptedMS), APIContractVersion: p.APIContractVersion,
		TargetType: "task", TargetID: p.TaskID, ResponseStatus: 202, ResponseProjection: response,
	}
	return Admission{Task: storedTask, Attempt: storedAttempt, Receipt: receipt}, nil
}

// validateAdmission checks only command authority. IDs, the prompt, and the
// environment selection come from Fern itself (runapi validated the HTTP
// input); schema CHECKs bound what is persisted.
func validateAdmission(p AdmitBackgroundRunParams) error {
	if p.Claim.Scope.CommandKind != CreateBackgroundRunCommand || p.Claim.Actor.Type != task.ActorOpenCode || p.BackgroundRun == nil ||
		p.BackgroundRun.Profile != BackgroundRunSourceProfile {
		return fmt.Errorf("%w: background run admission", ErrInvalidInput)
	}
	return nil
}

func receiptByKey(ctx context.Context, tx *sql.Tx, workspaceID task.WorkspaceID, kind string, key task.IdempotencyKey) (Receipt, bool, error) {
	row := tx.QueryRowContext(ctx, receiptSelect+` WHERE r.workspace_id=? AND r.command_kind=? AND r.idempotency_key=?`, workspaceID, kind, key)
	receipt, err := scanReceipt(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, fmt.Errorf("read idempotency receipt: %w", err)
	}
	return receipt, true, nil
}
