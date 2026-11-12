package store_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/nebler/fern/internal/store"
)

func TestMountSource(t *testing.T) {
	const mounts = `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
58 22 0:46 / /var/lib/fern rw,noatime shared:89 - btrfs /dev/loop0 rw,ssd,subvolid=5,subvol=/
61 22 0:50 / /run rw,nosuid shared:5 - tmpfs tmpfs rw,size=800m
70 22 0:51 / /mnt/data rw - btrfs /dev/sdb rw
71 70 0:52 / /mnt/data rw - btrfs /dev/loop3 rw
`
	tests := []struct {
		name, path, want string
	}{
		{"the pool's mount point", "/var/lib/fern", "/dev/loop0"},
		{"a folder in the pool", "/var/lib/fern/envs/demo", "/dev/loop0"},
		{"a folder beside it, on the root", "/var/lib/fern-img", "/dev/sda1"},
		{"a mount that names no device", "/run/fern", ""},
		{"the later of two mounts at one point", "/mnt/data/pool", "/dev/loop3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := store.MountSource(strings.NewReader(mounts), tt.path); got != tt.want {
				t.Errorf("MountSource(%s) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestParseUsage(t *testing.T) {
	const out = `     Total   Exclusive  Set shared  Filename
10881675264  3235479552  7646195712  worktrees/app/t3-450ce76d
2528673792   842891264   842891264  envs/t3-af480e7f
`
	got, err := store.ParseUsage([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(store.Usage{Total: 13410349056, Exclusive: 4078370816}, got); diff != "" {
		t.Errorf("ParseUsage mismatch (-want +got):\n%s", diff)
	}
	if _, err := store.ParseUsage([]byte("ERROR: not a btrfs filesystem\n")); err == nil {
		t.Error("ParseUsage of an error message returned no error")
	}
}

// A pool that is a file on another disk refuses new snapshots and
// environments when that disk is almost full, however much room the pool
// itself reports: it can't grow into a disk that has none.
func TestDiskUnderPoolFull(t *testing.T) {
	st, _ := openPool(t)
	create(t, st, "demo", store.CreateOptions{})
	// A disk with less free than the pool must leave: 1 MiB.
	disk := t.TempDir()
	if err := unix.Mount("tmpfs", disk, "tmpfs", 0, "size=1m"); err != nil {
		t.Fatalf("mount a small disk: %v", err)
	}
	t.Cleanup(func() { _ = unix.Unmount(disk, unix.MNT_DETACH) })
	store.SetDiskUnder(st, disk)

	err := st.Snapshot(t.Context(), "demo", "s1")
	if !errors.Is(err, store.ErrPoolFull) || !strings.Contains(err.Error(), "fern gc") {
		t.Errorf("Snapshot(demo, s1) error = %v, want %v, naming fern gc", err, store.ErrPoolFull)
	}
	if _, err := st.Create(t.Context(), "other", store.CreateOptions{}); !errors.Is(err, store.ErrPoolFull) {
		t.Errorf("Create(other) error = %v, want %v", err, store.ErrPoolFull)
	}
	sp, err := st.Space()
	if err != nil {
		t.Fatal(err)
	}
	if sp.Under == nil || sp.Under.Path != disk || sp.Under.Total != 1<<20 {
		t.Errorf("Space().Under = %+v, want the 1 MiB disk at %s", sp.Under, disk)
	}

	// With room on the disk again, the pool's own free space decides.
	store.SetDiskUnder(st, os.TempDir())
	if err := st.Snapshot(t.Context(), "demo", "s1"); err != nil {
		t.Errorf("Snapshot(demo, s1) with room under the pool: %v", err)
	}
}

// Open finds the disk under a pool that is a file, as lab/test-store.sh
// makes this one.
func TestOpenFindsDiskUnderPool(t *testing.T) {
	st, dir := openPool(t)
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = f.Close() }()
	dev := store.MountSource(f, dir)
	file, err := os.ReadFile(filepath.Join("/sys/block", filepath.Base(dev), "loop/backing_file"))
	if err != nil {
		t.Skipf("the test pool isn't a file: %v", err)
	}
	sp, err := st.Space()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Dir(strings.TrimSpace(string(file))); sp.Under == nil || sp.Under.Path != want {
		t.Errorf("Space().Under = %+v, want the disk of %s", sp.Under, want)
	}
}

// Usage counts an environment's workspace and its snapshots.
func TestUsage(t *testing.T) {
	st, dir := openPool(t)
	path := filepath.Join(dir, "worktrees", "repo", "wt")
	env := create(t, st, "wt", store.CreateOptions{Workspace: path})
	writeFile(t, filepath.Join(env.Workspace, "data"), strings.Repeat("x", 1<<20))
	snapshot(t, st, "wt", "s1")
	got, err := st.Usage(t.Context(), "wt")
	if err != nil {
		t.Fatal(err)
	}
	// The megabyte is in the workspace and in its snapshot: counted twice in
	// Total, and shared.
	if got.Total < 2<<20 || got.Exclusive >= 1<<20 {
		t.Errorf("Usage(wt) = %+v, want at least 2 MiB in total and less than 1 MiB of it exclusive", got)
	}
	if _, err := st.Usage(t.Context(), "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Usage(missing) error = %v, want %v", err, store.ErrNotFound)
	}
}
