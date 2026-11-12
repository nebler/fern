package store

import (
	"context"
	"crypto/rand"
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
// isn't nil. Creating an environment that exists returns it unchanged,
// except that a workspace at a path that is gone or no longer a subvolume
// (T3 removed the worktree and made it again) is made or adopted again,
// keeping the environment's snapshots. A new environment isn't made when the
// pool is almost full (ErrPoolFull).
//
// With opts.Workspace, the workspace is at that path: if nothing is there
// yet, an empty subvolume (about 10 ms; `git worktree add` can fill it); if a
// plain directory is there, it is adopted, which costs one reflink per file.
func (s *Store) Create(ctx context.Context, id string, opts CreateOptions) (Env, error) {
	env, err := s.Get(ctx, id)
	if err == nil {
		if opts.Workspace != "" && filepath.Clean(opts.Workspace) != env.Workspace {
			return Env{}, fmt.Errorf("create %s: it exists with workspace %s: %w", id, env.Workspace, ErrInvalidPath)
		}
		if env, err = s.readopt(ctx, env); err != nil {
			return Env{}, fmt.Errorf("create %s: %w", id, err)
		}
		return env, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Env{}, err
	}
	if opts.Fork != nil && (opts.Workspace != "" || opts.Seed != nil || opts.From != nil) {
		return Env{}, fmt.Errorf("create %s: a fork's workspace is its own: %w", id, ErrInvalidPath)
	}
	if err := s.checkFree(); err != nil {
		return Env{}, fmt.Errorf("create %s: %w", id, err)
	}
	if env, err = s.create(ctx, id, opts); err != nil {
		return Env{}, fmt.Errorf("create %s: %w", id, err)
	}
	return env, nil
}

// create makes environment id, which has no record. The marker goes first,
// so whatever a create that stops leaves is garbage the next one clears, and
// the record last: with it the environment exists.
func (s *Store) create(ctx context.Context, id string, opts CreateOptions) (Env, error) {
	workspace := s.livePath(id)
	var err error
	if opts.Workspace != "" {
		if workspace, err = s.claimWorkspace(ctx, id, opts.Workspace); err != nil {
			return Env{}, err
		}
		defer s.release(workspace)
	}
	if err := s.clearLeftovers(ctx, id); err != nil {
		return Env{}, err
	}
	if err := s.writeMarker(id, ""); err != nil {
		return Env{}, err
	}
	if err := s.step("create: marked"); err != nil {
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
	var from string
	if opts.From != nil {
		if from, err = s.snapshotPath(ctx, *opts.From); err != nil {
			return Env{}, err
		}
	}
	if opts.Fork != nil {
		err = s.forkWorkspace(ctx, *opts.Fork, workspace)
	} else {
		err = s.makeWorkspace(ctx, id, workspace, from)
	}
	if err != nil {
		return Env{}, err
	}
	if err := s.step("create: workspace made"); err != nil {
		return Env{}, err
	}
	if opts.Seed != nil {
		if err := untar(ctx, workspace, opts.Seed); err != nil {
			return Env{}, err
		}
		// The record must not outlive a power cut that the files don't.
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
	rec := record{Workspace: workspace, Git: git, Created: now(), BoxToken: rand.Text()}
	if err := s.writeRecord(id, rec); err != nil {
		return Env{}, err
	}
	if err := s.step("create: recorded"); err != nil {
		return Env{}, err
	}
	// With the record written, the marker means nothing.
	if err := os.Remove(s.markerPath(id)); err != nil {
		return Env{}, err
	}
	return s.env(id, rec), nil
}

// forkWorkspace makes workspace a writable copy of snapshot src, or of
// environment src.Env's workspace if src.Snapshot is empty. Taking it
// flushes the source's writes first, as a snapshot does, so a database in
// it is as a crash would leave it, which it recovers from as from a revert.
// Its git file still names the source's worktree, whose git folder the box
// gets read-only.
func (s *Store) forkWorkspace(ctx context.Context, src SnapshotRef, workspace string) error {
	var from string
	if src.Snapshot == "" {
		env, err := s.Get(ctx, src.Env)
		if err != nil {
			return err
		}
		from = env.Workspace
	} else {
		var err error
		if from, err = s.snapshotPath(ctx, src); err != nil {
			return err
		}
	}
	if err := snapshotSubvolume(ctx, from, workspace, false); err != nil {
		return err
	}
	return os.Chown(workspace, BoxUID, BoxGID)
}

// readopt makes env's workspace a subvolume again if it is gone or a plain
// directory, and records its git folder, which a new worktree may name
// differently. Its steps are safe to repeat: Reconcile clears an adopt that
// stopped half-way.
func (s *Store) readopt(ctx context.Context, env Env) (Env, error) {
	if env.Workspace == s.livePath(env.ID) {
		return env, nil
	}
	if fi, err := os.Lstat(env.Workspace); err == nil && isSubvolume(fi) {
		return env, nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Env{}, err
	}
	// Recorded before the workspace is swapped in: a re-adopt that stops
	// after its swap isn't made again, as the workspace is a subvolume then.
	// The copy names the same folder as the directory it is made from.
	git, err := s.repoGitDir(env.Workspace)
	if err != nil {
		return Env{}, err
	}
	if git != env.Git {
		rec := record{Workspace: env.Workspace, Git: git, Created: env.Created, BoxToken: env.BoxToken}
		if err := s.writeRecord(env.ID, rec); err != nil {
			return Env{}, err
		}
		env.Git = git
	}
	if err := s.makeWorkspace(ctx, env.ID, env.Workspace, ""); err != nil {
		return Env{}, err
	}
	return env, nil
}

// claimWorkspace checks that path can be environment id's new workspace and
// returns it cleaned, claimed for id until release: no other environment
// has it, and no create under way is making one there.
func (s *Store) claimWorkspace(ctx context.Context, id, path string) (string, error) {
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
	// The look and the claim are one step: of two creates at one path, the
	// second finds the first's claim or its record.
	s.mu.Lock()
	defer s.mu.Unlock()
	if other, ok := s.claims[clean]; ok {
		return "", fmt.Errorf("%s is being made environment %s's workspace: %w", path, other, ErrInvalidPath)
	}
	envs, err := s.List(ctx)
	if err != nil {
		return "", err
	}
	for _, other := range envs {
		if other.Workspace == clean {
			return "", fmt.Errorf("%s is environment %s's workspace: %w", path, other.ID, ErrInvalidPath)
		}
	}
	s.claims[clean] = id
	return clean, nil
}

// release ends a claim claimWorkspace made.
func (s *Store) release(workspace string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claims, workspace)
}

// makeWorkspace makes workspace a subvolume owned by the box user, keeping
// what is already there; a plain directory there is adopted onto snapshot
// from, if it isn't empty.
func (s *Store) makeWorkspace(ctx context.Context, id, workspace, from string) error {
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
		if err := createSubvolume(ctx, workspace); err != nil {
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
		return s.adopt(ctx, id, workspace, from)
	}
}

// adopt turns the plain directory path into a subvolume with the same files:
// a reflink copy (no data is copied, but every file gets a new inode) that is
// swapped in with one atomic rename. With from, the subvolume starts as a
// writable copy of that golden snapshot, so it holds its build output under
// path's files. Writes to path during the copy are lost, so adopt before an
// agent starts working in it.
func (s *Store) adopt(ctx context.Context, id, path, from string) error {
	next := s.adoptPath(id)
	// What an adopt that stopped before its swap left is garbage; after it,
	// the old plain directory.
	if err := s.deleteSubvolumes(ctx, next); err != nil {
		return err
	}
	if err := os.RemoveAll(next); err != nil {
		return err
	}
	if from != "" {
		if err := snapshotSubvolume(ctx, from, next, false); err != nil {
			return err
		}
	} else if err := createSubvolume(ctx, next); err != nil {
		return err
	}
	if from != "" {
		if err := overlay(next, path); err != nil {
			return fmt.Errorf("adopt %s onto golden: %w", path, err)
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if err := os.Chmod(next, fi.Mode().Perm()); err != nil {
			return err
		}
	} else if err := run(exec.CommandContext(ctx, "cp", "-a", "--reflink=always", "-T", path, next)); err != nil {
		// -T copies path's own owner and mode onto next, and its contents into it.
		return fmt.Errorf("adopt %s: cp: %w", path, err)
	}
	if err := s.step("adopt: copied"); err != nil {
		return err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, next, unix.AT_FDCWD, path, unix.RENAME_EXCHANGE); err != nil {
		return fmt.Errorf("adopt %s: swap: %w", path, err)
	}
	if err := s.step("adopt: swapped"); err != nil {
		return err
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
	if err := run(cmd); err != nil {
		return fmt.Errorf("untar: %w", err)
	}
	return nil
}
