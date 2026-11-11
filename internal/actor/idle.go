package actor

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nebler/fern/internal/store"
)

// prewakers is how many pre-wakes run at once, and prewakeQueue how many may
// wait for one. A pre-wake is a guess, so it never holds up more than two
// boxes' starts on the node, and one asked for when the queue is full is
// dropped.
const (
	prewakers    = 2
	prewakeQueue = 8
)

// Prewake asks for environment id's box to be woken in the background,
// because a command is likely soon: T3 asks when a thread is opened or
// reverted. It returns at once. A box that is awake is left as it is, its
// idle times included; one it wakes sleeps after Idle.Prewake unless a
// command comes first. An unknown environment is only counted.
func (g *Group) Prewake(id string) error {
	if err := store.CheckEnvID(store.EnvOfBox(id)); err != nil {
		return err
	}
	// With idle sleep off, a box woken ahead would never sleep again.
	if idle := g.cfg.Idle; idle.Prewake == 0 || idle.Agent == 0 || idle.Human == 0 {
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
		if a.phase != boxAsleep {
			result = "awake"
			return nil
		}
		// Set first, so that a wake that fails half-way, leaving the box
		// running, still ends with the lease.
		a.lease = lease{leasePrewaking, time.Now().Add(g.cfg.Idle.Prewake)}
		if _, err := a.wake(ctx); err != nil {
			if a.phase == boxAsleep {
				a.lease = lease{}
			}
			return err
		}
		a.lease = lease{leasePrewoken, time.Now().Add(g.cfg.Idle.Prewake)}
		g.log.InfoContext(ctx, "box woken ahead", slog.String("env", id), slog.Time("until", a.lease.until))
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

// lease is when a box with no command running sleeps instead of after the
// idle times, and why. Any command that starts ends it.
type lease struct {
	kind  leaseKind
	until time.Time
}

type leaseKind int

const (
	// leaseNone: the box sleeps after the idle times.
	leaseNone leaseKind = iota
	// leaseLinger: its last command asked for a quick sleep (CloseAndSleep).
	// Only set with no job running.
	leaseLinger
	// leasePrewaking: a pre-wake is under way, or failed with the box
	// perhaps running.
	leasePrewaking
	// leasePrewoken: a pre-wake woke it. Its end is counted: used by a
	// command, or wasted.
	leasePrewoken
	// leaseRetry: its stop failed, and is tried again then, whatever runs.
	leaseRetry
)

// endLease ends the lease, if there is one, counting result for a pre-wake
// that succeeded: used when a command came, wasted when the box stopped
// first.
func (a *actor) endLease(result string) {
	if a.lease.kind == leasePrewoken {
		a.g.prewakeResults.WithLabelValues(result).Inc()
	}
	a.lease = lease{}
}

// sleepAt returns when a box may be put to sleep, and whether it may at
// all: not while it is asleep, a command runs in it, or idle sleep is off.
// A job doesn't keep it awake past the idle times, but it does past a
// linger or a pre-wake, which are for a box with nothing left to do.
func sleepAt(phase boxPhase, execs, jobs int, l lease, opened, closed time.Time, idle Idle) (time.Time, bool) {
	if phase == boxAsleep || execs > 0 || idle.Agent == 0 || idle.Human == 0 {
		return time.Time{}, false
	}
	if l.kind == leaseRetry || (l.kind != leaseNone && jobs == 0) {
		return l.until, true
	}
	return later(closed.Add(idle.Agent), opened.Add(idle.Human)), true
}

func (a *actor) sleepAt() (time.Time, bool) {
	return sleepAt(a.phase, len(a.execs), len(a.jobs), a.lease, a.opened, a.closed, a.g.cfg.Idle)
}

// armIdle sets the idle timer for when the box may sleep, and stops it if
// it may not.
func (a *actor) armIdle() {
	at, ok := a.sleepAt()
	if !ok {
		a.idle.Stop()
		return
	}
	a.idle.Reset(time.Until(at))
}

func (a *actor) sleepIfIdle(ctx context.Context) {
	at, ok := a.sleepAt()
	if !ok || time.Now().Before(at) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), changeTimeout)
	defer cancel()
	if err := a.stopBox(ctx); err != nil {
		// Try again after another idle time, whatever the lease was: a time
		// in the past would have the timer retry at once, over and over.
		a.g.log.WarnContext(ctx, "idle sleep", slog.String("env", a.id), slog.Any("error", err))
		if a.lease.kind == leaseNone {
			a.lease.kind = leaseRetry
		}
		a.lease.until = time.Now().Add(a.g.cfg.Idle.Agent)
		return
	}
	a.g.idleSleeps.Inc()
	a.g.log.InfoContext(ctx, "idle box went to sleep", slog.String("env", a.id),
		slog.Time("last_opened", a.opened), slog.Time("last_closed", a.closed), slog.Time("idle_at", at))
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
