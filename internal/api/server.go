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
	"strings"
	"sync"

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
	store.Env
	State driver.State `json:"state"`
}

// Server handles API requests. Mutations of one environment run one at a
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
	mu   sync.Mutex // held for the whole of a mutation or a wake
	addr string     // the box's exec address while it is known to be up
}

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

// create makes an environment, filled from a tar body if there is one.
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	e := s.env(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	var seed io.Reader
	if r.Header.Get("Content-Type") == "application/x-tar" {
		seed = r.Body
	}
	env, err := s.store.Create(r.Context(), id, r.URL.Query().Get("repo"), seed)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.driver.Create(r.Context(), id, env.Token); err != nil {
		s.fail(w, r, err)
		return
	}
	s.reply(w, r, Env{Env: env, State: s.state(r.Context(), id)})
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	env, err := s.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.reply(w, r, Env{Env: env, State: s.state(r.Context(), env.ID)})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	envs, err := s.store.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]Env, 0, len(envs))
	for _, env := range envs {
		out = append(out, Env{Env: env, State: s.state(r.Context(), env.ID)})
	}
	s.reply(w, r, out)
}

// delete stops the box, then removes the disk.
func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	e := s.env(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := s.store.Get(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	e.addr = ""
	if err := s.driver.Delete(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.Delete(r.Context(), id); err != nil {
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
	e := s.env(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	snap, err := s.store.Snapshot(r.Context(), id, req.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.reply(w, r, snap)
}

func (s *Server) snapshots(w http.ResponseWriter, r *http.Request) {
	snaps, err := s.store.Snapshots(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if snaps == nil {
		snaps = []store.Snapshot{}
	}
	s.reply(w, r, snaps)
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
	e := s.env(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := s.store.Get(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.sleepLocked(r.Context(), e, id); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.Restore(r.Context(), id, req.Snapshot); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sleep(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	e := s.env(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := s.sleepLocked(r.Context(), e, id); err != nil {
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
	defer box.CloseNow()
	client, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has answered the client.
	}
	defer client.CloseNow()
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
		conn, err := boxexec.Dial(ctx, "ws://"+addr+"/exec", env.Token, nil)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		s.forget(env.ID, addr)
	}
	return nil, fmt.Errorf("connect to box %s: %w", env.ID, lastErr)
}

// boxAddr returns the address of the environment's box, waking it if it
// isn't known to be up.
func (s *Server) boxAddr(ctx context.Context, id string) (string, error) {
	e := s.env(id)
	e.mu.Lock()
	defer e.mu.Unlock()
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

func (s *Server) forget(id, addr string) {
	e := s.env(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.addr == addr {
		e.addr = ""
	}
}

func (s *Server) sleepLocked(ctx context.Context, e *envState, id string) error {
	e.addr = ""
	return s.driver.Sleep(ctx, id)
}

func (s *Server) env(id string) *envState {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.envs[id]
	if !ok {
		e = &envState{}
		s.envs[id] = e
	}
	return e
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
