package taskstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

const StopBackgroundRunCommand = "run.stop"
const BackgroundRunStoppedBeforeStart = "background_run_stopped_before_start"

const backgroundRunSelect = `
SELECT r.task_id,r.attempt_id,r.workspace_id,r.generation,r.repository_id,r.repository_remote,r.base_oid,r.branch,
       r.instruction_sha256,r.profile,r.profile_sha256,r.environment_sha256,r.resource_spec_version,r.image_identity,r.clone_identity,r.volume_identity,
       r.container_identity,r.endpoint_identity,r.opencode_session_id,r.opencode_message_id,r.state,r.effect_phase,
       r.cancel_epoch,r.stop_receipt_id,r.stop_requested_at,r.claim_owner,r.claim_expires_at,r.claim_generation,
	       r.clone_evidence,r.volume_evidence,r.observed_container_id,r.observed_container_started_at,r.runtime_epoch,r.host_port,
	       r.health_evidence,r.ready_evidence,r.session_evidence,r.prompt_evidence,r.writer_inactive_evidence,r.route_removed_evidence,
	       r.container_removed_evidence,r.volume_removed_evidence,r.clone_removed_evidence,r.last_evidence,r.last_error,
	       r.provision_intent_at,r.clone_observed_at,r.volume_observed_at,r.container_observed_at,r.health_observed_at,r.ready_at,
	       r.session_observed_at,r.prompt_intent_at,r.prompt_request_attempted_at,r.prompt_admitted_at,r.timeout_requested_at,r.stop_intent_at,r.writer_inactive_at,r.route_removed_at,
	       r.container_removed_at,r.volume_removed_at,r.clone_removed_at,r.cleanup_completed_at,r.cleanup_proof,r.absence_proof,
	       r.revision,r.created_at,r.updated_at,r.background_seal_request_id,r.artifact_export_id,r.retained_artifact_id,
	       r.materialization_id,r.retained_result_id,r.result_authority_phase,
	       c.actor_type,c.actor_id,c.display_name,c.credential_id,c.authentication,c.request_id,
	       s.actor_type,s.actor_id,s.display_name,s.credential_id,s.authentication,s.request_id,
	       x.actor_type,x.actor_id,x.display_name,x.credential_id,x.authentication,x.request_id
FROM background_runs r
JOIN actor_snapshots c ON c.id=r.creator_actor_snapshot_id
LEFT JOIN actor_snapshots s ON s.id=r.stop_actor_snapshot_id
LEFT JOIN actor_snapshots x ON x.id=r.timeout_actor_snapshot_id`

func (s *Store) GetBackgroundRun(ctx context.Context, workspaceID task.WorkspaceID, taskID task.TaskID, actor task.ActorSnapshot) (BackgroundRun, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil {
		return BackgroundRun{}, fmt.Errorf("%w: background run workspace", ErrInvalidInput)
	}
	if _, err := task.ParseTaskID(string(taskID)); err != nil || actor.Validate() != nil || actor.Type != task.ActorOpenCode {
		return BackgroundRun{}, fmt.Errorf("%w: background run identity", ErrInvalidInput)
	}
	run, err := scanBackgroundRun(s.db.QueryRowContext(ctx, backgroundRunSelect+`
WHERE r.workspace_id=? AND r.task_id=? AND c.actor_type=? AND c.actor_id=? AND c.credential_id=? AND c.authentication=?`,
		workspaceID, taskID, actor.Type, actor.ID, actor.CredentialID, actor.Authentication))
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRun{}, ErrNotFound
	}
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("read background run: %w", err)
	}
	return run, nil
}

// GetBackgroundRunOwners returns the exact parent revisions only after the
// same ownership-hiding check used by GetBackgroundRun.
func (s *Store) GetBackgroundRunOwners(ctx context.Context, workspaceID task.WorkspaceID, taskID task.TaskID, actor task.ActorSnapshot) (Task, Attempt, error) {
	run, err := s.GetBackgroundRun(ctx, workspaceID, taskID, actor)
	if err != nil {
		return Task{}, Attempt{}, err
	}
	owner, err := getTask(ctx, s.db, run.TaskID)
	if err != nil {
		return Task{}, Attempt{}, err
	}
	attempt, err := getAttempt(ctx, s.db, run.AttemptID)
	return owner, attempt, err
}

const MaxBackgroundRunListLimit = 100

// ListBackgroundRuns applies actor authority in SQL before its bound, so runs
// owned by another plugin credential can neither consume nor escape the page.
func (s *Store) ListBackgroundRuns(ctx context.Context, workspaceID task.WorkspaceID, actor task.ActorSnapshot, limit int) ([]BackgroundRun, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil || actor.Validate() != nil || actor.Type != task.ActorOpenCode || limit < 1 || limit > MaxBackgroundRunListLimit {
		return nil, fmt.Errorf("%w: background run list", ErrInvalidInput)
	}
	rows, err := s.db.QueryContext(ctx, backgroundRunSelect+`
WHERE r.workspace_id=? AND c.actor_type=? AND c.actor_id=? AND c.credential_id=? AND c.authentication=?
ORDER BY r.created_at DESC,r.task_id DESC LIMIT ?`, workspaceID, actor.Type, actor.ID, actor.CredentialID, actor.Authentication, limit)
	if err != nil {
		return nil, fmt.Errorf("list background runs: %w", err)
	}
	defer rows.Close()
	runs := make([]BackgroundRun, 0)
	for rows.Next() {
		run, scanErr := scanBackgroundRun(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan background run list: %w", scanErr)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate background run list: %w", err)
	}
	return runs, nil
}

func (s *Store) StopBackgroundRun(ctx context.Context, p StopBackgroundRunParams) (_ BackgroundRunStop, err error) {
	if err := validateBackgroundRunStop(p); err != nil {
		return BackgroundRunStop{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRunStop{}, fmt.Errorf("begin background run stop: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	existing, found, err := receiptByKey(ctx, tx, p.Claim.Scope.WorkspaceID, p.Claim.Scope.CommandKind, p.Claim.Key)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	if found {
		disposition, classifyErr := task.ClassifyIdempotency(&task.IdempotencyClaim{
			Scope: task.IdempotencyScope{WorkspaceID: existing.WorkspaceID, CommandKind: existing.CommandKind},
			Key:   existing.IdempotencyKey, RequestHash: existing.RequestHash, Actor: existing.Actor,
		}, p.Claim)
		if classifyErr != nil {
			return BackgroundRunStop{}, classifyErr
		}
		switch disposition {
		case task.IdempotencyReplay:
			if existing.TargetID != p.TaskID {
				return BackgroundRunStop{}, ErrIdempotencyConflict
			}
			run, getErr := getBackgroundRunOwned(ctx, tx, p.WorkspaceID, existing.TargetID, p.Claim.Actor)
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

	run, err := getBackgroundRunOwned(ctx, tx, p.WorkspaceID, p.TaskID, p.Claim.Actor)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	queuedStop := run.State == BackgroundRunQueued && run.EffectPhase == BackgroundRunEffectAbsent
	activeStop := run.State == BackgroundRunSettingUp || run.State == BackgroundRunWorking || run.State == BackgroundRunNeedsYou || run.State == BackgroundRunUncertain
	if run.CancelEpoch != 0 || (!queuedStop && !activeStop) {
		return BackgroundRunStop{}, ErrInvalidState
	}
	owner, err := getTask(ctx, tx, run.TaskID)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	attempt, err := getAttempt(ctx, tx, run.AttemptID)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	if owner.WorkspaceID != p.WorkspaceID || owner.CurrentAttemptID != attempt.ID || owner.State != task.TaskQueued ||
		owner.CancelEpoch != 0 || attempt.TaskID != owner.ID || attempt.WorkspaceID != owner.WorkspaceID ||
		attempt.ID != run.AttemptID || attempt.Sequence != run.Generation || attempt.State != task.AttemptPrepared || attempt.DeliveryPhase != DeliveryPhaseNone {
		return BackgroundRunStop{}, ErrInvalidState
	}
	actorID, err := ensureActor(ctx, tx, p.Claim.Actor)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	stopState := BackgroundRunFailed
	if activeStop {
		stopState = BackgroundRunCanceling
	}
	response, _ := json.Marshal(struct {
		RunID task.TaskID        `json:"run_id"`
		State BackgroundRunState `json:"state"`
	}{run.TaskID, stopState})
	now := unixMillis(p.StoppedAt)
	if _, err := tx.ExecContext(ctx, `INSERT INTO receipts(
id,workspace_id,command_kind,state,idempotency_key,request_hash,actor_snapshot_id,accepted_at,
api_contract_version,target_type,target_id,response_status,response_projection)
VALUES(?,?,?,'accepted',?,?,?,?,?,'task',?,202,?)`, p.ReceiptID, run.WorkspaceID, StopBackgroundRunCommand,
		p.Claim.Key, p.Claim.RequestHash[:], actorID, now, p.APIContractVersion, run.TaskID, string(response)); err != nil {
		return BackgroundRunStop{}, fmt.Errorf("insert background run stop receipt: %w", err)
	}
	if activeStop {
		result, updateErr := tx.ExecContext(ctx, `UPDATE background_runs SET state='canceling',effect_phase='stop_intent',cancel_epoch=1,
stop_receipt_id=?,stop_actor_snapshot_id=?,stop_requested_at=?,stop_intent_at=?,claim_owner=NULL,claim_expires_at=NULL,
revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND cancel_epoch=0 AND revision=? AND
state IN ('setting_up','working','needs_you','uncertain')`, p.ReceiptID, actorID, now, now, now,
			run.TaskID, run.AttemptID, run.WorkspaceID, run.Generation, run.Revision)
		if updateErr != nil {
			return BackgroundRunStop{}, fmt.Errorf("request active background run stop: %w", updateErr)
		}
		if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
			return BackgroundRunStop{}, ErrInvalidState
		}
		stored, getErr := getBackgroundRunOwned(ctx, tx, p.WorkspaceID, run.TaskID, p.Claim.Actor)
		if getErr != nil {
			return BackgroundRunStop{}, getErr
		}
		if err := tx.Commit(); err != nil {
			return BackgroundRunStop{}, fmt.Errorf("commit active background run stop: %w", err)
		}
		return BackgroundRunStop{Run: stored, Receipt: Receipt{ID: p.ReceiptID, WorkspaceID: run.WorkspaceID,
			CommandKind: StopBackgroundRunCommand, State: ReceiptAccepted, IdempotencyKey: p.Claim.Key,
			RequestHash: p.Claim.RequestHash, Actor: p.Claim.Actor, AcceptedAt: fromUnixMillis(now),
			APIContractVersion: p.APIContractVersion, TargetType: "task", TargetID: run.TaskID,
			ResponseStatus: 202, ResponseProjection: response}}, nil
	}
	payload, err := json.Marshal(struct {
		RunID         task.TaskID    `json:"runId"`
		Reason        string         `json:"reason"`
		StopReceiptID task.ReceiptID `json:"stopReceiptId"`
	}{run.TaskID, BackgroundRunStoppedBeforeStart, p.ReceiptID})
	if err != nil {
		return BackgroundRunStop{}, fmt.Errorf("encode background run stop event: %w", err)
	}
	attemptEvent, err := insertAttemptEvent(ctx, tx, p.AttemptEventID, attempt, "attempt.failed", now, actorID, payload)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	taskEvent, err := insertTaskEvent(ctx, tx, p.TaskEventID, owner, "task.failed", now, actorID, payload)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	if attemptEvent.Cursor >= taskEvent.Cursor {
		return BackgroundRunStop{}, ErrCorruptStore
	}
	result, err := tx.ExecContext(ctx, `UPDATE attempts SET state='failed',terminal_reason=?,revision=revision+1,updated_at=?
WHERE id=? AND task_id=? AND workspace_id=? AND state='prepared' AND delivery_phase='none' AND revision=?`,
		BackgroundRunStoppedBeforeStart, now, attempt.ID, owner.ID, owner.WorkspaceID, attempt.Revision)
	if err != nil {
		return BackgroundRunStop{}, fmt.Errorf("terminalize background run attempt: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunStop{}, ErrInvalidState
	}
	result, err = tx.ExecContext(ctx, `UPDATE tasks SET state='failed',terminal_reason=?,latest_event_cursor=?,revision=revision+1,updated_at=?
WHERE id=? AND workspace_id=? AND state='queued' AND cancel_epoch=0 AND current_attempt_id=? AND revision=?`,
		BackgroundRunStoppedBeforeStart, taskEvent.Cursor, now, owner.ID, owner.WorkspaceID, attempt.ID, owner.Revision)
	if err != nil {
		return BackgroundRunStop{}, fmt.Errorf("terminalize background run task: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunStop{}, ErrInvalidState
	}
	result, err = tx.ExecContext(ctx, `UPDATE background_runs SET state='failed',effect_phase='pre_effect_failed',cancel_epoch=1,
stop_receipt_id=?,stop_actor_snapshot_id=?,stop_requested_at=?,absence_proof='queued:no_effect_claim',last_error=?,revision=revision+1,updated_at=?
WHERE task_id=? AND attempt_id=? AND workspace_id=? AND generation=? AND state='queued' AND effect_phase='absent' AND cancel_epoch=0 AND revision=?`,
		p.ReceiptID, actorID, now, BackgroundRunStoppedBeforeStart, now, run.TaskID, run.AttemptID, run.WorkspaceID, run.Generation, run.Revision)
	if err != nil {
		return BackgroundRunStop{}, fmt.Errorf("fence background run stop: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunStop{}, ErrInvalidState
	}
	stored, err := getBackgroundRunOwned(ctx, tx, p.WorkspaceID, run.TaskID, p.Claim.Actor)
	if err != nil {
		return BackgroundRunStop{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRunStop{}, fmt.Errorf("commit background run stop: %w", err)
	}
	return BackgroundRunStop{Run: stored, Receipt: Receipt{ID: p.ReceiptID, WorkspaceID: run.WorkspaceID,
		CommandKind: StopBackgroundRunCommand, State: ReceiptAccepted, IdempotencyKey: p.Claim.Key,
		RequestHash: p.Claim.RequestHash, Actor: p.Claim.Actor, AcceptedAt: fromUnixMillis(now),
		APIContractVersion: p.APIContractVersion, TargetType: "task", TargetID: run.TaskID,
		ResponseStatus: 202, ResponseProjection: response}}, nil
}

func validateBackgroundRunStop(p StopBackgroundRunParams) error {
	if _, err := task.ParseWorkspaceID(string(p.WorkspaceID)); err != nil || p.WorkspaceID != p.Claim.Scope.WorkspaceID {
		return fmt.Errorf("%w: run workspace", ErrInvalidInput)
	}
	if _, err := task.ParseTaskID(string(p.TaskID)); err != nil {
		return fmt.Errorf("%w: run ID", ErrInvalidInput)
	}
	if _, err := task.ParseReceiptID(string(p.ReceiptID)); err != nil {
		return fmt.Errorf("%w: receipt ID", ErrInvalidInput)
	}
	if _, err := task.ParseEventID(string(p.AttemptEventID)); err != nil {
		return fmt.Errorf("%w: attempt event ID", ErrInvalidInput)
	}
	if _, err := task.ParseEventID(string(p.TaskEventID)); err != nil || p.TaskEventID == p.AttemptEventID {
		return fmt.Errorf("%w: task event ID", ErrInvalidInput)
	}
	if err := p.Claim.Validate(); err != nil || p.Claim.Scope.CommandKind != StopBackgroundRunCommand || p.Claim.Actor.Type != task.ActorOpenCode {
		return fmt.Errorf("%w: run stop claim", ErrInvalidInput)
	}
	if !validBoundedText(p.APIContractVersion, 1, 64) {
		return fmt.Errorf("%w: API contract version", ErrInvalidInput)
	}
	return validExactTimestamp(p.StoppedAt)
}

func getBackgroundRunOwned(ctx context.Context, q queryRower, workspaceID task.WorkspaceID, taskID task.TaskID, actor task.ActorSnapshot) (BackgroundRun, error) {
	run, err := scanBackgroundRun(q.QueryRowContext(ctx, backgroundRunSelect+`
WHERE r.workspace_id=? AND r.task_id=? AND c.actor_type=? AND c.actor_id=? AND c.credential_id=? AND c.authentication=?`,
		workspaceID, taskID, actor.Type, actor.ID, actor.CredentialID, actor.Authentication))
	if errors.Is(err, sql.ErrNoRows) {
		return BackgroundRun{}, ErrNotFound
	}
	if err != nil {
		return BackgroundRun{}, fmt.Errorf("read owned background run: %w", err)
	}
	return run, nil
}

func backgroundAttemptExists(ctx context.Context, q queryRower, attemptID task.AttemptID) (bool, error) {
	var exists int
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM background_runs WHERE attempt_id=?)`, attemptID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("inspect background run ownership: %w", err)
	}
	return exists == 1, nil
}

func scanBackgroundRun(row rowScanner) (BackgroundRun, error) {
	var run BackgroundRun
	var repositoryID, cancelEpoch int64
	var branch, stopReceipt, claimOwner, cloneEvidence, volumeEvidence, containerID, containerStarted sql.NullString
	var healthEvidence, readyEvidence, sessionEvidence, promptEvidence, writerEvidence, routeEvidence sql.NullString
	var containerRemovedEvidence, volumeRemovedEvidence, cloneRemovedEvidence, evidence, lastError, cleanupProof, absenceProof sql.NullString
	var sealRequestID, artifactExportID, retainedArtifactID, materializationID, retainedResultID, resultAuthorityPhase sql.NullString
	var stopAt, claimExpiry, runtimeEpoch, hostPort sql.NullInt64
	var provisionIntent, cloneObserved, volumeObserved, containerObserved, healthObserved, readyAt sql.NullInt64
	var sessionObserved, promptIntent, promptAttempted, promptAdmitted, timeoutRequested, stopIntent, writerInactive, routeRemoved sql.NullInt64
	var containerRemoved, volumeRemoved, cloneRemoved, cleanupCompleted sql.NullInt64
	var instructionHash, profileHash, environmentHash []byte
	var created, updated int64
	var stopType, stopID, stopName, stopCredential, stopAuth, stopRequest sql.NullString
	var timeoutType, timeoutID, timeoutName, timeoutCredential, timeoutAuth, timeoutRequest sql.NullString
	err := row.Scan(&run.TaskID, &run.AttemptID, &run.WorkspaceID, &run.Generation, &repositoryID,
		&run.RepositoryRemote, &run.BaseOID, &branch, &instructionHash, &run.Profile, &profileHash, &environmentHash, &run.ResourceSpecVersion,
		&run.ImageIdentity, &run.CloneIdentity, &run.VolumeIdentity, &run.ContainerIdentity, &run.EndpointIdentity,
		&run.OpenCodeSessionID, &run.OpenCodeMessageID, &run.State, &run.EffectPhase, &cancelEpoch,
		&stopReceipt, &stopAt, &claimOwner, &claimExpiry, &run.ClaimGeneration,
		&cloneEvidence, &volumeEvidence, &containerID, &containerStarted, &runtimeEpoch, &hostPort,
		&healthEvidence, &readyEvidence, &sessionEvidence, &promptEvidence, &writerEvidence, &routeEvidence,
		&containerRemovedEvidence, &volumeRemovedEvidence, &cloneRemovedEvidence, &evidence, &lastError,
		&provisionIntent, &cloneObserved, &volumeObserved, &containerObserved, &healthObserved, &readyAt,
		&sessionObserved, &promptIntent, &promptAttempted, &promptAdmitted, &timeoutRequested, &stopIntent, &writerInactive, &routeRemoved,
		&containerRemoved, &volumeRemoved, &cloneRemoved, &cleanupCompleted, &cleanupProof, &absenceProof,
		&run.Revision, &created, &updated, &sealRequestID, &artifactExportID, &retainedArtifactID, &materializationID, &retainedResultID, &resultAuthorityPhase,
		&run.Creator.Type, &run.Creator.ID, &run.Creator.DisplayName, &run.Creator.CredentialID, &run.Creator.Authentication, &run.Creator.RequestID,
		&stopType, &stopID, &stopName, &stopCredential, &stopAuth, &stopRequest,
		&timeoutType, &timeoutID, &timeoutName, &timeoutCredential, &timeoutAuth, &timeoutRequest)
	if err != nil {
		return BackgroundRun{}, err
	}
	if len(instructionHash) != 32 || len(profileHash) != 32 || len(environmentHash) != 32 || bytes.Equal(environmentHash, make([]byte, 32)) ||
		(run.ResourceSpecVersion != 8 && run.ResourceSpecVersion != 9) || repositoryID <= 0 || run.Generation <= 0 || cancelEpoch < 0 ||
		!run.State.valid() || !run.EffectPhase.valid() || !validBackgroundRunStatePhase(run.Profile, run.State, run.EffectPhase) || run.Creator.Validate() != nil || run.Creator.Type != task.ActorOpenCode {
		return BackgroundRun{}, ErrCorruptStore
	}
	copy(run.InstructionSHA256[:], instructionHash)
	copy(run.ProfileSHA256[:], profileHash)
	copy(run.EnvironmentSHA256[:], environmentHash)
	run.RepositoryID = task.RepositoryID(repositoryID)
	run.CancelEpoch = uint64(cancelEpoch)
	run.Branch = nullableString(branch)
	run.CreatedAt, run.UpdatedAt = fromUnixMillis(created), fromUnixMillis(updated)
	run.ClaimOwner = nullableText(claimOwner)
	run.ClaimExpiresAt = nullableTime(claimExpiry)
	run.ObservedContainerID = nullableText(containerID)
	run.ObservedContainerStartedAt = nullableText(containerStarted)
	run.RuntimeEpoch = runtimeEpoch.Int64
	run.HostPort = int(hostPort.Int64)
	run.CloneEvidence = nullableText(cloneEvidence)
	run.VolumeEvidence = nullableText(volumeEvidence)
	run.HealthEvidence = nullableText(healthEvidence)
	run.ReadyEvidence = nullableText(readyEvidence)
	run.SessionEvidence = nullableText(sessionEvidence)
	run.PromptEvidence = nullableText(promptEvidence)
	run.WriterInactiveEvidence = nullableText(writerEvidence)
	run.RouteRemovedEvidence = nullableText(routeEvidence)
	run.ContainerRemovedEvidence = nullableText(containerRemovedEvidence)
	run.VolumeRemovedEvidence = nullableText(volumeRemovedEvidence)
	run.CloneRemovedEvidence = nullableText(cloneRemovedEvidence)
	run.LastEvidence = nullableText(evidence)
	run.LastError = nullableText(lastError)
	run.ProvisionIntentAt = nullableTime(provisionIntent)
	run.CloneObservedAt = nullableTime(cloneObserved)
	run.VolumeObservedAt = nullableTime(volumeObserved)
	run.ContainerObservedAt = nullableTime(containerObserved)
	run.HealthObservedAt = nullableTime(healthObserved)
	run.ReadyAt = nullableTime(readyAt)
	run.SessionObservedAt = nullableTime(sessionObserved)
	run.PromptIntentAt = nullableTime(promptIntent)
	run.PromptRequestAttemptedAt = nullableTime(promptAttempted)
	run.PromptAdmittedAt = nullableTime(promptAdmitted)
	run.TimeoutRequestedAt = nullableTime(timeoutRequested)
	run.StopIntentAt = nullableTime(stopIntent)
	run.WriterInactiveAt = nullableTime(writerInactive)
	run.RouteRemovedAt = nullableTime(routeRemoved)
	run.ContainerRemovedAt = nullableTime(containerRemoved)
	run.VolumeRemovedAt = nullableTime(volumeRemoved)
	run.CloneRemovedAt = nullableTime(cloneRemoved)
	run.CleanupCompletedAt = nullableTime(cleanupCompleted)
	run.CleanupProof = nullableText(cleanupProof)
	run.AbsenceProof = nullableText(absenceProof)
	run.BackgroundSealRequestID = task.SealRequestID(nullableText(sealRequestID))
	run.ArtifactExportID = task.ArtifactExportID(nullableText(artifactExportID))
	run.RetainedArtifactID = task.RetainedArtifactID(nullableText(retainedArtifactID))
	run.MaterializationID = task.MaterializationID(nullableText(materializationID))
	run.RetainedResultID = task.ResultID(nullableText(retainedResultID))
	run.ResultAuthorityPhase = nullableText(resultAuthorityPhase)
	switch run.ResultAuthorityPhase {
	case "seal_intent":
		run.State, run.EffectPhase = BackgroundRunCanceling, BackgroundRunEffectSealIntent
	case "writer_inactive":
		// The physical cleanup state is already explicit in the stored tuple.
	case "exporting":
		run.EffectPhase = BackgroundRunEffectExporting
	case "artifact_committed":
		run.State, run.EffectPhase = BackgroundRunResultReady, BackgroundRunEffectArtifactCommitted
	case "cleanup", "legacy_result_not_retained", "":
	default:
		return BackgroundRun{}, ErrCorruptStore
	}
	if cancelEpoch == 1 {
		if !stopReceipt.Valid || !stopAt.Valid || !stopType.Valid || !stopID.Valid || !stopCredential.Valid || !stopAuth.Valid || !stopRequest.Valid {
			return BackgroundRun{}, ErrCorruptStore
		}
		run.StopReceiptID = task.ReceiptID(stopReceipt.String)
		run.StopRequestedAt = nullableTime(stopAt)
		actor := task.ActorSnapshot{Type: task.ActorType(stopType.String), ID: stopID.String, DisplayName: stopName.String,
			CredentialID: stopCredential.String, Authentication: stopAuth.String, RequestID: stopRequest.String}
		if actor.Validate() != nil {
			return BackgroundRun{}, ErrCorruptStore
		}
		run.StopActor = &actor
	}
	if timeoutRequested.Valid {
		if !timeoutType.Valid || !timeoutID.Valid || !timeoutCredential.Valid || !timeoutAuth.Valid || !timeoutRequest.Valid {
			return BackgroundRun{}, ErrCorruptStore
		}
		actor := task.ActorSnapshot{Type: task.ActorType(timeoutType.String), ID: timeoutID.String, DisplayName: timeoutName.String,
			CredentialID: timeoutCredential.String, Authentication: timeoutAuth.String, RequestID: timeoutRequest.String}
		if actor.Validate() != nil || actor.Type != task.ActorSystem {
			return BackgroundRun{}, ErrCorruptStore
		}
		run.TimeoutActor = &actor
	} else if timeoutType.Valid || timeoutID.Valid || timeoutCredential.Valid || timeoutAuth.Valid || timeoutRequest.Valid {
		return BackgroundRun{}, ErrCorruptStore
	}
	return run, nil
}

func validBackgroundRunStatePhase(profile string, state BackgroundRunState, phase BackgroundRunEffectPhase) bool {
	if profile == "opencode-1.18.16" {
		return state == BackgroundRunFailed && phase == BackgroundRunEffectCleanupComplete
	}
	if profile != BackgroundRunSourceProfile {
		return false
	}
	switch state {
	case BackgroundRunQueued:
		return phase == BackgroundRunEffectAbsent
	case BackgroundRunSettingUp:
		return phase == BackgroundRunEffectProvisionIntent || phase == BackgroundRunEffectCloneObserved || phase == BackgroundRunEffectVolumeObserved ||
			phase == BackgroundRunEffectContainerObserved || phase == BackgroundRunEffectHealthObserved || phase == BackgroundRunEffectReady || phase == BackgroundRunEffectSessionObserved
	case BackgroundRunWorking, BackgroundRunNeedsYou:
		return phase == BackgroundRunEffectPromptAdmitted
	case BackgroundRunCanceling:
		return cleanupEffectPhase(phase) || phase == BackgroundRunEffectSealIntent || phase == BackgroundRunEffectExporting
	case BackgroundRunUncertain:
		return phase == BackgroundRunEffectProvisionIntent || phase == BackgroundRunEffectCloneObserved || phase == BackgroundRunEffectVolumeObserved ||
			phase == BackgroundRunEffectContainerObserved || phase == BackgroundRunEffectHealthObserved || phase == BackgroundRunEffectReady ||
			phase == BackgroundRunEffectSessionObserved || phase == BackgroundRunEffectPromptIntent || phase == BackgroundRunEffectPromptAdmitted || phase == BackgroundRunEffectStopIntent
	case BackgroundRunResultReady:
		return phase == BackgroundRunEffectArtifactCommitted || cleanupEffectPhase(phase) || phase == BackgroundRunEffectCleanupComplete
	case BackgroundRunFailed:
		return phase == BackgroundRunEffectPreEffectFailed || phase == BackgroundRunEffectCleanupComplete
	case BackgroundRunCleanupRequired:
		return cleanupEffectPhase(phase) || phase == BackgroundRunEffectExporting
	default:
		return false
	}
}

func cleanupEffectPhase(phase BackgroundRunEffectPhase) bool {
	return phase == BackgroundRunEffectStopIntent || phase == BackgroundRunEffectWriterInactive || phase == BackgroundRunEffectRouteRemoved ||
		phase == BackgroundRunEffectContainerRemoved || phase == BackgroundRunEffectVolumeRemoved || phase == BackgroundRunEffectCloneRemoved
}

func nullableText(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}
