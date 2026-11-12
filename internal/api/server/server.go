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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/api"
	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/setup"
	"github.com/nebler/fern/internal/store"
)

// Store reads environments and snapshots on disk.
type Store interface {
	Get(ctx context.Context, id string) (store.Env, error)
	FindByPath(ctx context.Context, path string) (store.Env, error)
	List(ctx context.Context) ([]store.Env, error)
	Snapshots(ctx context.Context, id string) ([]store.Snapshot, error)
}

// Driver reports the state of boxes, and the token a running box was
// started with.
type Driver interface {
	States(ctx context.Context) (map[string]runsc.State, error)
	Token(id string) (string, error)
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
	jobs  jobs     // Claude's background commands
	calls sync.Map // call key to its running Bash command, a *call
	// schemas keeps the tools' JSON schemas: an MCP server is made for every
	// request, and working them out again each time was most of a call's
	// cost in the daemon (BenchmarkMCPCall).
	schemas *mcp.SchemaCache

	// Cloudflare returns the node's Cloudflare API token, "" while there is
	// none. With one, an agent in the agent box of a repository that deploys
	// to Cloudflare (setup.IsCloudflare) gets Cloudflare's MCP server, which
	// acts on that account. The token goes to the agent box only: never to
	// the box that runs the repository's code, nor into a workspace. May be
	// nil.
	Cloudflare func(ctx context.Context) (string, error)
	// T3 is T3's server on the node, such as http://127.0.0.1:30773; ""
	// for none. T3 gives its Claude an MCP server there, on the node's
	// loopback, which a box can't reach: an agent box's fern-init listens
	// at that address in the box and passes the calls to T3Handler's route.
	T3 string
	// GitHub sets repositories' Actions secrets (cloudflare_connect). May be
	// nil: no GitHub App.
	GitHub Secrets

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

// opener counts a command in a box, waking it: actor.Group's Exec or Job.
type opener func(ctx context.Context, id string, cancel context.CancelFunc) (*actor.Exec, error)

// New returns a Server that reads from st and drv, changes environments
// through envs and records every request with tr. With agentMCP, where agent
// boxes reach MCPHandler, Claude runs in each environment's agent box.
func New(st Store, drv Driver, envs *actor.Group, agentMCP string, tr trace.Tracer, log *slog.Logger) *Server {
	return &Server{
		store: st, driver: drv, envs: envs, tracer: tr, log: log, agentMCP: agentMCP,
		boxes:   &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 250 * time.Millisecond}).DialContext}},
		schemas: mcp.NewSchemaCache(),
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
	mux.HandleFunc("POST /envs/{id}/fork", s.handle(s.fork))
	mux.HandleFunc("POST /envs/{id}/sleep", s.handle(s.sleep))
	mux.HandleFunc("POST /envs/{id}/wake", s.handle(s.wake))
	mux.HandleFunc("POST /envs/{id}/prewake", s.handle(s.prewake))
	mux.HandleFunc("GET /envs/{id}/holds", s.handle(s.holds))
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
	if s.agentMCP != "" {
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
	if s.agentMCP != "" {
		id = store.AgentBox(id)
	}
	return nil, s.envs.Prewake(id)
}

// exec runs a command in the environment's box, waking it first if needed,
// by relaying the client's WebSocket to the box's. With ?then=sleep the box
// sleeps as soon as the command ends, unless another runs. With ?box=agent
// and agent boxes, it runs in the environment's agent box, and Claude there
// gets Fern's Bash in place of its own (startAgent). It wakes and dials the
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
		if err := s.startAgent(ctx, client, box, r.PathValue("id"), ex); err != nil {
			s.log.WarnContext(ctx, "starting a command in an agent box", slog.String("env", r.PathValue("id")), slog.Any("error", err))
			return
		}
	}
	boxexec.Proxy(ctx, client, box, s.stopping)
}

// noShell are Claude's tools that run shell commands. In an agent box they
// would run without the services; Fern's Bash runs them in the services' box.
var noShell = []string{"Bash", "BashOutput", "KillShell", "KillBash", "Monitor"}

// startAgent passes on the client's first message, the command's request,
// and if it starts an agent (Claude, or OpenCode's server), gives the agent
// Fern's MCP server for environment id (its Bash) in place of its own shell.
// It has none on a later connection to a session.
func (s *Server) startAgent(ctx context.Context, client, box *websocket.Conn, id string, ex *actor.Exec) error {
	typ, data, err := client.Read(ctx)
	if err != nil {
		return err
	}
	var req boxexec.Request
	if typ == websocket.MessageText && json.Unmarshal(data, &req) == nil && len(req.Argv) > 0 {
		agent := path.Base(req.Argv[0])
		if agent != "claude" && agent != "opencode" {
			// Any other command starts as it is, without a question to the box.
			return box.Write(ctx, typ, data)
		}
		url, token := s.mcpURL(ctx, ex)
		mcp := fernMCP{
			URL: url + "/mcp/" + id, Token: token,
			Cloudflare: s.cloudflareToken(ctx, id), AstroDocs: s.isAstro(ctx, id),
		}
		if agent == "claude" {
			err = mcp.claude(&req)
		} else {
			err = mcp.openCode(&req, ex.Addr)
		}
		if err != nil {
			return err
		}
		if data, err = json.Marshal(req); err != nil {
			return err
		}
	}
	return box.Write(ctx, typ, data)
}

// cloudflareMCP is Cloudflare's MCP server for its whole API, which takes an
// API token as the bearer token.
const cloudflareMCP = "https://mcp.cloudflare.com/mcp"

// cloudflareToken returns the Cloudflare token for an agent in environment
// id's agent box: the node's, if its repository deploys to Cloudflare, else
// "". The agent works without it, so a failure is logged, not returned.
func (s *Server) cloudflareToken(ctx context.Context, id string) string {
	if s.Cloudflare == nil {
		return ""
	}
	token, err := s.Cloudflare(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "reading the Cloudflare token", slog.Any("error", err))
		return ""
	}
	if token == "" {
		return ""
	}
	env, err := s.store.Get(ctx, id)
	if err != nil {
		s.log.WarnContext(ctx, "finding the workspace for Cloudflare's MCP server", slog.String("env", id), slog.Any("error", err))
		return ""
	}
	// The workspace is the agent's: IsCloudflare keeps its reads inside it.
	is, err := setup.IsCloudflare(env.Workspace)
	if err != nil {
		s.log.WarnContext(ctx, "reading whether the repository deploys to Cloudflare", slog.String("env", id), slog.Any("error", err))
		return ""
	}
	if !is {
		return ""
	}
	return token
}

// astroDocsMCP is Astro's MCP server for its documentation, which takes no
// login.
const astroDocsMCP = "https://mcp.docs.astro.build/mcp"

// isAstro reports whether environment id's repository is an Astro site, for
// Astro's docs server. The agent works without it, so a failure is logged.
func (s *Server) isAstro(ctx context.Context, id string) bool {
	env, err := s.store.Get(ctx, id)
	if err != nil {
		s.log.WarnContext(ctx, "finding the workspace for Astro's docs server", slog.String("env", id), slog.Any("error", err))
		return false
	}
	// The workspace is the agent's: IsAstro keeps its reads inside it.
	is, err := setup.IsAstro(env.Workspace)
	if err != nil {
		s.log.WarnContext(ctx, "reading whether the repository is an Astro site", slog.String("env", id), slog.Any("error", err))
	}
	return is
}

// fernMCP is Fern's MCP server as an agent in an agent box reaches it, and
// those for the repository: Cloudflare's for one that deploys there,
// Astro's docs for an Astro site.
type fernMCP struct {
	URL   string
	Token string // the box's, which the server takes as the bearer token
	// Cloudflare is the Cloudflare API token for Cloudflare's MCP server; ""
	// adds no server.
	Cloudflare string
	AstroDocs  bool // adds Astro's docs server
}

// t3MCPPath is where T3's server has its MCP endpoint.
const t3MCPPath = "/mcp"

// claude adds the MCP server to Claude's arguments, takes away Claude's own
// shell tools, and ignores the settings and MCP servers in the workspace:
// the box writes those, and they run code in the agent box, next to
// Claude's login.
func (m fernMCP) claude(req *boxexec.Request) error {
	fern := map[string]any{"type": "http", "url": m.URL}
	if m.Token != "" {
		fern["headers"] = map[string]string{"Authorization": "Bearer " + m.Token}
	}
	servers := map[string]any{"fern": fern}
	if m.Cloudflare != "" {
		servers["cloudflare"] = map[string]any{
			"type":    "http",
			"url":     cloudflareMCP,
			"headers": map[string]string{"Authorization": "Bearer " + m.Cloudflare},
		}
	}
	if m.AstroDocs {
		servers["astro-docs"] = map[string]any{"type": "http", "url": astroDocsMCP}
	}
	cfg, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return err
	}
	req.Argv = append(req.Argv, "--mcp-config", string(cfg), "--disallowedTools", strings.Join(noShell, ","),
		"--setting-sources=user")
	return nil
}

// openCodeConfig is the variable OpenCode reads settings from, as JSON, after
// its settings files.
const openCodeConfig = "OPENCODE_CONFIG_CONTENT"

// openCode changes the request of OpenCode's server, which T3 calls over
// HTTP. The server listens on the box's address, which is at addr, as T3
// can't reach the box's loopback. Its settings get the MCP server and lose
// the shell tool; a rule also refuses the tool, should a later settings file
// bring it back. Any other opencode command is left as it is.
func (m fernMCP) openCode(req *boxexec.Request, addr string) error {
	if len(req.Argv) < 2 || req.Argv[1] != "serve" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("the box's address: %w", err)
	}
	req.Argv = slices.DeleteFunc(slices.Clone(req.Argv), func(arg string) bool { return strings.HasPrefix(arg, "--hostname=") })
	req.Argv = append(req.Argv, "--hostname="+host)

	cfg := map[string]any{}
	req.Environ = slices.DeleteFunc(slices.Clone(req.Environ), func(v string) bool {
		content, ok := strings.CutPrefix(v, openCodeConfig+"=")
		if ok && json.Unmarshal([]byte(content), &cfg) != nil {
			cfg = map[string]any{} // not an object: OpenCode would ignore it too
		}
		return ok
	})
	mcp, _ := cfg["mcp"].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
	}
	servers, _ := mcp["servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	// Not in Code Mode, where only scripts could call it, and without OAuth,
	// which OpenCode would try if the token were refused.
	fern := map[string]any{"type": "remote", "url": m.URL, "oauth": false, "codemode": false}
	if m.Token != "" {
		fern["headers"] = map[string]string{"Authorization": "Bearer " + m.Token}
	}
	servers["fern"] = fern
	if m.Cloudflare != "" {
		servers["cloudflare"] = map[string]any{
			"type": "remote", "url": cloudflareMCP, "headers": map[string]string{"Authorization": "Bearer " + m.Cloudflare},
			"oauth": false,
		}
	}
	if m.AstroDocs {
		servers["astro-docs"] = map[string]any{"type": "remote", "url": astroDocsMCP, "oauth": false}
	}
	mcp["servers"] = servers
	cfg["mcp"] = mcp
	plugins, _ := cfg["plugins"].([]any)
	cfg["plugins"] = append(plugins, "-opencode.tool.shell")
	policies, _ := cfg["policies"].([]any)
	cfg["policies"] = append([]any{map[string]string{"action": "tool.use", "resource": "shell:*", "effect": "deny"}}, policies...)
	content, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	req.Environ = append(req.Environ, openCodeConfig+"="+string(content))
	return nil
}

// mcpURL returns where Claude in the agent box of ex reaches Fern's MCP
// server, and the token it must send, if any: through the box's own relay,
// which carries a call across a restart of the daemon and adds the box's
// token itself, so the agent is given none. An agent box still running from
// before the relay has none, and one from before 2026-11-12 has a relay that
// adds no token: their agent gets the token in its arguments, as it did.
func (s *Server) mcpURL(ctx context.Context, ex *actor.Exec) (url, token string) {
	relay, auth := boxexec.HasMCPRelay(ctx, s.boxes, ex.Addr, ex.Token)
	if !relay {
		return s.agentMCP, ex.Token
	}
	url = "http://127.0.0.1:" + strconv.Itoa(boxexec.MCPRelayPort)
	if auth {
		return url, ""
	}
	return url, ex.Token
}

// dialBox connects to environment id's box, waking it if needed, for a
// command open counts. A box seen earlier may be gone (a crash, a box stopped
// by hand): then it is woken once more.
func (s *Server) dialBox(ctx context.Context, id string, cancel context.CancelFunc, open opener) (*websocket.Conn, *actor.Exec, error) {
	var err error
	for try := range 2 {
		var ex *actor.Exec
		if ex, err = open(ctx, id, cancel); err != nil {
			return nil, nil, err
		}
		dialCtx, cancelDial := context.WithTimeout(ctx, dialTimeout)
		conn, dialErr := boxexec.Dial(dialCtx, "ws://"+ex.Addr+boxexec.Path, ex.Token, s.boxes)
		cancelDial()
		if dialErr == nil {
			return conn, ex, nil
		}
		err = dialErr
		if try > 0 {
			// The next command finds it and says so: a command counts one
			// death of the box, not two.
			ex.Close()
			break
		}
		ex.Dead()
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
	case errors.Is(err, actor.ErrTooManyForks):
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
