package publication

import (
	"context"
	"errors"
	"sync"
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

func (store *coordinatorStore) PreparePublication(_, repository, base, branch, commit string, _ time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.events = append(store.events, "persist")
	store.record.Repository, store.record.Base = repository, base
	store.record.Branch, store.record.Commit, store.record.State = branch, commit, "pushing"
	return nil
}

func (store *coordinatorStore) FinishPublication(_ string, pullURL, failure string, _ time.Time) (control.Publication, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.events = append(store.events, "finish")
	store.record.PullURL, store.record.Error = pullURL, failure
	if failure == "" {
		store.record.State = "published"
	} else {
		store.record.State = "failed"
	}
	return store.record, nil
}

type coordinatorFencer struct{}

func (coordinatorFencer) AcquirePaused(context.Context) (func(), error) { return func() {}, nil }

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
	return Prepared{Repository: "owner/repo", Base: "main", Branch: "fern/demo/op", Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, nil
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
	return Result{Prepared: prepared, URL: "https://github.com/owner/repo/pull/1"}, nil
}

func TestCoordinatorPersistsPreparedBeforePublishing(t *testing.T) {
	store := &coordinatorStore{record: control.Publication{ID: "pub", State: "requested", Operation: "op", Title: "title"}}
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
	prepared := Prepared{Repository: "owner/repo", Base: "main", Branch: "fern/demo/op", Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	store := &coordinatorStore{record: control.Publication{ID: "pub", State: "pushing", Title: "title", Repository: prepared.Repository, Base: prepared.Base, Branch: prepared.Branch, Commit: prepared.Commit}}
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
	store := &coordinatorStore{record: control.Publication{ID: "pub", State: "pushing", Title: "title", Repository: "owner/repo", Base: "main", Branch: "fern/demo/op", Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
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
