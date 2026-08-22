package publication

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nebler/fern/internal/control"
)

type coordinatorStore struct {
	mu     sync.Mutex
	record control.Publication
	events []string
}

func (store *coordinatorStore) Publication(id string) (control.Publication, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.record, store.record.ID == id
}

func (store *coordinatorStore) Publications() []control.Publication {
	store.mu.Lock()
	defer store.mu.Unlock()
	return []control.Publication{store.record}
}

func (store *coordinatorStore) PreparePublication(_ string, prepared control.PreparedPublication, _ time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.events = append(store.events, "persist")
	store.record.RepositoryID, store.record.RepositoryFullName = prepared.RepositoryID, prepared.RepositoryFullName
	store.record.BaseSHA, store.record.BaseRef = prepared.BaseSHA, prepared.BaseRef
	store.record.Branch, store.record.ResultCommit, store.record.State = prepared.Branch, prepared.ResultCommit, "pushing"
	return nil
}

func (store *coordinatorStore) FinishPublication(_ string, pullRequest *control.PullRequestObservation, failure string, _ time.Time) (control.Publication, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.events = append(store.events, "finish")
	store.record.PullRequest, store.record.Error = pullRequest, failure
	if pullRequest != nil {
		store.record.PullURL = pullRequest.URL
	}
	if failure == "" {
		store.record.State = "published"
	} else {
		store.record.State = "failed"
	}
	return store.record, nil
}

type coordinatorFencer struct{}

func (coordinatorFencer) AcquirePaused(context.Context) (func(), error) { return func() {}, nil }

type countingCoordinatorFencer struct{ calls atomic.Int32 }

func (fencer *countingCoordinatorFencer) AcquirePaused(context.Context) (func(), error) {
	fencer.calls.Add(1)
	return func() {}, nil
}

type coordinatorBackend struct {
	store        *coordinatorStore
	prepareCalls int
	published    chan Prepared
	block        bool
}

func (backend *coordinatorBackend) Prepare(context.Context, Request) (Prepared, error) {
	backend.prepareCalls++
	backend.store.mu.Lock()
	backend.store.events = append(backend.store.events, "prepare")
	backend.store.mu.Unlock()
	return coordinatorPrepared(), nil
}

func (backend *coordinatorBackend) PublishPrepared(ctx context.Context, prepared Prepared, _, _ string) (Result, error) {
	backend.store.mu.Lock()
	backend.store.events = append(backend.store.events, "publish")
	backend.store.mu.Unlock()
	if backend.published != nil {
		backend.published <- prepared
	}
	if backend.block {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	return Result{Prepared: prepared, PullRequest: coordinatorPullRequest(prepared)}, nil
}

func TestCoordinatorPersistsPreparedBeforePublishing(t *testing.T) {
	store := &coordinatorStore{record: control.Publication{SchemaVersion: control.PublicationSchemaVersion, ID: "pub", State: "requested", Operation: "op", Title: "title"}}
	backend := &coordinatorBackend{store: store}
	coordinator := NewCoordinator(context.Background(), store, coordinatorFencer{}, backend)
	defer coordinator.Close(context.Background())

	result, err := coordinator.Execute(context.Background(), "pub")
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "published" {
		t.Fatalf("result = %+v", result)
	}
	store.mu.Lock()
	events := append([]string(nil), store.events...)
	store.mu.Unlock()
	want := []string{"prepare", "persist", "publish", "finish"}
	if len(events) != len(want) {
		t.Fatalf("events = %v", events)
	}
	for index := range want {
		if events[index] != want[index] {
			t.Fatalf("events = %v", events)
		}
	}
}

func TestCoordinatorRetriesExactPreparedRecord(t *testing.T) {
	prepared := coordinatorPrepared()
	store := &coordinatorStore{record: control.Publication{SchemaVersion: control.PublicationSchemaVersion, ID: "pub", State: "pushing", Title: "title", RepositoryID: prepared.RepositoryID, RepositoryFullName: prepared.RepositoryFullName, BaseSHA: prepared.BaseSHA, BaseRef: prepared.BaseRef, Branch: prepared.Branch, ResultCommit: prepared.ResultCommit}}
	published := make(chan Prepared, 1)
	backend := &coordinatorBackend{store: store, published: published}
	coordinator := NewCoordinator(context.Background(), store, coordinatorFencer{}, backend)
	defer coordinator.Close(context.Background())

	if _, err := coordinator.Execute(context.Background(), "pub"); err != nil {
		t.Fatal(err)
	}
	if backend.prepareCalls != 0 {
		t.Fatalf("Prepare called %d times", backend.prepareCalls)
	}
	if got := <-published; got != prepared {
		t.Fatalf("published %+v, want %+v", got, prepared)
	}
}

func TestCoordinatorCloseCancelsAndWaitsForWorker(t *testing.T) {
	prepared := coordinatorPrepared()
	store := &coordinatorStore{record: control.Publication{SchemaVersion: control.PublicationSchemaVersion, ID: "pub", State: "pushing", Title: "title", RepositoryID: prepared.RepositoryID, RepositoryFullName: prepared.RepositoryFullName, BaseSHA: prepared.BaseSHA, BaseRef: prepared.BaseRef, Branch: prepared.Branch, ResultCommit: prepared.ResultCommit}}
	started := make(chan Prepared, 1)
	backend := &coordinatorBackend{store: store, published: started, block: true}
	coordinator := NewCoordinator(context.Background(), store, coordinatorFencer{}, backend)
	done := make(chan error, 1)
	go func() {
		_, err := coordinator.Execute(context.Background(), "pub")
		done <- err
	}()
	<-started
	if err := coordinator.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("worker error = %v", err)
	}
	if _, err := coordinator.Execute(context.Background(), "pub"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Execute after Close error = %v", err)
	}
}

func TestCoordinatorBlocksLegacyEffectsButDisplaysTerminalRecord(t *testing.T) {
	fencer := &countingCoordinatorFencer{}
	legacy := control.Publication{ID: "pub", State: control.PublicationPrepared, Operation: "op", Title: "old", Repository: "owner/repo", Base: "main", Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Branch: "fern/demo/op"}
	store := &coordinatorStore{record: legacy}
	backend := &coordinatorBackend{store: store}
	coordinator := NewCoordinator(context.Background(), store, fencer, backend)
	defer coordinator.Close(context.Background())

	if err := coordinator.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Execute(context.Background(), legacy.ID); err == nil {
		t.Fatalf("legacy execution error = %v", err)
	}
	if fencer.calls.Load() != 0 || backend.prepareCalls != 0 {
		t.Fatalf("legacy record reached fencer/backend: fence=%d prepare=%d", fencer.calls.Load(), backend.prepareCalls)
	}

	store.mu.Lock()
	store.record.State = control.PublicationPublished
	store.mu.Unlock()
	got, err := coordinator.Execute(context.Background(), legacy.ID)
	if err != nil || got.State != control.PublicationPublished || fencer.calls.Load() != 0 {
		t.Fatalf("terminal legacy record=%+v err=%v fence=%d", got, err, fencer.calls.Load())
	}
}

func coordinatorPrepared() Prepared {
	return Prepared{
		RepositoryID: 123, RepositoryFullName: "owner/repo",
		BaseSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", BaseRef: "main",
		ResultCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Branch: "fern/demo/op",
	}
}

func coordinatorPullRequest(prepared Prepared) PullRequestObservation {
	return PullRequestObservation{
		TargetRepositoryID: prepared.RepositoryID, TargetRepositoryFullName: prepared.RepositoryFullName,
		Number: 1, URL: "https://github.com/owner/repo/pull/1", State: "open", Draft: true,
		Base: PullRequestRefObservation{RepositoryID: prepared.RepositoryID, RepositoryFullName: prepared.RepositoryFullName, RepositoryOwner: "owner", RepositoryName: "repo", Ref: prepared.BaseRef, SHA: prepared.BaseSHA},
		Head: PullRequestRefObservation{RepositoryID: prepared.RepositoryID, RepositoryFullName: prepared.RepositoryFullName, RepositoryOwner: "owner", RepositoryName: "repo", Ref: prepared.Branch, SHA: prepared.ResultCommit},
	}
}
