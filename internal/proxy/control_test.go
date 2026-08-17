package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/publication"
	"github.com/nebler/fern/internal/runtime"
)

type fakePublicationFencer struct {
	acquired atomic.Int32
	released atomic.Int32
	err      error
}

func (fencer *fakePublicationFencer) AcquirePaused(context.Context) (func(), error) {
	if fencer.err != nil {
		return nil, fencer.err
	}
	fencer.acquired.Add(1)
	return func() { fencer.released.Add(1) }, nil
}

type fakeGitHubPublisher struct {
	calls atomic.Int32
}

type recoveryGitHubPublisher struct {
	prepared chan publication.Prepared
}

func (publisher *recoveryGitHubPublisher) Publish(context.Context, publication.Request) (publication.Result, error) {
	return publication.Result{}, errors.New("unexpected mutable preflight")
}

func (publisher *recoveryGitHubPublisher) PublishPrepared(_ context.Context, prepared publication.Prepared, _, _ string) (publication.Result, error) {
	publisher.prepared <- prepared
	return publication.Result{Prepared: prepared, URL: "https://github.com/owner/repo/pull/1"}, nil
}

func (publisher *fakeGitHubPublisher) Publish(_ context.Context, request publication.Request) (publication.Result, error) {
	publisher.calls.Add(1)
	prepared := publication.Prepared{
		Repository: "owner/repo", Base: "main", Commit: strings.Repeat("a", 40), Branch: "fern/demo/operation",
	}
	if request.BeforePush != nil {
		if err := request.BeforePush(prepared); err != nil {
			return publication.Result{}, err
		}
	}
	return publication.Result{Prepared: prepared, URL: "https://github.com/owner/repo/pull/1"}, nil
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
	fencer := &fakePublicationFencer{}
	publisher := &fakeGitHubPublisher{}
	handler := NewWithControls(waker, runtime.ServerAuth{Password: "secret"}, Controls{
		Store: store, Fencer: fencer, Publisher: publisher,
	}, testLogger())
	request := httptest.NewRequest(http.MethodPost, "/fern/api/v1/workflows/"+workflow.ID+"/publish", strings.NewReader(`{"operation":"operation","title":"Fix signup"}`))
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth("opencode", "secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("publication status=%d body=%q", response.Code, response.Body.String())
	}
	if waker.wakes.Load() != 0 || fencer.acquired.Load() != 1 || fencer.released.Load() != 1 || publisher.calls.Load() != 1 {
		t.Fatalf("wake=%d acquired=%d released=%d publishes=%d", waker.wakes.Load(), fencer.acquired.Load(), fencer.released.Load(), publisher.calls.Load())
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
	fencer := &fakePublicationFencer{}
	publisher := &fakeGitHubPublisher{}
	handler := NewWithControls(&countingWaker{}, runtime.ServerAuth{Password: "secret"}, Controls{
		Store: store, Fencer: fencer, Publisher: publisher,
	}, testLogger())
	request := httptest.NewRequest(http.MethodPost, "/fern/api/v1/workflows/"+workflow.ID+"/publish", strings.NewReader(`{}`))
	request.Host = "fern.example"
	request.Header.Set("Origin", "https://evil.example")
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth("opencode", "secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || fencer.acquired.Load() != 0 || publisher.calls.Load() != 0 {
		t.Fatalf("status=%d acquired=%d publishes=%d", response.Code, fencer.acquired.Load(), publisher.calls.Load())
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
	fencer := &fakePublicationFencer{}
	publisher := &fakeGitHubPublisher{}
	request := httptest.NewRequest(http.MethodPost, "/fern/api/v1/workflows/"+workflow.ID+"/publish", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	NewWithControls(&countingWaker{}, runtime.ServerAuth{}, Controls{Store: store, Fencer: fencer, Publisher: publisher}, testLogger()).ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || fencer.acquired.Load() != 0 || publisher.calls.Load() != 0 {
		t.Fatalf("status=%d acquired=%d publishes=%d", response.Code, fencer.acquired.Load(), publisher.calls.Load())
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
	fencer := &fakePublicationFencer{err: errors.New("busy")}
	publisher := &fakeGitHubPublisher{}
	request := httptest.NewRequest(http.MethodPost, "/fern/api/v1/workflows/"+workflow.ID+"/publish", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth("opencode", "secret")
	response := httptest.NewRecorder()
	NewWithControls(&countingWaker{}, runtime.ServerAuth{Password: "secret"}, Controls{Store: store, Fencer: fencer, Publisher: publisher}, testLogger()).ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || publisher.calls.Load() != 0 {
		t.Fatalf("status=%d publishes=%d", response.Code, publisher.calls.Load())
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
	fencer := &fakePublicationFencer{}
	publisher := &fakeGitHubPublisher{}
	request := httptest.NewRequest(http.MethodPost, "/fern/api/v1/workflows/"+workflow.ID+"/publish", strings.NewReader(`{"operation":"../unsafe"}`))
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth("opencode", "secret")
	response := httptest.NewRecorder()
	NewWithControls(&countingWaker{}, runtime.ServerAuth{Password: "secret"}, Controls{Store: store, Fencer: fencer, Publisher: publisher}, testLogger()).ServeHTTP(response, request)
	updated, _ := store.Workflow(workflow.ID)
	if response.Code != http.StatusUnprocessableEntity || updated.PublicationID != "" || fencer.acquired.Load() != 0 || publisher.calls.Load() != 0 {
		t.Fatalf("status=%d workflow=%+v acquired=%d publishes=%d", response.Code, updated, fencer.acquired.Load(), publisher.calls.Load())
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
	if _, err := store.FinishPublication(publicationRecord.ID, "", "publication failed", time.Now()); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/fern/", nil)
	request.SetBasicAuth("opencode", "secret")
	response := httptest.NewRecorder()
	NewWithControls(nil, runtime.ServerAuth{Password: "secret"}, Controls{
		Store: store, Fencer: &fakePublicationFencer{}, Publisher: &fakeGitHubPublisher{},
	}, testLogger()).ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Retry publication") {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestStartupReconcilesPreparedPublicationFromReopenedStore(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "control")
	store, err := control.Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	workflow, err := store.CreateWorkflow("Fix signup", "ses_123", now)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := store.RequestPublication(workflow.ID, control.Publication{
		ID: "publication-1", Operation: "operation", Title: "Fix signup",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	if err := store.PreparePublication(record.ID, "owner/repo", "main", "fern/demo/operation", commit, now); err != nil {
		t.Fatal(err)
	}
	reopened, err := control.Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	publisher := &recoveryGitHubPublisher{prepared: make(chan publication.Prepared, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ReconcilePublications(ctx, Controls{Store: reopened, Fencer: &fakePublicationFencer{}, Publisher: publisher}, testLogger())
	select {
	case prepared := <-publisher.prepared:
		if prepared.Commit != commit || prepared.Branch != "fern/demo/operation" {
			t.Fatalf("reconciled target = %+v", prepared)
		}
	case <-time.After(time.Second):
		t.Fatal("startup reconciliation did not run")
	}
	deadline := time.Now().Add(time.Second)
	for {
		published, _ := reopened.Publication(record.ID)
		if published.State == "published" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("publication did not settle: %+v", published)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestControlMutationOriginPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{name: "non-browser client", host: "fern.example", want: true},
		{name: "same HTTPS origin", origin: "https://fern.example", host: "fern.example", want: true},
		{name: "same HTTP origin", origin: "http://127.0.0.1:8080", host: "127.0.0.1:8080", want: true},
		{name: "cross origin", origin: "https://evil.example", host: "fern.example"},
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
			if got := sameOrigin(request); got != test.want {
				t.Fatalf("sameOrigin() = %t, want %t", got, test.want)
			}
		})
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
	request.SetBasicAuth("opencode", "secret")
	response := httptest.NewRecorder()
	NewWithControl(nil, runtime.ServerAuth{Password: "secret"}, store, testLogger()).ServeHTTP(response, request)
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
