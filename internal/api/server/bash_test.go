package server_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/api/server"
	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
)

// bearer adds an Authorization header to every request.
type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// newMCP serves environment demo's MCP endpoint, its box a real boxexec
// server, and returns the endpoint's URL, the fake driver and the actors.
func newMCP(t *testing.T) (string, *runsc.Fake, *actor.Group) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	mux := http.NewServeMux()
	mux.Handle(boxexec.Path, boxexec.NewServer(boxToken, t.TempDir(), nil, log))
	box := httptest.NewServer(mux)
	t.Cleanup(box.Close)
	drv := runsc.NewFake(box.Listener.Addr().String())
	st := newFakeStore()
	if _, err := st.Create(t.Context(), "demo", store.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	envs := actor.New(st, drv, actor.Config{Image: "box"}, notrace, prometheus.NewRegistry(), log)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = envs.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	api := server.New(st, drv, envs, "http://fern-daemon:7071", notrace, log)
	t.Cleanup(api.Close)
	srv := httptest.NewServer(api.MCPHandler())
	t.Cleanup(srv.Close)
	return srv.URL + "/mcp/demo", drv, envs
}

func connect(t *testing.T, url, token string) (*mcp.ClientSession, error) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return c.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer{token}}}, nil)
}

// Bash runs the command in the environment's box, waking it, and returns
// its output and exit code.
func TestBash(t *testing.T) {
	url, drv, _ := newMCP(t)
	s, err := connect(t, url, boxToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "Bash", Arguments: map[string]any{"command": "echo hi; echo oops >&2; exit 3"}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"hi", "oops", "[exit code 3]"} {
		if !strings.Contains(text, want) {
			t.Errorf("Bash answered %q, want %q in it", text, want)
		}
	}
	if res.IsError {
		t.Errorf("a command that failed is a result, not a tool error: %q", text)
	}
	states, _ := drv.States(t.Context())
	if states["demo"] != runsc.Running {
		t.Errorf("box %s after Bash, want it woken", states["demo"])
	}
}

// Without the environment's token, or for another environment, there is no
// endpoint.
func TestBashNeedsToken(t *testing.T) {
	url, _, _ := newMCP(t)
	for _, tc := range []struct{ name, url, token string }{
		{"wrong token", url, "nope"},
		{"no token", url, ""},
		{"another environment", strings.TrimSuffix(url, "demo") + "other", boxToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := connect(t, tc.url, tc.token)
			if err == nil {
				_ = s.Close()
				t.Error("connected, want refused")
			}
		})
	}
}

// jobID finds a background command's ID in Bash's answer.
var jobID = regexp.MustCompile(`bash_[0-9]+`)

// call calls tool with args and returns its text, failing the test if the
// tool answers with an error.
func call(t *testing.T, s *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError {
		t.Fatalf("%s %v: %s", tool, args, text)
	}
	return text
}

// output reads background command id's output until it holds want, and
// returns all of it.
func output(t *testing.T, s *mcp.ClientSession, id, want string) string {
	t.Helper()
	var all string
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(all, want) {
		if time.Now().After(deadline) {
			t.Fatalf("BashOutput %s: %q, want %q in it", id, all, want)
		}
		all += call(t, s, "BashOutput", map[string]any{"bash_id": id})
	}
	return all
}

// A command run in the background goes on after its call; BashOutput reads
// what it printed since the last read, and KillShell stops it.
func TestBashBackground(t *testing.T) {
	url, _, _ := newMCP(t)
	s, err := connect(t, url, boxToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	id := jobID.FindString(call(t, s, "Bash", map[string]any{"command": "echo started; sleep 100; echo never", "run_in_background": true}))
	got := output(t, s, id, "started")
	if !strings.Contains(got, "is running") {
		t.Errorf("BashOutput = %q, want it running", got)
	}
	got = call(t, s, "KillShell", map[string]any{"shell_id": id})
	if !strings.Contains(got, "is killed") || strings.Contains(got, "never") {
		t.Errorf("KillShell = %q, want it killed", got)
	}
	res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "BashOutput", Arguments: map[string]any{"bash_id": "bash_404"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("BashOutput of an unknown command isn't an error")
	}
}

// A background command that exits is reported with its exit code; one whose
// box sleeps is stopped.
func TestBashBackgroundEnds(t *testing.T) {
	url, _, envs := newMCP(t)
	s, err := connect(t, url, boxToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	start := func(command string) string {
		return jobID.FindString(call(t, s, "Bash", map[string]any{"command": command, "run_in_background": true}))
	}
	exits := start("echo bye; exit 4")
	output(t, s, exits, "exited with code 4")
	runs := start("echo up; sleep 100")
	output(t, s, runs, "up")
	if err := envs.Sleep(t.Context(), "demo"); err != nil {
		t.Fatal(err)
	}
	output(t, s, runs, "stopped: its box went to sleep")
}
