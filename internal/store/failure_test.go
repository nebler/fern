package store_test

import (
	"errors"
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
