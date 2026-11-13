package actor

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/nebler/fern/internal/span"
)

// changeTimeout bounds a change of an environment. Changes don't stop when
// their caller goes away, which would leave them half done.
const changeTimeout = 10 * time.Minute

// message is a call for an actor to make.
type message struct {
	fn    func(ctx context.Context, a *actor) error
	reply chan error // buffered, so the actor never waits for the caller
}

// doFunc is Group.do, as the Group's parts (builder, snapshotter, prewaker)
// hold it: they change environments through the actors, and need nothing
// else of the Group for that.
type doFunc func(ctx context.Context, change, id string, fn func(ctx context.Context, a *actor) error) error

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

func (a *actor) handle(ctx context.Context, m message) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), changeTimeout)
	defer cancel()
	m.reply <- m.fn(ctx, a)
}
