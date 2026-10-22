package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// repoGitDir returns the git folder of the main repository under repos/ that
// the worktree at workspace belongs to, or "" if it isn't a worktree of one.
// `git worktree add` writes a .git file naming the worktree's own git folder,
// which lies inside the repository's and names it in its commondir file.
// Nothing outside repos/ is read: the files there are T3's.
func (s *Store) repoGitDir(workspace string) (string, error) {
	own, err := pathIn(workspace, ".git", "gitdir: ")
	if err != nil || own == "" || !within(s.repos(), own) || !isRealDir(own) {
		return "", err
	}
	common, err := pathIn(own, "commondir", "")
	// The box gets exactly this folder, so it must be what it says: under
	// repos/, holding the worktree's own folder, with no symlink on the way.
	if err != nil || common == "" || !within(s.repos(), common) || !within(common, own) || !isRealDir(common) {
		return "", err
	}
	return common, nil
}

// pathIn returns the path that the regular file dir/name holds after prefix,
// resolved against dir, or "" if there is no such file or it holds none. It
// never follows a symlink or opens a pipe.
func pathIn(dir, name, prefix string) (string, error) {
	file := filepath.Join(dir, name)
	fi, err := os.Lstat(file)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.Mode().IsRegular()) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	path, ok := strings.CutPrefix(strings.TrimSpace(string(data)), prefix)
	if !ok || path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return filepath.Clean(path), nil
}

// isRealDir reports whether path is a directory reached through no symlink.
func isRealDir(path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// within reports whether path is below dir.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
