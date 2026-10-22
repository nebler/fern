package store_test

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	_ "modernc.org/sqlite" // the tests edit the index behind the store's back

	"github.com/nebler/fern/internal/store"
)

// openPool opens a store in a fresh directory of the btrfs filesystem at
// $FERN_TEST_BTRFS. These tests need root and btrfs-progs; lab/test-store.sh
// provides both.
func openPool(t *testing.T) (*store.Store, string) {
	t.Helper()
	root := os.Getenv("FERN_TEST_BTRFS")
	if root == "" {
		t.Skip("FERN_TEST_BTRFS is not set; run lab/test-store.sh for the btrfs store tests")
	}
	dir, err := os.MkdirTemp(root, "pool")
	if err != nil {
		t.Fatal(err)
	}
	// MkdirTemp makes it 0700, but seeds are unpacked as the box user, who
	// must be able to reach live/.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open(%s) error: %v", dir, err)
	}
	t.Cleanup(func() {
		// Subvolumes and read-only snapshots can't be removed with RemoveAll.
		ctx := context.WithoutCancel(t.Context())
		envs, _ := st.List(ctx)
		for _, env := range envs {
			if err := st.Delete(ctx, env.ID); err != nil {
				t.Errorf("cleanup: Delete(%s) error: %v", env.ID, err)
			}
		}
		_ = st.Close()
		_ = os.RemoveAll(dir)
	})
	return st, dir
}

// tarball returns a tar stream of files, named by path.
func tarball(t *testing.T, files map[string]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func livePath(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	live, err := st.LivePath(t.Context(), id)
	if err != nil {
		t.Fatalf("LivePath(%s) error: %v", id, err)
	}
	return live
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCreate(t *testing.T) {
	st, _ := openPool(t)
	ctx := t.Context()
	seed := tarball(t, map[string]string{"README.md": "hello\n", "src/main.go": "package main\n"})
	env, err := st.Create(ctx, "demo", store.CreateOptions{Repo: "/src/demo", Seed: seed})
	if err != nil {
		t.Fatalf("Create(demo) error: %v", err)
	}
	if env.ID != "demo" || env.Repo != "/src/demo" || env.Token == "" || env.Created.IsZero() {
		t.Errorf("Create(demo) = %+v, want ID, Repo, Token and Created set", env)
	}
	live := livePath(t, st, "demo")
	for name, want := range map[string]string{"README.md": "hello\n", "src/main.go": "package main\n"} {
		if got := readFile(t, filepath.Join(live, name)); got != want {
			t.Errorf("live/%s = %q, want %q", name, got, want)
		}
	}
	// The box runs as BoxUID and must be able to write everything.
	for _, name := range []string{".", "README.md", "src"} {
		info, err := os.Stat(filepath.Join(live, name))
		if err != nil {
			t.Fatal(err)
		}
		if uid := info.Sys().(*syscall.Stat_t).Uid; uid != store.BoxUID {
			t.Errorf("live/%s is owned by %d, want %d", name, uid, store.BoxUID)
		}
	}

	// Claude's folder: the box user's alone, beside live/ so a restore keeps it.
	info, err := os.Stat(env.Claude)
	if err != nil {
		t.Fatalf("stat Claude's folder: %v", err)
	}
	if uid := info.Sys().(*syscall.Stat_t).Uid; uid != store.BoxUID || info.Mode().Perm() != 0o700 {
		t.Errorf("Claude's folder is owned by %d with mode %v, want %d and 0700", uid, info.Mode().Perm(), store.BoxUID)
	}
	if filepath.Dir(env.Claude) != filepath.Dir(live) {
		t.Errorf("Claude's folder %s isn't beside the workspace %s", env.Claude, live)
	}

	again, err := st.Create(ctx, "demo", store.CreateOptions{Repo: "/other"})
	if err != nil {
		t.Fatalf("Create(demo) again error: %v", err)
	}
	if diff := cmp.Diff(env, again); diff != "" {
		t.Errorf("Create(demo) again mismatch (-want +got):\n%s", diff)
	}
	if got := readFile(t, filepath.Join(live, "README.md")); got != "hello\n" {
		t.Errorf("after second Create: live/README.md = %q, want it unchanged", got)
	}
	list, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if diff := cmp.Diff([]store.Env{env}, list); diff != "" {
		t.Errorf("List() mismatch (-want +got):\n%s", diff)
	}
}

func TestSnapshotRestore(t *testing.T) {
	st, _ := openPool(t)
	ctx := t.Context()
	if _, err := st.Create(ctx, "demo", store.CreateOptions{}); err != nil {
		t.Fatalf("Create(demo) error: %v", err)
	}
	live := livePath(t, st, "demo")
	writeFile(t, filepath.Join(live, "data"), "v1")

	s1, err := st.Snapshot(ctx, "demo", "s1")
	if err != nil {
		t.Fatalf("Snapshot(demo, s1) error: %v", err)
	}
	writeFile(t, filepath.Join(live, "data"), "v2")
	writeFile(t, filepath.Join(live, "extra"), "x")
	// Taking s1 again returns it unchanged; it doesn't capture v2.
	again, err := st.Snapshot(ctx, "demo", "s1")
	if err != nil {
		t.Fatalf("Snapshot(demo, s1) again error: %v", err)
	}
	if diff := cmp.Diff(s1, again); diff != "" {
		t.Errorf("Snapshot(demo, s1) again mismatch (-want +got):\n%s", diff)
	}
	s2, err := st.Snapshot(ctx, "demo", "s2")
	if err != nil {
		t.Fatalf("Snapshot(demo, s2) error: %v", err)
	}
	s0, err := st.Snapshot(ctx, "demo", "s0") // IDs don't decide the order
	if err != nil {
		t.Fatalf("Snapshot(demo, s0) error: %v", err)
	}
	snaps, err := st.Snapshots(ctx, "demo")
	if err != nil {
		t.Fatalf("Snapshots(demo) error: %v", err)
	}
	if diff := cmp.Diff([]store.Snapshot{s1, s2, s0}, snaps); diff != "" {
		t.Errorf("Snapshots(demo) mismatch (-want +got):\n%s", diff)
	}

	if err := st.Restore(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Restore(demo, s1) error: %v", err)
	}
	if got := readFile(t, filepath.Join(live, "data")); got != "v1" {
		t.Errorf("after Restore(s1): live/data = %q, want %q", got, "v1")
	}
	if _, err := os.Stat(filepath.Join(live, "extra")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("after Restore(s1): stat live/extra error = %v, want it gone", err)
	}
	// The restored live/ is writable, and writing it leaves the snapshot alone.
	writeFile(t, filepath.Join(live, "data"), "v3")
	if err := st.Restore(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Restore(demo, s1) again error: %v", err)
	}
	if got := readFile(t, filepath.Join(live, "data")); got != "v1" {
		t.Errorf("after second Restore(s1): live/data = %q, want %q", got, "v1")
	}
	if env, err := st.Get(ctx, "demo"); err != nil || env.Head != "s1" {
		t.Errorf("Get(demo) = %+v, %v, want Head s1", env, err)
	}
}

// A live.new left by a restore that crashed is garbage the next restore
// clears away.
func TestRestoreLeftover(t *testing.T) {
	st, _ := openPool(t)
	ctx := t.Context()
	if _, err := st.Create(ctx, "demo", store.CreateOptions{}); err != nil {
		t.Fatalf("Create(demo) error: %v", err)
	}
	live := livePath(t, st, "demo")
	writeFile(t, filepath.Join(live, "data"), "v1")
	if _, err := st.Snapshot(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Snapshot(demo, s1) error: %v", err)
	}
	out, err := exec.CommandContext(ctx, "btrfs", "subvolume", "create", live+".new").CombinedOutput()
	if err != nil {
		t.Fatalf("btrfs subvolume create: %v: %s", err, out)
	}
	writeFile(t, filepath.Join(live+".new", "junk"), "junk")

	if err := st.Restore(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Restore(demo, s1) with a leftover live.new error: %v", err)
	}
	if got := readFile(t, filepath.Join(live, "data")); got != "v1" {
		t.Errorf("live/data = %q, want %q", got, "v1")
	}
	if _, err := os.Stat(filepath.Join(live, "junk")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat live/junk error = %v, want it gone", err)
	}
	if _, err := os.Stat(live + ".new"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat live.new error = %v, want it gone", err)
	}
}

func TestErrors(t *testing.T) {
	st, _ := openPool(t)
	if _, err := st.Create(t.Context(), "demo", store.CreateOptions{}); err != nil {
		t.Fatalf("Create(demo) error: %v", err)
	}
	tests := []struct {
		name string
		call func(ctx context.Context) error
		want error
	}{
		{"get unknown", func(ctx context.Context) error { _, err := st.Get(ctx, "nope"); return err }, store.ErrNotFound},
		{"restore unknown snapshot", func(ctx context.Context) error { return st.Restore(ctx, "demo", "nope") }, store.ErrNotFound},
		{"restore unknown env", func(ctx context.Context) error { return st.Restore(ctx, "nope", "s1") }, store.ErrNotFound},
		{"snapshot unknown env", func(ctx context.Context) error {
			_, err := st.Snapshot(ctx, "nope", "s1")
			return err
		}, store.ErrNotFound},
		{"create invalid", func(ctx context.Context) error {
			_, err := st.Create(ctx, "Bad_ID", store.CreateOptions{})
			return err
		}, store.ErrInvalidID},
		{"get path", func(ctx context.Context) error { _, err := st.Get(ctx, "../demo"); return err }, store.ErrInvalidID},
		{"delete invalid", func(ctx context.Context) error { return st.Delete(ctx, "a/b") }, store.ErrInvalidID},
		{"snapshot invalid", func(ctx context.Context) error {
			_, err := st.Snapshot(ctx, "demo", "../x")
			return err
		}, store.ErrInvalidID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(t.Context()); !errors.Is(err, tt.want) {
				t.Errorf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestDelete(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	if _, err := st.Create(ctx, "demo", store.CreateOptions{}); err != nil {
		t.Fatalf("Create(demo) error: %v", err)
	}
	if _, err := st.Snapshot(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Snapshot(demo, s1) error: %v", err)
	}
	if err := st.Delete(ctx, "demo"); err != nil {
		t.Fatalf("Delete(demo) error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "envs", "demo")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat envs/demo error = %v, want it gone", err)
	}
	if _, err := st.Get(ctx, "demo"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get(demo) after Delete error = %v, want %v", err, store.ErrNotFound)
	}
	if err := st.Delete(ctx, "demo"); err != nil {
		t.Errorf("Delete(demo) again error: %v, want nil", err)
	}
	// The ID can be used again, with nothing left of the old environment.
	if _, err := st.Create(ctx, "demo", store.CreateOptions{}); err != nil {
		t.Fatalf("Create(demo) after Delete error: %v", err)
	}
	if snaps, err := st.Snapshots(ctx, "demo"); err != nil || len(snaps) != 0 {
		t.Errorf("Snapshots(demo) after re-create = %v, %v, want none", snaps, err)
	}
}

func TestReopen(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	env, err := st.Create(ctx, "demo", store.CreateOptions{Repo: "/src/demo"})
	if err != nil {
		t.Fatalf("Create(demo) error: %v", err)
	}
	snap, err := st.Snapshot(ctx, "demo", "s1")
	if err != nil {
		t.Fatalf("Snapshot(demo, s1) error: %v", err)
	}
	env.Head = "s1"
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	st2, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open(%s) again error: %v", dir, err)
	}
	defer func() { _ = st2.Close() }()
	got, err := st2.Get(ctx, "demo")
	if err != nil {
		t.Fatalf("Get(demo) after reopen error: %v", err)
	}
	if diff := cmp.Diff(env, got); diff != "" {
		t.Errorf("Get(demo) after reopen mismatch (-want +got):\n%s", diff)
	}
	snaps, err := st2.Snapshots(ctx, "demo")
	if err != nil {
		t.Fatalf("Snapshots(demo) after reopen error: %v", err)
	}
	if diff := cmp.Diff([]store.Snapshot{snap}, snaps); diff != "" {
		t.Errorf("Snapshots(demo) after reopen mismatch (-want +got):\n%s", diff)
	}
	if err := st2.Delete(ctx, "demo"); err != nil { // the cleanup's store is closed
		t.Errorf("Delete(demo) error: %v", err)
	}
}

func TestOpen(t *testing.T) {
	_, dir := openPool(t)
	info, err := os.Stat(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("state/ mode = %v, want 0700: the index holds the tokens", got)
	}

	other := t.TempDir()
	var fsInfo syscall.Statfs_t
	if err := syscall.Statfs(other, &fsInfo); err != nil {
		t.Fatal(err)
	}
	if fsInfo.Type == 0x9123683e { // BTRFS_SUPER_MAGIC
		t.Skip("the temporary directory is on btrfs")
	}
	if st, err := store.Open(t.Context(), other); err == nil {
		_ = st.Close()
		t.Errorf("Open(%s) on a filesystem that isn't btrfs succeeded, want an error", other)
	}
}

// A snapshot must hold writes still in the page cache: the store relies on
// btrfs flushing live/ when it takes one.
func TestSnapshotHasUnflushedWrites(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	if _, err := st.Create(ctx, "demo", store.CreateOptions{}); err != nil {
		t.Fatalf("Create(demo) error: %v", err)
	}
	data := bytes.Repeat([]byte("fern"), 1<<20)
	if err := os.WriteFile(filepath.Join(livePath(t, st, "demo"), "big"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Snapshot(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Snapshot(demo, s1) error: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "envs", "demo", "snaps", "s1", "big"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("snapshot's file differs from what was written (%d bytes, want %d)", len(got), len(data))
	}
}

// A box can make subvolumes inside its workspace; they must not stop a
// restore or a delete.
func TestNestedSubvolume(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	if _, err := st.Create(ctx, "demo", store.CreateOptions{}); err != nil {
		t.Fatalf("Create(demo) error: %v", err)
	}
	if _, err := st.Snapshot(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Snapshot(demo, s1) error: %v", err)
	}
	nested := filepath.Join(livePath(t, st, "demo"), "nested")
	nest := func() {
		t.Helper()
		if out, err := exec.CommandContext(ctx, "btrfs", "subvolume", "create", nested).CombinedOutput(); err != nil {
			t.Fatalf("btrfs subvolume create: %v: %s", err, out)
		}
		writeFile(t, filepath.Join(nested, "f"), "x")
	}
	nest()
	if err := st.Restore(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Restore(demo, s1) with a nested subvolume error: %v", err)
	}
	nest()
	if err := st.Delete(ctx, "demo"); err != nil {
		t.Fatalf("Delete(demo) with a nested subvolume error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "envs", "demo")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat envs/demo error = %v, want it gone", err)
	}
}

// What a create or delete leaves when it stops half-way is garbage, but a
// workspace the index has lost is not.
func TestCreateOverLeftovers(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	subvolume := func(id string) string {
		t.Helper()
		live := filepath.Join(dir, "envs", id, "live")
		if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.CommandContext(ctx, "btrfs", "subvolume", "create", live).CombinedOutput(); err != nil {
			t.Fatalf("btrfs subvolume create: %v: %s", err, out)
		}
		writeFile(t, filepath.Join(live, "old"), "old")
		return live
	}

	live := subvolume("half")
	writeFile(t, filepath.Join(dir, "envs", "half", "incomplete"), "")
	if _, err := st.Create(ctx, "half", store.CreateOptions{}); err != nil {
		t.Fatalf("Create(half) over a marked leftover error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(live, "old")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat live/old error = %v, want the leftover gone", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "envs", "half", "incomplete")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat incomplete error = %v, want the marker gone after Create", err)
	}

	live = subvolume("lost")
	if _, err := st.Create(ctx, "lost", store.CreateOptions{}); err == nil {
		t.Errorf("Create(lost) over an unmarked workspace succeeded, want an error")
	}
	if got := readFile(t, filepath.Join(live, "old")); got != "old" {
		t.Errorf("live/old = %q after a refused Create, want it kept", got)
	}
	if err := st.Delete(ctx, "lost"); err == nil {
		t.Errorf("Delete(lost) of an unmarked workspace succeeded, want an error")
	}
	removeByHand(t, filepath.Join(dir, "envs", "lost"), live)

	// Snapshots without a workspace are kept too.
	snap := filepath.Join(dir, "envs", "snaps-only", "snaps", "s1")
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, "snaps-only", store.CreateOptions{}); err == nil {
		t.Errorf("Create(snaps-only) over unmarked snapshots succeeded, want an error")
	}
	if _, err := os.Stat(snap); err != nil {
		t.Errorf("stat snaps/s1 after a refused Create error = %v, want it kept", err)
	}
}

// removeByHand deletes an environment directory the store refuses to touch,
// as a person would, and the subvolumes in it.
func removeByHand(t *testing.T, dir string, subvolumes ...string) {
	t.Helper()
	for _, sv := range subvolumes {
		if out, err := exec.CommandContext(context.WithoutCancel(t.Context()), "btrfs", "subvolume", "delete", sv).CombinedOutput(); err != nil {
			t.Errorf("btrfs subvolume delete %s: %v: %s", sv, err, out)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Error(err)
	}
}

// subvolumeIno is the inode number of every btrfs subvolume's root.
const subvolumeIno = 256

func ino(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Ino
}

// A workspace at a caller's path: made as an empty subvolume when nothing is
// there, found by any path inside it, snapshotted, restored and deleted there.
func TestPathWorkspace(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	path := filepath.Join(dir, "worktrees", "fern", "main")
	env, err := st.Create(ctx, "wt", store.CreateOptions{Path: path})
	if err != nil {
		t.Fatalf("Create(wt, %s) error: %v", path, err)
	}
	if env.Path != path {
		t.Errorf("Create(wt).Path = %s, want %s", env.Path, path)
	}
	if got := ino(t, path); got != subvolumeIno {
		t.Errorf("%s has inode %d, want a subvolume", path, got)
	}
	for _, p := range []string{path, filepath.Dir(path)} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if uid := info.Sys().(*syscall.Stat_t).Uid; uid != store.BoxUID {
			t.Errorf("%s is owned by %d, want %d (T3 runs as the box user)", p, uid, store.BoxUID)
		}
	}
	if err := os.MkdirAll(filepath.Join(path, "src", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, filepath.Join(path, "src", "pkg")} {
		got, err := st.FindByPath(ctx, p)
		if err != nil || got.ID != "wt" {
			t.Errorf("FindByPath(%s) = %q, %v, want wt", p, got.ID, err)
		}
	}
	if _, err := st.FindByPath(ctx, filepath.Join(dir, "worktrees", "fern")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("FindByPath(parent) error = %v, want %v", err, store.ErrNotFound)
	}

	writeFile(t, filepath.Join(path, "data"), "v1")
	if _, err := st.Snapshot(ctx, "wt", "s1"); err != nil {
		t.Fatalf("Snapshot(wt, s1) error: %v", err)
	}
	writeFile(t, filepath.Join(path, "data"), "v2")
	if err := st.Restore(ctx, "wt", "s1"); err != nil {
		t.Fatalf("Restore(wt, s1) error: %v", err)
	}
	if got := readFile(t, filepath.Join(path, "data")); got != "v1" {
		t.Errorf("after Restore: data = %q, want v1", got)
	}
	if err := st.Delete(ctx, "wt"); err != nil {
		t.Fatalf("Delete(wt) error: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat %s after Delete error = %v, want it gone", path, err)
	}
}

// A plain directory at the path (a worktree T3 made first) is adopted: the
// same files, owners and modes, now in a subvolume.
func TestAdopt(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	path := filepath.Join(dir, "worktrees", "fern", "feature")
	if err := os.MkdirAll(filepath.Join(path, "src"), 0o750); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{".git": "gitdir: /elsewhere/.git/worktrees/feature\n", "src/main.go": "package main\n"}
	for name, body := range files {
		writeFile(t, filepath.Join(path, name), body)
	}
	if err := os.Chmod(filepath.Join(path, "src", "main.go"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(filepath.Join(path, "src"), store.BoxUID, store.BoxGID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, "feat", store.CreateOptions{Path: path}); err != nil {
		t.Fatalf("Create(feat) adopting %s error: %v", path, err)
	}
	if got := ino(t, path); got != subvolumeIno {
		t.Errorf("%s has inode %d after adopt, want a subvolume", path, got)
	}
	for name, want := range files {
		if got := readFile(t, filepath.Join(path, name)); got != want {
			t.Errorf("%s = %q after adopt, want %q", name, got, want)
		}
	}
	info, err := os.Stat(filepath.Join(path, "src", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("src/main.go mode = %v after adopt, want 0600", info.Mode().Perm())
	}
	info, err = os.Stat(filepath.Join(path, "src"))
	if err != nil {
		t.Fatal(err)
	}
	if uid := info.Sys().(*syscall.Stat_t).Uid; uid != store.BoxUID || info.Mode().Perm() != 0o750 {
		t.Errorf("src is owned by %d with mode %v after adopt, want %d and 0750", uid, info.Mode().Perm(), store.BoxUID)
	}
	if _, err := os.Lstat(filepath.Join(dir, "envs", "feat", "adopt.new")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat adopt.new error = %v, want it gone", err)
	}
}

func TestCreatePathErrors(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	wt := filepath.Join(dir, "worktrees")
	if _, err := st.Create(ctx, "taken", store.CreateOptions{Path: filepath.Join(wt, "r", "taken")}); err != nil {
		t.Fatalf("Create(taken) error: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "elsewhere"), filepath.Join(wt, "r", "link")); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, id, path string
	}{
		{"outside worktrees", "a", filepath.Join(dir, "elsewhere", "x")},
		{"too shallow", "b", filepath.Join(wt, "r")},
		{"too deep", "c", filepath.Join(wt, "r", "x", "y")},
		{"dot dot", "d", filepath.Join(wt, "r", "..", "..", "envs", "x")},
		{"symlink", "e", filepath.Join(wt, "r", "link")},
		{"another environment's", "f", filepath.Join(wt, "r", "taken")},
		{"existing environment, other path", "taken", filepath.Join(wt, "r", "other")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := st.Create(t.Context(), tt.id, store.CreateOptions{Path: tt.path}); !errors.Is(err, store.ErrInvalidPath) {
				t.Errorf("Create(%s, %s) error = %v, want %v", tt.id, tt.path, err, store.ErrInvalidPath)
			}
		})
	}
}

// A worktree of a repository under repos/ gets that repository's git folder,
// which its box mounts read-only. Anything that would hand the box another
// folder gets none.
func TestWorktreeGit(t *testing.T) {
	st, dir := openPool(t)
	repos := filepath.Join(dir, "repos")
	gitDir := filepath.Join(repos, "app", ".git")
	// What `git worktree add` leaves in the repository: a folder per
	// worktree whose commondir points back at the repository's git folder.
	own := func(name string) string { return filepath.Join(gitDir, "worktrees", name) }
	for name, commondir := range map[string]string{"abs": "../..", "rel": "../..", "escape": "../../../../../elsewhere"} {
		if err := os.MkdirAll(own(name), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(own(name), "commondir"), commondir+"\n")
	}
	if err := os.MkdirAll(filepath.Join(dir, "elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repos, "app"), filepath.Join(repos, "alias")); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, id string
		dotGit   string // the worktree's .git file; "dir" makes a directory, "link" a symlink to a good one, "" nothing
		want     string
	}{
		{"worktree of a repository under repos", "abs", "gitdir: " + own("abs"), gitDir},
		{"relative gitdir", "rel", "gitdir: ../../../repos/app/.git/worktrees/rel", gitDir},
		{"commondir outside repos", "escape", "gitdir: " + own("escape"), ""},
		{"repository outside the pool", "outside", "gitdir: /elsewhere/.git/worktrees/x", ""},
		{"repository through a symlink", "symlink", "gitdir: " + filepath.Join(repos, "alias", ".git", "worktrees", "abs"), ""},
		{"a repository of its own", "own", "dir", ""},
		{"a .git symlink", "link", "link", ""},
		{"no git", "none", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, "worktrees", "app", tt.id)
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			switch tt.dotGit {
			case "":
			case "dir":
				if err := os.Mkdir(filepath.Join(path, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "link":
				good := filepath.Join(dir, "elsewhere", "dotgit")
				writeFile(t, good, "gitdir: "+own("abs")+"\n")
				if err := os.Symlink(good, filepath.Join(path, ".git")); err != nil {
					t.Fatal(err)
				}
			default:
				writeFile(t, filepath.Join(path, ".git"), tt.dotGit+"\n")
			}
			env, err := st.Create(t.Context(), tt.id, store.CreateOptions{Path: path})
			if err != nil {
				t.Fatalf("Create(%s) error: %v", tt.id, err)
			}
			if env.Git != tt.want {
				t.Errorf("Create(%s).Git = %q, want %q", tt.id, env.Git, tt.want)
			}
			got, err := st.Get(t.Context(), tt.id)
			if err != nil || got.Git != tt.want {
				t.Errorf("Get(%s).Git = %q, %v, want %q", tt.id, got.Git, err, tt.want)
			}
		})
	}
}

// An index from before workspaces had paths and git folders still opens, and
// its environments get the defaults.
func TestOpenOlderIndex(t *testing.T) {
	root := os.Getenv("FERN_TEST_BTRFS")
	if root == "" {
		t.Skip("FERN_TEST_BTRFS is not set; run lab/test-store.sh for the btrfs store tests")
	}
	dir, err := os.MkdirTemp(root, "pool")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "state", "fern.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE environments (id TEXT PRIMARY KEY, repo TEXT NOT NULL, head TEXT NOT NULL DEFAULT '',
			token TEXT NOT NULL, created_at INTEGER NOT NULL)`,
		`INSERT INTO environments (id, repo, token, created_at) VALUES ('old', '/src/old', 'tok', 0)`,
	} {
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open(%s) with an older index error: %v", dir, err)
	}
	defer func() { _ = st.Close() }()
	got, err := st.Get(t.Context(), "old")
	if err != nil {
		t.Fatalf("Get(old) error: %v", err)
	}
	want := store.Env{
		ID: "old", Repo: "/src/old", Token: "tok", Created: time.Unix(0, 0).UTC(),
		Path: filepath.Join(dir, "envs", "old", "live"), Claude: filepath.Join(dir, "envs", "old", "claude"),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Get(old) mismatch (-want +got):\n%s", diff)
	}
}

// T3 deletes checkpoints after a revert and later reuses their names: a
// snapshot taken again after DeleteSnapshot must hold the new state.
func TestDeleteSnapshot(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	if _, err := st.Create(ctx, "demo", store.CreateOptions{}); err != nil {
		t.Fatalf("Create(demo) error: %v", err)
	}
	live := livePath(t, st, "demo")
	writeFile(t, filepath.Join(live, "data"), "old")
	if _, err := st.Snapshot(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Snapshot(demo, s1) error: %v", err)
	}
	for range 2 { // deleting again is fine
		if err := st.DeleteSnapshot(ctx, "demo", "s1"); err != nil {
			t.Fatalf("DeleteSnapshot(demo, s1) error: %v", err)
		}
	}
	if snaps, err := st.Snapshots(ctx, "demo"); err != nil || len(snaps) != 0 {
		t.Errorf("Snapshots(demo) after DeleteSnapshot = %v, %v, want none", snaps, err)
	}
	snapDir := filepath.Join(dir, "envs", "demo", "snaps", "s1")
	if _, err := os.Lstat(snapDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat snaps/s1 error = %v, want it gone", err)
	}
	writeFile(t, filepath.Join(live, "data"), "new")
	if _, err := st.Snapshot(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Snapshot(demo, s1) again error: %v", err)
	}
	if got := readFile(t, filepath.Join(snapDir, "data")); got != "new" {
		t.Errorf("snapshot s1 taken again holds %q, want %q", got, "new")
	}
}

// Reconcile finishes what a crash interrupted and keeps what the index lost.
func TestReconcile(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	// A delete that stopped after removing the row: marked, no row.
	if _, err := st.Create(ctx, "gone", store.CreateOptions{Path: filepath.Join(dir, "worktrees", "r", "gone")}); err != nil {
		t.Fatalf("Create(gone) error: %v", err)
	}
	writeFile(t, filepath.Join(dir, "envs", "gone", "incomplete"), "delete "+filepath.Join(dir, "worktrees", "r", "gone")+"\n")
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "state", "fern.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `DELETE FROM environments WHERE id = 'gone'`); err != nil {
		t.Fatal(err)
	}
	// A restore that stopped before removing live.new.
	if _, err := st.Create(ctx, "kept", store.CreateOptions{}); err != nil {
		t.Fatalf("Create(kept) error: %v", err)
	}
	next := filepath.Join(dir, "envs", "kept", "live.new")
	if out, err := exec.CommandContext(ctx, "btrfs", "subvolume", "create", next).CombinedOutput(); err != nil {
		t.Fatalf("btrfs subvolume create: %v: %s", err, out)
	}
	// A workspace the index has lost.
	lostLive := filepath.Join(dir, "envs", "lost", "live")
	if err := os.MkdirAll(filepath.Dir(lostLive), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, "btrfs", "subvolume", "create", lostLive).CombinedOutput(); err != nil {
		t.Fatalf("btrfs subvolume create: %v: %s", err, out)
	}

	lost, err := st.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}
	if diff := cmp.Diff([]string{filepath.Join(dir, "envs", "lost")}, lost); diff != "" {
		t.Errorf("Reconcile() lost mismatch (-want +got):\n%s", diff)
	}
	for _, p := range []string{filepath.Join(dir, "envs", "gone"), filepath.Join(dir, "worktrees", "r", "gone"), next} {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("stat %s after Reconcile error = %v, want it gone", p, err)
		}
	}
	if _, err := os.Lstat(lostLive); err != nil {
		t.Errorf("stat %s after Reconcile error = %v, want it kept", lostLive, err)
	}
	removeByHand(t, filepath.Join(dir, "envs", "lost"), lostLive)
}
