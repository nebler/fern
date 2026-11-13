package store_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/store"
)

// A builder's failed build is a file with it: the next store reads it, a
// build that works clears it, deleting the builder takes it along, and an
// environment that is gone has none to write.
func TestBuildFailure(t *testing.T) {
	st, root := openPool(t)
	create(t, st, "golden-demo", store.CreateOptions{})
	if _, ok, err := st.BuildFailure(t.Context(), "golden-demo"); ok || err != nil {
		t.Fatalf("BuildFailure() of a builder that never failed = %t, %v; want none", ok, err)
	}
	want := store.BuildFailure{Key: "golden-0123", At: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC), Error: ".fern/setup exited with code 1"}
	if err := st.SetBuildFailure(t.Context(), "golden-demo", &want); err != nil {
		t.Fatalf("SetBuildFailure() error: %v", err)
	}
	again, err := store.Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := again.BuildFailure(t.Context(), "golden-demo")
	if !ok || err != nil {
		t.Fatalf("BuildFailure() after a reopen = %t, %v; want the failure", ok, err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("BuildFailure() mismatch (-want +got):\n%s", diff)
	}

	long := store.BuildFailure{Key: "golden-0123", Error: strings.Repeat("x", 1<<20)}
	if err := st.SetBuildFailure(t.Context(), "golden-demo", &long); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.BuildFailure(t.Context(), "golden-demo"); len(got.Error) > 8<<10 {
		t.Errorf("a long failure is kept whole: %d bytes, want at most 8 KiB", len(got.Error))
	}

	if err := st.SetBuildFailure(t.Context(), "golden-demo", nil); err != nil {
		t.Fatalf("SetBuildFailure(nil) error: %v", err)
	}
	if err := st.SetBuildFailure(t.Context(), "golden-demo", nil); err != nil {
		t.Errorf("SetBuildFailure(nil) again error: %v, want it safe to repeat", err)
	}
	if _, ok, _ := st.BuildFailure(t.Context(), "golden-demo"); ok {
		t.Error("BuildFailure() after a build that worked: still there")
	}

	if err := st.SetBuildFailure(t.Context(), "golden-demo", &want); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(t.Context(), "golden-demo"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.BuildFailure(t.Context(), "golden-demo"); ok {
		t.Error("BuildFailure() of a deleted builder: still there")
	}
	if err := st.SetBuildFailure(t.Context(), "golden-demo", &want); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetBuildFailure() for a deleted builder error = %v, want %v", err, store.ErrNotFound)
	}
}

// An environment's failed snapshot is a file with it, outside its
// workspace: the next store reads it, a snapshot that works clears it,
// deleting the environment takes it along, and one that is gone has none
// to write.
func TestSnapshotFailure(t *testing.T) {
	st, root := openPool(t)
	create(t, st, "demo", store.CreateOptions{})
	if _, ok, err := st.SnapshotFailure(t.Context(), "demo"); ok || err != nil {
		t.Fatalf("SnapshotFailure() of an environment whose snapshots worked = %t, %v; want none", ok, err)
	}
	want := store.SnapshotFailure{Snapshot: "t3-0123", At: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC), Error: "btrfs: no space left on device"}
	if err := st.SetSnapshotFailure(t.Context(), "demo", &want); err != nil {
		t.Fatalf("SetSnapshotFailure() error: %v", err)
	}
	again, err := store.Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := again.SnapshotFailure(t.Context(), "demo")
	if !ok || err != nil {
		t.Fatalf("SnapshotFailure() after a reopen = %t, %v; want the failure", ok, err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("SnapshotFailure() mismatch (-want +got):\n%s", diff)
	}
	// It is the environment's, not its workspace's: no snapshot holds it,
	// and the environment is still listed and usable with it there.
	if err := st.Snapshot(t.Context(), "demo", "s1"); err != nil {
		t.Fatalf("Snapshot() with a failure recorded: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "envs/demo/snaps/s1/snapshot-failed.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the failure is in a snapshot of the workspace: %v", err)
	}
	if _, ok, _ := st.BuildFailure(t.Context(), "demo"); ok {
		t.Error("a failed snapshot reads as a failed build")
	}

	long := store.SnapshotFailure{Snapshot: "t3-0123", Error: strings.Repeat("x", 1<<20)}
	if err := st.SetSnapshotFailure(t.Context(), "demo", &long); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.SnapshotFailure(t.Context(), "demo"); len(got.Error) > 8<<10 {
		t.Errorf("a long failure is kept whole: %d bytes, want at most 8 KiB", len(got.Error))
	}
	// A write that was cut short still says that one failed.
	if err := os.WriteFile(filepath.Join(root, "envs/demo/snapshot-failed.json"), []byte(`{"snapshot":"t3-`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := st.SnapshotFailure(t.Context(), "demo"); !ok || err != nil || got.Error == "" {
		t.Errorf("SnapshotFailure() of a file cut short = %+v, %t, %v; want a failure that says so", got, ok, err)
	}

	if err := st.SetSnapshotFailure(t.Context(), "demo", nil); err != nil {
		t.Fatalf("SetSnapshotFailure(nil) error: %v", err)
	}
	if err := st.SetSnapshotFailure(t.Context(), "demo", nil); err != nil {
		t.Errorf("SetSnapshotFailure(nil) again error: %v, want it safe to repeat", err)
	}
	if _, ok, _ := st.SnapshotFailure(t.Context(), "demo"); ok {
		t.Error("SnapshotFailure() after a snapshot that worked: still there")
	}

	if err := st.SetSnapshotFailure(t.Context(), "demo", &want); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(t.Context(), "demo"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.SnapshotFailure(t.Context(), "demo"); ok {
		t.Error("SnapshotFailure() of a deleted environment: still there")
	}
	if err := st.SetSnapshotFailure(t.Context(), "demo", &want); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetSnapshotFailure() for a deleted environment error = %v, want %v", err, store.ErrNotFound)
	}
}
