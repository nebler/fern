package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/store"
)

// The limits of one Bash call, as Claude's own Bash has them: two minutes
// unless asked for more, at most ten, and the output cut in the middle past
// maxOutput.
const (
	bashTimeout    = 2 * time.Minute
	bashMaxTimeout = 10 * time.Minute
	maxOutput      = 30_000
)

// bashLinger is how long the services' box stays up after the last Bash
// call: an agent's commands come in bursts, and one within it pays no wake.
const bashLinger = 5 * time.Second

// bashInput is a Bash call.
type bashInput struct {
	Command     string `json:"command" jsonschema:"the command to run, with /bin/sh, in the workspace"`
	Timeout     int    `json:"timeout,omitempty" jsonschema:"milliseconds before the command is stopped; 120000 if 0, at most 600000"`
	Description string `json:"description,omitempty" jsonschema:"what the command does, in a few words"`
	// Background keeps the command running after the call, as a job.
	Background bool `json:"run_in_background,omitempty" jsonschema:"keep the command running after this call returns, such as a dev server; read its output with BashOutput, stop it with KillShell"`
}

// bashOutputInput and killShellInput name a background command.
type (
	bashOutputInput struct {
		ID string `json:"bash_id" jsonschema:"the ID Bash gave the background command"`
	}
	killShellInput struct {
		ID string `json:"shell_id" jsonschema:"the ID Bash gave the background command"`
	}
)

// The fork tools' calls.
type (
	forkInput struct {
		Count int `json:"count" jsonschema:"how many forks to make, from 1 to 8"`
	}
	forkExecInput struct {
		Env     string `json:"env" jsonschema:"the fork's ID, as env_fork gave it"`
		Command string `json:"command" jsonschema:"the command to run, with /bin/sh, in the fork's workspace"`
		Timeout int    `json:"timeout,omitempty" jsonschema:"milliseconds before the command is stopped; 120000 if 0, at most 600000"`
	}
	forkDeleteInput struct {
		Env string `json:"env" jsonschema:"the fork's ID, as env_fork gave it"`
	}
)

// MCPHandler serves each environment's MCP server at /mcp/{id}, for its agent
// box's Claude. Its tool Bash runs a command in the environment's own box,
// where the services are, waking it first; the box sleeps bashLinger after
// the last. A command run in the background goes on after the call (jobs),
// and BashOutput and KillShell read and stop it. A request must carry the token
// of the environment's running agent box, which the relay in that box adds.
func (s *Server) MCPHandler() http.Handler {
	mux := http.NewServeMux()
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return s.bashServer(r.PathValue("id"))
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	if s.T3 != "" {
		mux.Handle("/t3/{id}", s.t3Handler())
	}
	mux.HandleFunc("/mcp/{id}", func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !s.hasBoxToken(r.PathValue("id"), token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Claude giving up on a call ends its command. The MCP library's context
		// for a tool isn't the request's.
		if key := r.Header.Get(boxexec.CallKeyHeader); key != "" {
			stop := context.AfterFunc(r.Context(), func() {
				if c, ok := s.calls.Load(key); ok && !s.stopping() {
					if c, ok := c.(*call); ok {
						c.cancel()
					}
				}
			})
			defer stop()
		}
		answer := &heldAnswer{header: http.Header{}}
		h.ServeHTTP(answer, r)
		// A call cut off by the stop mustn't tell Claude it failed: the
		// connection breaks, with no answer, and the agent box's relay sends
		// the call again to the next daemon.
		if s.stopping() {
			panic(http.ErrAbortHandler)
		}
		answer.send(w)
	})
	return mux
}

// hasBoxToken says whether token is the token of environment id's running
// agent box, the only box these routes are for: the same answer whether the
// token is wrong, the box asleep or the environment unknown. The token was
// made when that box started and ends with it, and the box that runs the
// repository's code has another.
func (s *Server) hasBoxToken(id, token string) bool {
	// The ID becomes a path in the driver.
	if store.CheckEnvID(id) != nil {
		return false
	}
	want, err := s.driver.Token(store.AgentBox(id))
	return err == nil && want != "" && subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1
}

// t3Handler passes an agent box's calls to T3's own MCP server on to T3, at
// /t3/{id}: T3 serves it on the node's loopback, which a box can't reach.
// Only that one path of T3's is reachable this way, and a call needs two
// tokens: the box's, which its fern-init adds and the daemon checks and takes off, and T3's for
// the thread, in Authorization, which T3 checks and only Claude's arguments
// hold. The box that runs the repository's code has the first and not the
// second.
func (s *Server) t3Handler() http.Handler {
	target, err := url.Parse(s.T3 + t3MCPPath)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme, r.Out.URL.Host, r.Out.URL.Path, r.Out.URL.RawPath = target.Scheme, target.Host, target.Path, ""
			r.Out.Host = target.Host
			r.Out.Header.Del(boxexec.BoxTokenHeader)
		},
		FlushInterval: -1, // answers may be streams
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err != nil {
			http.Error(w, "T3's address: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if !s.hasBoxToken(r.PathValue("id"), r.Header.Get(boxexec.BoxTokenHeader)) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

// heldAnswer holds an answer until its call has ended, for a daemon that may
// be stopping by then. Answers are JSON (StreamableHTTPOptions), so nothing is
// held for long.
type heldAnswer struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (h *heldAnswer) Header() http.Header         { return h.header }
func (h *heldAnswer) WriteHeader(code int)        { h.code = code }
func (h *heldAnswer) Write(p []byte) (int, error) { return h.body.Write(p) }

// send passes the answer on to w.
func (h *heldAnswer) send(w http.ResponseWriter) {
	for name, values := range h.header {
		w.Header()[name] = values
	}
	if h.code != 0 {
		w.WriteHeader(h.code)
	}
	_, _ = w.Write(h.body.Bytes())
}

func (s *Server) bashServer(id string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "fern", Version: "1"}, &mcp.ServerOptions{
		SchemaCache: s.schemas,
		// Tools only, and a list that never changes: the server sends nothing
		// by itself. Saying it might (the SDK's default) has Claude open a
		// stream to listen on, which this endpoint holds like any answer, so
		// every Claude waited 25 s for it at its start before its first turn.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name: "Bash",
		Meta: alwaysLoad(),
		Description: "Runs a shell command in this thread's box, where its services (databases, servers) run, " +
			"in the workspace, and returns what it printed and its exit code. Use it as you would Bash. " +
			"Every command starts in the workspace: put a cd in the same command if you need another folder. " +
			"Run servers that must keep running, such as `npm run dev`, with run_in_background: they keep running " +
			"after your turn, until KillShell stops them or the box sleeps, 20 minutes after its last use.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in bashInput) (*mcp.CallToolResult, any, error) {
		if in.Background {
			jobID, err := s.startJob(ctx, id, in, callKey(req))
			if err != nil {
				return toolError(err), nil, nil
			}
			return toolText(fmt.Sprintf("Running in the background as %s: read its output with BashOutput, stop it with KillShell.", jobID)), nil, nil
		}
		// A command that couldn't run is the tool's error, for Claude to read,
		// not the protocol's.
		res := &mcp.CallToolResult{}
		out, code, err := s.bash(ctx, id, in, callKey(req))
		text := out
		switch {
		case err != nil:
			res.IsError, text = true, err.Error()
		case code != 0:
			text += fmt.Sprintf("\n[exit code %d]", code)
		}
		res.Content = []mcp.Content{&mcp.TextContent{Text: text}}
		return res, nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "BashOutput",
		Meta:        alwaysLoad(),
		Description: "Returns what a background command started by Bash printed since the last call, and whether it still runs.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in bashOutputInput) (*mcp.CallToolResult, any, error) {
		text, err := s.readJob(ctx, id, in.ID)
		if err != nil {
			return toolError(err), nil, nil
		}
		return toolText(text), nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "KillShell",
		Meta:        alwaysLoad(),
		Description: "Stops a background command started by Bash, with what it started, and returns its last output.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in killShellInput) (*mcp.CallToolResult, any, error) {
		text, err := s.killJob(ctx, id, in.ID)
		if err != nil {
			return toolError(err), nil, nil
		}
		return toolText(text), nil, nil
	})
	s.addCommitTool(srv, id)
	s.addForkTools(srv, id)
	s.addCloudflareTools(srv, id)
	return srv
}

// alwaysLoad marks a tool whose definition Claude gets with its prompt.
// Claude Code otherwise lists an MCP server's tools by name and fetches a
// definition at its first use: for the shell that stands in for Claude's
// own, that is a step at the start of every session, which Claude also
// reports as if its shell were missing.
func alwaysLoad() mcp.Meta { return mcp.Meta{"anthropic/alwaysLoad": true} }

// addForkTools adds to srv the tools with which environment id's Claude
// forks it, and runs commands in and deletes its forks, and only its own.
func (s *Server) addForkTools(srv *mcp.Server, id string) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "env_fork",
		Description: "Copies this thread's box as it is now into new ones, forks: the files, the installed dependencies " +
			"and the databases' data. Each fork runs its own services (its own Postgres on the same port, say), so " +
			"forks can run tests, migrations or experiments at the same time without touching each other or this " +
			"thread. Returns the forks' IDs: run commands in them with env_exec, and delete them with env_delete " +
			"when done. A fork's changes never come back to this thread. Forks sleep when idle.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in forkInput) (*mcp.CallToolResult, any, error) {
		if in.Count == 0 {
			in.Count = 1
		}
		ids, err := s.envs.Fork(ctx, id, "", in.Count)
		if err != nil {
			return toolError(err), nil, nil
		}
		return toolText("Forks: " + strings.Join(ids, ", ")), nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name: "env_exec",
		Description: "Runs a shell command in a fork of this thread (env_fork), in its workspace, with its services, " +
			"and returns what it printed and its exit code. Calls for different forks run at the same time.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in forkExecInput) (*mcp.CallToolResult, any, error) {
		if err := ownFork(id, in.Env); err != nil {
			return toolError(err), nil, nil
		}
		out, code, err := s.bash(ctx, in.Env, bashInput{Command: in.Command, Timeout: in.Timeout}, callKey(req))
		if err != nil {
			return toolError(err), nil, nil
		}
		text := out
		if code != 0 {
			text += fmt.Sprintf("\n[exit code %d]", code)
		}
		return toolText(text), nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "env_delete",
		Description: "Deletes a fork of this thread (env_fork), its box and its files.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in forkDeleteInput) (*mcp.CallToolResult, any, error) {
		if err := ownFork(id, in.Env); err != nil {
			return toolError(err), nil, nil
		}
		if err := s.envs.Delete(ctx, in.Env); err != nil {
			return toolError(err), nil, nil
		}
		return toolText("Deleted " + in.Env + "."), nil, nil
	})
}

// ownFork returns an error unless env is a fork of environment id: its
// Claude reaches no other environment.
func ownFork(id, env string) error {
	if parent, _, ok := store.ForkOf(env); !ok || parent != id {
		return fmt.Errorf("%q isn't a fork of this thread: env_fork gives their IDs", env)
	}
	return nil
}

func toolText(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func toolError(err error) *mcp.CallToolResult {
	res := toolText(err.Error())
	res.IsError = true
	return res
}

// call is a running Bash command that its call's key can cancel.
type call struct{ cancel context.CancelFunc }

// callKey returns the name the agent box's relay gave this call, the same
// each time it sends it (boxexec.MCPRelay), or "" if it came another way.
func callKey(req *mcp.CallToolRequest) string {
	if req == nil || req.Extra == nil {
		return ""
	}
	return req.Extra.Header.Get(boxexec.CallKeyHeader)
}

// bash runs in's command in environment id's box and returns its output,
// stdout and stderr together, clipped to maxOutput, and exit code. A key
// names the call: the command runs as a session of that name, so a command
// running when the daemon restarts runs on, and the call sent again finds it
// instead of running it twice.
func (s *Server) bash(ctx context.Context, id string, in bashInput, key string) (string, int, error) {
	if strings.TrimSpace(in.Command) == "" {
		return "", 0, errors.New("no command")
	}
	timeout := bashTimeout
	if in.Timeout > 0 {
		timeout = min(time.Duration(in.Timeout)*time.Millisecond, bashMaxTimeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if key != "" {
		// Its own record: a call sent again under the same key replaces it,
		// and this one's end must not remove that one's.
		c := &call{cancel: cancel}
		s.calls.Store(key, c)
		defer s.calls.CompareAndDelete(key, c)
	}
	conn, ex, err := s.dialBox(ctx, id, cancel, s.envs.Exec)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", 0, fmt.Errorf("no environment %s", id)
		}
		return "", 0, err
	}
	defer ex.CloseAndSleep(bashLinger)
	defer func() { _ = conn.CloseNow() }()
	// Both streams are written from one goroutine.
	out := boxexec.NewClipped(maxOutput)
	req := boxexec.Request{Argv: []string{"/bin/sh", "-c", in.Command}}
	streams := boxexec.Streams{Stdout: out, Stderr: out}
	var code int
	if key != "" {
		req.Session = key
		code, err = boxexec.RunSession(ctx, conn, req, streams, boxexec.Resume{Abandon: s.stopping})
	} else {
		code, err = boxexec.Run(ctx, conn, req, streams)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out.String() + fmt.Sprintf("\n[stopped after %s]", timeout), 124, nil
	}
	if err != nil {
		return out.String(), 0, err
	}
	return out.String(), code, nil
}

// clip keeps the start and end of output longer than maxOutput.
func clip(out string) string {
	c := boxexec.NewClipped(maxOutput)
	_, _ = c.Write([]byte(out))
	return c.String()
}
