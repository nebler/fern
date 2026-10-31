// Package server serves the daemon's HTTP API on a Unix socket.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/coder/websocket"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/api"
	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
)

// Store reads environments and snapshots on disk.
type Store interface {
	Get(ctx context.Context, id string) (store.Env, error)
	FindByPath(ctx context.Context, path string) (store.Env, error)
	List(ctx context.Context) ([]store.Env, error)
	Snapshots(ctx context.Context, id string) ([]store.Snapshot, error)
}

// Driver reports the state of boxes.
type Driver interface {
	States(ctx context.Context) (map[string]runsc.State, error)
}

// dialTimeout bounds connecting to a box and its WebSocket handshake.
const dialTimeout = 10 * time.Second

// errBadBody means a request's body isn't what its route takes.
var errBadBody = errors.New("bad request body")

// Server handles API requests. Reads go to the store and the driver; every
// change, wake and exec goes through the environment's actor.
type Server struct {
	store  Store
	driver Driver
	envs   *actor.Group
	tracer trace.Tracer
	log    *slog.Logger
	// agentMCP is where agent boxes reach the daemon's MCP endpoint (Bash),
	// such as http://fern-daemon:7071; "" runs Claude in the environment's
	// own box.
	agentMCP string
	// boxes connects to boxes. A box that vanished can leave an address
	// nothing answers; the same short connect timeout as readiness gets to
	// the next wake without waiting a second on that stale address.
	boxes *http.Client
	jobs  jobs // Claude's background commands
}

// opener counts a command in a box, waking it: actor.Group's Exec or Job.
type opener func(ctx context.Context, id, stale string, cancel context.CancelFunc) (*actor.Exec, error)

// New returns a Server that reads from st and drv, changes environments
// through envs and records every request with tr. With agentMCP, where agent
// boxes reach MCPHandler, Claude runs in each environment's agent box.
func New(st Store, drv Driver, envs *actor.Group, agentMCP string, tr trace.Tracer, log *slog.Logger) *Server {
	return &Server{
		store: st, driver: drv, envs: envs, tracer: tr, log: log, agentMCP: agentMCP,
		boxes: &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 250 * time.Millisecond}).DialContext}},
	}
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
	mux.HandleFunc("POST /envs/{id}/prewake", s.handle(s.prewake))
	mux.HandleFunc("PUT /goldens/{repo}", s.handle(s.prebuild))
	// WebSocket handshakes are GET requests.
	mux.HandleFunc("GET /envs/{id}/exec", s.exec)
	return mux
}

// handle serves a request with h and answers with what it returns: an error,
// nothing (204), or a value as JSON.
func (s *Server) handle(h func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := s.tracer.Start(r.Context(), r.Pattern)
		defer span.End()
		v, err := h(r.WithContext(ctx))
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		}
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
// state costs a runsc call.
func (s *Server) list(r *http.Request) (any, error) {
	if path := r.URL.Query().Get("path"); path != "" {
		env, err := s.store.FindByPath(r.Context(), path)
		if errors.Is(err, store.ErrNotFound) {
			return []api.Env{}, nil
		}
		if err != nil {
			return nil, err
		}
		return []api.Env{toAPIEnv(env, "")}, nil
	}
	envs, err := s.store.List(r.Context())
	if err != nil {
		return nil, err
	}
	// One runsc call for all of them, not one each.
	states, err := s.driver.States(r.Context())
	if err != nil {
		s.log.WarnContext(r.Context(), "box states", slog.Any("error", err))
	}
	out := make([]api.Env, 0, len(envs))
	for _, env := range envs {
		state, ok := states[env.ID]
		switch {
		case err != nil:
			state = runsc.Unknown
		case !ok:
			state = runsc.Asleep
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

// prebuild builds the golden snapshot of a repository from its checkout, a
// tar stream, ahead of its first worktree: T3 sends it when a project is
// added. The answer comes before the build.
func (s *Server) prebuild(r *http.Request) (any, error) {
	if r.Header.Get("Content-Type") != "application/x-tar" {
		return nil, fmt.Errorf("a checkout as application/x-tar: %w", errBadBody)
	}
	return nil, s.envs.Prebuild(r.Context(), r.PathValue("repo"), r.Body)
}

func (s *Server) delete(r *http.Request) (any, error) {
	return nil, s.envs.Delete(r.Context(), r.PathValue("id"))
}

func (s *Server) snapshot(r *http.Request) (any, error) {
	var req api.SnapshotRequest
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
	out := make([]api.Snapshot, 0, len(snaps))
	for _, snap := range snaps {
		out = append(out, api.Snapshot{ID: snap.ID, Created: snap.Created})
	}
	return out, nil
}

func (s *Server) deleteSnapshot(r *http.Request) (any, error) {
	return nil, s.envs.DeleteSnapshot(r.Context(), r.PathValue("id"), r.PathValue("snap"))
}

func (s *Server) restore(r *http.Request) (any, error) {
	var req api.RestoreRequest
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

// prewake queues a wake of the environment's box and answers at once,
// without saying whether the environment exists: its caller, T3, only
// guesses that a command is coming.
func (s *Server) prewake(r *http.Request) (any, error) {
	id := r.PathValue("id")
	// The next turn needs Claude, whose box is the agent box; the services'
	// box waits for its Bash.
	if s.agentMCP != "" {
		id = store.AgentBox(id)
	}
	return nil, s.envs.Prewake(id)
}

// exec runs a command in the environment's box, waking it first if needed,
// by relaying the client's WebSocket to the box's. With ?then=sleep the box
// sleeps as soon as the command ends, unless another runs. With ?box=agent
// and agent boxes, it runs in the environment's agent box, and Claude there
// gets Fern's Bash in place of its own (startClaude). It wakes and dials the
// box before taking the client's WebSocket, so that a failure is still an
// HTTP error with a body Client.Exec can read. A restore, sleep or delete
// of the environment ends it.
func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// The span ends once the command can start: its run is the box's time.
	spanCtx, span := s.tracer.Start(ctx, "exec.open")
	id := r.PathValue("id")
	agent := s.agentMCP != "" && r.URL.Query().Get("box") == "agent"
	if agent {
		id = store.AgentBox(id)
	}
	box, ex, err := s.dialBox(spanCtx, id, cancel, s.envs.Exec)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.End()
		s.fail(w, r, err)
		return
	}
	// An agent that stopped because it had nothing to do lets its box sleep.
	if r.URL.Query().Get("then") == "sleep" {
		defer ex.CloseAndSleep(0)
	} else {
		defer ex.Close()
	}
	defer func() { _ = box.CloseNow() }()
	client, err := boxexec.Accept(w, r)
	span.End()
	if err != nil {
		return // Accept has answered the client.
	}
	defer func() { _ = client.CloseNow() }()
	if agent {
		if err := s.startClaude(ctx, client, box, r.PathValue("id"), ex.Token); err != nil {
			s.log.WarnContext(ctx, "starting a command in an agent box", slog.String("env", r.PathValue("id")), slog.Any("error", err))
			return
		}
	}
	boxexec.Proxy(ctx, client, box)
}

// noShell are Claude's tools that run shell commands. In an agent box they
// would run without the services; Fern's Bash runs them in the services' box.
var noShell = []string{"Bash", "BashOutput", "KillShell", "KillBash", "Monitor"}

// startClaude passes on the client's first message, the command's request,
// and if it starts Claude, adds Fern's MCP server for environment id (its
// Bash) and takes away Claude's own shell tools.
func (s *Server) startClaude(ctx context.Context, client, box *websocket.Conn, id, token string) error {
	typ, data, err := client.Read(ctx)
	if err != nil {
		return err
	}
	var req boxexec.Request
	if typ == websocket.MessageText && json.Unmarshal(data, &req) == nil && len(req.Argv) > 0 && path.Base(req.Argv[0]) == "claude" {
		cfg, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"fern": map[string]any{
			"type":    "http",
			"url":     s.agentMCP + "/mcp/" + id,
			"headers": map[string]string{"Authorization": "Bearer " + token},
		}}})
		if err != nil {
			return err
		}
		req.Argv = append(req.Argv, "--mcp-config", string(cfg), "--disallowedTools", strings.Join(noShell, ","))
		if data, err = json.Marshal(req); err != nil {
			return err
		}
	}
	return box.Write(ctx, typ, data)
}

// dialBox connects to environment id's box, waking it if needed, for a
// command open counts. A box seen earlier may be gone (a crash, a box stopped
// by hand): then it is woken once more.
func (s *Server) dialBox(ctx context.Context, id string, cancel context.CancelFunc, open opener) (*websocket.Conn, *actor.Exec, error) {
	var stale string
	var err error
	for range 2 {
		var ex *actor.Exec
		if ex, err = open(ctx, id, stale, cancel); err != nil {
			return nil, nil, err
		}
		dialCtx, cancelDial := context.WithTimeout(ctx, dialTimeout)
		conn, dialErr := boxexec.Dial(dialCtx, "ws://"+ex.Addr+boxexec.Path, ex.Token, s.boxes)
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
	case errors.Is(err, store.ErrPoolFull):
		// The caller can free space; the daemon has nothing to log.
		status = http.StatusInsufficientStorage
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		// The client gave up waiting; there is no one to answer.
	default:
		s.log.ErrorContext(r.Context(), "request failed",
			slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Any("error", err))
	}
	writeJSON(w, status, api.Error{Message: err.Error()})
}

// writeJSON answers with v. A client that went away gets nothing more.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func toAPIEnv(env store.Env, state runsc.State) api.Env {
	return api.Env{ID: env.ID, Workspace: env.Workspace, Created: env.Created, State: string(state)}
}
