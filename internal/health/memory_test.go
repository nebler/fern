package health_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/api"
	"github.com/nebler/fern/internal/health"
	"github.com/nebler/fern/internal/store"
)

// cgroup makes the boxes' cgroup as the kernel shows it, in a folder: the
// named files, and no kills and no page cache unless they say.
func cgroup(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if _, ok := files["memory.events"]; !ok {
		kills(t, dir, 0)
	}
	if _, ok := files["memory.stat"]; !ok {
		files["memory.stat"] = "anon 0\nfile 0\nkernel 0\nshmem 0\n"
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// kills sets how many processes the kernel has killed in the cgroup at dir.
func kills(t *testing.T, dir string, n int) {
	t.Helper()
	events := "low 0\nhigh 12\nmax 3\noom " + strconv.Itoa(n) + "\noom_kill " + strconv.Itoa(n) + "\noom_group_kill 0\n"
	if err := os.WriteFile(filepath.Join(dir, "memory.events"), []byte(events), 0o644); err != nil {
		t.Fatal(err)
	}
}

// memoryProblems returns the node's problems with nothing wrong but what m
// finds, each as its name, what it is about and its detail.
func memoryProblems(t *testing.T, m *health.Memory) []string {
	t.Helper()
	roomy := store.Space{Pool: store.Volume{Total: 100 << 30, Free: 50 << 30}, Least: 5 << 30}
	got, err := health.Checker{Store: fakeStore{space: roomy}, Pool: t.TempDir(), Memory: m}.Problems(t.Context())
	if err != nil {
		t.Fatalf("Problems() error: %v", err)
	}
	var out []string
	for _, p := range got {
		if p.Fix == "" {
			t.Errorf("%s has no fix", p.Detail)
		}
		out = append(out, key(p)+": "+p.Detail)
	}
	return out
}

func key(p api.Problem) string {
	if p.About == "" {
		return p.Name
	}
	return p.Name + " " + p.About
}

// The boxes' memory together is a problem from the point where the kernel
// slows them, and one that can't be read is one too.
func TestMemoryNearlyUsed(t *testing.T) {
	const gib = 1 << 30
	bytes := func(n float64) string { return strconv.FormatInt(int64(n*gib), 10) + "\n" }
	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name:  "room",
			files: map[string]string{"memory.current": bytes(2), "memory.max": bytes(6), "memory.high": bytes(5.4)},
		},
		{
			name:  "just under where the kernel slows them",
			files: map[string]string{"memory.current": bytes(5.39), "memory.max": bytes(6), "memory.high": bytes(5.4)},
		},
		{
			name:  "nearly used",
			files: map[string]string{"memory.current": bytes(5.5), "memory.max": bytes(6), "memory.high": bytes(5.4)},
			want: []string{"memory: the boxes together use 5.5 GiB of the 6.0 GiB they may: from 5.4 GiB on the kernel slows them, " +
				"and at the limit it stops the box that uses the most"},
		},
		{
			name: "the page cache doesn't count",
			files: map[string]string{
				"memory.current": bytes(5.8), "memory.max": bytes(6), "memory.high": bytes(5.4),
				"memory.stat": "anon 3221225472\nfile 1288490188\nkernel 52428800\nshmem 214748364\n",
			},
		},
		{
			name: "gVisor's shared memory does",
			files: map[string]string{
				"memory.current": bytes(5.8), "memory.max": bytes(6), "memory.high": bytes(5.4),
				"memory.stat": "anon 1073741824\nfile 4294967296\nshmem 4187593113\n",
			},
			want: []string{"memory: the boxes together use 5.7 GiB of the 6.0 GiB they may"},
		},
		{
			name:  "a memory.stat that holds no number",
			files: map[string]string{"memory.current": bytes(2), "memory.max": bytes(6), "memory.high": bytes(5.4), "memory.stat": "file lots\n"},
			want:  []string{`memory: what the boxes use together can't be read: memory.stat: file is "lots"`},
		},
		{
			name:  "a limit with no point before it: a tenth before",
			files: map[string]string{"memory.current": bytes(3.7), "memory.max": bytes(4), "memory.high": "max\n"},
			want: []string{"memory: the boxes together use 3.7 GiB of the 4.0 GiB they may: from 3.6 GiB on the kernel slows them, " +
				"and at the limit it stops the box that uses the most"},
		},
		{
			name:  "no limit, as before a daemon that sets one",
			files: map[string]string{"memory.current": bytes(7), "memory.max": "max\n", "memory.high": "max\n"},
		},
		{
			name:  "a cgroup without its files",
			files: map[string]string{"memory.max": bytes(6), "memory.high": bytes(5.4)},
			want:  []string{"memory: what the boxes use together can't be read: open "},
		},
		{
			name:  "a file that holds no number",
			files: map[string]string{"memory.current": "lots\n", "memory.max": bytes(6), "memory.high": bytes(5.4)},
			want:  []string{`memory: what the boxes use together can't be read: memory.current is "lots"`},
		},
		{
			name:  "kills that can't be read",
			files: map[string]string{"memory.current": bytes(2), "memory.max": bytes(6), "memory.high": bytes(5.4), "memory.events": "low 0\n"},
			want:  []string{"memory killed: whether the kernel stopped a box for its memory can't be read: memory.events has no oom_kill"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := memoryProblems(t, health.NewMemory(cgroup(t, tt.files)))
			for i := range got {
				if i < len(tt.want) {
					got[i] = got[i][:min(len(got[i]), len(tt.want[i]))]
				}
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Problems() of the boxes' memory (-want +got):\n%s", diff)
			}
		})
	}
}

// A process the kernel kills in a box is a problem for a quarter of an hour
// after it was seen, for everyone who looks; those from before the daemon
// started are not.
func TestMemoryKills(t *testing.T) {
	const gib = 1 << 30
	synctest.Test(t, func(t *testing.T) {
		dir := cgroup(t, map[string]string{
			"memory.current": strconv.Itoa(2 * gib), "memory.max": strconv.Itoa(6 * gib), "memory.high": strconv.Itoa(5 * gib),
		})
		kills(t, dir, 4)
		// The bubble's clock starts at midnight UTC.
		m := health.NewMemory(dir)
		const why = ": a box at its own limit, or the one that used the most when the boxes together were at theirs (6.0 GiB)"
		steps := []struct {
			name  string
			after time.Duration
			kills int
			want  []string
		}{
			{name: "those before the daemon started", after: time.Minute, kills: 4},
			{
				name: "one more", after: 4 * time.Minute, kills: 5,
				want: []string{"memory killed: the kernel stopped a box for memory, killing 1 of the boxes' processes between 00:01 UTC and 00:05 UTC" + why},
			},
			{
				name: "the next to look sees it too", after: time.Minute, kills: 5,
				want: []string{"memory killed: the kernel stopped a box for memory, killing 1 of the boxes' processes between 00:01 UTC and 00:05 UTC" + why},
			},
			{
				name: "two more while it lasts", after: 4 * time.Minute, kills: 7,
				want: []string{"memory killed: the kernel stopped a box for memory, killing 3 of the boxes' processes between 00:01 UTC and 00:10 UTC" + why},
			},
			{
				name: "still, just under a quarter of an hour after the last", after: 15*time.Minute - time.Second, kills: 7,
				want: []string{"memory killed: the kernel stopped a box for memory, killing 3 of the boxes' processes between 00:01 UTC and 00:10 UTC" + why},
			},
			{name: "over", after: time.Second, kills: 7},
			{
				name: "one later is a new one", after: time.Hour, kills: 8,
				want: []string{"memory killed: the kernel stopped a box for memory, killing 1 of the boxes' processes between 00:25 UTC and 01:25 UTC" + why},
			},
			{name: "a cgroup made anew counts from nothing", after: 20 * time.Minute, kills: 0},
			{
				name: "and a kill in it is seen", after: time.Minute, kills: 1,
				want: []string{"memory killed: the kernel stopped a box for memory, killing 1 of the boxes' processes between 01:45 UTC and 01:46 UTC" + why},
			},
		}
		for _, step := range steps {
			time.Sleep(step.after)
			kills(t, dir, step.kills)
			if diff := cmp.Diff(step.want, memoryProblems(t, m)); diff != "" {
				t.Errorf("%s: Problems() of the boxes' memory (-want +got):\n%s", step.name, diff)
			}
		}
	})
}
