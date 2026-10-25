package actor

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/nebler/fern/internal/setup"
	"github.com/nebler/fern/internal/store"
)

// goldenSource is the snapshot a new environment keeps of its first
// workspace for a golden build, until the build has copied it.
const goldenSource = "golden-source"

// buildQueue bounds the golden builds waiting; more are dropped, and the
// next new environment of that repository asks again.
const buildQueue = 16

// build is a golden build to make: of repo's checkout in snapshot src, for
// key.
type build struct {
	repo, key string
	src       store.SnapshotRef
}

// golden returns the golden snapshot for a new environment at workspace, a
// worktree T3 made, or nil and the build to make: a miss doesn't make the
// thread wait, the next one gets it. A repository without a setup contract
// has neither.
func (g *Group) golden(ctx context.Context, id, workspace string) (*store.SnapshotRef, *build) {
	repo := filepath.Base(filepath.Dir(workspace))
	key, err := setup.Key(workspace, g.image)
	if errors.Is(err, setup.ErrNoContract) {
		g.goldens.WithLabelValues("none").Inc()
		return nil, nil
	}
	if err != nil {
		g.log.WarnContext(ctx, "golden key failed", slog.String("env", id), slog.Any("error", err))
		g.goldens.WithLabelValues("none").Inc()
		return nil, nil
	}
	builder := setup.ID(repo)
	snaps, err := g.store.Snapshots(ctx, builder)
	if err == nil && slices.ContainsFunc(snaps, func(s store.Snapshot) bool { return s.ID == key }) {
		g.goldens.WithLabelValues("hit").Inc()
		return &store.SnapshotRef{Env: builder, Snapshot: key}, nil
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		g.log.WarnContext(ctx, "looking for a golden snapshot failed", slog.String("env", id), slog.Any("error", err))
	}
	g.goldens.WithLabelValues("miss").Inc()
	return nil, &build{repo: repo, key: key, src: store.SnapshotRef{Env: id, Snapshot: goldenSource}}
}

// queueBuild hands b to the builder, or drops it and its source if the
// queue is full.
func (g *Group) queueBuild(ctx context.Context, b *build) {
	select {
	case g.builds <- *b:
	default:
		g.log.WarnContext(ctx, "golden build queue full; dropped", slog.String("repo", b.repo))
		if err := g.store.DeleteSnapshot(ctx, b.src.Env, b.src.Snapshot); err != nil {
			g.log.WarnContext(ctx, "deleting a golden source failed", slog.String("env", b.src.Env), slog.Any("error", err))
		}
	}
}

// buildGoldens makes the queued golden builds one at a time, until ctx is
// done.
func (g *Group) buildGoldens(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-g.builds:
			start := time.Now()
			err := g.buildGolden(ctx, b)
			observe(g.goldenBuilds, start, &err)
			if err != nil {
				g.log.WarnContext(ctx, "golden build failed", slog.String("repo", b.repo), slog.String("key", b.key), slog.Any("error", err))
			}
			if err := g.DeleteSnapshot(ctx, b.src.Env, b.src.Snapshot); err != nil && !errors.Is(err, store.ErrNotFound) {
				g.log.WarnContext(ctx, "deleting a golden source failed", slog.String("env", b.src.Env), slog.Any("error", err))
			}
		}
	}
}

// buildGolden makes golden snapshot b.key in repo's builder environment
// (architecture §7.5): the checkout, then its services' first start (such
// as Postgres's initdb and seed), stopped cleanly, then the checkout
// stripped, so that the snapshot holds only what the build made. A build
// that stops leaves no snapshot, and the next one starts afresh.
func (g *Group) buildGolden(ctx context.Context, b build) error {
	id := setup.ID(b.repo)
	return g.do(ctx, "golden_build", id, func(ctx context.Context, a *actor) error {
		if snaps, err := g.store.Snapshots(ctx, id); err == nil &&
			slices.ContainsFunc(snaps, func(s store.Snapshot) bool { return s.ID == b.key }) {
			return nil // an earlier build made it
		}
		if _, err := g.store.Create(ctx, id, store.CreateOptions{}); err != nil {
			return err
		}
		if err := a.stopBox(ctx, g.driver.Sleep); err != nil {
			return err
		}
		if err := g.store.BuildSource(ctx, id, b.src); err != nil {
			return err
		}
		_, err := a.wake(ctx, "")
		if stopErr := a.stopBox(ctx, g.driver.Sleep); err == nil {
			err = stopErr
		}
		if err != nil {
			return err
		}
		if err := g.store.StripSource(ctx, id); err != nil {
			return err
		}
		if err := g.store.Snapshot(ctx, id, b.key); err != nil {
			return err
		}
		g.log.InfoContext(ctx, "golden built", slog.String("repo", b.repo), slog.String("key", b.key))
		if err := g.store.Prune(ctx, id); err != nil {
			g.log.WarnContext(ctx, "pruning old goldens failed", slog.String("env", id), slog.Any("error", err))
		}
		return nil
	})
}
