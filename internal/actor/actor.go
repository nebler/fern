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
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel/trace"

	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/span"
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
	HasSnapshot(ctx context.Context, ref store.SnapshotRef) (bool, error)
	Restore(ctx context.Context, id, snapID string) error
	Prune(ctx context.Context, id string) error
	Orphans(ctx context.Context) ([]string, error)
	BuildSource(ctx context.Context, id string, src store.SnapshotRef) error
	StripSource(ctx context.Context, id string) error
	Sources(ctx context.Context, name string) ([]store.SnapshotRef, error)
}

// Driver runs boxes.
type Driver interface {
	Wake(ctx context.Context, box runsc.Box) (addr string, err error)
	Sleep(ctx context.Context, id string) error
	States(ctx context.Context) (map[string]runsc.State, error)
}

// Config is how a Group runs its environments' boxes.
type Config struct {
	Idle  Idle
	Image string // the box image's ID, part of every golden key
	// Agents runs Claude in each environment's agent box (store.AgentBox),
	// next to the box with the services, which its Bash tool wakes
	// (docs/briefs/claude-outside-the-box.md). The agent box has an actor of
	// its own, so it wakes and sleeps on its own clocks; a restore, sleep or
	// delete of the environment stops it too.
	Agents bool
	// Watcher, with Agents, passes the files Claude writes on to the
	// services' box while it is awake, whose file watchers would not hear of
	// them otherwise: gVisor tells a box only of its own changes. nil: they
	// don't.
	Watcher Watcher
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

// Group runs the actors of a node's environments.
type Group struct {
	store  Store
	driver Driver
	cfg    Config
	tracer trace.Tracer
	log    *slog.Logger
	// touches asks services' boxes to touch the files Claude changed.
	touches *http.Client

	// Time histograms, by result ("ok" or "error"), and, for queue, by
	// change: how long a change waited for its actor.
	wakes, stops, restores, snapshots, queue, goldenBuilds *prometheus.HistogramVec
	idleSleeps                                             prometheus.Counter
	goldens                                                *prometheus.CounterVec // new worktrees, by golden hit, miss or none
	prewakeResults                                         *prometheus.CounterVec // pre-wakes, by what came of them

	find     chan findRequest // to Run, for an environment's actor
	builds   chan string      // to Run's builder, the environments whose golden source to build
	prewakes chan string      // to Run's prewakers, environments to wake ahead
	stopped  chan struct{}    // closed when Run returns
}

type findRequest struct {
	id    string
	reply chan *actor
}

// New returns a Group for the environments in st, whose boxes drv runs as
// cfg says, that records its changes with tr and its metrics in reg. Call
// Run before anything else.
func New(st Store, drv Driver, cfg Config, tr trace.Tracer, reg prometheus.Registerer, log *slog.Logger) *Group {
	// Boxes take from tens of milliseconds to the timeouts' minutes; disk
	// changes and queueing take from a millisecond.
	boxBuckets := prometheus.ExponentialBuckets(0.025, 2, 13)  // 25ms to 102s
	diskBuckets := prometheus.ExponentialBuckets(0.001, 2, 16) // 1ms to 33s
	metrics := promauto.With(reg)
	histogram := func(name, help string, buckets []float64, label string) *prometheus.HistogramVec {
		return metrics.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, []string{label})
	}
	return &Group{
		store: tracedStore{st: st, tr: tr}, driver: drv, cfg: cfg, tracer: tr, log: log,
		touches:      &http.Client{Timeout: touchTimeout},
		wakes:        histogram("fern_wake_seconds", "Time to wake a box, until its services are ready.", boxBuckets, "result"),
		stops:        histogram("fern_stop_seconds", "Time to stop a box, until it is gone.", boxBuckets, "result"),
		restores:     histogram("fern_restore_seconds", "Time to restore a snapshot, stopping the box included.", boxBuckets, "result"),
		snapshots:    histogram("fern_snapshot_seconds", "Time to take a snapshot, replacing one included.", diskBuckets, "result"),
		goldenBuilds: histogram("fern_golden_build_seconds", "Time to build a golden snapshot.", boxBuckets, "result"),
		goldens: metrics.NewCounterVec(prometheus.CounterOpts{
			Name: "fern_golden_total", Help: "New worktree environments, by whether a golden snapshot was there (hit), is being built (miss) or has no setup contract (none).",
		}, []string{"result"}),
		queue: histogram("fern_queue_seconds", "Time a change waited for its environment's actor.", diskBuckets, "change"),
		idleSleeps: metrics.NewCounter(prometheus.CounterOpts{
			Name: "fern_idle_sleeps_total", Help: "Boxes put to sleep because they were idle.",
		}),
		prewakeResults: metrics.NewCounterVec(prometheus.CounterOpts{
			Name: "fern_prewakes_total",
			Help: "Pre-wakes, by result: woken; then used (a command came) or wasted (the box stopped first); " +
				"or awake (it was already), unknown (no such environment), dropped (the queue was full), error.",
		}, []string{"result"}),
		find: make(chan findRequest), builds: make(chan string, buildQueue), prewakes: make(chan string, prewakeQueue),
		stopped: make(chan struct{}),
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
	for id := range states {
		start(id, true)
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
	if err := store.CheckEnvID(store.EnvOfBox(id)); err != nil {
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
	ctx, sp := span.Start(ctx, g.tracer, change, id)
	defer func() { span.End(sp, err) }()
	queued := time.Now()
	m := message{reply: make(chan error, 1), fn: func(actx context.Context, a *actor) error {
		g.queue.WithLabelValues(change).Observe(time.Since(queued).Seconds())
		// The actor's context carries the caller's span, not its deadline.
		_, wait := g.tracer.Start(ctx, "queue", trace.WithTimestamp(queued))
		wait.End()
		return fn(trace.ContextWithSpan(actx, sp), a)
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
	jobs     map[int]context.CancelFunc // background commands running in it (Job), by number
	next     int                        // the last command's number
	opened   time.Time                  // when the last command started
	closed   time.Time                  // when the last command ended
	lease    time.Time                  // when an awake box with nothing running sleeps instead of after the idle times: set by a pre-wake, or a command that asked
	prewoken bool                       // the lease's wake succeeded, so its end is counted
	idle     *time.Timer                // fires when the box may have been idle long enough
	// stopWatch stops the watch of the agent box's writes for this box
	// (watch), and is nil while there is none.
	stopWatch func()
}

// run handles messages until ctx is done, or until the box is asleep with
// nothing left to do. awake says the box may be running already.
func (a *actor) run(ctx context.Context, awake bool) {
	a.execs, a.jobs = make(map[int]context.CancelFunc), make(map[int]context.CancelFunc)
	a.idle = time.NewTimer(0)
	a.idle.Stop()
	defer a.idle.Stop()
	defer func() {
		if a.stopWatch != nil {
			a.stopWatch()
		}
	}()
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
		if a.awake || len(a.execs) > 0 || len(a.jobs) > 0 {
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

// wake returns the record of the environment a's box belongs to once the
// box is up, waking it unless it is known to be up somewhere other than
// stale.
func (a *actor) wake(ctx context.Context, stale string) (store.Env, error) {
	env, err := a.g.store.Get(ctx, store.EnvOfBox(a.id))
	if err != nil {
		return store.Env{}, err
	}
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
	ctx, sp := span.Start(ctx, a.g.tracer, "box.wake", a.id)
	addr, err := a.g.driver.Wake(ctx, a.g.boxOf(a.id, env))
	span.End(sp, err)
	observe(a.g.wakes, start, &err)
	if err != nil {
		return store.Env{}, err
	}
	a.g.log.InfoContext(ctx, "box woke", slog.String("env", a.id), slog.Duration("took", time.Since(start)))
	a.addr = addr
	a.watch(ctx, env.Workspace)
	return env, nil
}

// stopBox stops the box, then cancels the commands still counted as running
// in it, so that none goes on against the disk that comes next.
func (a *actor) stopBox(ctx context.Context) error {
	a.addr = ""
	// Started again by the next wake.
	if a.stopWatch != nil {
		a.stopWatch()
	}
	start := time.Now()
	spanCtx, sp := span.Start(ctx, a.g.tracer, "box.stop", a.id)
	err := a.g.driver.Sleep(spanCtx, a.id)
	span.End(sp, err)
	observe(a.g.stops, start, &err)
	if err != nil {
		return err
	}
	a.endLease("wasted")
	a.awake = false
	for _, running := range []map[int]context.CancelFunc{a.execs, a.jobs} {
		for n, cancel := range running {
			cancel()
			delete(running, n)
		}
	}
	return nil
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
