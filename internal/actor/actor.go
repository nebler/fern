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
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel/trace"

	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/setup"
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
	PruneGoldens(ctx context.Context, id string) error
	Orphans(ctx context.Context) ([]string, error)
	BuildSource(ctx context.Context, id string, src store.SnapshotRef) error
	StripSource(ctx context.Context, id string) error
	Sources(ctx context.Context, name string) ([]store.SnapshotRef, error)
	SetBuildFailure(ctx context.Context, id string, f *store.BuildFailure) error
	BuildFailure(ctx context.Context, id string) (store.BuildFailure, bool, error)
	Space() (store.Space, error)
}

// Driver runs boxes.
type Driver interface {
	Wake(ctx context.Context, box runsc.Box) (addr string, err error)
	// Token returns the token a running box was started with.
	Token(id string) (string, error)
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
	// Woken, if not nil, gets the ID of every environment whose own box
	// (not its agent box, not a golden builder) has just woken, so that its
	// default preview can be rendered ahead (preview.Server.Warm). A full
	// channel drops it.
	Woken chan<- string
	// SetupTimeout is how long a repository's setup script may run in a
	// golden build; 0 means DefaultSetupTimeout.
	SetupTimeout time.Duration
	// RunSetup runs argv in the box at addr, which token opens, with what it
	// prints written to out, and returns its exit code: a golden build's
	// setup script. nil means over boxexec, as every command; tests have no
	// box to connect to.
	RunSetup func(ctx context.Context, addr, token string, argv []string, out io.Writer) (int, error)
}

// DefaultSetupTimeout is how long a setup script may run. The contract asks
// for 20 minutes (architecture §8); a native toolchain built in a box, at
// about twice the CPU cost, took 25 (lab/RESULTS.md, 2026-11-12).
const DefaultSetupTimeout = time.Hour

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

// ErrCrashing means a box died or failed to wake crashLimit times within
// crashWindow: it isn't woken again until the oldest of those is older,
// or a person wakes it (Wake).
var ErrCrashing = errors.New("the box keeps crashing")

// A box that dies under its commands, or fails to wake, is woken again at
// the next command, up to crashLimit times in crashWindow; then commands
// fail at once with ErrCrashing, rather than each paying for a wake that
// ends the same way.
const (
	crashLimit  = 3
	crashWindow = 5 * time.Minute
)

// Group runs the actors of a node's environments.
type Group struct {
	store  Store
	driver Driver
	cfg    Config
	tracer trace.Tracer
	log    *slog.Logger
	// touches asks services' boxes to touch the files Claude changed.
	touches *http.Client
	// boxes connects to boxes for their commands (Dial), and asks them when
	// they were last used (AdoptClocks). A box that vanished can leave an
	// address nothing answers; the same short connect timeout as readiness
	// gets to the next wake without waiting a second on that stale address.
	boxes *http.Client

	// Time histograms, by result ("ok" or "error"), and, for queue, by
	// change: how long a change waited for its actor.
	wakes, stops, restores, snapshots, queue, goldenBuilds *prometheus.HistogramVec
	idleSleeps                                             prometheus.Counter
	boxFailures                                            prometheus.Counter     // wakes that failed, and boxes that died under a command
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
		boxes:        &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 250 * time.Millisecond}).DialContext}},
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
		boxFailures: metrics.NewCounter(prometheus.CounterOpts{
			Name: "fern_box_failures_total", Help: "Wakes that failed, and boxes that stopped answering under a command, as one does that ran out of memory.",
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
	// An actor ends when its box sleeps; the next one goes on counting
	// the box's crashes.
	crashes := make(map[string][]time.Time)
	causes := make(map[string]string)
	exited := make(chan *actor)
	start := func(id string, awake bool) *actor {
		a := &actor{
			id: id, g: g, mailbox: make(chan message), done: make(chan struct{}),
			crashes: slices.Clone(crashes[id]), crashCause: causes[id],
		}
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
	// retire takes an actor whose goroutine has ended (a.done is closed) off
	// the books, once, and keeps its count of crashes for the next.
	retire := func(a *actor) {
		if actors[a.id] != a {
			return
		}
		delete(actors, a.id)
		if cs := recent(a.crashes); len(cs) > 0 {
			crashes[a.id], causes[a.id] = cs, a.crashCause
		} else {
			delete(crashes, a.id)
			delete(causes, a.id)
		}
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
			if ok && isClosed(a.done) {
				// It has ended, and its word of that is still on the way.
				retire(a)
				ok = false
			}
			if !ok {
				a = start(req.id, false)
			}
			req.reply <- a
		case a := <-exited:
			retire(a)
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

// boxPhase is what an actor knows of its box. It moves in four ways:
//
//	asleep   → maybe up   a wake starts (wake), or the box was found running at the daemon's start (run)
//	maybe up → up         the wake succeeded (wake)
//	up       → maybe up   a stop starts (stopBox), or a command found the box dead (endExec)
//	maybe up → asleep     the stop succeeded (stopBox)
//
// A wake or a stop that fails leaves it maybe up, from where either can be
// tried again. When it sleeps is not a state of its own: sleepAt works it
// out from the phase, the commands counted, the lease and the two clocks.
type boxPhase int

const (
	// boxAsleep: it doesn't run.
	boxAsleep boxPhase = iota
	// boxMaybeUp: it may be running, and nothing can be run in it before a
	// wake: it was found at the daemon's start, is being woken, died under a
	// command, or a wake or a stop of it failed half-way. It is put to
	// sleep like a box that is up.
	boxMaybeUp
	// boxUp: it runs, at addr.
	boxUp
)

// actor owns one environment. Only its own goroutine touches the fields
// below done.
type actor struct {
	id      string
	g       *Group
	mailbox chan message
	done    chan struct{} // closed when run returns

	phase boxPhase // what is known of the box
	addr  string   // boxUp: the box's exec address
	token string   // boxUp: the token it was started with
	wakes int      // how often this actor has woken the box: a command names the wake it ran in when it reports the box dead
	// running is everything counted as running in the box, by number: the
	// one list of what holds it. A command or a preview request keeps it
	// awake; a job doesn't, past the idle times (sleepAt).
	running map[int]running
	next    int         // the last command's number
	opened  time.Time   // when the last command started
	closed  time.Time   // when the last command ended
	lease   lease       // when the box sleeps instead of after the idle times, and why
	idle    *time.Timer // fires when the box may have been idle long enough
	// stopWatch stops the watch of the agent box's writes for this box
	// (watch), and is nil while there is none.
	stopWatch func()
	// crashes are when the box was found dead or failed to wake, within
	// crashWindow; Run reads them once the actor is done, for the next.
	// crashCause is why, the last time: what whoever is told that it keeps
	// crashing needs, and can't read from a log on the node.
	crashes    []time.Time
	crashCause string
	// setup makes the next wake of a golden builder's box the one for its
	// repository's setup script (runsc.Setup): a build sets it for that
	// step (buildGolden).
	setup bool
}

// recent returns the times in ts within crashWindow of now.
func recent(ts []time.Time) []time.Time {
	return slices.DeleteFunc(ts, func(t time.Time) bool { return time.Since(t) >= crashWindow })
}

// run handles messages until ctx is done, or until the box is asleep with
// nothing left to do. awake says the box may be running already.
func (a *actor) run(ctx context.Context, awake bool) {
	a.running = make(map[int]running)
	a.idle = time.NewTimer(0)
	a.idle.Stop()
	defer a.idle.Stop()
	defer func() {
		if a.stopWatch != nil {
			a.stopWatch()
		}
	}()
	if awake {
		a.phase = boxMaybeUp
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
		if a.phase != boxAsleep || len(a.running) > 0 {
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

// wake returns once a's box is up, at a.addr with a.token, waking it unless
// it is known to be.
func (a *actor) wake(ctx context.Context) error {
	env, err := a.g.store.Get(ctx, store.EnvOfBox(a.id))
	if err != nil {
		return err
	}
	if a.phase == boxUp {
		return nil
	}
	if a.crashes = recent(a.crashes); len(a.crashes) >= crashLimit {
		return fmt.Errorf("%s: %w: it died or failed to wake %d times in %s, the last time because: %s. "+
			"It is tried again in %s, or at once by `fern wake %s` on the server, where its log is boxes/%s/log in the pool",
			a.id, ErrCrashing, len(a.crashes), crashWindow, a.crashCause,
			time.Until(a.crashes[0].Add(crashWindow)).Round(time.Second), a.id, a.id)
	}
	// Set first: a wake that fails half-way may leave the box running. Its
	// idle times start now, so a slow wake isn't put back to sleep at once.
	if a.phase == boxAsleep {
		a.opened, a.closed = time.Now(), time.Now()
	}
	a.phase = boxMaybeUp
	start := time.Now()
	ctx, sp := span.Start(ctx, a.g.tracer, "box.wake", a.id)
	box := a.g.boxOf(a.id, env)
	if a.setup && box.Kind == runsc.Builder {
		box.Kind = runsc.Setup
	}
	if box.Env.Claude != "" {
		box.Notices = a.g.notices(ctx, env)
	}
	addr, err := a.g.driver.Wake(ctx, box)
	span.End(sp, err)
	observe(a.g.wakes, start, &err)
	if err != nil {
		a.crashes, a.crashCause = append(a.crashes, time.Now()), err.Error()
		a.g.boxFailures.Inc()
		return err
	}
	// The driver made the token at the box's start, or kept it since.
	token, err := a.g.driver.Token(a.id)
	if err != nil {
		a.crashes, a.crashCause = append(a.crashes, time.Now()), err.Error()
		a.g.boxFailures.Inc()
		return fmt.Errorf("the token of %s: %w", a.id, err)
	}
	a.g.log.InfoContext(ctx, "box woke", slog.String("env", a.id), slog.Duration("took", time.Since(start)))
	a.phase, a.addr, a.token = boxUp, addr, token
	a.wakes++
	if a.g.cfg.Woken != nil && store.EnvOfBox(a.id) == a.id && !setup.IsBuilder(a.id) {
		select {
		case a.g.cfg.Woken <- a.id:
		default:
		}
	}
	a.watch(ctx, env.Workspace)
	return nil
}

// stopBox stops the box, then cancels the commands still counted as running
// in it, so that none goes on against the disk that comes next.
func (a *actor) stopBox(ctx context.Context) error {
	// A stop that fails may have stopped it all the same.
	if a.phase == boxUp {
		a.phase, a.addr, a.token = boxMaybeUp, "", ""
	}
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
	a.phase = boxAsleep
	for n, r := range a.running {
		r.cancel()
		delete(a.running, n)
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
