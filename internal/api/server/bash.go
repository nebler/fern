package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
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

// MCPHandler serves each environment's MCP server at /mcp/{id}, for its agent
// box's Claude. Its tool Bash runs a command in the environment's own box,
// where the services are, waking it first; the box sleeps bashLinger after
// the last. A command run in the background goes on after the call (jobs),
// and BashOutput and KillShell read and stop it. A request must carry the environment's box token, which
// only its boxes have.
func (s *Server) MCPHandler() http.Handler {
	mux := http.NewServeMux()
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return s.bashServer(r.PathValue("id"))
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	mux.HandleFunc("/mcp/{id}", func(w http.ResponseWriter, r *http.Request) {
		env, err := s.store.Get(r.Context(), r.PathValue("id"))
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if err != nil || env.BoxToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(env.BoxToken)) != 1 {
			// The same answer whether or not the environment exists.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
	return mux
}

func (s *Server) bashServer(id string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "fern", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "Bash",
		Description: "Runs a shell command in this thread's box, where its services (databases, servers) run, " +
			"in the workspace, and returns what it printed and its exit code. Use it as you would Bash. " +
			"Every command starts in the workspace: put a cd in the same command if you need another folder. " +
			"Run servers that must keep running, such as `npm run dev`, with run_in_background: they keep running " +
			"after your turn, until KillShell stops them or the box sleeps, 20 minutes after its last use.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in bashInput) (*mcp.CallToolResult, any, error) {
		if in.Background {
			jobID, err := s.startJob(ctx, id, in)
			if err != nil {
				return toolError(err), nil, nil
			}
			return toolText(fmt.Sprintf("Running in the background as %s: read its output with BashOutput, stop it with KillShell.", jobID)), nil, nil
		}
		// A command that couldn't run is the tool's error, for Claude to read,
		// not the protocol's.
		res := &mcp.CallToolResult{}
		out, code, err := s.bash(ctx, id, in)
		text := clip(out)
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
		Description: "Stops a background command started by Bash, with what it started, and returns its last output.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in killShellInput) (*mcp.CallToolResult, any, error) {
		text, err := s.killJob(ctx, id, in.ID)
		if err != nil {
			return toolError(err), nil, nil
		}
		return toolText(text), nil, nil
	})
	return srv
}

func toolText(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func toolError(err error) *mcp.CallToolResult {
	res := toolText(err.Error())
	res.IsError = true
	return res
}

// bash runs in's command in environment id's box and returns its output,
// stdout and stderr together, and exit code.
func (s *Server) bash(ctx context.Context, id string, in bashInput) (string, int, error) {
	if strings.TrimSpace(in.Command) == "" {
		return "", 0, errors.New("no command")
	}
	timeout := bashTimeout
	if in.Timeout > 0 {
		timeout = min(time.Duration(in.Timeout)*time.Millisecond, bashMaxTimeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, ex, err := s.dialBox(ctx, id, cancel, s.envs.Exec)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", 0, fmt.Errorf("no environment %s", id)
		}
		return "", 0, err
	}
	defer ex.CloseAndSleep(bashLinger)
	defer func() { _ = conn.CloseNow() }()
	// boxexec.Run writes stdout and stderr from one goroutine.
	var out bytes.Buffer
	code, err := boxexec.Run(ctx, conn, boxexec.Request{Argv: []string{"/bin/sh", "-c", in.Command}},
		boxexec.Streams{Stdout: &out, Stderr: &out})
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
	if len(out) <= maxOutput {
		return out
	}
	half := maxOutput / 2
	return out[:half] + fmt.Sprintf("\n[… %d bytes left out …]\n", len(out)-maxOutput) + out[len(out)-half:]
}
