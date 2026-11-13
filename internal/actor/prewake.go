package actor

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

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

// prewaker wakes boxes ahead of their next command, a few at a time from
// goroutines of Run's (prewakeBoxes). It is a part of the Group with what
// it uses alone: no disk, and the actors through do; an actor's lease, which
// says when a box woken ahead sleeps, stays the actor's.
type prewaker struct {
	idle    Idle
	queue   chan string            // environments to wake ahead
	results *prometheus.CounterVec // pre-wakes, by what came of them
	log     *slog.Logger
	do      doFunc
}

// Prewake asks for environment id's box to be woken in the background,
// because a command is likely soon: T3 asks when a thread is opened or
// reverted. It returns at once. A box that is awake is left as it is, its
// idle times included; one it wakes sleeps after Idle.Prewake unless a
// command comes first. An unknown environment is only counted.
func (g *Group) Prewake(id string) error {
	return g.prewaker.ask(id)
}

func (p *prewaker) ask(id string) error {
	if err := store.CheckEnvID(store.EnvOfBox(id)); err != nil {
		return err
	}
	// With idle sleep off, a box woken ahead would never sleep again.
	if idle := p.idle; idle.Prewake == 0 || idle.Agent == 0 || idle.Human == 0 {
		return nil
	}
	select {
	case p.queue <- id:
	default:
		p.results.WithLabelValues("dropped").Inc()
	}
	return nil
}

// prewakeBoxes makes the pre-wakes asked for, one at a time, until ctx is
// done.
func (p *prewaker) prewakeBoxes(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-p.queue:
			p.prewake(ctx, id)
		}
	}
}

func (p *prewaker) prewake(ctx context.Context, id string) {
	result := "woken"
	err := p.do(ctx, "prewake", id, func(ctx context.Context, a *actor) error {
		if a.phase != boxAsleep {
			result = "awake"
			return nil
		}
		// Set first, so that a wake that fails half-way, leaving the box
		// running, still ends with the lease.
		a.lease = lease{leasePrewaking, time.Now().Add(p.idle.Prewake)}
		if err := a.wake(ctx); err != nil {
			if a.phase == boxAsleep {
				a.lease = lease{}
			}
			return err
		}
		a.lease = lease{leasePrewoken, time.Now().Add(p.idle.Prewake)}
		p.log.InfoContext(ctx, "box woken ahead", slog.String("env", id), slog.Time("until", a.lease.until))
		return nil
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Such as a thread whose first checkpoint hasn't adopted its worktree.
		result = "unknown"
	case err != nil:
		result = "error"
		p.log.WarnContext(ctx, "pre-wake failed", slog.String("env", id), slog.Any("error", err))
	}
	p.results.WithLabelValues(result).Inc()
}
