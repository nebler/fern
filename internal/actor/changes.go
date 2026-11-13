package actor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

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
			opts.From, build = g.builder.golden(ctx, id, opts.Workspace)
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
		if build {
			g.builder.keep(ctx, id)
		}
		return nil
	})
}

// Delete stops environment id's box, then removes its disk, and its forks
// before it.
func (g *Group) Delete(ctx context.Context, id string) error {
	return g.do(ctx, "delete", id, func(ctx context.Context, a *actor) error {
		if _, err := g.store.Get(ctx, id); err != nil {
			return err
		}
		forks, err := g.forks(ctx, id)
		if err != nil {
			return err
		}
		// A fork's actor never waits for its source's, so this can't wait
		// for itself.
		for _, fork := range forks {
			if err := g.Delete(ctx, store.ForkID(id, fork)); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		if err := a.stopBox(ctx); err != nil {
			return err
		}
		// The boxes' counts of crashes go with the environment: one made
		// again under its ID starts clean (until 2026-11-13 it was refused
		// for the one before it, for five minutes).
		forget := func(a *actor) { a.crashes, a.crashCause = nil, "" }
		err = g.do(ctx, "stop_agent", store.AgentBox(id), func(ctx context.Context, agent *actor) error {
			forget(agent)
			if agent.phase == boxAsleep {
				return nil
			}
			return agent.stopBox(ctx)
		})
		if err != nil {
			return err
		}
		if err := g.store.Delete(ctx, id); err != nil {
			return err
		}
		forget(a)
		return nil
	})
}

// MaxForks is how many forks an environment can have at once: each awake
// one is a box with its own services.
const MaxForks = 8

// ErrTooManyForks means a fork would give an environment more than MaxForks.
var ErrTooManyForks = errors.New("too many forks")

// Fork makes count new environments, forks, each a writable copy of
// environment id's snapshot snapID, or with snapID empty, of its workspace
// as it is now: the files, the dependencies, and the services' data as a
// crash would leave them, which they recover from as from a revert. Each
// fork's box runs its own services, its own Postgres on :5432 among them,
// and sleeps when idle like any other. It returns the forks' IDs
// (store.ForkID), with the lowest numbers id has free. Deleting id deletes
// them.
func (g *Group) Fork(ctx context.Context, id, snapID string, count int) ([]string, error) {
	if count < 1 || count > MaxForks {
		return nil, fmt.Errorf("%d forks: from 1 to %d at once: %w", count, MaxForks, ErrTooManyForks)
	}
	var ids []string
	// The source's actor makes them one call at a time, so two calls never
	// pick the same numbers.
	err := g.do(ctx, "fork", id, func(ctx context.Context, _ *actor) error {
		if _, err := g.store.Get(ctx, id); err != nil {
			return err
		}
		forks, err := g.forks(ctx, id)
		if err != nil {
			return err
		}
		if len(forks)+count > MaxForks {
			return fmt.Errorf("%s has %d forks; %d more would be over %d: delete some first: %w",
				id, len(forks), count, MaxForks, ErrTooManyForks)
		}
		src := store.SnapshotRef{Env: id, Snapshot: snapID}
		for n := 1; len(ids) < count; n++ {
			if slices.Contains(forks, n) {
				continue
			}
			fork := store.ForkID(id, n)
			if err := g.Create(ctx, fork, store.CreateOptions{Fork: &src}); err != nil {
				return err
			}
			ids = append(ids, fork)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("fork %s: %w", id, err)
	}
	g.log.InfoContext(ctx, "forked", slog.String("env", id), slog.String("snapshot", snapID), slog.Any("forks", ids))
	return ids, nil
}

// forks returns the numbers of environment id's forks.
func (g *Group) forks(ctx context.Context, id string) ([]int, error) {
	envs, err := g.store.List(ctx)
	if err != nil {
		return nil, err
	}
	var forks []int
	for _, env := range envs {
		if parent, n, ok := store.ForkOf(env.ID); ok && parent == id {
			forks = append(forks, n)
		}
	}
	return forks, nil
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
		if a.phase == boxAsleep {
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
		if err := a.wake(ctx); err != nil {
			return err
		}
		a.endLease("used")
		now := time.Now()
		a.opened, a.closed = now, now
		commands, _ := a.counts()
		g.log.InfoContext(ctx, "box touched", slog.String("env", id), slog.Int("active", commands))
		return nil
	})
}
