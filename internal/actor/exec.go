package actor

import (
	"context"
	"log/slog"
	"time"
)

// Exec is a command running in a box, counted by its environment's actor
// until Close or CloseAndSleep.
type Exec struct {
	Addr  string // the box's exec address
	Token string // authenticates the daemon to the box

	close func(ending)
}

// ending is how a command's count ends.
type ending struct {
	sleep bool          // the box sleeps after `after`, if nothing else runs in it
	after time.Duration // …
	dead  bool          // the box didn't answer
}

// Close ends the count of the command, which starts the idle times. It may
// be called more than once.
func (e *Exec) Close() { e.close(ending{}) }

// Dead ends the count of a command that couldn't run because the box at
// Addr didn't answer: the box died, which is counted once however many
// commands report it, and the next command wakes it again.
func (e *Exec) Dead() { e.close(ending{dead: true}) }

// CloseAndSleep ends the count of the command and, if no other command or
// job runs in the box, puts it to sleep after d instead of after the idle
// times: the command was an agent that stopped because it had nothing to do
// (package hibernate, d = 0), one of an agent's Bash calls, which come in
// bursts, or a job the agent stopped.
func (e *Exec) CloseAndSleep(d time.Duration) { e.close(ending{sleep: true, after: d}) }

// Exec wakes environment id's box if needed and counts a command running in
// it until the returned Exec is closed. A restore, sleep or delete calls
// cancel, which must end the command.
func (g *Group) Exec(ctx context.Context, id string, cancel context.CancelFunc) (*Exec, error) {
	return g.exec(ctx, "exec", id, cancel)
}

// Job is Exec for a command that runs in the background, such as a dev
// server Claude started. A job doesn't hold its box awake as a command
// does: the box sleeps after the idle times, Idle.Human after it was last
// used (a command started, or Claude's edits passed on), and that sleep
// cancels the job. A lease (CloseAndSleep, a pre-wake) doesn't apply while
// a job runs.
func (g *Group) Job(ctx context.Context, id string, cancel context.CancelFunc) (*Exec, error) {
	return g.exec(ctx, "job", id, cancel)
}

// Preview is Exec for a request to a preview of the box (internal/preview):
// while it lasts, the box stays awake, as a browser tab with a dev server's
// live reload open keeps it. It is logged only at debug level: a page load
// can be a hundred requests.
// It returns the box's exec address and done, which ends the count.
func (g *Group) Preview(ctx context.Context, id string, cancel context.CancelFunc) (addr string, done func(), err error) {
	ex, err := g.exec(ctx, "preview", id, cancel)
	if err != nil {
		return "", nil, err
	}
	return ex.Addr, ex.Close, nil
}

func (g *Group) exec(ctx context.Context, kind, id string, cancel context.CancelFunc) (*Exec, error) {
	var ex *Exec
	err := g.do(ctx, kind, id, func(ctx context.Context, a *actor) error {
		env, err := a.wake(ctx)
		if err != nil {
			return err
		}
		a.endLease("used")
		a.next++
		n := a.next
		if kind == "job" {
			a.jobs[n] = cancel
		} else {
			a.execs[n] = cancel
		}
		a.opened = time.Now()
		level := slog.LevelInfo
		if kind == "preview" {
			level = slog.LevelDebug
		}
		g.log.Log(ctx, level, kind+" opened", slog.String("env", id), slog.Int("exec", n),
			slog.Int("active", len(a.execs)), slog.Int("jobs", len(a.jobs)))
		wake := a.wakes
		ex = &Exec{Addr: a.addr, Token: env.BoxToken, close: func(how ending) { a.endExec(n, wake, how, level) }}
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

// AdoptClocks sets the idle clocks of environment id's awake box to the times
// it was last used, known from the box: they survive a daemon's restart there.
// A command opened since keeps the box's newer times.
func (g *Group) AdoptClocks(ctx context.Context, id string, opened, closed time.Time) error {
	return g.do(ctx, "adopt", id, func(_ context.Context, a *actor) error {
		if a.phase != boxAsleep && len(a.execs) == 0 {
			a.opened, a.closed = opened, closed
		}
		return nil
	})
}

// linger sets the lease a command that ended at `from` asked for, if nothing
// else runs. A job is something to do: the box sleeps on its idle clocks,
// and a lease set while one runs would apply, long past, when it ends.
func (a *actor) linger(how ending, from time.Time) {
	if how.sleep && len(a.execs) == 0 && len(a.jobs) == 0 {
		a.lease = lease{leaseLinger, from.Add(how.after)}
	}
}

// endExec stops counting command n, which ran in the box's wake number
// wake, from the goroutine that ran it. Its end is logged at level. A
// command that a stop already cancelled is not counted any more.
func (a *actor) endExec(n, wake int, how ending, level slog.Level) {
	end := func(ctx context.Context, _ *actor) error {
		if how.dead && a.phase == boxUp && wake == a.wakes {
			// The first to say so of this wake: the next command wakes it again.
			a.phase, a.addr = boxMaybeUp, ""
			a.crashes = append(a.crashes, time.Now())
			a.g.log.WarnContext(ctx, "box didn't answer", slog.String("env", a.id), slog.Int("exec", n))
		}
		if _, ok := a.jobs[n]; ok {
			delete(a.jobs, n)
			// A job the agent stopped is its last call: the box lingers as
			// after any. One that ended by itself asks for nothing.
			a.linger(how, time.Now())
			a.g.log.InfoContext(ctx, "job closed", slog.String("env", a.id), slog.Int("exec", n), slog.Int("jobs", len(a.jobs)))
			return nil
		}
		if _, ok := a.execs[n]; ok {
			delete(a.execs, n)
			a.closed = time.Now()
			a.linger(how, a.closed)
			a.g.log.Log(ctx, level, "exec closed", slog.String("env", a.id), slog.Int("exec", n),
				slog.Int("active", len(a.execs)), slog.Time("last_opened", a.opened))
		}
		return nil
	}
	select {
	case a.mailbox <- message{fn: end, reply: make(chan error, 1)}:
	case <-a.done:
	}
}
