// Package actor gives every environment one goroutine, its actor, which makes
// the environment's changes one at a time, counts the commands running in its
// box, and puts the box to sleep when it has been idle. It also wakes boxes
// ahead of their next command (Prewake).
//
// An actor lives while its environment's box may be running or a message
// for it is waiting; an asleep environment has none. Its state is touched
// only by its own goroutine.
package actor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"

	"github.com/nebler/fern/internal/driver"
	"github.com/nebler/fern/internal/store"
)

// Store keeps environments and snapshots on disk.
type Store interface {
	Create(ctx context.Context, id string, opts store.CreateOptions) (store.Env, error)
	Get(ctx context.Context, id string) (store.Env, error)
	List(ctx context.Context) ([]store.Env, error)
	Delete(ctx context.Context, id string) error
	Snapshot(ctx context.Context, id, snapID string) error
	DeleteSnapshot(ctx context.Context, id, snapID string) error
	Snapshots(ctx context.Context, id string) ([]store.Snapshot, error)
	Restore(ctx context.Context, id, snapID string) error
	Prune(ctx context.Context, id string) error
	Orphans(ctx context.Context) ([]string, error)
	BuildSource(ctx context.Context, id string, src store.SnapshotRef) error
	StripSource(ctx context.Context, id string) error
	Sources(ctx context.Context, name string) ([]store.SnapshotRef, error)
}

// Driver runs environments' boxes.
type Driver interface {
	Wake(ctx context.Context, env store.Env) (addr string, err error)
	Sleep(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	States(ctx context.Context) (map[string]driver.State, error)
}

// Idle says when a box with no command running goes to sleep: Agent after
// its last command ended, and Human after its last command started. Zero
// durations keep boxes awake. A box woken by Prewake instead sleeps Prewake
// after its wake, unless a command comes first; a zero Prewake turns
// Prewake off.
type Idle struct {
	Agent   time.Duration
	Human   time.Duration
	Prewake time.Duration
}

// prewakers is how many pre-wakes run at once, and prewakeQueue how many may
// wait for one. A pre-wake is a guess, so it never holds up more than two
// boxes' starts on the node, and one asked for when the queue is full is
// dropped.
const (
	prewakers    = 2
	prewakeQueue = 8
)

// sweepEvery is how often Run looks for environments whose worktree T3
// removed. One that is still gone at the next look is deleted: T3 removes a
// deleted thread's worktree, and makes it again only for a thread that
// resumes, which comes with a checkpoint that adopts it back.
const sweepEvery = time.Hour

// changeTimeout bounds a change of an environment. Changes don't stop when
// their caller goes away, which would leave them half done.
const changeTimeout = 10 * time.Minute

// ErrStopped means the daemon is shutting down.
var ErrStopped = errors.New("daemon is stopping")

// AgentBox is the name of environment id's agent box (store.AgentBox): with
// --agent-boxes Claude runs in a box of its own, next to the box with the
// services, which its Bash tool wakes (docs/briefs/claude-outside-the-box.md).
// Its actor is its own, so it wakes and sleeps on its own clocks; a restore,
// sleep or delete of the environment stops it too.
func AgentBox(id string) string { return store.AgentBox(id) }

// envOf is the environment a box belongs to.
func envOf(id string) string { return store.EnvOfBox(id) }

// Group runs the actors of a node's environments. It is a Prometheus
// collector of their metrics.
type Group struct {
	store  Store
	driver Driver
	idle   Idle
	image  string // the box image, part of every golden key
	tracer trace.Tracer
	log    *slog.Logger

	// Time histograms, by result ("ok" or "error"), and, for queue, by
	// change: how long a change waited for its actor.
	wakes, stops, restores, snapshots, queue, goldenBuilds *prometheus.HistogramVec
	idleSleeps                                             prometheus.Counter
	goldens                                                *prometheus.CounterVec // new worktrees, by golden hit, miss or none
	prewakeResults                                         *prometheus.CounterVec // pre-wakes, by what came of them

	find     chan findRequest // to Run, for an environment's actor
	builds   chan build       // to Run's builder, golden builds to make
	prewakes chan string      // to Run's prewakers, environments to wake ahead
	stopped  chan struct{}    // closed when Run returns
}

type findRequest struct {
	id    string
	reply chan *actor
}

// New returns a Group for the environments in st, run by drv from image,
// that records its changes with tr. Call Run before anything else.
func New(st Store, drv Driver, idle Idle, image string, tr trace.Tracer, log *slog.Logger) *Group {
	// Boxes take from tens of milliseconds to the timeouts' minutes; disk
	// changes and queueing take from a millisecond.
	boxBuckets := prometheus.ExponentialBuckets(0.025, 2, 13)  // 25ms to 102s
	diskBuckets := prometheus.ExponentialBuckets(0.001, 2, 16) // 1ms to 33s
	histogram := func(name, help string, buckets []float64, label string) *prometheus.HistogramVec {
		return prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, []string{label})
	}
	return &Group{
		store: tracedStore{st: st, tr: tr}, driver: drv, idle: idle, image: image, tracer: tr, log: log,
		wakes:        histogram("fern_wake_seconds", "Time to wake a box, until its services are ready.", boxBuckets, "result"),
		stops:        histogram("fern_stop_seconds", "Time to stop a box, until its pod is gone.", boxBuckets, "result"),
		restores:     histogram("fern_restore_seconds", "Time to restore a snapshot, stopping the box included.", boxBuckets, "result"),
		snapshots:    histogram("fern_snapshot_seconds", "Time to take a snapshot, replacing one included.", diskBuckets, "result"),
		goldenBuilds: histogram("fern_golden_build_seconds", "Time to build a golden snapshot.", boxBuckets, "result"),
		goldens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fern_golden_total", Help: "New worktree environments, by whether a golden snapshot was there (hit), is being built (miss) or has no setup contract (none).",
		}, []string{"result"}),
		queue: histogram("fern_queue_seconds", "Time a change waited for its environment's actor.", diskBuckets, "change"),
		idleSleeps: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "fern_idle_sleeps_total", Help: "Boxes put to sleep because they were idle.",
		}),
		prewakeResults: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fern_prewakes_total",
			Help: "Pre-wakes, by result: woken; then used (a command came) or wasted (the box stopped first); " +
				"or awake (it was already), unknown (no such environment), dropped (the queue was full), error.",
		}, []string{"result"}),
		find: make(chan findRequest), builds: make(chan build, buildQueue), prewakes: make(chan string, prewakeQueue),
		stopped: make(chan struct{}),
	}
}

func (g *Group) collectors() []prometheus.Collector {
	return []prometheus.Collector{g.wakes, g.stops, g.restores, g.snapshots, g.queue, g.goldenBuilds, g.idleSleeps, g.goldens, g.prewakeResults}
}

// Describe sends the descriptions of the Group's metrics.
func (g *Group) Describe(ch chan<- *prometheus.Desc) {
	for _, c := range g.collectors() {
		c.Describe(ch)
	}
}

// Collect sends the Group's metrics.
func (g *Group) Collect(ch chan<- prometheus.Metric) {
	for _, c := range g.collectors() {
		c.Collect(ch)
	}
}

// Run runs the actors until ctx is done, then waits for each to finish the
// change it is making. It first gives every box that is running an actor, so
// that boxes left awake by an earlier daemon go to sleep when idle; if it
// can't learn which, it returns the error.
func (g *Group) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	defer close(g.stopped)
	actors := make(map[string]*actor)
	exited := make(chan *actor)
	start := func(id string, awake bool) *actor {
		a := &actor{id: id, g: g, mailbox: make(chan message), done: make(chan struct{})}
		actors[id] = a
		wg.Go(func() {
			a.run(ctx, awake)
			close(a.done)
			select {
			case exited <- a:
			case <-ctx.Done():
			}
		})
		return a
	}
	states, err := g.driver.States(ctx)
	if err != nil {
		return fmt.Errorf("box states: %w", err)
	}
	wg.Go(func() { g.sweep(ctx) })
	wg.Go(func() { g.buildGoldens(ctx) })
	for range prewakers {
		wg.Go(func() { g.prewakeBoxes(ctx) })
	}
	for id, state := range states {
		if state != driver.Asleep {
			start(id, true)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case req := <-g.find:
			a, ok := actors[req.id]
			if !ok || isClosed(a.done) {
				a = start(req.id, false)
			}
			req.reply <- a
		case a := <-exited:
			if actors[a.id] == a {
				delete(actors, a.id)
			}
		}
	}
}

// sweep deletes, every sweepEvery, the environments whose worktree was gone
// at the last look too, until ctx is done.
func (g *Group) sweep(ctx context.Context) {
	ticker := time.NewTicker(sweepEvery)
	defer ticker.Stop()
	var gone map[string]bool
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		ids, err := g.store.Orphans(ctx)
		if err != nil {
			g.log.WarnContext(ctx, "looking for removed worktrees failed", slog.Any("error", err))
			continue
		}
		now := make(map[string]bool, len(ids))
		for _, id := range ids {
			now[id] = true
			if !gone[id] {
				continue
			}
			if err := g.Delete(ctx, id); err != nil {
				g.log.WarnContext(ctx, "deleting an environment whose worktree is gone failed", slog.String("env", id), slog.Any("error", err))
				continue
			}
			g.log.InfoContext(ctx, "deleted an environment whose worktree is gone", slog.String("env", id))
		}
		gone = now
	}
}

// actorOf returns environment id's actor, starting one if it has none.
func (g *Group) actorOf(ctx context.Context, id string) (*actor, error) {
	if err := store.CheckEnvID(envOf(id)); err != nil {
		return nil, err
	}
	req := findRequest{id: id, reply: make(chan *actor, 1)}
	select {
	case g.find <- req:
		return <-req.reply, nil
	case <-g.stopped:
		return nil, ErrStopped
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// do runs fn, the change named change, on environment id's actor and
// returns its error. Waiting for the actor to take fn can be cancelled; once
// it has, fn runs to the end, and do waits for it, so that fn never uses what
// its caller has let go of (such as a request's body). changeTimeout bounds
// the wait.
func (g *Group) do(ctx context.Context, change, id string, fn func(ctx context.Context, a *actor) error) (err error) {
	ctx, span := startSpan(ctx, g.tracer, change, id)
	defer func() { endSpan(span, err) }()
	queued := time.Now()
	m := message{reply: make(chan error, 1), fn: func(actx context.Context, a *actor) error {
		g.queue.WithLabelValues(change).Observe(time.Since(queued).Seconds())
		// The actor's context carries the caller's span, not its deadline.
		_, wait := g.tracer.Start(ctx, "queue", trace.WithTimestamp(queued))
		wait.End()
		return fn(trace.ContextWithSpan(actx, span), a)
	}}
	for {
		a, err := g.actorOf(ctx, id)
		if err != nil {
			return err
		}
		select {
		case a.mailbox <- m:
			return <-m.reply
		case <-a.done:
			// It has just exited; Run starts another.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func isClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// Create makes environment id. Its box comes with its first wake. A new
// environment at a worktree whose repository has a golden snapshot for its
// setup starts from it; without one, it keeps its first workspace for a
// golden build, so the next one does.
func (g *Group) Create(ctx context.Context, id string, opts store.CreateOptions) error {
	return g.do(ctx, "create", id, func(ctx context.Context, _ *actor) error {
		var b *build
		if _, err := g.store.Get(ctx, id); errors.Is(err, store.ErrNotFound) && opts.Workspace != "" && opts.Seed == nil {
			opts.From, b = g.golden(ctx, id, opts.Workspace)
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
		if b == nil {
			return nil
		}
		if err := g.store.Snapshot(ctx, id, goldenSource); err != nil {
			g.log.WarnContext(ctx, "keeping a golden source failed", slog.String("env", id), slog.Any("error", err))
			return nil
		}
		g.queueBuild(ctx, b)
		return nil
	})
}

// Delete stops environment id's box, then removes its disk.
func (g *Group) Delete(ctx context.Context, id string) error {
	return g.do(ctx, "delete", id, func(ctx context.Context, a *actor) error {
		if _, err := g.store.Get(ctx, id); err != nil {
			return err
		}
		if err := a.stopBox(ctx, g.driver.Delete); err != nil {
			return err
		}
		if err := g.stopAgent(ctx, id, g.driver.Delete); err != nil {
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
		snaps, err := g.store.Snapshots(ctx, id)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(snaps, func(snap store.Snapshot) bool { return snap.ID == snapID }) {
			return fmt.Errorf("snapshot %s/%s: %w", id, snapID, store.ErrNotFound)
		}
		if err := a.stopBox(ctx, g.driver.Sleep); err != nil {
			return err
		}
		// Claude's box sees the old disk through its mount: it goes too.
		if err := g.stopAgent(ctx, id, g.driver.Sleep); err != nil {
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
		if _, err := g.store.Get(ctx, envOf(id)); err != nil {
			return err
		}
		if err := a.stopBox(ctx, g.driver.Sleep); err != nil {
			return err
		}
		if id == envOf(id) {
			return g.stopAgent(ctx, id, g.driver.Sleep)
		}
		return nil
	})
}

// stopAgent stops environment id's agent box with stop if it is awake, from
// the environment's own actor: the agent box's actor never waits for that
// one, so the two can't wait for each other.
func (g *Group) stopAgent(ctx context.Context, id string, stop func(ctx context.Context, id string) error) error {
	return g.do(ctx, "stop_agent", AgentBox(id), func(ctx context.Context, a *actor) error {
		if !a.awake {
			return nil
		}
		return a.stopBox(ctx, stop)
	})
}

// Wake starts environment id's box. It counts as a touch, so the box stays
// up for the idle times.
func (g *Group) Wake(ctx context.Context, id string) error {
	return g.do(ctx, "wake", id, func(ctx context.Context, a *actor) error {
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

// Prewake asks for environment id's box to be woken in the background,
// because a command is likely soon: T3 asks when a thread is opened or
// reverted. It returns at once. A box that is awake is left as it is, its
// idle times included; one it wakes sleeps after Idle.Prewake unless a
// command comes first. An unknown environment is only counted.
func (g *Group) Prewake(id string) error {
	if err := store.CheckEnvID(envOf(id)); err != nil {
		return err
	}
	// With idle sleep off, a box woken ahead would never sleep again.
	if g.idle.Prewake == 0 || g.idle.Agent == 0 || g.idle.Human == 0 {
		return nil
	}
	select {
	case g.prewakes <- id:
	default:
		g.prewakeResults.WithLabelValues("dropped").Inc()
	}
	return nil
}

// prewakeBoxes makes the pre-wakes asked for, one at a time, until ctx is
// done.
func (g *Group) prewakeBoxes(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-g.prewakes:
			g.prewake(ctx, id)
		}
	}
}

func (g *Group) prewake(ctx context.Context, id string) {
	result := "woken"
	err := g.do(ctx, "prewake", id, func(ctx context.Context, a *actor) error {
		if a.awake {
			result = "awake"
			return nil
		}
		// Set first, so that a wake that fails half-way, leaving the box
		// running, still ends with the lease.
		a.lease = time.Now().Add(g.idle.Prewake)
		if _, err := a.wake(ctx, ""); err != nil {
			if !a.awake {
				a.lease = time.Time{}
			}
			return err
		}
		a.lease, a.prewoken = time.Now().Add(g.idle.Prewake), true
		g.log.InfoContext(ctx, "box woken ahead", slog.String("env", id), slog.Time("until", a.lease))
		return nil
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Such as a thread whose first checkpoint hasn't adopted its worktree.
		result = "unknown"
	case err != nil:
		result = "error"
		g.log.WarnContext(ctx, "pre-wake failed", slog.String("env", id), slog.Any("error", err))
	}
	g.prewakeResults.WithLabelValues(result).Inc()
}

// Exec is a command running in a box, counted by its environment's actor
// until Close or CloseAndSleep.
type Exec struct {
	Addr  string // the box's exec address
	Token string // authenticates the daemon to the box

	close func(sleep bool, after time.Duration)
}

// Close ends the count of the command, which starts the idle times. It may
// be called more than once.
func (e *Exec) Close() { e.close(false, 0) }

// CloseAndSleep ends the count of the command and, if no other command runs
// in the box, puts it to sleep after d instead of after the idle times: the
// command was an agent that stopped because it had nothing to do (package
// hibernate, d = 0), or one of an agent's Bash calls, which come in bursts.
func (e *Exec) CloseAndSleep(d time.Duration) { e.close(true, d) }

// Exec wakes environment id's box if needed and counts a command running in
// it until the returned Exec is closed. A restore, sleep or delete calls
// cancel, which must end the command. stale is an address of the box that
// didn't answer, if any: then the box is woken again.
func (g *Group) Exec(ctx context.Context, id, stale string, cancel context.CancelFunc) (*Exec, error) {
	var ex *Exec
	err := g.do(ctx, "exec", id, func(ctx context.Context, a *actor) error {
		env, err := a.wake(ctx, stale)
		if err != nil {
			return err
		}
		a.endLease("used")
		a.next++
		n := a.next
		a.execs[n] = cancel
		a.opened = time.Now()
		g.log.InfoContext(ctx, "exec opened", slog.String("env", id), slog.Int("exec", n), slog.Int("active", len(a.execs)))
		ex = &Exec{Addr: a.addr, Token: env.BoxToken, close: func(sleep bool, after time.Duration) { a.endExec(n, sleep, after) }}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The caller gave up during the wake: it won't run the command.
	if err := ctx.Err(); err != nil {
		ex.Close()
		return nil, err
	}
	return ex, nil
}

// message is a call for an actor to make.
type message struct {
	fn    func(ctx context.Context, a *actor) error
	reply chan error // buffered, so the actor never waits for the caller
}

// actor owns one environment. Only its own goroutine touches the fields
// below done.
type actor struct {
	id      string
	g       *Group
	mailbox chan message
	done    chan struct{} // closed when run returns

	addr     string                     // the box's exec address while it is known to be up
	awake    bool                       // the box may be running
	execs    map[int]context.CancelFunc // commands running in the box, by number
	next     int                        // the last command's number
	opened   time.Time                  // when the last command started
	closed   time.Time                  // when the last command ended
	lease    time.Time                  // when an awake box with nothing running sleeps instead of after the idle times: set by a pre-wake, or a command that asked
	prewoken bool                       // the lease's wake succeeded, so its end is counted
	idle     *time.Timer                // fires when the box may have been idle long enough
}

// run handles messages until ctx is done, or until the box is asleep with
// nothing left to do. awake says the box may be running already.
func (a *actor) run(ctx context.Context, awake bool) {
	a.execs = make(map[int]context.CancelFunc)
	a.idle = time.NewTimer(0)
	a.idle.Stop()
	defer a.idle.Stop()
	if awake {
		a.awake = true
		a.opened, a.closed = time.Now(), time.Now()
	}
	for {
		a.armIdle()
		select {
		case <-ctx.Done():
			return
		case m := <-a.mailbox:
			a.handle(ctx, m)
		case <-a.idle.C:
			a.sleepIfIdle(ctx)
		}
		if a.awake || len(a.execs) > 0 {
			continue
		}
		// Asleep with nothing running: exit, unless a message is waiting.
		select {
		case m := <-a.mailbox:
			a.handle(ctx, m)
		default:
			return
		}
	}
}

func (a *actor) handle(ctx context.Context, m message) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), changeTimeout)
	defer cancel()
	m.reply <- m.fn(ctx, a)
}

// wake returns environment id's record once its box is up, waking it unless
// it is known to be up somewhere other than stale.
func (a *actor) wake(ctx context.Context, stale string) (store.Env, error) {
	env, err := a.g.store.Get(ctx, envOf(a.id))
	if err != nil {
		return store.Env{}, err
	}
	env.ID = a.id // the driver runs the agent box apart
	if a.addr != "" && a.addr != stale {
		return env, nil
	}
	a.addr = ""
	// Set first: a wake that fails half-way may leave the box running. Its
	// idle times start now, so a slow wake isn't put back to sleep at once.
	if !a.awake {
		a.awake = true
		a.opened, a.closed = time.Now(), time.Now()
	}
	start := time.Now()
	ctx, span := startSpan(ctx, a.g.tracer, "box.wake", a.id)
	addr, err := a.g.driver.Wake(ctx, env)
	endSpan(span, err)
	observe(a.g.wakes, start, &err)
	if err != nil {
		return store.Env{}, err
	}
	a.g.log.InfoContext(ctx, "box woke", slog.String("env", a.id), slog.Duration("took", time.Since(start)))
	a.addr = addr
	return env, nil
}

// stopBox stops the box with stop (the driver's Sleep or Delete), then
// cancels the commands still counted as running in it, so that none goes on
// against the disk that comes next.
func (a *actor) stopBox(ctx context.Context, stop func(ctx context.Context, id string) error) error {
	a.addr = ""
	start := time.Now()
	spanCtx, span := startSpan(ctx, a.g.tracer, "box.stop", a.id)
	err := stop(spanCtx, a.id)
	endSpan(span, err)
	observe(a.g.stops, start, &err)
	if err != nil {
		return err
	}
	a.endLease("wasted")
	a.awake = false
	for n, cancel := range a.execs {
		cancel()
		delete(a.execs, n)
	}
	return nil
}

// endExec stops counting command n, from the goroutine that ran it; with
// sleep, the box sleeps after d if nothing else runs. A command that a stop
// already cancelled is not counted any more.
func (a *actor) endExec(n int, sleep bool, d time.Duration) {
	end := func(ctx context.Context, _ *actor) error {
		if _, ok := a.execs[n]; ok {
			delete(a.execs, n)
			a.closed = time.Now()
			if sleep && len(a.execs) == 0 {
				a.lease = a.closed.Add(d)
			}
			a.g.log.InfoContext(ctx, "exec closed", slog.String("env", a.id), slog.Int("exec", n),
				slog.Int("active", len(a.execs)), slog.Time("last_opened", a.opened))
		}
		return nil
	}
	select {
	case a.mailbox <- message{fn: end, reply: make(chan error, 1)}:
	case <-a.done:
	}
}

// endLease ends a pre-wake's lease, if there is one, counting result for a
// wake that succeeded: used when a command came, wasted when the box stopped
// first.
func (a *actor) endLease(result string) {
	if a.prewoken {
		a.g.prewakeResults.WithLabelValues(result).Inc()
	}
	a.lease, a.prewoken = time.Time{}, false
}

// deadline is when an awake box with nothing running may go to sleep.
func (a *actor) deadline() time.Time {
	if !a.lease.IsZero() {
		return a.lease
	}
	return later(a.closed.Add(a.g.idle.Agent), a.opened.Add(a.g.idle.Human))
}

// armIdle sets the idle timer for an awake box with nothing running, and
// stops it otherwise.
func (a *actor) armIdle() {
	if !a.awake || len(a.execs) > 0 || a.g.idle.Agent == 0 || a.g.idle.Human == 0 {
		a.idle.Stop()
		return
	}
	a.idle.Reset(time.Until(a.deadline()))
}

func (a *actor) sleepIfIdle(ctx context.Context) {
	if !a.awake || len(a.execs) > 0 || time.Now().Before(a.deadline()) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), changeTimeout)
	defer cancel()
	if err := a.stopBox(ctx, a.g.driver.Sleep); err != nil {
		// Try again after another idle time.
		a.g.log.WarnContext(ctx, "idle sleep", slog.String("env", a.id), slog.Any("error", err))
		a.closed = time.Now()
		return
	}
	a.g.idleSleeps.Inc()
	a.g.log.InfoContext(ctx, "idle box went to sleep", slog.String("env", a.id),
		slog.Time("last_opened", a.opened), slog.Time("last_closed", a.closed), slog.Time("idle_at", a.deadline()))
}

// observe adds the time since start to h, labelled with whether *err is
// nil. It takes a pointer so that it can be deferred.
func observe(h *prometheus.HistogramVec, start time.Time, err *error) {
	result := "ok"
	if *err != nil {
		result = "error"
	}
	h.WithLabelValues(result).Observe(time.Since(start).Seconds())
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
