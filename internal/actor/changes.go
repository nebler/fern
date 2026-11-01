package actor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/setup"
	"github.com/nebler/fern/internal/store"
)

// Create makes environment id. Its box comes with its first wake. A new
// environment at a worktree whose repository has a golden snapshot for its
// setup starts from it; without one, it keeps its first workspace for a
// golden build, so the next one does.
func (g *Group) Create(ctx context.Context, id string, opts store.CreateOptions) error {
	return g.do(ctx, "create", id, func(ctx context.Context, _ *actor) error {
		build := false
		if _, err := g.store.Get(ctx, id); errors.Is(err, store.ErrNotFound) && opts.Workspace != "" && opts.Seed == nil {
			opts.From, build = g.golden(ctx, id, opts.Workspace)
		}
		_, err := g.store.Create(ctx, id, opts)
		if err != nil && opts.From != nil {
			// Such as a build that pruned the golden since it was looked up:
			// a thread starts without one rather than not at all. A create
			// that stopped is cleared by the next.
			g.log.WarnContext(ctx, "creating from a golden failed; creating without", slog.String("env", id), slog.Any("error", err))
			opts.From = nil
			_, err = g.store.Create(ctx, id, opts)
		}
		if err != nil {
			return err
		}
		if !build {
			return nil
		}
		if err := g.store.Snapshot(ctx, id, goldenSource); err != nil {
			g.log.WarnContext(ctx, "keeping a golden source failed", slog.String("env", id), slog.Any("error", err))
			return nil
		}
		g.queueBuild(ctx, id)
		return nil
	})
}

// Delete stops environment id's box, then removes its disk.
func (g *Group) Delete(ctx context.Context, id string) error {
	return g.do(ctx, "delete", id, func(ctx context.Context, a *actor) error {
		if _, err := g.store.Get(ctx, id); err != nil {
			return err
		}
		if err := a.stopBox(ctx); err != nil {
			return err
		}
		if err := g.stopAgent(ctx, id); err != nil {
			return err
		}
		return g.store.Delete(ctx, id)
	})
}

// Snapshot takes snapshot snapID of environment id, again if replace is set.
func (g *Group) Snapshot(ctx context.Context, id, snapID string, replace bool) error {
	return g.do(ctx, "snapshot", id, func(ctx context.Context, _ *actor) (err error) {
		defer observe(g.snapshots, time.Now(), &err)
		if replace {
			if err := g.store.DeleteSnapshot(ctx, id, snapID); err != nil {
				return err
			}
		}
		if err := g.store.Snapshot(ctx, id, snapID); err != nil {
			return err
		}
		// The snapshot is taken; an old one left over costs only disk, and
		// the next snapshot prunes again.
		if err := g.store.Prune(ctx, id); err != nil {
			g.log.WarnContext(ctx, "pruning old snapshots failed", slog.String("env", id), slog.Any("error", err))
		}
		return nil
	})
}

// DeleteSnapshot removes snapshot snapID of environment id; removing one that
// doesn't exist is not an error.
func (g *Group) DeleteSnapshot(ctx context.Context, id, snapID string) error {
	return g.do(ctx, "delete_snapshot", id, func(ctx context.Context, _ *actor) error {
		return g.store.DeleteSnapshot(ctx, id, snapID)
	})
}

// Restore puts environment id to sleep, ending the commands running in its
// box, and swaps its disk for snapshot snapID. It stays asleep; the next
// command wakes it on the restored disk.
func (g *Group) Restore(ctx context.Context, id, snapID string) error {
	return g.do(ctx, "restore", id, func(ctx context.Context, a *actor) (err error) {
		start := time.Now()
		defer observe(g.restores, start, &err)
		// Check the snapshot before stopping the box: a typo mustn't cost
		// the agent its running processes.
		ok, err := g.store.HasSnapshot(ctx, store.SnapshotRef{Env: id, Snapshot: snapID})
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("snapshot %s/%s: %w", id, snapID, store.ErrNotFound)
		}
		if err := a.stopBox(ctx); err != nil {
			return err
		}
		// Claude's box sees the old disk through its mount: it goes too.
		if err := g.stopAgent(ctx, id); err != nil {
			return err
		}
		if err := g.store.Restore(ctx, id, snapID); err != nil {
			return err
		}
		g.log.InfoContext(ctx, "restored", slog.String("env", id), slog.String("snapshot", snapID), slog.Duration("took", time.Since(start)))
		return nil
	})
}

// Sleep stops environment id's box, ending the commands running in it, and
// its agent box if it has one awake.
func (g *Group) Sleep(ctx context.Context, id string) error {
	return g.do(ctx, "sleep", id, func(ctx context.Context, a *actor) error {
		if _, err := g.store.Get(ctx, store.EnvOfBox(id)); err != nil {
			return err
		}
		if err := a.stopBox(ctx); err != nil {
			return err
		}
		if !store.IsAgentBox(id) {
			return g.stopAgent(ctx, id)
		}
		return nil
	})
}

// stopAgent stops environment id's agent box if it is awake, from the
// environment's own actor: the agent box's actor never waits for that one,
// so the two can't wait for each other.
func (g *Group) stopAgent(ctx context.Context, id string) error {
	return g.do(ctx, "stop_agent", store.AgentBox(id), func(ctx context.Context, a *actor) error {
		if !a.awake {
			return nil
		}
		return a.stopBox(ctx)
	})
}

// Wake starts environment id's box. It counts as a touch, so the box stays
// up for the idle times. A box that kept crashing (ErrCrashing) is tried
// again: a person asked.
func (g *Group) Wake(ctx context.Context, id string) error {
	return g.do(ctx, "wake", id, func(ctx context.Context, a *actor) error {
		// A person asks: a box that kept crashing gets another try.
		a.crashes = nil
		if _, err := a.wake(ctx, ""); err != nil {
			return err
		}
		a.endLease("used")
		now := time.Now()
		a.opened, a.closed = now, now
		g.log.InfoContext(ctx, "box touched", slog.String("env", id), slog.Int("active", len(a.execs)))
		return nil
	})
}

// boxOf returns box id of env as the driver is to wake it. This is where
// it is decided what each box runs and gets:
//
//   - a golden builder's box runs the services, started at once, and gets
//     no Claude: what it writes goes into every thread of the repository;
//   - with agent boxes, an agent box runs Claude and no services, and the
//     environment's own box the services and no Claude, so it can't plant
//     settings in Claude's folder;
//   - without, the environment's box runs both.
func (g *Group) boxOf(id string, env store.Env) runsc.Box {
	box := runsc.Box{ID: id, Kind: runsc.Services, Env: env}
	switch {
	case setup.IsBuilder(id):
		box.Kind, box.Env.Claude = runsc.Builder, ""
	case store.IsAgentBox(id):
		box.Kind = runsc.Agent
	case g.cfg.Agents:
		box.Env.Claude = ""
	}
	return box
}
