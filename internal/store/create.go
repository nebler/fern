package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Create makes an environment, its workspace filled from opts.Seed if it
// isn't nil. Creating an environment that exists returns it unchanged.
//
// With opts.Workspace, the workspace is at that path: if nothing is there
// yet, an empty subvolume (about 10 ms; `git worktree add` can fill it); if a
// plain directory is there, it is adopted, which costs one reflink per file.
func (s *Store) Create(ctx context.Context, id string, opts CreateOptions) (Env, error) {
	env, err := s.Get(ctx, id)
	if !errors.Is(err, ErrNotFound) {
		if err == nil && opts.Workspace != "" && filepath.Clean(opts.Workspace) != env.Workspace {
			return Env{}, fmt.Errorf("create %s: it exists with workspace %s: %w", id, env.Workspace, ErrInvalidPath)
		}
		return env, err
	}
	if env, err = s.create(ctx, id, opts); err != nil {
		return Env{}, fmt.Errorf("create %s: %w", id, err)
	}
	return env, nil
}

// create makes environment id, which has no row. The marker goes first, so
// whatever a create that stops leaves is garbage the next one clears.
func (s *Store) create(ctx context.Context, id string, opts CreateOptions) (Env, error) {
	workspace := s.livePath(id)
	if opts.Workspace != "" {
		var err error
		if workspace, err = s.checkWorkspace(ctx, opts.Workspace); err != nil {
			return Env{}, err
		}
	}
	if err := s.clearLeftovers(ctx, id); err != nil {
		return Env{}, err
	}
	if err := s.writeMarker(id, ""); err != nil {
		return Env{}, err
	}
	if err := os.MkdirAll(s.snapsDir(id), 0o755); err != nil {
		return Env{}, err
	}
	// Claude's folder holds its transcripts: for the box user alone.
	if err := os.MkdirAll(s.claudePath(id), 0o700); err != nil {
		return Env{}, err
	}
	if err := os.Chown(s.claudePath(id), BoxUID, BoxGID); err != nil {
		return Env{}, err
	}
	if err := s.makeWorkspace(ctx, id, workspace); err != nil {
		return Env{}, err
	}
	if opts.Seed != nil {
		if err := untar(ctx, workspace, opts.Seed); err != nil {
			return Env{}, err
		}
		// The row must not outlive a power cut that the files don't.
		if err := syncfs(workspace); err != nil {
			return Env{}, err
		}
	}
	// Read now, before an agent has run in the workspace and could have
	// changed its .git file.
	git, err := s.repoGitDir(workspace)
	if err != nil {
		return Env{}, err
	}
	env := Env{ID: id, Workspace: workspace, Claude: s.claudePath(id), Git: git, Created: now(), BoxToken: rand.Text()}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO environments (`+envColumns+`) VALUES (?, ?, ?, ?, ?)`,
		env.ID, env.Workspace, env.Git, env.Created.Unix(), env.BoxToken); err != nil {
		return Env{}, err
	}
	// With the row written, the marker means nothing.
	if err := os.Remove(s.markerPath(id)); err != nil {
		return Env{}, err
	}
	return env, nil
}

// checkWorkspace checks that path can be a new workspace and returns it
// cleaned.
func (s *Store) checkWorkspace(ctx context.Context, path string) (string, error) {
	clean := filepath.Clean(path)
	rel, err := filepath.Rel(s.worktrees(), clean)
	parts := strings.Split(rel, string(filepath.Separator))
	if err != nil || len(parts) != 2 || !pathPartPattern.MatchString(parts[0]) || !pathPartPattern.MatchString(parts[1]) {
		return "", fmt.Errorf("%s: want %s/<repo>/<name>: %w", path, s.worktrees(), ErrInvalidPath)
	}
	// What the box mounts must be what the caller named.
	for _, p := range []string{clean, filepath.Dir(clean)} {
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is a symlink: %w", p, ErrInvalidPath)
		}
	}
	var other string
	err = s.db.QueryRowContext(ctx, `SELECT id FROM environments WHERE workspace = ?`, clean).Scan(&other)
	if err == nil {
		return "", fmt.Errorf("%s is environment %s's workspace: %w", path, other, ErrInvalidPath)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return clean, nil
}

// makeWorkspace makes workspace a subvolume owned by the box user, keeping
// what is already there.
func (s *Store) makeWorkspace(ctx context.Context, id, workspace string) error {
	fi, err := os.Lstat(workspace)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// T3 adds worktrees beside this one, as the box user. An
		// environment's own folder stays root's: the daemon deletes in it.
		if workspace != s.livePath(id) {
			parent := filepath.Dir(workspace)
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return err
			}
			if err := os.Chown(parent, BoxUID, BoxGID); err != nil {
				return err
			}
		}
		if err := btrfs(ctx, "subvolume", "create", workspace); err != nil {
			return err
		}
		return os.Chown(workspace, BoxUID, BoxGID)
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("%s is not a directory: %w", workspace, ErrInvalidPath)
	case isSubvolume(fi):
		// A create that stopped after making it, or one that had already
		// adopted it.
		return nil
	default:
		return s.adopt(ctx, id, workspace)
	}
}

// adopt turns the plain directory path into a subvolume with the same files:
// a reflink copy (no data is copied, but every file gets a new inode) that is
// swapped in with one atomic rename. Writes to path during the copy are lost,
// so adopt before an agent starts working in it.
func (s *Store) adopt(ctx context.Context, id, path string) error {
	next := s.adoptPath(id)
	if err := btrfs(ctx, "subvolume", "create", next); err != nil {
		return err
	}
	// -T copies path's own owner and mode onto next, and its contents into it.
	out, err := exec.CommandContext(ctx, "cp", "-a", "--reflink=always", "-T", path, next).CombinedOutput()
	if err != nil {
		return fmt.Errorf("adopt %s: cp: %w: %s", path, err, bytes.TrimSpace(out))
	}
	if err := unix.Renameat2(unix.AT_FDCWD, next, unix.AT_FDCWD, path, unix.RENAME_EXCHANGE); err != nil {
		return fmt.Errorf("adopt %s: swap: %w", path, err)
	}
	// next is now the old plain directory.
	return os.RemoveAll(next)
}

// untar extracts a tar stream into dir as the box user, so the files belong to
// it and a hostile archive can't use root's rights.
func untar(ctx context.Context, dir string, r io.Reader) error {
	cmd := exec.CommandContext(ctx, "tar", "-x", "--no-same-owner", "-C", dir, "-f", "-")
	cmd.Stdin = r
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: BoxUID, Gid: BoxGID}}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("untar: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
