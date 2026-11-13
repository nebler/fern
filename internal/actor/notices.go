package actor

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/nebler/fern/internal/setup"
	"github.com/nebler/fern/internal/store"
)

// noticeError is how much of a failure's text a notice carries: Claude reads
// it at every start, so the whole of a setup script's output is too much.
const (
	noticeErrorHead = 400
	noticeErrorTail = 1200
)

// notices returns what has gone wrong on the node that the person working
// in environment env should hear of, a sentence or two each: its Claude is
// told at its box's start (claudeVars, fern-init's hint) and passes
// it on. The thread is where the person looks; the daemon's log is not.
// Something that can't be read is left out: a notice never holds up a wake.
//
//   - New snapshots are refused, because the pool or the disk under it is
//     full: the thread's turns leave nothing to revert to.
//   - The golden build of what this workspace was made from failed, so it
//     started without what the setup script and the services' first start
//     make.
func (g *Group) notices(ctx context.Context, env store.Env) []string {
	var out []string
	if sp, err := g.store.Space(); err == nil {
		if full := sp.Full(); full != nil {
			out = append(out, "Fern takes no new snapshots, so a revert can't bring back what this turn changes: "+full.Error()+".")
		}
	}
	// The repository of a worktree is its parent folder (golden).
	builder := store.BuilderID(filepath.Base(filepath.Dir(env.Workspace)))
	failed, ok, err := g.store.BuildFailure(ctx, builder)
	if err != nil || !ok {
		return out
	}
	// Only now, with a failure to compare it to, is the key worked out: it
	// reads the lockfiles. A failed build of other files isn't this one's.
	if key, err := setup.Key(env.Workspace, g.cfg.Image); err != nil || key != failed.Key {
		return out
	}
	return append(out, fmt.Sprintf("Fern's build of this repository's starting point failed (%s, %s ago), so this thread began "+
		"without what the setup script and the services' first start make. It is tried again when a thread is "+
		"made, at most every %s. Why it failed: %s",
		failed.At.UTC().Format("2006-01-02 15:04 UTC"), time.Since(failed.At).Round(time.Minute), buildRetry, clip(failed.Error)))
}

// clip returns s with its middle left out if it is long: a failure's cause
// is at its start, and a script's last output at its end.
func clip(s string) string {
	if len(s) <= noticeErrorHead+noticeErrorTail {
		return s
	}
	return s[:noticeErrorHead] + " […] " + s[len(s)-noticeErrorTail:]
}
