package taskapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskresultcoord"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	testWorkspace = task.WorkspaceID("wsp_0198d34d-5e40-7c5a-8e3f-6bfad471ae12")
	testTask      = task.TaskID("tsk_0198d34d-6a50-75fb-b1f2-b4a14d70ec55")
	testAttempt   = task.AttemptID("att_0198d34d-6a50-75fb-b1f2-b4a14d70ec56")
	testReceipt   = task.ReceiptID("rcp_0198d34d-6a50-75fb-b1f2-b4a14d70ec57")
	testEvent     = task.EventID("fev_0198d34d-6a50-75fb-b1f2-b4a14d70ec58")
	testSHA       = task.GitOID("0123456789abcdef0123456789abcdef01234567")
)

var testNow = time.Date(2026, 8, 22, 18, 57, 11, 565000000, time.UTC)

type fakeStore struct {
	admit    func(context.Context, taskstore.AdmitTaskParams) (taskstore.Admission, error)
	receipt  func(context.Context, task.WorkspaceID, string, task.IdempotencyKey) (taskstore.Receipt, bool, error)
	get      func(context.Context, task.TaskID) (taskstore.Task, error)
	attempt  func(context.Context, task.AttemptID) (taskstore.Attempt, error)
	snapshot func(context.Context, task.WorkspaceID, task.TaskID) (taskstore.TaskSnapshot, error)
	list     func(context.Context, task.WorkspaceID, int) ([]taskstore.TaskSnapshot, error)
	cancel   func(context.Context, taskstore.RequestCancellationParams) (taskstore.Cancellation, error)
	seal     func(context.Context, task.ReceiptID) (taskstore.SealRequest, error)
	events   func(context.Context, task.WorkspaceID, task.Cursor, int) (taskstore.EventPage, error)
}

func (s *fakeStore) ListTasks(ctx context.Context, workspace task.WorkspaceID, limit int) ([]taskstore.TaskSnapshot, error) {
	if s.list == nil {
		return []taskstore.TaskSnapshot{}, nil
	}
	return s.list(ctx, workspace, limit)
}

func (s *fakeStore) GetTaskSnapshot(ctx context.Context, workspace task.WorkspaceID, id task.TaskID) (taskstore.TaskSnapshot, error) {
	if s.snapshot != nil {
		return s.snapshot(ctx, workspace, id)
	}
	owner, err := s.GetTask(ctx, id)
	if err != nil {
		return taskstore.TaskSnapshot{}, err
	}
	if owner.WorkspaceID != workspace {
		return taskstore.TaskSnapshot{}, taskstore.ErrNotFound
	}
	attempt, err := s.GetAttempt(ctx, owner.CurrentAttemptID)
	return taskstore.TaskSnapshot{Task: owner, Attempt: attempt, Verifications: []taskstore.Verification{}}, err
}

type fakeSealAuthorizer struct {
	preview func(context.Context, task.TaskID) (taskresultcoord.SealSnapshot, error)
	request func(context.Context, taskresultcoord.SealSnapshot, taskstore.RequestSealParams) (taskstore.SealAdmission, error)
}

func (a *fakeSealAuthorizer) Preview(ctx context.Context, id task.TaskID) (taskresultcoord.SealSnapshot, error) {
	return a.preview(ctx, id)
}

func (a *fakeSealAuthorizer) Request(ctx context.Context, expected taskresultcoord.SealSnapshot, params taskstore.RequestSealParams) (taskstore.SealAdmission, error) {
	return a.request(ctx, expected, params)
}

func (s *fakeStore) GetSealRequestByReceipt(ctx context.Context, id task.ReceiptID) (taskstore.SealRequest, error) {
	if s.seal == nil {
		return taskstore.SealRequest{}, errors.New("unexpected GetSealRequestByReceipt")
	}
	return s.seal(ctx, id)
}

func (s *fakeStore) FindReceiptByIdempotency(ctx context.Context, workspace task.WorkspaceID, kind string, key task.IdempotencyKey) (taskstore.Receipt, bool, error) {
	if s.receipt == nil {
		return taskstore.Receipt{}, false, nil
	}
	return s.receipt(ctx, workspace, kind, key)
}

func (s *fakeStore) AdmitTask(ctx context.Context, p taskstore.AdmitTaskParams) (taskstore.Admission, error) {
	if s.admit == nil {
		return taskstore.Admission{}, errors.New("unexpected AdmitTask")
	}
	return s.admit(ctx, p)
}

func (s *fakeStore) GetTask(ctx context.Context, id task.TaskID) (taskstore.Task, error) {
	if s.get == nil {
		return taskstore.Task{}, errors.New("unexpected GetTask")
	}
	return s.get(ctx, id)
}

func (s *fakeStore) GetAttempt(ctx context.Context, id task.AttemptID) (taskstore.Attempt, error) {
	if s.attempt == nil {
		return taskstore.Attempt{}, errors.New("unexpected GetAttempt")
	}
	return s.attempt(ctx, id)
}

func (s *fakeStore) RequestCancellation(ctx context.Context, p taskstore.RequestCancellationParams) (taskstore.Cancellation, error) {
	if s.cancel == nil {
		return taskstore.Cancellation{}, errors.New("unexpected RequestCancellation")
	}
	return s.cancel(ctx, p)
}

func (s *fakeStore) ListEvents(ctx context.Context, workspace task.WorkspaceID, after task.Cursor, limit int) (taskstore.EventPage, error) {
	if s.events == nil {
		return taskstore.EventPage{}, errors.New("unexpected ListEvents")
	}
	return s.events(ctx, workspace, after, limit)
}

func testActor() task.ActorSnapshot {
	return task.ActorSnapshot{
		Type: task.ActorOperator, ID: "operator", DisplayName: "Operator", CredentialID: "credential-v1",
		Authentication: "basic", RequestID: "request-1",
	}
}

func testConfig(t *testing.T, store Store) Config {
	t.Helper()
	ids, err := task.NewGenerator(bytes.NewReader(bytes.Repeat([]byte{0xa5}, 64*1024)), func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		WorkspaceID: testWorkspace, RepositoryID: 987654321, Store: store, Generator: ids,
		ActorResolver: func(context.Context) (task.ActorSnapshot, error) { return testActor(), nil },
		BaseResolver:  func(context.Context, string) (task.GitOID, error) { return testSHA, nil },
		Wake:          func() {}, Now: func() time.Time { return testNow }, AttemptTimeout: 30 * time.Minute,
		ObjectFormat: "sha1", APIContractVersion: "fern.task.v1", ExecutionContractVersion: "execution.v1",
		Agent: "build", ModelProvider: "test", Model: "test-model", BudgetSnapshot: json.RawMessage(`{"turns":10}`),
	}
}

func newTestHandler(t *testing.T, config Config) *Handler {
	t.Helper()
	handler, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func request(handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func postHeaders() map[string]string {
	return map[string]string{"Content-Type": "application/json", "Idempotency-Key": "command-1"}
}

func acceptedAdmission(p taskstore.AdmitTaskParams) taskstore.Admission {
	projection, _ := json.Marshal(struct {
		ReceiptID task.ReceiptID `json:"receiptId"`
		TaskID    task.TaskID    `json:"taskId"`
		AttemptID task.AttemptID `json:"attemptId"`
	}{p.ReceiptID, p.TaskID, p.AttemptID})
	return taskstore.Admission{
		Task: taskstore.Task{
			ID: p.TaskID, WorkspaceID: p.Claim.Scope.WorkspaceID, Title: p.Title, Prompt: p.Prompt,
			RepositoryID: p.RepositoryID, BaseRef: p.BaseRef, BaseSHA: p.BaseSHA, State: task.TaskQueued,
			CurrentAttemptID: p.AttemptID, LatestEventCursor: 2, Revision: 1, CreatedAt: p.AcceptedAt, UpdatedAt: p.AcceptedAt,
		},
		Attempt: taskstore.Attempt{ID: p.AttemptID, TaskID: p.TaskID, WorkspaceID: p.Claim.Scope.WorkspaceID, Sequence: 1, State: task.AttemptPrepared, Deadline: p.Deadline},
		Receipt: taskstore.Receipt{
			ID: p.ReceiptID, WorkspaceID: p.Claim.Scope.WorkspaceID, CommandKind: taskstore.SubmitTaskCommand,
			State: taskstore.ReceiptAccepted, AcceptedAt: p.AcceptedAt, TargetID: p.TaskID, ResponseProjection: projection,
		},
		AttemptEvent: taskstore.Event{Cursor: 2},
	}
}

func acceptedCancellation(p taskstore.RequestCancellationParams) taskstore.Cancellation {
	projection, _ := json.Marshal(struct {
		AttemptID   task.AttemptID                          `json:"attemptId"`
		CancelEpoch uint64                                  `json:"cancelEpoch"`
		Disposition taskstore.CancellationEffectDisposition `json:"effectDisposition"`
	}{testAttempt, 1, taskstore.CancellationEffectInterrupt})
	return taskstore.Cancellation{
		Task:    taskstore.Task{ID: p.TaskID, WorkspaceID: p.Claim.Scope.WorkspaceID, CancelEpoch: 1},
		Attempt: taskstore.Attempt{ID: testAttempt, TaskID: p.TaskID, WorkspaceID: p.Claim.Scope.WorkspaceID},
		Receipt: taskstore.Receipt{
			ID: p.ReceiptID, WorkspaceID: p.Claim.Scope.WorkspaceID, CommandKind: taskstore.CancelTaskCommand,
			State: taskstore.ReceiptAccepted, AcceptedAt: p.Now, TargetID: p.TaskID, ResponseProjection: projection,
		},
		Disposition: taskstore.CancellationEffectInterrupt,
	}
}

func TestAuthenticationIsRequiredBeforeRouting(t *testing.T) {
	config := testConfig(t, &fakeStore{})
	config.ActorResolver = func(context.Context) (task.ActorSnapshot, error) { return task.ActorSnapshot{}, ErrUnauthenticated }
	handler := newTestHandler(t, config)

	for _, path := range []string{submitPath, eventsPath, apiPrefix + string(testTask), "/not-an-api-route"} {
		response := request(handler, http.MethodGet, path, "", nil)
		if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), "credential") {
			t.Fatalf("path %s: status/body = %d %s", path, response.Code, response.Body.String())
		}
	}
}

func TestRoutesAndMethods(t *testing.T) {
	handler := newTestHandler(t, testConfig(t, &fakeStore{}))
	tests := []struct {
		method, path, allow string
		status              int
	}{
		{http.MethodDelete, submitPath, "GET, POST", http.StatusMethodNotAllowed},
		{http.MethodPost, eventsPath, http.MethodGet, http.StatusMethodNotAllowed},
		{http.MethodPost, apiPrefix + string(testTask), http.MethodGet, http.StatusMethodNotAllowed},
		{http.MethodGet, apiPrefix + string(testTask) + "/cancel", http.MethodPost, http.StatusMethodNotAllowed},
		{http.MethodGet, submitPath + "/", "", http.StatusNotFound},
		{http.MethodGet, apiPrefix + "bad", "", http.StatusNotFound},
		{http.MethodGet, "/fern/api/v1/unknown", "", http.StatusNotFound},
	}
	for _, test := range tests {
		response := request(handler, test.method, test.path, "", nil)
		if response.Code != test.status || response.Header().Get("Allow") != test.allow {
			t.Errorf("%s %s: status/allow = %d %q", test.method, test.path, response.Code, response.Header().Get("Allow"))
		}
	}
}

func TestSubmitStrictJSONAndHeaders(t *testing.T) {
	var calls atomic.Int64
	store := &fakeStore{admit: func(_ context.Context, p taskstore.AdmitTaskParams) (taskstore.Admission, error) {
		calls.Add(1)
		return acceptedAdmission(p), nil
	}}
	handler := newTestHandler(t, testConfig(t, store))
	tests := []struct {
		name, body string
		headers    map[string]string
	}{
		{"missing key", `{"title":"t","prompt":"p","baseRef":"main"}`, map[string]string{"Content-Type": "application/json"}},
		{"wrong content type", `{"title":"t","prompt":"p","baseRef":"main"}`, map[string]string{"Content-Type": "application/json; charset=utf-8", "Idempotency-Key": "x"}},
		{"unknown", `{"title":"t","prompt":"p","baseRef":"main","agent":"x"}`, postHeaders()},
		{"duplicate", `{"title":"t","title":"u","prompt":"p","baseRef":"main"}`, postHeaders()},
		{"case alias", `{"Title":"t","prompt":"p","baseRef":"main"}`, postHeaders()},
		{"trailing", `{"title":"t","prompt":"p","baseRef":"main"}{}`, postHeaders()},
		{"wrong type", `{"title":1,"prompt":"p","baseRef":"main"}`, postHeaders()},
		{"unpaired surrogate", `{"title":"\ud800","prompt":"p","baseRef":"main"}`, postHeaders()},
		{"missing", `{"title":"t","prompt":"p"}`, postHeaders()},
		{"query", `{"title":"t","prompt":"p","baseRef":"main"}`, postHeaders()},
	}
	for _, test := range tests {
		path := submitPath
		if test.name == "query" {
			path += "?extra=1"
		}
		response := request(handler, http.MethodPost, path, test.body, test.headers)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status/body = %d %s", test.name, response.Code, response.Body.String())
		}
	}
	invalidUTF8 := string([]byte{'{', '"', 't', 'i', 't', 'l', 'e', '"', ':', '"', 0xff, '"', '}'})
	if response := request(handler, http.MethodPost, submitPath, invalidUTF8, postHeaders()); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid UTF-8 status = %d", response.Code)
	}
	if calls.Load() != 0 {
		t.Fatalf("store called %d times for rejected requests", calls.Load())
	}
}

func TestSubmitAllowsEscapedMaximumPromptAndRejectsOversizeBody(t *testing.T) {
	var calls atomic.Int64
	store := &fakeStore{admit: func(_ context.Context, p taskstore.AdmitTaskParams) (taskstore.Admission, error) {
		calls.Add(1)
		if len(p.Prompt) != maxPromptBytes {
			t.Fatalf("prompt length = %d", len(p.Prompt))
		}
		return acceptedAdmission(p), nil
	}}
	handler := newTestHandler(t, testConfig(t, store))
	body, _ := json.Marshal(map[string]string{"title": "t", "prompt": strings.Repeat("\x01", maxPromptBytes), "baseRef": "main"})
	if response := request(handler, http.MethodPost, submitPath, string(body), postHeaders()); response.Code != http.StatusAccepted {
		t.Fatalf("maximum escaped prompt: %d %s", response.Code, response.Body.String())
	}
	oversize := `{"title":"t","prompt":"p","baseRef":"main"}` + strings.Repeat(" ", maxSubmitBodyBytes)
	if response := request(handler, http.MethodPost, submitPath, oversize, postHeaders()); response.Code != http.StatusBadRequest {
		t.Fatalf("oversize status = %d", response.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("store calls = %d", calls.Load())
	}
}

func TestSubmitPolicyReplayHashDeadlineAndWake(t *testing.T) {
	var mu sync.Mutex
	var first *taskstore.Admission
	var firstClaim task.IdempotencyClaim
	var wakes atomic.Int64
	var storeCalls atomic.Int64
	store := &fakeStore{admit: func(_ context.Context, p taskstore.AdmitTaskParams) (taskstore.Admission, error) {
		call := storeCalls.Add(1)
		if (call == 1 && wakes.Load() != 0) || (call > 1 && wakes.Load() != 1) {
			t.Fatal("wake happened before store commit")
		}
		if p.RepositoryID != 987654321 || p.BaseSHA != testSHA || p.Agent != "build" || p.Model != "test-model" || p.AcceptedAt != testNow || p.Deadline != testNow.Add(30*time.Minute) {
			t.Fatalf("unexpected server policy: %+v", p)
		}
		mu.Lock()
		defer mu.Unlock()
		if first == nil {
			value := acceptedAdmission(p)
			first = &value
			firstClaim = p.Claim
			return value, nil
		}
		disposition, err := task.ClassifyIdempotency(&firstClaim, p.Claim)
		if err != nil {
			return taskstore.Admission{}, err
		}
		switch disposition {
		case task.IdempotencyReplay:
			value := *first
			value.Replayed = true
			value.Task.State = task.TaskRunning
			value.Task.Revision = 9
			return value, nil
		case task.IdempotencyConflict:
			return taskstore.Admission{}, &taskstore.ConflictError{ReceiptID: testReceipt, TargetID: testTask}
		default:
			return taskstore.Admission{}, taskstore.ErrIdempotencyOwnerMismatch
		}
	}}
	config := testConfig(t, store)
	config.Wake = func() { wakes.Add(1) }
	handler := newTestHandler(t, config)
	body := `{"prompt":"secret prompt","baseRef":"main","title":"Fix it"}`
	firstResponse := request(handler, http.MethodPost, submitPath, body, postHeaders())
	replayResponse := request(handler, http.MethodPost, submitPath, `{"title":"Fix it","prompt":"secret prompt","baseRef":"main"}`, postHeaders())
	if firstResponse.Code != http.StatusAccepted || replayResponse.Code != http.StatusAccepted || firstResponse.Body.String() != replayResponse.Body.String() {
		t.Fatalf("responses not stable: first=%d %s replay=%d %s", firstResponse.Code, firstResponse.Body.String(), replayResponse.Code, replayResponse.Body.String())
	}
	if replayResponse.Header().Get("Idempotency-Replayed") != "true" || wakes.Load() != 1 {
		t.Fatalf("replay header/wakes = %q/%d", replayResponse.Header().Get("Idempotency-Replayed"), wakes.Load())
	}
	for _, secret := range []string{"secret prompt", "operator", "credential-v1", "requestHash", "ses_", "deliveryClaim"} {
		if strings.Contains(firstResponse.Body.String(), secret) {
			t.Errorf("response leaked %q: %s", secret, firstResponse.Body.String())
		}
	}
	conflict := request(handler, http.MethodPost, submitPath, `{"title":"Fix it","prompt":"changed","baseRef":"main"}`, postHeaders())
	if conflict.Code != http.StatusConflict || strings.Contains(conflict.Body.String(), string(testReceipt)) || strings.Contains(conflict.Body.String(), string(testTask)) {
		t.Fatalf("unsafe conflict: %d %s", conflict.Code, conflict.Body.String())
	}
}

func TestSubmitDoesNotWakeOnResolverOrStoreFailure(t *testing.T) {
	var calls, wakes atomic.Int64
	store := &fakeStore{admit: func(context.Context, taskstore.AdmitTaskParams) (taskstore.Admission, error) {
		calls.Add(1)
		if wakes.Load() != 0 {
			t.Fatal("wake before failed commit")
		}
		return taskstore.Admission{}, errors.New("database secret")
	}}
	config := testConfig(t, store)
	config.Wake = func() { wakes.Add(1) }
	config.BaseResolver = func(context.Context, string) (task.GitOID, error) { return "", errors.New("git secret") }
	handler := newTestHandler(t, config)
	body := `{"title":"t","prompt":"p","baseRef":"main"}`
	response := request(handler, http.MethodPost, submitPath, body, postHeaders())
	if response.Code != http.StatusUnprocessableEntity || calls.Load() != 0 || wakes.Load() != 0 || strings.Contains(response.Body.String(), "git secret") {
		t.Fatalf("resolver failure = %d calls=%d wakes=%d body=%s", response.Code, calls.Load(), wakes.Load(), response.Body.String())
	}
	config.BaseResolver = func(context.Context, string) (task.GitOID, error) { return testSHA, nil }
	handler = newTestHandler(t, config)
	response = request(handler, http.MethodPost, submitPath, body, postHeaders())
	if response.Code != http.StatusInternalServerError || calls.Load() != 1 || wakes.Load() != 0 || strings.Contains(response.Body.String(), "database secret") {
		t.Fatalf("store failure = %d calls=%d wakes=%d body=%s", response.Code, calls.Load(), wakes.Load(), response.Body.String())
	}
}

func TestSubmitReplayDoesNotDependOnBaseResolver(t *testing.T) {
	var baseCalls, wakeCalls atomic.Int32
	receiptProjection := json.RawMessage(`{"receiptId":"rcp_0198d34d-6a50-75fb-b1f2-b4a14d70ec57","taskId":"tsk_0198d34d-6a50-75fb-b1f2-b4a14d70ec55","attemptId":"att_0198d34d-6a50-75fb-b1f2-b4a14d70ec56"}`)
	receipt := taskstore.Receipt{
		ID: testReceipt, WorkspaceID: testWorkspace, CommandKind: taskstore.SubmitTaskCommand,
		State: taskstore.ReceiptAccepted, AcceptedAt: testNow, APIContractVersion: "fern.task.v1",
		TargetID: testTask, ResponseProjection: receiptProjection,
	}
	owner := taskstore.Task{
		ID: testTask, WorkspaceID: testWorkspace, Title: "Replay", Prompt: "existing prompt",
		RepositoryID: 987654321, BaseRef: "main", BaseSHA: testSHA, ObjectFormat: "sha1",
		State: task.TaskRunning, CurrentAttemptID: testAttempt, CreatedAt: testNow, UpdatedAt: testNow,
	}
	attempt := taskstore.Attempt{
		ID: testAttempt, TaskID: testTask, WorkspaceID: testWorkspace, State: task.AttemptAdmitted,
		OpenCodeSessionID:        "ses_0123456789abcdef0123456789abcdef",
		OpenCodeMessageID:        "msg_0123456789abcdef0123456789abcdef",
		ExecutionContractVersion: "execution.v1", Agent: "build", ModelProvider: "test", Model: "test-model",
		BudgetSnapshot: json.RawMessage(`{"turns":10}`), Deadline: testNow.Add(30 * time.Minute),
	}
	store := &fakeStore{
		receipt: func(context.Context, task.WorkspaceID, string, task.IdempotencyKey) (taskstore.Receipt, bool, error) {
			return receipt, true, nil
		},
		get:     func(context.Context, task.TaskID) (taskstore.Task, error) { return owner, nil },
		attempt: func(context.Context, task.AttemptID) (taskstore.Attempt, error) { return attempt, nil },
		admit: func(_ context.Context, params taskstore.AdmitTaskParams) (taskstore.Admission, error) {
			if params.BaseSHA != testSHA || params.OpenCodeSessionID != attempt.OpenCodeSessionID || params.ReceiptID != receipt.ID {
				t.Fatalf("replay params = %+v", params)
			}
			result := acceptedAdmission(params)
			result.Receipt = receipt
			result.Replayed = true
			return result, nil
		},
	}
	config := testConfig(t, store)
	config.BaseResolver = func(context.Context, string) (task.GitOID, error) {
		baseCalls.Add(1)
		return "", errors.New("GitHub unavailable")
	}
	config.Wake = func() { wakeCalls.Add(1) }
	handler := newTestHandler(t, config)
	response := request(handler, http.MethodPost, submitPath, `{"title":"Replay","prompt":"existing prompt","baseRef":"main"}`, postHeaders())
	if response.Code != http.StatusAccepted || response.Header().Get("Idempotency-Replayed") != "true" || baseCalls.Load() != 0 || wakeCalls.Load() != 0 {
		t.Fatalf("status=%d replay=%q base=%d wake=%d body=%s", response.Code, response.Header().Get("Idempotency-Replayed"), baseCalls.Load(), wakeCalls.Load(), response.Body.String())
	}
}

func TestGetTaskOwnershipAndRedaction(t *testing.T) {
	prompt := "never expose this"
	owner := taskstore.Task{
		ID: testTask, WorkspaceID: testWorkspace, Title: "Safe title", Prompt: prompt, State: task.TaskRunning,
		RepositoryID: 987654321, BaseRef: "main", BaseSHA: testSHA, CurrentAttemptID: testAttempt,
		Actor: testActor(), LatestEventCursor: 8, Revision: 3, CreatedAt: testNow, UpdatedAt: testNow,
	}
	attempt := taskstore.Attempt{
		ID: testAttempt, TaskID: testTask, WorkspaceID: testWorkspace, Sequence: 1, State: task.AttemptRunning,
		Deadline: testNow.Add(time.Hour), DeliveryClaimOwner: ptr("internal-worker"), BudgetSnapshot: json.RawMessage(`{"secret":true}`),
		OpenCodeSessionID: "ses_0123456789abcdef0123456789abcdef", Revision: 4, CreatedAt: testNow, UpdatedAt: testNow,
	}
	store := &fakeStore{
		get:     func(context.Context, task.TaskID) (taskstore.Task, error) { return owner, nil },
		attempt: func(context.Context, task.AttemptID) (taskstore.Attempt, error) { return attempt, nil },
	}
	handler := newTestHandler(t, testConfig(t, store))
	response := request(handler, http.MethodGet, apiPrefix+string(testTask), "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{prompt, "operator", "internal-worker", "secret", "promptSHA256", "budget"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Errorf("response leaked %q: %s", secret, response.Body.String())
		}
	}
	if !strings.Contains(response.Body.String(), `"openCodePath":"/session/ses_0123456789abcdef0123456789abcdef"`) {
		t.Fatalf("response omitted exact OpenCode deep link: %s", response.Body.String())
	}

	owner.WorkspaceID = task.WorkspaceID("wsp_0198d34d-5e40-7c5a-8e3f-6bfad471ae99")
	response = request(handler, http.MethodGet, apiPrefix+string(testTask), "", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace status = %d", response.Code)
	}
	owner.WorkspaceID = testWorkspace
	attempt.WorkspaceID = task.WorkspaceID("wsp_0198d34d-5e40-7c5a-8e3f-6bfad471ae99")
	response = request(handler, http.MethodGet, apiPrefix+string(testTask), "", nil)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("attempt ownership status = %d", response.Code)
	}
}

func TestListTasksUsesDurableServerSnapshots(t *testing.T) {
	resultID := task.ResultID("res_0198d34d-6a50-75fb-b1f2-b4a14d70ec60")
	verificationID := task.VerificationID("ver_0198d34d-6a50-75fb-b1f2-b4a14d70ec61")
	publicationID := task.PublicationID("pub_0198d34d-6a50-75fb-b1f2-b4a14d70ec62")
	sealRequestID := task.SealRequestID("slr_0198d34d-6a50-75fb-b1f2-b4a14d70ec63")
	store := &fakeStore{list: func(_ context.Context, workspace task.WorkspaceID, limit int) ([]taskstore.TaskSnapshot, error) {
		if workspace != testWorkspace || limit != 25 {
			t.Fatalf("list scope = %s/%d", workspace, limit)
		}
		return []taskstore.TaskSnapshot{{
			Task: taskstore.Task{ID: testTask, WorkspaceID: testWorkspace, Title: "Durable task", State: task.TaskRunning,
				RepositoryID: 987654321, BaseRef: "main", BaseSHA: testSHA, CurrentAttemptID: testAttempt,
				SealedResultID: resultID, Revision: 3, CreatedAt: testNow, UpdatedAt: testNow, Prompt: "secret prompt"},
			Attempt: taskstore.Attempt{ID: testAttempt, TaskID: testTask, WorkspaceID: testWorkspace, Sequence: 1,
				State: task.AttemptRunning, OpenCodeSessionID: "ses_0123456789abcdef0123456789abcdef",
				Deadline: testNow.Add(time.Hour), Revision: 4, CreatedAt: testNow, UpdatedAt: testNow},
			SealRequest: &taskstore.SealRequest{ID: sealRequestID, WorkspaceID: testWorkspace, TaskID: testTask,
				AttemptID: testAttempt, ResultID: resultID, State: taskstore.SealRequestCompleted, AcceptedAt: testNow},
			Result: &taskstore.Result{ID: resultID, WorkspaceID: testWorkspace, TaskID: testTask, AttemptID: testAttempt,
				State: task.ResultSealed, Outcome: task.ResultChanged, BaseSHA: testSHA, ResultCommit: testSHA, SealedAt: testNow},
			Verifications: []taskstore.Verification{{ID: verificationID, ResultID: resultID, TaskID: testTask,
				AttemptID: testAttempt, WorkspaceID: testWorkspace, State: taskstore.VerificationSucceeded,
				PolicyName: "release", VerifiedCommit: testSHA, Outcome: "succeeded", Revision: 2, CreatedAt: testNow, UpdatedAt: testNow}},
			Publication: &taskstore.Publication{ID: publicationID, OperationID: "op_0198d34d-6a50-75fb-b1f2-b4a14d70ec64",
				ResultID: resultID, VerificationID: verificationID, TaskID: testTask, AttemptID: testAttempt,
				WorkspaceID: testWorkspace, State: taskstore.PublicationPrepared, EffectPhase: taskstore.PublicationPhaseNone,
				Tuple: task.PublicationTuple{Branch: "fern/demo/op", ResultCommit: testSHA}, Revision: 1, CreatedAt: testNow, UpdatedAt: testNow},
		}}, nil
	}}
	handler := newTestHandler(t, testConfig(t, store))
	response := request(handler, http.MethodGet, submitPath+"?limit=25", "", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"title":"Durable task"`) ||
		!strings.Contains(response.Body.String(), `"openCodePath":"/session/ses_0123456789abcdef0123456789abcdef"`) ||
		!strings.Contains(response.Body.String(), `"sealRequest":{"id":"`+string(sealRequestID)) ||
		!strings.Contains(response.Body.String(), `"verifications":[{"id":"`+string(verificationID)) ||
		!strings.Contains(response.Body.String(), `"publication":{"id":"`+string(publicationID)) {
		t.Fatalf("list = %d %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"secret prompt", "credential", "budget"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("list leaked %q: %s", secret, response.Body.String())
		}
	}
	for _, query := range []string{"?limit=0", "?limit=201", "?limit=01", "?other=1", "?limit=1&limit=2"} {
		if got := request(handler, http.MethodGet, submitPath+query, "", nil); got.Code != http.StatusBadRequest {
			t.Fatalf("query %q status=%d", query, got.Code)
		}
	}
}

func TestEventsQueryAndSafeProjection(t *testing.T) {
	store := &fakeStore{events: func(_ context.Context, workspace task.WorkspaceID, after task.Cursor, limit int) (taskstore.EventPage, error) {
		if workspace != testWorkspace || after != 7 || limit != 25 {
			t.Fatalf("query = %s/%d/%d", workspace, after, limit)
		}
		return taskstore.EventPage{
			Events: []taskstore.Event{{
				ID: testEvent, Cursor: 8, WorkspaceID: testWorkspace, TaskID: testTask, AttemptID: testAttempt,
				Type: "attempt.running", Version: 1, OccurredAt: testNow, Actor: testActor(), Payload: json.RawMessage(`{"prompt":"secret"}`),
			}},
			NextCursor: 8, Watermark: 9, CaughtUp: false,
		}, nil
	}}
	handler := newTestHandler(t, testConfig(t, store))
	response := request(handler, http.MethodGet, eventsPath+"?after=7&limit=25", "", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"cursor":"8"`) {
		t.Fatalf("events response = %d %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"prompt", "secret", "operator", "credential", "payload"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Errorf("events leaked %q: %s", secret, response.Body.String())
		}
	}
	for _, query := range []string{"?after=01", "?after=-1", "?limit=0", "?limit=501", "?limit=1&limit=2", "?unknown=1"} {
		if got := request(handler, http.MethodGet, eventsPath+query, "", nil); got.Code != http.StatusBadRequest {
			t.Errorf("query %q status = %d", query, got.Code)
		}
	}
}

func TestCancellationOptionalBodyReplayConflictAndStrictness(t *testing.T) {
	var mu sync.Mutex
	var first *taskstore.Cancellation
	var firstClaim task.IdempotencyClaim
	var wakes atomic.Int64
	var storeCalls atomic.Int64
	store := &fakeStore{cancel: func(_ context.Context, p taskstore.RequestCancellationParams) (taskstore.Cancellation, error) {
		if _, err := task.ParseReceiptID(string(p.ReceiptID)); err != nil {
			t.Fatalf("receipt ID: %v", err)
		}
		if _, err := task.ParseEventID(string(p.AttemptEventID)); err != nil {
			t.Fatalf("attempt event ID: %v", err)
		}
		if _, err := task.ParseEventID(string(p.TaskEventID)); err != nil || p.TaskEventID == p.AttemptEventID {
			t.Fatalf("task event ID: %v", err)
		}
		call := storeCalls.Add(1)
		if p.Now != testNow || (call == 1 && wakes.Load() != 0) || (call > 1 && wakes.Load() != 1) {
			t.Fatalf("time/wake at store = %s/%d", p.Now, wakes.Load())
		}
		mu.Lock()
		defer mu.Unlock()
		if first == nil {
			value := acceptedCancellation(p)
			first = &value
			firstClaim = p.Claim
			return value, nil
		}
		disposition, _ := task.ClassifyIdempotency(&firstClaim, p.Claim)
		if disposition == task.IdempotencyReplay {
			value := *first
			value.Replayed = true
			return value, nil
		}
		return taskstore.Cancellation{}, &taskstore.ConflictError{ReceiptID: testReceipt, TargetID: testTask}
	}}
	config := testConfig(t, store)
	config.Wake = func() { wakes.Add(1) }
	handler := newTestHandler(t, config)
	firstResponse := request(handler, http.MethodPost, apiPrefix+string(testTask)+"/cancel", "", postHeaders())
	replay := request(handler, http.MethodPost, apiPrefix+string(testTask)+"/cancel", "   ", postHeaders())
	if firstResponse.Code != http.StatusAccepted || replay.Code != http.StatusAccepted || firstResponse.Body.String() != replay.Body.String() || replay.Header().Get("Idempotency-Replayed") != "true" || wakes.Load() != 1 {
		t.Fatalf("cancel replay: first=%d %s replay=%d %s wakes=%d", firstResponse.Code, firstResponse.Body.String(), replay.Code, replay.Body.String(), wakes.Load())
	}
	conflict := request(handler, http.MethodPost, apiPrefix+string(testTask)+"/cancel", `{"reason":"changed"}`, postHeaders())
	if conflict.Code != http.StatusConflict || strings.Contains(conflict.Body.String(), string(testReceipt)) {
		t.Fatalf("cancel conflict = %d %s", conflict.Code, conflict.Body.String())
	}
	for _, body := range []string{`{"Reason":"x"}`, `{"reason":"x","reason":"y"}`, `{"reason":"x","other":1}`, `null`, `{"reason":1}`, `{"reason":"x"} {}`} {
		response := request(handler, http.MethodPost, apiPrefix+string(testTask)+"/cancel", body, map[string]string{"Content-Type": "application/json", "Idempotency-Key": "other"})
		if response.Code != http.StatusBadRequest {
			t.Errorf("body %q status = %d", body, response.Code)
		}
	}
}

func TestSealPreviewAndExactAuthorization(t *testing.T) {
	manifest := sha256.Sum256([]byte("[]"))
	snapshot := taskresultcoord.SealSnapshot{
		TaskID: testTask, AttemptID: testAttempt, WorkspaceRevision: 3, TaskRevision: 4, AttemptRevision: 5,
		ResultCommit: testSHA, TreeOID: task.GitOID("1111111111111111111111111111111111111111"),
		Outcome: task.ResultNoChanges, ManifestEntries: 0, ManifestSHA256: manifest, WorktreeClean: true,
	}
	var wakes atomic.Int64
	authorizer := &fakeSealAuthorizer{
		preview: func(_ context.Context, id task.TaskID) (taskresultcoord.SealSnapshot, error) {
			if id != testTask {
				t.Fatalf("preview task = %s", id)
			}
			return snapshot, nil
		},
		request: func(_ context.Context, expected taskresultcoord.SealSnapshot, params taskstore.RequestSealParams) (taskstore.SealAdmission, error) {
			if expected != snapshot || params.TaskID != testTask || params.Claim.Actor != testActor() ||
				params.Claim.Scope.CommandKind != taskstore.SealTaskCommand || params.AcceptedAt != testNow {
				t.Fatalf("expected=%+v params=%+v", expected, params)
			}
			return taskstore.SealAdmission{
				Request: taskstore.SealRequest{ID: params.SealRequestID, TaskID: testTask, AttemptID: testAttempt, State: taskstore.SealRequestPending},
				Receipt: taskstore.Receipt{ID: params.ReceiptID, WorkspaceID: testWorkspace, CommandKind: taskstore.SealTaskCommand,
					State: taskstore.ReceiptAccepted, AcceptedAt: testNow, TargetID: testTask},
			}, nil
		},
	}
	config := testConfig(t, &fakeStore{})
	config.SealAuthorizer = authorizer
	config.SealWake = func() { wakes.Add(1) }
	handler := newTestHandler(t, config)
	preview := request(handler, http.MethodPost, apiPrefix+string(testTask)+"/seal-preview", "{}", map[string]string{"Content-Type": "application/json"})
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), `"manifestSha256":"sha256:`) {
		t.Fatalf("preview = %d %s", preview.Code, preview.Body.String())
	}
	body := strings.TrimSpace(preview.Body.String())
	accepted := request(handler, http.MethodPost, apiPrefix+string(testTask)+"/seal", body, postHeaders())
	if accepted.Code != http.StatusAccepted || wakes.Load() != 1 || !strings.Contains(accepted.Body.String(), `"state":"pending"`) {
		t.Fatalf("accepted = %d %s wakes=%d", accepted.Code, accepted.Body.String(), wakes.Load())
	}
}

func TestSealPreviewRequiresExplicitEmptyPost(t *testing.T) {
	var previews atomic.Int64
	config := testConfig(t, &fakeStore{})
	config.SealAuthorizer = &fakeSealAuthorizer{preview: func(context.Context, task.TaskID) (taskresultcoord.SealSnapshot, error) {
		previews.Add(1)
		return taskresultcoord.SealSnapshot{}, nil
	}}
	handler := newTestHandler(t, config)
	path := apiPrefix + string(testTask) + "/seal-preview"

	get := request(handler, http.MethodGet, path, "", nil)
	if get.Code != http.StatusMethodNotAllowed || get.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET preview = %d allow=%q", get.Code, get.Header().Get("Allow"))
	}
	for _, test := range []struct {
		name    string
		path    string
		body    string
		headers map[string]string
	}{
		{name: "missing content type", path: path, body: "{}"},
		{name: "unknown field", path: path, body: `{"unexpected":true}`, headers: map[string]string{"Content-Type": "application/json"}},
		{name: "trailing JSON", path: path, body: "{}{}", headers: map[string]string{"Content-Type": "application/json"}},
		{name: "query", path: path + "?wake=true", body: "{}", headers: map[string]string{"Content-Type": "application/json"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := request(handler, http.MethodPost, test.path, test.body, test.headers)
			if response.Code < 400 {
				t.Fatalf("preview = %d %s", response.Code, response.Body.String())
			}
		})
	}
	if previews.Load() != 0 {
		t.Fatalf("invalid preview invoked authorizer %d times", previews.Load())
	}
}

func TestSealRejectsChangedSnapshotAndStrictInput(t *testing.T) {
	manifest := sha256.Sum256([]byte("[]"))
	snapshot := taskresultcoord.SealSnapshot{TaskID: testTask, AttemptID: testAttempt, WorkspaceRevision: 3, TaskRevision: 4,
		AttemptRevision: 5, ResultCommit: testSHA, TreeOID: testSHA, Outcome: task.ResultNoChanges,
		ManifestSHA256: manifest, WorktreeClean: true}
	authorizer := &fakeSealAuthorizer{
		preview: func(context.Context, task.TaskID) (taskresultcoord.SealSnapshot, error) { return snapshot, nil },
		request: func(context.Context, taskresultcoord.SealSnapshot, taskstore.RequestSealParams) (taskstore.SealAdmission, error) {
			return taskstore.SealAdmission{}, taskresultcoord.ErrSelectionChanged
		},
	}
	config := testConfig(t, &fakeStore{})
	config.SealAuthorizer = authorizer
	handler := newTestHandler(t, config)
	preview := request(handler, http.MethodPost, apiPrefix+string(testTask)+"/seal-preview", "{}", map[string]string{"Content-Type": "application/json"})
	changed := request(handler, http.MethodPost, apiPrefix+string(testTask)+"/seal", strings.TrimSpace(preview.Body.String()), postHeaders())
	if changed.Code != http.StatusConflict || !strings.Contains(changed.Body.String(), "snapshot_changed") {
		t.Fatalf("changed = %d %s", changed.Code, changed.Body.String())
	}
	for _, body := range []string{"{}", `{"attemptId":"bad"}`, strings.ReplaceAll(strings.TrimSpace(preview.Body.String()), `"worktreeClean":true`, `"worktreeClean":false`)} {
		response := request(handler, http.MethodPost, apiPrefix+string(testTask)+"/seal", body, postHeaders())
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body %s status=%d response=%s", body, response.Code, response.Body.String())
		}
	}
}

func TestDeadlineErrorsAreSafe(t *testing.T) {
	store := &fakeStore{
		admit: func(context.Context, taskstore.AdmitTaskParams) (taskstore.Admission, error) {
			return taskstore.Admission{}, context.DeadlineExceeded
		},
		get: func(context.Context, task.TaskID) (taskstore.Task, error) { return taskstore.Task{}, context.Canceled },
	}
	handler := newTestHandler(t, testConfig(t, store))
	response := request(handler, http.MethodPost, submitPath, `{"title":"t","prompt":"p","baseRef":"main"}`, postHeaders())
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("deadline status = %d", response.Code)
	}
	response = request(handler, http.MethodGet, apiPrefix+string(testTask), "", nil)
	if response.Code != http.StatusRequestTimeout {
		t.Fatalf("canceled status = %d", response.Code)
	}
}

func TestOwnershipAndCancellationErrorsDoNotLeakStoreDetails(t *testing.T) {
	store := &fakeStore{
		admit: func(context.Context, taskstore.AdmitTaskParams) (taskstore.Admission, error) {
			return taskstore.Admission{}, taskstore.ErrIdempotencyOwnerMismatch
		},
		get: func(context.Context, task.TaskID) (taskstore.Task, error) {
			return taskstore.Task{}, &taskstore.NotFoundError{Kind: "task", ID: string(testTask)}
		},
		cancel: func(context.Context, taskstore.RequestCancellationParams) (taskstore.Cancellation, error) {
			return taskstore.Cancellation{}, &taskstore.TerminalTaskError{TaskID: testTask, State: task.TaskCompleted}
		},
	}
	handler := newTestHandler(t, testConfig(t, store))
	submit := request(handler, http.MethodPost, submitPath, `{"title":"t","prompt":"p","baseRef":"main"}`, postHeaders())
	if submit.Code != http.StatusForbidden || strings.Contains(submit.Body.String(), string(testTask)) || strings.Contains(submit.Body.String(), string(testReceipt)) {
		t.Fatalf("owner mismatch = %d %s", submit.Code, submit.Body.String())
	}
	read := request(handler, http.MethodGet, apiPrefix+string(testTask), "", nil)
	if read.Code != http.StatusNotFound || strings.Contains(read.Body.String(), string(testTask)) {
		t.Fatalf("not found = %d %s", read.Code, read.Body.String())
	}
	cancel := request(handler, http.MethodPost, apiPrefix+string(testTask)+"/cancel", `{}`, postHeaders())
	if cancel.Code != http.StatusConflict || strings.Contains(cancel.Body.String(), string(testTask)) {
		t.Fatalf("terminal cancellation = %d %s", cancel.Code, cancel.Body.String())
	}
}

func TestConstructorRejectsIncompletePolicy(t *testing.T) {
	base := testConfig(t, &fakeStore{})
	tests := []func(*Config){
		func(c *Config) { c.Store = nil },
		func(c *Config) { c.ActorResolver = nil },
		func(c *Config) { c.BaseResolver = nil },
		func(c *Config) { c.Wake = nil },
		func(c *Config) { c.AttemptTimeout = 0 },
		func(c *Config) { c.RepositoryID = 0 },
		func(c *Config) { c.BudgetSnapshot = nil },
		func(c *Config) { c.ObjectFormat = "sha256" },
	}
	for i, mutate := range tests {
		config := base
		mutate(&config)
		if _, err := New(config); err == nil {
			t.Errorf("case %d was accepted", i)
		}
	}
}

func ptr(value string) *string { return &value }
