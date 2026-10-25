package store_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/store"
)

var errCrash = errors.New("crash")

// crashCase is a change to stop at every step, as a crash would.
type crashCase struct {
	name string
	// setup prepares the pool, before steps are watched.
	setup func(t *testing.T, st *store.Store, dir string)
	// change is the change to stop.
	change func(t *testing.T, st *store.Store, dir string) error
	// check checks the pool after the stopped change and a Reconcile, as
	// after a restart. It also checks the change can be made again.
	check func(t *testing.T, st *store.Store, dir string)
}

// TestCrash stops every change with more than one step at each of its
// steps, then runs Reconcile, as a restarted daemon does: the environment is
// as before the change or as after it, nothing the change was to keep is
// lost, and the change can be made again.
func TestCrash(t *testing.T) {
	for _, tc := range crashCases() {
		t.Run(tc.name, func(t *testing.T) {
			steps := crashSteps(t, tc)
			if len(steps) == 0 {
				t.Fatal("the change has no steps to stop at")
			}
			for _, step := range steps {
				t.Run(strings.ReplaceAll(step, " ", "_"), func(t *testing.T) {
					st, dir := openPool(t)
					tc.setup(t, st, dir)
					store.SetFailAt(st, func(s string) error {
						if s == step {
							return errCrash
						}
						return nil
					})
					if err := tc.change(t, st, dir); !errors.Is(err, errCrash) {
						t.Fatalf("change stopped at %q: error = %v, want %v", step, err, errCrash)
					}
					store.SetFailAt(st, nil)
					lost, err := st.Reconcile(t.Context())
					if err != nil || len(lost) != 0 {
						t.Fatalf("Reconcile() = %v, %v, want nothing lost", lost, err)
					}
					tc.check(t, st, dir)
				})
			}
		})
	}
}

// crashSteps returns the steps tc's change takes, in order.
func crashSteps(t *testing.T, tc crashCase) []string {
	t.Helper()
	st, dir := openPool(t)
	tc.setup(t, st, dir)
	var steps []string
	store.SetFailAt(st, func(s string) error { steps = append(steps, s); return nil })
	if err := tc.change(t, st, dir); err != nil {
		t.Fatalf("change error: %v", err)
	}
	store.SetFailAt(st, nil)
	return steps
}

func crashCases() []crashCase {
	worktree := func(dir string) string { return filepath.Join(dir, "worktrees", "repo", "wt") }
	live := func(dir string) string { return filepath.Join(dir, "envs", "demo", "live") }
	return []crashCase{
		{
			name:  "create",
			setup: func(*testing.T, *store.Store, string) {},
			change: func(t *testing.T, st *store.Store, _ string) error {
				_, err := st.Create(t.Context(), "demo", store.CreateOptions{Seed: tarball(t, map[string]string{"f": "seed"})})
				return err
			},
			check: func(t *testing.T, st *store.Store, dir string) {
				if _, err := st.Get(t.Context(), "demo"); errors.Is(err, store.ErrNotFound) {
					gone(t, filepath.Join(dir, "envs", "demo"))
				} else if err != nil {
					t.Fatal(err)
				}
				env := create(t, st, "demo", store.CreateOptions{Seed: tarball(t, map[string]string{"f": "seed"})})
				if got := readFile(t, filepath.Join(env.Workspace, "f")); got != "seed" {
					t.Errorf("f = %q after creating again, want %q", got, "seed")
				}
			},
		},
		{
			// Adopting T3's worktree must never lose the work in it.
			name: "adopt",
			setup: func(t *testing.T, _ *store.Store, dir string) {
				if err := os.MkdirAll(worktree(dir), 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(worktree(dir), "work"), "kept")
			},
			change: func(t *testing.T, st *store.Store, dir string) error {
				_, err := st.Create(t.Context(), "wt", store.CreateOptions{Workspace: worktree(dir)})
				return err
			},
			check: func(t *testing.T, st *store.Store, dir string) {
				if got := readFile(t, filepath.Join(worktree(dir), "work")); got != "kept" {
					t.Errorf("work = %q after the crash, want %q", got, "kept")
				}
				create(t, st, "wt", store.CreateOptions{Workspace: worktree(dir)})
				if _, ino, _ := stat(t, worktree(dir)); ino != subvolumeIno {
					t.Errorf("worktree inode = %d after creating again, want a subvolume", ino)
				}
				if got := readFile(t, filepath.Join(worktree(dir), "work")); got != "kept" {
					t.Errorf("work = %q after creating again, want %q", got, "kept")
				}
			},
		},
		{
			// T3 made a removed worktree again: adopting it back must keep
			// both its new files and the environment's snapshots.
			name: "readopt",
			setup: func(t *testing.T, st *store.Store, dir string) {
				create(t, st, "wt", store.CreateOptions{Workspace: worktree(dir)})
				snapshot(t, st, "wt", "s1")
				if out, err := exec.CommandContext(t.Context(), "btrfs", "subvolume", "delete", worktree(dir)).CombinedOutput(); err != nil {
					t.Fatalf("btrfs subvolume delete: %v: %s", err, out)
				}
				if err := os.MkdirAll(worktree(dir), 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(worktree(dir), "work"), "kept")
			},
			change: func(t *testing.T, st *store.Store, dir string) error {
				_, err := st.Create(t.Context(), "wt", store.CreateOptions{Workspace: worktree(dir)})
				return err
			},
			check: func(t *testing.T, st *store.Store, dir string) {
				if got := readFile(t, filepath.Join(worktree(dir), "work")); got != "kept" {
					t.Errorf("work = %q after the crash, want %q", got, "kept")
				}
				create(t, st, "wt", store.CreateOptions{Workspace: worktree(dir)})
				if _, ino, _ := stat(t, worktree(dir)); ino != subvolumeIno {
					t.Errorf("worktree inode = %d after creating again, want a subvolume", ino)
				}
				snaps, err := st.Snapshots(t.Context(), "wt")
				if err != nil || len(snaps) != 1 {
					t.Errorf("Snapshots(wt) = %v, %v after creating again, want s1", snaps, err)
				}
			},
		},
		{
			name: "delete",
			setup: func(t *testing.T, st *store.Store, dir string) {
				create(t, st, "demo", store.CreateOptions{})
				writeFile(t, filepath.Join(live(dir), "f"), "x")
				snapshot(t, st, "demo", "s1")
			},
			change: func(t *testing.T, st *store.Store, _ string) error { return st.Delete(t.Context(), "demo") },
			check: func(t *testing.T, st *store.Store, dir string) {
				if _, err := st.Get(t.Context(), "demo"); err == nil {
					if got := readFile(t, filepath.Join(live(dir), "f")); got != "x" {
						t.Errorf("f = %q in an environment still indexed, want %q", got, "x")
					}
				} else if !errors.Is(err, store.ErrNotFound) {
					t.Fatal(err)
				}
				if err := st.Delete(t.Context(), "demo"); err != nil {
					t.Fatalf("Delete(demo) again error: %v", err)
				}
				gone(t, filepath.Join(dir, "envs", "demo"))
			},
		},
		{
			name: "snapshot",
			setup: func(t *testing.T, st *store.Store, dir string) {
				create(t, st, "demo", store.CreateOptions{})
				writeFile(t, filepath.Join(live(dir), "f"), "v1")
			},
			change: func(t *testing.T, st *store.Store, _ string) error { return st.Snapshot(t.Context(), "demo", "s1") },
			check: func(t *testing.T, st *store.Store, dir string) {
				// Reconcile indexes a snapshot taken just before the crash.
				if got := snapIDs(t, st); len(got) != 1 || got[0] != "s1" {
					t.Errorf("Snapshots(demo) = %q, want [s1]", got)
				}
				if got := readFile(t, filepath.Join(dir, "envs", "demo", "snaps", "s1", "f")); got != "v1" {
					t.Errorf("s1/f = %q, want %q", got, "v1")
				}
			},
		},
		{
			name: "delete snapshot",
			setup: func(t *testing.T, st *store.Store, _ string) {
				create(t, st, "demo", store.CreateOptions{})
				snapshot(t, st, "demo", "s1")
			},
			change: func(t *testing.T, st *store.Store, _ string) error {
				return st.DeleteSnapshot(t.Context(), "demo", "s1")
			},
			check: func(t *testing.T, st *store.Store, dir string) {
				if got := snapIDs(t, st); len(got) != 0 {
					t.Errorf("Snapshots(demo) = %q, want none: its subvolume is gone", got)
				}
				gone(t, filepath.Join(dir, "envs", "demo", "snaps", "s1"))
			},
		},
		{
			name: "delete nested",
			setup: func(t *testing.T, st *store.Store, dir string) {
				create(t, st, "demo", store.CreateOptions{})
				snapshot(t, st, "demo", "s1")
				subvolume(t, filepath.Join(live(dir), "nested", "deep"))
			},
			change: func(t *testing.T, st *store.Store, _ string) error { return st.Delete(t.Context(), "demo") },
			check: func(t *testing.T, st *store.Store, dir string) {
				if err := st.Delete(t.Context(), "demo"); err != nil {
					t.Fatalf("Delete(demo) again: %v", err)
				}
				gone(t, filepath.Join(dir, "envs", "demo"))
			},
		},
		{
			name: "reconcile snapshots",
			setup: func(t *testing.T, st *store.Store, _ string) {
				create(t, st, "demo", store.CreateOptions{})
				snapshot(t, st, "demo", "stale")
				store.SetFailAt(st, func(step string) error {
					if step == "delete snapshot: removed" || step == "snapshot: taken" {
						return errCrash
					}
					return nil
				})
				if err := st.DeleteSnapshot(t.Context(), "demo", "stale"); !errors.Is(err, errCrash) {
					t.Fatalf("DeleteSnapshot() = %v, want crash", err)
				}
				for _, id := range []string{"orphan-a", "orphan-b"} {
					if err := st.Snapshot(t.Context(), "demo", id); !errors.Is(err, errCrash) {
						t.Fatalf("Snapshot(%s) = %v, want crash", id, err)
					}
				}
				store.SetFailAt(st, nil)
			},
			change: func(t *testing.T, st *store.Store, _ string) error {
				_, err := st.Reconcile(t.Context())
				return err
			},
			check: func(t *testing.T, st *store.Store, _ string) {
				if got := snapIDs(t, st); len(got) != 2 || got[0] != "orphan-a" || got[1] != "orphan-b" {
					t.Errorf("Snapshots() = %v, want [orphan-a orphan-b]", got)
				}
				if _, err := st.Reconcile(t.Context()); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// The workspace is the old one or the restored one, and the
			// old one is never lost.
			name: "restore",
			setup: func(t *testing.T, st *store.Store, dir string) {
				create(t, st, "demo", store.CreateOptions{})
				writeFile(t, filepath.Join(live(dir), "f"), "v1")
				snapshot(t, st, "demo", "s1")
				writeFile(t, filepath.Join(live(dir), "f"), "v2")
			},
			change: func(t *testing.T, st *store.Store, _ string) error { return st.Restore(t.Context(), "demo", "s1") },
			check: func(t *testing.T, st *store.Store, dir string) {
				got := readFile(t, filepath.Join(live(dir), "f"))
				if got != "v1" && got != "v2" {
					t.Fatalf("f = %q, want the old %q or the restored %q", got, "v2", "v1")
				}
				if got == "v1" {
					pre := preRestores(t, st)
					if len(pre) != 1 || readFile(t, filepath.Join(dir, "envs", "demo", "snaps", pre[0], "f")) != "v2" {
						t.Errorf("after the swap: pre-restore snapshots %q, want one holding %q", pre, "v2")
					}
				}
				gone(t, live(dir)+".new")
				if err := st.Restore(t.Context(), "demo", "s1"); err != nil {
					t.Fatalf("Restore(demo, s1) again error: %v", err)
				}
				if got := readFile(t, filepath.Join(live(dir), "f")); got != "v1" {
					t.Errorf("f = %q after restoring again, want %q", got, "v1")
				}
			},
		},
	}
}

func snapIDs(t *testing.T, st *store.Store) []string {
	t.Helper()
	snaps, err := st.Snapshots(t.Context(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range snaps {
		ids = append(ids, s.ID)
	}
	return ids
}

func preRestores(t *testing.T, st *store.Store) []string {
	t.Helper()
	var ids []string
	for _, id := range snapIDs(t, st) {
		if strings.HasPrefix(id, "pre-restore-") {
			ids = append(ids, id)
		}
	}
	return ids
}
