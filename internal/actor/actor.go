// Package actor gives every box one goroutine, its actor: an environment's
// own box, its agent box and a golden builder's each have one. The actor of
// an environment's box makes the environment's changes one at a time,
// asking the agent box's actor to stop that box where a change needs it.
// Each counts the commands running in its box, and puts the box to sleep
// when it has been idle. It also wakes boxes ahead of their next command
// (Prewake).
//
// An actor lives while its box may be running or a message for it is
// waiting; an asleep box has none. Its state is touched only by its own
// goroutine.
//
// Where things are: the Group and its Config here; Run's loop, which starts
// and retires actors, in run.go; the change queue (do and the mailbox) in
// queue.go; an actor and its box's wake and stop in lifecycle.go; its crash
// counts in crashes.go; what a box runs and gets in box.go; the changes in
// changes.go; the disk's interfaces in store.go.
package actor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel/trace"

	"github.com/nebler/fern/internal/runsc"
)

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
	// Login returns Claude's login, or "" if there is none yet: a box with
	// Claude's folder gets it. nil: none.
	Login func(ctx context.Context) (string, error)
	// Skills returns the skill packs the node turns on for Claude, such as
	// "beginner design": a box with Claude's folder learns them as
	// FERN_SKILLS (fern-init). nil: none.
	Skills func(ctx context.Context) (string, error)
	// MCPPort is the daemon's port agent boxes may reach: they learn it as
	// FERN_MCP.
	MCPPort int
	// T3MCP is where T3 serves its own MCP server for its Claude, on the
	// node's loopback, such as 127.0.0.1:30773; "" for none. An agent box's
	// fern-init listens there, in the box, and passes the calls to the
	// daemon (FERN_T3_ADDR, FERN_T3_MCP), which passes them to T3.
	T3MCP string
	// Preview is where people open previews, such as localhost:30780
	// (internal/preview). A box learns its own as FERN_PREVIEW, for Claude
	// to hand out links; "" means there are none.
	Preview string
	// Limits are services' boxes' limits; the zero value means
	// runsc.DefaultLimits().
	Limits runsc.Limits
	// MaxLimits is the most a repository's services.yaml may ask for its
	// box; the zero value means Limits, so that it can only ask for less.
	MaxLimits runsc.Limits
	// SnapshotFailures keeps a turn's snapshot that failed with its
	// environment, for `fern doctor` and the thread's Claude (Snapshot,
	// notices): the store. nil: none is kept, and none is told of.
	SnapshotFailures SnapshotFailures
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
	// boxes connects to boxes for their commands (Dial), and asks them when
	// they were last used (AdoptClocks). A box that vanished can leave an
	// address nothing answers; the same short connect timeout as readiness
	// gets to the next wake without waiting a second on that stale address.
	boxes *http.Client

	// builder makes the golden builds.
	builder *builder

	// Time histograms, by result ("ok" or "error"), and, for queue, by
	// change: how long a change waited for its actor.
	wakes, stops, restores, snapshots, queue *prometheus.HistogramVec
	idleSleeps                               prometheus.Counter
	boxFailures                              prometheus.Counter     // wakes that failed, and boxes that died under a command
	prewakeResults                           *prometheus.CounterVec // pre-wakes, by what came of them

	find     chan findRequest // to Run, for an environment's actor
	prewakes chan string      // to Run's prewakers, environments to wake ahead
	stopped  chan struct{}    // closed when Run returns
	// crashed asks Run what is known of boxes' crashes (crashing).
	crashed chan crashRequest
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
	traced := tracedStore{st: st, tr: tr}
	g := &Group{
		store: traced, driver: drv, cfg: cfg, tracer: tr, log: log,
		touches:   &http.Client{Timeout: touchTimeout},
		boxes:     &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 250 * time.Millisecond}).DialContext}},
		wakes:     histogram("fern_wake_seconds", "Time to wake a box, until its services are ready.", boxBuckets, "result"),
		stops:     histogram("fern_stop_seconds", "Time to stop a box, until it is gone.", boxBuckets, "result"),
		restores:  histogram("fern_restore_seconds", "Time to restore a snapshot, stopping the box included.", boxBuckets, "result"),
		snapshots: histogram("fern_snapshot_seconds", "Time to take a snapshot, replacing one included.", diskBuckets, "result"),
		queue:     histogram("fern_queue_seconds", "Time a change waited for its environment's actor.", diskBuckets, "change"),
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
		find: make(chan findRequest), prewakes: make(chan string, prewakeQueue),
		crashed: make(chan crashRequest),
		stopped: make(chan struct{}),
	}
	g.builder = &builder{
		store: traced, image: cfg.Image, setupTimeout: cfg.SetupTimeout, runIn: cfg.RunSetup, log: log,
		goldens: metrics.NewCounterVec(prometheus.CounterOpts{
			Name: "fern_golden_total", Help: "New worktree environments, by whether a golden snapshot was there (hit), is being built (miss) or has no setup contract (none).",
		}, []string{"result"}),
		took:  histogram("fern_golden_build_seconds", "Time to build a golden snapshot.", boxBuckets, "result"),
		queue: make(chan string, buildQueue),
		do:    g.do, exec: g.Exec, delete: g.Delete,
	}
	return g
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
