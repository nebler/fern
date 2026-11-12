package actor_test

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
)

// Holds answers "why is this box still awake?" from what its actor counts,
// with the rule the idle timer follows, and wakes nothing to do it.
func TestHolds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, drv := newFakeStore(), runsc.NewFake("box")
		g := start(t, st, drv, idle)
		ctx := t.Context()
		if err := g.Create(ctx, "demo", store.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		holds := func() actor.Holds {
			t.Helper()
			h, err := g.Holds(ctx, "demo")
			if err != nil {
				t.Fatalf("Holds(demo) error: %v", err)
			}
			return h
		}
		ignoreN := cmpopts.IgnoreFields(actor.Running{}, "N")

		if diff := cmp.Diff(actor.Holds{Box: "demo", Why: "asleep"}, holds()); diff != "" {
			t.Errorf("Holds of a box that never woke (-want +got):\n%s", diff)
		}
		if got := state(t, drv, "demo"); got != runsc.Asleep {
			t.Fatalf("asking woke the box: state %s", got)
		}

		t0 := time.Now()
		ex, err := g.Exec(ctx, "demo", func() {})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		job, err := g.Job(ctx, "demo", func() {})
		if err != nil {
			t.Fatal(err)
		}
		defer job.Close()
		want := actor.Holds{
			Box: "demo", Awake: true, Why: "stays awake while a command or a preview request runs",
			Running: []actor.Running{{Kind: "exec", Since: t0}, {Kind: "job", Since: t0.Add(time.Minute)}},
		}
		if diff := cmp.Diff(want, holds(), ignoreN); diff != "" {
			t.Errorf("Holds with a command and a job (-want +got):\n%s", diff)
		}

		// The command ends: the job is still counted, and the box sleeps on
		// its idle times, the later of which is the person's.
		ex.Close()
		synctest.Wait()
		want = actor.Holds{
			Box: "demo", Awake: true, SleepAt: t0.Add(time.Minute + idle.Human), Why: "20m0s after it was last used",
			Running: []actor.Running{{Kind: "job", Since: t0.Add(time.Minute)}},
		}
		if diff := cmp.Diff(want, holds(), ignoreN); diff != "" {
			t.Errorf("Holds with a job alone (-want +got):\n%s", diff)
		}
		// And it does sleep then, as Holds said.
		time.Sleep(idle.Human + time.Second)
		synctest.Wait()
		if got := state(t, drv, "demo"); got != runsc.Asleep {
			t.Errorf("at the time Holds gave: state %s, want %s", got, runsc.Asleep)
		}

		if _, err := g.Holds(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Holds(missing) error = %v, want %v", err, store.ErrNotFound)
		}
	})
}
