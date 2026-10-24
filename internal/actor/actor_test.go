package actor_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"
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
	go func() {
		if err := g.Run(ctx); err != nil {
			t.Errorf("Run() error: %v", err)
		}
		close(done)
	}()
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

// guard wraps a store and a driver and records an error when two calls for
// the same environment overlap: an actor must make them one at a time.
type guard struct {
	*fakeStore
	drv *driver.Fake

	mu       sync.Mutex
	busy     map[string]bool
	overlaps int
}

// enter marks id busy and lets an overlapping call run; leave ends it.
func (g *guard) enter(id string) {
	g.mu.Lock()
	if g.busy[id] {
		g.overlaps++
	}
	g.busy[id] = true
	g.mu.Unlock()
	runtime.Gosched()
}

func (g *guard) leave(id string) {
	g.mu.Lock()
	g.busy[id] = false
	g.mu.Unlock()
}

func (g *guard) Create(ctx context.Context, id string, opts store.CreateOptions) (store.Env, error) {
	g.enter(id)
	defer g.leave(id)
	return g.fakeStore.Create(ctx, id, opts)
}

func (g *guard) Delete(ctx context.Context, id string) error {
	g.enter(id)
	defer g.leave(id)
	return g.fakeStore.Delete(ctx, id)
}

func (g *guard) Snapshot(ctx context.Context, id, snapID string) error {
	g.enter(id)
	defer g.leave(id)
	return g.fakeStore.Snapshot(ctx, id, snapID)
}

func (g *guard) Restore(ctx context.Context, id, snapID string) error {
	g.enter(id)
	defer g.leave(id)
	return g.fakeStore.Restore(ctx, id, snapID)
}

// guardDriver is guard's driver side: its Delete deletes the box.
type guardDriver struct{ *guard }

func (d guardDriver) Wake(ctx context.Context, env store.Env) (string, error) {
	d.enter(env.ID)
	defer d.leave(env.ID)
	return d.drv.Wake(ctx, env)
}

func (d guardDriver) Sleep(ctx context.Context, id string) error {
	d.enter(id)
	defer d.leave(id)
	return d.drv.Sleep(ctx, id)
}

func (d guardDriver) Delete(ctx context.Context, id string) error {
	d.enter(id)
	defer d.leave(id)
	return d.drv.Delete(ctx, id)
}

func (d guardDriver) States(ctx context.Context) (map[string]driver.State, error) {
	return d.drv.States(ctx)
}

// E5: 50 goroutines make random changes and run commands on three
// environments at once. No two changes of one environment overlap, every
// call ends, and afterwards each environment that exists still works.
func TestConcurrentChanges(t *testing.T) {
	g := &guard{fakeStore: newFakeStore(), drv: driver.NewFake("box"), busy: make(map[string]bool)}
	envs := actor.New(g, guardDriver{g}, actor.Idle{}, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		if err := envs.Run(ctx); err != nil {
			t.Errorf("Run() error: %v", err)
		}
		close(done)
	}()
	defer func() { cancel(); <-done }()

	ids := []string{"a", "b", "c"}
	for _, id := range ids {
		if err := envs.Create(ctx, id, store.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := envs.Snapshot(ctx, id, "s", false); err != nil {
			t.Fatal(err)
		}
	}
	changes := []func(id string) error{
		func(id string) error { return envs.Snapshot(ctx, id, "s", true) },
		func(id string) error { return envs.Restore(ctx, id, "s") },
		func(id string) error { return envs.Sleep(ctx, id) },
		func(id string) error { return envs.Wake(ctx, id) },
		func(id string) error {
			ex, err := envs.Exec(ctx, id, "", func() {})
			if err != nil {
				return err
			}
			runtime.Gosched()
			ex.Close()
			return nil
		},
		func(id string) error {
			if err := envs.Delete(ctx, id); err != nil {
				return err
			}
			if err := envs.Create(ctx, id, store.CreateOptions{}); err != nil {
				return err
			}
			return envs.Snapshot(ctx, id, "s", false)
		},
	}
	var wg sync.WaitGroup
	for w := range 50 {
		wg.Go(func() {
			for i := range 40 {
				id := ids[(w+i)%len(ids)]
				// A delete and create of the same environment in another
				// goroutine can come between: not found is a fair answer.
				if err := changes[(w*7+i)%len(changes)](id); err != nil && !errors.Is(err, store.ErrNotFound) {
					t.Errorf("change %d of %s: %v", (w*7+i)%len(changes), id, err)
				}
			}
		})
	}
	wg.Wait()
	g.mu.Lock()
	overlaps := g.overlaps
	g.mu.Unlock()
	if overlaps != 0 {
		t.Errorf("%d calls overlapped another of the same environment", overlaps)
	}
	for _, id := range ids {
		if err := envs.Create(ctx, id, store.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		ex, err := envs.Exec(ctx, id, "", func() {})
		if err != nil {
			t.Errorf("Exec(%s) after the storm error: %v", id, err)
			continue
		}
		ex.Close()
	}
}

// The Group counts wakes, snapshots, restores and idle sleeps.
func TestMetrics(t *testing.T) {
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
		exec(t, g, time.Second)
		time.Sleep(idle.Human)
		synctest.Wait()
		if err := g.Restore(ctx, "demo", "s1"); err != nil {
			t.Fatal(err)
		}
		reg := prometheus.NewRegistry()
		reg.MustRegister(g)
		families, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		got := make(map[string]uint64)
		for _, f := range families {
			m := f.GetMetric()[0]
			if h := m.GetHistogram(); h != nil {
				got[f.GetName()] = h.GetSampleCount()
			} else {
				got[f.GetName()] = uint64(m.GetCounter().GetValue())
			}
		}
		want := map[string]uint64{"fern_wake_seconds": 1, "fern_snapshot_seconds": 1, "fern_restore_seconds": 1, "fern_idle_sleeps_total": 1}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("metrics mismatch (-want +got):\n%s", diff)
		}
	})
}

// slowWake is a driver whose Wake waits until release is closed.
type slowWake struct {
	*driver.Fake
	release chan struct{}
}

func (d *slowWake) Wake(ctx context.Context, env store.Env) (string, error) {
	<-d.release
	return d.Fake.Wake(ctx, env)
}

// A caller that gives up while the box wakes runs no command, so the box
// still goes to sleep when idle.
func TestExecGivenUpDuringWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, fake := newFakeStore(), driver.NewFake("box")
		drv := &slowWake{Fake: fake, release: make(chan struct{})}
		g := actor.New(st, drv, idle, slog.New(slog.DiscardHandler))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { _ = g.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		if err := g.Create(ctx, "demo", store.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		execCtx, giveUp := context.WithCancel(ctx)
		result := make(chan error, 1)
		go func() {
			_, err := g.Exec(execCtx, "demo", "", giveUp)
			result <- err
		}()
		synctest.Wait() // the wake has started
		giveUp()
		close(drv.release)
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("Exec() given up error = %v, want %v", err, context.Canceled)
		}
		time.Sleep(idle.Human)
		synctest.Wait()
		if got := state(t, fake, "demo"); got != driver.Asleep {
			t.Errorf("after %v: state %s, want %s", idle.Human, got, driver.Asleep)
		}
	})
}
