package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Snapshot freezes environment id's workspace as the read-only snapshot
// snapID. Taking a snapshot that exists changes nothing, so callers can
// retry; DeleteSnapshot first to take it again. It returns ErrPoolFull
// rather than fill the pool.
func (s *Store) Snapshot(ctx context.Context, id, snapID string) error {
	if err := checkSnapID(snapID); err != nil {
		return err
	}
	env, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.checkFree(); err != nil {
		return fmt.Errorf("snapshot %s/%s: %w", id, snapID, err)
	}
	if err := s.snapshot(ctx, env, snapID); err != nil {
		return fmt.Errorf("snapshot %s/%s: %w", id, snapID, err)
	}
	return nil
}

func (s *Store) snapshot(ctx context.Context, env Env, snapID string) error {
	path := s.snapPath(env.ID, snapID)
	_, err := os.Lstat(path)
	if err == nil {
		// A retry, or a snapshot taken just before a crash: btrfs snapshots
		// are atomic, so it is complete.
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(s.snapsDir(env.ID), 0o755); err != nil {
		return err
	}
	// Taking one flushes the workspace's dirty pages first, so it holds every
	// write made before. The subvolume is the whole snapshot: there is
	// nothing to record beside it.
	if err := snapshotSubvolume(ctx, env.Workspace, path, true); err != nil {
		return err
	}
	return s.step("snapshot: taken")
}

// DeleteSnapshot removes snapshot snapID of environment id. Deleting one
// that doesn't exist is not an error.
func (s *Store) DeleteSnapshot(ctx context.Context, id, snapID string) error {
	if err := checkSnapID(snapID); err != nil {
		return err
	}
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	if err := s.deleteSubvolumes(ctx, s.snapPath(id, snapID)); err != nil {
		return fmt.Errorf("delete snapshot %s/%s: %w", id, snapID, err)
	}
	return nil
}

// Snapshots returns environment id's snapshots, oldest first: the
// subvolumes in its snaps folder, each with the time btrfs says it was
// taken.
func (s *Store) Snapshots(ctx context.Context, id string) ([]Snapshot, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	dirs, err := os.ReadDir(s.snapsDir(id))
	if errors.Is(err, fs.ErrNotExist) {
		// An environment whose backup held no snapshot is restored without
		// the folder.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list snapshots of %s: %w", id, err)
	}
	snaps := make([]Snapshot, 0, len(dirs))
	for _, d := range dirs {
		fi, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted since the folder was read
		}
		if err != nil {
			return nil, fmt.Errorf("list snapshots of %s: %w", id, err)
		}
		if !snapIDPattern.MatchString(d.Name()) || !isSubvolume(fi) {
			continue
		}
		created, err := subvolumeCreated(s.snapPath(id, d.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list snapshots of %s: %w", id, err)
		}
		snaps = append(snaps, Snapshot{ID: d.Name(), Created: created})
	}
	// Two taken in the same instant are in the order of their names, so the
	// order never changes between two calls.
	slices.SortFunc(snaps, func(a, b Snapshot) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return snaps, nil
}

// Restore replaces environment id's workspace with a writable copy of
// snapshot snapID, and keeps the workspace it replaces as snapshot
// pre-restore-<time>, so a restore can be undone. Those over a day old go at
// the next restore or Reconcile. The environment's box must
// be stopped. After a crash at any step, the workspace is the old one or the
// new one, and a replaced workspace is kept.
func (s *Store) Restore(ctx context.Context, id, snapID string) error {
	if err := checkSnapID(snapID); err != nil {
		return err
	}
	env, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.snapshotPath(ctx, SnapshotRef{Env: id, Snapshot: snapID}); err != nil {
		return err
	}
	if err := s.restore(ctx, env, snapID); err != nil {
		return fmt.Errorf("restore %s to %s: %w", id, snapID, err)
	}
	return nil
}

func (s *Store) restore(ctx context.Context, env Env, snapID string) error {
	if err := s.prunePreRestores(ctx, env.ID, snapID); err != nil {
		return err
	}
	next := s.restorePath(env.ID)
	// A leftover is a half-made copy, or the old workspace of a restore that
	// stopped before its last step, which its pre-restore snapshot holds.
	// Either way it is garbage.
	if err := s.deleteSubvolumes(ctx, next); err != nil {
		return err
	}
	if err := snapshotSubvolume(ctx, s.snapPath(env.ID, snapID), next, false); err != nil {
		return err
	}
	if err := s.step("restore: copied"); err != nil {
		return err
	}
	// Before the swap, so a crash can't lose the workspace being replaced.
	if err := s.snapshot(ctx, env, preRestorePrefix+time.Now().UTC().Format(preRestoreTime)); err != nil {
		return err
	}
	if err := s.step("restore: kept"); err != nil {
		return err
	}
	// One atomic step: the workspace is always either the old or the new tree.
	if err := unix.Renameat2(unix.AT_FDCWD, next, unix.AT_FDCWD, env.Workspace, unix.RENAME_EXCHANGE); err != nil {
		return fmt.Errorf("swap: %w", err)
	}
	if err := s.step("restore: swapped"); err != nil {
		return err
	}
	return s.deleteSubvolumes(ctx, next)
}

// A restore keeps the workspace it replaces as a snapshot named for the time,
// to the nanosecond so that two restores never share one, for preRestoreKeep.
const (
	preRestorePrefix = "pre-restore-"
	preRestoreTime   = "20060102-150405.000000000"
	preRestoreKeep   = 24 * time.Hour
)

// prunePreRestores deletes environment id's pre-restore snapshots that are
// older than preRestoreKeep, except snapshot keep.
func (s *Store) prunePreRestores(ctx context.Context, id, keep string) error {
	snaps, err := s.Snapshots(ctx, id)
	if err != nil {
		return err
	}
	before := time.Now().Add(-preRestoreKeep)
	for _, snap := range snaps {
		if snap.ID == keep || !strings.HasPrefix(snap.ID, preRestorePrefix) || !preRestored(snap).Before(before) {
			continue
		}
		if err := s.DeleteSnapshot(ctx, id, snap.ID); err != nil {
			return err
		}
	}
	return nil
}

// preRestored returns when the restore that kept pre-restore snapshot snap
// ran: the time in its name, or when it was taken if its name has none.
func preRestored(snap Snapshot) time.Time {
	if t, err := time.Parse(preRestoreTime, strings.TrimPrefix(snap.ID, preRestorePrefix)); err == nil {
		return t
	}
	return snap.Created
}

// HasSnapshot reports whether snapshot ref exists: its environment does, and
// the snapshot's subvolume is there.
func (s *Store) HasSnapshot(ctx context.Context, ref SnapshotRef) (bool, error) {
	if err := checkSnapID(ref.Snapshot); err != nil {
		return false, err
	}
	if _, err := s.Get(ctx, ref.Env); errors.Is(err, ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("snapshot %s/%s: %w", ref.Env, ref.Snapshot, err)
	}
	fi, err := os.Lstat(s.snapPath(ref.Env, ref.Snapshot))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("snapshot %s/%s: %w", ref.Env, ref.Snapshot, err)
	}
	return isSubvolume(fi), nil
}

// snapshotPath returns the path of snapshot ref, which must exist.
func (s *Store) snapshotPath(ctx context.Context, ref SnapshotRef) (string, error) {
	ok, err := s.HasSnapshot(ctx, ref)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("snapshot %s/%s: %w", ref.Env, ref.Snapshot, ErrNotFound)
	}
	return s.snapPath(ref.Env, ref.Snapshot), nil
}
