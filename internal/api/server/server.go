// Package server serves the daemon's HTTP API on a Unix socket.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/api"
	"github.com/nebler/fern/internal/backup"
	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
)

// Store reads environments and snapshots on disk.
type Store interface {
	FindByPath(ctx context.Context, path string) (store.Env, error)
	List(ctx context.Context) ([]store.Env, error)
	Snapshots(ctx context.Context, id string) ([]store.Snapshot, error)
}

// Driver reports the state of boxes.
type Driver interface {
	States(ctx context.Context) (map[string]runsc.State, error)
}

// Backer makes a backup of the node's threads now (backup.Backer).
type Backer interface {
	Once(ctx context.Context) error
}

// Keeper says what uses the node's disk, and removes what nothing needs
// (disk.Keeper).
type Keeper interface {
	Usage(ctx context.Context) (api.Disk, error)
	Free(ctx context.Context) (api.Freed, error)
}

// Limiter changes an environment's disk limit (store.Store).
type Limiter interface {
	SetLimit(ctx context.Context, id string, bytes uint64) error
}

// Checker says what is wrong on the node now (health.Checker).
type Checker interface {
	Problems(ctx context.Context) ([]api.Problem, error)
}

// errBadBody means a request's body isn't what its route takes.
var errBadBody = errors.New("bad request body")

// errNoBackups means the node has no backups set up.
var errNoBackups = errors.New("no backups are set up")

// Server handles API requests. Reads go to the store and the driver; every
// change, wake and exec goes through the environment's actor.
type Server struct {
	store  Store
	driver Driver
	envs   *actor.Group
	tracer trace.Tracer
	log    *slog.Logger
	// agents says whether environments have agent boxes, which run Claude;
	// without them it runs in the environment's own box.
	agents bool

	// StartAgent passes the first message of a command in environment id's
	// agent box, the command's request, from client on to box, and gives an
	// agent it starts Fern's tools in place of its own shell
	// (tools.Server.StartAgent). May be nil: the request goes on as it is.
	StartAgent func(ctx context.Context, client, box *websocket.Conn, id string, ex *actor.Exec) error

	// Backups makes a backup now, as the hourly ones are made (POST /backup,
	// fern backup). May be nil: no backups are set up, and BackupConfig is the
	// file whose absence says so, named in the answer.
	Backups      Backer
	BackupConfig string
	// Disk answers GET /disk and POST /gc (fern df, fern gc): what uses the
	// node's disk, and what nothing needs removed from it. May be nil: no
	// such routes.
	Disk Keeper
	// Health answers GET /problems (fern doctor): what is wrong now that
	// would otherwise be a line in the daemon's log. May be nil: no route.
	Health Checker
	// Limits answers PUT /envs/{id}/limit (fern limit), which sets an
	// environment's disk limit. It is the kernel's to keep, one ioctl, so
	// it doesn't go through the environment's actor. May be nil: no route.
	Limits Limiter
	// GitHubToken serves POST /github/token (github.Handler): T3's git and gh
	// get their tokens on the API's socket, which boxes can't reach. May be
	// nil: no route.
	GitHubToken http.Handler

	// Stopping is closed when the daemon begins to stop. May be nil. Then the
	// commands running in boxes are left to run: the next daemon's clients
	// take them up (boxexec's sessions).
	Stopping <-chan struct{}
}

// stopping reports whether the daemon has begun to stop.
func (s *Server) stopping() bool {
	select {
	case <-s.Stopping:
		return true
	default:
		return false
	}
}

// New returns a Server that reads from st and drv, changes environments
// through envs and records every request with tr. With agents, Claude runs
// in each environment's agent box.
func New(st Store, drv Driver, envs *actor.Group, agents bool, tr trace.Tracer, log *slog.Logger) *Server {
	return &Server{store: st, driver: drv, envs: envs, agents: agents, tracer: tr, log: log}
}

// Handler returns the API's routes, for the Server as its fields are now.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /envs", s.handle(s.list))
	mux.HandleFunc("PUT /envs/{id}", s.handle(s.create))
	mux.HandleFunc("DELETE /envs/{id}", s.handle(s.delete))
	mux.HandleFunc("POST /envs/{id}/snapshots", s.handle(s.snapshot))
	mux.HandleFunc("GET /envs/{id}/snapshots", s.handle(s.snapshots))
	mux.HandleFunc("DELETE /envs/{id}/snapshots/{snap}", s.handle(s.deleteSnapshot))
	mux.HandleFunc("POST /envs/{id}/restore", s.handle(s.restore))
	mux.HandleFunc("POST /envs/{id}/fork", s.handle(s.fork))
	mux.HandleFunc("POST /envs/{id}/sleep", s.handle(s.sleep))
	mux.HandleFunc("POST /envs/{id}/wake", s.handle(s.wake))
	mux.HandleFunc("POST /envs/{id}/prewake", s.handle(s.prewake))
	mux.HandleFunc("GET /envs/{id}/holds", s.handle(s.holds))
	mux.HandleFunc("PUT /goldens/{repo}", s.handle(s.prebuild))
	mux.HandleFunc("POST /backup", s.handle(s.backup))
	if s.Disk != nil {
		mux.HandleFunc("GET /disk", s.handle(s.disk))
		mux.HandleFunc("POST /gc", s.handle(s.gc))
	}
	if s.Health != nil {
		mux.HandleFunc("GET /problems", s.handle(s.problems))
	}
	if s.Limits != nil {
		mux.HandleFunc("PUT /envs/{id}/limit", s.handle(s.limit))
	}
	if s.GitHubToken != nil {
		mux.Handle("POST /github/token", s.GitHubToken)
	}
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
// so that answer comes from the records alone, without the box's state: a
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

func (s *Server) fork(r *http.Request) (any, error) {
	var req api.ForkRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if req.Count < 1 || req.Count > actor.MaxForks {
		return nil, fmt.Errorf("count %d: from 1 to %d: %w", req.Count, actor.MaxForks, errBadBody)
	}
	ids, err := s.envs.Fork(r.Context(), r.PathValue("id"), req.Snapshot, req.Count)
	if err != nil {
		return nil, err
	}
	return api.ForkResponse{Forks: ids}, nil
}

func (s *Server) sleep(r *http.Request) (any, error) {
	return nil, s.envs.Sleep(r.Context(), r.PathValue("id"))
}

func (s *Server) wake(r *http.Request) (any, error) {
	return nil, s.envs.Wake(r.Context(), r.PathValue("id"))
}

// holds says what holds the environment's box awake, and its agent box if
// boxes have one: the answer to "why is this still running?". It asks the
// actors, and wakes nothing.
func (s *Server) holds(r *http.Request) (any, error) {
	boxes := []string{r.PathValue("id")}
	if s.agents {
		boxes = append(boxes, store.AgentBox(boxes[0]))
	}
	out := make([]api.Holds, 0, len(boxes))
	for _, box := range boxes {
		h, err := s.envs.Holds(r.Context(), box)
		if err != nil {
			return nil, err
		}
		running := make([]api.Running, 0, len(h.Running))
		for _, run := range h.Running {
			running = append(running, api.Running{Kind: run.Kind, N: run.N, Since: run.Since})
		}
		out = append(out, api.Holds{Box: h.Box, Awake: h.Awake, Running: running, SleepAt: h.SleepAt, Why: h.Why})
	}
	return out, nil
}

// prewake queues a wake of the environment's box and answers at once,
// without saying whether the environment exists: its caller, T3, only
// guesses that a command is coming.
func (s *Server) prewake(r *http.Request) (any, error) {
	id := r.PathValue("id")
	// The next turn needs Claude, whose box is the agent box; the services'
	// box waits for its Bash.
	if s.agents {
		id = store.AgentBox(id)
	}
	return nil, s.envs.Prewake(id)
}

// nodeError is a failure of a route about the node, not about an environment
// the caller named (the backup, the disk, the problems): it is the daemon's
// whatever it wraps, as it was when the daemon's main served these routes.
// It has no Unwrap, so fail finds none of its cases in it: a thread deleted
// while its backup ran is no 404 for fern backup, which reads that as "no
// backups are set up".
type nodeError struct{ error }

// ofNode returns err as a nodeError, or nil if it is nil.
func ofNode(err error) error {
	if err == nil {
		return nil
	}
	return nodeError{err}
}

// backup makes a backup now, as the hourly ones are made.
func (s *Server) backup(r *http.Request) (any, error) {
	if s.Backups == nil {
		return nil, fmt.Errorf("%w: %s is missing (README: backups)", errNoBackups, s.BackupConfig)
	}
	err := s.Backups.Once(r.Context())
	if errors.Is(err, backup.ErrBusy) {
		return nil, err
	}
	return nil, ofNode(err)
}

// disk says what uses the node's disk.
func (s *Server) disk(r *http.Request) (any, error) {
	d, err := s.Disk.Usage(r.Context())
	return d, ofNode(err)
}

// gc removes from the node's disk what nothing needs, and says what it
// removed.
func (s *Server) gc(r *http.Request) (any, error) {
	freed, err := s.Disk.Free(r.Context())
	return freed, ofNode(err)
}

// limit sets an environment's disk limit.
func (s *Server) limit(r *http.Request) (any, error) {
	var req api.LimitRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	return nil, s.Limits.SetLimit(r.Context(), r.PathValue("id"), req.Bytes)
}

// problems says what is wrong on the node now.
func (s *Server) problems(r *http.Request) (any, error) {
	problems, err := s.Health.Problems(r.Context())
	return problems, ofNode(err)
}

// exec runs a command in the environment's box, waking it first if needed,
// by relaying the client's WebSocket to the box's. With ?then=sleep the box
// sleeps as soon as the command ends, unless another runs. With ?box=agent
// and agent boxes, it runs in the environment's agent box, and Claude there
// gets Fern's Bash in place of its own (StartAgent). It wakes and dials the
// box before taking the client's WebSocket, so that a failure is still an
// HTTP error with a body Client.Exec can read. A restore, sleep or delete
// of the environment ends it.
func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// The span ends once the command can start: its run is the box's time.
	spanCtx, span := s.tracer.Start(ctx, "exec.open")
	id := r.PathValue("id")
	agent := s.agents && r.URL.Query().Get("box") == "agent"
	if agent {
		id = store.AgentBox(id)
	}
	box, ex, err := s.envs.Dial(spanCtx, id, cancel)
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
	if agent && s.StartAgent != nil {
		if err := s.StartAgent(ctx, client, box, r.PathValue("id"), ex); err != nil {
			s.log.WarnContext(ctx, "starting a command in an agent box", slog.String("env", r.PathValue("id")), slog.Any("error", err))
			return
		}
	}
	boxexec.Proxy(ctx, client, box, s.stopping)
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
	case errors.Is(err, store.ErrNotFound), errors.Is(err, errNoBackups):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrInvalidID), errors.Is(err, store.ErrInvalidPath), errors.Is(err, errBadBody):
		status = http.StatusBadRequest
	case errors.Is(err, actor.ErrTooManyForks), errors.Is(err, backup.ErrBusy), errors.Is(err, store.ErrNoLimit):
		status = http.StatusConflict
	case errors.Is(err, actor.ErrStopped), errors.Is(err, actor.ErrCrashing):
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
