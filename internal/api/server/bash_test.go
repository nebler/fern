package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
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

// demoBox is environment demo, its box a real boxexec server with jobs, run
// by a fake driver and actors.
type demoBox struct {
	dir  string // where the box runs commands
	st   *fakeStore
	drv  *runsc.Fake
	envs *actor.Group
	log  *slog.Logger
}

func newDemoBox(t *testing.T) demoBox {
	t.Helper()
	return newDemoBoxWith(t, false)
}

// agentUp starts environment id's agent box in drv, as a thread's Claude
// has: its token is the one the MCP endpoint takes.
func agentUp(t testing.TB, drv *runsc.Fake, id string) {
	t.Helper()
	if _, err := drv.Wake(t.Context(), runsc.Box{ID: store.AgentBox(id), Kind: runsc.Agent}); err != nil {
		t.Fatal(err)
	}
}

// newDemoBoxWith is newDemoBox whose box, with relay, says it has the MCP
// relay that adds the box's token, as fern-init in an agent box does.
func newDemoBoxWith(t *testing.T, relay bool) demoBox {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	mux := http.NewServeMux()
	dir := t.TempDir()
	if relay {
		mux.Handle("GET "+boxexec.MCPRelayPath, boxexec.MCPRelayCheck(boxToken))
	}
	mux.Handle(boxexec.Path, boxexec.NewServer(boxToken, dir, nil, log))
	jobs := boxexec.NewJobs(boxToken, dir, nil, log)
	jobs.Register(mux)
	t.Cleanup(jobs.Close)
	box := httptest.NewServer(mux)
	t.Cleanup(box.Close)
	drv := runsc.NewFake(box.Listener.Addr().String())
	st := newFakeStore()
	if _, err := st.Create(t.Context(), "demo", store.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	agentUp(t, drv, "demo")
	envs := actor.New(st, drv, actor.Config{Image: "box"}, notrace, prometheus.NewRegistry(), log)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = envs.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return demoBox{dir: dir, st: st, drv: drv, envs: envs, log: log}
}

// serve serves the box's environment's MCP endpoint from a new API server,
// and returns the endpoint's URL and the server.
func (b demoBox) serve(t *testing.T) (string, *server.Server) {
	t.Helper()
	api := server.New(b.st, b.drv, b.envs, "http://fern-daemon:7071", notrace, b.log)
	t.Cleanup(api.Close)
	srv := httptest.NewServer(api.MCPHandler())
	t.Cleanup(srv.Close)
	return srv.URL + "/mcp/demo", api
}

// newMCP serves environment demo's MCP endpoint, its box a real boxexec
// server, and returns the endpoint's URL, the fake driver and the actors.
func newMCP(t *testing.T) (string, *runsc.Fake, *actor.Group) {
	t.Helper()
	b := newDemoBox(t)
	url, _ := b.serve(t)
	return url, b.drv, b.envs
}

func connect(t *testing.T, url, token string) (*mcp.ClientSession, error) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return c.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer{token}}}, nil)
}

// The server says it has tools and sends nothing by itself: a server that
// says its list may change has Claude open a stream to listen on, which the
// endpoint holds like any answer, and Claude waited 25 s for it at every
// start.
func TestMCPSendsNothingItself(t *testing.T) {
	url, _, _ := newMCP(t)
	s, err := connect(t, url, boxToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	caps := s.InitializeResult().Capabilities
	if caps.Tools == nil || caps.Tools.ListChanged || caps.Prompts != nil || caps.Resources != nil {
		t.Errorf("capabilities = %+v (tools %+v), want tools alone, with a list that doesn't change", caps, caps.Tools)
	}
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

// boxTokens is a fake driver whose boxes each have a token of their own, as
// a real driver's do.
type boxTokens struct {
	*runsc.Fake
	tokens map[string]string
}

func (d boxTokens) Token(id string) (string, error) {
	if _, err := d.Fake.Token(id); err != nil {
		return "", err
	}
	return d.tokens[id], nil
}

// The endpoint takes the token of the environment's running agent box, made
// when that box started. The box that runs the repository's code has
// another, which opens nothing here, and a token ends with its box.
func TestMCPTakesTheAgentBoxTokenAlone(t *testing.T) {
	b := newDemoBox(t)
	drv := boxTokens{b.drv, map[string]string{"demo": boxToken, "demo.agent": "the-agent-box"}}
	api := server.New(b.st, drv, b.envs, "http://fern-daemon:7071", notrace, b.log)
	t.Cleanup(api.Close)
	srv := httptest.NewServer(api.MCPHandler())
	t.Cleanup(srv.Close)
	url := srv.URL + "/mcp/demo"

	s, err := connect(t, url, "the-agent-box")
	if err != nil {
		t.Fatalf("with the agent box's token: %v", err)
	}
	_ = s.Close()
	if s, err := connect(t, url, boxToken); err == nil {
		_ = s.Close()
		t.Error("the token of the box that runs the code opened the endpoint")
	}
	// Claude's box sleeps: its token is gone with it.
	if err := b.drv.Sleep(t.Context(), "demo.agent"); err != nil {
		t.Fatal(err)
	}
	if s, err := connect(t, url, "the-agent-box"); err == nil {
		_ = s.Close()
		t.Error("the token of an agent box that has stopped opened the endpoint")
	}
	for _, id := range []string{"../demo", "demo.agent", "DEMO"} {
		if s, err := connect(t, srv.URL+"/mcp/"+id, "the-agent-box"); err == nil {
			_ = s.Close()
			t.Errorf("environment %q opened the endpoint", id)
		}
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
var jobID = regexp.MustCompile(`bash_[a-z0-9]+`)

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
	url, drv, envs := newMCP(t)
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
	// The sleep stopped Claude's box too. Its next message starts it again.
	agentUp(t, drv, "demo")
	output(t, s, runs, "stopped: its box went to sleep")
}

// keyed sends every request as the agent box's relay does a call: under key.
type keyed struct{ token, key string }

func (k keyed) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+k.token)
	r.Header.Set(boxexec.CallKeyHeader, k.key)
	return http.DefaultTransport.RoundTrip(r)
}

func connectKeyed(t *testing.T, url, key string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	s, err := c.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: keyed{boxToken, key}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// A background call sent again under its key, as the relay does when the
// daemon restarted under it, names the job the first one started.
func TestBashBackgroundSentAgain(t *testing.T) {
	b := newDemoBox(t)
	url, _ := b.serve(t)
	s := connectKeyed(t, url, "ABCDEFGH234567ABCDEFGH2345")
	marker := filepath.Join(t.TempDir(), "runs")
	args := map[string]any{"command": "echo run >> " + marker + "; sleep 100", "run_in_background": true}
	first := jobID.FindString(call(t, s, "Bash", args))
	if first != "bash_abcdefgh" {
		t.Fatalf("the job's ID = %q, want it made from the call's key", first)
	}
	if again := jobID.FindString(call(t, s, "Bash", args)); again != first {
		t.Errorf("the call sent again answered %q, want %q", again, first)
	}
	// And to a daemon that never heard of it, which finds it in the box.
	url, _ = b.serve(t)
	s = connectKeyed(t, url, "ABCDEFGH234567ABCDEFGH2345")
	if again := jobID.FindString(call(t, s, "Bash", args)); again != first {
		t.Errorf("the call sent to a new daemon answered %q, want %q", again, first)
	}
	if got := call(t, s, "BashOutput", map[string]any{"bash_id": first}); !strings.Contains(got, "is running") {
		t.Errorf("BashOutput from the new daemon = %q, want it running", got)
	}
	var runs int
	for deadline := time.Now().Add(20 * time.Second); runs == 0 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		data, _ := os.ReadFile(marker)
		runs = strings.Count(string(data), "run")
	}
	if runs != 1 {
		t.Errorf("the command ran %d times, want 1", runs)
	}
}

// A key that isn't the relay's is refused, and the daemon lives.
func TestBashBackgroundBadKey(t *testing.T) {
	url, _, _ := newMCP(t)
	for _, key := range []string{"short", "../../etc/passwd", "ÄÖÜäöüßß-and-more"} {
		s := connectKeyed(t, url, key)
		res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "Bash", Arguments: map[string]any{"command": "true", "run_in_background": true}})
		if err != nil {
			t.Fatalf("key %q: %v", key, err)
		}
		if !res.IsError {
			t.Errorf("key %q: a background command started", key)
		}
	}
}

// A background command runs on when the daemon stops, and the next daemon
// adopts it: BashOutput and KillShell reach it again.
func TestBashBackgroundAdopted(t *testing.T) {
	b := newDemoBox(t)
	url, api := b.serve(t)
	s, err := connect(t, url, boxToken)
	if err != nil {
		t.Fatal(err)
	}
	id := jobID.FindString(call(t, s, "Bash", map[string]any{"command": "echo up; sleep 100", "run_in_background": true}))
	output(t, s, id, "up")
	_ = s.Close()
	api.Close() // the daemon stops

	url, api = b.serve(t)
	api.AdoptJobs(t.Context())
	s, err = connect(t, url, boxToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if got := call(t, s, "BashOutput", map[string]any{"bash_id": id}); !strings.Contains(got, "is running") {
		t.Errorf("BashOutput after a new daemon = %q, want it running", got)
	}
	if got := call(t, s, "KillShell", map[string]any{"shell_id": id}); !strings.Contains(got, "is killed") {
		t.Errorf("KillShell after a new daemon = %q, want it killed", got)
	}
}

// Claude forks its environment, runs commands in the forks at the same time
// and deletes them, and reaches no other environment so.
func TestForkTools(t *testing.T) {
	b := newDemoBox(t)
	url, _ := b.serve(t)
	s, err := connect(t, url, boxToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if got := call(t, s, "env_fork", map[string]any{"count": 3}); got != "Forks: demo-fork-1, demo-fork-2, demo-fork-3" {
		t.Errorf("env_fork answered %q", got)
	}
	var wg sync.WaitGroup
	outs := make([]string, 3)
	for i := range outs {
		wg.Go(func() {
			args := map[string]any{"env": fmt.Sprintf("demo-fork-%d", i+1), "command": "echo ran; exit 2"}
			res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "env_exec", Arguments: args})
			if err != nil {
				outs[i] = err.Error()
				return
			}
			outs[i] = res.Content[0].(*mcp.TextContent).Text
		})
	}
	wg.Wait()
	for i, out := range outs {
		if out != "ran\n\n[exit code 2]" {
			t.Errorf("env_exec in demo-fork-%d answered %q", i+1, out)
		}
	}
	if states, _ := b.drv.States(t.Context()); states["demo-fork-2"] != runsc.Running || states["demo"] == runsc.Running {
		t.Errorf("after env_exec in the forks: boxes %v, want the forks' woken and not demo's", states)
	}
	for _, env := range []string{"demo", "other-fork-1", "demo-fork-x"} {
		res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "env_exec", Arguments: map[string]any{"env": env, "command": "true"}})
		if err != nil || !res.IsError {
			t.Errorf("env_exec in %s: %v, %v, want a tool error", env, res, err)
		}
	}
	call(t, s, "env_delete", map[string]any{"env": "demo-fork-1"})
	if _, err := b.st.Get(t.Context(), "demo-fork-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get(demo-fork-1) after env_delete: error = %v, want ErrNotFound", err)
	}
}

// A command's output can be far more than the daemon should hold: the answer,
// and what it keeps meanwhile, stay small.
func TestBashOutputBounded(t *testing.T) {
	url, _, _ := newMCP(t)
	s, err := connect(t, url, boxToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	text := call(t, s, "Bash", map[string]any{"command": "echo first; head -c 200000000 /dev/zero | tr '\\0' x; echo; echo last"})
	if len(text) > 31_000 {
		t.Errorf("Bash answered %d bytes", len(text))
	}
	for _, want := range []string{"first", "last", "bytes left out"} {
		if !strings.Contains(text, want) {
			t.Errorf("Bash's answer has no %q", want)
		}
	}
}

// swapHandler is a daemon's address that a new daemon can take over.
type swapHandler struct{ h atomic.Pointer[http.Handler] }

func (s *swapHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*s.h.Load()).ServeHTTP(w, r)
}

func (s *swapHandler) set(h http.Handler) { s.h.Store(&h) }

// The daemon restarts during a Bash call. Claude, through its box's relay,
// sees nothing: the same call gets the command's whole output, from a command
// that ran once and was never interrupted.
func TestBashSurvivesDaemonRestart(t *testing.T) {
	b := newDemoBox(t)
	marker := filepath.Join(t.TempDir(), "runs")

	stopping := make(chan struct{})
	first := server.New(b.st, b.drv, b.envs, "http://fern-daemon:7071", notrace, b.log)
	first.Stopping = stopping
	t.Cleanup(first.Close)
	swap := &swapHandler{}
	swap.set(first.MCPHandler())
	daemon := httptest.NewServer(swap)
	t.Cleanup(daemon.Close)
	relay := httptest.NewServer(boxexec.NewMCPRelay(daemon.URL, boxToken, 30*time.Second))
	t.Cleanup(relay.Close)

	s, err := connect(t, relay.URL+"/mcp/demo", boxToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	script := "echo run >> " + marker + "; echo before; while [ ! -e " + marker + ".go ]; do sleep 0.02; done; echo after; exit 4"
	answer := make(chan string, 1)
	go func() {
		res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "Bash", Arguments: map[string]any{"command": script}})
		if err != nil {
			answer <- "error: " + err.Error()
			return
		}
		answer <- res.Content[0].(*mcp.TextContent).Text
	}()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if data, _ := os.ReadFile(marker); len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the command didn't start")
		}
	}

	// The first daemon stops, and its connections break.
	close(stopping)
	daemon.CloseClientConnections()
	second := server.New(b.st, b.drv, b.envs, "http://fern-daemon:7071", notrace, b.log)
	t.Cleanup(second.Close)
	swap.set(second.MCPHandler())
	if err := os.WriteFile(marker+".go", nil, 0o644); err != nil {
		t.Fatal(err)
	}

	text := <-answer
	for _, want := range []string{"after", "[exit code 4]"} {
		if !strings.Contains(text, want) {
			t.Errorf("Bash answered %q, want %q in it", text, want)
		}
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "run"); got != 1 {
		t.Errorf("the command ran %d times, want 1", got)
	}
}

// A call cut off for any other reason ends its command.
func TestBashCancelledCallKillsCommand(t *testing.T) {
	url, _, _ := newMCP(t)
	relay := httptest.NewServer(boxexec.NewMCPRelay(strings.TrimSuffix(url, "/mcp/demo"), boxToken, 30*time.Second))
	t.Cleanup(relay.Close)
	pidfile := filepath.Join(t.TempDir(), "pid")
	// As Claude gives up on a call: its request goes away.
	call, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
		"name": "Bash", "arguments": map[string]any{"command": "echo $$ > " + pidfile + "; exec sleep 300"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, relay.URL+"/mcp/demo", bytes.NewReader(call))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+boxToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			t.Errorf("the call answered %s", resp.Status)
		}
	}()
	var pid int
	for deadline := time.Now().Add(20 * time.Second); pid == 0; time.Sleep(5 * time.Millisecond) {
		data, _ := os.ReadFile(pidfile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		if time.Now().After(deadline) {
			t.Fatal("the command didn't start")
		}
	}
	cancel()
	<-done
	for deadline := time.Now().Add(20 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the command outlived its call")
		}
	}
}

// With a Cloudflare token on the node, an agent in the agent box of a
// repository that deploys to Cloudflare gets Cloudflare's MCP server too; one
// in another repository doesn't, nor does any agent without a token.
func TestAgentStartCloudflare(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\"\nprintf '%s' \"$OPENCODE_CONFIG_CONTENT\"\n"
	for _, name := range []string{"claude", "opencode"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	b := newDemoBox(t)
	cf := t.TempDir()
	for name, data := range map[string]string{"wrangler.jsonc": "{}", "package.json": `{"dependencies": {"astro": "7"}}`} {
		if err := os.WriteFile(filepath.Join(cf, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.st.Create(t.Context(), "cf", store.CreateOptions{Workspace: cf}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		env   string
		token string
		want  bool
	}{
		{"a Cloudflare repository", "cf", "cf-token", true},
		{"another repository", "demo", "cf-token", false},
		{"no token on the node", "cf", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := server.New(b.st, b.drv, b.envs, "http://fern-daemon:7071", notrace, b.log)
			t.Cleanup(api.Close)
			api.Cloudflare = func(context.Context) (string, error) { return tt.token, nil }
			c := serveSocket(t, api.Handler())
			for _, agent := range []boxexec.Request{
				{Argv: []string{filepath.Join(bin, "claude"), "-p"}},
				{Argv: []string{filepath.Join(bin, "opencode"), "serve"}},
			} {
				var stdout bytes.Buffer
				if _, err := c.ExecAgent(t.Context(), tt.env, agent, boxexec.Streams{Stdout: &stdout, Stderr: io.Discard}); err != nil {
					t.Fatalf("ExecAgent(%s) error: %v", filepath.Base(agent.Argv[0]), err)
				}
				got := strings.Contains(stdout.String(), "https://mcp.cloudflare.com/mcp") &&
					strings.Contains(stdout.String(), "Bearer cf-token")
				if got != tt.want {
					t.Errorf("%s has Cloudflare's MCP server: %t, want %t\n%s", filepath.Base(agent.Argv[0]), got, tt.want, stdout.String())
				}
				// cf is an Astro site, demo isn't; Astro's docs need no token.
				if got := strings.Contains(stdout.String(), "https://mcp.docs.astro.build/mcp"); got != (tt.env == "cf") {
					t.Errorf("%s has Astro's docs server: %t, want %t", filepath.Base(agent.Argv[0]), got, tt.env == "cf")
				}
			}
		})
	}
}

// T3 serves its own MCP server on the node's loopback, which a box can't
// reach: the daemon passes a call with the box's token on to T3's /mcp, with
// T3's own token and without the box's, and refuses any other.
func TestT3MCP(t *testing.T) {
	type seen struct{ path, auth, box string }
	calls := make(chan seen, 1)
	t3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- seen{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get(boxexec.BoxTokenHeader)}
		_, _ = io.WriteString(w, "from t3")
	}))
	defer t3.Close()

	b := newDemoBox(t)
	api := server.New(b.st, b.drv, b.envs, "http://fern-daemon:7071", notrace, b.log)
	t.Cleanup(api.Close)
	api.T3 = t3.URL
	daemon := httptest.NewServer(api.MCPHandler())
	defer daemon.Close()
	call := func(env, box string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, daemon.URL+"/t3/"+env, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer t3-token")
		req.Header.Set(boxexec.BoxTokenHeader, box)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, body := call("demo", boxToken); code != http.StatusOK || body != "from t3" {
		t.Errorf("a call with the box's token = %d %q, want T3's answer", code, body)
	}
	if got, want := <-calls, (seen{"/mcp", "Bearer t3-token", ""}); got != want {
		t.Errorf("T3 saw %+v, want %+v", got, want)
	}
	for _, tt := range []struct{ name, env, box string }{
		{"another token", "demo", "guess"}, {"no token", "demo", ""}, {"no such environment", "none", boxToken},
	} {
		if code, _ := call(tt.env, tt.box); code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", tt.name, code)
		}
	}
	select {
	case got := <-calls:
		t.Errorf("a refused call reached T3: %+v", got)
	default:
	}
}

// fakeSecrets records the secrets cloudflare_connect sets.
type fakeSecrets struct {
	mu  sync.Mutex
	set map[string]string // owner/repo/secret to value
}

func (f *fakeSecrets) SetSecret(_ context.Context, owner, repo, secret, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set[owner+"/"+repo+"/"+secret] = value
	return nil
}

// cloudflare_connect sets the repository's CLOUDFLARE_API_TOKEN secret to
// the node's token, for the repository the box's git names; a repository
// that doesn't deploy to Cloudflare, or one that isn't on GitHub, is refused.
func TestCloudflareConnect(t *testing.T) {
	tests := []struct {
		name     string
		remote   string // "" for none
		wrangler bool
		want     map[string]string
		wantErr  string
	}{
		{"https", "https://github.com/owner/app.git", true, map[string]string{"owner/app/CLOUDFLARE_API_TOKEN": "cf-1"}, ""},
		{"ssh", "git@github.com:owner/app.git", true, map[string]string{"owner/app/CLOUDFLARE_API_TOKEN": "cf-1"}, ""},
		{"not cloudflare", "https://github.com/owner/app", false, map[string]string{}, "doesn't deploy to Cloudflare"},
		{"not github", "https://gitlab.com/owner/app.git", true, map[string]string{}, "isn't a GitHub repository"},
		{"no remote", "", true, map[string]string{}, "no remote"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newDemoBox(t)
			git := func(args ...string) {
				t.Helper()
				if out, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", b.dir}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
			git("init", "-q")
			if tt.remote != "" {
				git("remote", "add", "origin", tt.remote)
			}
			if tt.wrangler {
				if err := os.WriteFile(filepath.Join(b.dir, "wrangler.jsonc"), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := b.st.Create(t.Context(), "cf", store.CreateOptions{Workspace: b.dir}); err != nil {
				t.Fatal(err)
			}
			agentUp(t, b.drv, "cf")
			api := server.New(b.st, b.drv, b.envs, "http://fern-daemon:7071", notrace, b.log)
			t.Cleanup(api.Close)
			api.Cloudflare = func(context.Context) (string, error) { return "cf-1", nil }
			secrets := &fakeSecrets{set: make(map[string]string)}
			api.GitHub = secrets
			srv := httptest.NewServer(api.MCPHandler())
			t.Cleanup(srv.Close)
			s, err := connect(t, srv.URL+"/mcp/cf", boxToken)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "cloudflare_connect", Arguments: map[string]any{}})
			if err != nil {
				t.Fatal(err)
			}
			text := res.Content[0].(*mcp.TextContent).Text
			if tt.wantErr == "" && res.IsError || tt.wantErr != "" && (!res.IsError || !strings.Contains(text, tt.wantErr)) {
				t.Errorf("cloudflare_connect = %q (error %t), want error %q", text, res.IsError, tt.wantErr)
			}
			if strings.Contains(text, "cf-1") {
				t.Errorf("cloudflare_connect told Claude the token: %q", text)
			}
			if diff := cmp.Diff(tt.want, secrets.set); diff != "" {
				t.Errorf("secrets (-want +got):\n%s", diff)
			}
		})
	}
}

// The shell's tools come with Claude's prompt; the others are fetched at
// their first use, as Claude Code does with an MCP server's tools.
func TestShellToolsAlwaysLoad(t *testing.T) {
	url, _ := newDemoBox(t).serve(t)
	s, err := connect(t, url, boxToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	tools, err := s.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tool := range tools.Tools {
		if tool.Meta["anthropic/alwaysLoad"] == true {
			got = append(got, tool.Name)
		}
	}
	slices.Sort(got)
	if diff := cmp.Diff([]string{"Bash", "BashOutput", "KillShell"}, got); diff != "" {
		t.Errorf("tools always loaded (-want +got):\n%s", diff)
	}
}

// Without a Cloudflare token or a GitHub App on the node, Claude has no
// cloudflare_connect.
func TestCloudflareConnectAbsent(t *testing.T) {
	url, _ := newDemoBox(t).serve(t)
	s, err := connect(t, url, boxToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	tools, err := s.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "cloudflare_connect" {
			t.Error("cloudflare_connect is offered with no Cloudflare token or GitHub App")
		}
	}
}

// An agent started in an agent box gets Fern's MCP server in place of its
// own shell: Claude in its arguments, OpenCode's server in its settings,
// with the box's address to listen on. Other commands are left alone. The
// table's box is one started before the relay: its agent is still given the
// token, as the only way it has.
func TestAgentStart(t *testing.T) {
	// Each agent here prints its arguments, then OpenCode's settings.
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\"\nprintf '%s' \"$OPENCODE_CONFIG_CONTENT\"\n"
	for _, name := range []string{"claude", "opencode"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const mcpURL = "http://fern-daemon:7071/mcp/demo"
	fern := map[string]any{
		"type": "remote", "url": mcpURL, "headers": map[string]any{"Authorization": "Bearer " + boxToken},
		"oauth": false, "codemode": false,
	}
	noShell := map[string]any{"action": "tool.use", "resource": "shell:*", "effect": "deny"}
	claudeMCP := `{"mcpServers":{"fern":{"headers":{"Authorization":"Bearer ` + boxToken + `"},"type":"http","url":"` + mcpURL + `"}}}`

	tests := []struct {
		name     string
		req      boxexec.Request
		wantArgs []string
		wantCfg  map[string]any // nil: no settings
	}{
		{
			name:     "claude",
			req:      boxexec.Request{Argv: []string{filepath.Join(bin, "claude"), "-p"}},
			wantArgs: []string{"-p", "--mcp-config", claudeMCP, "--disallowedTools", "Bash,BashOutput,KillShell,KillBash,Monitor", "--setting-sources=user"},
		},
		{
			name:     "opencode's server",
			req:      boxexec.Request{Argv: []string{filepath.Join(bin, "opencode"), "serve", "--hostname=10.0.0.1", "--port=4096"}},
			wantArgs: []string{"serve", "--port=4096", "--hostname=127.0.0.1"},
			wantCfg: map[string]any{
				"mcp":      map[string]any{"servers": map[string]any{"fern": fern}},
				"plugins":  []any{"-opencode.tool.shell"},
				"policies": []any{noShell},
			},
		},
		{
			name: "opencode's server with settings",
			req: boxexec.Request{
				Argv:    []string{filepath.Join(bin, "opencode"), "serve"},
				Environ: []string{`OPENCODE_CONFIG_CONTENT={"model":"m","mcp":{"servers":{"mine":{"type":"local"},"fern":{}}},"plugins":["opencode.tool.shell"],"policies":[{"action":"a"}]}`},
			},
			wantArgs: []string{"serve", "--hostname=127.0.0.1"},
			wantCfg: map[string]any{
				"model":    "m",
				"mcp":      map[string]any{"servers": map[string]any{"mine": map[string]any{"type": "local"}, "fern": fern}},
				"plugins":  []any{"opencode.tool.shell", "-opencode.tool.shell"},
				"policies": []any{noShell, map[string]any{"action": "a"}},
			},
		},
		{
			name:     "another opencode command",
			req:      boxexec.Request{Argv: []string{filepath.Join(bin, "opencode"), "models", "--hostname=10.0.0.1"}},
			wantArgs: []string{"models", "--hostname=10.0.0.1"},
		},
	}
	// A box whose fern-init has the relay that adds the token: the agent is
	// pointed at the relay and given no token at all.
	const relayURL = "http://127.0.0.1:7072/mcp/demo"
	withRelay := newDemoBoxWith(t, true)
	relayAPI := server.New(withRelay.st, withRelay.drv, withRelay.envs, "http://fern-daemon:7071", notrace, withRelay.log)
	t.Cleanup(relayAPI.Close)
	relayed := serveSocket(t, relayAPI.Handler())
	var stdout bytes.Buffer
	req := boxexec.Request{Argv: []string{filepath.Join(bin, "claude"), "-p"}}
	if _, err := relayed.ExecAgent(t.Context(), "demo", req, boxexec.Streams{Stdout: &stdout, Stderr: io.Discard}); err != nil {
		t.Fatalf("ExecAgent() in a box with the relay: %v", err)
	}
	wantArgs := "-p\n--mcp-config\n" + `{"mcpServers":{"fern":{"type":"http","url":"` + relayURL + `"}}}` +
		"\n--disallowedTools\nBash,BashOutput,KillShell,KillBash,Monitor\n--setting-sources=user\n"
	if diff := cmp.Diff(wantArgs, stdout.String()); diff != "" {
		t.Errorf("Claude's arguments in a box with the relay (-want +got):\n%s", diff)
	}
	if strings.Contains(stdout.String(), boxToken) {
		t.Error("the box's token is in Claude's arguments")
	}

	b := newDemoBox(t)
	api := server.New(b.st, b.drv, b.envs, "http://fern-daemon:7071", notrace, b.log)
	t.Cleanup(api.Close)
	c := serveSocket(t, api.Handler())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout bytes.Buffer
			if _, err := c.ExecAgent(t.Context(), "demo", tt.req, boxexec.Streams{Stdout: &stdout, Stderr: io.Discard}); err != nil {
				t.Fatalf("ExecAgent() error: %v", err)
			}
			lines := strings.Split(stdout.String(), "\n")
			args, content := lines[:len(lines)-1], lines[len(lines)-1]
			if diff := cmp.Diff(tt.wantArgs, args); diff != "" {
				t.Errorf("arguments (-want +got):\n%s", diff)
			}
			var cfg map[string]any
			if content != "" {
				if err := json.Unmarshal([]byte(content), &cfg); err != nil {
					t.Fatalf("settings %q: %v", content, err)
				}
			}
			if diff := cmp.Diff(tt.wantCfg, cfg); diff != "" {
				t.Errorf("settings (-want +got):\n%s", diff)
			}
		})
	}
}

// BenchmarkMCPCall is the daemon's own share of a tool call, apart from the
// command: a request that the server answers itself.
func BenchmarkMCPCall(b *testing.B) {
	log := slog.New(slog.DiscardHandler)
	st := newFakeStore()
	if _, err := st.Create(b.Context(), "demo", store.CreateOptions{}); err != nil {
		b.Fatal(err)
	}
	drv := runsc.NewFake("box")
	agentUp(b, drv, "demo")
	envs := actor.New(st, drv, actor.Config{Image: "box"}, notrace, prometheus.NewRegistry(), log)
	api := server.New(st, drv, envs, "http://fern-daemon:7071", notrace, log)
	defer api.Close()
	srv := httptest.NewServer(api.MCPHandler())
	defer srv.Close()
	c := mcp.NewClient(&mcp.Implementation{Name: "bench", Version: "1"}, nil)
	s, err := c.Connect(b.Context(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp/demo", HTTPClient: &http.Client{Transport: bearer{boxToken}}}, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.ListTools(b.Context(), nil); err != nil {
			b.Fatal(err)
		}
	}
}
