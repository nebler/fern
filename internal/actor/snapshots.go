package actor

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nebler/fern/internal/store"
)

// snapshotStore is the disk as snapshots and retention use it: taking and
// deleting an environment's snapshots, and pruning every environment's.
type snapshotStore interface {
	Snapshot(ctx context.Context, id, snapID string) error
	DeleteSnapshot(ctx context.Context, id, snapID string) error
	Pruner
	List(ctx context.Context) ([]store.Env, error)
}

// snapshotRecorder keeps an environment's last failed snapshot with it
// (SnapshotFailures, which notices read).
type snapshotRecorder interface {
	SetSnapshotFailure(ctx context.Context, id string, f *store.SnapshotFailure) error
}

// snapshotter takes, deletes and prunes environments' snapshots through
// their actors, and keeps a turn's that failed. It is a part of the Group
// with what it uses alone: the disk through snapshotStore and
// snapshotRecorder, and the actors through do. A restore stays the
// Group's: it stops the environment's boxes.
type snapshotter struct {
	store    snapshotStore
	failures snapshotRecorder // nil: none is kept
	took     *prometheus.HistogramVec
	log      *slog.Logger
	do       doFunc
}

// Snapshot takes snapshot snapID of environment id, again if replace is
// set: a turn's checkpoint, which T3's hook asks for, or a person's (fern
// snapshot). T3 goes on without one that fails, and that turn then has no
// point to revert to. So the failure is kept with the environment
// (store.SnapshotFailure) until a snapshot of it works: `fern doctor` lists
// it, and the thread's Claude is told at its next start (notices).
func (g *Group) Snapshot(ctx context.Context, id, snapID string, replace bool) error {
	err := g.snapshotter.snapshot(ctx, id, snapID, replace)
	g.snapshotter.record(ctx, id, snapID, err)
	return err
}

// BackupSnapshot takes snapshot snapID of environment id for a backup. A
// backup reports its own failures (backup.json), so none is kept with the
// environment, and one that works says nothing of the turns' snapshots.
func (g *Group) BackupSnapshot(ctx context.Context, id, snapID string) error {
	return g.snapshotter.snapshot(ctx, id, snapID, false)
}

// record keeps with environment id that its snapshot snapID failed
// with err, or, if err is nil, that its snapshots work again. Not kept: a
// snapshot the pool was too full for, which is a problem and a notice of
// its own (store.Space.Full), and one of an environment that is gone or
// under a name that can't be a snapshot's, which is no turn's. One the
// actor never took, because the daemon was stopping or the caller left
// first, is kept: that turn has none either.
func (s *snapshotter) record(ctx context.Context, id, snapID string, err error) {
	keep := s.failures
	if keep == nil {
		return
	}
	var rec error
	switch {
	case err == nil:
		rec = keep.SetSnapshotFailure(ctx, id, nil)
	case errors.Is(err, store.ErrPoolFull), errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrInvalidID):
		return
	default:
		cause := err.Error()
		if errors.Is(err, ErrStopped) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			cause = "it wasn't taken: the daemon was stopping, or the caller stopped waiting for the environment's change before it (" + cause + ")"
		}
		rec = keep.SetSnapshotFailure(ctx, id, &store.SnapshotFailure{Snapshot: snapID, At: time.Now().UTC().Truncate(time.Second), Error: cause})
	}
	if rec != nil && !errors.Is(rec, store.ErrNotFound) {
		s.log.WarnContext(ctx, "recording a snapshot's result failed", slog.String("env", id), slog.Any("error", rec))
	}
}

// snapshot takes snapshot snapID of environment id through its actor.
func (s *snapshotter) snapshot(ctx context.Context, id, snapID string, replace bool) error {
	return s.do(ctx, "snapshot", id, func(ctx context.Context, _ *actor) (err error) {
		defer observe(s.took, time.Now(), &err)
		if replace {
			if err := s.store.DeleteSnapshot(ctx, id, snapID); err != nil {
				return err
			}
		}
		if err := s.store.Snapshot(ctx, id, snapID); err != nil {
			return err
		}
		// The snapshot is taken; an old one left over costs only disk, and
		// the next snapshot prunes again.
		if err := s.store.Prune(ctx, id); err != nil {
			s.log.WarnContext(ctx, "pruning old snapshots failed", slog.String("env", id), slog.Any("error", err))
		}
		return nil
	})
}

// PruneAll applies retention to every environment, as a snapshot does to
// its own: an environment nobody has taken a snapshot of since a revert
// otherwise keeps the copy the revert made, past its day. One that fails is
// logged, and the rest go on.
func (g *Group) PruneAll(ctx context.Context) error {
	return g.snapshotter.pruneAll(ctx)
}

func (s *snapshotter) pruneAll(ctx context.Context) error {
	envs, err := s.store.List(ctx)
	if err != nil {
		return err
	}
	for _, env := range envs {
		err := s.do(ctx, "prune", env.ID, func(ctx context.Context, _ *actor) error {
			// A builder keeps fewer, of goldens: until its next build it had
			// as many as it was left with.
			if store.IsBuilder(env.ID) {
				return s.store.PruneGoldens(ctx, env.ID)
			}
			return s.store.Prune(ctx, env.ID)
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log.WarnContext(ctx, "pruning old snapshots failed", slog.String("env", env.ID), slog.Any("error", err))
		}
	}
	return nil
}

// DeleteSnapshot removes snapshot snapID of environment id; removing one that
// doesn't exist is not an error.
func (g *Group) DeleteSnapshot(ctx context.Context, id, snapID string) error {
	return g.snapshotter.delete(ctx, id, snapID)
}

func (s *snapshotter) delete(ctx context.Context, id, snapID string) error {
	return s.do(ctx, "delete_snapshot", id, func(ctx context.Context, _ *actor) error {
		return s.store.DeleteSnapshot(ctx, id, snapID)
	})
}
