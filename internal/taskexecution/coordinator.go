// Package taskexecution projects only post-admission states that the pinned
// OpenCode read surfaces can prove without inferring terminal success.
package taskexecution

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/nebler/fern/internal/observability"
	"github.com/nebler/fern/internal/opencodeapi"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
	"github.com/nebler/fern/internal/workspace"
)

var (
	ErrNoWork          = errors.New("no task execution work is available")
	ErrObservationOpen = errors.New("task execution observation is not conclusive")
)

type Store interface {
	FindExecutionAttempt(context.Context, task.WorkspaceID) (taskstore.DeliveryWork, error)
	RecordExecutionProjection(context.Context, taskstore.RecordExecutionProjectionParams) (taskstore.ExecutionProjection, error)
	RequestCancellation(context.Context, taskstore.RequestCancellationParams) (taskstore.Cancellation, error)
}

type TargetAcquirer interface {
	AcquireRequest(context.Context, workspace.RequestIntent) (workspace.RequestTarget, func(), error)
	InvalidateEndpoint(workspace.RequestTarget)
}

type OpenCode interface {
	ReconcilePrompt(context.Context, string, opencodeapi.PromptRequest) (opencodeapi.PromptObservation, error)
	ReconcileSession(context.Context, opencodeapi.CreateSessionRequest) (opencodeapi.MatchState, error)
	ActiveSessions(context.Context) (opencodeapi.ActiveSessions, error)
	ListPermissions(context.Context, string) ([]opencodeapi.Permission, error)
	ListForms(context.Context, string) ([]opencodeapi.Form, error)
}

type ClientFactory func(workspace.RequestTarget) (OpenCode, error)

type Config struct {
	WorkspaceID        task.WorkspaceID
	SessionDirectory   string
	APIContractVersion string
	OperationTimeout   time.Duration
	PollInterval       time.Duration
	Actor              task.ActorSnapshot
	RecoveryActor      task.ActorSnapshot
	Now                func() time.Time
	OnError            func(error)
	OnSuccess          func()
}

type Coordinator struct {
	store   Store
	targets TargetAcquirer
	clients ClientFactory
	ids     *task.Generator
	config  Config
	runMu   sync.Mutex
}

func New(store Store, targets TargetAcquirer, clients ClientFactory, ids *task.Generator, config Config) (*Coordinator, error) {
	if store == nil || targets == nil || clients == nil || ids == nil {
		return nil, errors.New("task execution dependencies are required")
	}
	if _, err := task.ParseWorkspaceID(string(config.WorkspaceID)); err != nil {
		return nil, errors.New("valid task execution workspace is required")
	}
	if len(config.SessionDirectory) < 2 || len(config.SessionDirectory) > 4096 || !strings.HasPrefix(config.SessionDirectory, "/") || path.Clean(config.SessionDirectory) != config.SessionDirectory || strings.ContainsAny(config.SessionDirectory, "\x00\r\n") {
		return nil, errors.New("valid task execution session directory is required")
	}
	if len(config.APIContractVersion) < 1 || len(config.APIContractVersion) > 64 || strings.ContainsAny(config.APIContractVersion, "\x00\r\n") {
		return nil, errors.New("valid task execution API contract version is required")
	}
	if config.OperationTimeout <= 0 || config.OperationTimeout > time.Minute {
		return nil, errors.New("task execution operation timeout must be positive and at most one minute")
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
	if config.OnSuccess == nil {
		config.OnSuccess = func() {}
	}
	if err := config.Actor.Validate(); err != nil || config.Actor.Type != task.ActorSystem {
		return nil, errors.New("valid system execution actor is required")
	}
	if err := config.RecoveryActor.Validate(); err != nil || config.RecoveryActor.Type != task.ActorRecovery {
		return nil, errors.New("valid recovery execution actor is required")
	}
	return &Coordinator{store: store, targets: targets, clients: clients, ids: ids, config: config}, nil
}

func (coordinator *Coordinator) Run(ctx context.Context) error {
	retry := observability.NewRetry(coordinator.config.PollInterval, 30*time.Second)
	var delay time.Duration
	for {
		if err := observability.Wait(ctx, nil, delay); err != nil {
			return err
		}
		failed := false
		if err := coordinator.RunOnce(ctx); err != nil && !errors.Is(err, ErrNoWork) && !errors.Is(err, ErrObservationOpen) {
			if errors.Is(err, taskstore.ErrCorruptStore) {
				return err
			}
			coordinator.config.OnError(err)
			failed = true
		}
		if failed {
			delay = retry.Next()
		} else {
			coordinator.config.OnSuccess()
			retry.Reset()
			delay = coordinator.config.PollInterval
		}
	}
}

func (coordinator *Coordinator) RunOnce(ctx context.Context) error {
	coordinator.runMu.Lock()
	defer coordinator.runMu.Unlock()

	work, err := coordinator.store.FindExecutionAttempt(ctx, coordinator.config.WorkspaceID)
	if errors.Is(err, taskstore.ErrNotFound) {
		return ErrNoWork
	}
	if err != nil {
		return err
	}
	operationContext, cancel := context.WithTimeout(ctx, coordinator.config.OperationTimeout)
	defer cancel()
	target, release, err := coordinator.targets.AcquireRequest(operationContext, workspace.RequestRead)
	if err != nil {
		return err
	}
	defer release()
	if target.ImageID != work.Attempt.ImageDigest {
		return coordinator.record(operationContext, work, taskstore.ExecutionRecoveryRequired, "execution_image_conflict", observationEvidence{}, coordinator.config.RecoveryActor)
	}
	client, err := coordinator.clients(target)
	if err != nil {
		return err
	}
	observation, err := coordinator.observe(operationContext, client, work)
	if err != nil {
		if permanentObservationError(err) {
			return coordinator.record(operationContext, work, taskstore.ExecutionRecoveryRequired, "execution_protocol_invalid", observationEvidence{}, coordinator.config.RecoveryActor)
		}
		coordinator.targets.InvalidateEndpoint(target)
		return err
	}
	if observation.conflict {
		return coordinator.record(operationContext, work, taskstore.ExecutionRecoveryRequired, "execution_identity_conflict", observation, coordinator.config.RecoveryActor)
	}
	if observation.Permissions > 0 || observation.Forms > 0 {
		if work.Attempt.State == task.AttemptInputRequired {
			return ErrObservationOpen
		}
		return coordinator.record(operationContext, work, taskstore.ExecutionInputRequired, "", observation, coordinator.config.Actor)
	}
	if observation.Active {
		if work.Attempt.State == task.AttemptRunning {
			return ErrObservationOpen
		}
		return coordinator.record(operationContext, work, taskstore.ExecutionRunning, "", observation, coordinator.config.Actor)
	}
	if work.Attempt.State == task.AttemptAdmitted && observation.Promoted {
		return coordinator.record(operationContext, work, taskstore.ExecutionRunning, "", observation, coordinator.config.Actor)
	}
	now := coordinator.now()
	if !now.Before(work.Attempt.Deadline) {
		return coordinator.cancelExpired(operationContext, work, now)
	}
	return ErrObservationOpen
}

func (coordinator *Coordinator) cancelExpired(ctx context.Context, work taskstore.DeliveryWork, now time.Time) error {
	receiptID, err := coordinator.ids.ReceiptID()
	if err != nil {
		return err
	}
	attemptEventID, err := coordinator.ids.EventID()
	if err != nil {
		return err
	}
	taskEventID, err := coordinator.ids.EventID()
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte("task.execution.deadline\x00" + string(work.Task.ID) + "\x00" + work.Attempt.Deadline.UTC().Format(time.RFC3339Nano)))
	_, err = coordinator.store.RequestCancellation(ctx, taskstore.RequestCancellationParams{
		TaskID: work.Task.ID, ReceiptID: receiptID, AttemptEventID: attemptEventID, TaskEventID: taskEventID,
		Claim: task.IdempotencyClaim{
			Scope: task.IdempotencyScope{WorkspaceID: coordinator.config.WorkspaceID, CommandKind: taskstore.CancelTaskCommand},
			Key:   task.IdempotencyKey("deadline-" + string(work.Attempt.ID)), RequestHash: task.RequestHash(hash), Actor: coordinator.config.RecoveryActor,
		},
		Reason: "attempt deadline exceeded", Now: now, APIContractVersion: coordinator.config.APIContractVersion,
	})
	return err
}

func permanentObservationError(err error) bool {
	return errors.Is(err, opencodeapi.ErrProtocolConflict) || errors.Is(err, opencodeapi.ErrInvalidResponse) ||
		errors.Is(err, opencodeapi.ErrResponseTooLarge) || errors.Is(err, opencodeapi.ErrScanLimit) ||
		errors.Is(err, opencodeapi.ErrInvalidConfiguration) || errors.Is(err, opencodeapi.ErrRequestTooLarge)
}

type observationEvidence struct {
	Session     opencodeapi.MatchState `json:"session"`
	Inbox       opencodeapi.MatchState `json:"inbox"`
	Message     opencodeapi.MatchState `json:"message"`
	Resume      opencodeapi.MatchState `json:"resume"`
	Active      bool                   `json:"active"`
	Permissions int                    `json:"permissionCount"`
	Forms       int                    `json:"formCount"`
	Promoted    bool                   `json:"promoted"`
	conflict    bool
}

func (coordinator *Coordinator) observe(ctx context.Context, client OpenCode, work taskstore.DeliveryWork) (observationEvidence, error) {
	session, err := client.ReconcileSession(ctx, opencodeapi.CreateSessionRequest{
		ID: string(work.Attempt.OpenCodeSessionID), Title: work.Task.Title, Agent: work.Attempt.Agent,
		Model:    &opencodeapi.Model{ProviderID: work.Attempt.ModelProvider, ID: work.Attempt.Model},
		Location: &opencodeapi.Location{Directory: coordinator.config.SessionDirectory},
	})
	if err != nil {
		return observationEvidence{}, err
	}
	if session != opencodeapi.MatchExact {
		return observationEvidence{Session: session, conflict: true}, nil
	}
	resume := true
	prompt := opencodeapi.PromptRequest{ID: string(work.Attempt.OpenCodeMessageID), Text: work.Task.Prompt, Resume: &resume}
	projection, err := client.ReconcilePrompt(ctx, string(work.Attempt.OpenCodeSessionID), prompt)
	if err != nil {
		return observationEvidence{}, err
	}
	evidence := observationEvidence{
		Session: projection.Session, Inbox: projection.Inbox, Message: projection.Message, Resume: projection.Resume,
		Promoted: projection.Session == opencodeapi.MatchExact && projection.Inbox == opencodeapi.MatchAbsent && projection.Message == opencodeapi.MatchExact,
	}
	evidence.conflict = projection.Session == opencodeapi.MatchConflict || projection.Session == opencodeapi.MatchAbsent ||
		projection.Inbox == opencodeapi.MatchConflict || projection.Message == opencodeapi.MatchConflict || projection.Resume == opencodeapi.MatchConflict
	if evidence.conflict {
		return evidence, nil
	}
	active, err := client.ActiveSessions(ctx)
	if err != nil {
		return observationEvidence{}, err
	}
	_, evidence.Active = active[string(work.Attempt.OpenCodeSessionID)]
	permissions, err := client.ListPermissions(ctx, string(work.Attempt.OpenCodeSessionID))
	if err != nil {
		return observationEvidence{}, err
	}
	forms, err := client.ListForms(ctx, string(work.Attempt.OpenCodeSessionID))
	if err != nil {
		return observationEvidence{}, err
	}
	evidence.Permissions = len(permissions)
	evidence.Forms = len(forms)
	return evidence, nil
}

func (coordinator *Coordinator) record(ctx context.Context, work taskstore.DeliveryWork, outcome taskstore.ExecutionProjectionOutcome, reason string, evidence observationEvidence, actor task.ActorSnapshot) error {
	payload, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	attemptEventID, err := coordinator.ids.EventID()
	if err != nil {
		return err
	}
	taskEventID, err := coordinator.ids.EventID()
	if err != nil {
		return err
	}
	_, err = coordinator.store.RecordExecutionProjection(ctx, taskstore.RecordExecutionProjectionParams{
		TaskID: work.Task.ID, AttemptID: work.Attempt.ID,
		ExpectedAttemptRevision: work.Attempt.Revision, ExpectedTaskRevision: work.Task.Revision, ExpectedState: work.Attempt.State,
		OpenCodeSessionID: work.Attempt.OpenCodeSessionID, OpenCodeMessageID: work.Attempt.OpenCodeMessageID,
		Outcome: outcome, Reason: reason, AttemptEventID: attemptEventID, TaskEventID: taskEventID,
		ObservedAt: coordinator.now(), EvidencePayload: payload, EvidenceSHA256: sha256.Sum256(payload), Actor: actor,
	})
	return err
}

func (coordinator *Coordinator) now() time.Time {
	return coordinator.config.Now().UTC().Truncate(time.Millisecond)
}
