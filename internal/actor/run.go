package actor

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/nebler/fern/internal/store"
)

// sweepEvery is how often Run looks for environments whose worktree T3
// removed. One that is still gone at the next look is deleted: T3 removes a
// deleted thread's worktree, and makes it again only for a thread that
// resumes, which comes with a checkpoint that adopts it back.
const sweepEvery = time.Hour

type findRequest struct {
	id    string
	reply chan *actor
}

// Run runs the actors until ctx is done, then waits for each to finish the
// change it is making. It first gives every box that is running an actor, so
// that boxes left awake by an earlier daemon go to sleep when idle; if it
// can't learn which, it returns the error.
func (g *Group) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	defer close(g.stopped)
	actors := make(map[string]*actor)
	// An actor ends when its box sleeps; the next one goes on counting
	// the box's crashes.
	crashes := make(map[string][]time.Time)
	causes := make(map[string]string)
	exited := make(chan *actor)
	start := func(id string, awake bool) *actor {
		a := &actor{
			id: id, g: g, mailbox: make(chan message), done: make(chan struct{}),
			crashes: slices.Clone(crashes[id]), crashCause: causes[id],
		}
		actors[id] = a
		wg.Go(func() {
			a.run(ctx, awake)
			close(a.done)
			select {
			case exited <- a:
			case <-ctx.Done():
			}
		})
		return a
	}
	// retire takes an actor whose goroutine has ended (a.done is closed) off
	// the books, once, and keeps its count of crashes for the next.
	retire := func(a *actor) {
		if actors[a.id] != a {
			return
		}
		delete(actors, a.id)
		if cs := recent(a.crashes); len(cs) > 0 {
			crashes[a.id], causes[a.id] = cs, a.crashCause
		} else {
			delete(crashes, a.id)
			delete(causes, a.id)
		}
	}
	states, err := g.driver.States(ctx)
	if err != nil {
		return fmt.Errorf("box states: %w", err)
	}
	wg.Go(func() { g.sweep(ctx) })
	wg.Go(func() { g.builder.buildGoldens(ctx) })
	for range prewakers {
		wg.Go(func() { g.prewaker.prewakeBoxes(ctx) })
	}
	for id := range states {
		start(id, true)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case req := <-g.find:
			a, ok := actors[req.id]
			if ok && isClosed(a.done) {
				// It has ended, and its word of that is still on the way.
				retire(a)
				ok = false
			}
			if !ok {
				a = start(req.id, false)
			}
			req.reply <- a
		case a := <-exited:
			retire(a)
		case req := <-g.crashed:
			req.reply <- crashesKnown(req.box, actors, crashes, causes, retire)
		}
	}
}

// sweep deletes, every sweepEvery, the environments whose worktree was gone
// at the last look too, until ctx is done.
func (g *Group) sweep(ctx context.Context) {
	ticker := time.NewTicker(sweepEvery)
	defer ticker.Stop()
	var gone map[string]bool
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		ids, err := g.store.Orphans(ctx)
		if err != nil {
			g.log.WarnContext(ctx, "looking for removed worktrees failed", slog.Any("error", err))
			continue
		}
		now := make(map[string]bool, len(ids))
		for _, id := range ids {
			now[id] = true
			if !gone[id] {
				continue
			}
			if err := g.Delete(ctx, id); err != nil {
				g.log.WarnContext(ctx, "deleting an environment whose worktree is gone failed", slog.String("env", id), slog.Any("error", err))
				continue
			}
			g.log.InfoContext(ctx, "deleted an environment whose worktree is gone", slog.String("env", id))
		}
		gone = now
	}
}

// actorOf returns environment id's actor, starting one if it has none.
func (g *Group) actorOf(ctx context.Context, id string) (*actor, error) {
	if err := store.CheckEnvID(store.EnvOfBox(id)); err != nil {
		return nil, err
	}
	req := findRequest{id: id, reply: make(chan *actor, 1)}
	select {
	case g.find <- req:
		return <-req.reply, nil
	case <-g.stopped:
		return nil, ErrStopped
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func isClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}
