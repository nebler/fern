package actor_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
)

var prewakeIdle = actor.Idle{Agent: 5 * time.Minute, Human: 20 * time.Minute, Prewake: 2 * time.Minute}

// prewakes returns the pre-wake counts in reg by result.
func prewakes(t *testing.T, reg prometheus.Gatherer) map[string]uint64 {
	t.Helper()
	out := make(map[string]uint64)
	for name, n := range counts(t, reg) {
		var result string
		if _, err := fmt.Sscanf(name, "fern_prewakes_total{result=%s", &result); err == nil {
			out[result[:len(result)-1]] = n
		}
	}
	return out
}

// wantState fails the test unless demo's box is in state want after d more.
func wantState(t *testing.T, drv *runsc.Fake, d time.Duration, want runsc.State) {
	t.Helper()
	time.Sleep(d)
	synctest.Wait()
	if got := state(t, drv, "demo"); got != want {
		t.Fatalf("state %s, want %s", got, want)
	}
}

// prewake asks for demo's box ahead and waits for the pre-wake to be made.
func prewake(t *testing.T, g *actor.Group) {
	t.Helper()
	if err := g.Prewake("demo"); err != nil {
		t.Fatalf("Prewake(demo) error: %v", err)
	}
	synctest.Wait()
}

// A box woken ahead that no command comes to sleeps after Idle.Prewake; one
// a command comes to has the usual idle times from that command.
func TestPrewake(t *testing.T) {
	t.Run("no command", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			st, drv := newFakeStore(), runsc.NewFake("box")
			g, reg := startMetered(t, st, drv, prewakeIdle)
			if err := g.Create(t.Context(), "demo", store.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			prewake(t, g)
			wantState(t, drv, 0, runsc.Running)
			wantState(t, drv, prewakeIdle.Prewake-time.Second, runsc.Running)
			wantState(t, drv, time.Second, runsc.Asleep)
			if diff := cmp.Diff(map[string]uint64{"woken": 1, "wasted": 1}, prewakes(t, reg)); diff != "" {
				t.Errorf("pre-wakes mismatch (-want +got):\n%s", diff)
			}
		})
	})
	t.Run("a command comes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			st, drv := newFakeStore(), runsc.NewFake("box")
			g, reg := startMetered(t, st, drv, prewakeIdle)
			if err := g.Create(t.Context(), "demo", store.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			prewake(t, g)
			time.Sleep(time.Minute)
			exec(t, g, time.Second)
			if got := drv.Wakes(); got != 1 {
				t.Errorf("%d wakes, want 1: the command uses the box woken ahead", got)
			}
			// Asleep 20 minutes after the command started, not 2 after the wake.
			wantState(t, drv, prewakeIdle.Human-2*time.Second, runsc.Running)
			wantState(t, drv, time.Second, runsc.Asleep)
			if diff := cmp.Diff(map[string]uint64{"woken": 1, "used": 1}, prewakes(t, reg)); diff != "" {
				t.Errorf("pre-wakes mismatch (-want +got):\n%s", diff)
			}
		})
	})
	t.Run("a restore comes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			st, drv := newFakeStore(), runsc.NewFake("box")
			g, reg := startMetered(t, st, drv, prewakeIdle)
			ctx := t.Context()
			if err := g.Create(ctx, "demo", store.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := g.Snapshot(ctx, "demo", "s1", false); err != nil {
				t.Fatal(err)
			}
			prewake(t, g)
			if err := g.Restore(ctx, "demo", "s1"); err != nil {
				t.Fatal(err)
			}
			wantState(t, drv, 0, runsc.Asleep)
			wantState(t, drv, prewakeIdle.Prewake, runsc.Asleep)
			if diff := cmp.Diff(map[string]uint64{"woken": 1, "wasted": 1}, prewakes(t, reg)); diff != "" {
				t.Errorf("pre-wakes mismatch (-want +got):\n%s", diff)
			}
		})
	})
}

// A pre-wake leaves a box that is awake as it is: it doesn't move its
// idle times, earlier or later.
func TestPrewakeAwake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, drv := newFakeStore(), runsc.NewFake("box")
		g, reg := startMetered(t, st, drv, prewakeIdle)
		if err := g.Create(t.Context(), "demo", store.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		exec(t, g, time.Minute)
		time.Sleep(18 * time.Minute) // the box sleeps 20 minutes after the command started
		prewake(t, g)
		wantState(t, drv, time.Minute-time.Second, runsc.Running)
		wantState(t, drv, time.Second, runsc.Asleep)
		if diff := cmp.Diff(map[string]uint64{"awake": 1}, prewakes(t, reg)); diff != "" {
			t.Errorf("pre-wakes mismatch (-want +got):\n%s", diff)
		}
	})
}

// A pre-wake of an environment that doesn't exist, or with pre-wakes or
// idle sleep off, wakes nothing; a bad ID is an error.
func TestPrewakeNothing(t *testing.T) {
	tests := []struct {
		name   string
		idle   actor.Idle
		create bool
		want   map[string]uint64
	}{
		{"unknown environment", prewakeIdle, false, map[string]uint64{"unknown": 1}},
		{"pre-wakes off", idle, true, map[string]uint64{}},
		{"idle sleep off", actor.Idle{Prewake: time.Minute}, true, map[string]uint64{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st, drv := newFakeStore(), runsc.NewFake("box")
				g, reg := startMetered(t, st, drv, tt.idle)
				if tt.create {
					if err := g.Create(t.Context(), "demo", store.CreateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				prewake(t, g)
				if got := drv.Wakes(); got != 0 {
					t.Errorf("%d wakes, want 0", got)
				}
				if diff := cmp.Diff(tt.want, prewakes(t, reg)); diff != "" {
					t.Errorf("pre-wakes mismatch (-want +got):\n%s", diff)
				}
				if err := g.Prewake("../demo"); err == nil {
					t.Error("Prewake(../demo) error = nil, want one")
				}
			})
		})
	}
}

// blockedWake is a driver whose Wake waits until release is closed, and
// counts the wakes waiting.
type blockedWake struct {
	*runsc.Fake
	release chan struct{}
	mu      sync.Mutex
	waiting int
}

func (d *blockedWake) Wake(ctx context.Context, box runsc.Box) (string, error) {
	d.mu.Lock()
	d.waiting++
	d.mu.Unlock()
	<-d.release
	return d.Fake.Wake(ctx, box)
}

// Two pre-wakes run at once and eight wait; more are dropped. A command
// doesn't wait for another environment's pre-wake.
func TestPrewakeLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, fake := newFakeStore(), runsc.NewFake("box")
		drv := &blockedWake{Fake: fake, release: make(chan struct{})}
		reg := prometheus.NewRegistry()
		g := actor.New(st, drv, actor.Config{Idle: prewakeIdle, Image: "box"}, notrace, reg, slog.New(slog.DiscardHandler))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { _ = g.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		const n = 2 + 8 + 1
		for i := range n + 1 {
			if err := g.Create(ctx, fmt.Sprint("env", i), store.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		for i := range n {
			if err := g.Prewake(fmt.Sprint("env", i)); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
		}
		drv.mu.Lock()
		waiting := drv.waiting
		drv.mu.Unlock()
		if waiting != 2 {
			t.Errorf("%d wakes at once, want 2", waiting)
		}
		// A command's wake is not held up by the queue.
		execDone := make(chan error, 1)
		go func() {
			ex, err := g.Exec(ctx, fmt.Sprint("env", n), "", func() {})
			if err == nil {
				ex.Close()
			}
			execDone <- err
		}()
		synctest.Wait()
		drv.mu.Lock()
		waiting = drv.waiting
		drv.mu.Unlock()
		if waiting != 3 {
			t.Errorf("%d wakes at once with a command's, want 3", waiting)
		}
		close(drv.release)
		if err := <-execDone; err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if diff := cmp.Diff(map[string]uint64{"woken": n - 1, "dropped": 1}, prewakes(t, reg)); diff != "" {
			t.Errorf("pre-wakes mismatch (-want +got):\n%s", diff)
		}
	})
}

// failingWake is a driver whose Wake fails, perhaps leaving the box
// running, and that counts the stops.
type failingWake struct {
	*runsc.Fake
	mu     sync.Mutex
	sleeps int
}

func (d *failingWake) Wake(context.Context, runsc.Box) (string, error) {
	return "", errors.New("no node")
}

func (d *failingWake) Sleep(ctx context.Context, id string) error {
	d.mu.Lock()
	d.sleeps++
	d.mu.Unlock()
	return d.Fake.Sleep(ctx, id)
}

// A pre-wake whose wake fails counts as an error only, and the box it may
// have left running still sleeps after Idle.Prewake.
func TestPrewakeFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newFakeStore()
		drv := &failingWake{Fake: runsc.NewFake("box")}
		reg := prometheus.NewRegistry()
		g := actor.New(st, drv, actor.Config{Idle: prewakeIdle, Image: "box"}, notrace, reg, slog.New(slog.DiscardHandler))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { _ = g.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		if err := g.Create(ctx, "demo", store.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		prewake(t, g)
		time.Sleep(prewakeIdle.Prewake)
		synctest.Wait()
		drv.mu.Lock()
		defer drv.mu.Unlock()
		if got := drv.sleeps; got != 1 {
			t.Errorf("%d sleeps after %v, want 1", got, prewakeIdle.Prewake)
		}
		if diff := cmp.Diff(map[string]uint64{"error": 1}, prewakes(t, reg)); diff != "" {
			t.Errorf("pre-wakes mismatch (-want +got):\n%s", diff)
		}
	})
}
