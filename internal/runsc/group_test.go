package runsc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestGroupMemory(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct {
		name        string
		total, want int64
	}{
		{name: "the 8 GB server, which has 7.7 GiB", total: 7885 * mib, want: 6349 * mib},
		{name: "a big machine", total: 64 << 30, want: 64<<30 - 1536*mib},
		{name: "a machine where the reserve is half", total: 3072 * mib, want: 1536 * mib},
		{name: "a small machine keeps half for boxes", total: 2048 * mib, want: 1024 * mib},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := GroupMemory(tc.total); got != tc.want {
				t.Errorf("GroupMemory(%d MiB) = %d MiB, want %d MiB", tc.total/mib, got/mib, tc.want/mib)
			}
		})
	}
}

func TestMachineMemory(t *testing.T) {
	for _, tc := range []struct {
		name, meminfo string
		want          int64
		wantErr       bool
	}{
		{name: "a server", meminfo: "MemTotal:        8074240 kB\nMemFree:          412345 kB\nMemAvailable:    6000000 kB\n", want: 8074240 << 10},
		{name: "not first", meminfo: "MemFree: 1 kB\nMemTotal: 2048 kB\n", want: 2048 << 10},
		{name: "none", meminfo: "MemFree: 1 kB\n", wantErr: true},
		{name: "not a number", meminfo: "MemTotal: lots kB\n", wantErr: true},
		{name: "zero", meminfo: "MemTotal: 0 kB\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "meminfo")
			if err := os.WriteFile(file, []byte(tc.meminfo), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := machineMemory(file)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("machineMemory() = %d, %v; want %d, error: %t", got, err, tc.want, tc.wantErr)
			}
		})
	}
	if _, err := machineMemory(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("machineMemory() of a missing file: no error")
	}
}

// The boxes' cgroup gets its limit, and a tenth before it the point where
// the kernel slows the boxes; a second start writes the same again.
func TestLimitGroup(t *testing.T) {
	// A cgroup as the kernel makes one: the files are there, without limits.
	dir := t.TempDir()
	for _, name := range []string{"memory.max", "memory.high"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("max\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func() map[string]string {
		got := map[string]string{}
		for _, name := range []string{"memory.max", "memory.high"} {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			got[name] = string(data)
		}
		return got
	}
	want := map[string]string{"memory.max": "6000000000", "memory.high": "5400000000"}
	for range 2 {
		if err := limitGroup(dir, 6_000_000_000); err != nil {
			t.Fatalf("limitGroup() error: %v", err)
		}
		if diff := cmp.Diff(want, read()); diff != "" {
			t.Errorf("the cgroup's files (-want +got):\n%s", diff)
		}
	}
	if err := limitGroup(dir, 0); err == nil {
		t.Error("limitGroup(0): no error")
	}
	if diff := cmp.Diff(want, read()); diff != "" {
		t.Errorf("the cgroup's files after a refused limit (-want +got):\n%s", diff)
	}
	if err := limitGroup(filepath.Join(dir, "missing"), 1<<30); err == nil {
		t.Error("limitGroup() in a cgroup that isn't there: no error")
	}
}
