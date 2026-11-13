package actor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nebler/fern/internal/span"
	"github.com/nebler/fern/internal/store"
)

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
	// repository's setup script (boxOf): a build sets it for that step
	// (buildGolden).
	setup bool
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
			"It is tried again in %s, or at once by `fern wake %s` on the server, where `fern logs %s` shows what it last printed",
			a.id, ErrCrashing, len(a.crashes), crashWindow, a.crashCause,
			time.Until(a.crashes[0].Add(crashWindow)).Round(time.Second), a.id, store.EnvOfBox(a.id))
	}
	// Set first: a wake that fails half-way may leave the box running. Its
	// idle times start now, so a slow wake isn't put back to sleep at once.
	if a.phase == boxAsleep {
		a.opened, a.closed = time.Now(), time.Now()
	}
	a.phase = boxMaybeUp
	start := time.Now()
	ctx, sp := span.Start(ctx, a.g.tracer, "box.wake", a.id)
	addr, err := a.startBox(ctx, env)
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
	if a.g.cfg.Woken != nil && store.EnvOfBox(a.id) == a.id && !store.IsBuilder(a.id) {
		select {
		case a.g.cfg.Woken <- a.id:
		default:
		}
	}
	a.watch(ctx, env.Workspace)
	return nil
}

// startBox has the driver wake a's box, of env, with what boxOf decides it
// runs and gets, and returns its exec address.
func (a *actor) startBox(ctx context.Context, env store.Env) (string, error) {
	return a.g.driver.Wake(ctx, a.g.boxOf(ctx, a.id, env, a.setup))
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
