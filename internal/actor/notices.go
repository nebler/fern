package actor

import (
	"context"
	"fmt"
	"time"

	"github.com/nebler/fern/internal/setup"
	"github.com/nebler/fern/internal/store"
	"github.com/nebler/fern/internal/worktree"
)

// noticeError is how much of a failure's text a notice carries: Claude reads
// it at every start, so the whole of a setup script's output is too much.
const (
	noticeErrorHead = 400
	noticeErrorTail = 1200
)

// noticeStore is the disk as notices read it: the pool's room, env's disk
// limit and its repository's last failed golden build.
type noticeStore interface {
	Disk
	BuildFailure(ctx context.Context, id string) (store.BuildFailure, bool, error)
}

// snapshotFailures reads an environment's last failed snapshot
// (SnapshotFailures, which Snapshot writes).
type snapshotFailures interface {
	SnapshotFailure(ctx context.Context, id string) (store.SnapshotFailure, bool, error)
}

// reporter works out the notices of a box with Claude (notices), at its
// wake. It is a part of the Group with what it reads alone: the disk only
// through noticeStore and snapshotFailures, and the boxes' crashes only
// through crashing (Group.crashing), which asks Run and the actors.
type reporter struct {
	store    noticeStore
	failures snapshotFailures // nil: none is kept, and none is told of
	image    string           // the box image's ID, part of every golden key
	crashing func(ctx context.Context, box string, wait time.Duration) ([]Crashing, error)
}

// notices returns what has gone wrong on the node that the person working
// in environment env should hear of, a sentence or two each: its Claude is
// told at its box's start (claudeVars, fern-init's hint) and passes
// it on. The thread is where the person looks; the daemon's log is not.
// Something that can't be read is left out: a notice never holds up a wake.
//
//   - New snapshots are refused, because the pool or the disk under it is
//     full: the thread's turns leave nothing to revert to.
//   - The environment has used 90% of its disk limit or more (limitNotice):
//     its writes fail at the limit, and only the person can make room.
//   - The environment's own box, where Claude's Bash runs, died or failed to
//     wake so often that it isn't woken for now (crashNotice): Claude would
//     otherwise learn it from its first Bash call, and the person from
//     nothing.
//   - The environment's last snapshot failed (Snapshot): T3 went on with
//     the turn, which has no point to revert to. The record says when; T3's
//     name for the checkpoint is a digest, which tells a person nothing.
//   - The golden build of what this workspace was made from failed, so it
//     started without what the setup script and the services' first start
//     make.
func (r *reporter) notices(ctx context.Context, env store.Env) []string {
	var out []string
	if sp, err := r.store.Space(); err == nil {
		if full := sp.Full(); full != nil {
			out = append(out, "Fern takes no new snapshots, so a revert can't bring back what this turn changes: "+full.Error()+".")
		}
	}
	// Only an environment with a quota group has a limit: without limits,
	// nothing is read.
	if env.Group != 0 {
		if q, err := r.store.Quota(ctx, env); err == nil && q.Near() {
			out = append(out, limitNotice(env.ID, q))
		}
	}
	if notice := r.crashNotice(ctx, env); notice != "" {
		out = append(out, notice)
	}
	if keep := r.failures; keep != nil {
		if failed, ok, err := keep.SnapshotFailure(ctx, env.ID); err == nil && ok {
			out = append(out, snapshotNotice(failed, time.Now()))
		}
	}
	// The repository of a worktree, as golden finds it.
	builder := store.BuilderID(worktree.Repo(env.Workspace))
	failed, ok, err := r.store.BuildFailure(ctx, builder)
	if err != nil || !ok {
		return out
	}
	// Only now, with a failure to compare it to, is the key worked out: it
	// reads the lockfiles. A failed build of other files isn't this one's.
	if key, err := setup.Key(env.Workspace, r.image); err != nil || key != failed.Key {
		return out
	}
	return append(out, fmt.Sprintf("Fern's build of this repository's starting point failed (%s, %s ago), so this thread began "+
		"without what the setup script and the services' first start make. It is tried again when a thread is "+
		"made, at most every %s. Why it failed: %s",
		failed.At.UTC().Format("2006-01-02 15:04 UTC"), time.Since(failed.At).Round(time.Minute), buildRetry, clip(failed.Error)))
}

// crashNotice is what the Claude of environment env's thread is told when
// the environment's own box, where its Bash and the services run, isn't
// woken for now; "" when it is, or when its actor is busy and can't say: a
// notice is asked for during a wake, which never waits for another actor.
func (r *reporter) crashNotice(ctx context.Context, env store.Env) string {
	boxes, err := r.crashing(ctx, env.ID, 0)
	if err != nil || len(boxes) == 0 {
		return ""
	}
	c := boxes[0]
	return fmt.Sprintf("Fern has stopped starting this thread's box, where Bash and the services run: it died or failed to start "+
		"%d times since %s. Bash fails until %s, when the next command tries again; on the server, `fern wake %s` tries at once "+
		"and `fern logs %s` shows what the box last printed. Why it failed the last time: %s",
		c.Times, c.Since.UTC().Format("15:04 UTC"), c.Retry.UTC().Format("15:04 UTC"), c.Box, env.ID, clip(c.Cause))
}

// limitNotice says how much of its disk limit environment env has used, and
// what frees room: deleting files frees only what no snapshot holds, and
// the snapshots of the last turns hold what they wrote, so it is the
// person's call.
func limitNotice(env string, q store.Quota) string {
	gib := func(b uint64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }
	state := fmt.Sprintf("This thread has used %s of its disk limit of %s on Fern's server; at the limit, writing files, git and builds fail with \"Disk quota exceeded\".", gib(q.Used), gib(q.Limit))
	if q.Full() {
		state = fmt.Sprintf("This thread has reached its disk limit on Fern's server, %s of %s: writing files, git and builds fail with \"Disk quota exceeded\". "+
			"Snapshots and reverts still work.", gib(q.Used), gib(q.Limit))
	}
	return state + " Deleting files frees little: the snapshots Fern keeps of the last 50 turns still hold what those turns wrote. " +
		"Ask the person to raise the limit (`fern limit " + env + " 20G` on the server) or to delete old snapshots (`fern rmsnap`)."
}

// snapshotNotice says, at now, which turn has no snapshot and what that
// means: the one that ended when the snapshot failed. The record is of the
// last failure only, so it can't say whether the turns before it, back to
// the last snapshot that worked, failed too.
func snapshotNotice(failed store.SnapshotFailure, now time.Time) string {
	when := "at a time the record doesn't say"
	if !failed.At.IsZero() {
		when = fmt.Sprintf("at %s, %s ago", failed.At.UTC().Format("2006-01-02 15:04 UTC"), now.Sub(failed.At).Round(time.Minute))
	}
	return fmt.Sprintf("Fern could not take its snapshot of the turn that ended %s (checkpoint %s): that turn has no point to revert to, "+
		"and a revert to it is refused rather than bring back the code without the data. Only the last failure is kept: turns just before "+
		"it may lack theirs too. The next snapshot that works ends this notice. Why it failed: %s", when, failed.Snapshot, clip(failed.Error))
}

// clip returns s with its middle left out if it is long: a failure's cause
// is at its start, and a script's last output at its end.
func clip(s string) string {
	if len(s) <= noticeErrorHead+noticeErrorTail {
		return s
	}
	return s[:noticeErrorHead] + " […] " + s[len(s)-noticeErrorTail:]
}
