// Package health says what is wrong on the node now: the failures the
// daemon knows of that would otherwise be a line in its log, or the error
// of the next command. `fern doctor` lists them (GET /problems), a metric
// counts them, and a Notifier tells a person who isn't looking.
package health

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/api"
	"github.com/nebler/fern/internal/backup"
	"github.com/nebler/fern/internal/store"
)

// Store is what a Checker reads of the pool.
type Store interface {
	List(ctx context.Context) ([]store.Env, error)
	BuildFailure(ctx context.Context, id string) (store.BuildFailure, bool, error)
	SnapshotFailure(ctx context.Context, id string) (store.SnapshotFailure, bool, error)
	Space() (store.Space, error)
}

// Boxes says which boxes aren't woken for now because they keep dying or
// failing to wake (actor.Group).
type Boxes interface {
	Crashing(ctx context.Context) ([]actor.Crashing, error)
}

// Checker finds the node's problems.
type Checker struct {
	Store Store
	Pool  string // the pool, where the last backup's result is kept
	// Boxes knows the boxes that keep crashing. May be nil: none are listed.
	Boxes Boxes
	// BackupEvery is how often the daemon backs up; 0 if no backups are set
	// up, which is no problem.
	BackupEvery time.Duration
	// Records reads btrfs's room for its own records, which the pool's free
	// space doesn't show. May be nil: it isn't looked at.
	Records *Records
	// Memory knows what the boxes use together. May be nil: it isn't looked
	// at.
	Memory *Memory
}

// lateBackups is how many backups may be missed before it is a problem:
// one that runs long, or a restart, misses one.
const lateBackups = 3

// Problems returns what is wrong now, the most pressing first: a pool that
// takes no more snapshots, one with no room left for btrfs's records,
// boxes that have nearly used up their memory or were killed for it,
// backups that fail or have stopped, boxes that aren't woken because they
// keep crashing, environments whose last snapshot failed, and golden builds
// that failed. No problems is an empty list.
func (c Checker) Problems(ctx context.Context) ([]api.Problem, error) {
	out := []api.Problem{} // an empty list on the wire, not null
	sp, err := c.Store.Space()
	if err != nil {
		return nil, err
	}
	if full := sp.Full(); full != nil {
		out = append(out, api.Problem{
			Name: "disk", Detail: "no new snapshots or threads: " + full.Error(),
			Fix: "`fern df` shows what uses the disk, `fern gc` frees what nothing needs; delete threads that are done",
		})
	}
	if c.Records != nil {
		if p, ok := c.Records.problem(ctx, sp.Under != nil); ok {
			out = append(out, p)
		}
	}
	if c.Memory != nil {
		out = append(out, c.Memory.problems()...)
	}
	if c.BackupEvery > 0 {
		if p, ok := c.backups(); ok {
			out = append(out, p)
		}
	}
	if c.Boxes != nil {
		boxes, err := c.Boxes.Crashing(ctx)
		if err != nil {
			return nil, err
		}
		for _, box := range boxes {
			out = append(out, crashing(box))
		}
	}
	envs, err := c.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, env := range envs {
		failed, ok, err := c.Store.SnapshotFailure(ctx, env.ID)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, snapshot(env.ID, failed))
		}
	}
	for _, env := range envs {
		if _, source := store.BuilderOf(env.ID); !store.IsBuilder(env.ID) || source {
			continue
		}
		failed, ok, err := c.Store.BuildFailure(ctx, env.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, api.Problem{
			Name: "golden build", About: env.ID, At: failed.At,
			Detail: fmt.Sprintf("%s: its threads start without what the setup script and the services' first start make: %s", env.ID, firstLine(failed.Error)),
			Fix:    "mend the repository's setup script or services.yaml; the next new thread builds again. The whole failure: envs/" + env.ID + "/build-failed.json in the pool",
		})
	}
	return out, nil
}

// snapshot is the problem of environment env whose last snapshot failed:
// T3 went on with the turn, which has no point to revert to. It lasts until
// a snapshot of env works. A snapshot the pool was too full for is not
// among them (actor.Group.Snapshot): that is the problem "disk".
func snapshot(env string, failed store.SnapshotFailure) api.Problem {
	return api.Problem{
		Name: "snapshot", About: env, At: failed.At,
		Detail: fmt.Sprintf("%s: the snapshot of a turn wasn't taken (%s), so a revert to that turn is refused: %s", env, failed.Snapshot, firstLine(failed.Error)),
		Fix: "the next turn's snapshot is tried as usual, and one that works clears this; `fern snapshot " + env + " probe` tries one now (`fern rmsnap " +
			env + " probe` removes it again); `fern logs " + env + "` has what the daemon said",
	}
}

// crashing is the problem of a box that isn't woken for now: which, how
// often it failed, when it is tried again and why it failed the last time.
// It lasts until then; a box that fails again at that try is back.
func crashing(box actor.Crashing) api.Problem {
	const clock = "15:04 UTC"
	env := store.EnvOfBox(box.Box)
	return api.Problem{
		Name: "box", About: box.Box, At: box.Last,
		Detail: fmt.Sprintf("%s keeps crashing: it died or failed to wake %d times since %s, and its commands fail until %s, when the next one tries again. The last time: %s",
			box.Box, box.Times, box.Since.UTC().Format(clock), box.Retry.UTC().Format(clock), firstLine(box.Cause)),
		Fix: "`fern logs " + env + "` shows what the box last printed and what the daemon said of it; `fern wake " + box.Box + "` tries again now",
	}
}

// backups returns the problem with the node's backups, if there is one.
func (c Checker) backups() (api.Problem, bool) {
	fix := "journalctl -u fern-daemon | grep backup; `fern backup` tries one now"
	st, ok, err := backup.ReadStatus(c.Pool)
	switch {
	case err != nil:
		return api.Problem{Name: "backups", Detail: "the last backup's result can't be read: " + err.Error(), Fix: fix}, true
	case !ok:
		return api.Problem{}, false // none yet: the first is under way, or due
	case st.Error != "":
		good := "none has worked yet"
		if !st.Good.IsZero() {
			good = "the last that worked: " + st.Good.Format("2006-01-02 15:04 UTC")
		}
		return api.Problem{Name: "backups", At: st.At, Detail: fmt.Sprintf("the last backup failed (%s): %s", good, firstLine(st.Error)), Fix: fix}, true
	case time.Since(st.At) > lateBackups*c.BackupEvery:
		return api.Problem{
			Name: "backups", At: st.At,
			Detail: fmt.Sprintf("no backup since %s, and they are due every %s", st.At.Format("2006-01-02 15:04 UTC"), c.BackupEvery), Fix: fix,
		}, true
	}
	return api.Problem{}, false
}

// firstLine returns the start of s, at most a line and 300 bytes of it.
func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
