package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const mib = 1 << 20

// loopNode is a node whose pool is a sparse file under a loop device, laid
// out in a temporary folder: the kernel's files, and a losetup and a btrfs
// (loopPrograms) that do to them what the real ones do to the device and
// the filesystem.
type loopNode struct {
	node
	root string
	file string // the pool's file
	free uint64 // what the disk under the file has free
}

// loopPrograms writes a losetup and a btrfs that work on the loopNode whose
// folder is in FERN_TEST_NODE, and returns their folder. Each run of either
// is a line in the node's calls, and a file named fail-<program> there
// makes that program fail, as a crash at that step would leave things. One
// folder serves a test's cases: macOS checks a new program at its first
// run, which takes a moment.
func loopPrograms(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	for name, script := range map[string]string{
		"losetup": `#!/bin/sh
n=$FERN_TEST_NODE
echo "losetup $*" >> $n/calls
[ ! -e $n/fail-losetup ] || { echo "losetup: failed" >&2; exit 1; }
echo $(( $(wc -c < $n/img/pool.img) / 512 )) > $n/sys/block/loop7/size
`,
		"btrfs": `#!/bin/sh
n=$FERN_TEST_NODE
case "$2" in
usage) printf 'Overall:\n    Device size:\t\t       %s\n    Device allocated:\t\t  1\n' "$(cat $n/fs-size)" ;;
resize)
  echo "btrfs $*" >> $n/calls
  [ ! -e $n/fail-btrfs ] || { echo "ERROR: unable to resize" >&2; exit 1; }
  echo $(( $(cat $n/sys/block/loop7/size) * 512 )) > $n/fs-size ;;
esac
`,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

// newLoopNode lays out a node whose pool is size bytes, with the programs
// in bin.
func newLoopNode(t *testing.T, bin string, size uint64) *loopNode {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FERN_TEST_NODE", root)
	l := &loopNode{root: root, file: filepath.Join(root, "img/pool.img"), free: 8 << 30}
	l.node = node{
		pool: filepath.Join(root, "pool"), proc: filepath.Join(root, "proc"), sys: filepath.Join(root, "sys"),
		btrfs: filepath.Join(bin, "btrfs"), losetup: filepath.Join(bin, "losetup"),
		room: func(path string) (uint64, uint64, error) {
			if path == filepath.Dir(l.file) {
				return 75 << 30, l.free, nil
			}
			return l.read(t, "fs-size"), 1 << 30, nil
		},
	}
	for path, data := range map[string]string{
		"img/pool.img":                      "",
		"pool/x":                            "",
		"proc/self/mountinfo":               "25 1 8:1 / / rw - ext4 /dev/sda1 rw\n36 25 0:32 / " + l.pool + " rw,noatime - btrfs /dev/loop7 rw,space_cache=v2\n",
		"sys/block/loop7/loop/backing_file": l.file + "\n",
		"sys/block/loop7/size":              fmt.Sprintf("%d\n", size/512),
		"fs-size":                           fmt.Sprintf("%d\n", size),
	} {
		p := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Truncate(l.file, int64(size)); err != nil {
		t.Fatal(err)
	}
	return l
}

// read returns the number in a file of the node.
func (l *loopNode) read(t *testing.T, name string) uint64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(l.root, name))
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	if _, err := fmt.Sscan(string(data), &n); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return n
}

// sizes returns the pool's three sizes in MiB: its file's, its loop
// device's and btrfs's.
func (l *loopNode) sizes(t *testing.T) [3]uint64 {
	t.Helper()
	fi, err := os.Stat(l.file)
	if err != nil {
		t.Fatal(err)
	}
	return [3]uint64{uint64(fi.Size()) / mib, l.read(t, "sys/block/loop7/size") * 512 / mib, l.read(t, "fs-size") / mib}
}

// calls returns the programs run since the last call, and forgets them.
func (l *loopNode) calls(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(l.root, "calls")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	var out []string
	for line := range strings.Lines(string(data)) {
		out = append(out, strings.ReplaceAll(strings.TrimSpace(line), l.pool, "POOL"))
	}
	return out
}

// fern pool grow lengthens the file, tells the loop device and has btrfs
// take the room, and refuses what it must.
func TestPoolGrow(t *testing.T) {
	both := []string{"losetup -c /dev/loop7", "btrfs filesystem resize max POOL"}
	tests := []struct {
		name      string
		want      string
		free      uint64 // of the disk under the file; 0: plenty
		source    string // what the pool is mounted from, if not the loop device
		wantCode  int
		wantErr   string    // a part of the error
		wantOut   string    // a part of what it prints
		wantSizes [3]uint64 // MiB: file, device, filesystem
		wantCalls []string
	}{
		{name: "more than now", want: "+10M", wantOut: "the pool is 70 MiB", wantSizes: [3]uint64{70, 70, 70}, wantCalls: both},
		{name: "to a size", want: "1G", wantOut: "from 60 MiB to 1.0 GiB", wantSizes: [3]uint64{1024, 1024, 1024}, wantCalls: both},
		{name: "the size it has", want: "60M", wantOut: "nothing to do", wantSizes: [3]uint64{60, 60, 60}},
		{
			name: "smaller than now", want: "50M", wantCode: 1, wantErr: "a pool is never made smaller",
			wantSizes: [3]uint64{60, 60, 60},
		},
		{
			// 70 MiB and the 2 GiB a full pool must leave the disk are more
			// than the disk has.
			name: "more than the disk under it can keep", want: "70M", free: 2<<30 + 65*mib, wantCode: 1,
			wantErr: "the server's disk can't keep a 70 MiB pool", wantSizes: [3]uint64{60, 60, 60},
		},
		{
			name: "all the disk can keep", want: "70M", free: 2<<30 + 70*mib,
			wantOut: "the server's disk under it has 2.1 GiB free, and 2.0 GiB once the pool is full", wantSizes: [3]uint64{70, 70, 70}, wantCalls: both,
		},
		{
			name: "a pool on a partition of its own", want: "+10M", source: "/dev/sda2", wantCode: 1,
			wantErr: "not a file on the server's disk", wantSizes: [3]uint64{60, 60, 60},
		},
		{name: "no unit", want: "700", wantCode: 2, wantErr: "want a size such as 70G", wantSizes: [3]uint64{60, 60, 60}},
		{name: "nothing more", want: "+0G", wantCode: 2, wantErr: "want a size such as 70G", wantSizes: [3]uint64{60, 60, 60}},
	}
	bin := loopPrograms(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newLoopNode(t, bin, 60*mib)
			if tt.free != 0 {
				l.free = tt.free
			}
			if tt.source != "" {
				mounts := "36 25 8:2 / " + l.pool + " rw - btrfs " + tt.source + " rw\n"
				if err := os.WriteFile(filepath.Join(l.root, "proc/self/mountinfo"), []byte(mounts), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var out strings.Builder
			code, err := l.grow(t.Context(), tt.want, &out)
			if code != tt.wantCode || (err != nil) != (tt.wantErr != "") || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("grow(%q) = %d, %v; want %d and an error with %q", tt.want, code, err, tt.wantCode, tt.wantErr)
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("grow(%q) printed %q, want it to say %q", tt.want, out.String(), tt.wantOut)
			}
			if diff := cmp.Diff(tt.wantSizes, l.sizes(t)); diff != "" {
				t.Errorf("the file's, the device's and btrfs's MiB (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantCalls, l.calls(t)); diff != "" {
				t.Errorf("programs run (-want +got):\n%s", diff)
			}
		})
	}
}

// A grow stopped after any of its steps leaves sizes the next one reads,
// and the same command finishes it without adding to the pool twice.
func TestPoolGrowAgain(t *testing.T) {
	tests := []struct {
		name      string
		fail      string    // the program that fails the first time
		want      string    // the size asked for, both times
		wantLeft  [3]uint64 // MiB after the first: file, device, filesystem
		wantCalls []string  // the programs the second run needs
	}{
		{
			name: "stopped after the file", fail: "losetup", want: "+10M", wantLeft: [3]uint64{70, 60, 60},
			wantCalls: []string{"losetup -c /dev/loop7", "btrfs filesystem resize max POOL"},
		},
		{
			name: "stopped after the device", fail: "btrfs", want: "+10M", wantLeft: [3]uint64{70, 70, 60},
			wantCalls: []string{"btrfs filesystem resize max POOL"},
		},
		{
			name: "stopped after the device, with a size", fail: "btrfs", want: "70M", wantLeft: [3]uint64{70, 70, 60},
			wantCalls: []string{"btrfs filesystem resize max POOL"},
		},
	}
	bin := loopPrograms(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newLoopNode(t, bin, 60*mib)
			stop := filepath.Join(l.root, "fail-"+tt.fail)
			if err := os.WriteFile(stop, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			if code, err := l.grow(t.Context(), tt.want, &out); code != 1 || err == nil || !strings.Contains(err.Error(), tt.fail) {
				t.Fatalf("grow() with a failing %s = %d, %v; want 1 and its error", tt.fail, code, err)
			}
			if diff := cmp.Diff(tt.wantLeft, l.sizes(t)); diff != "" {
				t.Errorf("MiB after the grow that stopped (-want +got):\n%s", diff)
			}
			l.calls(t)
			if err := os.Remove(stop); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if code, err := l.grow(t.Context(), tt.want, &out); code != 0 || err != nil {
				t.Fatalf("grow() again = %d, %v; want it to finish", code, err)
			}
			if !strings.Contains(out.String(), "finishing a grow that was cut short") {
				t.Errorf("grow() again printed %q, want it to say it finishes one", out.String())
			}
			if diff := cmp.Diff([3]uint64{70, 70, 70}, l.sizes(t)); diff != "" {
				t.Errorf("MiB after the second grow (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantCalls, l.calls(t)); diff != "" {
				t.Errorf("programs the second grow ran (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMost(t *testing.T) {
	const gib = 1 << 30
	tests := []struct {
		name            string
		backs, fileSize uint64
		want            string
	}{
		// 75 GiB to back it: 5% of 71 GiB is 3.55, and 71 + 3.55 fits.
		{"a share of the pool decides", 75 * gib, 60 * gib, "`fern pool grow 71G`"},
		// 20 GiB: at least 2 GiB stay free, so 18.
		{"the least decides", 20 * gib, 10 * gib, "`fern pool grow 18G`"},
		{"no room for more", 62 * gib, 60 * gib, "can't keep a larger pool"},
		{"nothing at all", 1 * gib, 60 * gib, "can't keep a larger pool"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := most(tt.backs, tt.fileSize); !strings.Contains(got, tt.want) {
				t.Errorf("most(%d, %d) = %q, want it to say %q", tt.backs, tt.fileSize, got, tt.want)
			}
		})
	}
}

func TestGrowTarget(t *testing.T) {
	const gib = 1 << 30
	tests := []struct {
		want    string
		got     uint64
		wantErr bool
	}{
		{want: "70G", got: 70 * gib},
		{want: "+10G", got: 70 * gib},
		{want: "+512M", got: 60*gib + 512*mib},
		{want: "1T", got: 1024 * gib},
		{want: "", wantErr: true},
		{want: "G", wantErr: true},
		{want: "70", wantErr: true},
		{want: "-10G", wantErr: true},
		{want: "70g", wantErr: true},
		{want: "1.5G", wantErr: true},
		{want: "++1G", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got, err := growTarget(tt.want, 60*gib)
			if got != tt.got || (err != nil) != tt.wantErr {
				t.Errorf("growTarget(%q) = %d, %v; want %d, error %t", tt.want, got, err, tt.got, tt.wantErr)
			}
		})
	}
}
