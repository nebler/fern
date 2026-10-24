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
	"slices"
	"syscall"
	"testing"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
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

func create(t *testing.T, st *store.Store, id string, opts store.CreateOptions) store.Env {
	t.Helper()
	env, err := st.Create(t.Context(), id, opts)
	if err != nil {
		t.Fatalf("Create(%s) error: %v", id, err)
	}
	return env
}

func snapshot(t *testing.T, st *store.Store, id, snapID string) {
	t.Helper()
	if err := st.Snapshot(t.Context(), id, snapID); err != nil {
		t.Fatalf("Snapshot(%s, %s) error: %v", id, snapID, err)
	}
}

// subvolume makes a subvolume at path, as a crashed operation could have.
func subvolume(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), "btrfs", "subvolume", "create", path).CombinedOutput(); err != nil {
		t.Fatalf("btrfs subvolume create %s: %v: %s", path, err, out)
	}
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

func stat(t *testing.T, path string) (uid uint32, ino uint64, perm fs.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	return st.Uid, st.Ino, info.Mode().Perm()
}

func gone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat %s error = %v, want it gone", path, err)
	}
}

// subvolumeIno is the inode number of every btrfs subvolume's root.
const subvolumeIno = 256

func TestCreate(t *testing.T) {
	st, _ := openPool(t)
	seed := tarball(t, map[string]string{"README.md": "hello\n", "src/main.go": "package main\n"})
	env := create(t, st, "demo", store.CreateOptions{Seed: seed})
	if env.ID != "demo" || env.BoxToken == "" || env.Created.IsZero() {
		t.Errorf("Create(demo) = %+v, want ID, BoxToken and Created set", env)
	}
	for name, want := range map[string]string{"README.md": "hello\n", "src/main.go": "package main\n"} {
		if got := readFile(t, filepath.Join(env.Workspace, name)); got != want {
			t.Errorf("live/%s = %q, want %q", name, got, want)
		}
	}
	// The box runs as BoxUID and must be able to write everything.
	for _, name := range []string{".", "README.md", "src"} {
		if uid, _, _ := stat(t, filepath.Join(env.Workspace, name)); uid != store.BoxUID {
			t.Errorf("live/%s is owned by %d, want %d", name, uid, store.BoxUID)
		}
	}
	// The environment's own folder stays root's: T3 runs as the box user with
	// the whole pool mounted, and the daemon deletes inside this folder.
	if uid, _, _ := stat(t, filepath.Dir(env.Workspace)); uid != 0 {
		t.Errorf("%s is owned by %d, want root", filepath.Dir(env.Workspace), uid)
	}
	// Claude's folder: the box user's alone, beside live/ so a restore keeps it.
	if uid, _, perm := stat(t, env.Claude); uid != store.BoxUID || perm != 0o700 {
		t.Errorf("Claude's folder is owned by %d with mode %v, want %d and 0700", uid, perm, store.BoxUID)
	}
	if filepath.Dir(env.Claude) != filepath.Dir(env.Workspace) {
		t.Errorf("Claude's folder %s isn't beside the workspace %s", env.Claude, env.Workspace)
	}

	if diff := cmp.Diff(env, create(t, st, "demo", store.CreateOptions{})); diff != "" {
		t.Errorf("Create(demo) again mismatch (-want +got):\n%s", diff)
	}
	if got := readFile(t, filepath.Join(env.Workspace, "README.md")); got != "hello\n" {
		t.Errorf("after second Create: live/README.md = %q, want it unchanged", got)
	}
	list, err := st.List(t.Context())
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
	live := create(t, st, "demo", store.CreateOptions{}).Workspace
	writeFile(t, filepath.Join(live, "data"), "v1")
	snapshot(t, st, "demo", "s1")
	writeFile(t, filepath.Join(live, "data"), "v2")
	writeFile(t, filepath.Join(live, "extra"), "x")
	snapshot(t, st, "demo", "s1") // taking s1 again keeps it: it doesn't capture v2
	snapshot(t, st, "demo", "s2")
	snapshot(t, st, "demo", "s0") // IDs don't decide the order
	snaps, err := st.Snapshots(ctx, "demo")
	if err != nil {
		t.Fatalf("Snapshots(demo) error: %v", err)
	}
	var ids []string
	for _, s := range snaps {
		ids = append(ids, s.ID)
	}
	if diff := cmp.Diff([]string{"s1", "s2", "s0"}, ids); diff != "" {
		t.Errorf("Snapshots(demo) mismatch (-want +got):\n%s", diff)
	}

	if err := st.Restore(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Restore(demo, s1) error: %v", err)
	}
	if got := readFile(t, filepath.Join(live, "data")); got != "v1" {
		t.Errorf("after Restore(s1): live/data = %q, want %q", got, "v1")
	}
	gone(t, filepath.Join(live, "extra"))
	// The restored live/ is writable, and writing it leaves the snapshot alone.
	writeFile(t, filepath.Join(live, "data"), "v3")
	if err := st.Restore(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Restore(demo, s1) again error: %v", err)
	}
	if got := readFile(t, filepath.Join(live, "data")); got != "v1" {
		t.Errorf("after second Restore(s1): live/data = %q, want %q", got, "v1")
	}
}

// A live.new left by a restore that crashed is garbage the next restore
// clears away.
func TestRestoreLeftover(t *testing.T) {
	st, _ := openPool(t)
	live := create(t, st, "demo", store.CreateOptions{}).Workspace
	writeFile(t, filepath.Join(live, "data"), "v1")
	snapshot(t, st, "demo", "s1")
	subvolume(t, live+".new")
	writeFile(t, filepath.Join(live+".new", "junk"), "junk")

	if err := st.Restore(t.Context(), "demo", "s1"); err != nil {
		t.Fatalf("Restore(demo, s1) with a leftover live.new error: %v", err)
	}
	if got := readFile(t, filepath.Join(live, "data")); got != "v1" {
		t.Errorf("live/data = %q, want %q", got, "v1")
	}
	gone(t, filepath.Join(live, "junk"))
	gone(t, live+".new")
}

func TestErrors(t *testing.T) {
	st, _ := openPool(t)
	create(t, st, "demo", store.CreateOptions{})
	tests := []struct {
		name string
		call func(ctx context.Context) error
		want error
	}{
		{"get unknown", func(ctx context.Context) error { _, err := st.Get(ctx, "nope"); return err }, store.ErrNotFound},
		{"restore unknown snapshot", func(ctx context.Context) error { return st.Restore(ctx, "demo", "nope") }, store.ErrNotFound},
		{"restore unknown env", func(ctx context.Context) error { return st.Restore(ctx, "nope", "s1") }, store.ErrNotFound},
		{"snapshot unknown env", func(ctx context.Context) error { return st.Snapshot(ctx, "nope", "s1") }, store.ErrNotFound},
		{"create invalid", func(ctx context.Context) error {
			_, err := st.Create(ctx, "Bad_ID", store.CreateOptions{})
			return err
		}, store.ErrInvalidID},
		{"get path", func(ctx context.Context) error { _, err := st.Get(ctx, "../demo"); return err }, store.ErrInvalidID},
		{"delete invalid", func(ctx context.Context) error { return st.Delete(ctx, "a/b") }, store.ErrInvalidID},
		{"snapshot invalid", func(ctx context.Context) error { return st.Snapshot(ctx, "demo", "../x") }, store.ErrInvalidID},
		// The client escapes the ID, and the API hands it over unchanged: only
		// this check stops `fern rmsnap demo ../live` from deleting the workspace.
		{"delete snapshot outside snaps", func(ctx context.Context) error {
			return st.DeleteSnapshot(ctx, "demo", "../live")
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
	create(t, st, "demo", store.CreateOptions{})
	snapshot(t, st, "demo", "s1")
	if err := st.Delete(ctx, "demo"); err != nil {
		t.Fatalf("Delete(demo) error: %v", err)
	}
	gone(t, filepath.Join(dir, "envs", "demo"))
	if _, err := st.Get(ctx, "demo"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get(demo) after Delete error = %v, want %v", err, store.ErrNotFound)
	}
	if err := st.Delete(ctx, "demo"); err != nil {
		t.Errorf("Delete(demo) again error: %v, want nil", err)
	}
	// The ID can be used again, with nothing left of the old environment.
	create(t, st, "demo", store.CreateOptions{})
	if snaps, err := st.Snapshots(ctx, "demo"); err != nil || len(snaps) != 0 {
		t.Errorf("Snapshots(demo) after re-create = %v, %v, want none", snaps, err)
	}
}

func TestReopen(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	env := create(t, st, "demo", store.CreateOptions{})
	snapshot(t, st, "demo", "s1")
	snaps, err := st.Snapshots(ctx, "demo")
	if err != nil {
		t.Fatalf("Snapshots(demo) error: %v", err)
	}
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
	gotSnaps, err := st2.Snapshots(ctx, "demo")
	if err != nil {
		t.Fatalf("Snapshots(demo) after reopen error: %v", err)
	}
	if diff := cmp.Diff(snaps, gotSnaps); diff != "" {
		t.Errorf("Snapshots(demo) after reopen mismatch (-want +got):\n%s", diff)
	}
	if err := st2.Delete(ctx, "demo"); err != nil { // the cleanup's store is closed
		t.Errorf("Delete(demo) error: %v", err)
	}
}

func TestOpen(t *testing.T) {
	_, dir := openPool(t)
	if _, _, perm := stat(t, filepath.Join(dir, "state")); perm != 0o700 {
		t.Errorf("state/ mode = %v, want 0700: the index holds the box tokens", perm)
	}
	other := t.TempDir()
	var fsInfo unix.Statfs_t
	if err := unix.Statfs(other, &fsInfo); err != nil {
		t.Fatal(err)
	}
	if fsInfo.Type == unix.BTRFS_SUPER_MAGIC {
		t.Skip("the temporary directory is on btrfs")
	}
	if st, err := store.Open(t.Context(), other); err == nil {
		_ = st.Close()
		t.Errorf("Open(%s) on a filesystem that isn't btrfs succeeded, want an error", other)
	}
}

// A snapshot must hold writes still in the page cache: the store relies on
// btrfs flushing the workspace when it takes one.
func TestSnapshotHasUnflushedWrites(t *testing.T) {
	st, dir := openPool(t)
	live := create(t, st, "demo", store.CreateOptions{}).Workspace
	data := bytes.Repeat([]byte("fern"), 1<<20)
	if err := os.WriteFile(filepath.Join(live, "big"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot(t, st, "demo", "s1")
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
	nested := filepath.Join(create(t, st, "demo", store.CreateOptions{}).Workspace, "nested")
	snapshot(t, st, "demo", "s1")
	subvolume(t, nested)
	if err := st.Restore(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Restore(demo, s1) with a nested subvolume error: %v", err)
	}
	subvolume(t, nested)
	if err := st.Delete(ctx, "demo"); err != nil {
		t.Fatalf("Delete(demo) with a nested subvolume error: %v", err)
	}
	gone(t, filepath.Join(dir, "envs", "demo"))
}

// What a create or delete leaves when it stops half-way is garbage, but data
// the index has lost is not.
func TestCreateOverLeftovers(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	live := filepath.Join(dir, "envs", "half", "live")
	subvolume(t, live)
	writeFile(t, filepath.Join(live, "old"), "old")
	writeFile(t, filepath.Join(dir, "envs", "half", "incomplete"), "") // a create's marker
	create(t, st, "half", store.CreateOptions{})
	gone(t, filepath.Join(live, "old"))
	gone(t, filepath.Join(dir, "envs", "half", "incomplete"))

	// Unmarked data: a workspace, snapshots, or Claude's transcripts.
	for id, file := range map[string]string{
		"live-only":   "live/old",
		"snaps-only":  "snaps/s1/old",
		"claude-only": "claude/projects/t.jsonl",
	} {
		path := filepath.Join(dir, "envs", id, file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, "kept")
		if _, err := st.Create(ctx, id, store.CreateOptions{}); err == nil {
			t.Errorf("Create(%s) over unmarked %s succeeded, want an error", id, file)
		}
		if err := st.Delete(ctx, id); err == nil {
			t.Errorf("Delete(%s) over unmarked %s succeeded, want an error", id, file)
		}
		if got := readFile(t, path); got != "kept" {
			t.Errorf("%s = %q after a refused Create and Delete, want it kept", file, got)
		}
	}
}

// A workspace at a caller's path: made as an empty subvolume when nothing is
// there, found by any path inside it, snapshotted, restored and deleted there.
func TestPathWorkspace(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	path := filepath.Join(dir, "worktrees", "fern", "main")
	if env := create(t, st, "wt", store.CreateOptions{Workspace: path}); env.Workspace != path {
		t.Errorf("Create(wt).Workspace = %s, want %s", env.Workspace, path)
	}
	if _, ino, _ := stat(t, path); ino != subvolumeIno {
		t.Errorf("%s has inode %d, want a subvolume", path, ino)
	}
	for _, p := range []string{path, filepath.Dir(path)} {
		if uid, _, _ := stat(t, p); uid != store.BoxUID {
			t.Errorf("%s is owned by %d, want %d (T3 runs as the box user)", p, uid, store.BoxUID)
		}
	}
	if err := os.MkdirAll(filepath.Join(path, "src", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, filepath.Join(path, "src", "pkg")} {
		if got, err := st.FindByPath(ctx, p); err != nil || got.ID != "wt" {
			t.Errorf("FindByPath(%s) = %q, %v, want wt", p, got.ID, err)
		}
	}
	if _, err := st.FindByPath(ctx, filepath.Dir(path)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("FindByPath(parent) error = %v, want %v", err, store.ErrNotFound)
	}

	writeFile(t, filepath.Join(path, "data"), "v1")
	snapshot(t, st, "wt", "s1")
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
	gone(t, path)
}

// A plain directory at the path (a worktree T3 made first) is adopted: the
// same files, owners and modes, now in a subvolume.
func TestAdopt(t *testing.T) {
	st, dir := openPool(t)
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
	create(t, st, "feat", store.CreateOptions{Workspace: path})
	if _, ino, _ := stat(t, path); ino != subvolumeIno {
		t.Errorf("%s has inode %d after adopt, want a subvolume", path, ino)
	}
	for name, want := range files {
		if got := readFile(t, filepath.Join(path, name)); got != want {
			t.Errorf("%s = %q after adopt, want %q", name, got, want)
		}
	}
	if _, _, perm := stat(t, filepath.Join(path, "src", "main.go")); perm != 0o600 {
		t.Errorf("src/main.go mode = %v after adopt, want 0600", perm)
	}
	if uid, _, perm := stat(t, filepath.Join(path, "src")); uid != store.BoxUID || perm != 0o750 {
		t.Errorf("src is owned by %d with mode %v after adopt, want %d and 0750", uid, perm, store.BoxUID)
	}
	gone(t, filepath.Join(dir, "envs", "feat", "adopt.new"))
}

func TestCreatePathErrors(t *testing.T) {
	st, dir := openPool(t)
	wt := filepath.Join(dir, "worktrees")
	create(t, st, "taken", store.CreateOptions{Workspace: filepath.Join(wt, "r", "taken")})
	if err := os.MkdirAll(filepath.Join(dir, "elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{filepath.Join(wt, "r", "link"), filepath.Join(wt, "alias")} {
		if err := os.Symlink(filepath.Join(dir, "elsewhere"), link); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name, id, path string
	}{
		{"outside worktrees", "a", filepath.Join(dir, "elsewhere", "x")},
		{"too shallow", "b", filepath.Join(wt, "r")},
		{"too deep", "c", filepath.Join(wt, "r", "x", "y")},
		// Followed, the link would hand its target to the box user: T3 could
		// point it at state/, which holds every box token.
		{"repo folder is a symlink", "d", filepath.Join(wt, "alias", "x")},
		{"symlink", "e", filepath.Join(wt, "r", "link")},
		{"another environment's", "f", filepath.Join(wt, "r", "taken")},
		{"existing environment, other path", "taken", filepath.Join(wt, "r", "other")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := st.Create(t.Context(), tt.id, store.CreateOptions{Workspace: tt.path}); !errors.Is(err, store.ErrInvalidPath) {
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
			if env := create(t, st, tt.id, store.CreateOptions{Workspace: path}); env.Git != tt.want {
				t.Errorf("Create(%s).Git = %q, want %q", tt.id, env.Git, tt.want)
			}
			if got, err := st.Get(t.Context(), tt.id); err != nil || got.Git != tt.want {
				t.Errorf("Get(%s).Git = %q, %v, want %q", tt.id, got.Git, err, tt.want)
			}
		})
	}
}

// T3 deletes checkpoints after a revert and later reuses their names: a
// snapshot taken again after DeleteSnapshot must hold the new state.
func TestDeleteSnapshot(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	live := create(t, st, "demo", store.CreateOptions{}).Workspace
	writeFile(t, filepath.Join(live, "data"), "old")
	snapshot(t, st, "demo", "s1")
	for range 2 { // deleting again is fine
		if err := st.DeleteSnapshot(ctx, "demo", "s1"); err != nil {
			t.Fatalf("DeleteSnapshot(demo, s1) error: %v", err)
		}
	}
	if snaps, err := st.Snapshots(ctx, "demo"); err != nil || len(snaps) != 0 {
		t.Errorf("Snapshots(demo) after DeleteSnapshot = %v, %v, want none", snaps, err)
	}
	snapDir := filepath.Join(dir, "envs", "demo", "snaps", "s1")
	gone(t, snapDir)
	writeFile(t, filepath.Join(live, "data"), "new")
	snapshot(t, st, "demo", "s1")
	if got := readFile(t, filepath.Join(snapDir, "data")); got != "new" {
		t.Errorf("snapshot s1 taken again holds %q, want %q", got, "new")
	}
}

// Reconcile finishes what a crash interrupted and keeps what the index lost.
func TestReconcile(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "state", "fern.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	dropRow := func(id string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `DELETE FROM environments WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	// A delete that stopped after removing the row: its marker names the workspace.
	goneWorkspace := filepath.Join(dir, "worktrees", "r", "gone")
	create(t, st, "gone", store.CreateOptions{Workspace: goneWorkspace})
	writeFile(t, filepath.Join(dir, "envs", "gone", "incomplete"), goneWorkspace)
	dropRow("gone")
	// A create at a caller's path that stopped before its row: T3's worktree,
	// maybe with work in it, stays for the next Create to resume.
	half := filepath.Join(dir, "worktrees", "r", "half")
	create(t, st, "half", store.CreateOptions{Workspace: half})
	writeFile(t, filepath.Join(half, "work"), "kept")
	writeFile(t, filepath.Join(dir, "envs", "half", "incomplete"), "")
	dropRow("half")
	// A restore that stopped before removing live.new.
	create(t, st, "kept", store.CreateOptions{})
	next := filepath.Join(dir, "envs", "kept", "live.new")
	subvolume(t, next)
	// A workspace the index has lost.
	lostLive := filepath.Join(dir, "envs", "lost", "live")
	if err := os.MkdirAll(lostLive, 0o755); err != nil {
		t.Fatal(err)
	}

	lost, err := st.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}
	if diff := cmp.Diff([]string{filepath.Join(dir, "envs", "lost")}, lost); diff != "" {
		t.Errorf("Reconcile() lost mismatch (-want +got):\n%s", diff)
	}
	for _, p := range []string{filepath.Join(dir, "envs", "gone"), goneWorkspace, filepath.Join(dir, "envs", "half"), next} {
		gone(t, p)
	}
	if _, err := os.Lstat(lostLive); err != nil {
		t.Errorf("stat %s after Reconcile error = %v, want it kept", lostLive, err)
	}
	if got := readFile(t, filepath.Join(half, "work")); got != "kept" {
		t.Errorf("work in the half-made worktree = %q after Reconcile, want it kept", got)
	}
	if env := create(t, st, "half", store.CreateOptions{Workspace: half}); env.Workspace != half {
		t.Errorf("Create(half) again = %s, want the worktree at %s resumed", env.Workspace, half)
	}
}

// A restore keeps the workspace it replaces as a pre-restore snapshot, which
// can be restored in turn, and the next restore prunes those over a day old.
func TestPreRestore(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	live := create(t, st, "demo", store.CreateOptions{}).Workspace
	writeFile(t, filepath.Join(live, "f"), "v1")
	snapshot(t, st, "demo", "s1")
	writeFile(t, filepath.Join(live, "f"), "v2")
	if err := st.Restore(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Restore(demo, s1) error: %v", err)
	}
	pre := preRestores(t, st)
	if len(pre) != 1 {
		t.Fatalf("pre-restore snapshots %q, want one", pre)
	}
	if err := st.Restore(ctx, "demo", pre[0]); err != nil {
		t.Fatalf("Restore(demo, %s) error: %v", pre[0], err)
	}
	if got := readFile(t, filepath.Join(live, "f")); got != "v2" {
		t.Errorf("f = %q after undoing the restore, want %q", got, "v2")
	}

	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "state", "fern.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `UPDATE snapshots SET created_at = created_at - 2*86400 WHERE id LIKE 'pre-restore-%'`); err != nil {
		t.Fatal(err)
	}
	if err := st.Restore(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Restore(demo, s1) a day later error: %v", err)
	}
	got := preRestores(t, st)
	if len(got) != 1 || slices.Contains(pre, got[0]) {
		t.Errorf("pre-restore snapshots %q a day later, want only the new one", got)
	}
	for _, id := range pre {
		gone(t, filepath.Join(dir, "envs", "demo", "snaps", id))
	}
}
