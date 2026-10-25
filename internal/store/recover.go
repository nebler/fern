package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Delete removes environment id's index rows and disk, its workspace
// included. Deleting an environment that doesn't exist is not an error. Its
// box must be gone.
//
// It marks the environment and drops its rows, so it is never listed without
// its workspace, then clears the disk the way Reconcile or the next Create
// would after a crash.
func (s *Store) Delete(ctx context.Context, id string) error {
	env, err := s.Get(ctx, id)
	switch {
	case err == nil:
		if err := s.writeMarker(id, env.Workspace); err != nil {
			return fmt.Errorf("delete %s: %w", id, err)
		}
		if err := s.step("delete: marked"); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM environments WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete %s: %w", id, err)
		}
		if err := s.step("delete: unindexed"); err != nil {
			return err
		}
	case !errors.Is(err, ErrNotFound):
		return err
	}
	if err := s.clearLeftovers(ctx, id); err != nil {
		return fmt.Errorf("delete %s: %w", id, err)
	}
	return nil
}

// Reconcile brings the disk and the index together after a crash: it finishes
// the creates and deletes that stopped half-way, removes the leftovers of
// restores and adopts, brings the snapshot rows in line with the snapshots on
// disk, prunes old pre-restore snapshots, and returns the environment directories it keeps
// because the index has lost them.
func (s *Store) Reconcile(ctx context.Context) (lost []string, err error) {
	dirs, err := os.ReadDir(filepath.Join(s.root, "envs"))
	if err != nil {
		return nil, fmt.Errorf("reconcile: %w", err)
	}
	for _, d := range dirs {
		id := d.Name()
		if CheckEnvID(id) != nil {
			lost = append(lost, s.envDir(id))
			continue
		}
		env, err := s.Get(ctx, id)
		switch {
		case errors.Is(err, ErrNotFound):
			err := s.clearLeftovers(ctx, id)
			if errors.Is(err, errUnindexed) {
				lost = append(lost, s.envDir(id))
			} else if err != nil {
				return lost, fmt.Errorf("reconcile %s: %w", id, err)
			}
		case err != nil:
			return lost, fmt.Errorf("reconcile: %w", err)
		default:
			// A restore that stopped left live.new. So can an adopt its
			// adopt.new, when a power cut undoes the cleanup that came before
			// the row; the copy is garbage either way. A create or delete
			// that stopped next to its row left the marker.
			if err := deleteSubvolumes(ctx, s.restorePath(id), s.adoptPath(id)); err != nil {
				return lost, fmt.Errorf("reconcile %s: %w", id, err)
			}
			if err := os.RemoveAll(s.adoptPath(id)); err != nil {
				return lost, fmt.Errorf("reconcile %s: %w", id, err)
			}
			if err := os.Remove(s.markerPath(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return lost, fmt.Errorf("reconcile %s: %w", id, err)
			}
			if err := s.reconcileSnapshots(ctx, id); err != nil {
				return lost, fmt.Errorf("reconcile %s: %w", id, err)
			}
			// A golden build's BuildSource replaces its builder's live/; one
			// that stopped in between left none, and the next build makes it
			// afresh. Only that leaves an indexed environment without one.
			if _, err := os.Lstat(env.Workspace); errors.Is(err, fs.ErrNotExist) && env.Workspace == s.livePath(id) {
				if err := s.makeWorkspace(ctx, id, env.Workspace, ""); err != nil {
					return lost, fmt.Errorf("reconcile %s: %w", id, err)
				}
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return lost, fmt.Errorf("reconcile %s: %w", id, err)
			}
			if err := s.prunePreRestores(ctx, id, ""); err != nil {
				return lost, fmt.Errorf("reconcile %s: %w", id, err)
			}
		}
	}
	return lost, nil
}

// reconcileSnapshots makes environment id's snapshot rows match its
// snapshots on disk. A snapshot without a row was taken just before a crash;
// btrfs snapshots are atomic, so it is complete, and it gets the time it is
// found. A row without a snapshot is from a DeleteSnapshot that stopped
// half-way.
func (s *Store) reconcileSnapshots(ctx context.Context, id string) error {
	snaps, err := s.Snapshots(ctx, id)
	if err != nil {
		return err
	}
	indexed := make(map[string]bool, len(snaps))
	for _, snap := range snaps {
		indexed[snap.ID] = true
	}
	dirs, err := os.ReadDir(s.snapsDir(id))
	if err != nil {
		return err
	}
	var missing []string
	for _, d := range dirs {
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if !snapIDPattern.MatchString(d.Name()) || !isSubvolume(fi) {
			continue
		}
		if indexed[d.Name()] {
			delete(indexed, d.Name())
			continue
		}
		missing = append(missing, d.Name())
	}
	if len(missing) == 0 && len(indexed) == 0 {
		return nil
	}
	// The disk has already decided the result. Recover all rows atomically
	// and pay for one durable WAL commit, rather than one per snapshot.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.indexRecoveredSnapshots(ctx, tx, id, missing, indexed); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.step("reconcile: snapshot rows committed")
}

func (s *Store) indexRecoveredSnapshots(ctx context.Context, tx *sql.Tx, id string, missing []string, stale map[string]bool) error {
	insert, err := tx.PrepareContext(ctx, `INSERT INTO snapshots (env, id, created_at) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = insert.Close() }()
	created := now().Unix()
	for _, snapID := range missing {
		if _, err := insert.ExecContext(ctx, id, snapID, created); err != nil {
			return err
		}
		if err := s.step("reconcile: indexed " + snapID); err != nil {
			return err
		}
	}
	remove, err := tx.PrepareContext(ctx, `DELETE FROM snapshots WHERE env = ? AND id = ?`)
	if err != nil {
		return err
	}
	defer func() { _ = remove.Close() }()
	for snapID := range stale {
		if _, err := remove.ExecContext(ctx, id, snapID); err != nil {
			return err
		}
		if err := s.step("reconcile: unindexed " + snapID); err != nil {
			return err
		}
	}
	return nil
}

// clearLeftovers removes what a create or delete of environment id, which has
// no row, left when it stopped: a marked directory is garbage, and so is the
// workspace its marker names. It refuses an unmarked directory that holds
// data: then the index has lost it, and only a person should throw it away.
func (s *Store) clearLeftovers(ctx context.Context, id string) error {
	dir := s.envDir(id)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	workspace, err := os.ReadFile(s.markerPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		kept, err := holdsData(dir)
		if err != nil {
			return err
		}
		if kept {
			return fmt.Errorf("%s: %w", dir, errUnindexed)
		}
	} else if err != nil {
		return err
	}
	return s.removeDisk(ctx, id, strings.TrimSpace(string(workspace)))
}

// holdsData reports whether an environment directory has a live/, a
// snapshot or anything in Claude's folder.
func holdsData(dir string) (bool, error) {
	if _, err := os.Lstat(filepath.Join(dir, "live")); err == nil {
		return true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	for _, sub := range []string{"snaps", "claude"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		if len(entries) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// removeDisk deletes environment id's directory and, if workspace isn't
// empty, that workspace too. All subvolumes go in one btrfs call.
func (s *Store) removeDisk(ctx context.Context, id, workspace string) error {
	paths := []string{s.livePath(id), s.restorePath(id), s.adoptPath(id)}
	if workspace != "" && workspace != s.livePath(id) {
		paths = append(paths, workspace)
	}
	snaps, err := os.ReadDir(s.snapsDir(id))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, snap := range snaps {
		paths = append(paths, s.snapPath(id, snap.Name()))
	}
	if err := deleteSubvolumes(ctx, paths...); err != nil {
		return err
	}
	return os.RemoveAll(s.envDir(id))
}

// writeMarker records that a create or delete of environment id has begun,
// so that what it leaves is garbage. A delete names its workspace, which goes
// with it; a create names none: the next Create resumes a workspace at a
// caller's path.
func (s *Store) writeMarker(id, workspace string) error {
	if err := os.MkdirAll(s.envDir(id), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.markerPath(id), []byte(workspace), 0o644)
}
