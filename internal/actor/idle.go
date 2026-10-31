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
		if a.awake {
			result = "awake"
			return nil
		}
		// Set first, so that a wake that fails half-way, leaving the box
		// running, still ends with the lease.
		a.lease = time.Now().Add(g.cfg.Idle.Prewake)
		if _, err := a.wake(ctx, ""); err != nil {
			if !a.awake {
				a.lease = time.Time{}
			}
			return err
		}
		a.lease, a.prewoken = time.Now().Add(g.cfg.Idle.Prewake), true
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

// endLease ends a pre-wake's lease, if there is one, counting result for a
// wake that succeeded: used when a command came, wasted when the box stopped
// first.
func (a *actor) endLease(result string) {
	if a.prewoken {
		a.g.prewakeResults.WithLabelValues(result).Inc()
	}
	a.lease, a.prewoken = time.Time{}, false
}

// deadline is when an awake box with nothing running but jobs may go to
// sleep.
func (a *actor) deadline() time.Time {
	// A lease is for a box with nothing left to do; jobs are something.
	if !a.lease.IsZero() && len(a.jobs) == 0 {
		return a.lease
	}
	return later(a.closed.Add(a.g.cfg.Idle.Agent), a.opened.Add(a.g.cfg.Idle.Human))
}

// armIdle sets the idle timer for an awake box with nothing running, and
// stops it otherwise.
func (a *actor) armIdle() {
	if !a.awake || len(a.execs) > 0 || a.g.cfg.Idle.Agent == 0 || a.g.cfg.Idle.Human == 0 {
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
	if err := a.stopBox(ctx); err != nil {
		// Try again after another idle time.
		a.g.log.WarnContext(ctx, "idle sleep", slog.String("env", a.id), slog.Any("error", err))
		a.closed = time.Now()
		return
	}
	a.g.idleSleeps.Inc()
	a.g.log.InfoContext(ctx, "idle box went to sleep", slog.String("env", a.id),
		slog.Time("last_opened", a.opened), slog.Time("last_closed", a.closed), slog.Time("idle_at", a.deadline()))
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
