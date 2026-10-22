package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Snapshot freezes environment id's workspace as the read-only snapshot
// snapID. Taking a snapshot that exists changes nothing, so callers can
// retry; DeleteSnapshot first to take it again.
func (s *Store) Snapshot(ctx context.Context, id, snapID string) error {
	if !snapIDPattern.MatchString(snapID) {
		return fmt.Errorf("snapshot %q: %w", snapID, ErrInvalidID)
	}
	env, err := s.Get(ctx, id)
	if err != nil {
		return err
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
		// are atomic, so it is complete. It keeps its row if it has one.
		_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO snapshots (env, id, created_at) VALUES (?, ?, ?)`,
			env.ID, snapID, now().Unix())
		return err
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Taking one flushes the workspace's dirty pages first, so it holds every
	// write made before. It replaces a row whose subvolume a DeleteSnapshot
	// that stopped half-way already removed.
	if err := btrfs(ctx, "subvolume", "snapshot", "-r", env.Workspace, path); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO snapshots (env, id, created_at) VALUES (?, ?, ?)`,
		env.ID, snapID, now().Unix())
	return err
}

// DeleteSnapshot removes snapshot snapID of environment id. Deleting one
// that doesn't exist is not an error. The subvolume goes first, so a
// snapshot on disk without a row is always one that was being taken.
func (s *Store) DeleteSnapshot(ctx context.Context, id, snapID string) error {
	if !snapIDPattern.MatchString(snapID) {
		return fmt.Errorf("snapshot %q: %w", snapID, ErrInvalidID)
	}
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	if err := deleteSubvolumes(ctx, s.snapPath(id, snapID)); err != nil {
		return fmt.Errorf("delete snapshot %s/%s: %w", id, snapID, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM snapshots WHERE env = ? AND id = ?`, id, snapID); err != nil {
		return fmt.Errorf("delete snapshot %s/%s: %w", id, snapID, err)
	}
	return nil
}

// Snapshots returns environment id's snapshots, oldest first.
func (s *Store) Snapshots(ctx context.Context, id string) ([]Snapshot, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, created_at FROM snapshots WHERE env = ? ORDER BY created_at, rowid`, id)
	if err != nil {
		return nil, fmt.Errorf("list snapshots of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var snaps []Snapshot
	for rows.Next() {
		var snap Snapshot
		var created int64
		if err := rows.Scan(&snap.ID, &created); err != nil {
			return nil, fmt.Errorf("list snapshots of %s: %w", id, err)
		}
		snap.Created = time.Unix(created, 0).UTC()
		snaps = append(snaps, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list snapshots of %s: %w", id, err)
	}
	return snaps, nil
}

// Restore replaces environment id's workspace with a writable copy of
// snapshot snapID. The environment's box must be stopped. Every step can be
// repeated after a crash.
func (s *Store) Restore(ctx context.Context, id, snapID string) error {
	if !snapIDPattern.MatchString(snapID) {
		return fmt.Errorf("snapshot %q: %w", snapID, ErrInvalidID)
	}
	env, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	var one int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM snapshots WHERE env = ? AND id = ?`, id, snapID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("snapshot %s/%s: %w", id, snapID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("restore %s to %s: %w", id, snapID, err)
	}
	if err := s.restore(ctx, env, snapID); err != nil {
		return fmt.Errorf("restore %s to %s: %w", id, snapID, err)
	}
	return nil
}

func (s *Store) restore(ctx context.Context, env Env, snapID string) error {
	next := s.restorePath(env.ID)
	// A leftover is a half-made copy, or the old workspace of a restore that
	// stopped before its last step. Either way it is garbage.
	if err := deleteSubvolumes(ctx, next); err != nil {
		return err
	}
	if err := btrfs(ctx, "subvolume", "snapshot", s.snapPath(env.ID, snapID), next); err != nil {
		return err
	}
	// One atomic step: the workspace is always either the old or the new tree.
	if err := unix.Renameat2(unix.AT_FDCWD, next, unix.AT_FDCWD, env.Workspace, unix.RENAME_EXCHANGE); err != nil {
		return fmt.Errorf("swap: %w", err)
	}
	return deleteSubvolumes(ctx, next)
}
