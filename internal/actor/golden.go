package actor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/nebler/fern/internal/setup"
	"github.com/nebler/fern/internal/store"
)

// goldenSource is the snapshot a new environment keeps of its first
// workspace for a golden build, until the build has copied it. A build is
// queued by the environment's ID: its source is this snapshot of it, which
// can't change, rather than the worktree it was taken of.
const goldenSource = "golden-source"

// buildQueue bounds the golden builds waiting; more are dropped, and the
// next new environment of that repository asks again.
const buildQueue = 16

// golden returns the golden snapshot for new environment id at workspace, a
// worktree T3 made, or nil and whether id's first workspace is to be built
// into one: a miss doesn't make the thread wait, the next one gets it. A
// repository without a setup contract has neither.
func (g *Group) golden(ctx context.Context, id, workspace string) (*store.SnapshotRef, bool) {
	repo := filepath.Base(filepath.Dir(workspace))
	key, err := setup.Key(workspace, g.cfg.Image)
	if errors.Is(err, setup.ErrNoContract) {
		g.goldens.WithLabelValues("none").Inc()
		return nil, false
	}
	if err != nil {
		g.log.WarnContext(ctx, "golden key failed", slog.String("env", id), slog.Any("error", err))
		g.goldens.WithLabelValues("none").Inc()
		return nil, false
	}
	builder := setup.ID(repo)
	if g.hasSnapshot(ctx, builder, key) {
		g.goldens.WithLabelValues("hit").Inc()
		return &store.SnapshotRef{Env: builder, Snapshot: key}, false
	}
	g.goldens.WithLabelValues("miss").Inc()
	return nil, true
}

// hasSnapshot reports whether environment id has snapshot snapID, such as a
// builder its golden snapshot; one it can't learn counts as missing.
func (g *Group) hasSnapshot(ctx context.Context, id, snapID string) bool {
	ok, err := g.store.HasSnapshot(ctx, store.SnapshotRef{Env: id, Snapshot: snapID})
	if err != nil {
		g.log.WarnContext(ctx, "looking for a golden snapshot failed", slog.String("env", id), slog.Any("error", err))
	}
	return ok
}

// Prebuild builds the golden snapshot of repo from seed, a tar stream of its
// checkout, unless repo has one for that checkout already or no setup
// contract: then a project's first thread starts from a golden too, rather
// than paying its services' first start (such as initdb) itself. The build
// runs after Prebuild returns, in the order builds are queued. A prebuild of
// a repo whose last one is still waiting for its build does nothing.
func (g *Group) Prebuild(ctx context.Context, repo string, seed io.Reader) error {
	id := setup.SourceID(repo)
	return g.do(ctx, "golden_prebuild", id, func(ctx context.Context, _ *actor) error {
		if g.hasSnapshot(ctx, id, goldenSource) {
			return nil // the source of a build that is queued
		}
		// What a prebuild that failed left, if anything.
		if err := g.store.Delete(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		env, err := g.store.Create(ctx, id, store.CreateOptions{Seed: seed})
		var key string
		if err == nil {
			key, err = setup.Key(env.Workspace, g.cfg.Image)
		}
		if errors.Is(err, setup.ErrNoContract) || err == nil && g.hasSnapshot(ctx, setup.ID(repo), key) {
			return g.store.Delete(ctx, id)
		}
		if err == nil {
			// The snapshot marks the build as under way: a daemon that stops
			// before the build finds it at its next start (buildGoldens).
			err = g.store.Snapshot(ctx, id, goldenSource)
		}
		if err != nil {
			if delErr := g.store.Delete(ctx, id); delErr != nil {
				g.log.WarnContext(ctx, "deleting a golden source failed", slog.String("env", id), slog.Any("error", delErr))
			}
			return err
		}
		g.queueBuild(ctx, id)
		return nil
	})
}

// queueBuild hands the build of environment src's golden source to the
// builder, or drops it and the source if the queue is full.
func (g *Group) queueBuild(ctx context.Context, src string) {
	select {
	case g.builds <- src:
	default:
		g.log.WarnContext(ctx, "golden build queue full; dropped", slog.String("env", src))
		err := g.store.DeleteSnapshot(ctx, src, goldenSource)
		if _, ok := setup.BuilderOf(src); ok && err == nil {
			err = g.store.Delete(ctx, src) // a prebuild's source holds nothing else
		}
		if err != nil {
			g.log.WarnContext(ctx, "deleting a golden source failed", slog.String("env", src), slog.Any("error", err))
		}
	}
}

// buildGoldens makes the queued golden builds one at a time, until ctx is
// done. It first queues the builds the last daemon left: their sources are
// still there.
func (g *Group) buildGoldens(ctx context.Context) {
	srcs, err := g.store.Sources(ctx, goldenSource)
	if err != nil {
		g.log.WarnContext(ctx, "looking for golden sources failed", slog.Any("error", err))
	}
	for _, src := range srcs {
		g.queueBuild(ctx, src.Env)
	}
	g.clearSources(ctx, srcs)
	for {
		select {
		case <-ctx.Done():
			return
		case src := <-g.builds:
			start := time.Now()
			err := g.buildGolden(ctx, src)
			observe(g.goldenBuilds, start, &err)
			if err != nil {
				g.log.WarnContext(ctx, "golden build failed", slog.String("source", src), slog.Any("error", err))
			}
			if err := g.DeleteSnapshot(ctx, src, goldenSource); err != nil && !errors.Is(err, store.ErrNotFound) {
				g.log.WarnContext(ctx, "deleting a golden source failed", slog.String("env", src), slog.Any("error", err))
			}
			if _, ok := setup.BuilderOf(src); ok {
				if err := g.Delete(ctx, src); err != nil && !errors.Is(err, store.ErrNotFound) {
					g.log.WarnContext(ctx, "deleting a golden source failed", slog.String("env", src), slog.Any("error", err))
				}
			}
		}
	}
}

// clearSources deletes the source environments of prebuilds (Prebuild) that
// stopped before their source snapshot, which marks a build under way: they
// aren't among srcs.
func (g *Group) clearSources(ctx context.Context, srcs []store.SnapshotRef) {
	envs, err := g.store.List(ctx)
	if err != nil {
		g.log.WarnContext(ctx, "looking for golden sources failed", slog.Any("error", err))
		return
	}
	for _, env := range envs {
		if _, ok := setup.BuilderOf(env.ID); !ok || slices.ContainsFunc(srcs, func(r store.SnapshotRef) bool { return r.Env == env.ID }) {
			continue
		}
		if err := g.Delete(ctx, env.ID); err != nil {
			g.log.WarnContext(ctx, "deleting a golden source failed", slog.String("env", env.ID), slog.Any("error", err))
		}
	}
}

// buildGolden makes a golden snapshot from environment src's golden source
// in its repository's builder environment (architecture §7.5): the
// checkout, then its services' first start (such as Postgres's initdb and
// seed), stopped cleanly, then the checkout stripped, so that the snapshot
// holds only what the build made. A build that stops leaves no snapshot,
// and the next one starts afresh.
func (g *Group) buildGolden(ctx context.Context, src string) error {
	srcEnv, err := g.store.Get(ctx, src)
	if err != nil {
		return err
	}
	// A prebuild's source names its builder; a worktree's repository is its
	// parent folder.
	id, ok := setup.BuilderOf(src)
	repo := id
	if !ok {
		repo = filepath.Base(filepath.Dir(srcEnv.Workspace))
		id = setup.ID(repo)
	}
	return g.do(ctx, "golden_build", id, func(ctx context.Context, a *actor) error {
		if _, err := g.store.Create(ctx, id, store.CreateOptions{}); err != nil {
			return err
		}
		if err := a.stopBox(ctx); err != nil {
			return err
		}
		if err := g.store.BuildSource(ctx, id, store.SnapshotRef{Env: src, Snapshot: goldenSource}); err != nil {
			return err
		}
		env, err := g.store.Get(ctx, id)
		if err != nil {
			return err
		}
		key, err := setup.Key(env.Workspace, g.cfg.Image)
		if err != nil {
			return err
		}
		if g.hasSnapshot(ctx, id, key) {
			return nil // an earlier build made it
		}
		_, err = a.wake(ctx, "")
		if stopErr := a.stopBox(ctx); err == nil {
			err = stopErr
		}
		if err != nil {
			return err
		}
		if err := g.store.StripSource(ctx, id); err != nil {
			return err
		}
		if err := g.store.Snapshot(ctx, id, key); err != nil {
			return err
		}
		g.log.InfoContext(ctx, "golden built", slog.String("repo", repo), slog.String("key", key))
		if err := g.store.Prune(ctx, id); err != nil {
			g.log.WarnContext(ctx, "pruning old goldens failed", slog.String("env", id), slog.Any("error", err))
		}
		return nil
	})
}
