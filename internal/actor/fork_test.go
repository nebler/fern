package actor_test

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
)

func TestFork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, drv := newFakeStore(), runsc.NewFake("box")
		g := start(t, st, drv, actor.Idle{})
		ctx := t.Context()
		if err := g.Create(ctx, "demo", store.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		ids, err := g.Fork(ctx, "demo", "", 3)
		if err != nil {
			t.Fatalf("Fork(demo, 3) error: %v", err)
		}
		if diff := cmp.Diff([]string{"demo-fork-1", "demo-fork-2", "demo-fork-3"}, ids); diff != "" {
			t.Errorf("Fork(demo, 3) mismatch (-want +got):\n%s", diff)
		}
		for _, id := range ids {
			if got := st.forked[id]; got != (store.SnapshotRef{Env: "demo"}) {
				t.Errorf("%s forked from %+v, want demo's workspace", id, got)
			}
		}
		// A fork wakes like any environment, into a box of its own.
		if err := g.Wake(ctx, "demo-fork-2"); err != nil {
			t.Fatal(err)
		}
		if got := state(t, drv, "demo-fork-2"); got != runsc.Running {
			t.Errorf("demo-fork-2 is %s, want running", got)
		}

		// Numbers freed by a delete are used again, lowest first.
		if err := g.Delete(ctx, "demo-fork-2"); err != nil {
			t.Fatal(err)
		}
		ids, err = g.Fork(ctx, "demo", "s1", 2)
		if err != nil {
			t.Fatalf("Fork(demo, s1, 2) error: %v", err)
		}
		if diff := cmp.Diff([]string{"demo-fork-2", "demo-fork-4"}, ids); diff != "" {
			t.Errorf("Fork(demo, s1, 2) mismatch (-want +got):\n%s", diff)
		}
		if got := st.forked["demo-fork-4"]; got != (store.SnapshotRef{Env: "demo", Snapshot: "s1"}) {
			t.Errorf("demo-fork-4 forked from %+v, want demo/s1", got)
		}

		for _, tt := range []struct {
			name  string
			id    string
			count int
			want  error
		}{
			{"none", "demo", 0, actor.ErrTooManyForks},
			{"over the cap", "demo", actor.MaxForks - 3, actor.ErrTooManyForks},
			{"of no environment", "nope", 1, store.ErrNotFound},
		} {
			if _, err := g.Fork(ctx, tt.id, "", tt.count); !errors.Is(err, tt.want) {
				t.Errorf("%s: Fork(%s, %d) error = %v, want %v", tt.name, tt.id, tt.count, err, tt.want)
			}
		}

		// Deleting the environment deletes its forks, awake or not.
		if err := g.Wake(ctx, "demo-fork-1"); err != nil {
			t.Fatal(err)
		}
		if err := g.Delete(ctx, "demo"); err != nil {
			t.Fatal(err)
		}
		envs, err := st.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(envs) > 0 {
			t.Errorf("after Delete(demo): %s is left", envs[0].ID)
		}
		if got := state(t, drv, "demo-fork-1"); got != runsc.Asleep {
			t.Errorf("after Delete(demo): demo-fork-1's box is %s, want gone", got)
		}
	})
}
