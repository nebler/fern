package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// fern logs prints the daemon's lines about one environment and its agent
// box, not a neighbour's whose ID starts the same, then each box's own log,
// the last lines of each: the running box's, or else the one its last stop
// kept with the environment.
func TestLogs(t *testing.T) {
	journal := strings.Join([]string{
		`2026-11-13T09:00:01+0000 fern-daemon[1]: time=… level=INFO msg="box woke" env=t3-12 took=151ms`,
		`2026-11-13T09:00:02+0000 fern-daemon[1]: time=… level=INFO msg="box woke" env=t3-123 took=140ms`,
		`2026-11-13T09:00:03+0000 fern-daemon[1]: time=… level=INFO msg="exec closed" env=t3-12.agent exec=7`,
		`2026-11-13T09:00:04+0000 fern-daemon[1]: time=… level=WARN msg="golden build failed" source=t3-12`,
		`2026-11-13T09:00:05+0000 fern-daemon[1]: time=… level=INFO msg="fern daemon started" node=lab`,
	}, "\n")
	daemon := []string{
		"== the daemon about t3-12, since 24 hours ago (3 lines, the last 2 shown)",
		`2026-11-13T09:00:03+0000 fern-daemon[1]: time=… level=INFO msg="exec closed" env=t3-12.agent exec=7`,
		`2026-11-13T09:00:04+0000 fern-daemon[1]: time=… level=WARN msg="golden build failed" source=t3-12`,
		"",
	}
	stopped := time.Date(2026, 11, 13, 9, 2, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		files map[string]string // in the pool
		want  []string          // after the daemon's lines; POOL is the pool
	}{
		{
			name: "both boxes run",
			files: map[string]string{
				"boxes/t3-12/log":         "postgres: starting\npostgres: ready\n",
				"boxes/t3-12.agent/log":   "fern-init: claude started\n",
				"boxes/t3-123/log":        "a neighbour's\n",
				"boxes/t3-12.other/log":   "not a box of it\n",
				"boxes/t3-12/tmp/unused":  "",
				"envs/t3-123/last.log":    "a neighbour's last\n",
				"envs/t3-12/env.json":     "{}",
				"envs/t3-12/last.log.new": "a copy a crash cut short\n",
			},
			want: []string{
				"== box t3-12's own log, POOL/boxes/t3-12/log (the last 2 of 2 lines)",
				"postgres: starting",
				"postgres: ready",
				"",
				"== box t3-12.agent's own log, POOL/boxes/t3-12.agent/log (the last 1 of 1 lines)",
				"fern-init: claude started",
				"",
			},
		},
		{
			name: "a box woken again after a crash, and an agent box that sleeps",
			files: map[string]string{
				"boxes/t3-12/log":           "postgres: ready\n",
				"envs/t3-12/last.log":       "postgres: out of memory\n",
				"envs/t3-12/last.agent.log": "fern-init: claude started\nclaude: turn 1\nclaude: turn 2\nfern-init: stopping\n",
			},
			want: []string{
				"== box t3-12's own log (its last run's: POOL/envs/t3-12/last.log), POOL/boxes/t3-12/log (the last 1 of 1 lines)",
				"postgres: ready",
				"",
				"== box t3-12.agent isn't running: the log of its last run, kept when it stopped (2026-11-13 09:02 UTC), POOL/envs/t3-12/last.agent.log (the last 2 of 4 lines)",
				"claude: turn 2",
				"fern-init: stopping",
				"",
			},
		},
		{
			name:  "an environment whose boxes never ran",
			files: map[string]string{"envs/t3-12/env.json": "{}"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.files["../bin/journalctl"] = "#!/bin/sh\ncat <<'EOF'\n" + journal + "\nEOF\n"
			for path, data := range tc.files {
				p := filepath.Join(root, "pool", path)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(data), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(p, stopped, stopped); err != nil {
					t.Fatal(err)
				}
			}
			n := node{pool: filepath.Join(root, "pool"), journalctl: filepath.Join(root, "bin/journalctl")}
			var out strings.Builder
			if err := n.logs(t.Context(), "t3-12", "24 hours ago", 2, &out); err != nil {
				t.Fatalf("logs() error: %v", err)
			}
			want := strings.ReplaceAll(strings.Join(append(daemon[:len(daemon):len(daemon)], tc.want...), "\n"), "POOL", n.pool)
			if diff := cmp.Diff(want, out.String()); diff != "" {
				t.Errorf("logs(t3-12) mismatch (-want +got):\n%s", diff)
			}
			for _, id := range []string{"", "../etc", "a/b", ".agent"} {
				if err := n.logs(t.Context(), id, "24 hours ago", 2, &out); err == nil {
					t.Errorf("logs(%q) worked, want it refused: the ID becomes a path", id)
				}
			}
		})
	}
}
