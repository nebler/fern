package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Retention keeps the pool from filling up (architecture §9): every turn of
// every thread leaves a snapshot, and T3 never deletes old ones.
const (
	// keepTurns is how many of an environment's snapshots Prune keeps,
	// newest first. pre-restore ones aren't counted; preRestoreKeep (in
	// snapshot.go) bounds those.
	keepTurns = 50
	// keepGoldens is how many golden snapshots PruneGoldens keeps of a
	// repository, newest first: the one its threads start from, and the two
	// before, for a worktree made from an older commit. A golden with a
	// setup script's output can be gigabytes, and each is a full copy.
	keepGoldens = 3
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
	return s.prune(ctx, id, s.keepTurns)
}

// PruneGoldens deletes builder environment id's oldest golden snapshots
// beyond the newest keepGoldens. A thread that started from one that goes
// keeps its files: its workspace is a copy, which shares the blocks.
func (s *Store) PruneGoldens(ctx context.Context, id string) error {
	return s.prune(ctx, id, min(s.keepTurns, keepGoldens))
}

// prune is Prune, keeping the newest keep snapshots.
func (s *Store) prune(ctx context.Context, id string, keep int) error {
	if err := s.prunePreRestores(ctx, id, ""); err != nil {
		return fmt.Errorf("prune %s: %w", id, err)
	}
	snaps, err := s.Snapshots(ctx, id)
	if err != nil {
		return fmt.Errorf("prune %s: %w", id, err)
	}
	// Oldest first: every one with more than keep newer than it goes.
	turns := 0
	for _, snap := range snaps {
		if !strings.HasPrefix(snap.ID, preRestorePrefix) {
			turns++
		}
	}
	for _, snap := range snaps {
		if turns <= keep {
			break
		}
		if strings.HasPrefix(snap.ID, preRestorePrefix) {
			continue
		}
		if err := s.DeleteSnapshot(ctx, id, snap.ID); err != nil {
			return fmt.Errorf("prune %s: %w", id, err)
		}
		turns--
	}
	return nil
}

// checkFree returns ErrPoolFull if the pool has less free space than a new
// snapshot or environment may leave, or the disk under it has: a pool that
// is a file on that disk grows into it, and the disk filling up fails
// everything on the node at once, T3's writes first (VPS, 2026-11-12: the
// pool had 27 GiB free on a disk with none).
func (s *Store) checkFree() error {
	sp, err := s.Space()
	if err != nil {
		return err
	}
	if sp.Pool.Free < sp.Least {
		return fmt.Errorf("%w: %.1f of %.1f GiB free; delete threads or environments you don't need",
			ErrPoolFull, gib(sp.Pool.Free), gib(sp.Pool.Total))
	}
	if sp.Under != nil && sp.Under.Free < sp.Least {
		return fmt.Errorf("%w: the pool is a file on the server's disk, which has %.1f GiB free (%s); `fern df` shows what uses it, `fern gc` frees what nothing needs",
			ErrPoolFull, gib(sp.Under.Free), sp.Under.Path)
	}
	return nil
}

func gib(bytes uint64) float64 { return float64(bytes) / (1 << 30) }

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
