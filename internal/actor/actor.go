// Package actor gives every environment one goroutine, its actor, which makes
// the environment's changes one at a time, counts the commands running in its
// box, and puts the box to sleep when it has been idle.
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

	"github.com/nebler/fern/internal/driver"
	"github.com/nebler/fern/internal/store"
)

// Store keeps environments and snapshots on disk.
type Store interface {
	Create(ctx context.Context, id string, opts store.CreateOptions) (store.Env, error)
	Get(ctx context.Context, id string) (store.Env, error)
	Delete(ctx context.Context, id string) error
	Snapshot(ctx context.Context, id, snapID string) error
	DeleteSnapshot(ctx context.Context, id, snapID string) error
	Snapshots(ctx context.Context, id string) ([]store.Snapshot, error)
	Restore(ctx context.Context, id, snapID string) error
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
// durations keep boxes awake.
type Idle struct {
	Agent time.Duration
	Human time.Duration
}

// changeTimeout bounds a change of an environment. Changes don't stop when
// their caller goes away, which would leave them half done.
const changeTimeout = 10 * time.Minute

// ErrStopped means the daemon is shutting down.
var ErrStopped = errors.New("daemon is stopping")

// Group runs the actors of a node's environments. It is a Prometheus
// collector of their metrics.
type Group struct {
	store  Store
	driver Driver
	idle   Idle
	log    *slog.Logger

	wakes      prometheus.Histogram
	restores   prometheus.Histogram
	snapshots  prometheus.Histogram
	idleSleeps prometheus.Counter

	find    chan findRequest // to Run, for an environment's actor
	stopped chan struct{}    // closed when Run returns
}

type findRequest struct {
	id    string
	reply chan *actor
}

// New returns a Group for the environments in st, run by drv. Call Run
// before anything else.
func New(st Store, drv Driver, idle Idle, log *slog.Logger) *Group {
	return &Group{
		store: st, driver: drv, idle: idle, log: log,
		wakes: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "fern_wake_seconds", Help: "Time to wake a box, until its services are ready.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 10),
		}),
		restores: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "fern_restore_seconds", Help: "Time to restore a snapshot, stopping the box included.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 10),
		}),
		snapshots: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "fern_snapshot_seconds", Help: "Time to take a snapshot.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 12),
		}),
		idleSleeps: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "fern_idle_sleeps_total", Help: "Boxes put to sleep because they were idle.",
		}),
		find: make(chan findRequest), stopped: make(chan struct{}),
	}
}

func (g *Group) collectors() []prometheus.Collector {
	return []prometheus.Collector{g.wakes, g.restores, g.snapshots, g.idleSleeps}
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

// actorOf returns environment id's actor, starting one if it has none.
func (g *Group) actorOf(ctx context.Context, id string) (*actor, error) {
	if err := store.CheckEnvID(id); err != nil {
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

// do runs fn on environment id's actor and returns its error. Waiting for
// the actor to take fn can be cancelled; once it has, fn runs to the end, and
// do waits for it, so that fn never uses what its caller has let go of (such
// as a request's body). changeTimeout bounds the wait.
func (g *Group) do(ctx context.Context, id string, fn func(ctx context.Context, a *actor) error) error {
	m := message{fn: fn, reply: make(chan error, 1)}
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

// Create makes environment id. Its box comes with its first wake.
func (g *Group) Create(ctx context.Context, id string, opts store.CreateOptions) error {
	return g.do(ctx, id, func(ctx context.Context, _ *actor) error {
		_, err := g.store.Create(ctx, id, opts)
		return err
	})
}

// Delete stops environment id's box, then removes its disk.
func (g *Group) Delete(ctx context.Context, id string) error {
	return g.do(ctx, id, func(ctx context.Context, a *actor) error {
		if _, err := g.store.Get(ctx, id); err != nil {
			return err
		}
		if err := a.stopBox(ctx, g.driver.Delete); err != nil {
			return err
		}
		return g.store.Delete(ctx, id)
	})
}

// Snapshot takes snapshot snapID of environment id, again if replace is set.
func (g *Group) Snapshot(ctx context.Context, id, snapID string, replace bool) error {
	return g.do(ctx, id, func(ctx context.Context, _ *actor) error {
		if replace {
			if err := g.store.DeleteSnapshot(ctx, id, snapID); err != nil {
				return err
			}
		}
		start := time.Now()
		if err := g.store.Snapshot(ctx, id, snapID); err != nil {
			return err
		}
		g.snapshots.Observe(time.Since(start).Seconds())
		return nil
	})
}

// DeleteSnapshot removes snapshot snapID of environment id; removing one that
// doesn't exist is not an error.
func (g *Group) DeleteSnapshot(ctx context.Context, id, snapID string) error {
	return g.do(ctx, id, func(ctx context.Context, _ *actor) error {
		return g.store.DeleteSnapshot(ctx, id, snapID)
	})
}

// Restore puts environment id to sleep, ending the commands running in its
// box, and swaps its disk for snapshot snapID. It stays asleep; the next
// command wakes it on the restored disk.
func (g *Group) Restore(ctx context.Context, id, snapID string) error {
	return g.do(ctx, id, func(ctx context.Context, a *actor) error {
		// Check the snapshot before stopping the box: a typo mustn't cost
		// the agent its running processes.
		snaps, err := g.store.Snapshots(ctx, id)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(snaps, func(snap store.Snapshot) bool { return snap.ID == snapID }) {
			return fmt.Errorf("snapshot %s/%s: %w", id, snapID, store.ErrNotFound)
		}
		start := time.Now()
		if err := a.stopBox(ctx, g.driver.Sleep); err != nil {
			return err
		}
		if err := g.store.Restore(ctx, id, snapID); err != nil {
			return err
		}
		g.restores.Observe(time.Since(start).Seconds())
		return nil
	})
}

// Sleep stops environment id's box, ending the commands running in it.
func (g *Group) Sleep(ctx context.Context, id string) error {
	return g.do(ctx, id, func(ctx context.Context, a *actor) error {
		if _, err := g.store.Get(ctx, id); err != nil {
			return err
		}
		return a.stopBox(ctx, g.driver.Sleep)
	})
}

// Wake starts environment id's box. It counts as a touch, so the box stays
// up for the idle times.
func (g *Group) Wake(ctx context.Context, id string) error {
	return g.do(ctx, id, func(ctx context.Context, a *actor) error {
		if _, err := a.wake(ctx, ""); err != nil {
			return err
		}
		now := time.Now()
		a.opened, a.closed = now, now
		return nil
	})
}

// Exec is a command running in a box, counted by its environment's actor
// until Close.
type Exec struct {
	Addr  string // the box's exec address
	Token string // authenticates the daemon to the box

	close func()
}

// Close ends the count of the command, which starts the idle times. It may
// be called more than once.
func (e *Exec) Close() { e.close() }

// Exec wakes environment id's box if needed and counts a command running in
// it until the returned Exec is closed. A restore, sleep or delete calls
// cancel, which must end the command. stale is an address of the box that
// didn't answer, if any: then the box is woken again.
func (g *Group) Exec(ctx context.Context, id, stale string, cancel context.CancelFunc) (*Exec, error) {
	var ex *Exec
	err := g.do(ctx, id, func(ctx context.Context, a *actor) error {
		env, err := a.wake(ctx, stale)
		if err != nil {
			return err
		}
		a.next++
		n := a.next
		a.execs[n] = cancel
		a.opened = time.Now()
		ex = &Exec{Addr: a.addr, Token: env.BoxToken, close: func() { a.endExec(n) }}
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

	addr   string                     // the box's exec address while it is known to be up
	awake  bool                       // the box may be running
	execs  map[int]context.CancelFunc // commands running in the box, by number
	next   int                        // the last command's number
	opened time.Time                  // when the last command started
	closed time.Time                  // when the last command ended
	idle   *time.Timer                // fires when the box may have been idle long enough
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
	env, err := a.g.store.Get(ctx, a.id)
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
	addr, err := a.g.driver.Wake(ctx, env)
	if err != nil {
		return store.Env{}, err
	}
	a.g.wakes.Observe(time.Since(start).Seconds())
	a.addr = addr
	return env, nil
}

// stopBox stops the box with stop (the driver's Sleep or Delete), then
// cancels the commands still counted as running in it, so that none goes on
// against the disk that comes next.
func (a *actor) stopBox(ctx context.Context, stop func(ctx context.Context, id string) error) error {
	a.addr = ""
	if err := stop(ctx, a.id); err != nil {
		return err
	}
	a.awake = false
	for n, cancel := range a.execs {
		cancel()
		delete(a.execs, n)
	}
	return nil
}

// endExec stops counting command n, from the goroutine that ran it. A
// command that a stop already cancelled is not counted any more.
func (a *actor) endExec(n int) {
	end := func(context.Context, *actor) error {
		if _, ok := a.execs[n]; ok {
			delete(a.execs, n)
			a.closed = time.Now()
		}
		return nil
	}
	select {
	case a.mailbox <- message{fn: end, reply: make(chan error, 1)}:
	case <-a.done:
	}
}

// deadline is when an awake box with nothing running may go to sleep.
func (a *actor) deadline() time.Time {
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
	a.g.log.InfoContext(ctx, "idle box went to sleep", slog.String("env", a.id))
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
