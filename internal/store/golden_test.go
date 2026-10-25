package store_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/nebler/fern/internal/store"
)

// writeAll writes path, making its directories.
func writeAll(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, data)
}

// buildGolden makes a golden snapshot as a build does: builder environment
// "golden" gets the checkout in snapshot src, "runs" (build writes its
// output), is stripped and snapshotted as "g1".
func buildGolden(t *testing.T, st *store.Store, src store.SnapshotRef, build func(live string)) {
	t.Helper()
	ctx := t.Context()
	env := create(t, st, "golden", store.CreateOptions{})
	if err := st.BuildSource(ctx, "golden", src); err != nil {
		t.Fatalf("BuildSource() error: %v", err)
	}
	build(env.Workspace)
	if err := st.StripSource(ctx, "golden"); err != nil {
		t.Fatalf("StripSource() error: %v", err)
	}
	snapshot(t, st, "golden", "g1")
}

// A golden snapshot holds only what its build made, and a new worktree
// adopted onto it has its own files over that output.
func TestGolden(t *testing.T) {
	st, dir := openPool(t)
	worktree := func(name string) string { return filepath.Join(dir, "worktrees", "repo", name) }
	// The first thread's worktree, at the commit the golden is built from.
	first := worktree("first")
	for name, body := range map[string]string{".git": "gitdir: x\n", "seed.sql": "v1", "old.txt": "gone at the next commit", "src/main.go": "v1"} {
		writeAll(t, filepath.Join(first, name), body)
	}
	create(t, st, "first", store.CreateOptions{Workspace: first})
	snapshot(t, st, "first", "golden-source")
	buildGolden(t, st, store.SnapshotRef{Env: "first", Snapshot: "golden-source"}, func(live string) {
		writeAll(t, filepath.Join(live, ".fern", "pg", "PG_VERSION"), "17")
		writeAll(t, filepath.Join(live, "src", "main.go"), "rewritten by the build") // a source file it changed
		writeAll(t, filepath.Join(live, "node_modules", "x", "index.js"), "module")
	})
	golden := filepath.Join(dir, "envs", "golden", "snaps", "g1")
	for _, name := range []string{".git", "seed.sql", "old.txt", "src/main.go"} {
		if _, err := os.Lstat(filepath.Join(golden, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("golden has the checkout's %s (error %v), want only the build's output", name, err)
		}
	}

	// A later thread, at the next commit: old.txt is gone, main.go changed.
	next := worktree("next")
	for name, body := range map[string]string{".git": "gitdir: y\n", "seed.sql": "v1", "src/main.go": "v2"} {
		writeAll(t, filepath.Join(next, name), body)
	}
	if err := os.Chmod(filepath.Join(next, "src", "main.go"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(filepath.Join(next, "src"), store.BoxUID, store.BoxGID); err != nil {
		t.Fatal(err)
	}
	env := create(t, st, "next", store.CreateOptions{Workspace: next, From: &store.SnapshotRef{Env: "golden", Snapshot: "g1"}})
	if _, ino, _ := stat(t, env.Workspace); ino != subvolumeIno {
		t.Errorf("workspace has inode %d, want a subvolume", ino)
	}
	for name, want := range map[string]string{
		".git": "gitdir: y\n", "src/main.go": "v2", ".fern/pg/PG_VERSION": "17", "node_modules/x/index.js": "module",
	} {
		if got := readFile(t, filepath.Join(next, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	gone(t, filepath.Join(next, "old.txt"))
	if _, _, perm := stat(t, filepath.Join(next, "src", "main.go")); perm != 0o600 {
		t.Errorf("src/main.go mode = %v, want the worktree's 0600", perm)
	}
	if uid, _, _ := stat(t, filepath.Join(next, "src")); uid != store.BoxUID {
		t.Errorf("src is owned by %d, want the worktree's %d", uid, store.BoxUID)
	}
}

// What a build made came from the repository's code: a symlink it left
// can't lead a new worktree's files out of the workspace.
func TestGoldenSymlinkStaysInside(t *testing.T) {
	st, dir := openPool(t)
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(dir, "worktrees", "repo", "first")
	writeAll(t, filepath.Join(first, "README"), "x")
	create(t, st, "first", store.CreateOptions{Workspace: first})
	snapshot(t, st, "first", "golden-source")
	buildGolden(t, st, store.SnapshotRef{Env: "first", Snapshot: "golden-source"}, func(live string) {
		for name, target := range map[string]string{"src": outside, "file": filepath.Join(outside, "file")} {
			if err := os.Symlink(target, filepath.Join(live, name)); err != nil {
				t.Fatal(err)
			}
		}
	})
	next := filepath.Join(dir, "worktrees", "repo", "next")
	writeAll(t, filepath.Join(next, "src", "main.go"), "v2")
	writeAll(t, filepath.Join(next, "file"), "v2")
	create(t, st, "next", store.CreateOptions{Workspace: next, From: &store.SnapshotRef{Env: "golden", Snapshot: "g1"}})
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Errorf("outside the workspace: %v, %v; want nothing written there", entries, err)
	}
	if got := readFile(t, filepath.Join(next, "src", "main.go")); got != "v2" {
		t.Errorf("src/main.go = %q, want v2 in the workspace", got)
	}
}

// A build's strip removes the checkout even when a build before it stopped
// half-way: the list of what was copied in is written before the copy.
func TestStripAfterAnUnfinishedBuild(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	first := filepath.Join(dir, "worktrees", "repo", "first")
	writeAll(t, filepath.Join(first, "a.txt"), "first")
	create(t, st, "first", store.CreateOptions{Workspace: first})
	snapshot(t, st, "first", "golden-source")
	env := create(t, st, "golden", store.CreateOptions{})
	src := store.SnapshotRef{Env: "first", Snapshot: "golden-source"}
	if err := st.BuildSource(ctx, "golden", src); err != nil { // and then the daemon stops
		t.Fatal(err)
	}
	if err := st.BuildSource(ctx, "golden", src); err != nil {
		t.Fatal(err)
	}
	writeAll(t, filepath.Join(env.Workspace, "out"), "made")
	if err := st.StripSource(ctx, "golden"); err != nil {
		t.Fatal(err)
	}
	gone(t, filepath.Join(env.Workspace, "a.txt"))
	if got := readFile(t, filepath.Join(env.Workspace, "out")); got != "made" {
		t.Errorf("out = %q, want the build's output kept", got)
	}
}

// A build's list is its own: a file an earlier build copied in and this one
// made is kept, and a name with a newline is stripped whole.
func TestStripListsThisBuild(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	for name, files := range map[string][]string{"a": {"made-later.js", "keep"}, "b": {"two\nlines", "keep"}} {
		wt := filepath.Join(dir, "worktrees", "repo", name)
		for _, f := range files {
			writeAll(t, filepath.Join(wt, f), f)
		}
		create(t, st, name, store.CreateOptions{Workspace: wt})
		snapshot(t, st, name, "golden-source")
	}
	env := create(t, st, "golden", store.CreateOptions{})
	if err := st.BuildSource(ctx, "golden", store.SnapshotRef{Env: "a", Snapshot: "golden-source"}); err != nil {
		t.Fatal(err) // and then this build fails
	}
	if err := st.BuildSource(ctx, "golden", store.SnapshotRef{Env: "b", Snapshot: "golden-source"}); err != nil {
		t.Fatal(err)
	}
	writeAll(t, filepath.Join(env.Workspace, "made-later.js"), "made by build b")
	writeAll(t, filepath.Join(env.Workspace, "two"), "made by build b")
	if err := st.StripSource(ctx, "golden"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"made-later.js", "two"} {
		if got := readFile(t, filepath.Join(env.Workspace, f)); got != "made by build b" {
			t.Errorf("%s = %q, want build b's output kept", f, got)
		}
	}
	for _, f := range []string{"two\nlines", "keep"} {
		gone(t, filepath.Join(env.Workspace, f))
	}
	srcs, err := st.Sources(ctx, "golden-source")
	if err != nil || len(srcs) != 2 {
		t.Errorf("Sources() = %v, %v, want a's and b's", srcs, err)
	}
}

// A worktree's symlinks are copied as symlinks, never read through, and
// what isn't a file, a directory or a symlink isn't copied.
func TestOverlayCopiesSymlinksAsLinks(t *testing.T) {
	st, dir := openPool(t)
	first := filepath.Join(dir, "worktrees", "repo", "first")
	writeAll(t, filepath.Join(first, "README"), "x")
	create(t, st, "first", store.CreateOptions{Workspace: first})
	snapshot(t, st, "first", "golden-source")
	buildGolden(t, st, store.SnapshotRef{Env: "first", Snapshot: "golden-source"}, func(string) {})
	secret := filepath.Join(dir, "secret")
	writeFile(t, secret, "root's")
	next := filepath.Join(dir, "worktrees", "repo", "next")
	writeAll(t, filepath.Join(next, "README"), "x")
	if err := os.Symlink(secret, filepath.Join(next, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(next, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	create(t, st, "next", store.CreateOptions{Workspace: next, From: &store.SnapshotRef{Env: "golden", Snapshot: "g1"}})
	if target, err := os.Readlink(filepath.Join(next, "link")); err != nil || target != secret {
		t.Errorf("link = %q, %v, want the symlink itself", target, err)
	}
	gone(t, filepath.Join(next, "pipe"))
}
