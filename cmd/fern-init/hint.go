package main

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// hintMark starts the CLAUDE.md that fern-init writes, so it knows the file
// is its own to replace.
const hintMark = "<!-- written by Fern's fern-init; it replaces this file at every start -->"

// writeHint tells Claude where it runs: the user-level CLAUDE.md in Claude's
// folder (CLAUDE_CONFIG_DIR), which is outside the workspace, so the hint is
// never in a snapshot, a diff or the repository. A CLAUDE.md that someone
// else wrote there is left alone.
func writeHint(claudeDir string, services map[string]service) error {
	path := filepath.Join(claudeDir, "CLAUDE.md")
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil && !strings.HasPrefix(string(old), hintMark) {
		return nil
	}
	var b strings.Builder
	fmt.Fprintln(&b, hintMark)
	fmt.Fprintln(&b, "# You are in a Fern environment")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "- This thread's worktree is its own environment, in a sandboxed box. Other threads have their own copies; nothing you change here reaches them or the main branch's data.")
	if len(services) > 0 {
		fmt.Fprintf(&b, "- The box runs this repository's services (services.yaml): %s. They are running now; use them directly.\n",
			strings.Join(slices.Sorted(maps.Keys(services)), ", "))
		fmt.Fprintln(&b, "- Their data lives in the worktree (git ignores it), so T3's diff doesn't show data changes: say what you changed.")
	}
	fmt.Fprintln(&b, "- Every turn is snapshotted. T3's \"Revert files too\" brings back the files and the services' data together, so changing data is safe.")
	fmt.Fprintln(&b, "- Don't commit or push: the repository's git folder is read-only here. The person commits from T3.")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
