package taskstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nebler/fern/internal/evidence"
	"github.com/nebler/fern/internal/task"
)

const (
	maxDeliveryEvidenceBytes = 16 * 1024
	// maxDeliveryLease is the Go-side delivery lease bound. The SQL mirror of
	// this constant is the 300000-millisecond literal in the
	// attempts_delivery_resume_integrity trigger predicate
	// (migrations.go, initialSchema); they MUST stay equal.
	maxDeliveryLease = 5 * time.Minute
)

// FindPreparedAttempt returns the next accepted task whose current attempt can
// be claimed. ClaimPreparedAttempt remains the authority if another worker wins
// after this read.
func (s *Store) FindPreparedAttempt(ctx context.Context, workspaceID task.WorkspaceID) (DeliveryWork, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil {
		return DeliveryWork{}, fmt.Errorf("%w: workspace ID", ErrInvalidInput)
	}
	var attemptID task.AttemptID
	err := s.db.QueryRowContext(ctx, `
SELECT a.id
FROM attempts a
JOIN tasks t ON t.id=a.task_id AND t.workspace_id=a.workspace_id
JOIN events e ON e.task_id=t.id AND e.type='task.accepted'
WHERE a.workspace_id=? AND a.state='prepared' AND t.state='queued' AND t.current_attempt_id=a.id
ORDER BY e.cursor ASC
LIMIT 1`, workspaceID).Scan(&attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return DeliveryWork{}, &NotFoundError{Kind: "prepared attempt", ID: string(workspaceID)}
	}
	if err != nil {
		return DeliveryWork{}, fmt.Errorf("find prepared attempt: %w", err)
	}
	return s.InspectDeliveryAttempt(ctx, attemptID)
}

// FindDeliveringAttempt returns the workspace's single persisted delivery
// claim. The caller uses its recorded owner, revision, and expiry to reconcile
// or recover it; it must never re-claim the external effect as prepared.
func (s *Store) FindDeliveringAttempt(ctx context.Context, workspaceID task.WorkspaceID) (DeliveryWork, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil {
		return DeliveryWork{}, fmt.Errorf("%w: workspace ID", ErrInvalidInput)
	}
	var attemptID task.AttemptID
	err := s.db.QueryRowContext(ctx, `
SELECT a.id
FROM attempts a
JOIN tasks t ON t.id=a.task_id AND t.workspace_id=a.workspace_id
WHERE a.workspace_id=? AND a.state='delivering' AND t.state='running' AND t.current_attempt_id=a.id
LIMIT 1`, workspaceID).Scan(&attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return DeliveryWork{}, &NotFoundError{Kind: "delivering attempt", ID: string(workspaceID)}
	}
	if err != nil {
		return DeliveryWork{}, fmt.Errorf("find delivering attempt: %w", err)
	}
	return s.InspectDeliveryAttempt(ctx, attemptID)
}

// FindAmbiguousDeliveryAttempt returns the workspace's current delivery that
// either still owns a claim or requires reconciliation after an ambiguous
// effect. It never returns admitted execution work.
func (s *Store) FindAmbiguousDeliveryAttempt(ctx context.Context, workspaceID task.WorkspaceID) (DeliveryWork, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil {
		return DeliveryWork{}, fmt.Errorf("%w: workspace ID", ErrInvalidInput)
	}
	var attemptID task.AttemptID
	err := s.db.QueryRowContext(ctx, `
SELECT a.id
FROM attempts a
JOIN tasks t ON t.id=a.task_id AND t.workspace_id=a.workspace_id
WHERE a.workspace_id=? AND t.current_attempt_id=a.id AND
      ((a.state='delivering' AND t.state='running') OR (a.state='uncertain' AND t.state='uncertain'))
ORDER BY CASE a.state WHEN 'delivering' THEN 0 ELSE 1 END, a.id
LIMIT 1`, workspaceID).Scan(&attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return DeliveryWork{}, &NotFoundError{Kind: "ambiguous delivery attempt", ID: string(workspaceID)}
	}
	if err != nil {
		return DeliveryWork{}, fmt.Errorf("find ambiguous delivery attempt: %w", err)
	}
	attempt, err := getAttempt(ctx, s.db, attemptID)
	if err != nil {
		return DeliveryWork{}, err
	}
	owner, err := getTask(ctx, s.db, attempt.TaskID)
	if err != nil {
		return DeliveryWork{}, err
	}
	return DeliveryWork{Task: owner, Attempt: attempt}, nil
}

// InspectDeliveryAttempt reads only a delivery-eligible attempt and its owning
// task. It does not expose a generic update surface.
func (s *Store) InspectDeliveryAttempt(ctx context.Context, attemptID task.AttemptID) (DeliveryWork, error) {
	if _, err := task.ParseAttemptID(string(attemptID)); err != nil {
		return DeliveryWork{}, fmt.Errorf("%w: attempt ID", ErrInvalidInput)
	}
	attempt, err := getAttempt(ctx, s.db, attemptID)
	if errors.Is(err, ErrNotFound) {
		return DeliveryWork{}, &NotFoundError{Kind: "attempt", ID: string(attemptID)}
	}
	if err != nil {
		return DeliveryWork{}, err
	}
	if attempt.State != task.AttemptPrepared && attempt.State != task.AttemptDelivering {
		return DeliveryWork{}, &StateError{AttemptID: attempt.ID, State: attempt.State, Required: task.AttemptPrepared}
	}
	owner, err := getTask(ctx, s.db, attempt.TaskID)
	if err != nil {
		return DeliveryWork{}, err
	}
	return DeliveryWork{Task: owner, Attempt: attempt}, nil
}

// ClaimPreparedAttempt atomically fences a prepared attempt for one worker and
// moves its queued task to running. It performs no delivery effect.
func (s *Store) ClaimPreparedAttempt(ctx context.Context, p ClaimPreparedAttemptParams) (_ DeliveryTransition, err error) {
	if err := validateClaim(p); err != nil {
		return DeliveryTransition{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return DeliveryTransition{}, fmt.Errorf("begin delivery claim: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	attempt, owner, err := deliveryRows(ctx, tx, p.AttemptID)
	if err != nil {
		return DeliveryTransition{}, err
	}
	if attempt.State != task.AttemptPrepared {
		return DeliveryTransition{}, &StateError{AttemptID: attempt.ID, State: attempt.State, Required: task.AttemptPrepared}
	}
	if owner.State != task.TaskQueued || owner.CurrentAttemptID != attempt.ID {
		return DeliveryTransition{}, fmt.Errorf("%w: prepared attempt does not own a queued task", ErrInvalidState)
	}
	if p.Now.Before(attempt.CreatedAt) {
		return DeliveryTransition{}, fmt.Errorf("%w: claim precedes attempt", ErrInvalidInput)
	}
	if !p.Now.Before(attempt.Deadline) {
		return DeliveryTransition{}, fmt.Errorf("%w: attempt deadline reached", ErrInvalidState)
	}
	if p.LeaseExpiresAt.After(attempt.Deadline) {
		return DeliveryTransition{}, fmt.Errorf("%w: lease exceeds attempt deadline", ErrInvalidInput)
	}
	var busyAttempt task.AttemptID
	err = tx.QueryRowContext(ctx, `
SELECT id FROM attempts
WHERE workspace_id=? AND id<>? AND state IN
('delivering','admitted','running','input_required','cancel_requested','uncertain','recovery_required')
LIMIT 1`, attempt.WorkspaceID, attempt.ID).Scan(&busyAttempt)
	if err == nil {
		return DeliveryTransition{}, &WorkspaceBusyError{WorkspaceID: attempt.WorkspaceID, AttemptID: busyAttempt}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DeliveryTransition{}, fmt.Errorf("inspect workspace effect owner: %w", err)
	}

	actorID, err := ensureActor(ctx, tx, p.Actor)
	if err != nil {
		return DeliveryTransition{}, err
	}
	nowMS := unixMillis(p.Now)
	attemptPayload, err := json.Marshal(struct {
		LeaseOwner     string        `json:"leaseOwner"`
		LeaseExpiresAt time.Time     `json:"leaseExpiresAt"`
		Phase          DeliveryPhase `json:"phase"`
	}{p.LeaseOwner, p.LeaseExpiresAt, DeliveryPhaseClaimed})
	if err != nil {
		return DeliveryTransition{}, fmt.Errorf("encode delivery claim event: %w", err)
	}
	taskPayload, err := json.Marshal(struct {
		AttemptID task.AttemptID `json:"attemptId"`
	}{p.AttemptID})
	if err != nil {
		return DeliveryTransition{}, fmt.Errorf("encode running event: %w", err)
	}
	attemptEvent, err := insertAttemptEvent(ctx, tx, p.ClaimEventID, attempt, "attempt.delivery_started", nowMS, actorID, attemptPayload)
	if err != nil {
		return DeliveryTransition{}, err
	}
	taskEvent, err := insertTaskEvent(ctx, tx, p.TaskEventID, owner, "task.running", nowMS, actorID, taskPayload)
	if err != nil {
		return DeliveryTransition{}, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE attempts
SET state='delivering',delivery_phase='claimed',delivery_claim_owner=?,delivery_claim_expires_at=?,delivery_started_at=?,
    revision=revision+1,updated_at=?
WHERE id=? AND state='prepared' AND delivery_phase='none' AND revision=?`,
		p.LeaseOwner, unixMillis(p.LeaseExpiresAt), nowMS, nowMS, p.AttemptID, attempt.Revision)
	if err != nil {
		return DeliveryTransition{}, fmt.Errorf("claim prepared attempt: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return DeliveryTransition{}, fmt.Errorf("%w: claim lost", ErrInvalidState)
	}
	result, err = tx.ExecContext(ctx, `
UPDATE tasks SET state='running',latest_event_cursor=?,revision=revision+1,updated_at=?
WHERE id=? AND state='queued' AND current_attempt_id=?`, taskEvent.Cursor, nowMS, owner.ID, attempt.ID)
	if err != nil {
		return DeliveryTransition{}, fmt.Errorf("mark task running: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return DeliveryTransition{}, fmt.Errorf("%w: task state changed", ErrInvalidState)
	}
	return finishDeliveryTransition(ctx, tx, owner.ID, attempt.ID, attemptEvent, taskEvent)
}

// AdvanceDeliveryPhase persists one exact delivery-effect boundary. The caller
// must commit this before performing the effect represented by To.
func (s *Store) AdvanceDeliveryPhase(ctx context.Context, p AdvanceDeliveryPhaseParams) (_ DeliveryPhaseTransition, err error) {
	if err := validateAdvanceDeliveryPhase(p); err != nil {
		return DeliveryPhaseTransition{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return DeliveryPhaseTransition{}, fmt.Errorf("begin delivery phase advance: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	attempt, owner, err := deliveryRows(ctx, tx, p.AttemptID)
	if err != nil {
		return DeliveryPhaseTransition{}, err
	}
	if attempt.DeliveryClaimOwner == nil || *attempt.DeliveryClaimOwner != p.LeaseOwner {
		return DeliveryPhaseTransition{}, &LeaseConflictError{AttemptID: attempt.ID}
	}
	if attempt.Revision != p.ExpectedAttemptRevision {
		return DeliveryPhaseTransition{}, &StaleRevisionError{AttemptID: attempt.ID, Expected: p.ExpectedAttemptRevision, Actual: attempt.Revision}
	}
	if attempt.State != task.AttemptDelivering || attempt.DeliveryPhase != p.From {
		return DeliveryPhaseTransition{}, &StateError{AttemptID: attempt.ID, State: attempt.State, Required: task.AttemptDelivering}
	}
	if owner.State != task.TaskRunning || owner.CurrentAttemptID != attempt.ID {
		return DeliveryPhaseTransition{}, fmt.Errorf("%w: delivering attempt does not own a running task", ErrInvalidState)
	}
	if p.Now.Before(attempt.CreatedAt) {
		return DeliveryPhaseTransition{}, fmt.Errorf("%w: phase advance precedes attempt", ErrInvalidInput)
	}
	if !p.Now.Before(attempt.Deadline) {
		return DeliveryPhaseTransition{}, fmt.Errorf("%w: attempt deadline reached", ErrInvalidState)
	}
	if attempt.DeliveryClaimExpiresAt == nil || !p.Now.Before(*attempt.DeliveryClaimExpiresAt) {
		return DeliveryPhaseTransition{}, &LeaseConflictError{AttemptID: attempt.ID}
	}

	actorID, err := ensureActor(ctx, tx, p.Actor)
	if err != nil {
		return DeliveryPhaseTransition{}, err
	}
	payload, err := json.Marshal(struct {
		From DeliveryPhase `json:"from"`
		To   DeliveryPhase `json:"to"`
	}{p.From, p.To})
	if err != nil {
		return DeliveryPhaseTransition{}, fmt.Errorf("encode delivery phase event: %w", err)
	}
	nowMS := unixMillis(p.Now)
	event, err := insertAttemptEvent(ctx, tx, p.EventID, attempt, "attempt.delivery_phase_advanced", nowMS, actorID, payload)
	if err != nil {
		return DeliveryPhaseTransition{}, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE attempts SET delivery_phase=?,revision=revision+1,updated_at=?
WHERE id=? AND state='delivering' AND delivery_phase=? AND delivery_claim_owner=? AND revision=?`,
		p.To, nowMS, attempt.ID, p.From, p.LeaseOwner, p.ExpectedAttemptRevision)
	if err != nil {
		return DeliveryPhaseTransition{}, fmt.Errorf("advance delivery phase: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return DeliveryPhaseTransition{}, fmt.Errorf("%w: delivery phase advance lost", ErrLeaseConflict)
	}
	result, err = tx.ExecContext(ctx, `
UPDATE tasks SET latest_event_cursor=?,revision=revision+1,updated_at=?
WHERE id=? AND state='running' AND current_attempt_id=? AND revision=?`,
		event.Cursor, nowMS, owner.ID, attempt.ID, owner.Revision)
	if err != nil {
		return DeliveryPhaseTransition{}, fmt.Errorf("project delivery phase event: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return DeliveryPhaseTransition{}, fmt.Errorf("%w: task changed during delivery phase advance", ErrInvalidState)
	}
	storedAttempt, err := getAttempt(ctx, tx, attempt.ID)
	if err != nil {
		return DeliveryPhaseTransition{}, err
	}
	storedTask, err := getTask(ctx, tx, owner.ID)
	if err != nil {
		return DeliveryPhaseTransition{}, err
	}
	if storedTask.LatestEventCursor != event.Cursor {
		return DeliveryPhaseTransition{}, fmt.Errorf("%w: delivery phase event cursor", ErrCorruptStore)
	}
	if err := tx.Commit(); err != nil {
		return DeliveryPhaseTransition{}, fmt.Errorf("commit delivery phase advance: %w", err)
	}
	return DeliveryPhaseTransition{Task: storedTask, Attempt: storedAttempt, Event: event}, nil
}

// RecoverExpiredDeliveryClaim fences an expired worker and records ambiguity;
// it never makes the attempt prepared for another external effect.
func (s *Store) RecoverExpiredDeliveryClaim(ctx context.Context, p RecoverExpiredDeliveryClaimParams) (DeliveryTransition, error) {
	if err := validateRecovery(p); err != nil {
		return DeliveryTransition{}, err
	}
	payload, err := deliveryEvidencePayload(p.Reason, p.EvidencePayload, p.EvidenceSHA256)
	if err != nil {
		return DeliveryTransition{}, err
	}
	return s.recordDeliveryTransition(ctx, deliveryRecord{
		attemptID: p.AttemptID, leaseOwner: p.ExpiredLeaseOwner, expectedRevision: p.ExpectedAttemptRevision,
		attemptEventID: p.RecoveryEventID, taskEventID: p.TaskEventID, now: p.Now, actor: p.Actor,
		attemptState: task.AttemptUncertain, taskState: task.TaskUncertain,
		attemptEventType: "attempt.delivery_claim_expired", taskEventType: "task.uncertain",
		attemptPayload: payload, taskPayload: payload, recoveryReason: p.Reason, requireExpired: true,
	})
}

func (s *Store) RecordAdmission(ctx context.Context, p RecordAdmissionParams) (DeliveryTransition, error) {
	if err := validateRecordBase(p.AttemptID, p.LeaseOwner, p.ExpectedAttemptRevision, p.AttemptEventID, p.TaskEventID, p.Now, p.Actor); err != nil {
		return DeliveryTransition{}, err
	}
	payload, err := deliveryEvidencePayload("", p.EvidencePayload, p.EvidenceSHA256)
	if err != nil {
		return DeliveryTransition{}, err
	}
	return s.recordDeliveryTransition(ctx, deliveryRecord{
		attemptID: p.AttemptID, leaseOwner: p.LeaseOwner, expectedRevision: p.ExpectedAttemptRevision,
		attemptEventID: p.AttemptEventID, taskEventID: p.TaskEventID, now: p.Now, actor: p.Actor,
		attemptState: task.AttemptAdmitted, taskState: task.TaskRunning,
		attemptEventType: "attempt.admitted", taskEventType: "task.delivery_admitted",
		attemptPayload: payload, taskPayload: payload, admitted: true,
	})
}

func (s *Store) RecordDeliveryUncertain(ctx context.Context, p RecordDeliveryUncertainParams) (DeliveryTransition, error) {
	return s.recordDeliveryOutcome(ctx, p, task.AttemptUncertain, task.TaskUncertain, "attempt.delivery_uncertain", "task.uncertain")
}

func (s *Store) RecordDeliveryRecoveryRequired(ctx context.Context, p RecordDeliveryRecoveryRequiredParams) (DeliveryTransition, error) {
	return s.recordDeliveryOutcome(ctx, p, task.AttemptRecoveryRequired, task.TaskRecoveryRequired, "attempt.delivery_recovery_required", "task.recovery_required")
}

// ResolveUncertainDelivery records a read-only reconciliation result. It has no
// lease-owner input: an expired or fenced delivery worker cannot impersonate
// the reconciler and regain mutation authority.
func (s *Store) ResolveUncertainDelivery(ctx context.Context, p ResolveUncertainDeliveryParams) (_ DeliveryTransition, err error) {
	if err := validateResolveUncertainDelivery(p); err != nil {
		return DeliveryTransition{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return DeliveryTransition{}, fmt.Errorf("begin uncertain delivery resolution: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	attempt, owner, err := deliveryRows(ctx, tx, p.AttemptID)
	if err != nil {
		return DeliveryTransition{}, err
	}
	if attempt.Revision != p.ExpectedAttemptRevision {
		return DeliveryTransition{}, &StaleRevisionError{AttemptID: attempt.ID, Expected: p.ExpectedAttemptRevision, Actual: attempt.Revision}
	}
	if owner.Revision != p.ExpectedTaskRevision {
		return DeliveryTransition{}, &StaleTaskRevisionError{TaskID: owner.ID, Expected: p.ExpectedTaskRevision, Actual: owner.Revision}
	}
	if attempt.State != task.AttemptUncertain || owner.State != task.TaskUncertain || owner.CurrentAttemptID != attempt.ID {
		return DeliveryTransition{}, &StateError{AttemptID: attempt.ID, State: attempt.State, Required: task.AttemptUncertain}
	}
	if attempt.DeliveryClaimOwner != nil || attempt.DeliveryClaimExpiresAt != nil || attempt.DeliveryPhase == DeliveryPhaseNone {
		return DeliveryTransition{}, fmt.Errorf("%w: uncertain delivery ownership", ErrInvalidState)
	}
	if p.Now.Before(attempt.CreatedAt) {
		return DeliveryTransition{}, fmt.Errorf("%w: resolution precedes attempt", ErrInvalidInput)
	}
	if p.Outcome == ResolveUncertainDeliveryAdmitted && attempt.DeliveryPhase != DeliveryPhasePromptStarted {
		return DeliveryTransition{}, fmt.Errorf("%w: prompt delivery was not started", ErrInvalidState)
	}

	actorID, err := ensureActor(ctx, tx, p.Actor)
	if err != nil {
		return DeliveryTransition{}, err
	}
	payload, err := deliveryResolutionPayload(p.Outcome, attempt.DeliveryPhase, p.Reason, p.EvidencePayload, p.EvidenceSHA256)
	if err != nil {
		return DeliveryTransition{}, err
	}
	attemptState, taskState := task.AttemptAdmitted, task.TaskRunning
	attemptEventType, taskEventType := "attempt.admitted", "task.delivery_admitted"
	admittedAt, recoveryReason := any(unixMillis(p.Now)), any(nil)
	if p.Outcome == ResolveUncertainDeliveryRecoveryRequired {
		attemptState, taskState = task.AttemptRecoveryRequired, task.TaskRecoveryRequired
		attemptEventType, taskEventType = "attempt.delivery_recovery_required", "task.recovery_required"
		admittedAt, recoveryReason = nil, p.Reason
	}
	nowMS := unixMillis(p.Now)
	attemptEvent, err := insertAttemptEvent(ctx, tx, p.AttemptEventID, attempt, attemptEventType, nowMS, actorID, payload)
	if err != nil {
		return DeliveryTransition{}, err
	}
	taskEvent, err := insertTaskEvent(ctx, tx, p.TaskEventID, owner, taskEventType, nowMS, actorID, payload)
	if err != nil {
		return DeliveryTransition{}, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE attempts SET state=?,admitted_at=?,recovery_reason=?,revision=revision+1,updated_at=?
WHERE id=? AND state='uncertain' AND delivery_phase=? AND delivery_claim_owner IS NULL AND revision=?`,
		attemptState, admittedAt, recoveryReason, nowMS, attempt.ID, attempt.DeliveryPhase, p.ExpectedAttemptRevision)
	if err != nil {
		return DeliveryTransition{}, fmt.Errorf("resolve uncertain attempt: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return DeliveryTransition{}, fmt.Errorf("%w: uncertain attempt changed", ErrInvalidState)
	}
	result, err = tx.ExecContext(ctx, `
UPDATE tasks SET state=?,latest_event_cursor=?,revision=revision+1,updated_at=?
WHERE id=? AND state='uncertain' AND current_attempt_id=? AND revision=?`,
		taskState, taskEvent.Cursor, nowMS, owner.ID, attempt.ID, p.ExpectedTaskRevision)
	if err != nil {
		return DeliveryTransition{}, fmt.Errorf("resolve uncertain task: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return DeliveryTransition{}, fmt.Errorf("%w: uncertain task changed", ErrInvalidState)
	}
	return finishDeliveryTransition(ctx, tx, owner.ID, attempt.ID, attemptEvent, taskEvent)
}

// ResumeUncertainPrePromptDelivery installs a fresh delivery lease after
// read-only reconciliation proves that continuing from the persisted
// pre-prompt phase is safe. It performs no external effect.
func (s *Store) ResumeUncertainPrePromptDelivery(ctx context.Context, p ResumeUncertainPrePromptDeliveryParams) (_ DeliveryTransition, err error) {
	if err := validateResumeUncertainPrePromptDelivery(p); err != nil {
		return DeliveryTransition{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return DeliveryTransition{}, fmt.Errorf("begin uncertain delivery resume: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	attempt, owner, err := deliveryRows(ctx, tx, p.AttemptID)
	if err != nil {
		return DeliveryTransition{}, err
	}
	if attempt.Revision != p.ExpectedAttemptRevision {
		return DeliveryTransition{}, &StaleRevisionError{AttemptID: attempt.ID, Expected: p.ExpectedAttemptRevision, Actual: attempt.Revision}
	}
	if owner.Revision != p.ExpectedTaskRevision {
		return DeliveryTransition{}, &StaleTaskRevisionError{TaskID: owner.ID, Expected: p.ExpectedTaskRevision, Actual: owner.Revision}
	}
	if attempt.State != task.AttemptUncertain || owner.State != task.TaskUncertain || owner.CurrentAttemptID != attempt.ID {
		return DeliveryTransition{}, &StateError{AttemptID: attempt.ID, State: attempt.State, Required: task.AttemptUncertain}
	}
	if attempt.DeliveryPhase != p.ExpectedPhase || !resumablePrePromptPhase(attempt.DeliveryPhase) {
		return DeliveryTransition{}, fmt.Errorf("%w: uncertain delivery phase %s", ErrInvalidState, attempt.DeliveryPhase)
	}
	if attempt.DeliveryClaimOwner != nil || attempt.DeliveryClaimExpiresAt != nil {
		return DeliveryTransition{}, &LeaseConflictError{AttemptID: attempt.ID}
	}
	if attempt.AdmittedAt != nil || attempt.DeliveryStartedAt == nil {
		return DeliveryTransition{}, fmt.Errorf("%w: uncertain pre-prompt delivery shape", ErrInvalidState)
	}
	if p.Now.Before(attempt.CreatedAt) {
		return DeliveryTransition{}, fmt.Errorf("%w: resume precedes attempt", ErrInvalidInput)
	}
	if !p.Now.Before(attempt.Deadline) {
		return DeliveryTransition{}, fmt.Errorf("%w: attempt deadline reached", ErrInvalidState)
	}
	if p.LeaseExpiresAt.After(attempt.Deadline) {
		return DeliveryTransition{}, fmt.Errorf("%w: lease exceeds attempt deadline", ErrInvalidInput)
	}

	var busyAttempt task.AttemptID
	err = tx.QueryRowContext(ctx, `
SELECT id FROM attempts
WHERE workspace_id=? AND id<>? AND state IN
('delivering','admitted','running','input_required','cancel_requested','uncertain','recovery_required')
LIMIT 1`, attempt.WorkspaceID, attempt.ID).Scan(&busyAttempt)
	if err == nil {
		return DeliveryTransition{}, &WorkspaceBusyError{WorkspaceID: attempt.WorkspaceID, AttemptID: busyAttempt}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DeliveryTransition{}, fmt.Errorf("inspect workspace effect owner: %w", err)
	}

	actorID, err := ensureActor(ctx, tx, p.Actor)
	if err != nil {
		return DeliveryTransition{}, err
	}
	payload, err := deliveryResumePayload(attempt.ID, p.ExpectedAttemptRevision, p.ExpectedTaskRevision, p.ExpectedPhase, p.LeaseOwner, p.LeaseExpiresAt, p.EvidencePayload, p.EvidenceSHA256)
	if err != nil {
		return DeliveryTransition{}, err
	}
	nowMS := unixMillis(p.Now)
	attemptEvent, err := insertAttemptEvent(ctx, tx, p.AttemptEventID, attempt, "attempt.delivery_resumed", nowMS, actorID, payload)
	if err != nil {
		return DeliveryTransition{}, err
	}
	taskEvent, err := insertTaskEvent(ctx, tx, p.TaskEventID, owner, "task.running", nowMS, actorID, payload)
	if err != nil {
		return DeliveryTransition{}, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE attempts
SET state='delivering',delivery_claim_owner=?,delivery_claim_expires_at=?,recovery_reason=NULL,
    revision=revision+1,updated_at=?
WHERE id=? AND state='uncertain' AND delivery_phase=? AND delivery_claim_owner IS NULL
  AND delivery_claim_expires_at IS NULL AND revision=?`,
		p.LeaseOwner, unixMillis(p.LeaseExpiresAt), nowMS, attempt.ID, p.ExpectedPhase, p.ExpectedAttemptRevision)
	if err != nil {
		return DeliveryTransition{}, fmt.Errorf("resume uncertain delivery attempt: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return DeliveryTransition{}, fmt.Errorf("%w: uncertain delivery resume lost", ErrLeaseConflict)
	}
	result, err = tx.ExecContext(ctx, `
UPDATE tasks SET state='running',latest_event_cursor=?,revision=revision+1,updated_at=?
WHERE id=? AND state='uncertain' AND current_attempt_id=? AND revision=?`,
		taskEvent.Cursor, nowMS, owner.ID, attempt.ID, p.ExpectedTaskRevision)
	if err != nil {
		return DeliveryTransition{}, fmt.Errorf("resume uncertain delivery task: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return DeliveryTransition{}, fmt.Errorf("%w: uncertain delivery task changed", ErrInvalidState)
	}
	return finishDeliveryTransition(ctx, tx, owner.ID, attempt.ID, attemptEvent, taskEvent)
}

// ExpirePreparedAttempt terminates queued work whose immutable deadline has
// elapsed. It performs no wake or external effect.
func (s *Store) ExpirePreparedAttempt(ctx context.Context, p ExpirePreparedAttemptParams) (_ DeliveryTransition, err error) {
	if err := validateExpirePreparedAttempt(p); err != nil {
		return DeliveryTransition{}, err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return DeliveryTransition{}, fmt.Errorf("begin prepared attempt expiration: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	attempt, owner, err := deliveryRows(ctx, tx, p.AttemptID)
	if err != nil {
		return DeliveryTransition{}, err
	}
	if attempt.Revision != p.ExpectedAttemptRevision {
		return DeliveryTransition{}, &StaleRevisionError{AttemptID: attempt.ID, Expected: p.ExpectedAttemptRevision, Actual: attempt.Revision}
	}
	if owner.Revision != p.ExpectedTaskRevision {
		return DeliveryTransition{}, &StaleTaskRevisionError{TaskID: owner.ID, Expected: p.ExpectedTaskRevision, Actual: owner.Revision}
	}
	if attempt.State != task.AttemptPrepared || attempt.DeliveryPhase != DeliveryPhaseNone || owner.State != task.TaskQueued || owner.CurrentAttemptID != attempt.ID {
		return DeliveryTransition{}, &StateError{AttemptID: attempt.ID, State: attempt.State, Required: task.AttemptPrepared}
	}
	if p.Now.Before(attempt.Deadline) {
		return DeliveryTransition{}, fmt.Errorf("%w: attempt deadline has not elapsed", ErrInvalidState)
	}

	actorID, err := ensureActor(ctx, tx, p.Actor)
	if err != nil {
		return DeliveryTransition{}, err
	}
	payload := json.RawMessage(`{"reason":"deadline_elapsed"}`)
	nowMS := unixMillis(p.Now)
	attemptEvent, err := insertAttemptEvent(ctx, tx, p.AttemptEventID, attempt, "attempt.failed", nowMS, actorID, payload)
	if err != nil {
		return DeliveryTransition{}, err
	}
	taskEvent, err := insertTaskEvent(ctx, tx, p.TaskEventID, owner, "task.failed", nowMS, actorID, payload)
	if err != nil {
		return DeliveryTransition{}, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE attempts SET state='failed',terminal_reason=?,revision=revision+1,updated_at=?
WHERE id=? AND state='prepared' AND delivery_phase='none' AND revision=?`,
		PreparedAttemptDeadlineElapsed, nowMS, attempt.ID, p.ExpectedAttemptRevision)
	if err != nil {
		return DeliveryTransition{}, fmt.Errorf("expire prepared attempt: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return DeliveryTransition{}, fmt.Errorf("%w: prepared attempt changed", ErrInvalidState)
	}
	result, err = tx.ExecContext(ctx, `
UPDATE tasks SET state='failed',terminal_reason=?,latest_event_cursor=?,revision=revision+1,updated_at=?
WHERE id=? AND state='queued' AND current_attempt_id=? AND revision=?`,
		PreparedAttemptDeadlineElapsed, taskEvent.Cursor, nowMS, owner.ID, attempt.ID, p.ExpectedTaskRevision)
	if err != nil {
		return DeliveryTransition{}, fmt.Errorf("fail expired task: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return DeliveryTransition{}, fmt.Errorf("%w: queued task changed", ErrInvalidState)
	}
	return finishDeliveryTransition(ctx, tx, owner.ID, attempt.ID, attemptEvent, taskEvent)
}

func (s *Store) recordDeliveryOutcome(ctx context.Context, p RecordDeliveryUncertainParams, attemptState task.AttemptState, taskState task.TaskState, attemptEventType, taskEventType string) (DeliveryTransition, error) {
	if err := validateRecordBase(p.AttemptID, p.LeaseOwner, p.ExpectedAttemptRevision, p.AttemptEventID, p.TaskEventID, p.Now, p.Actor); err != nil {
		return DeliveryTransition{}, err
	}
	if !validBoundedText(p.Reason, 1, 1000) {
		return DeliveryTransition{}, fmt.Errorf("%w: delivery reason", ErrInvalidInput)
	}
	payload, err := deliveryEvidencePayload(p.Reason, p.EvidencePayload, p.EvidenceSHA256)
	if err != nil {
		return DeliveryTransition{}, err
	}
	return s.recordDeliveryTransition(ctx, deliveryRecord{
		attemptID: p.AttemptID, leaseOwner: p.LeaseOwner, expectedRevision: p.ExpectedAttemptRevision,
		attemptEventID: p.AttemptEventID, taskEventID: p.TaskEventID, now: p.Now, actor: p.Actor,
		attemptState: attemptState, taskState: taskState, attemptEventType: attemptEventType,
		taskEventType: taskEventType, attemptPayload: payload, taskPayload: payload, recoveryReason: p.Reason,
	})
}

type deliveryRecord struct {
	attemptID        task.AttemptID
	leaseOwner       string
	expectedRevision int64
	attemptEventID   task.EventID
	taskEventID      task.EventID
	now              time.Time
	actor            task.ActorSnapshot
	attemptState     task.AttemptState
	taskState        task.TaskState
	attemptEventType string
	taskEventType    string
	attemptPayload   json.RawMessage
	taskPayload      json.RawMessage
	recoveryReason   string
	admitted         bool
	requireExpired   bool
}

func (s *Store) recordDeliveryTransition(ctx context.Context, p deliveryRecord) (_ DeliveryTransition, err error) {
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return DeliveryTransition{}, fmt.Errorf("begin delivery transition: %w", err)
	}
	defer release()
	defer rollback(tx, &err)

	attempt, owner, err := deliveryRows(ctx, tx, p.attemptID)
	if err != nil {
		return DeliveryTransition{}, err
	}
	if attempt.DeliveryClaimOwner == nil || *attempt.DeliveryClaimOwner != p.leaseOwner {
		return DeliveryTransition{}, &LeaseConflictError{AttemptID: attempt.ID}
	}
	if attempt.Revision != p.expectedRevision {
		return DeliveryTransition{}, &StaleRevisionError{AttemptID: attempt.ID, Expected: p.expectedRevision, Actual: attempt.Revision}
	}
	if attempt.State != task.AttemptDelivering {
		return DeliveryTransition{}, &StateError{AttemptID: attempt.ID, State: attempt.State, Required: task.AttemptDelivering}
	}
	if attempt.DeliveryPhase == DeliveryPhaseNone || (p.admitted && attempt.DeliveryPhase != DeliveryPhasePromptStarted) {
		return DeliveryTransition{}, fmt.Errorf("%w: delivery phase %s", ErrInvalidState, attempt.DeliveryPhase)
	}
	if owner.State != task.TaskRunning || owner.CurrentAttemptID != attempt.ID {
		return DeliveryTransition{}, fmt.Errorf("%w: delivering attempt does not own a running task", ErrInvalidState)
	}
	if p.now.Before(attempt.CreatedAt) {
		return DeliveryTransition{}, fmt.Errorf("%w: transition precedes attempt", ErrInvalidInput)
	}
	expired := attempt.DeliveryClaimExpiresAt == nil || !p.now.Before(*attempt.DeliveryClaimExpiresAt)
	if p.requireExpired != expired {
		return DeliveryTransition{}, &LeaseConflictError{AttemptID: attempt.ID}
	}

	actorID, err := ensureActor(ctx, tx, p.actor)
	if err != nil {
		return DeliveryTransition{}, err
	}
	nowMS := unixMillis(p.now)
	admittedAt := any(nil)
	if p.admitted {
		admittedAt = nowMS
	}
	recoveryReason := any(nil)
	if p.recoveryReason != "" {
		recoveryReason = p.recoveryReason
	}
	result, err := tx.ExecContext(ctx, `
UPDATE attempts
SET state=?,delivery_claim_owner=NULL,delivery_claim_expires_at=NULL,admitted_at=?,recovery_reason=?,
    revision=revision+1,updated_at=?
WHERE id=? AND state='delivering' AND delivery_claim_owner=? AND revision=?`,
		p.attemptState, admittedAt, recoveryReason, nowMS, attempt.ID, p.leaseOwner, p.expectedRevision)
	if err != nil {
		return DeliveryTransition{}, fmt.Errorf("record delivery transition: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return DeliveryTransition{}, fmt.Errorf("%w: delivery transition lost", ErrLeaseConflict)
	}
	attemptEvent, err := insertAttemptEvent(ctx, tx, p.attemptEventID, attempt, p.attemptEventType, nowMS, actorID, p.attemptPayload)
	if err != nil {
		return DeliveryTransition{}, err
	}
	taskEvent, err := insertTaskEvent(ctx, tx, p.taskEventID, owner, p.taskEventType, nowMS, actorID, p.taskPayload)
	if err != nil {
		return DeliveryTransition{}, err
	}
	result, err = tx.ExecContext(ctx, `
UPDATE tasks SET state=?,latest_event_cursor=?,revision=revision+1,updated_at=?
WHERE id=? AND state='running' AND current_attempt_id=?`, p.taskState, taskEvent.Cursor, nowMS, owner.ID, attempt.ID)
	if err != nil {
		return DeliveryTransition{}, fmt.Errorf("record delivery task state: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return DeliveryTransition{}, fmt.Errorf("%w: task state changed", ErrInvalidState)
	}
	return finishDeliveryTransition(ctx, tx, owner.ID, attempt.ID, attemptEvent, taskEvent)
}

func finishDeliveryTransition(ctx context.Context, tx *sql.Tx, taskID task.TaskID, attemptID task.AttemptID, attemptEvent, taskEvent Event) (DeliveryTransition, error) {
	owner, err := getTask(ctx, tx, taskID)
	if err != nil {
		return DeliveryTransition{}, err
	}
	attempt, err := getAttempt(ctx, tx, attemptID)
	if err != nil {
		return DeliveryTransition{}, err
	}
	if owner.LatestEventCursor != taskEvent.Cursor || attemptEvent.Cursor >= taskEvent.Cursor {
		return DeliveryTransition{}, fmt.Errorf("%w: delivery event ordering", ErrCorruptStore)
	}
	if err := tx.Commit(); err != nil {
		return DeliveryTransition{}, fmt.Errorf("commit delivery transition: %w", err)
	}
	return DeliveryTransition{Task: owner, Attempt: attempt, AttemptEvent: attemptEvent, TaskEvent: taskEvent}, nil
}

func deliveryRows(ctx context.Context, tx *sql.Tx, attemptID task.AttemptID) (Attempt, Task, error) {
	attempt, err := getAttempt(ctx, tx, attemptID)
	if errors.Is(err, ErrNotFound) {
		return Attempt{}, Task{}, &NotFoundError{Kind: "attempt", ID: string(attemptID)}
	}
	if err != nil {
		return Attempt{}, Task{}, err
	}
	owner, err := getTask(ctx, tx, attempt.TaskID)
	if err != nil {
		return Attempt{}, Task{}, err
	}
	if owner.WorkspaceID != attempt.WorkspaceID {
		return Attempt{}, Task{}, fmt.Errorf("%w: attempt workspace ownership", ErrCorruptStore)
	}
	return attempt, owner, nil
}

func insertAttemptEvent(ctx context.Context, tx *sql.Tx, id task.EventID, attempt Attempt, eventType string, occurredAt, actorID int64, payload []byte) (Event, error) {
	return insertDeliveryEvent(ctx, tx, id, attempt.WorkspaceID, attempt.TaskID, attempt.ID, "attempt", string(attempt.ID), eventType, occurredAt, actorID, payload)
}

func insertTaskEvent(ctx context.Context, tx *sql.Tx, id task.EventID, owner Task, eventType string, occurredAt, actorID int64, payload []byte) (Event, error) {
	return insertDeliveryEvent(ctx, tx, id, owner.WorkspaceID, owner.ID, "", "task", string(owner.ID), eventType, occurredAt, actorID, payload)
}

func insertDeliveryEvent(ctx context.Context, tx *sql.Tx, id task.EventID, workspaceID task.WorkspaceID, taskID task.TaskID, attemptID task.AttemptID, entityType, entityID, eventType string, occurredAt, actorID int64, payload []byte) (Event, error) {
	var attemptValue any
	if attemptID != "" {
		attemptValue = attemptID
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO events(id,workspace_id,task_id,attempt_id,entity_type,entity_id,type,version,occurred_at,actor_snapshot_id,payload)
VALUES(?,?,?,?,?,?,?,1,?,?,?)`, id, workspaceID, taskID, attemptValue, entityType, entityID, eventType, occurredAt, actorID, string(payload))
	if err != nil {
		return Event{}, fmt.Errorf("insert %s event: %w", eventType, err)
	}
	cursor, err := result.LastInsertId()
	if err != nil || cursor <= 0 {
		return Event{}, fmt.Errorf("read %s event cursor: %w", eventType, err)
	}
	event, err := scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.id=?`, id))
	if err != nil {
		return Event{}, fmt.Errorf("read %s event: %w", eventType, err)
	}
	if int64(event.Cursor) != cursor {
		return Event{}, fmt.Errorf("%w: event cursor changed", ErrCorruptStore)
	}
	return event, nil
}

func validateClaim(p ClaimPreparedAttemptParams) error {
	if err := validateAttemptAndEvents(p.AttemptID, p.ClaimEventID, p.TaskEventID); err != nil {
		return err
	}
	if !validBoundedText(p.LeaseOwner, 1, 64) {
		return fmt.Errorf("%w: lease owner", ErrInvalidInput)
	}
	if err := validExactTimestamp(p.Now); err != nil {
		return err
	}
	if err := validExactTimestamp(p.LeaseExpiresAt); err != nil || !p.LeaseExpiresAt.After(p.Now) || p.LeaseExpiresAt.Sub(p.Now) > maxDeliveryLease {
		return fmt.Errorf("%w: lease expiry", ErrInvalidInput)
	}
	if err := p.Actor.Validate(); err != nil {
		return fmt.Errorf("%w: actor: %v", ErrInvalidInput, err)
	}
	return nil
}

func validateAdvanceDeliveryPhase(p AdvanceDeliveryPhaseParams) error {
	if _, err := task.ParseAttemptID(string(p.AttemptID)); err != nil {
		return fmt.Errorf("%w: attempt ID", ErrInvalidInput)
	}
	if _, err := task.ParseEventID(string(p.EventID)); err != nil {
		return fmt.Errorf("%w: event ID", ErrInvalidInput)
	}
	if !validBoundedText(p.LeaseOwner, 1, 64) || p.ExpectedAttemptRevision < 1 {
		return fmt.Errorf("%w: lease owner or revision", ErrInvalidInput)
	}
	if !p.From.valid() || !p.To.valid() || !validDeliveryPhaseAdvance(p.From, p.To) {
		return fmt.Errorf("%w: delivery phase transition", ErrInvalidInput)
	}
	if err := validExactTimestamp(p.Now); err != nil {
		return err
	}
	if err := p.Actor.Validate(); err != nil {
		return fmt.Errorf("%w: actor: %v", ErrInvalidInput, err)
	}
	return nil
}

func validDeliveryPhaseAdvance(from, to DeliveryPhase) bool {
	return (from == DeliveryPhaseClaimed && to == DeliveryPhaseSessionCreateStarted) ||
		(from == DeliveryPhaseSessionCreateStarted && to == DeliveryPhaseSessionReady) ||
		(from == DeliveryPhaseSessionReady && to == DeliveryPhasePromptStarted)
}

func validateRecovery(p RecoverExpiredDeliveryClaimParams) error {
	if err := validateRecordBase(p.AttemptID, p.ExpiredLeaseOwner, p.ExpectedAttemptRevision, p.RecoveryEventID, p.TaskEventID, p.Now, p.Actor); err != nil {
		return err
	}
	if !validBoundedText(p.Reason, 1, 1000) {
		return fmt.Errorf("%w: recovery reason", ErrInvalidInput)
	}
	return validateDeliveryEvidence(p.EvidencePayload, p.EvidenceSHA256)
}

func validateResolveUncertainDelivery(p ResolveUncertainDeliveryParams) error {
	if err := validateAttemptAndEvents(p.AttemptID, p.AttemptEventID, p.TaskEventID); err != nil {
		return err
	}
	if p.ExpectedAttemptRevision < 1 || p.ExpectedTaskRevision < 1 || !p.Outcome.valid() {
		return fmt.Errorf("%w: revisions or resolution outcome", ErrInvalidInput)
	}
	if p.Outcome == ResolveUncertainDeliveryAdmitted {
		if p.Reason != "" {
			return fmt.Errorf("%w: admitted resolution reason", ErrInvalidInput)
		}
	} else if !validBoundedText(p.Reason, 1, 1000) {
		return fmt.Errorf("%w: recovery reason", ErrInvalidInput)
	}
	if err := validExactTimestamp(p.Now); err != nil {
		return err
	}
	if err := p.Actor.Validate(); err != nil {
		return fmt.Errorf("%w: actor: %v", ErrInvalidInput, err)
	}
	return validateDeliveryEvidence(p.EvidencePayload, p.EvidenceSHA256)
}

func validateResumeUncertainPrePromptDelivery(p ResumeUncertainPrePromptDeliveryParams) error {
	if err := validateAttemptAndEvents(p.AttemptID, p.AttemptEventID, p.TaskEventID); err != nil {
		return err
	}
	if p.ExpectedAttemptRevision < 1 || p.ExpectedTaskRevision < 1 || !resumablePrePromptPhase(p.ExpectedPhase) {
		return fmt.Errorf("%w: revisions or expected delivery phase", ErrInvalidInput)
	}
	if !validBoundedText(p.LeaseOwner, 1, 64) {
		return fmt.Errorf("%w: lease owner", ErrInvalidInput)
	}
	if err := validExactTimestamp(p.Now); err != nil {
		return err
	}
	if err := validExactTimestamp(p.LeaseExpiresAt); err != nil || !p.LeaseExpiresAt.After(p.Now) || p.LeaseExpiresAt.Sub(p.Now) > maxDeliveryLease {
		return fmt.Errorf("%w: lease expiry", ErrInvalidInput)
	}
	if err := p.Actor.Validate(); err != nil || p.Actor.Type != task.ActorRecovery {
		return fmt.Errorf("%w: recovery actor", ErrInvalidInput)
	}
	return validateDeliveryEvidence(p.EvidencePayload, p.EvidenceSHA256)
}

func resumablePrePromptPhase(phase DeliveryPhase) bool {
	return phase == DeliveryPhaseClaimed || phase == DeliveryPhaseSessionCreateStarted || phase == DeliveryPhaseSessionReady
}

func validateExpirePreparedAttempt(p ExpirePreparedAttemptParams) error {
	if err := validateAttemptAndEvents(p.AttemptID, p.AttemptEventID, p.TaskEventID); err != nil {
		return err
	}
	if p.ExpectedAttemptRevision < 1 || p.ExpectedTaskRevision < 1 {
		return fmt.Errorf("%w: revisions", ErrInvalidInput)
	}
	if err := validExactTimestamp(p.Now); err != nil {
		return err
	}
	if err := p.Actor.Validate(); err != nil {
		return fmt.Errorf("%w: actor: %v", ErrInvalidInput, err)
	}
	return nil
}

func validateRecordBase(attemptID task.AttemptID, leaseOwner string, revision int64, attemptEventID, taskEventID task.EventID, now time.Time, actor task.ActorSnapshot) error {
	if err := validateAttemptAndEvents(attemptID, attemptEventID, taskEventID); err != nil {
		return err
	}
	if !validBoundedText(leaseOwner, 1, 64) || revision < 1 {
		return fmt.Errorf("%w: lease owner or revision", ErrInvalidInput)
	}
	if err := validExactTimestamp(now); err != nil {
		return err
	}
	if err := actor.Validate(); err != nil {
		return fmt.Errorf("%w: actor: %v", ErrInvalidInput, err)
	}
	return nil
}

func validateAttemptAndEvents(attemptID task.AttemptID, attemptEventID, taskEventID task.EventID) error {
	if _, err := task.ParseAttemptID(string(attemptID)); err != nil {
		return fmt.Errorf("%w: attempt ID", ErrInvalidInput)
	}
	if _, err := task.ParseEventID(string(attemptEventID)); err != nil {
		return fmt.Errorf("%w: attempt event ID", ErrInvalidInput)
	}
	if _, err := task.ParseEventID(string(taskEventID)); err != nil || attemptEventID == taskEventID {
		return fmt.Errorf("%w: task event ID", ErrInvalidInput)
	}
	return nil
}

func validExactTimestamp(v time.Time) error {
	if err := validTimestamp(v); err != nil || !v.Equal(fromUnixMillis(unixMillis(v))) {
		return fmt.Errorf("%w: timestamp must be exact Unix milliseconds", ErrInvalidInput)
	}
	return nil
}

func validateDeliveryEvidence(payload json.RawMessage, expected [32]byte) error {
	trimmed := bytes.TrimSpace(payload)
	if len(payload) < 2 || len(payload) > maxDeliveryEvidenceBytes || !json.Valid(payload) || len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return fmt.Errorf("%w: delivery evidence must be a bounded JSON object", ErrInvalidInput)
	}
	if actual := sha256.Sum256(payload); actual != expected {
		return fmt.Errorf("%w: delivery evidence hash", ErrInvalidInput)
	}
	var decoded any
	if json.Unmarshal(payload, &decoded) != nil || evidence.ContainsSensitiveKey(decoded) {
		return fmt.Errorf("%w: delivery evidence contains a sensitive raw field", ErrInvalidInput)
	}
	return nil
}

func deliveryEvidencePayload(reason string, evidence json.RawMessage, evidenceHash [32]byte) (json.RawMessage, error) {
	if err := validateDeliveryEvidence(evidence, evidenceHash); err != nil {
		return nil, err
	}
	var encoded bytes.Buffer
	encoded.WriteByte('{')
	if reason != "" {
		reasonJSON, err := json.Marshal(reason)
		if err != nil {
			return nil, fmt.Errorf("encode delivery reason: %w", err)
		}
		encoded.WriteString(`"reason":`)
		encoded.Write(reasonJSON)
		encoded.WriteByte(',')
	}
	encoded.WriteString(`"evidence":`)
	encoded.Write(evidence)
	encoded.WriteString(`,"evidenceSha256":"sha256:`)
	encoded.WriteString(hex.EncodeToString(evidenceHash[:]))
	encoded.WriteString(`"}`)
	if !json.Valid(encoded.Bytes()) {
		return nil, fmt.Errorf("%w: encoded delivery evidence", ErrCorruptStore)
	}
	return encoded.Bytes(), nil
}

func deliveryResolutionPayload(outcome ResolveUncertainDeliveryOutcome, phase DeliveryPhase, reason string, evidence json.RawMessage, evidenceHash [32]byte) (json.RawMessage, error) {
	base, err := deliveryEvidencePayload(reason, evidence, evidenceHash)
	if err != nil {
		return nil, err
	}
	outcomeJSON, _ := json.Marshal(outcome)
	phaseJSON, _ := json.Marshal(phase)
	payload := make([]byte, 0, len(base)+len(outcomeJSON)+len(phaseJSON)+24)
	payload = append(payload, "{\"outcome\":"...)
	payload = append(payload, outcomeJSON...)
	payload = append(payload, ",\"phase\":"...)
	payload = append(payload, phaseJSON...)
	payload = append(payload, ',')
	payload = append(payload, base[1:]...)
	return payload, nil
}

func deliveryResumePayload(attemptID task.AttemptID, attemptRevision, taskRevision int64, phase DeliveryPhase, leaseOwner string, leaseExpiresAt time.Time, evidence json.RawMessage, evidenceHash [32]byte) (json.RawMessage, error) {
	base, err := deliveryEvidencePayload("", evidence, evidenceHash)
	if err != nil {
		return nil, err
	}
	attemptJSON, _ := json.Marshal(attemptID)
	phaseJSON, _ := json.Marshal(phase)
	ownerJSON, _ := json.Marshal(leaseOwner)
	payload := make([]byte, 0, len(base)+len(attemptJSON)+len(phaseJSON)+len(ownerJSON)+160)
	payload = append(payload, "{\"attemptId\":"...)
	payload = append(payload, attemptJSON...)
	payload = append(payload, fmt.Sprintf(",\"expectedAttemptRevision\":%d,\"expectedTaskRevision\":%d", attemptRevision, taskRevision)...)
	payload = append(payload, ",\"phase\":"...)
	payload = append(payload, phaseJSON...)
	payload = append(payload, ",\"leaseOwner\":"...)
	payload = append(payload, ownerJSON...)
	payload = append(payload, fmt.Sprintf(",\"leaseExpiresAtMillis\":%d,", unixMillis(leaseExpiresAt))...)
	payload = append(payload, base[1:]...)
	if !json.Valid(payload) {
		return nil, fmt.Errorf("%w: encoded delivery resume payload", ErrCorruptStore)
	}
	return payload, nil
}
