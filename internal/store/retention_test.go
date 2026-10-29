package store_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/store"
)

// Prune keeps the newest snapshots of an environment's turns and the
// pre-restore copies that are less than a day old.
func TestPrune(t *testing.T) {
	st, _ := openPool(t)
	store.SetRetention(st, 3, 0)
	create(t, st, "demo", store.CreateOptions{})
	for _, id := range []string{"t1", "t2", "t3", "t4"} {
		snapshot(t, st, "demo", id)
	}
	if err := st.Restore(t.Context(), "demo", "t2"); err != nil {
		t.Fatal(err)
	}
	snapshot(t, st, "demo", "t5")
	if err := st.Prune(t.Context(), "demo"); err != nil {
		t.Fatalf("Prune(demo) error: %v", err)
	}
	var turns []string
	for _, id := range snapIDs(t, st) {
		if !slices.Contains(preRestores(t, st), id) {
			turns = append(turns, id)
		}
	}
	if diff := cmp.Diff([]string{"t3", "t4", "t5"}, turns); diff != "" {
		t.Errorf("turn snapshots after Prune mismatch (-want +got):\n%s", diff)
	}
	if got := preRestores(t, st); len(got) != 1 {
		t.Errorf("pre-restore snapshots after Prune = %v, want the one from the restore", got)
	}
	if err := st.Restore(t.Context(), "demo", "t1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Restore(demo, t1) after Prune error = %v, want %v", err, store.ErrNotFound)
	}
}

// Below the free space it must leave, the pool refuses new snapshots and
// environments, but an environment that exists still answers.
func TestPoolFull(t *testing.T) {
	st, _ := openPool(t)
	create(t, st, "demo", store.CreateOptions{})
	store.SetRetention(st, 50, 1<<62)
	if err := st.Snapshot(t.Context(), "demo", "s1"); !errors.Is(err, store.ErrPoolFull) {
		t.Errorf("Snapshot(demo, s1) error = %v, want %v", err, store.ErrPoolFull)
	}
	if _, err := st.Create(t.Context(), "other", store.CreateOptions{}); !errors.Is(err, store.ErrPoolFull) {
		t.Errorf("Create(other) error = %v, want %v", err, store.ErrPoolFull)
	}
	if _, err := st.Create(t.Context(), "demo", store.CreateOptions{}); err != nil {
		t.Errorf("Create(demo), which exists, error = %v, want nil", err)
	}
}

// When T3 removes a worktree, its environment is an orphan; when T3 makes the
// worktree again, Create adopts it back with its snapshots.
func TestOrphanAndReadopt(t *testing.T) {
	st, dir := openPool(t)
	path := filepath.Join(dir, "worktrees", "repo", "wt")
	create(t, st, "wt", store.CreateOptions{Workspace: path})
	create(t, st, "inpool", store.CreateOptions{})
	writeFile(t, filepath.Join(path, "data"), "v1")
	snapshot(t, st, "wt", "s1")
	orphans := func() []string {
		t.Helper()
		ids, err := st.Orphans(t.Context())
		if err != nil {
			t.Fatalf("Orphans() error: %v", err)
		}
		return ids
	}
	if got := orphans(); len(got) != 0 {
		t.Errorf("Orphans() = %v with every workspace there, want none", got)
	}

	// git worktree remove: the files, then the empty subvolume.
	if out, err := exec.CommandContext(t.Context(), "btrfs", "subvolume", "delete", path).CombinedOutput(); err != nil {
		t.Fatalf("btrfs subvolume delete: %v: %s", err, out)
	}
	if diff := cmp.Diff([]string{"wt"}, orphans()); diff != "" {
		t.Errorf("Orphans() after the worktree went mismatch (-want +got):\n%s", diff)
	}

	// git worktree add makes a plain directory at the same path.
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(path, "data"), "v2")
	create(t, st, "wt", store.CreateOptions{Workspace: path})
	if _, ino, _ := stat(t, path); ino != subvolumeIno {
		t.Errorf("%s has inode %d after Create, want a subvolume again", path, ino)
	}
	if got := readFile(t, filepath.Join(path, "data")); got != "v2" {
		t.Errorf("data = %q after Create, want the new worktree's v2", got)
	}
	if got := orphans(); len(got) != 0 {
		t.Errorf("Orphans() = %v after Create, want none", got)
	}
	if err := st.Restore(t.Context(), "wt", "s1"); err != nil {
		t.Fatalf("Restore(wt, s1) after the worktree came back error: %v", err)
	}
	if got := readFile(t, filepath.Join(path, "data")); got != "v1" {
		t.Errorf("data = %q after Restore, want v1", got)
	}
}
