package actor_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
)

// A box that isn't woken for now is listed, with how often it failed, why
// the last time and when it is tried again: while its actor runs, after the
// actor has ended, and no longer once the window has passed, a person has
// woken it, or its environment is deleted.
func TestCrashing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, drv := newFakeStore(), runsc.NewFake("box")
		g := start(t, st, drv, idle)
		ctx := t.Context()
		for _, id := range []string{"demo", "fine", "gone", "mended"} {
			if err := g.Create(ctx, id, store.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		crashing := func() []actor.Crashing {
			t.Helper()
			got, err := g.Crashing(ctx)
			if err != nil {
				t.Fatalf("Crashing() error: %v", err)
			}
			return got
		}
		// fail has box id fail to wake at a command.
		fail := func(id string) {
			t.Helper()
			if _, err := g.Exec(ctx, id, func() {}); err == nil {
				t.Fatalf("Exec(%s) worked, want its wake to fail", id)
			}
		}
		// A box that is well, with a command running.
		fine, err := g.Exec(ctx, "fine", func() {})
		if err != nil {
			t.Fatal(err)
		}
		defer fine.Close()
		if got := crashing(); len(got) != 0 {
			t.Fatalf("Crashing() with nothing wrong = %+v, want none", got)
		}

		for _, id := range []string{"demo", "gone", "mended"} {
			drv.FailWakes(id, errors.New("runsc: boom"))
		}
		t0 := time.Now()
		for i := range 3 {
			if i == 2 {
				if got := crashing(); len(got) != 0 {
					t.Fatalf("Crashing() after two failed wakes = %+v, want none: the box is still tried", got)
				}
				drv.FailWakes("demo", errors.New("postgres: out of memory"))
			}
			for _, id := range []string{"demo", "gone", "mended"} {
				fail(id)
			}
			time.Sleep(time.Minute)
		}
		demo := actor.Crashing{
			Box: "demo", Times: 3, Since: t0, Last: t0.Add(2 * time.Minute), Cause: "postgres: out of memory",
			Retry: t0.Add(5 * time.Minute),
		}
		boom := func(id string) actor.Crashing {
			c := demo
			c.Box, c.Cause = id, "runsc: boom"
			return c
		}
		if diff := cmp.Diff([]actor.Crashing{demo, boom("gone"), boom("mended")}, crashing()); diff != "" {
			t.Errorf("Crashing() after three failed wakes each (-want +got):\n%s", diff)
		}
		if wakes := drv.Wakes(); wakes != 1 {
			t.Errorf("asking woke boxes: %d wakes, want the 1 of the box that is well", wakes)
		}

		// A deleted environment's box isn't listed, nor one a person woke.
		if err := g.Delete(ctx, "gone"); err != nil {
			t.Fatal(err)
		}
		drv.FailWakes("mended", nil)
		if err := g.Wake(ctx, "mended"); err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff([]actor.Crashing{demo}, crashing()); diff != "" {
			t.Errorf("Crashing() after a delete and a person's wake (-want +got):\n%s", diff)
		}

		// The box's actor ends with its sleep; Run answers for it from then.
		if err := g.Sleep(ctx, "demo"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if diff := cmp.Diff([]actor.Crashing{demo}, crashing()); diff != "" {
			t.Errorf("Crashing() once the box's actor has ended (-want +got):\n%s", diff)
		}

		// Five minutes after the first failure the box is tried again.
		time.Sleep(2 * time.Minute)
		if got := crashing(); len(got) != 0 {
			t.Errorf("Crashing() after the window = %+v, want none", got)
		}
	})
}

// heldStore is a fakeStore whose snapshots wait for hold to be closed.
type heldStore struct {
	*fakeStore
	hold chan struct{}
}

func (s heldStore) Snapshot(ctx context.Context, id, snapID string) error {
	<-s.hold
	return s.fakeStore.Snapshot(ctx, id, snapID)
}

// An actor in the middle of a change doesn't hold the list up: its box is
// left out, and the others' are there within a moment.
func TestCrashingDoesNotWaitForAChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, drv := heldStore{newFakeStore(), make(chan struct{})}, runsc.NewFake("box")
		g := actor.New(st, drv, actor.Config{Idle: idle, Image: "box"}, notrace, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { _ = g.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		for _, id := range []string{"busy", "demo"} {
			if err := g.Create(ctx, id, store.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		drv.FailWakes("demo", errors.New("runsc: boom"))
		for range 3 {
			if _, err := g.Exec(ctx, "demo", func() {}); err == nil {
				t.Fatal("Exec(demo) worked, want its wake to fail")
			}
		}
		snapshot := make(chan error, 1)
		go func() { snapshot <- g.Snapshot(ctx, "busy", "s1", false) }()
		synctest.Wait() // the snapshot holds busy's actor

		asked := time.Now()
		got, err := g.Crashing(ctx)
		if err != nil {
			t.Fatalf("Crashing() error: %v", err)
		}
		if len(got) != 1 || got[0].Box != "demo" {
			t.Errorf("Crashing() = %+v, want demo alone", got)
		}
		if took := time.Since(asked); took > time.Second {
			t.Errorf("Crashing() took %s beside an actor in a change, want well under a second", took)
		}
		close(st.hold)
		if err := <-snapshot; err != nil {
			t.Errorf("Snapshot() error: %v", err)
		}
	})
}

// The thread's Claude is told at its box's start when the environment's own
// box, where its Bash runs, isn't woken for now: which box, how often it
// failed, why, and when it is tried again.
func TestCrashNoticeAtClaudesWake(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures int
		want     []string // parts of the one notice; none: no notice
	}{
		{name: "a box that is still tried", failures: 2},
		{
			name: "a box that isn't woken for now", failures: 3,
			want: []string{
				"stopped starting this thread's box", "3 times since ", "Bash fails until ", "`fern wake one`", "`fern logs one`",
				"Why it failed the last time: postgres: out of memory",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st, drv := newFakeStore(), runsc.NewFake("box")
				cfg := actor.Config{Idle: idle, Image: "box", Agents: true}
				g := actor.New(st, drv, cfg, notrace, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() { _ = g.Run(ctx); close(done) }()
				defer func() { cancel(); <-done }()
				if err := g.Create(ctx, "one", store.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				drv.FailWakes("one", errors.New("postgres: out of memory"))
				for range tc.failures {
					if _, err := g.Exec(ctx, "one", func() {}); err == nil {
						t.Fatal("Exec(one) worked, want its wake to fail")
					}
				}
				// Its actor waits for its next message: only then does it answer
				// a wake, which doesn't wait for it.
				synctest.Wait()
				if err := g.Wake(ctx, store.AgentBox("one")); err != nil {
					t.Fatal(err)
				}
				got := drv.Woken(store.AgentBox("one")).Notices
				if len(tc.want) == 0 {
					if len(got) != 0 {
						t.Errorf("notices = %q, want none", got)
					}
					return
				}
				if len(got) != 1 {
					t.Fatalf("notices = %q, want one", got)
				}
				for _, part := range tc.want {
					if !strings.Contains(got[0], part) {
						t.Errorf("the notice = %q, want %q in it", got[0], part)
					}
				}
			})
		})
	}
}
