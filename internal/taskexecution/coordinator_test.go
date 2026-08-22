package taskexecution

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nebler/fern/internal/opencodeapi"
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
	"github.com/nebler/fern/internal/workspace"
)

const (
	testWorkspace = task.WorkspaceID("wsp_01890f8e-7b21-7000-8000-000000000001")
	testTask      = task.TaskID("tsk_01890f8e-7b21-7000-8000-000000000002")
	testAttempt   = task.AttemptID("att_01890f8e-7b21-7000-8000-000000000003")
	testSession   = task.OpenCodeSessionID("ses_0123456789abcdef0123456789abcdef")
	testMessage   = task.OpenCodeMessageID("msg_0123456789abcdef0123456789abcdef")
	testImage     = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

type fakeStore struct {
	work         taskstore.DeliveryWork
	findErr      error
	projection   *taskstore.RecordExecutionProjectionParams
	cancellation *taskstore.RequestCancellationParams
}

func (store *fakeStore) RequestCancellation(_ context.Context, params taskstore.RequestCancellationParams) (taskstore.Cancellation, error) {
	store.cancellation = &params
	return taskstore.Cancellation{}, nil
}

func (store *fakeStore) FindExecutionAttempt(context.Context, task.WorkspaceID) (taskstore.DeliveryWork, error) {
	return store.work, store.findErr
}

func (store *fakeStore) RecordExecutionProjection(_ context.Context, params taskstore.RecordExecutionProjectionParams) (taskstore.ExecutionProjection, error) {
	store.projection = &params
	return taskstore.ExecutionProjection{}, nil
}

type fakeTargets struct {
	target      workspace.RequestTarget
	invalidated int
}

func (targets *fakeTargets) AcquireRequest(context.Context, workspace.RequestIntent) (workspace.RequestTarget, func(), error) {
	return targets.target, func() {}, nil
}

func (targets *fakeTargets) InvalidateEndpoint(workspace.RequestTarget) { targets.invalidated++ }

type fakeClient struct {
	session     opencodeapi.MatchState
	sessionWant opencodeapi.CreateSessionRequest
	promptWant  opencodeapi.PromptRequest
	prompt      opencodeapi.PromptObservation
	promptErr   error
	active      opencodeapi.ActiveSessions
	permissions []opencodeapi.Permission
	forms       []opencodeapi.Form
}

func (client *fakeClient) ReconcileSession(_ context.Context, request opencodeapi.CreateSessionRequest) (opencodeapi.MatchState, error) {
	client.sessionWant = request
	if client.session == "" {
		return opencodeapi.MatchExact, nil
	}
	return client.session, nil
}

func (client *fakeClient) ReconcilePrompt(_ context.Context, _ string, request opencodeapi.PromptRequest) (opencodeapi.PromptObservation, error) {
	client.promptWant = request
	return client.prompt, client.promptErr
}

func (client *fakeClient) ActiveSessions(context.Context) (opencodeapi.ActiveSessions, error) {
	return client.active, nil
}

func (client *fakeClient) ListPermissions(context.Context, string) ([]opencodeapi.Permission, error) {
	return client.permissions, nil
}

func (client *fakeClient) ListForms(context.Context, string) ([]opencodeapi.Form, error) {
	return client.forms, nil
}

func TestExecutionObserverProjectsOnlyProvenStates(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		state     task.AttemptState
		deadline  time.Time
		client    fakeClient
		want      taskstore.ExecutionProjectionOutcome
		wantErr   error
		wantActor task.ActorType
	}{
		{name: "promoted caller message", state: task.AttemptAdmitted, deadline: now.Add(time.Hour), client: fakeClient{prompt: promotedPrompt()}, want: taskstore.ExecutionRunning, wantActor: task.ActorSystem},
		{name: "live form", state: task.AttemptRunning, deadline: now.Add(time.Hour), client: fakeClient{prompt: promotedPrompt(), forms: []opencodeapi.Form{{ID: "frm_one", SessionID: string(testSession)}}}, want: taskstore.ExecutionInputRequired, wantActor: task.ActorSystem},
		{name: "live permission", state: task.AttemptAdmitted, deadline: now.Add(time.Hour), client: fakeClient{prompt: promotedPrompt(), permissions: []opencodeapi.Permission{{ID: "per_one", SessionID: string(testSession)}}}, want: taskstore.ExecutionInputRequired, wantActor: task.ActorSystem},
		{name: "positive active resume", state: task.AttemptInputRequired, deadline: now.Add(time.Hour), client: fakeClient{prompt: promotedPrompt(), active: opencodeapi.ActiveSessions{string(testSession): {Type: "busy"}}}, want: taskstore.ExecutionRunning, wantActor: task.ActorSystem},
		{name: "inactive remains open", state: task.AttemptRunning, deadline: now.Add(time.Hour), client: fakeClient{prompt: promotedPrompt()}, wantErr: ErrObservationOpen},
		{name: "input disappearance remains open", state: task.AttemptInputRequired, deadline: now.Add(time.Hour), client: fakeClient{prompt: promotedPrompt()}, wantErr: ErrObservationOpen},
		{name: "identity conflict", state: task.AttemptRunning, deadline: now.Add(time.Hour), client: fakeClient{prompt: opencodeapi.PromptObservation{Session: opencodeapi.MatchExact, Inbox: opencodeapi.MatchConflict, Message: opencodeapi.MatchAbsent, Resume: opencodeapi.MatchUnobservable}}, want: taskstore.ExecutionRecoveryRequired, wantActor: task.ActorRecovery},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{work: testWork(test.state, test.deadline, now.Add(-time.Minute))}
			coordinator, _ := testCoordinator(t, store, &test.client, now, testImage)
			err := coordinator.RunOnce(context.Background())
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr != nil {
				if store.projection != nil {
					t.Fatalf("unexpected projection = %+v", store.projection)
				}
				return
			}
			if store.projection == nil || store.projection.Outcome != test.want || store.projection.Actor.Type != test.wantActor {
				t.Fatalf("projection = %+v", store.projection)
			}
			if len(store.projection.EvidencePayload) == 0 || store.projection.EvidenceSHA256 == ([32]byte{}) {
				t.Fatalf("missing bounded evidence = %+v", store.projection)
			}
		})
	}
}

func TestExecutionObserverDeadlineCommitsCancellationBeforeInterrupt(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{work: testWork(task.AttemptRunning, now, now.Add(-time.Minute))}
	coordinator, _ := testCoordinator(t, store, &fakeClient{prompt: promotedPrompt()}, now, testImage)
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.projection != nil || store.cancellation == nil {
		t.Fatalf("projection=%+v cancellation=%+v", store.projection, store.cancellation)
	}
	if store.cancellation.TaskID != testTask || store.cancellation.Reason != "attempt deadline exceeded" ||
		store.cancellation.Claim.Actor.Type != task.ActorRecovery || store.cancellation.Claim.Scope.CommandKind != taskstore.CancelTaskCommand ||
		store.cancellation.APIContractVersion != "fern.task.v1" {
		t.Fatalf("cancellation = %+v", store.cancellation)
	}
}

func TestExecutionObserverImageConflictFailsBeforeOpenCode(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{work: testWork(task.AttemptAdmitted, now.Add(time.Hour), now.Add(-time.Minute))}
	client := &fakeClient{promptErr: errors.New("must not be called")}
	coordinator, targets := testCoordinator(t, store, client, now, "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.projection == nil || store.projection.Outcome != taskstore.ExecutionRecoveryRequired || store.projection.Reason != "execution_image_conflict" || targets.invalidated != 0 {
		t.Fatalf("projection=%+v invalidations=%d", store.projection, targets.invalidated)
	}
}

func TestExecutionObserverReconcilesTheExactDeliveryTuple(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{work: testWork(task.AttemptAdmitted, now.Add(time.Hour), now.Add(-time.Minute))}
	client := &fakeClient{prompt: promotedPrompt()}
	coordinator, _ := testCoordinator(t, store, client, now, testImage)
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.sessionWant.ID != string(testSession) || client.sessionWant.Title != "Exact task" || client.sessionWant.Agent != "build" ||
		client.sessionWant.Model == nil || client.sessionWant.Model.ProviderID != "test" || client.sessionWant.Model.ID != "model" ||
		client.sessionWant.Location == nil || client.sessionWant.Location.Directory != "/home/user/workspace" {
		t.Fatalf("session request = %+v", client.sessionWant)
	}
	if client.promptWant.ID != string(testMessage) || client.promptWant.Text != "do the exact work" || client.promptWant.Resume == nil || !*client.promptWant.Resume {
		t.Fatalf("prompt request = %+v", client.promptWant)
	}
}

func TestExecutionObserverInvalidatesTransientReadFailure(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{work: testWork(task.AttemptAdmitted, now.Add(time.Hour), now.Add(-time.Minute))}
	wantErr := errors.New("temporary read failure")
	coordinator, targets := testCoordinator(t, store, &fakeClient{promptErr: wantErr}, now, testImage)
	if err := coordinator.RunOnce(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("error = %v", err)
	}
	if targets.invalidated != 1 || store.projection != nil {
		t.Fatalf("invalidations=%d projection=%+v", targets.invalidated, store.projection)
	}
}

func TestExecutionObserverFencesPermanentProtocolFailure(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{work: testWork(task.AttemptAdmitted, now.Add(time.Hour), now.Add(-time.Minute))}
	coordinator, targets := testCoordinator(t, store, &fakeClient{promptErr: opencodeapi.ErrProtocolConflict}, now, testImage)
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if targets.invalidated != 0 || store.projection == nil || store.projection.Outcome != taskstore.ExecutionRecoveryRequired || store.projection.Reason != "execution_protocol_invalid" {
		t.Fatalf("invalidations=%d projection=%+v", targets.invalidated, store.projection)
	}
}

func promotedPrompt() opencodeapi.PromptObservation {
	return opencodeapi.PromptObservation{Session: opencodeapi.MatchExact, Inbox: opencodeapi.MatchAbsent, Message: opencodeapi.MatchExact, Resume: opencodeapi.MatchUnobservable}
}

func testWork(state task.AttemptState, deadline, updated time.Time) taskstore.DeliveryWork {
	taskState := task.TaskRunning
	if state == task.AttemptInputRequired {
		taskState = task.TaskInputRequired
	}
	admitted := updated.Add(-time.Minute)
	return taskstore.DeliveryWork{
		Task: taskstore.Task{ID: testTask, WorkspaceID: testWorkspace, Title: "Exact task", Prompt: "do the exact work", State: taskState, CurrentAttemptID: testAttempt, Revision: 4, UpdatedAt: updated},
		Attempt: taskstore.Attempt{
			ID: testAttempt, TaskID: testTask, WorkspaceID: testWorkspace, State: state, DeliveryPhase: taskstore.DeliveryPhasePromptStarted,
			OpenCodeSessionID: testSession, OpenCodeMessageID: testMessage, ImageDigest: testImage,
			Agent: "build", ModelProvider: "test", Model: "model", Deadline: deadline, AdmittedAt: &admitted, Revision: 6, UpdatedAt: updated,
		},
	}
}

func testCoordinator(t *testing.T, store *fakeStore, client *fakeClient, now time.Time, image string) (*Coordinator, *fakeTargets) {
	t.Helper()
	ids, err := task.NewGenerator(bytes.NewReader(bytes.Repeat([]byte{0x42}, 128)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	targets := &fakeTargets{target: workspace.RequestTarget{Endpoint: runtime.Endpoint{Host: "127.0.0.1", Port: 4096}, ImageID: image, Generation: 1}}
	actor := task.ActorSnapshot{Type: task.ActorSystem, ID: "execution", DisplayName: "Execution coordinator", CredentialID: "service-v1", Authentication: "internal", RequestID: "worker-1"}
	recovery := actor
	recovery.Type = task.ActorRecovery
	recovery.ID = "recovery"
	coordinator, err := New(store, targets, func(workspace.RequestTarget) (OpenCode, error) { return client, nil }, ids, Config{
		WorkspaceID: testWorkspace, OperationTimeout: time.Second, PollInterval: time.Second,
		SessionDirectory: "/home/user/workspace", APIContractVersion: "fern.task.v1",
		Actor: actor, RecoveryActor: recovery, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return coordinator, targets
}
