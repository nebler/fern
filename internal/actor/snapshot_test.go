package actor_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
)

// failingStore is a fakeStore whose snapshots fail with err while one is
// set, and which keeps the environments' failed snapshots, as the store
// does on disk.
type failingStore struct {
	*fakeStore
	err    error
	failed map[string]store.SnapshotFailure
}

func (s *failingStore) Snapshot(ctx context.Context, id, snapID string) error {
	s.mu.Lock()
	err := s.err
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("snapshot %s/%s: %w", id, snapID, err)
	}
	return s.fakeStore.Snapshot(ctx, id, snapID)
}

func (s *failingStore) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *failingStore) SetSnapshotFailure(_ context.Context, id string, f *store.SnapshotFailure) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f == nil {
		delete(s.failed, id)
		return nil
	}
	s.failed[id] = *f
	return nil
}

func (s *failingStore) SnapshotFailure(_ context.Context, id string) (store.SnapshotFailure, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.failed[id]
	return f, ok, nil
}

// A turn's snapshot that fails is kept with its environment and told to
// the thread's Claude at its next start, until a snapshot works. What has a
// word of its own is not kept: a full pool, an environment that is gone,
// and a backup's snapshot.
func TestSnapshotFailureIsKept(t *testing.T) {
	broken := errors.New("btrfs: input/output error")
	for _, tc := range []struct {
		name string
		// take takes the snapshot that fails with cause.
		take  func(ctx context.Context, g *actor.Group) error
		cause error
		want  string // a part of the failure kept; "": none is kept
	}{
		{
			name:  "a turn's snapshot",
			take:  func(ctx context.Context, g *actor.Group) error { return g.Snapshot(ctx, "one", "t3-0123", true) },
			cause: broken, want: "btrfs: input/output error",
		},
		{
			name:  "refused by a full pool",
			take:  func(ctx context.Context, g *actor.Group) error { return g.Snapshot(ctx, "one", "t3-0123", true) },
			cause: store.ErrPoolFull,
		},
		{
			name:  "of an environment that is gone",
			take:  func(ctx context.Context, g *actor.Group) error { return g.Snapshot(ctx, "one", "t3-0123", true) },
			cause: store.ErrNotFound,
		},
		{
			name:  "a backup's snapshot",
			take:  func(ctx context.Context, g *actor.Group) error { return g.BackupSnapshot(ctx, "one", "fern-backup-1") },
			cause: broken,
		},
		{
			name: "a snapshot nobody waited for",
			take: func(ctx context.Context, g *actor.Group) error {
				gone, cancel := context.WithCancel(ctx)
				cancel()
				return g.Snapshot(gone, "one", "t3-0123", true)
			},
			// Whichever notices first that the caller is gone, the wait
			// for the actor or the store.
			cause: context.Canceled, want: "it wasn't taken",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st, drv := &failingStore{fakeStore: newFakeStore(), failed: make(map[string]store.SnapshotFailure)}, runsc.NewFake("box")
				cfg := actor.Config{Idle: idle, Image: "box", Agents: true, SnapshotFailures: st}
				g := actor.New(st, drv, cfg, notrace, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() { _ = g.Run(ctx); close(done) }()
				defer func() { cancel(); <-done }()
				if err := g.Create(ctx, "one", store.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				agent := store.AgentBox("one")

				st.fail(tc.cause)
				if err := tc.take(ctx, g); err == nil {
					t.Fatal("the snapshot worked, want it to fail")
				}
				got, kept, _ := st.SnapshotFailure(ctx, "one")
				if kept != (tc.want != "") || !strings.Contains(got.Error, tc.want) {
					t.Fatalf("the failure kept = %+v, %t; want one with %q in it, or none without", got, kept, tc.want)
				}
				if err := g.Wake(ctx, agent); err != nil {
					t.Fatal(err)
				}
				notices := noticesOf(t, drv.Woken(agent))
				if tc.want == "" {
					if len(notices) != 0 {
						t.Errorf("notices = %q, want none", notices)
					}
					return
				}
				if got.Snapshot != "t3-0123" || got.At.IsZero() {
					t.Errorf("the failure kept = %+v, want the snapshot's name and a time", got)
				}
				if len(notices) != 1 {
					t.Fatalf("notices = %q, want one", notices)
				}
				for _, part := range []string{"could not take its snapshot of the turn that ended at ", "checkpoint t3-0123", "a revert to it is refused", tc.want} {
					if !strings.Contains(notices[0], part) {
						t.Errorf("the notice = %q, want %q in it", notices[0], part)
					}
				}

				// A backup's snapshot that works says nothing of the turns'.
				st.fail(nil)
				if err := g.BackupSnapshot(ctx, "one", "fern-backup-1"); err != nil {
					t.Fatal(err)
				}
				if _, kept, _ := st.SnapshotFailure(ctx, "one"); !kept {
					t.Error("a backup's snapshot cleared the turn's failure")
				}
				// The next turn's does, and Claude's next start has no notice.
				if err := g.Snapshot(ctx, "one", "t3-0124", true); err != nil {
					t.Fatal(err)
				}
				if _, kept, _ := st.SnapshotFailure(ctx, "one"); kept {
					t.Error("the failure is still kept after a snapshot that worked")
				}
				if err := g.Sleep(ctx, agent); err != nil {
					t.Fatal(err)
				}
				if err := g.Wake(ctx, agent); err != nil {
					t.Fatal(err)
				}
				if notices := noticesOf(t, drv.Woken(agent)); len(notices) != 0 {
					t.Errorf("notices after a snapshot that worked = %q, want none", notices)
				}
			})
		})
	}
}
