package actor

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/store"
)

// Watcher reports the files a box writes in a folder (package watch).
type Watcher interface {
	Watch(ctx context.Context, dir, box string, changed func(ctx context.Context, paths []string)) error
}

// touchTimeout bounds one touch request to a box.
const touchTimeout = 5 * time.Second

// watch starts passing the files agent box a writes in workspace on to the
// services' box, unless a isn't an agent box, already does, or the Group has
// no Watcher. stopWatch, which stopBox and run's end call, stops it.
func (a *actor) watch(ctx context.Context, workspace string) {
	if a.g.cfg.Watcher == nil || !store.IsAgentBox(a.id) || a.stopWatch != nil {
		return
	}
	// The watch outlives the wake that starts it, and isn't part of its trace.
	ctx = trace.ContextWithSpanContext(context.WithoutCancel(ctx), trace.SpanContext{})
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	id := store.EnvOfBox(a.id)
	go func() {
		defer close(done)
		err := a.g.cfg.Watcher.Watch(ctx, workspace, a.id, func(ctx context.Context, paths []string) { a.g.touch(ctx, id, paths) })
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
// watchers (dev servers) hear of them, if the box is awake: an asleep one
// runs no dev server, and its next wake reads the files as they are.
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
