// Package health says what is wrong on the node now: the failures the
// daemon knows of that would otherwise be a line in its log. `fern doctor`
// lists them (GET /problems), and a metric counts them.
package health

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nebler/fern/internal/api"
	"github.com/nebler/fern/internal/backup"
	"github.com/nebler/fern/internal/store"
)

// Store is what a Checker reads of the pool.
type Store interface {
	List(ctx context.Context) ([]store.Env, error)
	BuildFailure(ctx context.Context, id string) (store.BuildFailure, bool, error)
	Space() (store.Space, error)
}

// Checker finds the node's problems.
type Checker struct {
	Store Store
	Pool  string // the pool, where the last backup's result is kept
	// BackupEvery is how often the daemon backs up; 0 if no backups are set
	// up, which is no problem.
	BackupEvery time.Duration
}

// lateBackups is how many backups may be missed before it is a problem:
// one that runs long, or a restart, misses one.
const lateBackups = 3

// Problems returns what is wrong now, the most pressing first: a pool that
// takes no more snapshots, backups that fail or have stopped, and golden
// builds that failed. No problems is an empty list.
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
	if c.BackupEvery > 0 {
		if p, ok := c.backups(); ok {
			out = append(out, p)
		}
	}
	envs, err := c.Store.List(ctx)
	if err != nil {
		return nil, err
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
			Name: "golden build", At: failed.At,
			Detail: fmt.Sprintf("%s: its threads start without what the setup script and the services' first start make: %s", env.ID, firstLine(failed.Error)),
			Fix:    "mend the repository's setup script or services.yaml; the next new thread builds again. The whole failure: envs/" + env.ID + "/build-failed.json in the pool",
		})
	}
	return out, nil
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
