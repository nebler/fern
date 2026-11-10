package hibernate_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/hibernate"
)

const (
	serveIdle = 300 * time.Millisecond
	password  = "pw"
	waitLong  = 5 * time.Second
)

var auth = "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:"+password))

// fakeOpenCode is `opencode serve` in a box, as Exec runs it: an HTTP
// server that says where it listens, with an event stream. A prompt starts
// an execution, which ends at once unless the prompt says "hold", then when
// the test sends on release. A signal stops it with 0; die, with 1.
type fakeOpenCode struct {
	mu      sync.Mutex
	runs    int
	mcp     [][]string    // the MCP servers each run was given
	exited  chan int      // each run's exit
	release chan struct{} // ends a held execution
	die     chan struct{} // makes the first server fail
}

func newFakeOpenCode() *fakeOpenCode {
	return &fakeOpenCode{exited: make(chan int, 16), release: make(chan struct{}), die: make(chan struct{})}
}

func (f *fakeOpenCode) exec(ctx context.Context, _ boxexec.Request, s boxexec.Streams) (int, error) {
	f.mu.Lock()
	run := f.runs
	f.runs++
	f.mcp = append(f.mcp, nil)
	var die chan struct{}
	if run == 0 {
		die = f.die
	}
	f.mu.Unlock()
	stopped := make(chan struct{})
	defer close(stopped)
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return 1, err
	}
	events := make(chan string, 16)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/event", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"server.connected\",\"data\":{\"run\":%d}}\n\n: heartbeat\n\n", run)
		w.(http.Flusher).Flush()
		for {
			select {
			case ev := <-events:
				_, _ = fmt.Fprintf(w, "data: %s\n\n", ev)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	mux.HandleFunc("POST /api/session/{id}/prompt", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		events <- fmt.Sprintf(`{"type":"session.execution.started","data":{"sessionID":%q,"run":%d}}`, id, run)
		body, _ := io.ReadAll(r.Body)
		if string(body) == "hold" {
			go func() {
				select {
				case <-f.release:
				case <-stopped:
					return
				}
				events <- fmt.Sprintf(`{"type":"session.execution.succeeded","data":{"sessionID":%q,"run":%d}}`, id, run)
			}()
		} else {
			events <- fmt.Sprintf(`{"type":"session.execution.succeeded","data":{"sessionID":%q,"run":%d}}`, id, run)
		}
		_, _ = fmt.Fprintf(w, "%d", run)
	})
	mux.HandleFunc("PUT /api/experimental/mcp/{name}", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.mcp[run] = append(f.mcp[run], r.PathValue("name")+"?"+r.URL.RawQuery+" "+string(body))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != auth {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})}
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ln) }()
	_, _ = fmt.Fprintf(s.Stdout, "Warning: something\nopencode server listening on http://%s\n", ln.Addr())
	code := 0
	select {
	case <-s.Signal:
	case <-ctx.Done():
		code = 143
	case <-die:
		code = 1
	}
	_ = srv.Close()
	<-done
	f.exited <- code
	return code, nil
}

func (f *fakeOpenCode) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs
}

// served is a Serve under test.
type served struct {
	url    string
	sigs   chan<- os.Signal
	result chan [2]any
}

func startServe(t *testing.T, f *fakeOpenCode) *served {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	req := boxexec.Request{
		Argv:    []string{"opencode", "serve", "--hostname=127.0.0.1", fmt.Sprintf("--port=%d", port)},
		Environ: []string{"OPENCODE_PASSWORD=" + password},
	}
	pr, pw := io.Pipe()
	sigs := make(chan os.Signal, 1)
	result := make(chan [2]any, 1)
	go func() {
		code, err := hibernate.Serve(t.Context(), f.exec, req, boxexec.Streams{Stdout: pw, Stderr: io.Discard, Signal: sigs}, serveIdle)
		_ = pw.Close()
		result <- [2]any{code, err}
	}()
	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil {
		t.Fatalf("Serve printed nothing: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	want := fmt.Sprintf("opencode server listening on http://127.0.0.1:%d\n", port)
	if line != want {
		t.Fatalf("Serve printed %q, want %q", line, want)
	}
	s := &served{url: strings.TrimSpace(strings.TrimPrefix(line, "opencode server listening on ")), sigs: sigs, result: result}
	return s
}

// stop ends Serve as T3 does, and returns its code.
func (s *served) stop(t *testing.T) int {
	t.Helper()
	s.sigs <- syscall.SIGTERM
	select {
	case r := <-s.result:
		if r[1] != nil {
			t.Fatalf("Serve: %v", r[1])
		}
		return r[0].(int)
	case <-time.After(waitLong):
		t.Fatal("Serve did not end")
	}
	return 0
}

func (s *served) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, s.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// subscribe opens T3's event stream and returns its events' data, one per
// receive; the channel closes with the stream.
func (s *served) subscribe(t *testing.T) <-chan string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.url+"/api/event", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events: %s", resp.Status)
	}
	out := make(chan string, 64)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				out <- data
			}
		}
	}()
	return out
}

// next returns the type and run of the stream's next event.
func next(t *testing.T, events <-chan string) string {
	t.Helper()
	select {
	case data, ok := <-events:
		if !ok {
			return "closed"
		}
		var ev struct {
			Type string
			Data struct{ Run int }
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s/%d", ev.Type, ev.Data.Run)
	case <-time.After(waitLong):
		t.Fatal("no event")
	}
	return ""
}

func waitExit(t *testing.T, f *fakeOpenCode) int {
	t.Helper()
	select {
	case code := <-f.exited:
		return code
	case <-time.After(waitLong):
		t.Fatal("the server did not stop")
	}
	return 0
}

func TestServeStopsAndStartsAgain(t *testing.T) {
	f := newFakeOpenCode()
	s := startServe(t, f)
	events := s.subscribe(t)
	var got []string
	got = append(got, next(t, events))
	if code, _ := s.do(t, http.MethodPut, "/api/experimental/mcp/t3?directory=%2Fw", `{"config":1}`); code != http.StatusNoContent {
		t.Fatalf("mcp.add: %d", code)
	}
	_, run := s.do(t, http.MethodPost, "/api/session/s1/prompt", "hi")
	got = append(got, "prompt/"+run, next(t, events), next(t, events))

	// Idle: the server stops; T3's stream stays open.
	got = append(got, fmt.Sprintf("exit/%d", waitExit(t, f)))
	_, run = s.do(t, http.MethodPost, "/api/session/s1/prompt", "again")
	got = append(got, "prompt/"+run, next(t, events), next(t, events))
	got = append(got, fmt.Sprintf("exit/%d", waitExit(t, f)))
	if code := s.stop(t); code != 0 {
		t.Errorf("Serve's code = %d, want 0", code)
	}
	if ev := next(t, events); ev != "closed" {
		t.Errorf("after Serve ended, the stream sent %s", ev)
	}

	want := []string{
		"server.connected/0",
		"prompt/0", "session.execution.started/0", "session.execution.succeeded/0",
		"exit/0",
		"prompt/1", "session.execution.started/1", "session.execution.succeeded/1",
		"exit/0",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	mcp := []string{`t3?directory=%2Fw {"config":1}`}
	if diff := cmp.Diff([][]string{mcp, mcp}, f.mcp); diff != "" {
		t.Errorf("MCP servers per run (-want +got):\n%s", diff)
	}
}

func TestServeKeepsAnExecutionRunning(t *testing.T) {
	f := newFakeOpenCode()
	s := startServe(t, f)
	events := s.subscribe(t)
	next(t, events)
	s.do(t, http.MethodPost, "/api/session/s1/prompt", "hold")
	next(t, events)
	select {
	case <-f.exited:
		t.Fatal("the server stopped during an execution")
	case <-time.After(10 * serveIdle):
	}
	f.release <- struct{}{}
	if ev := next(t, events); ev != "session.execution.succeeded/0" {
		t.Fatalf("event %s", ev)
	}
	waitExit(t, f)
	s.stop(t)
}

func TestServeRefusesWithoutPassword(t *testing.T) {
	f := newFakeOpenCode()
	s := startServe(t, f)
	waitExit(t, f)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, s.url+"/api/session", nil)
	req.Header.Set("Authorization", "Basic bm8=")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", resp.StatusCode)
	}
	if n := f.count(); n != 1 {
		t.Errorf("%d servers started, want 1: a refused request woke the box", n)
	}
	s.stop(t)
}

func TestServeLosesAServerThatDies(t *testing.T) {
	f := newFakeOpenCode()
	s := startServe(t, f)
	events := s.subscribe(t)
	next(t, events)
	s.do(t, http.MethodPost, "/api/session/s1/prompt", "hold")
	next(t, events)
	close(f.die)
	if code := waitExit(t, f); code != 1 {
		t.Errorf("exit %d", code)
	}
	if ev := next(t, events); ev != "closed" {
		t.Errorf("T3's stream sent %s, want it closed so T3 reconnects", ev)
	}
	// T3 reconnects; the server starts at its next request.
	if ev := next(t, s.subscribe(t)); ev != "server.connected/0" {
		t.Errorf("a new stream began with %s", ev)
	}
	_, run := s.do(t, http.MethodPost, "/api/session/s1/prompt", "hi")
	if run != "1" {
		t.Errorf("served by run %s, want 1", run)
	}
	if code := s.stop(t); code != 0 {
		t.Errorf("Serve's code = %d", code)
	}
}

func TestServePassesOtherCommands(t *testing.T) {
	var argv []string
	exec := func(_ context.Context, req boxexec.Request, _ boxexec.Streams) (int, error) {
		argv = req.Argv
		return 3, nil
	}
	req := boxexec.Request{Argv: []string{"opencode", "models"}}
	code, err := hibernate.Serve(t.Context(), exec, req, boxexec.Streams{}, serveIdle)
	if err != nil || code != 3 {
		t.Fatalf("Serve = %d, %v", code, err)
	}
	if diff := cmp.Diff(req.Argv, argv); diff != "" {
		t.Error(diff)
	}
}
