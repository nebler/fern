package api

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// Env is an environment as the API reports it.
type Env struct {
	ID        string    `json:"id"`
	Workspace string    `json:"workspace"` // on the host and in the box
	Created   time.Time `json:"created"`
	State     string    `json:"state,omitempty"` // not in a lookup by path
}

// Snapshot is a snapshot as the API reports it.
type Snapshot struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
}

// Holds is what holds one box awake, as GET /envs/{id}/holds reports it for
// an environment's box and its agent box: what runs in it, and when it
// sleeps and by which rule.
type Holds struct {
	Box     string    `json:"box"`
	Awake   bool      `json:"awake"`
	Running []Running `json:"running,omitempty"` // oldest first
	SleepAt time.Time `json:"sleep_at,omitzero"` // absent: it doesn't sleep at a time, for Why
	Why     string    `json:"why"`               // the rule that decides, in words
}

// Running is a command, a preview request or a job counted as running in a
// box.
type Running struct {
	Kind  string    `json:"kind"` // exec, preview or job
	N     int       `json:"n"`    // its number in the daemon's log
	Since time.Time `json:"since"`
}

// Disk is what uses the node's disk, as GET /disk reports it (fern df).
type Disk struct {
	Pool Volume `json:"pool"`
	// Under is the server's disk, when the pool is a file on it: the pool
	// can only grow into what that disk has free.
	Under *Volume `json:"under,omitempty"`
	// Least is the free space below which the daemon refuses new snapshots
	// and environments, on the pool and on the disk under it.
	Least uint64     `json:"least"`
	Parts []DiskPart `json:"parts"`
}

// Volume is a filesystem's room, in bytes.
type Volume struct {
	Path  string `json:"path"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

// DiskPart is one thing that uses disk space.
type DiskPart struct {
	Kind string `json:"kind"` // environment, golden, image, data or podman
	Name string `json:"name"`
	// Total counts blocks it shares with others (a workspace with its
	// snapshots, a thread with its golden); Exclusive only what nothing
	// else holds, the least that removing it frees.
	Total     uint64 `json:"total"`
	Exclusive uint64 `json:"exclusive"`
	Unused    bool   `json:"unused,omitempty"` // nothing needs it: fern gc removes it
	Note      string `json:"note,omitempty"`
}

// Freed answers POST /gc (fern gc): what was removed, what couldn't be, and
// the room there is now. Blocks freed in a pool that is a file come back to
// the disk under it a little later.
type Freed struct {
	Removed []string `json:"removed,omitempty"`
	Failed  []string `json:"failed,omitempty"`
	Pool    Volume   `json:"pool"`
	Under   *Volume  `json:"under,omitempty"`
}

// Problem is something wrong on the node now that the daemon knows of
// (GET /problems, fern doctor): a failed golden build, backups that fail, a
// pool that takes no more snapshots, a box that keeps crashing.
type Problem struct {
	Name string `json:"name"` // the part: "golden build", "backups", "disk", "memory", "box"
	// About is which one, of a part the node can have several of: a builder,
	// a box. With Name it names the problem for as long as it lasts
	// (health.Notifier), which its Detail doesn't.
	About  string    `json:"about,omitempty"`
	At     time.Time `json:"at,omitzero"`   // when it went wrong, if that is known
	Detail string    `json:"detail"`        // what is wrong, and what it means
	Fix    string    `json:"fix,omitempty"` // what to do
}

// SnapshotRequest is the body of POST /envs/{id}/snapshots.
type SnapshotRequest struct {
	ID string `json:"id"`
	// Replace takes the snapshot again if one with this ID exists. T3 reuses
	// checkpoint names after a revert.
	Replace bool `json:"replace,omitempty"`
}

// ForkRequest is the body of POST /envs/{id}/fork.
type ForkRequest struct {
	// Snapshot is the snapshot to fork; empty forks the workspace as it is.
	Snapshot string `json:"snapshot,omitempty"`
	Count    int    `json:"count"`
}

// ForkResponse answers POST /envs/{id}/fork.
type ForkResponse struct {
	Forks []string `json:"forks"` // the new environments' IDs
}

// RestoreRequest is the body of POST /envs/{id}/restore.
type RestoreRequest struct {
	Snapshot string `json:"snapshot"`
}

// Error is an error the daemon answered with. Its status says what kind:
// http.StatusNotFound, http.StatusBadRequest, http.StatusServiceUnavailable
// (the daemon is stopping), or a failure.
type Error struct {
	Status  int    `json:"-"`
	Message string `json:"error"`
}

func (e *Error) Error() string { return e.Message }

// readError returns the error a failed answer's body describes.
func readError(status int, body io.Reader) error {
	e := &Error{Status: status}
	if err := json.NewDecoder(io.LimitReader(body, 1<<16)).Decode(e); err != nil || e.Message == "" {
		e.Message = http.StatusText(status)
	}
	return e
}
