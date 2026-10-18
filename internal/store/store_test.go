package store_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/google/go-cmp/cmp"

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
	env, err := st.Create(ctx, "demo", "/src/demo", seed)
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

	again, err := st.Create(ctx, "demo", "/other", nil)
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
	if _, err := st.Create(ctx, "demo", "", nil); err != nil {
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
	if _, err := st.Create(ctx, "demo", "", nil); err != nil {
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
	if _, err := st.Create(t.Context(), "demo", "", nil); err != nil {
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
			_, err := st.Create(ctx, "Bad_ID", "", nil)
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
	if _, err := st.Create(ctx, "demo", "", nil); err != nil {
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
	if _, err := st.Create(ctx, "demo", "", nil); err != nil {
		t.Fatalf("Create(demo) after Delete error: %v", err)
	}
	if snaps, err := st.Snapshots(ctx, "demo"); err != nil || len(snaps) != 0 {
		t.Errorf("Snapshots(demo) after re-create = %v, %v, want none", snaps, err)
	}
}

func TestReopen(t *testing.T) {
	st, dir := openPool(t)
	ctx := t.Context()
	env, err := st.Create(ctx, "demo", "/src/demo", nil)
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
