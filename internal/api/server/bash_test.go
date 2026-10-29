package server_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
// server, and returns the endpoint's URL and the fake driver.
func newMCP(t *testing.T) (string, *runsc.Fake) {
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
	srv := httptest.NewServer(server.New(st, drv, envs, "http://fern-daemon:7071", notrace, log).MCPHandler())
	t.Cleanup(srv.Close)
	return srv.URL + "/mcp/demo", drv
}

func connect(t *testing.T, url, token string) (*mcp.ClientSession, error) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return c.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer{token}}}, nil)
}

// Bash runs the command in the environment's box, waking it, and returns
// its output and exit code.
func TestBash(t *testing.T) {
	url, drv := newMCP(t)
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
	url, _ := newMCP(t)
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
