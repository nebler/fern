package actor

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/nebler/fern/internal/store"
)

// ErrCrashing means a box died or failed to wake crashLimit times within
// crashWindow: it isn't woken again until the oldest of those is older,
// or a person wakes it (Wake).
var ErrCrashing = errors.New("the box keeps crashing")

// A box that dies under its commands, or fails to wake, is woken again at
// the next command, up to crashLimit times in crashWindow; then commands
// fail at once with ErrCrashing, rather than each paying for a wake that
// ends the same way.
const (
	crashLimit  = 3
	crashWindow = 5 * time.Minute
)

// recent returns the times in ts within crashWindow of now.
func recent(ts []time.Time) []time.Time {
	return slices.DeleteFunc(ts, func(t time.Time) bool { return time.Since(t) >= crashWindow })
}

// Crashing is a box that isn't woken for now, because it died under its
// commands or failed to wake crashLimit times within crashWindow: every
// command for it fails at once with ErrCrashing until Retry.
type Crashing struct {
	Box   string    // the environment's ID, or its agent box's
	Times int       // how often it died or failed to wake since Since
	Since time.Time // the first of those times
	Last  time.Time // the last
	Cause string    // why, the last time
	Retry time.Time // from when a command wakes it again
}

// crashingOf says whether box id, which died or failed to wake at times (all
// within crashWindow, oldest first), the last time because of cause, is not
// woken for now, and gives it as a Crashing.
func crashingOf(id string, times []time.Time, cause string) (Crashing, bool) {
	if len(times) < crashLimit {
		return Crashing{}, false
	}
	return Crashing{
		Box: id, Times: len(times), Since: times[0], Last: times[len(times)-1], Cause: cause,
		Retry: times[0].Add(crashWindow),
	}, true
}

// crashAsk is how long Crashing waits for an actor to say how its box is. An
// actor that refuses to wake its box answers at once, as it does every
// command; one that takes longer is in the middle of a change, such as a
// wake that is trying again, and is left out.
const crashAsk = 100 * time.Millisecond

// crashRequest asks Run what is known of boxes' crashes: of box, or of every
// box if box is "".
type crashRequest struct {
	box   string
	reply chan crashReply // buffered, so Run never waits for the caller
}

// crashReply is Run's answer: the actors that run, who alone know how their
// boxes are, and of the boxes whose actor has ended those that aren't woken
// for now, from what the actor left with Run.
type crashReply struct {
	actors []*actor
	ended  []Crashing
}

// crashesKnown answers a crashRequest for box, from Run's goroutine, with
// Run's books: its actors, and the crashes and their causes that ended
// actors left. retire takes an ended actor off those books; one whose word
// of its end is still on the way is retired here, so that each box is in one
// place only.
func crashesKnown(box string, actors map[string]*actor, crashes map[string][]time.Time, causes map[string]string, retire func(*actor)) crashReply {
	var r crashReply
	for id, a := range actors {
		if box != "" && id != box {
			continue
		}
		if isClosed(a.done) {
			retire(a)
			continue
		}
		r.actors = append(r.actors, a)
	}
	for id, times := range crashes {
		if _, runs := actors[id]; runs || (box != "" && id != box) {
			continue // a running actor counts on from these, and knows better
		}
		times = recent(times)
		if len(times) == 0 {
			delete(crashes, id)
			delete(causes, id)
			continue
		}
		crashes[id] = times
		if c, ok := crashingOf(id, times, causes[id]); ok {
			r.ended = append(r.ended, c)
		}
	}
	return r
}

// ask runs fn on a's goroutine and reports whether it did: it gives up on
// an actor that doesn't take it within wait, because it is in the middle of
// a change, or that has ended. With a wait of 0 it doesn't wait at all, and
// only an actor that is waiting for its next message takes it.
func (a *actor) ask(ctx context.Context, wait time.Duration, fn func()) bool {
	ran := make(chan struct{})
	m := message{reply: make(chan error, 1), fn: func(context.Context, *actor) error {
		fn()
		close(ran)
		return nil
	}}
	if wait == 0 {
		select {
		case a.mailbox <- m:
		default:
			return false
		}
	} else {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case a.mailbox <- m:
		case <-a.done:
			return false
		case <-timer.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
	<-ran // an actor finishes what it has taken
	return true
}

// crashing returns, of box (of every box, if box is ""), those that aren't
// woken for now. The count of a box's crashes is its actor's, so each
// running actor is asked through its mailbox, for at most wait (ask), and
// Run answers for the boxes whose actor has ended. It wakes nothing and
// starts no actor.
func (g *Group) crashing(ctx context.Context, box string, wait time.Duration) ([]Crashing, error) {
	req := crashRequest{box: box, reply: make(chan crashReply, 1)}
	select {
	case g.crashed <- req:
	case <-g.stopped:
		return nil, ErrStopped
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	r := <-req.reply
	out := r.ended
	for _, a := range r.actors {
		var (
			c  Crashing
			ok bool
		)
		asked := a.ask(ctx, wait, func() {
			a.crashes = recent(a.crashes)
			c, ok = crashingOf(a.id, a.crashes, a.crashCause)
		})
		if asked && ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// Crashing returns the boxes that aren't woken for now because they keep
// dying or failing to wake, by name: what `fern doctor` lists, where until
// 2026-11-13 it was only the error of the next command. A box whose
// environment has been deleted since is left out.
//
// The counts live in the daemon's memory, as they always have: a restart of
// the daemon forgets them, and every box gets its tries again.
func (g *Group) Crashing(ctx context.Context) ([]Crashing, error) {
	all, err := g.crashing(ctx, "", crashAsk)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, c := range all {
		if _, err := g.store.Get(ctx, store.EnvOfBox(c.Box)); errors.Is(err, store.ErrNotFound) {
			continue
		} else if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(x, y Crashing) int { return cmp.Compare(x.Box, y.Box) })
	return out, nil
}
