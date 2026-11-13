package actor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/setup"
	"github.com/nebler/fern/internal/store"
	"github.com/nebler/fern/internal/worktree"
)

// goldenSource is the snapshot a new environment keeps of its first
// workspace for a golden build, until the build has copied it. A build is
// queued by the environment's ID: its source is this snapshot of it, which
// can't change, rather than the worktree it was taken of.
const goldenSource = "golden-source"

// buildQueue bounds the golden builds waiting; more are dropped, and the
// next new environment of that repository asks again.
const buildQueue = 16

// buildRetry is how long a golden build that failed isn't made again from
// the same key. Every new worktree without a golden asks for one, and a
// setup script that fails does so again, after as long as an hour in the
// builder's box each time.
const buildRetry = 15 * time.Minute

// errBuildWaits means a golden build wasn't made: the last one of the same
// key failed less than buildRetry ago.
var errBuildWaits = errors.New("the last build of this key failed; not built again yet")

// goldenStore is the disk as a golden build uses it: the builder's and the
// sources' environments, their snapshots, the goldens' sources and pruning,
// and the record of a failed build.
type goldenStore interface {
	Environments
	Snapshots
	GoldenSources
	BuildFailures
	List(ctx context.Context) ([]store.Env, error)
	PruneGoldens(ctx context.Context, id string) error
}

// builder makes the golden builds, one at a time, from the goroutine Run
// starts for them (buildGoldens), and decides whether a new environment
// starts from a golden (golden). It is a part of the Group with what it
// uses alone: the disk through goldenStore, and the actors through the
// three calls below, so that nothing of a build reaches the rest of the
// Group's state or the store's other jobs.
type builder struct {
	store goldenStore
	image string // the box image's ID, part of every golden key
	// setupTimeout and runIn are Config's SetupTimeout and RunSetup.
	setupTimeout time.Duration
	runIn        func(ctx context.Context, addr, token string, argv []string, out io.Writer) (int, error)
	log          *slog.Logger
	goldens      *prometheus.CounterVec   // new worktrees, by golden hit, miss or none
	took         *prometheus.HistogramVec // builds' times, by result
	queue        chan string              // the environments whose golden source to build

	// A build changes its builder through the builder's actor (Group.do),
	// runs the setup script as a command of the builder's box (Group.Exec),
	// and deletes a prebuild's source as any environment (Group.Delete),
	// whose box and actor go with it.
	do     doFunc
	exec   func(ctx context.Context, id string, cancel context.CancelFunc) (*Exec, error)
	delete func(ctx context.Context, id string) error
}

// golden returns the golden snapshot for new environment id at workspace, a
// worktree T3 made, or nil and whether id's first workspace is to be built
// into one: a miss doesn't make the thread wait, the next one gets it. A
// repository without a setup contract has neither.
func (b *builder) golden(ctx context.Context, id, workspace string) (*store.SnapshotRef, bool) {
	repo := worktree.Repo(workspace)
	key, err := setup.Key(workspace, b.image)
	if errors.Is(err, setup.ErrNoContract) {
		b.goldens.WithLabelValues("none").Inc()
		return nil, false
	}
	if err != nil {
		b.log.WarnContext(ctx, "golden key failed", slog.String("env", id), slog.Any("error", err))
		b.goldens.WithLabelValues("none").Inc()
		return nil, false
	}
	builder := store.BuilderID(repo)
	if b.hasSnapshot(ctx, builder, key) {
		b.goldens.WithLabelValues("hit").Inc()
		return &store.SnapshotRef{Env: builder, Snapshot: key}, false
	}
	b.goldens.WithLabelValues("miss").Inc()
	return nil, true
}

// keep keeps new environment id's first workspace as a golden source and
// queues its build, from id's actor (Create). A source it can't keep only
// costs the next thread its golden.
func (b *builder) keep(ctx context.Context, id string) {
	if err := b.store.Snapshot(ctx, id, goldenSource); err != nil {
		b.log.WarnContext(ctx, "keeping a golden source failed", slog.String("env", id), slog.Any("error", err))
		return
	}
	b.queueBuild(ctx, id)
}

// hasSnapshot reports whether environment id has snapshot snapID, such as a
// builder its golden snapshot; one it can't learn counts as missing.
func (b *builder) hasSnapshot(ctx context.Context, id, snapID string) bool {
	ok, err := b.store.HasSnapshot(ctx, store.SnapshotRef{Env: id, Snapshot: snapID})
	if err != nil {
		b.log.WarnContext(ctx, "looking for a golden snapshot failed", slog.String("env", id), slog.Any("error", err))
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
	return g.builder.prebuild(ctx, repo, seed)
}

func (b *builder) prebuild(ctx context.Context, repo string, seed io.Reader) error {
	id := store.SourceID(repo)
	return b.do(ctx, "golden_prebuild", id, func(ctx context.Context, _ *actor) error {
		if b.hasSnapshot(ctx, id, goldenSource) {
			return nil // the source of a build that is queued
		}
		// What a prebuild that failed left, if anything.
		if err := b.store.Delete(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		env, err := b.store.Create(ctx, id, store.CreateOptions{Seed: seed})
		var key string
		if err == nil {
			key, err = setup.Key(env.Workspace, b.image)
		}
		if errors.Is(err, setup.ErrNoContract) || err == nil && b.hasSnapshot(ctx, store.BuilderID(repo), key) {
			return b.store.Delete(ctx, id)
		}
		if err == nil {
			// The snapshot marks the build as under way: a daemon that stops
			// before the build finds it at its next start (buildGoldens).
			err = b.store.Snapshot(ctx, id, goldenSource)
		}
		if err != nil {
			if delErr := b.store.Delete(ctx, id); delErr != nil {
				b.log.WarnContext(ctx, "deleting a golden source failed", slog.String("env", id), slog.Any("error", delErr))
			}
			return err
		}
		b.queueBuild(ctx, id)
		return nil
	})
}

// queueBuild hands the build of environment src's golden source to the
// builder, or drops it and the source if the queue is full.
func (b *builder) queueBuild(ctx context.Context, src string) {
	select {
	case b.queue <- src:
	default:
		b.log.WarnContext(ctx, "golden build queue full; dropped", slog.String("env", src))
		err := b.store.DeleteSnapshot(ctx, src, goldenSource)
		if _, ok := store.BuilderOf(src); ok && err == nil {
			err = b.store.Delete(ctx, src) // a prebuild's source holds nothing else
		}
		if err != nil {
			b.log.WarnContext(ctx, "deleting a golden source failed", slog.String("env", src), slog.Any("error", err))
		}
	}
}

// buildGoldens makes the golden builds one at a time, until ctx is done:
// first the ones the last daemon left, whose sources are still there, then
// the queued ones.
func (b *builder) buildGoldens(ctx context.Context) {
	srcs, err := b.store.Sources(ctx, goldenSource)
	if err != nil {
		b.log.WarnContext(ctx, "looking for golden sources failed", slog.Any("error", err))
	}
	b.clearSources(ctx, srcs)
	// Straight from the store, not through the queue: it holds only 16, and
	// one dropped would lose its source.
	for _, src := range srcs {
		if ctx.Err() != nil {
			return
		}
		b.build(ctx, src.Env)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case src := <-b.queue:
			b.build(ctx, src)
		}
	}
}

// build makes the golden build of environment src's golden source, then
// deletes the source.
func (b *builder) build(ctx context.Context, src string) {
	start := time.Now()
	err := b.buildGolden(ctx, src)
	switch {
	case errors.Is(err, errBuildWaits):
		b.log.InfoContext(ctx, "golden build skipped", slog.String("source", src), slog.Any("why", err))
	default:
		observe(b.took, start, &err)
		if err != nil {
			b.log.WarnContext(ctx, "golden build failed", slog.String("source", src), slog.Any("error", err))
		}
	}
	err = b.do(ctx, "delete_snapshot", src, func(ctx context.Context, _ *actor) error {
		return b.store.DeleteSnapshot(ctx, src, goldenSource)
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		b.log.WarnContext(ctx, "deleting a golden source failed", slog.String("env", src), slog.Any("error", err))
	}
	if _, ok := store.BuilderOf(src); ok {
		if err := b.delete(ctx, src); err != nil && !errors.Is(err, store.ErrNotFound) {
			b.log.WarnContext(ctx, "deleting a golden source failed", slog.String("env", src), slog.Any("error", err))
		}
	}
}

// clearSources deletes the source environments of prebuilds (Prebuild) that
// stopped before their source snapshot, which marks a build under way: they
// aren't among srcs.
func (b *builder) clearSources(ctx context.Context, srcs []store.SnapshotRef) {
	envs, err := b.store.List(ctx)
	if err != nil {
		b.log.WarnContext(ctx, "looking for golden sources failed", slog.Any("error", err))
		return
	}
	for _, env := range envs {
		if _, ok := store.BuilderOf(env.ID); !ok || slices.ContainsFunc(srcs, func(r store.SnapshotRef) bool { return r.Env == env.ID }) {
			continue
		}
		if err := b.delete(ctx, env.ID); err != nil {
			b.log.WarnContext(ctx, "deleting a golden source failed", slog.String("env", env.ID), slog.Any("error", err))
		}
	}
}

// buildGolden makes a golden snapshot from environment src's golden source
// in its repository's builder environment (architecture §7.5): the
// checkout, then the repository's setup script, if it has one, in a box with
// the tools and no services, then its services' first start (such as
// Postgres's initdb and seed), stopped cleanly, then the checkout stripped,
// so that the snapshot holds only what the build made. A build that stops
// leaves no snapshot, and the next one starts afresh.
//
// It is three changes of the builder, not one: the script runs between them
// as a command of the box, for up to an hour, and a change holds the
// actor's mailbox and has ten minutes.
func (b *builder) buildGolden(ctx context.Context, src string) (err error) {
	srcEnv, err := b.store.Get(ctx, src)
	if err != nil {
		return err
	}
	// A prebuild's source names its builder; a worktree's repository is its
	// parent folder.
	id, ok := store.BuilderOf(src)
	repo := id
	if !ok {
		repo = worktree.Repo(srcEnv.Workspace)
		id = store.BuilderID(repo)
	}
	// The key is of the checkout as it came: a setup script may change a
	// lockfile, and the golden is for the threads that start from what it
	// was built from.
	var key, script string
	// A failed build is recorded with the builder, on disk (store.BuildFailure):
	// the next build of its key waits, a restart of the daemon keeps it, and
	// the repository's threads and `fern doctor` say so. A build the daemon's
	// stop cut short is no failure.
	var last store.BuildFailure
	waits := false
	defer func() {
		var rec error
		switch {
		case waits, ctx.Err() != nil:
		case err == nil:
			rec = b.store.SetBuildFailure(ctx, id, nil)
		case key != "":
			rec = b.store.SetBuildFailure(ctx, id, &store.BuildFailure{Key: key, At: time.Now(), Error: err.Error()})
		}
		if rec != nil && !errors.Is(rec, store.ErrNotFound) {
			b.log.WarnContext(ctx, "recording a golden build's result failed", slog.String("env", id), slog.Any("error", rec))
		}
	}()
	err = b.do(ctx, "golden_build", id, func(ctx context.Context, a *actor) error {
		if _, err := b.store.Create(ctx, id, store.CreateOptions{}); err != nil {
			return err
		}
		a.setup = false
		if err := a.stopBox(ctx); err != nil {
			return err
		}
		if err := b.store.BuildSource(ctx, id, store.SnapshotRef{Env: src, Snapshot: goldenSource}); err != nil {
			return err
		}
		env, err := b.store.Get(ctx, id)
		if err != nil {
			return err
		}
		if key, err = setup.Key(env.Workspace, b.image); err != nil {
			return err
		}
		if b.hasSnapshot(ctx, id, key) {
			key = "" // an earlier build made it
			return nil
		}
		failed := false
		if last, failed, err = b.store.BuildFailure(ctx, id); err != nil {
			return err
		}
		if failed && last.Key == key && time.Since(last.At) < buildRetry {
			waits = true
			return nil
		}
		if script, err = setup.Script(env.Workspace); err != nil || script == "" {
			return err
		}
		// The box for the script: the tools, and no services yet.
		a.setup = true
		if err := a.wake(ctx); err != nil {
			a.setup = false
			return err
		}
		return nil
	})
	if err != nil || key == "" {
		return err
	}
	if waits {
		return fmt.Errorf("%s: %w (%s ago)", id, errBuildWaits, time.Since(last.At).Round(time.Second))
	}
	if script != "" {
		err := b.runSetup(ctx, id, script)
		stopErr := b.do(ctx, "golden_setup_end", id, func(ctx context.Context, a *actor) error {
			a.setup = false
			return a.stopBox(ctx)
		})
		if err != nil {
			return err
		}
		if stopErr != nil {
			return stopErr
		}
	}
	return b.do(ctx, "golden_finish", id, func(ctx context.Context, a *actor) error {
		err := a.wake(ctx)
		if stopErr := a.stopBox(ctx); err == nil {
			err = stopErr
		}
		if err != nil {
			return err
		}
		if err := b.store.StripSource(ctx, id); err != nil {
			return err
		}
		if err := b.store.Snapshot(ctx, id, key); err != nil {
			return err
		}
		b.log.InfoContext(ctx, "golden built", slog.String("repo", repo), slog.String("key", key))
		if err := b.store.PruneGoldens(ctx, id); err != nil {
			b.log.WarnContext(ctx, "pruning old goldens failed", slog.String("env", id), slog.Any("error", err))
		}
		return nil
	})
}

// setupOutput is how much of what a setup script printed a failed build's
// error quotes, its start and its end.
const setupOutput = 4 << 10

// runSetup runs the setup script at path script of builder id's workspace
// in its box, which the build has woken for it, as a command of the box: it
// keeps the box awake, and the box's stop ends it. The script runs as a
// program if it may, with /bin/sh if not, in the workspace, with the
// repository's tools on its PATH. A script that fails, or runs past the
// limit, fails the build with what it printed.
func (b *builder) runSetup(ctx context.Context, id, script string) error {
	limit := b.setupTimeout
	if limit <= 0 {
		limit = DefaultSetupTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	ex, err := b.exec(ctx, id, cancel)
	if err != nil {
		return err
	}
	defer ex.Close()
	run := b.runIn
	if run == nil {
		run = runInBox
	}
	start := time.Now()
	out := boxexec.NewClipped(setupOutput)
	argv := []string{"/bin/sh", "-c", `if [ -x "$1" ]; then exec "./$1"; else exec /bin/sh "$1"; fi`, "setup", script}
	code, err := run(ctx, ex.Addr, ex.Token, argv, out)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("%s ran past its limit of %s and was stopped; it had printed: %s", script, limit, out)
	case err != nil:
		return fmt.Errorf("running %s: %w", script, err)
	case code != 0:
		return fmt.Errorf("%s exited with code %d after %s; it printed: %s", script, code, time.Since(start).Round(time.Second), out)
	}
	b.log.InfoContext(ctx, "setup script ran", slog.String("env", id), slog.String("script", script), slog.Duration("took", time.Since(start)))
	return nil
}

// runInBox runs argv in the box at addr over boxexec, with what it prints
// written to out.
func runInBox(ctx context.Context, addr, token string, argv []string, out io.Writer) (int, error) {
	conn, err := boxexec.Dial(ctx, "ws://"+addr+boxexec.Path, token, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.CloseNow() }()
	// Both streams are written from one goroutine.
	return boxexec.Run(ctx, conn, boxexec.Request{Argv: argv}, boxexec.Streams{Stdout: out, Stderr: out})
}
