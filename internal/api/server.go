// Package api is the daemon's HTTP API, served on a unix socket, and a
// client for it.
package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/driver"
	"github.com/nebler/fern/internal/store"
)

// Store keeps environments and snapshots on disk.
type Store interface {
	Create(ctx context.Context, id, repo string, seed io.Reader) (store.Env, error)
	Get(ctx context.Context, id string) (store.Env, error)
	List(ctx context.Context) ([]store.Env, error)
	Delete(ctx context.Context, id string) error
	Snapshot(ctx context.Context, env, snap string) (store.Snapshot, error)
	Snapshots(ctx context.Context, env string) ([]store.Snapshot, error)
	Restore(ctx context.Context, env, snap string) error
}

// Driver runs environments' boxes.
type Driver interface {
	Create(ctx context.Context, env, token string) error
	Wake(ctx context.Context, env string) (addr string, err error)
	Sleep(ctx context.Context, env string) error
	Delete(ctx context.Context, env string) error
	State(ctx context.Context, env string) (driver.State, error)
}

// Env is an environment as the API reports it.
type Env struct {
	ID      string       `json:"id"`
	Repo    string       `json:"repo,omitempty"`
	Head    string       `json:"head,omitempty"` // last snapshot taken or restored
	Created time.Time    `json:"created"`
	State   driver.State `json:"state"`
}

// Snapshot is a snapshot as the API reports it.
type Snapshot struct {
	ID      string    `json:"id"`
	Env     string    `json:"env"`
	Created time.Time `json:"created"`
}

func snapshotOf(s store.Snapshot) Snapshot {
	return Snapshot{ID: s.ID, Env: s.Env, Created: s.Created}
}

const (
	// changeTimeout bounds a change of an environment. Changes don't stop
	// when the client goes away, which would leave them half done.
	changeTimeout = 10 * time.Minute
	// dialTimeout bounds connecting to a box that is up; a box that vanished
	// can leave an address nothing answers.
	dialTimeout = 10 * time.Second
)

// Server handles API requests. Changes of one environment run one at a
// time; execs run alongside them.
type Server struct {
	store  Store
	driver Driver
	log    *slog.Logger

	mu   sync.Mutex
	envs map[string]*envState
}

// envState is what the server keeps in memory about one environment.
type envState struct {
	lock chan struct{} // holds a value for the whole of a change or a wake
	addr string        // the box's exec address while it is known to be up; guarded by lock
}

// acquire waits until no one else holds e, or ctx is done.
func (e *envState) acquire(ctx context.Context) error {
	select {
	case e.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *envState) release() { <-e.lock }

// NewServer returns a Server for the environments in st, run by drv.
func NewServer(st Store, drv Driver, log *slog.Logger) *Server {
	return &Server{store: st, driver: drv, log: log, envs: make(map[string]*envState)}
}

// Handler returns the API's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /envs", s.list)
	mux.HandleFunc("PUT /envs/{id}", s.create)
	mux.HandleFunc("GET /envs/{id}", s.get)
	mux.HandleFunc("DELETE /envs/{id}", s.delete)
	mux.HandleFunc("POST /envs/{id}/snapshots", s.snapshot)
	mux.HandleFunc("GET /envs/{id}/snapshots", s.snapshots)
	mux.HandleFunc("POST /envs/{id}/restore", s.restore)
	mux.HandleFunc("POST /envs/{id}/sleep", s.sleep)
	mux.HandleFunc("POST /envs/{id}/wake", s.wake)
	// WebSocket handshakes are GET requests.
	mux.HandleFunc("GET /envs/{id}/exec", s.exec)
	return mux
}

// change runs fn on environment id while no one else changes or wakes it.
// Waiting can be cancelled by the client; fn can't.
func (s *Server) change(r *http.Request, id string, fn func(ctx context.Context, e *envState) error) error {
	e, err := s.env(id)
	if err != nil {
		return err
	}
	if err := e.acquire(r.Context()); err != nil {
		return err
	}
	defer e.release()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), changeTimeout)
	defer cancel()
	return fn(ctx, e)
}

// create makes an environment, filled from a tar body if there is one.
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var seed io.Reader
	if r.Header.Get("Content-Type") == "application/x-tar" {
		seed = r.Body
	}
	var env store.Env
	err := s.change(r, id, func(ctx context.Context, _ *envState) error {
		var err error
		if env, err = s.store.Create(ctx, id, r.URL.Query().Get("repo"), seed); err != nil {
			return err
		}
		return s.driver.Create(ctx, id, env.Token)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.reply(w, r, s.envOf(r.Context(), env))
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	env, err := s.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.reply(w, r, s.envOf(r.Context(), env))
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	envs, err := s.store.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]Env, 0, len(envs))
	for _, env := range envs {
		out = append(out, s.envOf(r.Context(), env))
	}
	s.reply(w, r, out)
}

// delete stops the box, then removes the disk.
func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.change(r, id, func(ctx context.Context, e *envState) error {
		if _, err := s.store.Get(ctx, id); err != nil {
			return err
		}
		e.addr = ""
		if err := s.driver.Delete(ctx, id); err != nil {
			return err
		}
		return s.store.Delete(ctx, id)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SnapshotRequest is the body of POST /envs/{id}/snapshots.
type SnapshotRequest struct {
	ID string `json:"id,omitempty"` // generated if empty
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	var req SnapshotRequest
	if !s.decode(w, r, &req) {
		return
	}
	if req.ID == "" {
		req.ID = strings.ToLower(rand.Text())
	}
	id := r.PathValue("id")
	var snap store.Snapshot
	err := s.change(r, id, func(ctx context.Context, _ *envState) error {
		var err error
		snap, err = s.store.Snapshot(ctx, id, req.ID)
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.reply(w, r, snapshotOf(snap))
}

func (s *Server) snapshots(w http.ResponseWriter, r *http.Request) {
	snaps, err := s.store.Snapshots(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]Snapshot, 0, len(snaps))
	for _, snap := range snaps {
		out = append(out, snapshotOf(snap))
	}
	s.reply(w, r, out)
}

// RestoreRequest is the body of POST /envs/{id}/restore.
type RestoreRequest struct {
	Snapshot string `json:"snapshot"`
}

// restore puts the environment to sleep and swaps its disk. It stays asleep;
// the next exec wakes it on the restored disk.
func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	var req RestoreRequest
	if !s.decode(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	err := s.change(r, id, func(ctx context.Context, e *envState) error {
		// Check the snapshot before stopping the box: a typo mustn't cost
		// the agent its running processes.
		snaps, err := s.store.Snapshots(ctx, id)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(snaps, func(snap store.Snapshot) bool { return snap.ID == req.Snapshot }) {
			return fmt.Errorf("snapshot %s/%s: %w", id, req.Snapshot, store.ErrNotFound)
		}
		if err := s.sleepLocked(ctx, e, id); err != nil {
			return err
		}
		return s.store.Restore(ctx, id, req.Snapshot)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sleep(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.change(r, id, func(ctx context.Context, e *envState) error {
		if _, err := s.store.Get(ctx, id); err != nil {
			return err
		}
		return s.sleepLocked(ctx, e, id)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) wake(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.Get(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.boxAddr(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// exec runs a command in the environment's box, waking it first if needed,
// by relaying the client's WebSocket to the box's.
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
	if err := boxexec.Proxy(r.Context(), client, box); err != nil {
		s.log.DebugContext(r.Context(), "exec relay ended", slog.String("env", env.ID), slog.Any("error", err))
	}
}

// dialBox connects to the environment's box, waking it if needed.
func (s *Server) dialBox(ctx context.Context, env store.Env) (*websocket.Conn, error) {
	var lastErr error
	// A box we saw earlier may be gone (a crash, a deleted pod): then forget
	// it and wake once more.
	for range 2 {
		addr, err := s.boxAddr(ctx, env.ID)
		if err != nil {
			return nil, err
		}
		dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		conn, err := boxexec.Dial(dialCtx, boxexec.URL(addr), env.Token, nil)
		cancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if err := s.forget(ctx, env.ID, addr); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("connect to box %s: %w", env.ID, lastErr)
}

// boxAddr returns the address of the environment's box, waking it if it
// isn't known to be up.
func (s *Server) boxAddr(ctx context.Context, id string) (string, error) {
	e, err := s.env(id)
	if err != nil {
		return "", err
	}
	if err := e.acquire(ctx); err != nil {
		return "", err
	}
	defer e.release()
	if e.addr != "" {
		return e.addr, nil
	}
	addr, err := s.driver.Wake(ctx, id)
	if err != nil {
		return "", err
	}
	e.addr = addr
	return addr, nil
}

// forget drops the box address addr, unless someone has replaced it.
func (s *Server) forget(ctx context.Context, id, addr string) error {
	e, err := s.env(id)
	if err != nil {
		return err
	}
	if err := e.acquire(ctx); err != nil {
		return err
	}
	defer e.release()
	if e.addr == addr {
		e.addr = ""
	}
	return nil
}

// sleepLocked stops the box; the caller holds e.
func (s *Server) sleepLocked(ctx context.Context, e *envState, id string) error {
	e.addr = ""
	return s.driver.Sleep(ctx, id)
}

// env returns the in-memory state of environment id. It checks the ID, so
// a client can't grow the map with names that will never be environments.
func (s *Server) env(id string) (*envState, error) {
	if err := store.CheckEnvID(id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.envs[id]
	if !ok {
		e = &envState{lock: make(chan struct{}, 1)}
		s.envs[id] = e
	}
	return e, nil
}

// envOf is env as the API reports it.
func (s *Server) envOf(ctx context.Context, env store.Env) Env {
	return Env{ID: env.ID, Repo: env.Repo, Head: env.Head, Created: env.Created, State: s.state(ctx, env.ID)}
}

// state is the box state for listings, where an error shouldn't fail the
// whole answer.
func (s *Server) state(ctx context.Context, id string) driver.State {
	state, err := s.driver.State(ctx, id)
	if err != nil {
		s.log.WarnContext(ctx, "box state", slog.String("env", id), slog.Any("error", err))
		return "unknown"
	}
	return state
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		s.writeError(w, r, &Error{Code: CodeInvalid, Message: "bad request body: " + err.Error()})
		return false
	}
	return true
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.DebugContext(r.Context(), "write reply", slog.Any("error", err))
	}
}

// fail answers with err as a typed error, and logs failures that aren't the
// caller's fault.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	e := &Error{Code: CodeFailed, Message: err.Error()}
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, driver.ErrNotFound):
		e.Code = CodeNotFound
	case errors.Is(err, store.ErrInvalidID):
		e.Code = CodeInvalid
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		// The client gave up waiting; there is no one to answer.
	default:
		s.log.ErrorContext(r.Context(), "request failed",
			slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Any("error", err))
	}
	s.writeError(w, r, e)
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, e *Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status())
	if err := json.NewEncoder(w).Encode(e); err != nil {
		s.log.DebugContext(r.Context(), "write error reply", slog.Any("error", err))
	}
}
