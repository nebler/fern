package actor_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
)

// A job keeps its box awake past a Bash call's linger and the agent clock,
// until the human clock runs out, and that sleep cancels it.
func TestJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, drv := newFakeStore(), runsc.NewFake("box")
		g := start(t, st, drv, idle)
		if err := g.Create(t.Context(), "demo", store.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		jobCtx, cancelJob := context.WithCancel(t.Context())
		job, err := g.Job(t.Context(), "demo", "", cancelJob)
		if err != nil {
			t.Fatal(err)
		}
		defer job.Close()
		// The Bash call that started it, which asks for a short linger.
		bash, err := g.Exec(t.Context(), "demo", "", func() {})
		if err != nil {
			t.Fatal(err)
		}
		bash.CloseAndSleep(5 * time.Second)

		time.Sleep(10 * time.Minute) // past the linger and the agent clock
		synctest.Wait()
		if got := state(t, drv, "demo"); got != runsc.Running {
			t.Fatalf("10 minutes on, with a job: state %s, want %s", got, runsc.Running)
		}
		if jobCtx.Err() != nil {
			t.Fatal("the job was cancelled while its box was awake")
		}
		time.Sleep(10*time.Minute + time.Second) // past the human clock
		synctest.Wait()
		if got := state(t, drv, "demo"); got != runsc.Asleep {
			t.Errorf("20 minutes on: state %s, want %s", got, runsc.Asleep)
		}
		if jobCtx.Err() == nil {
			t.Error("the box slept and its job wasn't cancelled")
		}
	})
}

// Without a job, the same Bash call lets the box sleep after its linger.
func TestNoJobLinger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, drv := newFakeStore(), runsc.NewFake("box")
		g := start(t, st, drv, idle)
		if err := g.Create(t.Context(), "demo", store.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		bash, err := g.Exec(t.Context(), "demo", "", func() {})
		if err != nil {
			t.Fatal(err)
		}
		bash.CloseAndSleep(5 * time.Second)
		time.Sleep(6 * time.Second)
		synctest.Wait()
		if got := state(t, drv, "demo"); got != runsc.Asleep {
			t.Errorf("after the linger: state %s, want %s", got, runsc.Asleep)
		}
	})
}
