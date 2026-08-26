package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/publication"
	"github.com/nebler/fern/internal/runtime"
)

type fakePublicationExecutor struct {
	store *control.Store
	calls atomic.Int32
	err   error
}

func (executor *fakePublicationExecutor) Execute(_ context.Context, id string) (control.Publication, error) {
	executor.calls.Add(1)
	record, _ := executor.store.Publication(id)
	if executor.err != nil {
		return record, executor.err
	}
	if record.ResultCommit == "" {
		prepared := proxyPreparedPublication()
		if err := executor.store.PreparePublication(id, prepared, time.Now()); err != nil {
			return record, err
		}
	}
	pull := proxyPullRequestObservation(proxyPreparedPublication())
	return executor.store.FinishPublication(id, &pull, "", time.Now())
}

func TestControlWorkflowAndPublicationDoNotWakeWorkspace(t *testing.T) {
	t.Parallel()
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := store.CreateWorkflow("Fix signup", "ses_123", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	waker := &countingWaker{}
	executor := &fakePublicationExecutor{store: store}
	handler := NewWithControls(waker, runtime.ServerAuth{Password: "secret"}, Controls{
		Store: store, Publications: executor, ControlAuth: ControlAuth{Password: "control-secret"},
	}, testLogger())
	request := httptest.NewRequest(http.MethodPost, "/fern/api/v1/workflows/"+workflow.ID+"/publish", strings.NewReader(`{"operation":"operation","title":"Fix signup"}`))
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth("fern", "control-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("publication status=%d body=%q", response.Code, response.Body.String())
	}
	if waker.wakes.Load() != 0 || executor.calls.Load() != 1 {
		t.Fatalf("wake=%d publications=%d", waker.wakes.Load(), executor.calls.Load())
	}
	updated, _ := store.Workflow(workflow.ID)
	publicationRecord, exists := store.Publication(updated.PublicationID)
	if !exists || updated.Status != control.WorkflowPublished || publicationRecord.State != "published" || publicationRecord.PullURL == "" {
		t.Fatalf("workflow=%+v publication=%+v exists=%t", updated, publicationRecord, exists)
	}
}

func TestControlPublicationRejectsCrossOriginBeforeFencing(t *testing.T) {
	t.Parallel()
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := store.CreateWorkflow("Fix signup", "ses_123", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	executor := &fakePublicationExecutor{store: store}
	handler := NewWithControls(&countingWaker{}, runtime.ServerAuth{Password: "secret"}, Controls{
		Store: store, Publications: executor, ControlAuth: ControlAuth{Password: "control-secret"},
	}, testLogger())
	request := httptest.NewRequest(http.MethodPost, "/fern/api/v1/workflows/"+workflow.ID+"/publish", strings.NewReader(`{}`))
	request.Host = "fern.example"
	request.Header.Set("Origin", "https://evil.example")
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth("fern", "control-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || executor.calls.Load() != 0 {
		t.Fatalf("status=%d publications=%d", response.Code, executor.calls.Load())
	}
}

func TestControlPublicationRequiresConfiguredAuthentication(t *testing.T) {
	t.Parallel()
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := store.CreateWorkflow("Fix signup", "ses_123", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	executor := &fakePublicationExecutor{store: store}
	request := httptest.NewRequest(http.MethodPost, "/fern/api/v1/workflows/"+workflow.ID+"/publish", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	NewWithControls(&countingWaker{}, runtime.ServerAuth{}, Controls{Store: store, Publications: executor}, testLogger()).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || executor.calls.Load() != 0 {
		t.Fatalf("status=%d publications=%d", response.Code, executor.calls.Load())
	}
}

func TestControlPublicationReportsBusyWorkspace(t *testing.T) {
	t.Parallel()
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := store.CreateWorkflow("Fix signup", "ses_123", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	executor := &fakePublicationExecutor{store: store, err: errors.New("busy")}
	request := httptest.NewRequest(http.MethodPost, "/fern/api/v1/workflows/"+workflow.ID+"/publish", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth("fern", "control-secret")
	response := httptest.NewRecorder()
	NewWithControls(&countingWaker{}, runtime.ServerAuth{Password: "secret"}, Controls{Store: store, Publications: executor, ControlAuth: ControlAuth{Password: "control-secret"}}, testLogger()).ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || executor.calls.Load() != 1 {
		t.Fatalf("status=%d publications=%d", response.Code, executor.calls.Load())
	}
}

func TestInvalidPublicationDoesNotPoisonWorkflow(t *testing.T) {
	t.Parallel()
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := store.CreateWorkflow("Fix signup", "ses_123", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	executor := &fakePublicationExecutor{store: store}
	request := httptest.NewRequest(http.MethodPost, "/fern/api/v1/workflows/"+workflow.ID+"/publish", strings.NewReader(`{"operation":"../unsafe"}`))
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth("fern", "control-secret")
	response := httptest.NewRecorder()
	NewWithControls(&countingWaker{}, runtime.ServerAuth{Password: "secret"}, Controls{Store: store, Publications: executor, ControlAuth: ControlAuth{Password: "control-secret"}}, testLogger()).ServeHTTP(response, request)
	updated, _ := store.Workflow(workflow.ID)
	if response.Code != http.StatusUnprocessableEntity || updated.PublicationID != "" || executor.calls.Load() != 0 {
		t.Fatalf("status=%d workflow=%+v publications=%d", response.Code, updated, executor.calls.Load())
	}
}

func TestFailedPublicationRemainsRetryableInControlPage(t *testing.T) {
	t.Parallel()
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := store.CreateWorkflow("Fix signup", "ses_123", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	publicationRecord, _, err := store.RequestPublication(workflow.ID, control.Publication{
		ID: "publication-1", Operation: "operation", Title: "Fix signup",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishPublication(publicationRecord.ID, nil, "publication failed", time.Now()); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/fern/control", nil)
	request.SetBasicAuth("fern", "control-secret")
	response := httptest.NewRecorder()
	NewWithControls(nil, runtime.ServerAuth{Password: "secret"}, Controls{
		Store: store, Publications: &fakePublicationExecutor{store: store}, ControlAuth: ControlAuth{Password: "control-secret"},
	}, testLogger()).ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Retry publication") {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func proxyPreparedPublication() control.PreparedPublication {
	return control.PreparedPublication{
		RepositoryID: 123, RepositoryFullName: "owner/repo",
		BaseSHA: strings.Repeat("b", 40), BaseRef: "main",
		ResultCommit: strings.Repeat("a", 40), Branch: "fern/demo/operation",
	}
}

func proxyPullRequestObservation(prepared control.PreparedPublication) control.PullRequestObservation {
	return control.PullRequestObservation{
		TargetRepositoryID: prepared.RepositoryID, TargetRepositoryFullName: prepared.RepositoryFullName,
		Number: 1, URL: "https://github.com/owner/repo/pull/1", State: "open", Draft: true,
		Base: control.PullRequestRefObservation{RepositoryID: prepared.RepositoryID, RepositoryFullName: prepared.RepositoryFullName, RepositoryOwner: "owner", RepositoryName: "repo", Ref: prepared.BaseRef, SHA: prepared.BaseSHA},
		Head: control.PullRequestRefObservation{RepositoryID: prepared.RepositoryID, RepositoryFullName: prepared.RepositoryFullName, RepositoryOwner: "owner", RepositoryName: "repo", Ref: prepared.Branch, SHA: prepared.ResultCommit},
	}
}

func TestControlMutationOriginPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		origin    string
		host      string
		fetchSite string
		want      bool
	}{
		{name: "non-browser client", host: "fern.example", want: true},
		{name: "same HTTPS origin", origin: "https://fern.example", host: "fern.example", fetchSite: "same-origin", want: true},
		{name: "same HTTP origin", origin: "http://127.0.0.1:8080", host: "127.0.0.1:8080", want: true},
		{name: "cross origin", origin: "https://evil.example", host: "fern.example"},
		{name: "same site sibling", origin: "https://fern.example", host: "fern.example", fetchSite: "same-site"},
		{name: "cross site metadata", host: "fern.example", fetchSite: "cross-site"},
		{name: "opaque metadata", host: "fern.example", fetchSite: "none"},
		{name: "opaque origin", origin: "null", host: "fern.example"},
		{name: "userinfo", origin: "https://user@fern.example", host: "fern.example"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "https://"+test.host+"/fern/workflows", nil)
			request.Host = test.host
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.fetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", test.fetchSite)
			}
			if got := sameOrigin(request); got != test.want {
				t.Fatalf("sameOrigin() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestOperatorMutationUsesTrustedOriginNotClientHost(t *testing.T) {
	t.Parallel()
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	handlers, err := NewHandlers(nil, runtime.ServerAuth{Password: "backend-secret"}, Controls{
		Store: store, ControlAuth: ControlAuth{Password: "control-secret"},
	}, TrustedOrigins{Remote: "https://fern.example.ts.net", Operator: "http://127.0.0.1:8081"}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/fern/api/v1/workflows", strings.NewReader(`{"title":"Trusted","sessionId":"ses_1"}`))
	request.Host = "spoofed.example"
	request.Header.Set("Origin", "http://127.0.0.1:8081")
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth("fern", "control-secret")
	response := httptest.NewRecorder()
	handlers.Operator.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("trusted operator mutation status=%d body=%q", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/fern/api/v1/workflows", strings.NewReader(`{"title":"Rejected","sessionId":"ses_2"}`))
	request.Host = "127.0.0.1:8081"
	request.Header.Set("Origin", "http://spoofed.example")
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth("fern", "control-secret")
	response = httptest.NewRecorder()
	handlers.Operator.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin operator mutation status=%d", response.Code)
	}
}

func TestOversizedWorkflowFormDoesNotMutateState(t *testing.T) {
	t.Parallel()
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	body := "title=" + strings.Repeat("x", 20<<10) + "&sessionId=ses_demo"
	request := httptest.NewRequest(http.MethodPost, "/fern/workflows", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetBasicAuth("fern", "control-secret")
	response := httptest.NewRecorder()
	NewWithControls(nil, runtime.ServerAuth{Password: "secret"}, Controls{Store: store, ControlAuth: ControlAuth{Password: "control-secret"}}, testLogger()).ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge && response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if workflows := store.Workflows(); len(workflows) != 0 {
		t.Fatalf("oversized form created workflows: %+v", workflows)
	}
}

func TestGeneratedPublicationOperationAlwaysStartsAlphanumeric(t *testing.T) {
	t.Parallel()
	for range 100 {
		id, err := randomCredential()
		if err != nil {
			t.Fatal(err)
		}
		operation := "op-" + id[:13]
		if err := publication.ValidateRequest(publication.Request{Operation: operation, Title: "Demo"}); err != nil {
			t.Fatalf("generated operation %q failed validation: %v", operation, err)
		}
	}
}

func TestRevocationCallbackRunsOnlyAfterSuccessfulPersistence(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "control")
	store, err := control.Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	device, err := store.AddDevice("first", "First", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	callbackIDs := make(chan string, 2)
	callback := func(id string) {
		if _, valid, authErr := store.AuthenticateDeviceIdentity("first", now); authErr != nil || valid {
			t.Errorf("callback ran before durable removal: valid=%t err=%v", valid, authErr)
		}
		callbackIDs <- id
	}
	if err := revokeDevice(store, device.ID, callback); err != nil {
		t.Fatal(err)
	}
	if got := <-callbackIDs; got != device.ID {
		t.Fatalf("callback ID=%q, want %q", got, device.ID)
	}
	if err := revokeDevice(store, device.ID, callback); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second revocation error=%v", err)
	}
	select {
	case id := <-callbackIDs:
		t.Fatalf("failed revocation invoked callback for %q", id)
	default:
	}

	second, err := store.AddDevice("second", "Second", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	moved := directory + "-moved"
	if err := os.Rename(directory, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(directory, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := revokeDevice(store, second.ID, callback); err == nil {
		t.Fatal("revocation unexpectedly persisted with an unavailable state directory")
	}
	select {
	case id := <-callbackIDs:
		t.Fatalf("persistence failure invoked callback for %q", id)
	default:
	}
}

func TestDeviceRegistrationRevocationRaceIsFenced(t *testing.T) {
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for iteration := range 100 {
		token := "device-" + string(rune(iteration+1))
		device, err := store.AddDevice(token, "Racing device", now, now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		start := make(chan struct{})
		registered := make(chan bool, 1)
		go func() {
			<-start
			_, admitted := store.RegisterDeviceRequest(device.ID, cancel)
			registered <- admitted
		}()
		revoked := make(chan error, 1)
		go func() {
			<-start
			revoked <- revokeDevice(store, device.ID, store.CancelDeviceRequests)
		}()
		close(start)
		admitted := <-registered
		if err := <-revoked; err != nil {
			t.Fatal(err)
		}
		if !admitted {
			cancel()
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatalf("iteration %d left an admitted request alive", iteration)
		}
	}
}
