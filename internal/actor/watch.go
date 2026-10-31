package actor

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/setup"
	"github.com/nebler/fern/internal/store"
)

// Watcher reports the files a box writes in a folder (package watch).
type Watcher interface {
	Watch(ctx context.Context, dir, box string, changed func(ctx context.Context, paths []string)) error
}

// touchTimeout bounds one touch request to a box.
const touchTimeout = 5 * time.Second

// watch starts passing the files the environment's agent box writes in
// workspace on to a's box, the environment's own, which runs its dev
// servers, unless a is another kind of box, already does, or the Group has
// no Watcher. stopWatch, which stopBox and run's end call, stops it. The
// watch is the services' box's, not the agent box's: the agent box often
// sleeps right after a write, and its changes must still be passed on.
func (a *actor) watch(ctx context.Context, workspace string) {
	if a.g.cfg.Watcher == nil || !a.g.cfg.Agents || store.IsAgentBox(a.id) || setup.IsBuilder(a.id) || a.stopWatch != nil {
		return
	}
	// The watch outlives the wake that starts it, and isn't part of its trace.
	ctx = trace.ContextWithSpanContext(context.WithoutCancel(ctx), trace.SpanContext{})
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	id := a.id
	go func() {
		defer close(done)
		err := a.g.cfg.Watcher.Watch(ctx, workspace, store.AgentBox(id), func(ctx context.Context, paths []string) { a.g.touch(ctx, id, paths) })
		if err != nil {
			a.g.log.WarnContext(ctx, "watching the workspace failed: its dev servers won't hear of Claude's edits",
				slog.String("env", id), slog.Any("error", err))
		}
	}()
	a.stopWatch = func() {
		cancel()
		<-done
		a.stopWatch = nil
	}
}

// touch tells environment id's box that paths changed, so that its file
// watchers (dev servers) hear of them, if the box is awake. It counts as a
// use of the box, for the human clock that keeps its jobs (Job).
func (g *Group) touch(ctx context.Context, id string, paths []string) {
	var addr, token string
	err := g.do(ctx, "touch", id, func(ctx context.Context, a *actor) error {
		if a.addr == "" {
			return nil
		}
		env, err := g.store.Get(ctx, id)
		if err != nil {
			return err
		}
		addr, token = a.addr, env.BoxToken
		a.opened = time.Now()
		return nil
	})
	if err == nil && addr != "" {
		ctx, cancel := context.WithTimeout(ctx, touchTimeout)
		err = boxexec.Touch(ctx, g.touches, addr, token, paths)
		cancel()
	}
	if err != nil && ctx.Err() == nil {
		g.log.WarnContext(ctx, "passing Claude's edits on to the box failed", slog.String("env", id),
			slog.Int("files", len(paths)), slog.Any("error", err))
	}
}
