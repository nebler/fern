package actor

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/nebler/fern/internal/store"
)

// The kinds of thing counted as running in a box.
const (
	kindExec    = "exec"    // a command: the box stays awake while it runs
	kindPreview = "preview" // a request to a preview: the same
	kindJob     = "job"     // a background command: the idle times still apply
)

// running is one thing counted as running in a box.
type running struct {
	kind   string
	cancel context.CancelFunc // a stop of the box calls it
	since  time.Time
}

// counts returns how many commands (preview requests among them) and how
// many jobs run in the box.
func (a *actor) counts() (commands, jobs int) {
	for _, r := range a.running {
		if r.kind == kindJob {
			jobs++
		} else {
			commands++
		}
	}
	return commands, jobs
}

// Holds is what holds a box awake, as its actor knows it: what runs in it,
// and when it sleeps and by which rule.
type Holds struct {
	Box     string
	Awake   bool      // not known to be asleep
	Running []Running // oldest first
	SleepAt time.Time // zero: it doesn't sleep at a time, for Why
	Why     string    // the rule that decides, in words
}

// Running is a command, a preview request or a job counted as running.
type Running struct {
	Kind  string // exec, preview or job
	N     int    // its number in the daemon's log
	Since time.Time
}

// Holds reports what holds box id awake, without waking it: an
// environment's own box, or its agent box.
func (g *Group) Holds(ctx context.Context, id string) (Holds, error) {
	if _, err := g.store.Get(ctx, store.EnvOfBox(id)); err != nil {
		return Holds{}, err
	}
	h := Holds{Box: id}
	err := g.do(ctx, "holds", id, func(_ context.Context, a *actor) error {
		at, why := a.sleepAt()
		h.Awake, h.Why = a.phase != boxAsleep, why.words(g.cfg.Idle)
		if why.sleeps() {
			h.SleepAt = at
		}
		for n, r := range a.running {
			h.Running = append(h.Running, Running{Kind: r.kind, N: n, Since: r.since})
		}
		slices.SortFunc(h.Running, func(x, y Running) int { return cmp.Compare(x.N, y.N) })
		return nil
	})
	return h, err
}
