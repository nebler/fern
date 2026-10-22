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
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/driver"
	"github.com/nebler/fern/internal/store"
)

// Store keeps environments and snapshots on disk.
type Store interface {
	Create(ctx context.Context, id string, opts store.CreateOptions) (store.Env, error)
	Get(ctx context.Context, id string) (store.Env, error)
	FindByPath(ctx context.Context, path string) (store.Env, error)
	List(ctx context.Context) ([]store.Env, error)
	Delete(ctx context.Context, id string) error
	Snapshot(ctx context.Context, id, snapID string) error
	DeleteSnapshot(ctx context.Context, id, snapID string) error
	Snapshots(ctx context.Context, id string) ([]store.Snapshot, error)
	Restore(ctx context.Context, id, snapID string) error
}

// Driver runs environments' boxes.
type Driver interface {
	Wake(ctx context.Context, env store.Env) (addr string, err error)
	Sleep(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	States(ctx context.Context) (map[string]driver.State, error)
}

const (
	// changeTimeout bounds a change of an environment. Changes don't stop
	// when the client goes away, which would leave them half done.
	changeTimeout = 10 * time.Minute
	// dialTimeout bounds connecting to a box that is up; a box that vanished
	// can leave an address nothing answers.
	dialTimeout = 10 * time.Second
)

// errBadBody means a request's body isn't the JSON its route takes.
var errBadBody = errors.New("bad request body")

// Server handles API requests.
//
// Each environment has one lock, a channel so that waiting for it can be
// cancelled. A change (create, delete, snapshot, delete snapshot, restore,
// sleep) holds it to the end and outlives its request. A wake, or an exec
// finding its box, holds it only while it finds or wakes the box. So a
// running exec never holds up a change, a new exec waits for one, and
// restore and sleep end running execs by stopping the box.
type Server struct {
	store  Store
	driver Driver
	log    *slog.Logger

	mu   sync.Mutex
	envs map[string]*envState

	changes sync.WaitGroup // changes still running, which outlive their requests
}

// envState is what the server keeps in memory about one environment.
type envState struct {
	lock chan struct{} // holds a value while someone changes or wakes it
	addr string        // the box's exec address while it is known to be up; guarded by lock
}

func (e *envState) release() { <-e.lock }

// NewServer returns a Server for the environments in st, run by drv.
func NewServer(st Store, drv Driver, log *slog.Logger) *Server {
	return &Server{store: st, driver: drv, log: log, envs: make(map[string]*envState)}
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

// Wait waits for the changes still running. Call it after the HTTP server
// has shut down and before closing the store.
func (s *Server) Wait() { s.changes.Wait() }

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
	id := r.PathValue("id")
	opts := store.CreateOptions{Workspace: r.URL.Query().Get("path")}
	if r.Header.Get("Content-Type") == "application/x-tar" {
		opts.Seed = r.Body
	}
	return nil, s.change(r.Context(), id, func(ctx context.Context, _ *envState) error {
		_, err := s.store.Create(ctx, id, opts)
		return err
	})
}

// delete stops the box, then removes the disk.
func (s *Server) delete(r *http.Request) (any, error) {
	id := r.PathValue("id")
	return nil, s.change(r.Context(), id, func(ctx context.Context, e *envState) error {
		if _, err := s.store.Get(ctx, id); err != nil {
			return err
		}
		e.addr = ""
		if err := s.driver.Delete(ctx, id); err != nil {
			return err
		}
		return s.store.Delete(ctx, id)
	})
}

func (s *Server) snapshot(r *http.Request) (any, error) {
	var req snapshotRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	id := r.PathValue("id")
	return nil, s.change(r.Context(), id, func(ctx context.Context, _ *envState) error {
		if req.Replace {
			if err := s.store.DeleteSnapshot(ctx, id, req.ID); err != nil {
				return err
			}
		}
		return s.store.Snapshot(ctx, id, req.ID)
	})
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

// deleteSnapshot removes a snapshot; removing one that doesn't exist is not
// an error.
func (s *Server) deleteSnapshot(r *http.Request) (any, error) {
	id, snapID := r.PathValue("id"), r.PathValue("snap")
	return nil, s.change(r.Context(), id, func(ctx context.Context, _ *envState) error {
		return s.store.DeleteSnapshot(ctx, id, snapID)
	})
}

// restore puts the environment to sleep and swaps its disk. It stays asleep;
// the next exec wakes it on the restored disk.
func (s *Server) restore(r *http.Request) (any, error) {
	var req restoreRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	id := r.PathValue("id")
	return nil, s.change(r.Context(), id, func(ctx context.Context, e *envState) error {
		// Check the snapshot before stopping the box: a typo mustn't cost
		// the agent its running processes.
		snaps, err := s.store.Snapshots(ctx, id)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(snaps, func(snap store.Snapshot) bool { return snap.ID == req.Snapshot }) {
			return fmt.Errorf("snapshot %s/%s: %w", id, req.Snapshot, store.ErrNotFound)
		}
		e.addr = ""
		if err := s.driver.Sleep(ctx, id); err != nil {
			return err
		}
		return s.store.Restore(ctx, id, req.Snapshot)
	})
}

func (s *Server) sleep(r *http.Request) (any, error) {
	id := r.PathValue("id")
	return nil, s.change(r.Context(), id, func(ctx context.Context, e *envState) error {
		if _, err := s.store.Get(ctx, id); err != nil {
			return err
		}
		e.addr = ""
		return s.driver.Sleep(ctx, id)
	})
}

func (s *Server) wake(r *http.Request) (any, error) {
	env, err := s.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	_, err = s.wakeBox(r.Context(), env, "")
	return nil, err
}

// exec runs a command in the environment's box, waking it first if needed,
// by relaying the client's WebSocket to the box's. It wakes and dials the
// box before taking the client's WebSocket, so that a failure is still an
// HTTP error with a body Client.Exec can read.
func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	env, err := s.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	box, err := s.dialBox(r.Context(), env)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer func() { _ = box.CloseNow() }()
	client, err := boxexec.Accept(w, r)
	if err != nil {
		return // Accept has answered the client.
	}
	defer func() { _ = client.CloseNow() }()
	_ = boxexec.Proxy(r.Context(), client, box) // it ends when either side does
}

// dialBox connects to the environment's box, waking it if needed. A box seen
// earlier may be gone (a crash, a deleted pod): then it is woken once more.
func (s *Server) dialBox(ctx context.Context, env store.Env) (*websocket.Conn, error) {
	var stale string
	var err error
	for range 2 {
		var addr string
		if addr, err = s.wakeBox(ctx, env, stale); err != nil {
			return nil, err
		}
		dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		conn, dialErr := boxexec.Dial(dialCtx, boxexec.URL(addr), env.BoxToken, nil)
		cancel()
		if dialErr == nil {
			return conn, nil
		}
		stale, err = addr, dialErr
	}
	return nil, fmt.Errorf("connect to box %s: %w", env.ID, err)
}

// wakeBox returns the address of the environment's box, waking it unless it
// is known to be up somewhere other than stale, which didn't answer.
func (s *Server) wakeBox(ctx context.Context, env store.Env, stale string) (string, error) {
	e, err := s.lock(ctx, env.ID)
	if err != nil {
		return "", err
	}
	defer e.release()
	if e.addr != "" && e.addr != stale {
		return e.addr, nil
	}
	e.addr = ""
	addr, err := s.driver.Wake(ctx, env)
	if err != nil {
		return "", err
	}
	e.addr = addr
	return addr, nil
}

// change runs fn on environment id while no one else changes or wakes it.
// Waiting can be cancelled by the client; fn can't.
func (s *Server) change(ctx context.Context, id string, fn func(ctx context.Context, e *envState) error) error {
	e, err := s.lock(ctx, id)
	if err != nil {
		return err
	}
	defer e.release()
	s.changes.Add(1)
	defer s.changes.Done()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), changeTimeout)
	defer cancel()
	return fn(ctx, e)
}

// lock returns environment id's state once no one else changes or wakes it,
// or ctx is done. The caller releases it. It checks the ID first, so a
// client can't grow the map with names that will never be environments.
func (s *Server) lock(ctx context.Context, id string) (*envState, error) {
	if err := store.CheckEnvID(id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	e, ok := s.envs[id]
	if !ok {
		e = &envState{lock: make(chan struct{}, 1)}
		s.envs[id] = e
	}
	s.mu.Unlock()
	select {
	case e.lock <- struct{}{}:
		return e, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
