package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// Retention keeps the pool from filling up (architecture §9): every turn of
// every thread leaves a snapshot, and T3 never deletes old ones.
const (
	// keepTurns is how many of an environment's snapshots Prune keeps,
	// newest first. pre-restore ones aren't counted; preRestoreKeep (in
	// snapshot.go) bounds those.
	keepTurns = 50
	// New snapshots and environments are refused when less than
	// minFreeShare of the pool, and at least minFreeBytes, is free: a box
	// keeps working, but the disk never fills.
	minFreeShare = 0.05
	minFreeBytes = 2 << 30
)

// ErrPoolFull means the pool has too little free space for a new snapshot
// or environment.
var ErrPoolFull = errors.New("the pool is almost full")

// Prune deletes environment id's oldest snapshots beyond the newest
// keepTurns, and its pre-restore ones older than preRestoreKeep. A revert to
// a turn whose snapshot is gone is refused with the reason.
func (s *Store) Prune(ctx context.Context, id string) error {
	if err := s.prunePreRestores(ctx, id, ""); err != nil {
		return fmt.Errorf("prune %s: %w", id, err)
	}
	old, err := column[string](ctx, s.db,
		`SELECT id FROM snapshots WHERE env = ? AND id NOT LIKE ? ORDER BY created_at DESC, rowid DESC LIMIT -1 OFFSET ?`,
		id, preRestorePrefix+"%", s.keepTurns)
	if err != nil {
		return fmt.Errorf("prune %s: %w", id, err)
	}
	for _, snapID := range old {
		if err := s.DeleteSnapshot(ctx, id, snapID); err != nil {
			return fmt.Errorf("prune %s: %w", id, err)
		}
	}
	return nil
}

// checkFree returns ErrPoolFull if the pool has less free space than a new
// snapshot or environment may leave.
func (s *Store) checkFree() error {
	var st unix.Statfs_t
	if err := unix.Statfs(s.root, &st); err != nil {
		return err
	}
	free, total := st.Bavail*uint64(st.Bsize), st.Blocks*uint64(st.Bsize)
	if free < max(s.minFreeBytes, uint64(float64(total)*minFreeShare)) {
		return fmt.Errorf("%w: %.1f of %.1f GiB free; delete threads or environments you don't need",
			ErrPoolFull, float64(free)/(1<<30), float64(total)/(1<<30))
	}
	return nil
}

// Orphans returns the environments whose workspace, a worktree at a path,
// is gone: T3 removed the worktree, as it does when its thread is deleted.
// T3 may also make it again, at the same path, when its thread resumes;
// Create then adopts it back.
func (s *Store) Orphans(ctx context.Context) ([]string, error) {
	envs, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	var gone []string
	for _, env := range envs {
		if env.Workspace == s.livePath(env.ID) {
			continue
		}
		if _, err := os.Lstat(env.Workspace); errors.Is(err, fs.ErrNotExist) {
			gone = append(gone, env.ID)
		} else if err != nil {
			return nil, err
		}
	}
	return gone, nil
}
