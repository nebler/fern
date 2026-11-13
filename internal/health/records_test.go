package health_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/health"
	"github.com/nebler/fern/internal/store"
)

// usage is what `btrfs filesystem usage -b` prints of a pool whose device
// has unallocated bytes left, and used of the gigabyte set aside for its
// records.
func usage(unallocated, used uint64) string {
	return fmt.Sprintf(`Overall:
    Device size:		       64424509440
    Device allocated:		       53686042624
    Device unallocated:		       %d
    Used:			       49719373824

Data,single: Size:51521781760, Used:48090607616 (93.34%%)
   /dev/loop0	51521781760

Metadata,DUP: Size:1073741824, Used:%d (72.79%%)
   /dev/loop0	2147483648
`, unallocated, used)
}

// fakeBtrfs writes a btrfs that prints the file usage in dir, or fails if
// there is none, and adds a line to the file runs there each time it runs.
func fakeBtrfs(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "btrfs")
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %[1]s/runs\nexec cat %[1]s/usage 2>/dev/null\n", dir)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// runs returns how often the fake btrfs in dir has run.
func runs(t *testing.T, dir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "runs"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

const mib = 1 << 20

// A pool with no room left for btrfs's records, and none to add, is a
// problem; one with either is not, nor is a btrfs that doesn't say.
func TestRecordsProblem(t *testing.T) {
	roomy := store.Volume{Total: 100 << 30, Free: 50 << 30}
	tests := []struct {
		name  string
		usage string // what btrfs prints; "": it fails
		file  bool   // the pool is a file on the server's disk
		want  []string
		fix   string // a part of the fix
	}{
		{name: "room to add", usage: usage(10<<30, 1000*mib)},
		{name: "room left", usage: usage(1*mib, 700*mib)},
		{
			name: "neither, in a pool that is a file", usage: usage(1*mib, 924*mib), file: true,
			want: []string{"pool records: btrfs has 100 MiB left for its records and 1 MiB it could add: the pool can fail writes with free space showing"},
			fix:  "`fern pool grow +10G` on the server",
		},
		{
			name: "neither, in a pool on a partition", usage: usage(1*mib, 924*mib),
			want: []string{"pool records: btrfs has 100 MiB left for its records and 1 MiB it could add: the pool can fail writes with free space showing"},
			fix:  "give the pool's partition more room",
		},
		{name: "btrfs fails"},
		{name: "btrfs prints something else", usage: "ERROR: not a btrfs filesystem\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.usage != "" {
				if err := os.WriteFile(filepath.Join(dir, "usage"), []byte(tt.usage), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			space := store.Space{Pool: roomy, Least: 5 << 30}
			if tt.file {
				space.Under = &store.Volume{Path: "/img", Total: 75 << 30, Free: 20 << 30}
			}
			c := health.Checker{Store: fakeStore{space: space}, Pool: dir, Records: &health.Records{Btrfs: fakeBtrfs(t, dir), Pool: "/var/lib/fern"}}
			got, err := c.Problems(t.Context())
			if err != nil {
				t.Fatalf("Problems() error: %v", err)
			}
			var lines []string
			for _, p := range got {
				lines = append(lines, p.Name+": "+p.Detail)
				if !strings.Contains(p.Fix, tt.fix) || p.At.IsZero() {
					t.Errorf("the problem's fix = %q at %v, want %q in it and when btrfs was asked", p.Fix, p.At, tt.fix)
				}
			}
			if diff := cmp.Diff(tt.want, lines); diff != "" {
				t.Errorf("Problems() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// btrfs is asked at most every ten minutes, however often the node's
// problems are looked for, and a new reading then shows what has changed.
func TestRecordsAreReadEveryTenMinutes(t *testing.T) {
	dir := t.TempDir()
	btrfs := fakeBtrfs(t, dir)
	set := func(usage string) {
		if err := os.WriteFile(filepath.Join(dir, "usage"), []byte(usage), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	synctest.Test(t, func(t *testing.T) {
		c := health.Checker{
			Store: fakeStore{space: store.Space{Pool: store.Volume{Total: 100 << 30, Free: 50 << 30}, Least: 5 << 30}}, Pool: dir,
			Records: &health.Records{Btrfs: btrfs, Pool: "/var/lib/fern"},
		}
		problems := func() int {
			got, err := c.Problems(t.Context())
			if err != nil {
				t.Fatalf("Problems() error: %v", err)
			}
			return len(got)
		}
		set(usage(1*mib, 924*mib))
		steps := []struct {
			name         string
			wait         time.Duration // since the step before
			usage        string        // what btrfs prints from now on; "": as before
			wantProblems int
			wantRuns     int // how often btrfs has run by now
		}{
			{name: "the first look asks btrfs", wantProblems: 1, wantRuns: 1},
			{name: "a look a minute later doesn't", wait: time.Minute, wantProblems: 1, wantRuns: 1},
			{name: "the pool is grown: still listed", wait: time.Minute, usage: usage(10<<30, 924*mib), wantProblems: 1, wantRuns: 1},
			{name: "ten minutes after the first, btrfs is asked again", wait: 8 * time.Minute, wantProblems: 0, wantRuns: 2},
			{name: "and not again for ten more", wait: 9 * time.Minute, wantProblems: 0, wantRuns: 2},
		}
		for _, step := range steps {
			time.Sleep(step.wait)
			if step.usage != "" {
				set(step.usage)
			}
			if got := problems(); got != step.wantProblems {
				t.Errorf("%s: %d problems, want %d", step.name, got, step.wantProblems)
			}
			if got := runs(t, dir); got != step.wantRuns {
				t.Errorf("%s: btrfs has run %d times, want %d", step.name, got, step.wantRuns)
			}
		}
	})
}
