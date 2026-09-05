package runcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/backgroundruncoord"
	"github.com/nebler/fern/internal/runterminal"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	testWorkspace = task.WorkspaceID("wsp_0198d34d-6a50-75fb-b1f2-000000000001")
	testRun       = task.TaskID("tsk_0198d34d-6a50-75fb-b1f2-000000000201")
)

func TestRunControlReadAndDurableSteer(t *testing.T) {
	store := &fakeStore{run: taskstore.BackgroundRun{WorkspaceID: testWorkspace, TaskID: testRun, State: taskstore.BackgroundRunWorking,
		EffectPhase: taskstore.BackgroundRunEffectPromptAdmitted}, ownership: taskstore.BackgroundRunOwnership{
		WorkspaceID: testWorkspace, TaskID: testRun, Mode: taskstore.BackgroundRunAgentOwned,
		Phase: taskstore.BackgroundRunOwnershipAgentActive, WriterGeneration: 3, Revision: 7,
	}}
	controller := &fakeController{}
	handler := testHandler(t, store, controller, task.ActorSnapshot{Type: task.ActorDevice, ID: "device-1", DisplayName: "Phone",
		CredentialID: "device-1", Authentication: "paired_cookie", RequestID: "request-1"})

	read := httptest.NewRecorder()
	handler.ServeHTTP(read, httptest.NewRequest(http.MethodGet, APIPathPrefix+"/"+string(testRun), nil))
	if read.Code != http.StatusOK || strings.Contains(read.Body.String(), "container") || strings.Contains(read.Body.String(), "runtime") {
		t.Fatalf("read status=%d body=%s", read.Code, read.Body.String())
	}
	var got projection
	if err := json.Unmarshal(read.Body.Bytes(), &got); err != nil || got.Ownership != taskstore.BackgroundRunAgentOwned || got.WriterGeneration != 3 || got.Intervention != "local_idle" {
		t.Fatalf("projection=%+v error=%v", got, err)
	}

	request := httptest.NewRequest(http.MethodPost, APIPathPrefix+"/"+string(testRun)+"/steer", strings.NewReader(`{"instruction":"Read the latest tests and continue."}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "steer-key-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || store.admitted.Claim.Scope.CommandKind != taskstore.SteerBackgroundRunCommand ||
		store.admitted.Instruction != "Read the latest tests and continue." || store.admitted.OpenCodeMessageID == "" || controller.wakes != 1 {
		t.Fatalf("steer status=%d body=%s admission=%+v wakes=%d", response.Code, response.Body.String(), store.admitted, controller.wakes)
	}

	invalid := httptest.NewRequest(http.MethodPost, APIPathPrefix+"/"+string(testRun)+"/steer", strings.NewReader(`{"instruction":"   "}`))
	invalid.Header.Set("Content-Type", "application/json")
	invalid.Header.Set("Idempotency-Key", "steer-key-2")
	invalidResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid steer status=%d body=%s", invalidResponse.Code, invalidResponse.Body.String())
	}
}

func TestRunControlRejectsPluginActorAndCrossOriginTerminal(t *testing.T) {
	store := &fakeStore{run: taskstore.BackgroundRun{WorkspaceID: testWorkspace, TaskID: testRun}, ownership: taskstore.BackgroundRunOwnership{WorkspaceID: testWorkspace, TaskID: testRun}}
	plugin := testHandler(t, store, &fakeController{}, task.ActorSnapshot{Type: task.ActorOpenCode, ID: "plugin", DisplayName: "Plugin",
		CredentialID: "plugin", Authentication: "fern_plugin_bearer", RequestID: "request-2"})
	response := httptest.NewRecorder()
	plugin.ServeHTTP(response, httptest.NewRequest(http.MethodGet, APIPathPrefix, nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("plugin actor status=%d", response.Code)
	}

	device := testHandler(t, store, &fakeController{}, task.ActorSnapshot{Type: task.ActorDevice, ID: "device-1", DisplayName: "Phone",
		CredentialID: "device-1", Authentication: "paired_cookie", RequestID: "request-3"})
	terminal := httptest.NewRequest(http.MethodGet, APIPathPrefix+"/"+string(testRun)+"/terminal/human", nil)
	terminal.Header.Set("Upgrade", "websocket")
	terminal.Header.Set("Origin", "https://other.invalid")
	terminal.Header.Set("Sec-Fetch-Site", "cross-site")
	terminalResponse := httptest.NewRecorder()
	device.ServeHTTP(terminalResponse, terminal)
	if terminalResponse.Code != http.StatusBadRequest {
		t.Fatalf("cross-origin terminal status=%d", terminalResponse.Code)
	}
}

func testHandler(t *testing.T, store *fakeStore, controller *fakeController, actor task.ActorSnapshot) *Handler {
	t.Helper()
	terminal, err := runterminal.New(controller)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{WorkspaceID: testWorkspace, Store: store, Controller: controller, Generator: task.NewSecureGenerator(),
		Terminal:      terminal,
		ActorResolver: func(context.Context) (task.ActorSnapshot, error) { return actor, nil },
		Now:           func() time.Time { return time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

type fakeStore struct {
	run       taskstore.BackgroundRun
	ownership taskstore.BackgroundRunOwnership
	admitted  taskstore.AdmitBackgroundRunControlParams
}

func (s *fakeStore) GetBackgroundRunForControl(context.Context, task.WorkspaceID, task.TaskID, task.ActorSnapshot) (taskstore.BackgroundRun, taskstore.BackgroundRunOwnership, error) {
	if s.run.TaskID == "" {
		return taskstore.BackgroundRun{}, taskstore.BackgroundRunOwnership{}, taskstore.ErrNotFound
	}
	return s.run, s.ownership, nil
}

func (s *fakeStore) ListBackgroundRunsForControl(context.Context, task.WorkspaceID, task.ActorSnapshot, int) ([]taskstore.BackgroundRunControlView, error) {
	return []taskstore.BackgroundRunControlView{{Run: s.run, Ownership: s.ownership}}, nil
}

func (s *fakeStore) RequestBackgroundRunTakeover(context.Context, taskstore.RequestBackgroundRunTakeoverParams) (taskstore.BackgroundRunOwnershipAdmission, error) {
	return taskstore.BackgroundRunOwnershipAdmission{}, errors.New("not implemented")
}

func (s *fakeStore) RequestBackgroundRunHandback(context.Context, taskstore.RequestBackgroundRunHandbackParams) (taskstore.BackgroundRunOwnershipAdmission, error) {
	return taskstore.BackgroundRunOwnershipAdmission{}, errors.New("not implemented")
}

func (s *fakeStore) AdmitBackgroundRunControl(_ context.Context, value taskstore.AdmitBackgroundRunControlParams) (taskstore.BackgroundRunControlAdmission, error) {
	s.admitted = value
	return taskstore.BackgroundRunControlAdmission{Control: taskstore.BackgroundRunControl{ReceiptID: value.ReceiptID, State: taskstore.BackgroundRunControlRequested}}, nil
}

func (s *fakeStore) LatestBackgroundRunControl(context.Context, task.WorkspaceID, task.TaskID) (taskstore.BackgroundRunControl, error) {
	return taskstore.BackgroundRunControl{}, taskstore.ErrNotFound
}

type fakeController struct{ wakes int }

func (*fakeController) ObserveIntervention(context.Context, taskstore.BackgroundRun, taskstore.BackgroundRunOwnership) (backgroundruncoord.InterventionStatus, error) {
	return backgroundruncoord.InterventionStatus{State: "local_idle"}, nil
}

func (*fakeController) OpenTerminal(context.Context, taskstore.BackgroundRun, taskstore.BackgroundRunOwnership, string) (*taskenvdocker.Terminal, func(), error) {
	return nil, nil, errors.New("not implemented")
}

func (c *fakeController) Wake() { c.wakes++ }
