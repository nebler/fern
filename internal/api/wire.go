package api

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/nebler/fern/internal/driver"
	"github.com/nebler/fern/internal/store"
)

// Env is an environment as the API reports it.
type Env struct {
	ID        string       `json:"id"`
	Workspace string       `json:"workspace"` // on the host and in the box
	Created   time.Time    `json:"created"`
	State     driver.State `json:"state,omitempty"` // not in a lookup by path
}

func toAPIEnv(env store.Env, state driver.State) Env {
	return Env{ID: env.ID, Workspace: env.Workspace, Created: env.Created, State: state}
}

// Snapshot is a snapshot as the API reports it.
type Snapshot struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
}

// snapshotRequest is the body of POST /envs/{id}/snapshots.
type snapshotRequest struct {
	ID string `json:"id"`
	// Replace takes the snapshot again if one with this ID exists. T3 reuses
	// checkpoint names after a revert.
	Replace bool `json:"replace,omitempty"`
}

// restoreRequest is the body of POST /envs/{id}/restore.
type restoreRequest struct {
	Snapshot string `json:"snapshot"`
}

// Error is an error the daemon answered with. Its status says what kind:
// http.StatusNotFound, http.StatusBadRequest, or a failure.
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
