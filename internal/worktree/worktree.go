// Package worktree says what a T3 worktree's path means to Fern: which
// worktree a folder is in, the environment that holds it and its
// repository. Each answer is worked out from the path alone, with nothing
// on disk looked at, so the CLI, the daemon and the shell wrappers (through
// `fern worktree`) agree without asking each other.
//
// It is a package of its own, not part of internal/store, because the CLI
// uses it and builds everywhere Go does; the store only builds on Linux.
package worktree

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// Top returns the top folder of the T3 worktree that holds path in the pool
// at pool, <pool>/worktrees/<repo>/<name>, and false if path is in none.
func Top(pool, path string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(pool), filepath.Clean(path))
	if err != nil || !filepath.IsLocal(rel) {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 3 || parts[0] != "worktrees" {
		return "", false
	}
	return filepath.Join(pool, parts[0], parts[1], parts[2]), true
}

// EnvID returns the ID of the environment whose workspace is the worktree
// at top (from Top): "t3-" and the first 16 hex digits of the SHA-256 of
// the path. The same path always gives the same ID, so whichever of T3's
// checkpoint hook, a terminal (fern-shell) or OpenCode (fern-opencode) comes
// first makes the environment the others find. T3 works it out itself, in
// TypeScript: fernEnvironmentId in images/t3/fern-checkpoints.patch.
func EnvID(top string) string {
	sum := sha256.Sum256([]byte(top))
	return "t3-" + hex.EncodeToString(sum[:])[:16]
}

// Repo returns the repository of the worktree at top: its parent folder's
// name, the <repo> of worktrees/<repo>/<name>, which names the repository's
// golden builder (store.BuilderID).
func Repo(top string) string {
	return filepath.Base(filepath.Dir(top))
}
