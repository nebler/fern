// Package api is the daemon's HTTP API, served on a unix socket, and a
// client for it.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/driver"
	"github.com/nebler/fern/internal/store"
)

// Store reads environments and snapshots on disk.
type Store interface {
	FindByPath(ctx context.Context, path string) (store.Env, error)
	List(ctx context.Context) ([]store.Env, error)
	Snapshots(ctx context.Context, id string) ([]store.Snapshot, error)
}

// Driver reports the state of environments' boxes.
type Driver interface {
	States(ctx context.Context) (map[string]driver.State, error)
}

// dialTimeout bounds connecting to a box that is up; a box that vanished can
// leave an address nothing answers.
const dialTimeout = 10 * time.Second

// errBadBody means a request's body isn't the JSON its route takes.
var errBadBody = errors.New("bad request body")

// Server handles API requests. Reads go to the store and the driver; every
// change, wake and exec goes through the environment's actor.
type Server struct {
	store  Store
	driver Driver
	envs   *actor.Group
	log    *slog.Logger
}

// NewServer returns a Server that reads from st and drv and changes
// environments through envs.
func NewServer(st Store, drv Driver, envs *actor.Group, log *slog.Logger) *Server {
	return &Server{store: st, driver: drv, envs: envs, log: log}
}

// Handler returns the API's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /envs", s.handle(s.list))
	mux.HandleFunc("PUT /envs/{id}", s.handle(s.create))
	mux.HandleFunc("DELETE /envs/{id}", s.handle(s.delete))
	mux.HandleFunc("POST /envs/{id}/snapshots", s.handle(s.snapshot))
	mux.HandleFunc("GET /envs/{id}/snapshots", s.handle(s.snapshots))
	mux.HandleFunc("DELETE /envs/{id}/snapshots/{snap}", s.handle(s.deleteSnapshot))
	mux.HandleFunc("POST /envs/{id}/restore", s.handle(s.restore))
	mux.HandleFunc("POST /envs/{id}/sleep", s.handle(s.sleep))
	mux.HandleFunc("POST /envs/{id}/wake", s.handle(s.wake))
	// WebSocket handshakes are GET requests.
	mux.HandleFunc("GET /envs/{id}/exec", s.exec)
	return mux
}

// handle serves a request with h and answers with what it returns: an error,
// nothing (204), or a value as JSON.
func (s *Server) handle(h func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := h(r)
		switch {
		case err != nil:
			s.fail(w, r, err)
		case v == nil:
			w.WriteHeader(http.StatusNoContent)
		default:
			writeJSON(w, http.StatusOK, v)
		}
	}
}

// list returns all environments, or with ?path= the one whose workspace
// holds that path (none if there is none).
//
// fern-claude looks its environment up by path before every Claude start,
// so that answer comes from the index alone, without the box's state: a
// state costs a Kubernetes call.
func (s *Server) list(r *http.Request) (any, error) {
	if path := r.URL.Query().Get("path"); path != "" {
		env, err := s.store.FindByPath(r.Context(), path)
		if errors.Is(err, store.ErrNotFound) {
			return []Env{}, nil
		}
		if err != nil {
			return nil, err
		}
		return []Env{toAPIEnv(env, "")}, nil
	}
	envs, err := s.store.List(r.Context())
	if err != nil {
		return nil, err
	}
	// One Kubernetes call for all of them, not one each.
	states, err := s.driver.States(r.Context())
	if err != nil {
		s.log.WarnContext(r.Context(), "box states", slog.Any("error", err))
	}
	out := make([]Env, 0, len(envs))
	for _, env := range envs {
		state, ok := states[env.ID]
		switch {
		case err != nil:
			state = driver.Unknown
		case !ok:
			state = driver.Asleep // never woken: no box yet
		}
		out = append(out, toAPIEnv(env, state))
	}
	return out, nil
}

// create makes an environment, filled from a tar body if there is one. Its
// box comes with its first wake.
func (s *Server) create(r *http.Request) (any, error) {
	opts := store.CreateOptions{Workspace: r.URL.Query().Get("path")}
	if r.Header.Get("Content-Type") == "application/x-tar" {
		opts.Seed = r.Body
	}
	return nil, s.envs.Create(r.Context(), r.PathValue("id"), opts)
}

func (s *Server) delete(r *http.Request) (any, error) {
	return nil, s.envs.Delete(r.Context(), r.PathValue("id"))
}

func (s *Server) snapshot(r *http.Request) (any, error) {
	var req snapshotRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	return nil, s.envs.Snapshot(r.Context(), r.PathValue("id"), req.ID, req.Replace)
}

func (s *Server) snapshots(r *http.Request) (any, error) {
	snaps, err := s.store.Snapshots(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(snaps))
	for _, snap := range snaps {
		out = append(out, Snapshot{ID: snap.ID, Created: snap.Created})
	}
	return out, nil
}

func (s *Server) deleteSnapshot(r *http.Request) (any, error) {
	return nil, s.envs.DeleteSnapshot(r.Context(), r.PathValue("id"), r.PathValue("snap"))
}

func (s *Server) restore(r *http.Request) (any, error) {
	var req restoreRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	return nil, s.envs.Restore(r.Context(), r.PathValue("id"), req.Snapshot)
}

func (s *Server) sleep(r *http.Request) (any, error) {
	return nil, s.envs.Sleep(r.Context(), r.PathValue("id"))
}

func (s *Server) wake(r *http.Request) (any, error) {
	return nil, s.envs.Wake(r.Context(), r.PathValue("id"))
}

// exec runs a command in the environment's box, waking it first if needed,
// by relaying the client's WebSocket to the box's. It wakes and dials the
// box before taking the client's WebSocket, so that a failure is still an
// HTTP error with a body Client.Exec can read. A restore, sleep or delete
// of the environment ends it.
func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	box, ex, err := s.dialBox(ctx, r.PathValue("id"), cancel)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer ex.Close()
	defer func() { _ = box.CloseNow() }()
	client, err := boxexec.Accept(w, r)
	if err != nil {
		return // Accept has answered the client.
	}
	defer func() { _ = client.CloseNow() }()
	boxexec.Proxy(ctx, client, box)
}

// dialBox connects to environment id's box, waking it if needed. A box seen
// earlier may be gone (a crash, a deleted pod): then it is woken once more.
func (s *Server) dialBox(ctx context.Context, id string, cancel context.CancelFunc) (*websocket.Conn, *actor.Exec, error) {
	var stale string
	var err error
	for range 2 {
		var ex *actor.Exec
		if ex, err = s.envs.Exec(ctx, id, stale, cancel); err != nil {
			return nil, nil, err
		}
		dialCtx, cancelDial := context.WithTimeout(ctx, dialTimeout)
		conn, dialErr := boxexec.Dial(dialCtx, boxexec.URL(ex.Addr), ex.Token, nil)
		cancelDial()
		if dialErr == nil {
			return conn, ex, nil
		}
		ex.Close()
		stale, err = ex.Addr, dialErr
	}
	return nil, nil, fmt.Errorf("connect to box %s: %w", id, err)
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("%w: %w", errBadBody, err)
	}
	return nil
}

// fail answers with err, and logs failures that aren't the caller's fault.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrInvalidID), errors.Is(err, store.ErrInvalidPath), errors.Is(err, errBadBody):
		status = http.StatusBadRequest
	case errors.Is(err, actor.ErrStopped):
		status = http.StatusServiceUnavailable
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		// The client gave up waiting; there is no one to answer.
	default:
		s.log.ErrorContext(r.Context(), "request failed",
			slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Any("error", err))
	}
	writeJSON(w, status, Error{Message: err.Error()})
}

// writeJSON answers with v. A client that went away gets nothing more.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
