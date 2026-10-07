package taskstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	rundomain "github.com/nebler/fern/internal/run"
	"github.com/nebler/fern/internal/task"
)

type rowScanner interface {
	Scan(...any) error
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// actorRecord is the stored JSON form of an actor snapshot. Actors are
// attribution on the row that names them, not an interned table.
type actorRecord struct {
	Type           task.ActorType `json:"type"`
	ID             string         `json:"id"`
	DisplayName    string         `json:"display_name"`
	CredentialID   string         `json:"credential_id"`
	Authentication string         `json:"authentication"`
	RequestID      string         `json:"request_id"`
}

func encodeActor(actor task.ActorSnapshot) string {
	encoded, _ := json.Marshal(actorRecord(actor))
	return string(encoded)
}

func decodeActor(value string) (task.ActorSnapshot, error) {
	var record actorRecord
	if err := json.Unmarshal([]byte(value), &record); err != nil {
		return task.ActorSnapshot{}, fmt.Errorf("%w: actor: %v", ErrCorruptStore, err)
	}
	return task.ActorSnapshot(record), nil
}

// ownedBy restricts a run query (aliased r) to one authenticated authority;
// display name and request ID are not authority.
const ownedBy = ` AND json_extract(r.creator_actor,'$.type')=? AND json_extract(r.creator_actor,'$.id')=?
AND json_extract(r.creator_actor,'$.credential_id')=? AND json_extract(r.creator_actor,'$.authentication')=?`

func ownerArgs(actor task.ActorSnapshot) []any {
	return []any{actor.Type, actor.ID, actor.CredentialID, actor.Authentication}
}

const runSelect = `
SELECT r.id,r.workspace_id,r.repository_id,r.repository_remote,r.base_oid,r.branch,
       r.agent,r.model_provider,r.model,r.deadline,r.profile,r.image_identity,r.environment_sha256,r.resource_spec_version,
       r.opencode_session_id,r.opencode_message_id,r.creator_actor,r.state,r.effect_phase,
       r.stop_receipt_id,r.stop_requested_at,r.timeout_requested_at,
       r.observed_container_id,r.observed_container_started_at,r.runtime_epoch,r.host_port,r.prompt_request_attempted_at,
       r.seal_receipt_id,r.seal_requested_at,r.seal_policy_version,r.result_id,
       r.writer_fence_kind,r.writer_fence_container_id,r.writer_fence_started_at,r.writer_fence_token,r.writer_fence_stopped_at,
       r.last_evidence,r.last_error,r.cleanup_proof,r.revision,r.created_at,r.updated_at
FROM runs r`

// scanRun reads one run row. Schema CHECKs keep the runtime, stop, seal, and
// writer-fence column groups all-or-nothing, so they are not re-checked here.
func scanRun(row rowScanner) (BackgroundRun, error) {
	var run BackgroundRun
	var branch, containerID, containerStarted, sealPolicy, resultID sql.NullString
	var fenceKind, fenceContainer, fenceStarted, fenceToken, evidence, lastError, cleanupProof sql.NullString
	var stopReceipt, stopAt, timeoutAt, runtimeEpoch, hostPort, promptAttempted sql.NullInt64
	var sealReceipt, sealRequested, fenceStopped sql.NullInt64
	var environmentHash []byte
	var deadline, created, updated int64
	var creator string
	err := row.Scan(&run.RunID, &run.WorkspaceID, &run.RepositoryID, &run.RepositoryRemote, &run.BaseOID, &branch,
		&run.Agent, &run.ModelProvider, &run.Model, &deadline, &run.Profile, &run.ImageIdentity, &environmentHash, &run.ResourceSpecVersion,
		&run.OpenCodeSessionID, &run.OpenCodeMessageID, &creator, &run.State, &run.EffectPhase,
		&stopReceipt, &stopAt, &timeoutAt,
		&containerID, &containerStarted, &runtimeEpoch, &hostPort, &promptAttempted,
		&sealReceipt, &sealRequested, &sealPolicy, &resultID,
		&fenceKind, &fenceContainer, &fenceStarted, &fenceToken, &fenceStopped,
		&evidence, &lastError, &cleanupProof, &run.Revision, &created, &updated)
	if err != nil {
		return BackgroundRun{}, err
	}
	copy(run.EnvironmentSHA256[:], environmentHash)
	resources := rundomain.NewResources(run.RunID)
	run.CloneIdentity, run.VolumeIdentity, run.ContainerIdentity, run.EndpointIdentity =
		resources.Clone(), resources.Volume(), resources.Container(), resources.Endpoint()
	run.Branch = nullableString(branch)
	run.Deadline, run.CreatedAt, run.UpdatedAt = fromUnixMillis(deadline), fromUnixMillis(created), fromUnixMillis(updated)
	run.StopReceiptID, run.StopRequestedAt, run.TimeoutRequestedAt = stopReceipt.Int64, nullableTime(stopAt), nullableTime(timeoutAt)
	run.ObservedContainerID, run.ObservedContainerStartedAt = containerID.String, containerStarted.String
	run.RuntimeEpoch, run.HostPort = runtimeEpoch.Int64, int(hostPort.Int64)
	run.PromptRequestAttemptedAt = nullableTime(promptAttempted)
	if sealReceipt.Valid {
		run.Seal = &Seal{ReceiptID: sealReceipt.Int64, ResultID: task.ResultID(resultID.String),
			RequestedAt: fromUnixMillis(sealRequested.Int64), PolicyVersion: sealPolicy.String}
	}
	if fenceKind.Valid {
		run.WriterFence = &WriterFence{Kind: WriterFenceKind(fenceKind.String), ContainerID: fenceContainer.String,
			ContainerStartedAt: fenceStarted.String, RuntimeToken: fenceToken.String, StoppedAt: nullableTime(fenceStopped)}
	}
	run.LastEvidence, run.LastError, run.CleanupProof = evidence.String, lastError.String, cleanupProof.String
	run.Creator, err = decodeActor(creator)
	return run, err
}

func readRun(ctx context.Context, q queryRower, workspaceID task.WorkspaceID, id task.RunID) (BackgroundRun, error) {
	run, err := scanRun(q.QueryRowContext(ctx, runSelect+` WHERE r.workspace_id=? AND r.id=?`, workspaceID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRun{}, ErrNotFound
	}
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("read run: %w", err)
	}
	return run, nil
}

func readOwnedRun(ctx context.Context, q queryRower, workspaceID task.WorkspaceID, id task.RunID, actor task.ActorSnapshot) (BackgroundRun, error) {
	run, err := scanRun(q.QueryRowContext(ctx, runSelect+` WHERE r.workspace_id=? AND r.id=?`+ownedBy,
		append([]any{workspaceID, id}, ownerArgs(actor)...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRun{}, ErrNotFound
	}
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("read owned run: %w", err)
	}
	return run, nil
}

func nullableString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func nullableTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromUnixMillis(v.Int64)
	return &t
}

const receiptSelect = `SELECT id,workspace_id,command_kind,idempotency_key,request_hash,actor,accepted_at,
api_contract_version,run_id,response_status,response_projection FROM receipts`

// FindReceiptByIdempotency returns the durable command receipt for one exact
// workspace/kind/key scope. Ownership and request-hash classification remains
// in the command transition that consumes this read.
func (s *Store) FindReceiptByIdempotency(ctx context.Context, workspaceID task.WorkspaceID, commandKind string, key task.IdempotencyKey) (Receipt, bool, error) {
	return receiptByKey(ctx, s.db, workspaceID, commandKind, key)
}

func receiptByKey(ctx context.Context, q queryRower, workspaceID task.WorkspaceID, kind string, key task.IdempotencyKey) (Receipt, bool, error) {
	var r Receipt
	var requestHash []byte
	var acceptedAt int64
	var response, actor string
	err := q.QueryRowContext(ctx, receiptSelect+` WHERE workspace_id=? AND command_kind=? AND idempotency_key=?`, workspaceID, kind, key).Scan(
		&r.ID, &r.WorkspaceID, &r.CommandKind, &r.IdempotencyKey, &requestHash, &actor, &acceptedAt,
		&r.APIContractVersion, &r.RunID, &r.ResponseStatus, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, fmt.Errorf("read idempotency receipt: %w", err)
	}
	copy(r.RequestHash[:], requestHash)
	r.AcceptedAt = fromUnixMillis(acceptedAt)
	r.ResponseProjection = json.RawMessage(response)
	r.Actor, err = decodeActor(actor)
	return r, true, err
}

// insertReceipt records an accepted command for run and returns it.
func insertReceipt(ctx context.Context, tx *sql.Tx, claim task.IdempotencyClaim, run task.RunID, apiVersion string,
	acceptedAt time.Time, response any) (Receipt, error) {
	projection, err := json.Marshal(response)
	if err != nil {
		return Receipt{}, fmt.Errorf("encode receipt projection: %w", err)
	}
	acceptedMS := unixMillis(acceptedAt)
	result, err := tx.ExecContext(ctx, `INSERT INTO receipts(workspace_id,command_kind,idempotency_key,request_hash,actor,
accepted_at,api_contract_version,run_id,response_status,response_projection) VALUES(?,?,?,?,?,?,?,?,202,?)`,
		claim.Scope.WorkspaceID, claim.Scope.CommandKind, claim.Key, claim.RequestHash[:], encodeActor(claim.Actor),
		acceptedMS, apiVersion, run, string(projection))
	if err != nil {
		return Receipt{}, fmt.Errorf("insert %s receipt: %w", claim.Scope.CommandKind, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: id, WorkspaceID: claim.Scope.WorkspaceID, CommandKind: claim.Scope.CommandKind, IdempotencyKey: claim.Key,
		RequestHash: claim.RequestHash, Actor: claim.Actor, AcceptedAt: fromUnixMillis(acceptedMS), APIContractVersion: apiVersion,
		RunID: run, ResponseStatus: 202, ResponseProjection: projection}, nil
}
