package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// fern logs prints the daemon's lines about one environment and its agent
// box, not a neighbour's whose ID starts the same, then each box's own log,
// the last lines of each.
func TestLogs(t *testing.T) {
	root := t.TempDir()
	journal := strings.Join([]string{
		`2026-11-13T09:00:01+0000 fern-daemon[1]: time=… level=INFO msg="box woke" env=t3-12 took=151ms`,
		`2026-11-13T09:00:02+0000 fern-daemon[1]: time=… level=INFO msg="box woke" env=t3-123 took=140ms`,
		`2026-11-13T09:00:03+0000 fern-daemon[1]: time=… level=INFO msg="exec closed" env=t3-12.agent exec=7`,
		`2026-11-13T09:00:04+0000 fern-daemon[1]: time=… level=WARN msg="golden build failed" source=t3-12`,
		`2026-11-13T09:00:05+0000 fern-daemon[1]: time=… level=INFO msg="fern daemon started" node=lab`,
	}, "\n")
	for path, data := range map[string]string{
		"bin/journalctl":              "#!/bin/sh\ncat <<'EOF'\n" + journal + "\nEOF\n",
		"pool/boxes/t3-12/log":        "postgres: starting\npostgres: ready\n",
		"pool/boxes/t3-12.agent/log":  "fern-init: claude started\n",
		"pool/boxes/t3-123/log":       "a neighbour's\n",
		"pool/boxes/t3-12.other/log":  "not a box of it\n",
		"pool/boxes/t3-12/tmp/unused": "",
	} {
		p := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	n := node{pool: filepath.Join(root, "pool"), journalctl: filepath.Join(root, "bin/journalctl")}
	var out strings.Builder
	if err := n.logs(t.Context(), "t3-12", "24 hours ago", 2, &out); err != nil {
		t.Fatalf("logs() error: %v", err)
	}
	want := strings.Join([]string{
		"== the daemon about t3-12, since 24 hours ago (3 lines, the last 2 shown)",
		`2026-11-13T09:00:03+0000 fern-daemon[1]: time=… level=INFO msg="exec closed" env=t3-12.agent exec=7`,
		`2026-11-13T09:00:04+0000 fern-daemon[1]: time=… level=WARN msg="golden build failed" source=t3-12`,
		"",
		"== box t3-12's own log, " + filepath.Join(root, "pool/boxes/t3-12/log") + " (the last 2 of 2 lines)",
		"postgres: starting",
		"postgres: ready",
		"",
		"== box t3-12.agent's own log, " + filepath.Join(root, "pool/boxes/t3-12.agent/log") + " (the last 1 of 1 lines)",
		"fern-init: claude started",
		"",
	}, "\n")
	if diff := cmp.Diff(want, out.String()); diff != "" {
		t.Errorf("logs(t3-12) mismatch (-want +got):\n%s", diff)
	}
	for _, id := range []string{"", "../etc", "a/b", ".agent"} {
		if err := n.logs(t.Context(), id, "24 hours ago", 2, &out); err == nil {
			t.Errorf("logs(%q) worked, want it refused: the ID becomes a path", id)
		}
	}
}
