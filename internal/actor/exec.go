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
