package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nebler/fern/internal/task"
)

const backgroundRunOwnershipSelect = `
SELECT o.task_id,o.attempt_id,o.workspace_id,o.run_generation,o.mode,o.phase,o.writer_generation,
       o.container_identity,o.container_id,o.container_started_at,o.runtime_epoch,o.runtime_token,o.volume_identity,o.endpoint_identity,o.host_port,
       o.opencode_session_id,o.opencode_message_id,o.target_writer_generation,o.target_container_identity,o.target_volume_identity,
       o.target_endpoint_identity,o.target_opencode_session_id,o.target_opencode_message_id,o.request_receipt_id,o.requested_at,
       o.route_evidence,o.writer_evidence,o.resource_evidence,o.git_evidence,o.last_error,
       o.claim_owner,o.claim_expires_at,o.claim_generation,o.revision,o.created_at,o.updated_at,
       a.actor_type,a.actor_id,a.display_name,a.credential_id,a.authentication,a.request_id
FROM background_run_ownerships o
LEFT JOIN actor_snapshots a ON a.id=o.request_actor_snapshot_id`

func (mode BackgroundRunOwnershipMode) valid() bool {
	switch mode {
	case BackgroundRunAgentOwned, BackgroundRunTakeoverRequested, BackgroundRunHumanOwned,
		BackgroundRunHandbackRequested, BackgroundRunOwnershipUncertain, BackgroundRunOwnershipClosed:
		return true
	default:
		return false
	}
}

func (phase BackgroundRunOwnershipPhase) valid() bool {
	switch phase {
	case BackgroundRunOwnershipAgentActive, BackgroundRunOwnershipAgentRouteRemoval, BackgroundRunOwnershipAgentStop,
		BackgroundRunOwnershipAgentRemove, BackgroundRunOwnershipAgentVolumeRemove, BackgroundRunOwnershipHumanCreate,
		BackgroundRunOwnershipHumanStart, BackgroundRunOwnershipHumanActive, BackgroundRunOwnershipHumanRouteRemoval,
		BackgroundRunOwnershipHumanStop, BackgroundRunOwnershipHumanRemove, BackgroundRunOwnershipAgentVolumeCreate,
		BackgroundRunOwnershipAgentCreate, BackgroundRunOwnershipAgentStart, BackgroundRunOwnershipAgentHealth,
		BackgroundRunOwnershipAgentSession, BackgroundRunOwnershipAgentPrompt, BackgroundRunOwnershipUncertainPhase,
		BackgroundRunOwnershipClosedPhase:
		return true
	default:
		return false
	}
}

func validOwnershipModePhase(mode BackgroundRunOwnershipMode, phase BackgroundRunOwnershipPhase) bool {
	switch mode {
	case BackgroundRunAgentOwned:
		return phase == BackgroundRunOwnershipAgentActive
	case BackgroundRunHumanOwned:
		return phase == BackgroundRunOwnershipHumanActive
	case BackgroundRunTakeoverRequested:
		switch phase {
		case BackgroundRunOwnershipAgentRouteRemoval, BackgroundRunOwnershipAgentStop, BackgroundRunOwnershipAgentRemove,
			BackgroundRunOwnershipAgentVolumeRemove, BackgroundRunOwnershipHumanCreate, BackgroundRunOwnershipHumanStart:
			return true
		}
	case BackgroundRunHandbackRequested:
		switch phase {
		case BackgroundRunOwnershipHumanRouteRemoval, BackgroundRunOwnershipHumanStop, BackgroundRunOwnershipHumanRemove,
			BackgroundRunOwnershipAgentVolumeCreate, BackgroundRunOwnershipAgentCreate, BackgroundRunOwnershipAgentStart,
			BackgroundRunOwnershipAgentHealth, BackgroundRunOwnershipAgentSession, BackgroundRunOwnershipAgentPrompt:
			return true
		}
	case BackgroundRunOwnershipUncertain:
		return phase == BackgroundRunOwnershipUncertainPhase
	case BackgroundRunOwnershipClosed:
		return phase == BackgroundRunOwnershipClosedPhase
	}
	return false
}

func (s *Store) GetBackgroundRunForControl(ctx context.Context, workspaceID task.WorkspaceID, taskID task.TaskID, actor task.ActorSnapshot) (BackgroundRun, BackgroundRunOwnership, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil || actor.Validate() != nil ||
		(actor.Type != task.ActorDevice && actor.Type != task.ActorOperator) {
		return BackgroundRun{}, BackgroundRunOwnership{}, fmt.Errorf("%w: run control authority", ErrInvalidInput)
	}
	if _, err := task.ParseTaskID(string(taskID)); err != nil {
		return BackgroundRun{}, BackgroundRunOwnership{}, fmt.Errorf("%w: run control identity", ErrInvalidInput)
	}
	run, err := readBackgroundRunExact(ctx, s.db, workspaceID, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRun{}, BackgroundRunOwnership{}, ErrNotFound
	}
	if err != nil {
		return BackgroundRun{}, BackgroundRunOwnership{}, err
	}
	ownership, err := getBackgroundRunOwnership(ctx, s.db, workspaceID, taskID)
	if err != nil {
		return BackgroundRun{}, BackgroundRunOwnership{}, err
	}
	return run, ownership, nil
}

func (s *Store) ListBackgroundRunsForControl(ctx context.Context, workspaceID task.WorkspaceID, actor task.ActorSnapshot, limit int) ([]BackgroundRunControlView, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil || actor.Validate() != nil ||
		(actor.Type != task.ActorDevice && actor.Type != task.ActorOperator) || limit < 1 || limit > MaxBackgroundRunListLimit {
		return nil, fmt.Errorf("%w: run control list", ErrInvalidInput)
	}
	rows, err := s.db.QueryContext(ctx, backgroundRunSelect+` WHERE r.workspace_id=? ORDER BY r.created_at DESC,r.task_id DESC LIMIT ?`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]BackgroundRunControlView, 0)
	for rows.Next() {
		run, scanErr := scanBackgroundRun(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		ownership, getErr := getBackgroundRunOwnership(ctx, s.db, workspaceID, run.TaskID)
		if getErr != nil {
			return nil, getErr
		}
		result = append(result, BackgroundRunControlView{Run: run, Ownership: ownership})
	}
	return result, rows.Err()
}

func (s *Store) GetBackgroundRunOwnership(ctx context.Context, workspaceID task.WorkspaceID, taskID task.TaskID) (BackgroundRunOwnership, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil {
		return BackgroundRunOwnership{}, fmt.Errorf("%w: ownership workspace", ErrInvalidInput)
	}
	if _, err := task.ParseTaskID(string(taskID)); err != nil {
		return BackgroundRunOwnership{}, fmt.Errorf("%w: ownership task", ErrInvalidInput)
	}
	return getBackgroundRunOwnership(ctx, s.db, workspaceID, taskID)
}

func getBackgroundRunOwnership(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, workspaceID task.WorkspaceID, taskID task.TaskID) (BackgroundRunOwnership, error) {
	value, err := scanBackgroundRunOwnership(query.QueryRowContext(ctx, backgroundRunOwnershipSelect+` WHERE o.workspace_id=? AND o.task_id=?`, workspaceID, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRunOwnership{}, ErrNotFound
	}
	if err != nil {
		return BackgroundRunOwnership{}, fmt.Errorf("read background run ownership: %w", err)
	}
	return value, nil
}

func scanBackgroundRunOwnership(scanner interface{ Scan(...any) error }) (BackgroundRunOwnership, error) {
	var value BackgroundRunOwnership
	var containerIdentity, containerID, startedAt, runtimeToken, volumeIdentity, endpointIdentity sql.NullString
	var hostPort, runtimeEpoch, targetGeneration sql.NullInt64
	var sessionID, messageID, targetContainer, targetVolume, targetEndpoint, targetSession, targetMessage sql.NullString
	var receiptID, routeEvidence, writerEvidence, resourceEvidence, gitEvidence, lastError sql.NullString
	var requestedAt sql.NullInt64
	var claimOwner sql.NullString
	var claimExpiry sql.NullInt64
	var created, updated int64
	var actorType, actorID, actorName, actorCredential, actorAuth, actorRequest sql.NullString
	err := scanner.Scan(&value.TaskID, &value.AttemptID, &value.WorkspaceID, &value.RunGeneration, &value.Mode, &value.Phase, &value.WriterGeneration,
		&containerIdentity, &containerID, &startedAt, &runtimeEpoch, &runtimeToken, &volumeIdentity, &endpointIdentity, &hostPort,
		&sessionID, &messageID, &targetGeneration, &targetContainer, &targetVolume, &targetEndpoint, &targetSession, &targetMessage,
		&receiptID, &requestedAt, &routeEvidence, &writerEvidence, &resourceEvidence, &gitEvidence, &lastError,
		&claimOwner, &claimExpiry, &value.ClaimGeneration, &value.Revision, &created, &updated,
		&actorType, &actorID, &actorName, &actorCredential, &actorAuth, &actorRequest)
	if err != nil {
		return BackgroundRunOwnership{}, err
	}
	if !value.Mode.valid() || !value.Phase.valid() || !validOwnershipModePhase(value.Mode, value.Phase) || value.WriterGeneration < 1 || value.RunGeneration < 1 {
		return BackgroundRunOwnership{}, ErrCorruptStore
	}
	value.ContainerIdentity, value.ContainerID, value.ContainerStartedAt, value.RuntimeToken = nullableText(containerIdentity), nullableText(containerID), nullableText(startedAt), nullableText(runtimeToken)
	value.RuntimeEpoch, value.HostPort = runtimeEpoch.Int64, int(hostPort.Int64)
	value.VolumeIdentity, value.EndpointIdentity = nullableText(volumeIdentity), nullableText(endpointIdentity)
	value.OpenCodeSessionID, value.OpenCodeMessageID = task.OpenCodeSessionID(nullableText(sessionID)), task.OpenCodeMessageID(nullableText(messageID))
	value.TargetWriterGeneration, value.TargetContainerIdentity = targetGeneration.Int64, nullableText(targetContainer)
	value.TargetVolumeIdentity, value.TargetEndpointIdentity = nullableText(targetVolume), nullableText(targetEndpoint)
	value.TargetOpenCodeSessionID, value.TargetOpenCodeMessageID = task.OpenCodeSessionID(nullableText(targetSession)), task.OpenCodeMessageID(nullableText(targetMessage))
	value.RequestReceiptID = task.ReceiptID(nullableText(receiptID))
	value.RequestedAt = nullableTime(requestedAt)
	value.RouteEvidence, value.WriterEvidence = nullableText(routeEvidence), nullableText(writerEvidence)
	value.ResourceEvidence, value.GitEvidence, value.LastError = nullableText(resourceEvidence), nullableText(gitEvidence), nullableText(lastError)
	value.ClaimOwner, value.ClaimExpiresAt = nullableText(claimOwner), nullableTime(claimExpiry)
	value.CreatedAt, value.UpdatedAt = fromUnixMillis(created), fromUnixMillis(updated)
	if value.ContainerID != "" {
		started, parseErr := time.Parse(time.RFC3339Nano, value.ContainerStartedAt)
		digest := sha256.Sum256([]byte(value.ContainerID + "\x00" + value.ContainerStartedAt))
		if parseErr != nil || started.UnixNano() != value.RuntimeEpoch || value.RuntimeToken != hex.EncodeToString(digest[:]) {
			return BackgroundRunOwnership{}, ErrCorruptStore
		}
	}
	if actorType.Valid {
		actor := task.ActorSnapshot{Type: task.ActorType(actorType.String), ID: actorID.String, DisplayName: actorName.String,
			CredentialID: actorCredential.String, Authentication: actorAuth.String, RequestID: actorRequest.String}
		if actor.Validate() != nil {
			return BackgroundRunOwnership{}, ErrCorruptStore
		}
		value.RequestActor = &actor
	}
	return value, nil
}

func (s *Store) RequestBackgroundRunTakeover(ctx context.Context, p RequestBackgroundRunTakeoverParams) (BackgroundRunOwnershipAdmission, error) {
	return s.requestBackgroundRunOwnership(ctx, p.WorkspaceID, p.TaskID, p.ReceiptID, p.Claim, p.APIContractVersion,
		p.RequestedAt, "takeover")
}

func (s *Store) RequestBackgroundRunHandback(ctx context.Context, p RequestBackgroundRunHandbackParams) (BackgroundRunOwnershipAdmission, error) {
	return s.requestBackgroundRunOwnership(ctx, p.WorkspaceID, p.TaskID, p.ReceiptID, p.Claim, p.APIContractVersion,
		p.RequestedAt, "handback")
}

func (s *Store) requestBackgroundRunOwnership(ctx context.Context, workspaceID task.WorkspaceID, taskID task.TaskID, receiptID task.ReceiptID,
	claim task.IdempotencyClaim, contract string, requested time.Time, direction string) (_ BackgroundRunOwnershipAdmission, err error) {
	command := RequestBackgroundRunTakeoverCommand
	if direction == "handback" {
		command = RequestBackgroundRunHandbackCommand
	}
	if _, parseErr := task.ParseWorkspaceID(string(workspaceID)); parseErr != nil || workspaceID != claim.Scope.WorkspaceID ||
		claim.Validate() != nil || claim.Scope.CommandKind != command ||
		(claim.Actor.Type != task.ActorDevice && claim.Actor.Type != task.ActorOperator) ||
		!validBoundedText(contract, 1, 64) || validExactTimestamp(requested) != nil {
		return BackgroundRunOwnershipAdmission{}, fmt.Errorf("%w: background run %s request", ErrInvalidInput, direction)
	}
	if _, parseErr := task.ParseTaskID(string(taskID)); parseErr != nil {
		return BackgroundRunOwnershipAdmission{}, fmt.Errorf("%w: background run %s task", ErrInvalidInput, direction)
	}
	if _, parseErr := task.ParseReceiptID(string(receiptID)); parseErr != nil {
		return BackgroundRunOwnershipAdmission{}, fmt.Errorf("%w: background run %s receipt", ErrInvalidInput, direction)
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRunOwnershipAdmission{}, err
	}
	defer release()
	defer rollback(tx, &err)
	existing, found, err := receiptByKey(ctx, tx, workspaceID, command, claim.Key)
	if err != nil {
		return BackgroundRunOwnershipAdmission{}, err
	}
	if found {
		disposition, classifyErr := task.ClassifyIdempotency(&task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: existing.WorkspaceID, CommandKind: existing.CommandKind}, Key: existing.IdempotencyKey, RequestHash: existing.RequestHash, Actor: existing.Actor}, claim)
		if classifyErr != nil {
			return BackgroundRunOwnershipAdmission{}, classifyErr
		}
		if disposition == task.IdempotencyOwnerMismatch {
			return BackgroundRunOwnershipAdmission{}, ErrNotFound
		}
		if disposition != task.IdempotencyReplay || existing.TargetID != taskID {
			return BackgroundRunOwnershipAdmission{}, ErrIdempotencyConflict
		}
		ownership, getErr := getBackgroundRunOwnership(ctx, tx, workspaceID, taskID)
		if getErr != nil {
			return BackgroundRunOwnershipAdmission{}, getErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return BackgroundRunOwnershipAdmission{}, commitErr
		}
		return BackgroundRunOwnershipAdmission{Ownership: ownership, Receipt: existing, Replayed: true}, nil
	}
	run, err := readBackgroundRunExact(ctx, tx, workspaceID, taskID)
	if err != nil {
		return BackgroundRunOwnershipAdmission{}, err
	}
	ownership, err := getBackgroundRunOwnership(ctx, tx, workspaceID, taskID)
	if err != nil {
		return BackgroundRunOwnershipAdmission{}, err
	}
	if run.CancelEpoch != 0 || run.EffectPhase != BackgroundRunEffectPromptAdmitted {
		return BackgroundRunOwnershipAdmission{}, ErrInvalidState
	}
	var activeControls int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM background_run_controls WHERE task_id=? AND state IN ('requested','attempted')`, run.TaskID).Scan(&activeControls); err != nil {
		return BackgroundRunOwnershipAdmission{}, err
	}
	if activeControls != 0 {
		return BackgroundRunOwnershipAdmission{}, ErrInvalidState
	}
	now := unixMillis(requested)
	compact := strings.ReplaceAll(strings.TrimPrefix(string(taskID), "tsk_"), "-", "")
	targetGeneration := ownership.WriterGeneration + 1
	var mode BackgroundRunOwnershipMode
	var phase BackgroundRunOwnershipPhase
	var targetContainer, targetVolume, targetEndpoint any
	var targetSession, targetMessage any
	if direction == "takeover" {
		if ownership.Mode != BackgroundRunAgentOwned || ownership.Phase != BackgroundRunOwnershipAgentActive ||
			(ownership.ContainerID == "" && (run.ObservedContainerID == "" || run.ObservedContainerStartedAt == "" || run.RuntimeEpoch <= 0)) {
			return BackgroundRunOwnershipAdmission{}, ErrInvalidState
		}
		mode, phase = BackgroundRunTakeoverRequested, BackgroundRunOwnershipAgentRouteRemoval
		targetContainer = fmt.Sprintf("fern-run-%s-g%d-w%d-human", compact, run.Generation, targetGeneration)
		targetVolume, targetEndpoint, targetSession, targetMessage = nil, nil, nil, nil
		if ownership.ContainerID == "" {
			digest := sha256.Sum256([]byte(run.ObservedContainerID + "\x00" + run.ObservedContainerStartedAt))
			ownership.ContainerIdentity, ownership.ContainerID = run.ContainerIdentity, run.ObservedContainerID
			ownership.ContainerStartedAt, ownership.RuntimeEpoch, ownership.RuntimeToken = run.ObservedContainerStartedAt, run.RuntimeEpoch, hex.EncodeToString(digest[:])
			ownership.VolumeIdentity, ownership.EndpointIdentity, ownership.HostPort = run.VolumeIdentity, run.EndpointIdentity, run.HostPort
			ownership.OpenCodeSessionID, ownership.OpenCodeMessageID = run.OpenCodeSessionID, run.OpenCodeMessageID
		}
	} else {
		if ownership.Mode != BackgroundRunHumanOwned || ownership.Phase != BackgroundRunOwnershipHumanActive {
			return BackgroundRunOwnershipAdmission{}, ErrInvalidState
		}
		mode, phase = BackgroundRunHandbackRequested, BackgroundRunOwnershipHumanRouteRemoval
		targetContainer, targetVolume, targetEndpoint = run.ContainerIdentity, run.VolumeIdentity, run.EndpointIdentity
		targetSession, targetMessage = run.OpenCodeSessionID, run.OpenCodeMessageID
	}
	actorID, err := ensureActor(ctx, tx, claim.Actor)
	if err != nil {
		return BackgroundRunOwnershipAdmission{}, err
	}
	projection, _ := json.Marshal(struct {
		RunID task.TaskID                `json:"run_id"`
		Mode  BackgroundRunOwnershipMode `json:"mode"`
	}{taskID, mode})
	if _, err := tx.ExecContext(ctx, `INSERT INTO receipts(
id,workspace_id,command_kind,state,idempotency_key,request_hash,actor_snapshot_id,accepted_at,api_contract_version,target_type,target_id,response_status,response_projection)
VALUES(?,?,?,'accepted',?,?,?,?,?,'task',?,202,?)`, receiptID, workspaceID, command, claim.Key, claim.RequestHash[:], actorID, now, contract, taskID, string(projection)); err != nil {
		return BackgroundRunOwnershipAdmission{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE background_runs SET claim_owner=NULL,claim_expires_at=NULL,revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND revision=? AND cancel_epoch=0 AND effect_phase='prompt_admitted'`,
		now, run.TaskID, run.AttemptID, run.WorkspaceID, run.Generation, run.Revision)
	if err != nil {
		return BackgroundRunOwnershipAdmission{}, err
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunOwnershipAdmission{}, ErrInvalidState
	}
	result, err = tx.ExecContext(ctx, `UPDATE background_run_ownerships SET mode=?,phase=?,
container_identity=?,container_id=?,container_started_at=?,runtime_epoch=?,runtime_token=?,volume_identity=?,endpoint_identity=?,host_port=?,opencode_session_id=?,opencode_message_id=?,
target_writer_generation=?,target_container_identity=?,target_volume_identity=?,target_endpoint_identity=?,target_opencode_session_id=?,target_opencode_message_id=?,
request_receipt_id=?,request_actor_snapshot_id=?,requested_at=?,route_evidence=NULL,writer_evidence=NULL,resource_evidence=NULL,git_evidence=NULL,last_error=NULL,
claim_owner=NULL,claim_expires_at=NULL,revision=revision+1,updated_at=?
WHERE task_id=? AND workspace_id=? AND revision=?`, mode, phase,
		nullableValue(ownership.ContainerIdentity), nullableValue(ownership.ContainerID), nullableValue(ownership.ContainerStartedAt), nullablePositive(ownership.RuntimeEpoch), nullableValue(ownership.RuntimeToken),
		nullableValue(ownership.VolumeIdentity), nullableValue(ownership.EndpointIdentity), nullablePositive(int64(ownership.HostPort)), nullableValue(string(ownership.OpenCodeSessionID)), nullableValue(string(ownership.OpenCodeMessageID)),
		targetGeneration, targetContainer, targetVolume, targetEndpoint, targetSession, targetMessage, receiptID, actorID, now, now, taskID, workspaceID, ownership.Revision)
	if err != nil {
		return BackgroundRunOwnershipAdmission{}, err
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunOwnershipAdmission{}, ErrInvalidState
	}
	updated, err := getBackgroundRunOwnership(ctx, tx, workspaceID, taskID)
	if err != nil {
		return BackgroundRunOwnershipAdmission{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRunOwnershipAdmission{}, err
	}
	receipt := Receipt{ID: receiptID, WorkspaceID: workspaceID, CommandKind: command, State: ReceiptAccepted,
		IdempotencyKey: claim.Key, RequestHash: claim.RequestHash, Actor: claim.Actor, AcceptedAt: fromUnixMillis(now),
		APIContractVersion: contract, TargetType: "task", TargetID: taskID, ResponseStatus: 202, ResponseProjection: projection}
	return BackgroundRunOwnershipAdmission{Ownership: updated, Receipt: receipt}, nil
}

func (s *Store) ClaimNextBackgroundRunOwnership(ctx context.Context, p ClaimNextBackgroundRunOwnershipParams) (_ BackgroundRunOwnership, err error) {
	if _, parseErr := task.ParseWorkspaceID(string(p.WorkspaceID)); parseErr != nil || !validBoundedText(p.ClaimOwner, 1, 128) ||
		validExactTimestamp(p.Now) != nil || p.LeaseDuration <= 0 || p.LeaseDuration > maxBackgroundRunLease {
		return BackgroundRunOwnership{}, fmt.Errorf("%w: ownership claim", ErrInvalidInput)
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRunOwnership{}, err
	}
	defer release()
	defer rollback(tx, &err)
	now, expiry := unixMillis(p.Now), unixMillis(p.Now.Add(p.LeaseDuration))
	var taskID task.TaskID
	var mode BackgroundRunOwnershipMode
	err = tx.QueryRowContext(ctx, `SELECT o.task_id,o.mode FROM background_run_ownerships o
JOIN attempts a ON a.id=o.attempt_id AND a.task_id=o.task_id AND a.workspace_id=o.workspace_id
WHERE o.workspace_id=? AND (o.mode IN ('takeover_requested','handback_requested') OR (o.mode='human_owned' AND a.deadline<=?)) AND
(o.claim_owner=? OR o.claim_owner IS NULL OR o.claim_expires_at<=?) ORDER BY o.updated_at,o.task_id LIMIT 1`,
		p.WorkspaceID, now, p.ClaimOwner, now).Scan(&taskID, &mode)
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRunOwnership{}, ErrNotFound
	}
	if err != nil {
		return BackgroundRunOwnership{}, err
	}
	if mode == BackgroundRunHumanOwned {
		result, transitionErr := tx.ExecContext(ctx, `UPDATE background_run_ownerships SET mode='handback_requested',phase='human_route_removal',
target_writer_generation=writer_generation+1,
target_container_identity=(SELECT container_identity FROM background_runs WHERE task_id=background_run_ownerships.task_id),
target_volume_identity=(SELECT volume_identity FROM background_runs WHERE task_id=background_run_ownerships.task_id),
target_endpoint_identity=(SELECT endpoint_identity FROM background_runs WHERE task_id=background_run_ownerships.task_id),
target_opencode_session_id=(SELECT opencode_session_id FROM background_runs WHERE task_id=background_run_ownerships.task_id),
target_opencode_message_id=(SELECT opencode_message_id FROM background_runs WHERE task_id=background_run_ownerships.task_id),
request_receipt_id=NULL,request_actor_snapshot_id=NULL,requested_at=NULL,route_evidence=NULL,writer_evidence=NULL,resource_evidence=NULL,git_evidence=NULL,last_error=NULL,
revision=revision+1,updated_at=? WHERE workspace_id=? AND task_id=? AND mode='human_owned' AND phase='human_active'`, now, p.WorkspaceID, taskID)
		if transitionErr != nil {
			return BackgroundRunOwnership{}, transitionErr
		}
		if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
			return BackgroundRunOwnership{}, ErrInvalidState
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE background_run_ownerships SET claim_owner=?,claim_expires_at=?,
claim_generation=claim_generation+CASE WHEN claim_owner=? THEN 0 ELSE 1 END,revision=revision+1,updated_at=?
WHERE workspace_id=? AND task_id=? AND (claim_owner=? OR claim_owner IS NULL OR claim_expires_at<=?)`, p.ClaimOwner, expiry, p.ClaimOwner, now,
		p.WorkspaceID, taskID, p.ClaimOwner, now)
	if err != nil {
		return BackgroundRunOwnership{}, err
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunOwnership{}, ErrInvalidState
	}
	value, err := getBackgroundRunOwnership(ctx, tx, p.WorkspaceID, taskID)
	if err != nil {
		return BackgroundRunOwnership{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRunOwnership{}, err
	}
	return value, nil
}

func (s *Store) ReadClaimedBackgroundRunOwnershipWork(ctx context.Context, claim BackgroundRunOwnershipClaim) (BackgroundRunOwnershipWork, error) {
	if err := validateOwnershipClaim(claim); err != nil {
		return BackgroundRunOwnershipWork{}, err
	}
	current, err := scanBackgroundRunOwnership(s.db.QueryRowContext(ctx, backgroundRunOwnershipSelect+`
WHERE o.task_id=? AND o.attempt_id=? AND o.workspace_id=? AND o.run_generation=? AND o.revision=? AND o.mode=? AND o.phase=? AND
o.claim_owner=? AND o.claim_generation=? AND o.claim_expires_at>?`, claim.TaskID, claim.AttemptID, claim.WorkspaceID, claim.RunGeneration,
		claim.ExpectedRevision, claim.ExpectedMode, claim.ExpectedPhase, claim.ClaimOwner, claim.ClaimGeneration, unixMillis(claim.Now)))
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRunOwnershipWork{}, ErrInvalidState
	}
	if err != nil {
		return BackgroundRunOwnershipWork{}, err
	}
	run, err := readBackgroundRunExact(ctx, s.db, claim.WorkspaceID, claim.TaskID)
	if err != nil {
		return BackgroundRunOwnershipWork{}, err
	}
	work, err := s.readBackgroundRunWork(ctx, run)
	if err != nil {
		return BackgroundRunOwnershipWork{}, err
	}
	return BackgroundRunOwnershipWork{Run: run, Prompt: work.Prompt, Ownership: current}, nil
}

func (s *Store) AdvanceBackgroundRunOwnership(ctx context.Context, p AdvanceBackgroundRunOwnershipParams) (BackgroundRunOwnership, error) {
	if err := validateOwnershipClaim(p.BackgroundRunOwnershipClaim); err != nil || !p.Mode.valid() || !p.Phase.valid() ||
		!validOwnershipModePhase(p.Mode, p.Phase) || !validOwnershipTransition(p.ExpectedMode, p.ExpectedPhase, p.Mode, p.Phase) {
		return BackgroundRunOwnership{}, fmt.Errorf("%w: ownership transition", ErrInvalidInput)
	}
	if p.WriterGeneration < 1 || !validOptionalBounded(p.ContainerIdentity, 256) || !validOptionalBounded(p.ContainerID, 128) ||
		!validOptionalBounded(p.ContainerStartedAt, 64) || !validOptionalBounded(p.RuntimeToken, 64) || !validOptionalBounded(p.VolumeIdentity, 256) ||
		!validOptionalBounded(p.EndpointIdentity, 256) || !validOptionalBounded(p.RouteEvidence, 4096) || !validOptionalBounded(p.WriterEvidence, 4096) ||
		!validOptionalBounded(p.ResourceEvidence, 4096) || !validOptionalBounded(p.GitEvidence, 4096) || !validOptionalBounded(p.LastError, 4096) {
		return BackgroundRunOwnership{}, fmt.Errorf("%w: ownership transition values", ErrInvalidInput)
	}
	if (p.ContainerID == "") != (p.ContainerStartedAt == "") || (p.ContainerID == "") != (p.RuntimeEpoch == 0) || (p.ContainerID == "") != (p.RuntimeToken == "") {
		return BackgroundRunOwnership{}, fmt.Errorf("%w: ownership runtime tuple", ErrInvalidInput)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE background_run_ownerships SET mode=?,phase=?,writer_generation=?,
container_identity=?,container_id=?,container_started_at=?,runtime_epoch=?,runtime_token=?,volume_identity=?,endpoint_identity=?,host_port=?,opencode_session_id=?,opencode_message_id=?,
route_evidence=COALESCE(?,route_evidence),writer_evidence=COALESCE(?,writer_evidence),resource_evidence=COALESCE(?,resource_evidence),git_evidence=COALESCE(?,git_evidence),
last_error=?,claim_owner=NULL,claim_expires_at=NULL,revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND run_generation=? AND revision=? AND mode=? AND phase=? AND claim_owner=? AND claim_generation=? AND claim_expires_at>?`,
		p.Mode, p.Phase, p.WriterGeneration, nullableValue(p.ContainerIdentity), nullableValue(p.ContainerID), nullableValue(p.ContainerStartedAt), nullablePositive(p.RuntimeEpoch), nullableValue(p.RuntimeToken),
		nullableValue(p.VolumeIdentity), nullableValue(p.EndpointIdentity), nullablePositive(int64(p.HostPort)), nullableValue(string(p.OpenCodeSessionID)), nullableValue(string(p.OpenCodeMessageID)),
		nullableValue(p.RouteEvidence), nullableValue(p.WriterEvidence), nullableValue(p.ResourceEvidence), nullableValue(p.GitEvidence), nullableValue(p.LastError), unixMillis(p.Now),
		p.TaskID, p.AttemptID, p.WorkspaceID, p.RunGeneration, p.ExpectedRevision, p.ExpectedMode, p.ExpectedPhase, p.ClaimOwner, p.ClaimGeneration, unixMillis(p.Now))
	if err != nil {
		return BackgroundRunOwnership{}, err
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunOwnership{}, ErrInvalidState
	}
	return getBackgroundRunOwnership(ctx, s.db, p.WorkspaceID, p.TaskID)
}

func validateOwnershipClaim(p BackgroundRunOwnershipClaim) error {
	if _, err := task.ParseWorkspaceID(string(p.WorkspaceID)); err != nil {
		return err
	}
	if _, err := task.ParseTaskID(string(p.TaskID)); err != nil {
		return err
	}
	if _, err := task.ParseAttemptID(string(p.AttemptID)); err != nil || p.RunGeneration < 1 || p.ExpectedRevision < 1 ||
		!p.ExpectedMode.valid() || !p.ExpectedPhase.valid() || !validOwnershipModePhase(p.ExpectedMode, p.ExpectedPhase) ||
		!validBoundedText(p.ClaimOwner, 1, 128) || p.ClaimGeneration < 1 || validExactTimestamp(p.Now) != nil {
		return ErrInvalidInput
	}
	return nil
}

func validOwnershipTransition(fromMode BackgroundRunOwnershipMode, from BackgroundRunOwnershipPhase, toMode BackgroundRunOwnershipMode, to BackgroundRunOwnershipPhase) bool {
	if toMode == BackgroundRunOwnershipUncertain && to == BackgroundRunOwnershipUncertainPhase &&
		(fromMode == BackgroundRunTakeoverRequested || fromMode == BackgroundRunHandbackRequested) {
		return true
	}
	want := map[BackgroundRunOwnershipPhase]struct {
		mode  BackgroundRunOwnershipMode
		phase BackgroundRunOwnershipPhase
	}{
		BackgroundRunOwnershipAgentRouteRemoval: {BackgroundRunTakeoverRequested, BackgroundRunOwnershipAgentStop},
		BackgroundRunOwnershipAgentStop:         {BackgroundRunTakeoverRequested, BackgroundRunOwnershipAgentRemove},
		BackgroundRunOwnershipAgentRemove:       {BackgroundRunTakeoverRequested, BackgroundRunOwnershipAgentVolumeRemove},
		BackgroundRunOwnershipAgentVolumeRemove: {BackgroundRunTakeoverRequested, BackgroundRunOwnershipHumanCreate},
		BackgroundRunOwnershipHumanCreate:       {BackgroundRunTakeoverRequested, BackgroundRunOwnershipHumanStart},
		BackgroundRunOwnershipHumanStart:        {BackgroundRunHumanOwned, BackgroundRunOwnershipHumanActive},
		BackgroundRunOwnershipHumanRouteRemoval: {BackgroundRunHandbackRequested, BackgroundRunOwnershipHumanStop},
		BackgroundRunOwnershipHumanStop:         {BackgroundRunHandbackRequested, BackgroundRunOwnershipHumanRemove},
		BackgroundRunOwnershipHumanRemove:       {BackgroundRunHandbackRequested, BackgroundRunOwnershipAgentVolumeCreate},
		BackgroundRunOwnershipAgentVolumeCreate: {BackgroundRunHandbackRequested, BackgroundRunOwnershipAgentCreate},
		BackgroundRunOwnershipAgentCreate:       {BackgroundRunHandbackRequested, BackgroundRunOwnershipAgentStart},
		BackgroundRunOwnershipAgentStart:        {BackgroundRunHandbackRequested, BackgroundRunOwnershipAgentHealth},
		BackgroundRunOwnershipAgentHealth:       {BackgroundRunHandbackRequested, BackgroundRunOwnershipAgentSession},
		BackgroundRunOwnershipAgentSession:      {BackgroundRunHandbackRequested, BackgroundRunOwnershipAgentPrompt},
		BackgroundRunOwnershipAgentPrompt:       {BackgroundRunAgentOwned, BackgroundRunOwnershipAgentActive},
	}
	next, ok := want[from]
	return ok && next.mode == toMode && next.phase == to
}

func validOptionalBounded(value string, max int) bool {
	return value == "" || validBoundedText(value, 1, max)
}

func nullableValue(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullablePositive(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}
