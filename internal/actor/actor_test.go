package actor_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/goleak"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/driver"
	"github.com/nebler/fern/internal/store"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fakeStore keeps environments and snapshot names in memory.
type fakeStore struct {
	mu    sync.Mutex
	envs  map[string]bool
	snaps map[string][]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{envs: make(map[string]bool), snaps: make(map[string][]string)}
}

func (f *fakeStore) Create(_ context.Context, id string, _ store.CreateOptions) (store.Env, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.envs[id] = true
	return store.Env{ID: id}, nil
}

func (f *fakeStore) Get(_ context.Context, id string) (store.Env, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.envs[id] {
		return store.Env{}, fmt.Errorf("environment %s: %w", id, store.ErrNotFound)
	}
	return store.Env{ID: id}, nil
}

func (f *fakeStore) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.envs, id)
	delete(f.snaps, id)
	return nil
}

func (f *fakeStore) Snapshot(_ context.Context, id, snapID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snaps[id] = append(f.snaps[id], snapID)
	return nil
}

func (f *fakeStore) DeleteSnapshot(_ context.Context, id, snapID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snaps[id] = slices.DeleteFunc(f.snaps[id], func(s string) bool { return s == snapID })
	return nil
}

func (f *fakeStore) Snapshots(_ context.Context, id string) ([]store.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Snapshot
	for _, s := range f.snaps[id] {
		out = append(out, store.Snapshot{ID: s})
	}
	return out, nil
}

func (f *fakeStore) Restore(context.Context, string, string) error { return nil }

var idle = actor.Idle{Agent: 5 * time.Minute, Human: 20 * time.Minute}

// start runs a Group for st and drv until the test ends. Call it inside a
// synctest bubble.
func start(t *testing.T, st *fakeStore, drv *driver.Fake, idle actor.Idle) *actor.Group {
	t.Helper()
	g := actor.New(st, drv, idle, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { g.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return g
}

func state(t *testing.T, drv *driver.Fake, id string) driver.State {
	t.Helper()
	states, err := drv.States(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return states[id]
}

// exec counts a command in demo's box from now for d.
func exec(t *testing.T, g *actor.Group, d time.Duration) {
	t.Helper()
	ex, err := g.Exec(t.Context(), "demo", "", func() {})
	if err != nil {
		t.Fatalf("Exec(demo) error: %v", err)
	}
	time.Sleep(d)
	ex.Close()
}

// The box sleeps when both idle times have passed: 5 minutes after the last
// command ended and 20 after it started.
func TestIdleSleep(t *testing.T) {
	tests := []struct {
		name string
		ran  time.Duration // how long the command ran
		want time.Duration // when the box sleeps, from the command's start
	}{
		{"short command", time.Minute, 20 * time.Minute},
		{"long command", 30 * time.Minute, 35 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st, drv := newFakeStore(), driver.NewFake("box")
				g := start(t, st, drv, idle)
				if err := g.Create(t.Context(), "demo", store.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				exec(t, g, tt.ran)
				time.Sleep(tt.want - tt.ran - time.Second)
				synctest.Wait()
				if got := state(t, drv, "demo"); got != driver.Running {
					t.Fatalf("a second before %v: state %s, want %s", tt.want, got, driver.Running)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if got := state(t, drv, "demo"); got != driver.Asleep {
					t.Errorf("at %v: state %s, want %s", tt.want, got, driver.Asleep)
				}
			})
		})
	}
}

// A box with a command running never sleeps, and neither does one when the
// idle times are zero.
func TestNoIdleSleep(t *testing.T) {
	tests := []struct {
		name string
		idle actor.Idle
		open bool
	}{
		{"command running", idle, true},
		{"idle times zero", actor.Idle{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st, drv := newFakeStore(), driver.NewFake("box")
				g := start(t, st, drv, tt.idle)
				if err := g.Create(t.Context(), "demo", store.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				ex, err := g.Exec(t.Context(), "demo", "", func() {})
				if err != nil {
					t.Fatal(err)
				}
				if !tt.open {
					ex.Close()
				}
				time.Sleep(24 * time.Hour)
				synctest.Wait()
				if got := state(t, drv, "demo"); got != driver.Running {
					t.Errorf("after a day: state %s, want %s", got, driver.Running)
				}
				ex.Close()
			})
		})
	}
}

// A box an earlier daemon left running goes to sleep when idle too.
func TestIdleSleepAfterRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, drv := newFakeStore(), driver.NewFake("box")
		if _, err := drv.Wake(t.Context(), store.Env{ID: "demo"}); err != nil {
			t.Fatal(err)
		}
		start(t, st, drv, idle)
		time.Sleep(idle.Human)
		synctest.Wait()
		if got := state(t, drv, "demo"); got != driver.Asleep {
			t.Errorf("after %v: state %s, want %s", idle.Human, got, driver.Asleep)
		}
	})
}

// A restore, sleep or delete cancels the commands running in the box, and a
// restore of a snapshot that doesn't exist leaves them alone.
func TestStopCancelsExecs(t *testing.T) {
	tests := []struct {
		name       string
		stop       func(ctx context.Context, g *actor.Group) error
		wantCancel bool
	}{
		{"restore", func(ctx context.Context, g *actor.Group) error { return g.Restore(ctx, "demo", "s1") }, true},
		{"sleep", func(ctx context.Context, g *actor.Group) error { return g.Sleep(ctx, "demo") }, true},
		{"delete", func(ctx context.Context, g *actor.Group) error { return g.Delete(ctx, "demo") }, true},
		{"restore unknown", func(ctx context.Context, g *actor.Group) error {
			if err := g.Restore(ctx, "demo", "typo"); !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("Restore(typo) error = %w, want %w", err, store.ErrNotFound)
			}
			return nil
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st, drv := newFakeStore(), driver.NewFake("box")
				g := start(t, st, drv, idle)
				ctx := t.Context()
				if err := g.Create(ctx, "demo", store.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				if err := g.Snapshot(ctx, "demo", "s1", false); err != nil {
					t.Fatal(err)
				}
				cmdCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				ex, err := g.Exec(ctx, "demo", "", cancel)
				if err != nil {
					t.Fatal(err)
				}
				defer ex.Close()
				if err := tt.stop(ctx, g); err != nil {
					t.Fatal(err)
				}
				if got := cmdCtx.Err() != nil; got != tt.wantCancel {
					t.Errorf("command cancelled = %t, want %t", got, tt.wantCancel)
				}
			})
		})
	}
}

// An exec of an environment that doesn't exist fails and wakes nothing.
func TestExecUnknown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, drv := newFakeStore(), driver.NewFake("box")
		g := start(t, st, drv, idle)
		if _, err := g.Exec(t.Context(), "nope", "", func() {}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Exec(nope) error = %v, want %v", err, store.ErrNotFound)
		}
		if n := drv.Wakes(); n != 0 {
			t.Errorf("Wakes() = %d, want 0", n)
		}
	})
}
