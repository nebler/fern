// Package taskdelivery coordinates durable task delivery to OpenCode.
package taskdelivery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/nebler/fern/internal/opencodeapi"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
	"github.com/nebler/fern/internal/workspace"
)

var (
	ErrNoWork              = errors.New("no task delivery work is available")
	ErrLeaseActive         = errors.New("task delivery lease is still active")
	ErrImageConflict       = errors.New("workspace image identity conflicts with the attempt")
	ErrCancellationPending = errors.New("task cancellation is not yet proven complete")
	ErrDeliveryPending     = errors.New("task delivery reconciliation is not yet conclusive")
)

const defaultOperationTimeout = 30 * time.Second

type Store interface {
	FindPendingCancellation(context.Context, task.WorkspaceID) (taskstore.Cancellation, error)
	AcknowledgeCancellation(context.Context, taskstore.AcknowledgeCancellationParams) (taskstore.CancellationAcknowledgment, error)
	FindPreparedAttempt(context.Context, task.WorkspaceID) (taskstore.DeliveryWork, error)
	FindAmbiguousDeliveryAttempt(context.Context, task.WorkspaceID) (taskstore.DeliveryWork, error)
	ClaimPreparedAttempt(context.Context, taskstore.ClaimPreparedAttemptParams) (taskstore.DeliveryTransition, error)
	AdvanceDeliveryPhase(context.Context, taskstore.AdvanceDeliveryPhaseParams) (taskstore.DeliveryPhaseTransition, error)
	RecoverExpiredDeliveryClaim(context.Context, taskstore.RecoverExpiredDeliveryClaimParams) (taskstore.DeliveryTransition, error)
	RecordAdmission(context.Context, taskstore.RecordAdmissionParams) (taskstore.DeliveryTransition, error)
	RecordDeliveryUncertain(context.Context, taskstore.RecordDeliveryUncertainParams) (taskstore.DeliveryTransition, error)
	RecordDeliveryRecoveryRequired(context.Context, taskstore.RecordDeliveryRecoveryRequiredParams) (taskstore.DeliveryTransition, error)
	ResolveUncertainDelivery(context.Context, taskstore.ResolveUncertainDeliveryParams) (taskstore.DeliveryTransition, error)
	ResumeUncertainPrePromptDelivery(context.Context, taskstore.ResumeUncertainPrePromptDeliveryParams) (taskstore.DeliveryTransition, error)
	ExpirePreparedAttempt(context.Context, taskstore.ExpirePreparedAttemptParams) (taskstore.DeliveryTransition, error)
}

type TargetAcquirer interface {
	AcquireRequest(context.Context, workspace.RequestIntent) (workspace.RequestTarget, func(), error)
	InvalidateEndpoint(workspace.RequestTarget)
}

type OpenCode interface {
	CreateOrReuseSession(context.Context, opencodeapi.CreateSessionRequest) (opencodeapi.Session, error)
	ReconcileSession(context.Context, opencodeapi.CreateSessionRequest) (opencodeapi.MatchState, error)
	AdmitPrompt(context.Context, string, opencodeapi.PromptRequest) (opencodeapi.Admission, error)
	ReconcilePrompt(context.Context, string, opencodeapi.PromptRequest) (opencodeapi.PromptObservation, error)
	CancelInboxOnce(context.Context, string, string) error
	ActiveSessions(context.Context) (opencodeapi.ActiveSessions, error)
	Interrupt(context.Context, string) error
}

type ClientFactory func(workspace.RequestTarget) (OpenCode, error)

type Config struct {
	WorkspaceID      task.WorkspaceID
	WorkerID         string
	SessionDirectory string
	LeaseDuration    time.Duration
	OperationTimeout time.Duration
	PollInterval     time.Duration
	Actor            task.ActorSnapshot
	RecoveryActor    task.ActorSnapshot
	Now              func() time.Time
	OnError          func(error)
}

type Coordinator struct {
	store   Store
	targets TargetAcquirer
	clients ClientFactory
	ids     *task.Generator
	config  Config
	wake    chan struct{}
	runMu   sync.Mutex
}

func New(store Store, targets TargetAcquirer, clients ClientFactory, ids *task.Generator, config Config) (*Coordinator, error) {
	if store == nil || targets == nil || clients == nil || ids == nil {
		return nil, errors.New("task delivery dependencies are required")
	}
	if _, err := task.ParseWorkspaceID(string(config.WorkspaceID)); err != nil {
		return nil, errors.New("valid task delivery workspace is required")
	}
	if !validASCII(config.WorkerID, 1, 64) || len(config.SessionDirectory) < 2 || len(config.SessionDirectory) > 4096 ||
		!strings.HasPrefix(config.SessionDirectory, "/") || path.Clean(config.SessionDirectory) != config.SessionDirectory || strings.ContainsAny(config.SessionDirectory, "\x00\r\n") {
		return nil, errors.New("valid task delivery worker and session directory are required")
	}
	if config.LeaseDuration <= 0 || config.LeaseDuration > 5*time.Minute {
		return nil, errors.New("task delivery lease must be between zero and five minutes")
	}
	if config.OperationTimeout <= 0 {
		config.OperationTimeout = defaultOperationTimeout
	}
	if config.PollInterval <= 0 {
		config.PollInterval = time.Second
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.OnError == nil {
		config.OnError = func(error) {}
	}
	if err := config.Actor.Validate(); err != nil || config.Actor.Type != task.ActorSystem {
		return nil, errors.New("valid system delivery actor is required")
	}
	if err := config.RecoveryActor.Validate(); err != nil || config.RecoveryActor.Type != task.ActorRecovery {
		return nil, errors.New("valid recovery actor is required")
	}
	return &Coordinator{store: store, targets: targets, clients: clients, ids: ids, config: config, wake: make(chan struct{}, 1)}, nil
}

// Wake requests a prompt scheduling pass without blocking the caller.
func (coordinator *Coordinator) Wake() {
	select {
	case coordinator.wake <- struct{}{}:
	default:
	}
}

// Run reconciles persisted ambiguity at startup and then drains available work
// after wake notifications or bounded polling intervals.
func (coordinator *Coordinator) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-coordinator.wake:
		case <-timer.C:
		}
		for {
			err := coordinator.RunOnce(ctx)
			if err == nil {
				continue
			}
			if !errors.Is(err, ErrNoWork) && !errors.Is(err, ErrLeaseActive) && !errors.Is(err, ErrCancellationPending) && !errors.Is(err, ErrDeliveryPending) {
				if errors.Is(err, taskstore.ErrCorruptStore) {
					return err
				}
				coordinator.config.OnError(err)
			}
			break
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(coordinator.config.PollInterval)
	}
}

// RunOnce performs at most one persisted delivery or reconciliation sequence.
// Calls are serialized so one process never owns two local workers.
func (coordinator *Coordinator) RunOnce(ctx context.Context) error {
	coordinator.runMu.Lock()
	defer coordinator.runMu.Unlock()

	cancellation, err := coordinator.store.FindPendingCancellation(ctx, coordinator.config.WorkspaceID)
	if err == nil {
		return coordinator.cancel(ctx, cancellation)
	}
	if !errors.Is(err, taskstore.ErrNotFound) {
		return err
	}

	work, err := coordinator.store.FindAmbiguousDeliveryAttempt(ctx, coordinator.config.WorkspaceID)
	if err == nil {
		return coordinator.reconcile(ctx, work)
	}
	if !errors.Is(err, taskstore.ErrNotFound) {
		return err
	}
	work, err = coordinator.store.FindPreparedAttempt(ctx, coordinator.config.WorkspaceID)
	if errors.Is(err, taskstore.ErrNotFound) {
		return ErrNoWork
	}
	if err != nil {
		return err
	}
	now := coordinator.now()
	if !now.Before(work.Attempt.Deadline) {
		attemptEvent, taskEvent, err := coordinator.eventPair()
		if err != nil {
			return err
		}
		_, err = coordinator.store.ExpirePreparedAttempt(ctx, taskstore.ExpirePreparedAttemptParams{
			AttemptID: work.Attempt.ID, ExpectedAttemptRevision: work.Attempt.Revision,
			ExpectedTaskRevision: work.Task.Revision, AttemptEventID: attemptEvent,
			TaskEventID: taskEvent, Now: now, Actor: coordinator.config.Actor,
		})
		return err
	}
	claimEvent, taskEvent, err := coordinator.eventPair()
	if err != nil {
		return err
	}
	leaseExpiry := coordinator.leaseExpiry(now, work.Attempt.Deadline)
	claimed, err := coordinator.store.ClaimPreparedAttempt(ctx, taskstore.ClaimPreparedAttemptParams{
		AttemptID: work.Attempt.ID, LeaseOwner: coordinator.config.WorkerID,
		ClaimEventID: claimEvent, TaskEventID: taskEvent, Now: now,
		LeaseExpiresAt: leaseExpiry, Actor: coordinator.config.Actor,
	})
	if err != nil {
		return err
	}
	return coordinator.deliver(ctx, taskstore.DeliveryWork{Task: claimed.Task, Attempt: claimed.Attempt})
}

func (coordinator *Coordinator) cancel(ctx context.Context, cancellation taskstore.Cancellation) error {
	switch cancellation.Disposition {
	case taskstore.CancellationEffectNonePrepared:
		return coordinator.acknowledgeCancellation(ctx, cancellation, evidence("cancellation_noop", "absent", "absent", "unobservable"))
	case taskstore.CancellationEffectNoneTerminal:
		return coordinator.acknowledgeCancellation(ctx, cancellation, evidence("cancellation_terminal", "unobserved", "unobserved", "unobservable"))
	case taskstore.CancellationEffectReconcileDelivery, taskstore.CancellationEffectInterrupt:
	default:
		return ErrCancellationPending
	}

	_, release, client, err := coordinator.acquireCancellationClient(ctx, cancellation.Attempt)
	if err != nil {
		return err
	}
	defer release()
	work := taskstore.DeliveryWork{Task: cancellation.Task, Attempt: cancellation.Attempt}
	if cancellation.Disposition == taskstore.CancellationEffectReconcileDelivery {
		switch cancellation.Attempt.DeliveryPhase {
		case taskstore.DeliveryPhaseClaimed:
			return coordinator.acknowledgeCancellation(ctx, cancellation, evidence("cancellation_delivery_reconcile", "absent", "absent", "unobservable"))
		case taskstore.DeliveryPhaseSessionCreateStarted, taskstore.DeliveryPhaseSessionReady:
			requestCtx, cancel := coordinator.cancellationContext(ctx)
			match, readErr := client.ReconcileSession(requestCtx, coordinator.sessionRequest(work))
			cancel()
			if readErr != nil || (match != opencodeapi.MatchExact && match != opencodeapi.MatchAbsent) {
				return firstError(readErr, ErrCancellationPending)
			}
			return coordinator.acknowledgeCancellation(ctx, cancellation, evidence("cancellation_session_reconcile", string(match), "absent", "unobservable"))
		case taskstore.DeliveryPhasePromptStarted:
		default:
			return ErrCancellationPending
		}
	}

	requestCtx, cancel := coordinator.cancellationContext(ctx)
	observation, err := client.ReconcilePrompt(requestCtx, string(cancellation.Attempt.OpenCodeSessionID), coordinator.promptRequest(work))
	cancel()
	if err != nil || observation.Session != opencodeapi.MatchExact || observation.Inbox == opencodeapi.MatchConflict || observation.Message == opencodeapi.MatchConflict {
		return firstError(err, ErrCancellationPending)
	}
	if observation.Inbox == opencodeapi.MatchExact && observation.Message == opencodeapi.MatchAbsent {
		requestCtx, cancel = coordinator.cancellationContext(ctx)
		err = client.CancelInboxOnce(requestCtx, string(cancellation.Attempt.OpenCodeSessionID), string(cancellation.Attempt.OpenCodeMessageID))
		cancel()
		if err != nil {
			return ErrCancellationPending
		}
		requestCtx, cancel = coordinator.cancellationContext(ctx)
		observation, err = client.ReconcilePrompt(requestCtx, string(cancellation.Attempt.OpenCodeSessionID), coordinator.promptRequest(work))
		cancel()
		if err != nil || observation.Session != opencodeapi.MatchExact || observation.Inbox != opencodeapi.MatchAbsent || observation.Message != opencodeapi.MatchAbsent {
			return firstError(err, ErrCancellationPending)
		}
		return coordinator.acknowledgeCancellation(ctx, cancellation, promptEvidence("cancellation_inbox_removed", observation))
	}
	if observation.Inbox == opencodeapi.MatchAbsent {
		requestCtx, cancel = coordinator.cancellationContext(ctx)
		active, activeErr := client.ActiveSessions(requestCtx)
		cancel()
		if activeErr != nil {
			return activeErr
		}
		if _, running := active[string(cancellation.Attempt.OpenCodeSessionID)]; running {
			requestCtx, cancel = coordinator.cancellationContext(ctx)
			err = client.Interrupt(requestCtx, string(cancellation.Attempt.OpenCodeSessionID))
			cancel()
			if err != nil {
				return ErrCancellationPending
			}
			requestCtx, cancel = coordinator.cancellationContext(ctx)
			active, activeErr = client.ActiveSessions(requestCtx)
			cancel()
			if activeErr != nil {
				return activeErr
			}
			if _, stillRunning := active[string(cancellation.Attempt.OpenCodeSessionID)]; stillRunning {
				return ErrCancellationPending
			}
		}
		return coordinator.acknowledgeCancellation(ctx, cancellation, promptEvidence("cancellation_inactive", observation))
	}
	return ErrCancellationPending
}

func (coordinator *Coordinator) acknowledgeCancellation(ctx context.Context, cancellation taskstore.Cancellation, payload json.RawMessage) error {
	attemptEvent, taskEvent, err := coordinator.eventPair()
	if err != nil {
		return err
	}
	_, err = coordinator.store.AcknowledgeCancellation(ctx, taskstore.AcknowledgeCancellationParams{
		TaskID: cancellation.Task.ID, AttemptID: cancellation.Attempt.ID, CancelEpoch: cancellation.Task.CancelEpoch,
		ExpectedAttemptRevision: cancellation.Attempt.Revision, ExpectedTaskRevision: cancellation.Task.Revision,
		AttemptEventID: attemptEvent, TaskEventID: taskEvent, Now: coordinator.now(), Disposition: cancellation.Disposition,
		EvidencePayload: payload, EvidenceSHA256: sha256.Sum256(payload), Actor: coordinator.config.Actor,
	})
	return err
}

func (coordinator *Coordinator) reconcile(ctx context.Context, work taskstore.DeliveryWork) error {
	now := coordinator.now()
	if work.Attempt.State == task.AttemptDelivering {
		if work.Attempt.DeliveryClaimExpiresAt != nil && now.Before(*work.Attempt.DeliveryClaimExpiresAt) {
			return ErrLeaseActive
		}
		if work.Attempt.DeliveryClaimOwner == nil {
			return errors.New("delivering attempt has no claim owner")
		}
		attemptEvent, taskEvent, err := coordinator.eventPair()
		if err != nil {
			return err
		}
		payload := evidence("claim_expired", "exact", "unobserved", "unobserved")
		recovered, err := coordinator.store.RecoverExpiredDeliveryClaim(ctx, taskstore.RecoverExpiredDeliveryClaimParams{
			AttemptID: work.Attempt.ID, ExpiredLeaseOwner: *work.Attempt.DeliveryClaimOwner,
			ExpectedAttemptRevision: work.Attempt.Revision, RecoveryEventID: attemptEvent,
			TaskEventID: taskEvent, Now: now, Reason: "delivery_lease_expired",
			EvidencePayload: payload, EvidenceSHA256: sha256.Sum256(payload), Actor: coordinator.config.RecoveryActor,
		})
		if err != nil {
			return err
		}
		work = taskstore.DeliveryWork{Task: recovered.Task, Attempt: recovered.Attempt}
	}
	if work.Attempt.State != task.AttemptUncertain {
		return fmt.Errorf("unexpected ambiguous delivery state %s", work.Attempt.State)
	}

	target, release, client, err := coordinator.acquireClient(ctx, work.Attempt)
	if err != nil {
		if errors.Is(err, ErrImageConflict) {
			return coordinator.resolveRecovery(ctx, work, "runtime_image_conflict", evidence("runtime_attestation", "conflict", "unobserved", "unobserved"))
		}
		return err
	}
	defer release()
	_ = target

	sessionRequest := coordinator.sessionRequest(work)
	promptRequest := coordinator.promptRequest(work)
	switch work.Attempt.DeliveryPhase {
	case taskstore.DeliveryPhaseClaimed:
		return coordinator.resumeAndDeliver(ctx, work, evidence("pre_prompt_reconcile", "absent", "absent", "unobservable"), client)
	case taskstore.DeliveryPhaseSessionCreateStarted, taskstore.DeliveryPhaseSessionReady:
		requestCtx, cancel := coordinator.operationContext(ctx, work.Attempt.Deadline)
		match, readErr := client.ReconcileSession(requestCtx, sessionRequest)
		cancel()
		if readErr != nil {
			return readErr
		}
		payload := evidence("session_reconcile", string(match), "absent", "unobservable")
		if match != opencodeapi.MatchExact {
			return coordinator.resolveRecovery(ctx, work, "session_not_exact", payload)
		}
		return coordinator.resumeAndDeliver(ctx, work, payload, client)
	case taskstore.DeliveryPhasePromptStarted:
		requestCtx, cancel := coordinator.operationContext(ctx, work.Attempt.Deadline)
		observation, readErr := client.ReconcilePrompt(requestCtx, string(work.Attempt.OpenCodeSessionID), promptRequest)
		cancel()
		if readErr != nil {
			return readErr
		}
		payload := promptEvidence("prompt_reconcile", observation)
		if observation.Admitted() {
			return coordinator.resolveAdmitted(ctx, work, payload)
		}
		if promptObservationConflicts(observation) || !coordinator.now().Before(work.Attempt.Deadline) {
			return coordinator.resolveRecovery(ctx, work, "prompt_not_exact", payload)
		}
		return ErrDeliveryPending
	default:
		return coordinator.resolveRecovery(ctx, work, "invalid_delivery_phase", evidence("phase_reconcile", "conflict", "conflict", "conflict"))
	}
}

func (coordinator *Coordinator) resumeAndDeliver(ctx context.Context, work taskstore.DeliveryWork, payload json.RawMessage, client OpenCode) error {
	now := coordinator.now()
	attemptEvent, taskEvent, err := coordinator.eventPair()
	if err != nil {
		return err
	}
	resumed, err := coordinator.store.ResumeUncertainPrePromptDelivery(ctx, taskstore.ResumeUncertainPrePromptDeliveryParams{
		AttemptID: work.Attempt.ID, ExpectedAttemptRevision: work.Attempt.Revision,
		ExpectedTaskRevision: work.Task.Revision, ExpectedPhase: work.Attempt.DeliveryPhase,
		LeaseOwner: coordinator.config.WorkerID, LeaseExpiresAt: coordinator.leaseExpiry(now, work.Attempt.Deadline),
		AttemptEventID: attemptEvent, TaskEventID: taskEvent, Now: now,
		EvidencePayload: payload, EvidenceSHA256: sha256.Sum256(payload), Actor: coordinator.config.RecoveryActor,
	})
	if err != nil {
		return err
	}
	return coordinator.deliverWithClient(ctx, taskstore.DeliveryWork{Task: resumed.Task, Attempt: resumed.Attempt}, client)
}

func (coordinator *Coordinator) deliver(ctx context.Context, work taskstore.DeliveryWork) error {
	_, release, client, err := coordinator.acquireClient(ctx, work.Attempt)
	if err != nil {
		payload := evidence("runtime_attestation", deliveryStateForError(err), "unobserved", "unobserved")
		if errors.Is(err, ErrImageConflict) {
			return coordinator.recordActiveRecovery(ctx, work, "runtime_image_conflict", payload)
		}
		return coordinator.recordActiveUncertain(ctx, work, "workspace_wake_failed", payload)
	}
	defer release()
	return coordinator.deliverWithClient(ctx, work, client)
}

func (coordinator *Coordinator) deliverWithClient(ctx context.Context, work taskstore.DeliveryWork, client OpenCode) error {
	if work.Attempt.DeliveryPhase == taskstore.DeliveryPhaseClaimed {
		advanced, err := coordinator.advance(ctx, work, taskstore.DeliveryPhaseClaimed, taskstore.DeliveryPhaseSessionCreateStarted)
		if err != nil {
			return err
		}
		work = taskstore.DeliveryWork{Task: advanced.Task, Attempt: advanced.Attempt}
		requestCtx, cancel := coordinator.operationContext(ctx, work.Attempt.Deadline)
		_, effectErr := client.CreateOrReuseSession(requestCtx, coordinator.sessionRequest(work))
		cancel()
		if effectErr != nil {
			return coordinator.recordActiveUncertain(ctx, work, "session_create_result_unknown", evidence("session_create", deliveryStateForError(effectErr), "unobserved", "unobservable"))
		}
	}
	if work.Attempt.DeliveryPhase == taskstore.DeliveryPhaseSessionCreateStarted {
		advanced, err := coordinator.advance(ctx, work, taskstore.DeliveryPhaseSessionCreateStarted, taskstore.DeliveryPhaseSessionReady)
		if err != nil {
			return err
		}
		work = taskstore.DeliveryWork{Task: advanced.Task, Attempt: advanced.Attempt}
	}
	if work.Attempt.DeliveryPhase == taskstore.DeliveryPhaseSessionReady {
		advanced, err := coordinator.advance(ctx, work, taskstore.DeliveryPhaseSessionReady, taskstore.DeliveryPhasePromptStarted)
		if err != nil {
			return err
		}
		work = taskstore.DeliveryWork{Task: advanced.Task, Attempt: advanced.Attempt}
		requestCtx, cancel := coordinator.operationContext(ctx, work.Attempt.Deadline)
		_, effectErr := client.AdmitPrompt(requestCtx, string(work.Attempt.OpenCodeSessionID), coordinator.promptRequest(work))
		cancel()
		if effectErr != nil {
			return coordinator.recordActiveUncertain(ctx, work, "prompt_result_unknown", evidence("prompt_submit", "exact", deliveryStateForError(effectErr), "unobservable"))
		}
	}
	if work.Attempt.DeliveryPhase != taskstore.DeliveryPhasePromptStarted {
		return coordinator.recordActiveRecovery(ctx, work, "invalid_delivery_phase", evidence("delivery", "conflict", "conflict", "conflict"))
	}
	requestCtx, cancel := coordinator.operationContext(ctx, work.Attempt.Deadline)
	observation, err := client.ReconcilePrompt(requestCtx, string(work.Attempt.OpenCodeSessionID), coordinator.promptRequest(work))
	cancel()
	if err != nil {
		return coordinator.recordActiveUncertain(ctx, work, "prompt_reconcile_unavailable", evidence("prompt_reconcile", "exact", deliveryStateForError(err), "unobservable"))
	}
	payload := promptEvidence("prompt_reconcile", observation)
	if !observation.Admitted() {
		if promptObservationConflicts(observation) {
			return coordinator.recordActiveRecovery(ctx, work, "prompt_not_exact", payload)
		}
		return coordinator.recordActiveUncertain(ctx, work, "prompt_reconcile_inconclusive", payload)
	}
	return coordinator.recordAdmission(ctx, work, payload)
}

func (coordinator *Coordinator) advance(ctx context.Context, work taskstore.DeliveryWork, from, to taskstore.DeliveryPhase) (taskstore.DeliveryPhaseTransition, error) {
	eventID, err := coordinator.ids.EventID()
	if err != nil {
		return taskstore.DeliveryPhaseTransition{}, err
	}
	return coordinator.store.AdvanceDeliveryPhase(ctx, taskstore.AdvanceDeliveryPhaseParams{
		AttemptID: work.Attempt.ID, LeaseOwner: coordinator.config.WorkerID,
		ExpectedAttemptRevision: work.Attempt.Revision, From: from, To: to,
		EventID: eventID, Now: coordinator.now(), Actor: coordinator.config.Actor,
	})
}

func (coordinator *Coordinator) recordAdmission(ctx context.Context, work taskstore.DeliveryWork, payload json.RawMessage) error {
	attemptEvent, taskEvent, err := coordinator.eventPair()
	if err != nil {
		return err
	}
	_, err = coordinator.store.RecordAdmission(ctx, taskstore.RecordAdmissionParams{
		AttemptID: work.Attempt.ID, LeaseOwner: coordinator.config.WorkerID,
		ExpectedAttemptRevision: work.Attempt.Revision, AttemptEventID: attemptEvent,
		TaskEventID: taskEvent, Now: coordinator.now(), EvidencePayload: payload,
		EvidenceSHA256: sha256.Sum256(payload), Actor: coordinator.config.Actor,
	})
	return err
}

func (coordinator *Coordinator) recordActiveUncertain(ctx context.Context, work taskstore.DeliveryWork, reason string, payload json.RawMessage) error {
	attemptEvent, taskEvent, err := coordinator.eventPair()
	if err != nil {
		return err
	}
	_, err = coordinator.store.RecordDeliveryUncertain(ctx, taskstore.RecordDeliveryUncertainParams{
		AttemptID: work.Attempt.ID, LeaseOwner: coordinator.config.WorkerID,
		ExpectedAttemptRevision: work.Attempt.Revision, AttemptEventID: attemptEvent,
		TaskEventID: taskEvent, Now: coordinator.now(), Reason: reason,
		EvidencePayload: payload, EvidenceSHA256: sha256.Sum256(payload), Actor: coordinator.config.Actor,
	})
	return err
}

func (coordinator *Coordinator) recordActiveRecovery(ctx context.Context, work taskstore.DeliveryWork, reason string, payload json.RawMessage) error {
	attemptEvent, taskEvent, err := coordinator.eventPair()
	if err != nil {
		return err
	}
	_, err = coordinator.store.RecordDeliveryRecoveryRequired(ctx, taskstore.RecordDeliveryRecoveryRequiredParams{
		AttemptID: work.Attempt.ID, LeaseOwner: coordinator.config.WorkerID,
		ExpectedAttemptRevision: work.Attempt.Revision, AttemptEventID: attemptEvent,
		TaskEventID: taskEvent, Now: coordinator.now(), Reason: reason,
		EvidencePayload: payload, EvidenceSHA256: sha256.Sum256(payload), Actor: coordinator.config.Actor,
	})
	return err
}

func (coordinator *Coordinator) resolveAdmitted(ctx context.Context, work taskstore.DeliveryWork, payload json.RawMessage) error {
	return coordinator.resolve(ctx, work, taskstore.ResolveUncertainDeliveryAdmitted, "", payload)
}

func (coordinator *Coordinator) resolveRecovery(ctx context.Context, work taskstore.DeliveryWork, reason string, payload json.RawMessage) error {
	return coordinator.resolve(ctx, work, taskstore.ResolveUncertainDeliveryRecoveryRequired, reason, payload)
}

func (coordinator *Coordinator) resolve(ctx context.Context, work taskstore.DeliveryWork, outcome taskstore.ResolveUncertainDeliveryOutcome, reason string, payload json.RawMessage) error {
	attemptEvent, taskEvent, err := coordinator.eventPair()
	if err != nil {
		return err
	}
	_, err = coordinator.store.ResolveUncertainDelivery(ctx, taskstore.ResolveUncertainDeliveryParams{
		AttemptID: work.Attempt.ID, ExpectedAttemptRevision: work.Attempt.Revision,
		ExpectedTaskRevision: work.Task.Revision, AttemptEventID: attemptEvent,
		TaskEventID: taskEvent, Now: coordinator.now(), Outcome: outcome, Reason: reason,
		EvidencePayload: payload, EvidenceSHA256: sha256.Sum256(payload), Actor: coordinator.config.RecoveryActor,
	})
	return err
}

func (coordinator *Coordinator) acquireClient(ctx context.Context, attempt taskstore.Attempt) (workspace.RequestTarget, func(), OpenCode, error) {
	requestCtx, cancel := coordinator.operationContext(ctx, attempt.Deadline)
	return coordinator.acquireClientWithContext(requestCtx, cancel, attempt)
}

func (coordinator *Coordinator) acquireCancellationClient(ctx context.Context, attempt taskstore.Attempt) (workspace.RequestTarget, func(), OpenCode, error) {
	requestCtx, cancel := coordinator.cancellationContext(ctx)
	return coordinator.acquireClientWithContext(requestCtx, cancel, attempt)
}

func (coordinator *Coordinator) acquireClientWithContext(requestCtx context.Context, cancel context.CancelFunc, attempt taskstore.Attempt) (workspace.RequestTarget, func(), OpenCode, error) {
	target, release, err := coordinator.targets.AcquireRequest(requestCtx, workspace.RequestWork)
	cancel()
	if err != nil {
		return workspace.RequestTarget{}, func() {}, nil, err
	}
	if target.ImageID != attempt.ImageDigest {
		release()
		return workspace.RequestTarget{}, func() {}, nil, ErrImageConflict
	}
	client, err := coordinator.clients(target)
	if err != nil {
		release()
		return workspace.RequestTarget{}, func() {}, nil, err
	}
	return target, release, client, nil
}

func (coordinator *Coordinator) sessionRequest(work taskstore.DeliveryWork) opencodeapi.CreateSessionRequest {
	return opencodeapi.CreateSessionRequest{
		ID: string(work.Attempt.OpenCodeSessionID), Title: work.Task.Title, Agent: work.Attempt.Agent,
		Model:    &opencodeapi.Model{ProviderID: work.Attempt.ModelProvider, ID: work.Attempt.Model},
		Location: &opencodeapi.Location{Directory: coordinator.config.SessionDirectory},
	}
}

func (coordinator *Coordinator) promptRequest(work taskstore.DeliveryWork) opencodeapi.PromptRequest {
	resume := true
	return opencodeapi.PromptRequest{ID: string(work.Attempt.OpenCodeMessageID), Text: work.Task.Prompt, Resume: &resume}
}

func (coordinator *Coordinator) operationContext(parent context.Context, attemptDeadline time.Time) (context.Context, context.CancelFunc) {
	deadline := coordinator.config.Now().Add(coordinator.config.OperationTimeout)
	if attemptDeadline.Before(deadline) {
		deadline = attemptDeadline
	}
	return context.WithDeadline(parent, deadline)
}

func (coordinator *Coordinator) cancellationContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, coordinator.config.OperationTimeout)
}

func (coordinator *Coordinator) leaseExpiry(now, deadline time.Time) time.Time {
	expires := now.Add(coordinator.config.LeaseDuration)
	if deadline.Before(expires) {
		expires = deadline
	}
	return expires
}

func (coordinator *Coordinator) now() time.Time {
	return coordinator.config.Now().UTC().Truncate(time.Millisecond)
}

func (coordinator *Coordinator) eventPair() (task.EventID, task.EventID, error) {
	first, err := coordinator.ids.EventID()
	if err != nil {
		return "", "", err
	}
	second, err := coordinator.ids.EventID()
	if err != nil {
		return "", "", err
	}
	return first, second, nil
}

func evidence(stage, session, prompt, resume string) json.RawMessage {
	payload, _ := json.Marshal(struct {
		Stage   string `json:"stage"`
		Session string `json:"session"`
		Prompt  string `json:"messageState"`
		Resume  string `json:"resume"`
	}{stage, session, prompt, resume})
	return payload
}

func promptEvidence(stage string, observation opencodeapi.PromptObservation) json.RawMessage {
	return evidence(stage, string(observation.Session), string(observation.Inbox)+"/"+string(observation.Message), string(observation.Resume))
}

func promptObservationConflicts(observation opencodeapi.PromptObservation) bool {
	return observation.Session == opencodeapi.MatchConflict || observation.Inbox == opencodeapi.MatchConflict ||
		observation.Message == opencodeapi.MatchConflict || observation.Resume == opencodeapi.MatchConflict ||
		observation.Session == opencodeapi.MatchAbsent
}

// deliveryStateForError maps delivery-phase errors into the delivery evidence
// vocabulary, which distinguishes only "conflict" and "unavailable".
func deliveryStateForError(err error) string {
	if errors.Is(err, opencodeapi.ErrConflict) || errors.Is(err, opencodeapi.ErrProtocolConflict) || errors.Is(err, ErrImageConflict) {
		return "conflict"
	}
	return "unavailable"
}

func validASCII(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum {
		return false
	}
	for _, character := range []byte(value) {
		if character <= 0x20 || character >= 0x7f {
			return false
		}
	}
	return true
}

func firstError(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
