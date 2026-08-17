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
