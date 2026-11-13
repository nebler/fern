package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/api"
)

// fern top shows each box that is up, from its cgroup and its bundle, with
// what the daemon says holds it; then the environments, the pool, and what
// is wrong as doctor words it. The node is a folder: its pool isn't btrfs,
// which doctor's check of it says.
func TestTop(t *testing.T) {
	now := time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)
	// A box with services and its agent box; a box that has exited and waits
	// to be cleared; the parent cgroup's own files.
	up := map[string]string{
		"cgroup/cgroup.procs":            "",
		"cgroup/demo/cgroup.procs":       "101\n102\n",
		"cgroup/demo/memory.stat":        "anon 104857600\nfile 734003200\nshmem 52428800\nkernel 4096\n",
		"cgroup/demo/cpu.stat":           "usage_usec 92500000\nuser_usec 80000000\nsystem_usec 12500000\n",
		"cgroup/demo.agent/cgroup.procs": "201\n",
		"cgroup/demo.agent/memory.stat":  "anon 150994944\nshmem 2097152\n",
		"cgroup/demo.agent/cpu.stat":     "usage_usec 3200000\n",
		"cgroup/exited/cgroup.procs":     "",
		"cgroup/exited/memory.stat":      "anon 4096\n",
		"bundles/resolv.conf":            "nameserver 1.1.1.1\n",
		"pool/envs/demo/env.json":        "{}",
		"pool/envs/other/env.json":       "{}",
		"pool/envs/half/claude/settings": "an environment being made: no record yet",
	}
	pool := []string{
		"FAIL  pool           ROOT/pool isn't btrfs: the pool isn't mounted",
		"                     → mount ROOT/pool (it is in /etc/fstab), then systemctl restart fern-daemon fern-t3",
	}
	for _, tc := range []struct {
		name     string
		files    map[string]string
		started  map[string]time.Duration // how long ago each box's bundle was written
		down     bool                     // the daemon doesn't answer
		holds    []api.Holds
		problems []api.Problem
		want     []string
	}{
		{
			name:    "two boxes up and a failed build",
			files:   up,
			started: map[string]time.Duration{"demo": 12*time.Minute + 5*time.Second, "demo.agent": 45 * time.Second},
			holds: []api.Holds{
				{
					Box: "demo", Awake: true, Running: []api.Running{{Kind: "job", N: 3, Since: now.Add(-11 * time.Minute)}},
					SleepAt: now.Add(8 * time.Minute), Why: "20m0s after it was last used",
				},
				{
					Box: "demo.agent", Awake: true, Running: []api.Running{{Kind: "exec", N: 7, Since: now.Add(-45 * time.Second)}},
					Why: "stays awake while a command or a preview request runs",
				},
			},
			problems: []api.Problem{{Name: "golden build", About: "golden-app-1234", Detail: "golden-app-1234: .fern/setup exited with code 1", Fix: "mend the script"}},
			want: append([]string{
				"ENV   BOX       UP      MEMORY     CPU    HELD BY",
				"demo  services  12m05s  150.0 MiB  1m32s  job 3 (11m00s), sleeps in 8m00s: 20m0s after it was last used",
				"demo  agent     45s     146.0 MiB  3.2s   exec 7 (45s)",
				"",
				"ok    environments   2, and 2 boxes up",
				pool[0], pool[1],
				"FAIL  golden build   golden-app-1234: .fern/setup exited with code 1",
				"                     → mend the script",
			}, ""),
		},
		{
			name:  "nothing up and nothing wrong",
			files: map[string]string{"pool/envs/demo/env.json": "{}"},
			want: []string{
				"no box is up",
				"",
				"ok    environments   1, and 0 boxes up",
				pool[0], pool[1],
				"ok    problems       none that the daemon knows of",
				"",
			},
		},
		{
			name:    "the daemon is down, and a box whose bundle is gone",
			files:   up,
			started: map[string]time.Duration{"demo": 26 * time.Hour},
			down:    true,
			want: []string{
				"ENV   BOX       UP     MEMORY     CPU    HELD BY",
				"demo  services  1d02h  150.0 MiB  1m32s  ?",
				"demo  agent     ?      146.0 MiB  3.2s   ?",
				"",
				"ok    environments   2, and 2 boxes up",
				pool[0], pool[1],
				"FAIL  daemon         no word of what holds the boxes or of what is wrong: the daemon isn't running (systemctl status fern-daemon; " +
					`journalctl -u fern-daemon -n 50): GET /problems: Get "http://fern/problems": dial unix ROOT/none.sock: connect: no such file or directory`,
				"                     → systemctl status fern-daemon; journalctl -u fern-daemon -n 50",
				"",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(path, data string, at time.Time) {
				t.Helper()
				p := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(p, at, at); err != nil {
					t.Fatal(err)
				}
			}
			for path, data := range tc.files {
				write(path, data, now)
			}
			for box, ago := range tc.started {
				write(filepath.Join("bundles", box, "config.json"), "{}", now.Add(-ago))
			}
			socket := filepath.Join(root, "none.sock")
			if !tc.down {
				d := newFakeDaemon(t)
				d.problems, d.holds, socket = tc.problems, tc.holds, d.socket
			}
			n := node{
				pool: filepath.Join(root, "pool"), poolImage: filepath.Join(root, "none"),
				cgroup: filepath.Join(root, "cgroup"), bundles: filepath.Join(root, "bundles"),
			}
			var out strings.Builder
			if err := n.top(t.Context(), api.NewClient(socket), now, &out); err != nil {
				t.Fatalf("top() error: %v", err)
			}
			got := strings.ReplaceAll(out.String(), root, "ROOT")
			if diff := cmp.Diff(strings.Join(tc.want, "\n"), got); diff != "" {
				t.Errorf("top() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBrief(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, "0.0s"},
		{800 * time.Millisecond, "0.8s"},
		{9900 * time.Millisecond, "9.9s"},
		{45 * time.Second, "45s"},
		{12*time.Minute + 5*time.Second, "12m05s"},
		{3*time.Hour + 7*time.Minute + 59*time.Second, "3h07m"},
		{52 * time.Hour, "2d04h"},
	} {
		if got := brief(tc.d); got != tc.want {
			t.Errorf("brief(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
