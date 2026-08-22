package taskdelivery

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/opencodeapi"
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
	"github.com/nebler/fern/internal/workspace"
)

const testImageID = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type fakeTargets struct {
	target   workspace.RequestTarget
	err      error
	acquired int
	released int
}

func (targets *fakeTargets) AcquireRequest(context.Context, workspace.RequestIntent) (workspace.RequestTarget, func(), error) {
	targets.acquired++
	if targets.err != nil {
		return workspace.RequestTarget{}, nil, targets.err
	}
	return targets.target, func() { targets.released++ }, nil
}

func (*fakeTargets) InvalidateEndpoint(workspace.RequestTarget) {}

type fakeOpenCode struct {
	mu                sync.Mutex
	createCalls       int
	promptCalls       int
	createErr         error
	promptErr         error
	sessionMatch      opencodeapi.MatchState
	sessionErr        error
	promptObservation opencodeapi.PromptObservation
	reconcileErr      error
	active            opencodeapi.ActiveSessions
	activeErr         error
	deleteCalls       int
	deleteErr         error
	interruptCalls    int
	interruptErr      error
}

func (client *fakeOpenCode) CreateOrReuseSession(_ context.Context, request opencodeapi.CreateSessionRequest) (opencodeapi.Session, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.createCalls++
	if client.createErr != nil {
		return opencodeapi.Session{}, client.createErr
	}
	return opencodeapi.Session{ID: request.ID, Title: request.Title, Agent: request.Agent, Model: request.Model, Location: request.Location}, nil
}

func (client *fakeOpenCode) ReconcileSession(context.Context, opencodeapi.CreateSessionRequest) (opencodeapi.MatchState, error) {
	return client.sessionMatch, client.sessionErr
}

func (client *fakeOpenCode) AdmitPrompt(_ context.Context, _ string, request opencodeapi.PromptRequest) (opencodeapi.Admission, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.promptCalls++
	if request.Resume == nil || !*request.Resume {
		return opencodeapi.Admission{}, errors.New("resume was not true")
	}
	if client.promptErr != nil {
		return opencodeapi.Admission{}, client.promptErr
	}
	return opencodeapi.Admission{ID: request.ID}, nil
}

func (client *fakeOpenCode) ReconcilePrompt(context.Context, string, opencodeapi.PromptRequest) (opencodeapi.PromptObservation, error) {
	return client.promptObservation, client.reconcileErr
}

func (client *fakeOpenCode) CancelInboxOnce(context.Context, string, string) error {
	client.deleteCalls++
	if client.deleteErr == nil {
		client.promptObservation.Inbox = opencodeapi.MatchAbsent
		client.promptObservation.Resume = opencodeapi.MatchAbsent
	}
	return client.deleteErr
}

func (client *fakeOpenCode) ActiveSessions(context.Context) (opencodeapi.ActiveSessions, error) {
	return client.active, client.activeErr
}

func (client *fakeOpenCode) Interrupt(context.Context, string) error {
	client.interruptCalls++
	if client.interruptErr == nil {
		client.active = opencodeapi.ActiveSessions{}
	}
	return client.interruptErr
}

func TestRunOnceDeliversAndRecordsExactAdmission(t *testing.T) {
	store, coordinator, client, targets, admission, _ := newFixture(t)
	client.promptObservation = admittedObservation()
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.GetAttempt(context.Background(), admission.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.State != task.AttemptAdmitted || attempt.DeliveryPhase != taskstore.DeliveryPhasePromptStarted || client.createCalls != 1 || client.promptCalls != 1 {
		t.Fatalf("attempt=%+v create=%d prompt=%d", attempt, client.createCalls, client.promptCalls)
	}
	if targets.acquired != 1 || targets.released != 1 {
		t.Fatalf("target acquire/release = %d/%d", targets.acquired, targets.released)
	}
	if err := coordinator.RunOnce(context.Background()); !errors.Is(err, ErrNoWork) {
		t.Fatalf("second pass error = %v", err)
	}
}

func TestPromptLostResponseIsReconciledWithoutMutationRetry(t *testing.T) {
	store, coordinator, client, _, admission, _ := newFixture(t)
	client.promptErr = opencodeapi.ErrRequestFailed
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.GetAttempt(context.Background(), admission.Attempt.ID)
	if err != nil || attempt.State != task.AttemptUncertain || attempt.DeliveryPhase != taskstore.DeliveryPhasePromptStarted {
		t.Fatalf("uncertain attempt=%+v err=%v", attempt, err)
	}
	client.promptErr = nil
	client.promptObservation = admittedObservation()
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempt, _ = store.GetAttempt(context.Background(), admission.Attempt.ID)
	if attempt.State != task.AttemptAdmitted || client.promptCalls != 1 || client.createCalls != 1 {
		t.Fatalf("reconciled attempt=%+v create=%d prompt=%d", attempt, client.createCalls, client.promptCalls)
	}
}

func TestInconclusivePromptReadRemainsUncertainUntilProofOrDeadline(t *testing.T) {
	store, coordinator, client, _, admission, clock := newFixture(t)
	client.promptObservation = opencodeapi.PromptObservation{
		Session: opencodeapi.MatchExact, Inbox: opencodeapi.MatchAbsent,
		Message: opencodeapi.MatchAbsent, Resume: opencodeapi.MatchUnobservable,
	}
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempt, _ := store.GetAttempt(context.Background(), admission.Attempt.ID)
	if attempt.State != task.AttemptUncertain || attempt.DeliveryPhase != taskstore.DeliveryPhasePromptStarted {
		t.Fatalf("attempt = %+v", attempt)
	}
	if err := coordinator.RunOnce(context.Background()); !errors.Is(err, ErrDeliveryPending) {
		t.Fatalf("pending error = %v", err)
	}
	attempt, _ = store.GetAttempt(context.Background(), admission.Attempt.ID)
	if attempt.State != task.AttemptUncertain || client.promptCalls != 1 {
		t.Fatalf("pending attempt=%+v prompts=%d", attempt, client.promptCalls)
	}
	*clock = admission.Attempt.Deadline
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempt, _ = store.GetAttempt(context.Background(), admission.Attempt.ID)
	if attempt.State != task.AttemptRecoveryRequired || client.promptCalls != 1 {
		t.Fatalf("deadline attempt=%+v prompts=%d", attempt, client.promptCalls)
	}
}

func TestSessionLostResponseRequiresExactSessionBeforeResume(t *testing.T) {
	store, coordinator, client, _, admission, _ := newFixture(t)
	client.createErr = opencodeapi.ErrRequestFailed
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempt, _ := store.GetAttempt(context.Background(), admission.Attempt.ID)
	if attempt.State != task.AttemptUncertain || attempt.DeliveryPhase != taskstore.DeliveryPhaseSessionCreateStarted {
		t.Fatalf("uncertain attempt = %+v", attempt)
	}
	client.createErr = nil
	client.sessionMatch = opencodeapi.MatchAbsent
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempt, _ = store.GetAttempt(context.Background(), admission.Attempt.ID)
	if attempt.State != task.AttemptRecoveryRequired || client.createCalls != 1 || client.promptCalls != 0 {
		t.Fatalf("recovery attempt=%+v create=%d prompt=%d", attempt, client.createCalls, client.promptCalls)
	}
}

func TestSessionLostResponseExactSessionResumesWithoutCreateRetry(t *testing.T) {
	store, coordinator, client, _, admission, _ := newFixture(t)
	client.createErr = opencodeapi.ErrRequestFailed
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.createErr = nil
	client.sessionMatch = opencodeapi.MatchExact
	client.promptObservation = admittedObservation()
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempt, _ := store.GetAttempt(context.Background(), admission.Attempt.ID)
	if attempt.State != task.AttemptAdmitted || client.createCalls != 1 || client.promptCalls != 1 {
		t.Fatalf("attempt=%+v create=%d prompt=%d", attempt, client.createCalls, client.promptCalls)
	}
}

func TestImageConflictFailsClosedBeforeOpenCode(t *testing.T) {
	store, coordinator, client, targets, admission, _ := newFixture(t)
	targets.target.ImageID = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempt, _ := store.GetAttempt(context.Background(), admission.Attempt.ID)
	if attempt.State != task.AttemptRecoveryRequired || client.createCalls != 0 || client.promptCalls != 0 {
		t.Fatalf("attempt=%+v create=%d prompt=%d", attempt, client.createCalls, client.promptCalls)
	}
}

func TestExpiredPreparedAttemptNeverWakesWorkspace(t *testing.T) {
	store, coordinator, _, targets, admission, clock := newFixture(t)
	*clock = admission.Attempt.Deadline
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempt, _ := store.GetAttempt(context.Background(), admission.Attempt.ID)
	owner, _ := store.GetTask(context.Background(), admission.Task.ID)
	if attempt.State != task.AttemptFailed || owner.State != task.TaskFailed || targets.acquired != 0 {
		t.Fatalf("attempt=%s task=%s acquired=%d", attempt.State, owner.State, targets.acquired)
	}
}

func TestRunOnceLeavesLiveLeaseUntouched(t *testing.T) {
	_, coordinator, _, _, admission, clock := newFixture(t)
	claimEvent, taskEvent, err := coordinator.eventPair()
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.store.ClaimPreparedAttempt(context.Background(), taskstore.ClaimPreparedAttemptParams{
		AttemptID: admission.Attempt.ID, LeaseOwner: "other-worker", ClaimEventID: claimEvent,
		TaskEventID: taskEvent, Now: *clock, LeaseExpiresAt: clock.Add(time.Minute), Actor: coordinator.config.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.RunOnce(context.Background()); !errors.Is(err, ErrLeaseActive) {
		t.Fatalf("live lease error = %v", err)
	}
}

func TestPreparedCancellationClosesBeforeDelivery(t *testing.T) {
	store, coordinator, client, targets, admission, clock := newFixture(t)
	cancellation := requestFixtureCancellation(t, store, coordinator.ids, admission.Task.ID, "cancel-prepared", *clock)
	if cancellation.Disposition != taskstore.CancellationEffectNonePrepared {
		t.Fatalf("disposition = %s", cancellation.Disposition)
	}
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	owner, _ := store.GetTask(context.Background(), admission.Task.ID)
	attempt, _ := store.GetAttempt(context.Background(), admission.Attempt.ID)
	if owner.State != task.TaskCanceled || attempt.State != task.AttemptCanceled || attempt.CancellationAckAt == nil || targets.acquired != 0 || client.createCalls != 0 {
		t.Fatalf("task=%s attempt=%s ack=%v acquired=%d create=%d", owner.State, attempt.State, attempt.CancellationAckAt, targets.acquired, client.createCalls)
	}
}

func TestPromptInboxCancellationDeletesOnceAndProvesAbsence(t *testing.T) {
	store, coordinator, client, _, admission, clock := newFixture(t)
	client.promptErr = opencodeapi.ErrRequestFailed
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempt, _ := store.GetAttempt(context.Background(), admission.Attempt.ID)
	if attempt.State != task.AttemptUncertain {
		t.Fatalf("attempt = %s", attempt.State)
	}
	cancellation := requestFixtureCancellation(t, store, coordinator.ids, admission.Task.ID, "cancel-inbox", *clock)
	if cancellation.Disposition != taskstore.CancellationEffectInterrupt {
		t.Fatalf("disposition = %s", cancellation.Disposition)
	}
	client.promptObservation = admittedObservation()
	client.promptErr = nil
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempt, _ = store.GetAttempt(context.Background(), admission.Attempt.ID)
	if attempt.State != task.AttemptCanceled || client.deleteCalls != 1 || client.interruptCalls != 0 {
		t.Fatalf("attempt=%s deletes=%d interrupts=%d", attempt.State, client.deleteCalls, client.interruptCalls)
	}
}

func TestExecutingCancellationInterruptsExactActiveSession(t *testing.T) {
	store, coordinator, client, _, admission, clock := newFixture(t)
	client.promptObservation = opencodeapi.PromptObservation{Session: opencodeapi.MatchExact, Inbox: opencodeapi.MatchAbsent, Message: opencodeapi.MatchExact, Resume: opencodeapi.MatchUnobservable}
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancellation := requestFixtureCancellation(t, store, coordinator.ids, admission.Task.ID, "cancel-running", *clock)
	client.active = opencodeapi.ActiveSessions{string(admission.Attempt.OpenCodeSessionID): {Type: "busy"}}
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempt, _ := store.GetAttempt(context.Background(), admission.Attempt.ID)
	if cancellation.Disposition != taskstore.CancellationEffectInterrupt || attempt.State != task.AttemptCanceled || client.interruptCalls != 1 || client.deleteCalls != 0 {
		t.Fatalf("disposition=%s attempt=%s interrupts=%d deletes=%d", cancellation.Disposition, attempt.State, client.interruptCalls, client.deleteCalls)
	}
}

func TestAmbiguousCancellationRemainsPending(t *testing.T) {
	store, coordinator, client, _, admission, clock := newFixture(t)
	client.promptObservation = admittedObservation()
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	requestFixtureCancellation(t, store, coordinator.ids, admission.Task.ID, "cancel-pending", *clock)
	client.reconcileErr = opencodeapi.ErrRequestFailed
	if err := coordinator.RunOnce(context.Background()); !errors.Is(err, opencodeapi.ErrRequestFailed) {
		t.Fatalf("pending error = %v", err)
	}
	owner, _ := store.GetTask(context.Background(), admission.Task.ID)
	if owner.State != task.TaskCancelRequested {
		t.Fatalf("task state = %s", owner.State)
	}
}

func TestNewRejectsUnsafeConfiguration(t *testing.T) {
	store, coordinator, _, targets, _, _ := newFixture(t)
	config := coordinator.config
	config.WorkerID = "worker\nunsafe"
	if _, err := New(store, targets, func(workspace.RequestTarget) (OpenCode, error) { return &fakeOpenCode{}, nil }, task.NewSecureGenerator(), config); err == nil {
		t.Fatal("unsafe worker accepted")
	}
	config = coordinator.config
	config.SessionDirectory = "/home/user/../escape"
	if _, err := New(store, targets, func(workspace.RequestTarget) (OpenCode, error) { return &fakeOpenCode{}, nil }, task.NewSecureGenerator(), config); err == nil {
		t.Fatal("unclean session directory accepted")
	}
}

func TestLocalClientFactoryRequiresAuthenticationAndBuildsLoopbackClient(t *testing.T) {
	t.Parallel()
	if _, err := LocalClientFactory(runtime.ServerAuth{}); err == nil {
		t.Fatal("empty authentication accepted")
	}
	factory, err := LocalClientFactory(runtime.ServerAuth{Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := factory(workspace.RequestTarget{Endpoint: runtime.Endpoint{Host: "127.0.0.1", Port: 4096}})
	if err != nil || client == nil {
		t.Fatalf("client=%v err=%v", client, err)
	}
}

func admittedObservation() opencodeapi.PromptObservation {
	return opencodeapi.PromptObservation{
		Session: opencodeapi.MatchExact, Inbox: opencodeapi.MatchExact,
		Message: opencodeapi.MatchAbsent, Resume: opencodeapi.MatchUnobservable,
	}
}

func requestFixtureCancellation(t *testing.T, store *taskstore.Store, ids *task.Generator, taskID task.TaskID, key string, now time.Time) taskstore.Cancellation {
	t.Helper()
	receiptID, err := ids.ReceiptID()
	if err != nil {
		t.Fatal(err)
	}
	attemptEvent, err := ids.EventID()
	if err != nil {
		t.Fatal(err)
	}
	taskEvent, err := ids.EventID()
	if err != nil {
		t.Fatal(err)
	}
	hash := task.RequestHash(sha256.Sum256([]byte(key)))
	actor := task.ActorSnapshot{Type: task.ActorDevice, ID: "device-1", CredentialID: "grant-1", Authentication: "device-cookie", RequestID: "cancel-request"}
	result, err := store.RequestCancellation(context.Background(), taskstore.RequestCancellationParams{
		TaskID: taskID, ReceiptID: receiptID, AttemptEventID: attemptEvent, TaskEventID: taskEvent,
		Claim:  task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: testWorkspaceIDFromTask(t, store, taskID), CommandKind: taskstore.CancelTaskCommand}, Key: task.IdempotencyKey(key), RequestHash: hash, Actor: actor},
		Reason: "stop", Now: now.UTC().Truncate(time.Millisecond), APIContractVersion: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func testWorkspaceIDFromTask(t *testing.T, store *taskstore.Store, taskID task.TaskID) task.WorkspaceID {
	t.Helper()
	owner, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	return owner.WorkspaceID
}

func newFixture(t *testing.T) (*taskstore.Store, *Coordinator, *fakeOpenCode, *fakeTargets, taskstore.Admission, *time.Time) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := taskstore.Open(context.Background(), filepath.Join(directory, "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ids := task.NewSecureGenerator()
	workspaceID, err := ids.WorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	if err := store.CreateWorkspace(context.Background(), taskstore.Workspace{
		ID: workspaceID, Name: "demo", State: taskstore.WorkspaceActive,
		RepositoryPath: "/repo", InstallationID: 1, RepositoryID: 10,
		RepositoryFullName: "owner/repo", ImageDigest: testImageID,
		OpenCodeProtocol: "0.0.0-next-17444", RuntimeDesiredState: "running", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	admissionIDs, err := ids.GenerateAdmissionIDs()
	if err != nil {
		t.Fatal(err)
	}
	actor := task.ActorSnapshot{Type: task.ActorDevice, ID: "device-1", CredentialID: "grant-1", Authentication: "device-cookie", RequestID: "request-1"}
	requestHash := task.RequestHash(sha256.Sum256([]byte(`{"prompt":"change it"}`)))
	admission, err := store.AdmitTask(context.Background(), taskstore.AdmitTaskParams{
		TaskID: admissionIDs.TaskID, AttemptID: admissionIDs.AttemptID, ReceiptID: admissionIDs.ReceiptID,
		TaskEventID: admissionIDs.TaskEventID, AttemptEventID: admissionIDs.AttemptEventID,
		OpenCodeSessionID: admissionIDs.OpenCodeSessionID, OpenCodeMessageID: admissionIDs.OpenCodeMessageID,
		Claim: task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: workspaceID, CommandKind: taskstore.SubmitTaskCommand}, Key: "submit-1", RequestHash: requestHash, Actor: actor},
		Title: "Change it", Prompt: "change it", RepositoryID: 10, BaseRef: "main",
		BaseSHA: "0123456789abcdef0123456789abcdef01234567", ObjectFormat: "sha1",
		ExecutionContractVersion: "exec-v1", Agent: "build", ModelProvider: "test", Model: "test-model",
		BudgetSnapshot: []byte(`{"turns":10}`), Deadline: now.Add(10 * time.Minute), APIContractVersion: "v1", AcceptedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeOpenCode{sessionMatch: opencodeapi.MatchExact}
	targets := &fakeTargets{target: workspace.RequestTarget{Endpoint: runtime.Endpoint{Host: "127.0.0.1", Port: 4096}, ImageID: testImageID, Generation: 1}}
	systemActor := task.ActorSnapshot{Type: task.ActorSystem, ID: "delivery", CredentialID: "process", Authentication: "internal", RequestID: "worker"}
	recoveryActor := task.ActorSnapshot{Type: task.ActorRecovery, ID: "delivery-recovery", CredentialID: "process", Authentication: "internal", RequestID: "worker"}
	coordinator, err := New(store, targets, func(workspace.RequestTarget) (OpenCode, error) { return client, nil }, ids, Config{
		WorkspaceID: workspaceID, WorkerID: "worker-1", SessionDirectory: "/home/user/workspace",
		LeaseDuration: 2 * time.Minute, OperationTimeout: 10 * time.Second, PollInterval: time.Second,
		Actor: systemActor, RecoveryActor: recoveryActor, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, coordinator, client, targets, admission, &now
}
