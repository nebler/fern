package boxexec_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/nebler/fern/internal/boxexec"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestClaudeTiming(t *testing.T) {
	for _, tt := range []struct {
		name, timing, script string
		want                 []string
	}{
		{name: "disabled", script: "printf hello"},
		{name: "stdout", timing: "1", script: "printf hello; printf again", want: []string{"claude started", "claude first stdout"}},
		{name: "stderr only", timing: "1", script: "printf warning >&2", want: []string{"claude started"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			command := filepath.Join(dir, "claude")
			if err := os.WriteFile(command, []byte("#!/bin/sh\n"+tt.script+"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			srv := httptest.NewServer(boxexec.NewServer(token, dir, nil, slog.New(slog.NewJSONHandler(&logs, nil))))
			defer srv.Close()
			req := boxexec.Request{Argv: []string{command, "private-argument"}, Environ: []string{"FERN_TIMING=" + tt.timing, "PRIVATE=private-environment"}}
			if got := run(t, "ws"+strings.TrimPrefix(srv.URL, "http"), req, nil); got.code != 0 {
				t.Fatalf("Run() = %+v, want exit 0", got)
			}
			if strings.Contains(logs.String(), "private-") {
				t.Fatal("timing log exposed an argument or environment value")
			}
			var got []string
			decoder := json.NewDecoder(&logs)
			for decoder.More() {
				var record struct {
					Message string `json:"msg"`
				}
				if err := decoder.Decode(&record); err != nil {
					t.Fatal(err)
				}
				got = append(got, record.Message)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("timing events (-want +got):\n%s", diff)
			}
		})
	}
}

const token = "secret"

// newBox serves exec in a fresh directory and returns its WebSocket URL and
// the directory.
func newBox(t testing.TB) (url, dir string) {
	t.Helper()
	dir = t.TempDir()
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	srv := httptest.NewServer(boxexec.NewServer(token, dir, env, slog.New(slog.DiscardHandler)))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), dir
}

// newRelay serves a WebSocket endpoint that relays every connection to the
// box at boxURL, the way the daemon does.
func newRelay(t testing.TB, boxURL string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		box, err := boxexec.Dial(r.Context(), boxURL, token, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = box.CloseNow() }()
		client, err := boxexec.Accept(w, r)
		if err != nil {
			return
		}
		defer func() { _ = client.CloseNow() }()
		boxexec.Proxy(r.Context(), client, box, nil)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// targets returns a box and a relay in front of it: the two ways a client
// reaches a command.
func targets(tb testing.TB) []struct{ name, url string } {
	tb.Helper()
	box, _ := newBox(tb)
	return []struct{ name, url string }{
		{"direct", box},
		{"relay", newRelay(tb, box)},
	}
}

type result struct {
	code           int
	stdout, stderr string
}

// run runs req at url and returns what came back.
func run(t *testing.T, url string, req boxexec.Request, stdin io.Reader) result {
	t.Helper()
	// A hang is a failure, not a test that runs until the global timeout.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	conn, err := boxexec.Dial(ctx, url, token, nil)
	if err != nil {
		t.Fatalf("Dial(%s) error: %v", url, err)
	}
	defer func() { _ = conn.CloseNow() }()
	var stdout, stderr bytes.Buffer
	code, err := boxexec.Run(ctx, conn, req, boxexec.Streams{Stdin: stdin, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatalf("Run(%q) error: %v", req.Argv, err)
	}
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		req        boxexec.Request
		stdin      string
		wantCode   int
		wantStdout string
		// wantStderr is a substring of stderr, so messages can change.
		wantStderr string
	}{
		{
			name:       "stdout and stderr",
			req:        boxexec.Request{Argv: []string{"sh", "-c", "echo out; echo err >&2"}},
			wantStdout: "out\n",
			wantStderr: "err\n",
		},
		{
			name:     "exit code",
			req:      boxexec.Request{Argv: []string{"sh", "-c", "exit 3"}},
			wantCode: 3,
		},
		{
			name:       "stdin",
			req:        boxexec.Request{Argv: []string{"cat"}},
			stdin:      "hello",
			wantStdout: "hello",
		},
		{
			// More than coder/websocket's default read limit (32 KiB) per message.
			name:       "large stdin",
			req:        boxexec.Request{Argv: []string{"cat"}},
			stdin:      strings.Repeat("0123456789", 10_000),
			wantStdout: strings.Repeat("0123456789", 10_000),
		},
		{
			// Without -i the command must still see EOF, or cat would wait forever.
			name: "no stdin",
			req:  boxexec.Request{Argv: []string{"cat"}},
		},
		{
			name:       "command not found",
			req:        boxexec.Request{Argv: []string{"no-such-command"}},
			wantCode:   127,
			wantStderr: "no-such-command",
		},
		{
			name:       "empty argv",
			req:        boxexec.Request{},
			wantCode:   127,
			wantStderr: "no command",
		},
		{
			name:       "tty",
			req:        boxexec.Request{Argv: []string{"sh", "-c", "test -t 0 && test -t 1 && echo yes"}, TTY: true},
			wantStdout: "yes\r\n", // the terminal turns \n into \r\n
		},
	}
	for _, target := range targets(t) {
		for _, tt := range tests {
			t.Run(target.name+"/"+tt.name, func(t *testing.T) {
				var stdin io.Reader
				if tt.stdin != "" {
					stdin = strings.NewReader(tt.stdin)
				}
				got := run(t, target.url, tt.req, stdin)
				if got.code != tt.wantCode {
					t.Errorf("Run(%q) code = %d, want %d (stderr %q)", tt.req.Argv, got.code, tt.wantCode, got.stderr)
				}
				if got.stdout != tt.wantStdout {
					t.Errorf("Run(%q) stdout = %q, want %q", tt.req.Argv, got.stdout, tt.wantStdout)
				}
				if !strings.Contains(got.stderr, tt.wantStderr) {
					t.Errorf("Run(%q) stderr = %q, want it to contain %q", tt.req.Argv, got.stderr, tt.wantStderr)
				}
			})
		}
	}
}

// The daemon reads how a box was used from its fern-init, with the box's
// token: a new box was last used when it started, and runs nothing.
func TestActivityOf(t *testing.T) {
	began := time.Now()
	execs := boxexec.NewServer(token, t.TempDir(), nil, slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+boxexec.ActivityPath, execs.ServeActivity)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()

	a, err := boxexec.ActivityOf(t.Context(), srv.Client(), addr, token)
	if err != nil {
		t.Fatalf("ActivityOf() error = %v", err)
	}
	if a.Running != 0 || a.Opened.Before(began.Truncate(time.Second)) || !a.Closed.Equal(a.Opened) {
		t.Errorf("ActivityOf() of a new box = %+v, want nothing running, last used at its start", a)
	}
	if _, err := boxexec.ActivityOf(t.Context(), srv.Client(), addr, "wrong"); err == nil {
		t.Error("ActivityOf() with the wrong token succeeded")
	}
	// A fern-init from before it has no such route.
	old := httptest.NewServer(http.NewServeMux())
	t.Cleanup(old.Close)
	if _, err := boxexec.ActivityOf(t.Context(), old.Client(), old.Listener.Addr().String(), token); err == nil {
		t.Error("ActivityOf() of a box without the route succeeded")
	}
}

func TestDialWrongToken(t *testing.T) {
	url, _ := newBox(t)
	for _, tok := range []string{"", "wrong"} {
		conn, err := boxexec.Dial(t.Context(), url, tok, nil)
		if err == nil {
			_ = conn.CloseNow()
			t.Errorf("Dial(token %q) succeeded, want an error", tok)
		}
	}
}

// Cancelling must also stop what the command started in the background, or
// it would go on changing the workspace.
func TestRunCancelKillsBackground(t *testing.T) {
	url, _ := newBox(t)
	ctx, cancel := context.WithCancel(t.Context())
	conn, err := boxexec.Dial(ctx, url, token, nil)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	stdout := writerFunc(func(p []byte) (int, error) {
		cancel()
		return len(p), nil
	})
	req := boxexec.Request{Argv: []string{"sh", "-c", "sleep 600 & echo $! > bg.pid; echo started; wait"}}
	if _, err := boxexec.Run(ctx, conn, req, boxexec.Streams{Stdout: stdout, Stderr: io.Discard}); err == nil {
		t.Fatalf("Run(%q) after cancel succeeded, want an error", req.Argv)
	}
	_ = conn.CloseNow() // the client goes away
	// The kill on cancel ends it at once; without it the command would wait
	// for the background process.
	start := time.Now()
	waitGone(t, url, "bg.pid")
	if took := time.Since(start); took >= time.Second {
		t.Errorf("the background process went %v after the client, want at once", took)
	}
}

// What a command leaves behind must not keep the exec open, and what is left
// in its process group must not outlive it.
func TestRunLeftovers(t *testing.T) {
	// Every command starts a leftover that ignores the hangup, writes its
	// PID to left.pid and sleeps. The command exits once the PID is written.
	const left = `trap "" HUP; echo $$ > left.pid; exec sleep 600`
	tests := []struct {
		name       string
		tty        bool
		start      string // the shell command that starts the leftover
		wantStdout string
		// wantGone says whether the leftover must be killed. One in another
		// session is out of reach and must only not hold up the exec.
		wantGone bool
	}{
		{
			name:       "pipes",
			start:      `sh -c "$left" </dev/null >/dev/null 2>&1 &`,
			wantStdout: "started\n",
			wantGone:   true,
		},
		{
			name:       "pipes held open",
			start:      `sh -c "$left" &`,
			wantStdout: "started\n",
			wantGone:   true,
		},
		{
			name:       "tty",
			tty:        true,
			start:      `sh -c "$left" </dev/null >/dev/null 2>&1 &`,
			wantStdout: "started\r\n",
			wantGone:   true,
		},
		{
			name:       "tty held open from another session",
			tty:        true,
			start:      `setsid sh -c "$left" &`,
			wantStdout: "started\r\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url, dir := newBox(t)
			if !tt.wantGone {
				t.Cleanup(func() { killPID(t, filepath.Join(dir, "left.pid")) })
			}
			script := "left='" + left + "'; " + tt.start + "\nuntil [ -s left.pid ]; do sleep 0.01; done; echo started"
			req := boxexec.Request{Argv: []string{"sh", "-c", script}, TTY: tt.tty}
			got := run(t, url, req, nil)
			if got.code != 0 || got.stdout != tt.wantStdout {
				t.Errorf("Run(%q) = code %d, stdout %q, want 0, %q", req.Argv, got.code, got.stdout, tt.wantStdout)
			}
			if tt.wantGone {
				waitGone(t, url, "left.pid")
			}
		})
	}
}

// killPID kills the process whose PID is in pidFile, if there is one.
func killPID(t *testing.T, pidFile string) {
	t.Helper()
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return // the process never started
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Errorf("PID in %s: %v", pidFile, err)
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// waitGone waits until the process whose PID is in the box's file pidFile
// has gone. The kill is asynchronous to the client, so it waits in the box,
// bounded by run's timeout. Nobody reaps the process in a test, so a zombie
// counts as gone. Without ps the check fails rather than passing unseen.
func waitGone(t *testing.T, url, pidFile string) {
	t.Helper()
	const script = `command -v ps >/dev/null || { echo "ps not found" >&2; exit 2; }
pid=$(cat "$1") || exit 2
while stat=$(ps -o stat= -p "$pid"); do
	case $stat in *Z*) break ;; esac
	sleep 0.05
done
echo gone`
	check := boxexec.Request{Argv: []string{"sh", "-c", script, "sh", pidFile}}
	if got := run(t, url, check, nil); got.code != 0 || got.stdout != "gone\n" {
		t.Errorf("process in %s: code %d, stdout %q, stderr %q, want it gone", pidFile, got.code, got.stdout, got.stderr)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestRunDirAndEnv(t *testing.T) {
	url, dir := newBox(t)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A program the box's own PATH doesn't have, as mise's shims.
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "tool"), []byte("#!/bin/sh\necho pinned\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		req  boxexec.Request
		want string
	}{
		{"default dir", boxexec.Request{Argv: []string{"pwd", "-P"}}, resolved + "\n"},
		{"dir", boxexec.Request{Argv: []string{"pwd", "-P"}, Dir: filepath.Join(dir, "sub")}, filepath.Join(resolved, "sub") + "\n"},
		{"env added", boxexec.Request{Argv: []string{"sh", "-c", `echo "$A $PATH"`}, Environ: []string{"A=1"}}, "1 /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n"},
		{"env replaces", boxexec.Request{Argv: []string{"sh", "-c", `echo "$PATH"`}, Environ: []string{"PATH=/bin"}}, "/bin\n"},
		{"found on the command's PATH", boxexec.Request{Argv: []string{"tool"}, Environ: []string{"PATH=" + bin + ":/usr/bin:/bin"}}, "pinned\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := run(t, url, tt.req, nil); got.code != 0 || got.stdout != tt.want {
				t.Errorf("Run(%+v) = %d, %q (stderr %q), want 0, %q", tt.req, got.code, got.stdout, got.stderr, tt.want)
			}
		})
	}
}

// A signal from the client reaches the command, which decides how to stop:
// T3 stops Claude with SIGTERM and waits for it to save its transcript.
func TestRunSignal(t *testing.T) {
	for _, target := range targets(t) {
		t.Run(target.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			conn, err := boxexec.Dial(ctx, target.url, token, nil)
			if err != nil {
				t.Fatalf("Dial() error: %v", err)
			}
			defer func() { _ = conn.CloseNow() }()
			sigs := make(chan os.Signal, 2)
			var stdout bytes.Buffer
			ready := writerFunc(func(p []byte) (int, error) {
				if bytes.Contains(p, []byte("ready")) {
					sigs <- syscall.SIGKILL // the server passes on only stop signals
					sigs <- syscall.SIGTERM
				}
				return stdout.Write(p)
			})
			req := boxexec.Request{Argv: []string{"sh", "-c", `trap 'echo stopping; exit 7' TERM; echo ready; while :; do sleep 0.05; done`}}
			code, err := boxexec.Run(ctx, conn, req, boxexec.Streams{Stdout: ready, Stderr: io.Discard, Signal: sigs})
			if err != nil || code != 7 || !strings.Contains(stdout.String(), "stopping") {
				t.Errorf("Run after SIGTERM = %d, %v, stdout %q; want 7 and the trap's output", code, err, stdout.String())
			}
		})
	}
}

// A command that leaves a process behind ends when it exits, not when the
// process lets go of its output: agents start servers in the background.
func TestRunBackgroundDoesNotDelay(t *testing.T) {
	for _, target := range targets(t) {
		t.Run(target.name, func(t *testing.T) {
			req := boxexec.Request{Argv: []string{"sh", "-c", "sleep 600 & echo ready"}}
			start := time.Now()
			got := run(t, target.url, req, nil)
			if took := time.Since(start); got.code != 0 || got.stdout != "ready\n" || took >= time.Second {
				t.Errorf("Run(%q) = %d, %q after %v, want 0, %q at once", req.Argv, got.code, got.stdout, took, "ready\n")
			}
		})
	}
}

// A signal reaches a command that isn't reading the stdin sent before it.
func TestRunSignalBehindUnreadStdin(t *testing.T) {
	for _, target := range targets(t) {
		t.Run(target.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			conn, err := boxexec.Dial(ctx, target.url, token, nil)
			if err != nil {
				t.Fatalf("Dial() error: %v", err)
			}
			defer func() { _ = conn.CloseNow() }()
			sigs := make(chan os.Signal, 1)
			ready := writerFunc(func(p []byte) (int, error) {
				if bytes.Contains(p, []byte("ready")) {
					sigs <- syscall.SIGTERM
				}
				return len(p), nil
			})
			// More than a pipe holds, so a write of it blocks.
			stdin := bytes.NewReader(make([]byte, 256<<10))
			req := boxexec.Request{Argv: []string{"sh", "-c", `trap 'exit 7' TERM; echo ready; while :; do sleep 0.05; done`}}
			start := time.Now()
			code, err := boxexec.Run(ctx, conn, req, boxexec.Streams{Stdin: stdin, Stdout: ready, Stderr: io.Discard, Signal: sigs})
			if took := time.Since(start); err != nil || code != 7 || took >= 5*time.Second {
				t.Errorf("Run after SIGTERM = %d, %v after %v, want 7 at once", code, err, took)
			}
		})
	}
}

// sessionServer serves token's commands in a temporary folder, with a short
// grace. It returns a dial for its exec endpoint.
func sessionServer(t *testing.T, grace time.Duration) func(context.Context) (*websocket.Conn, error) {
	t.Helper()
	s := boxexec.NewServer(token, t.TempDir(), os.Environ(), slog.New(slog.DiscardHandler))
	s.Grace = grace
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	return func(ctx context.Context) (*websocket.Conn, error) { return boxexec.Dial(ctx, url, token, nil) }
}

// breaker dials through dial, and breaks the newest connection with no close
// message, as a daemon that dies does.
type breaker struct {
	dial func(context.Context) (*websocket.Conn, error)
	mu   sync.Mutex
	conn *websocket.Conn
	n    int
}

func (b *breaker) Dial(ctx context.Context) (*websocket.Conn, error) {
	conn, err := b.dial(ctx)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err == nil {
		b.conn = conn
		b.n++
	}
	return conn, err
}

func (b *breaker) Break() {
	b.mu.Lock()
	defer b.mu.Unlock()
	_ = b.conn.CloseNow()
}

func (b *breaker) dials() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

// lockedBuffer can be read while a session writes to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestSessionSurvivesBrokenConnection(t *testing.T) {
	b := &breaker{dial: sessionServer(t, time.Minute)}
	ctx := t.Context()
	conn, err := b.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Echoes each line it reads, numbered, then exits with a code of its own.
	script := `n=0; while read -r line; do n=$((n+1)); echo "$n $line"; done; echo end; exit 7`
	inR, inW := io.Pipe()
	defer func() { _ = inW.Close() }()
	var out lockedBuffer
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := boxexec.RunSession(ctx, conn, boxexec.Request{Argv: []string{"/bin/sh", "-c", script}, Session: "s1"},
			boxexec.Streams{Stdin: inR, Stdout: &out, Stderr: &out}, boxexec.Resume{Dial: b.Dial, For: 10 * time.Second})
		done <- result{code, err}
	}()
	var want strings.Builder
	for i := 1; i <= 20; i++ {
		line := fmt.Sprintf("line%d", i)
		if _, err := fmt.Fprintln(inW, line); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&want, "%d %s\n", i, line)
		// The connection breaks between lines and while they are on their way.
		if i%5 == 0 {
			waitFor(t, "line "+line, func() bool { return strings.Contains(out.String(), line) })
			b.Break()
		} else if i%7 == 0 {
			b.Break()
		}
	}
	_ = inW.Close()
	want.WriteString("end\n")
	r := <-done
	if r.err != nil {
		t.Fatalf("RunSession: %v", r.err)
	}
	if r.code != 7 {
		t.Errorf("exit code = %d, want 7", r.code)
	}
	// Nothing lost, nothing repeated, and the same process all along.
	if diff := cmp.Diff(want.String(), out.String()); diff != "" {
		t.Errorf("output (-want +got):\n%s", diff)
	}
	if b.dials() < 2 {
		t.Errorf("dialled %d times: nothing was broken", b.dials())
	}
}

func TestSessionOutputPastWindow(t *testing.T) {
	b := &breaker{dial: sessionServer(t, time.Minute)}
	ctx := t.Context()
	conn, err := b.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	counted := &lockedBuffer{}
	stop := make(chan struct{})
	defer close(stop)
	var total atomic.Int64
	go func() {
		// Breaks the connection as the output goes by.
		for last := int64(0); ; time.Sleep(time.Millisecond) {
			select {
			case <-stop:
				return
			default:
			}
			if n := total.Load(); n-last > 3<<20 {
				last = n
				b.Break()
			}
		}
	}()
	out := writerFunc(func(p []byte) (int, error) {
		total.Add(int64(len(p)))
		return h.Write(p)
	})
	const size = 16 << 20
	code, err := boxexec.RunSession(ctx, conn,
		boxexec.Request{Argv: []string{"/bin/sh", "-c", fmt.Sprintf("awk 'BEGIN { for (i = 1; ; i++) print i }' | head -c %d", size)}, Session: "s2"},
		boxexec.Streams{Stdout: out, Stderr: counted}, boxexec.Resume{Dial: b.Dial, For: 10 * time.Second})
	if err != nil || code != 0 {
		t.Fatalf("RunSession = %d, %v; stderr %q", code, err, counted.String())
	}
	if total.Load() != size {
		t.Errorf("got %d bytes, want %d", total.Load(), size)
	}
	wantSum := sha256.New()
	// head cuts the numbers at size.
	var written int
	for i := 1; written < size; i++ {
		s := strconv.Itoa(i) + "\n"
		if written+len(s) > size {
			s = s[:size-written]
		}
		wantSum.Write([]byte(s))
		written += len(s)
	}
	if !bytes.Equal(h.Sum(nil), wantSum.Sum(nil)) {
		t.Error("the output differs from what the command printed")
	}
}

// A command that prints what it reads, given more stdin at once than both
// windows hold: the client's word that it has the output comes on the same
// connection as its stdin, and must be read however much stdin waits.
func TestSessionLargeStdinEchoed(t *testing.T) {
	dial := sessionServer(t, time.Minute)
	conn, err := dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	in := bytes.Repeat([]byte("0123456789abcde\n"), 8<<20/16)
	got := sha256.New()
	var stderr lockedBuffer
	code, err := boxexec.RunSession(t.Context(), conn, boxexec.Request{Argv: []string{"cat"}, Session: "echo"},
		boxexec.Streams{Stdin: bytes.NewReader(in), Stdout: got, Stderr: &stderr}, boxexec.Resume{})
	if err != nil || code != 0 {
		t.Fatalf("RunSession = %d, %v; stderr %q", code, err, stderr.String())
	}
	if want := sha256.Sum256(in); !bytes.Equal(got.Sum(nil), want[:]) {
		t.Error("the command's output differs from its stdin")
	}
}

// A connection that breaks while the command isn't reading leaves its reader
// holding a stdin frame it has not counted yet. The connection after it must
// not be told to send that frame again while the first can still deliver it.
func TestSessionTakeoverWithStdinBackedUp(t *testing.T) {
	b := &breaker{dial: sessionServer(t, time.Minute)}
	ctx := t.Context()
	conn, err := b.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Reads nothing until the file is there.
	gate := filepath.Join(t.TempDir(), "go")
	script := fmt.Sprintf(`while [ ! -e %q ]; do sleep 0.05; done; exec cat`, gate)
	inR, inW := io.Pipe()
	defer func() { _ = inW.Close() }()
	const lines = 400
	line := func(i int) string { return fmt.Sprintf("%06d %s\n", i, strings.Repeat("x", 4000)) }
	var sent atomic.Int64
	go func() {
		for i := range lines {
			if _, err := io.WriteString(inW, line(i)); err != nil {
				return
			}
			sent.Add(1)
		}
		_ = inW.Close()
	}()
	var out lockedBuffer
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := boxexec.RunSession(ctx, conn, boxexec.Request{Argv: []string{"/bin/sh", "-c", script}, Session: "backed-up"},
			boxexec.Streams{Stdin: inR, Stdout: &out, Stderr: &out}, boxexec.Resume{Dial: b.Dial, For: 10 * time.Second})
		done <- result{code, err}
	}()
	// The client holds a window of stdin by now, far more than the server
	// takes in for a command that reads none: its reader waits with a frame.
	waitFor(t, "stdin to back up", func() bool { return sent.Load() >= 256 })
	b.Break()
	waitFor(t, "a second connection", func() bool { return b.dials() >= 2 })
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.code != 0 {
		t.Fatalf("RunSession = %d, %v", r.code, r.err)
	}
	got := strings.SplitAfter(out.String(), "\n")
	if got[len(got)-1] == "" {
		got = got[:len(got)-1]
	}
	if len(got) != lines {
		t.Errorf("the command read %d lines, want %d", len(got), lines)
	}
	for i, l := range got {
		if i >= lines || l != line(i) {
			t.Fatalf("line %d is %.20q…: stdin was repeated or lost", i, l)
		}
	}
}

// During an upgrade a new client meets a box still running an older
// fern-init, which ignores the session: the command just runs, as an exec.
func TestSessionOnOlderServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := boxexec.Accept(w, r)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := r.Context()
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		// Back what stdin it was sent, then the end. Frames: 0 stdin, 3 its end, 1 stdout.
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if data[0] == 3 {
				break
			}
			if err := conn.Write(ctx, websocket.MessageBinary, append([]byte{1}, data[1:]...)); err != nil {
				return
			}
		}
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"code":5}`))
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()
	ctx := t.Context()
	conn, err := boxexec.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code, err := boxexec.RunSession(ctx, conn, boxexec.Request{Argv: []string{"cat"}, Session: "s"},
		boxexec.Streams{Stdin: strings.NewReader("hello"), Stdout: &out, Stderr: &out}, boxexec.Resume{})
	if err != nil || code != 5 {
		t.Fatalf("RunSession = %d, %v, want 5", code, err)
	}
	if out.String() != "hello" {
		t.Errorf("output = %q", out.String())
	}
}

func TestSessionEnds(t *testing.T) {
	const marker = "alive"
	running := func(_ *testing.T, pidfile string) func() bool {
		return func() bool {
			data, err := os.ReadFile(pidfile)
			if err != nil {
				return false
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				return false
			}
			return syscall.Kill(pid, 0) == nil
		}
	}
	start := func(t *testing.T, dial func(context.Context) (*websocket.Conn, error), ctx context.Context, resume boxexec.Resume, session string) (string, chan error) {
		t.Helper()
		pidfile := filepath.Join(t.TempDir(), "pid")
		conn, err := dial(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out lockedBuffer
		done := make(chan error, 1)
		go func() {
			_, err := boxexec.RunSession(ctx, conn, boxexec.Request{
				Argv:    []string{"/bin/sh", "-c", "echo $$ > " + pidfile + "; echo " + marker + "; exec sleep 300"},
				Session: session,
			}, boxexec.Streams{Stdout: &out, Stderr: &out}, resume)
			done <- err
		}()
		waitFor(t, "the command", func() bool { return strings.Contains(out.String(), marker) })
		return pidfile, done
	}

	t.Run("a cancelled client kills it at once", func(t *testing.T) {
		dial := sessionServer(t, time.Hour)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		pidfile, done := start(t, dial, ctx, boxexec.Resume{Dial: dial, For: time.Second}, "cancel")
		cancel()
		if err := <-done; err == nil {
			t.Error("RunSession returned no error")
		}
		waitFor(t, "the command to die", func() bool { return !running(t, pidfile)() })
	})

	t.Run("nobody coming back kills it after the grace", func(t *testing.T) {
		b := &breaker{dial: sessionServer(t, 200*time.Millisecond)}
		ctx := t.Context()
		// No Resume: the client gives up, and the command is on its own.
		pidfile, done := start(t, b.Dial, ctx, boxexec.Resume{}, "grace")
		b.Break()
		if err := <-done; err == nil {
			t.Error("RunSession returned no error")
		}
		if !running(t, pidfile)() {
			t.Error("the command died with its connection, inside the grace")
		}
		waitFor(t, "the command to die", func() bool { return !running(t, pidfile)() })
	})

	t.Run("a caller going away leaves it running", func(t *testing.T) {
		dial := sessionServer(t, 300*time.Millisecond)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		pidfile, done := start(t, dial, ctx, boxexec.Resume{Abandon: func() bool { return true }}, "abandon")
		cancel()
		if err := <-done; err == nil {
			t.Error("RunSession returned no error")
		}
		if !running(t, pidfile)() {
			t.Error("the command died with its caller")
		}
		waitFor(t, "the command to die", func() bool { return !running(t, pidfile)() })
	})

	t.Run("a session that is gone isn't started again", func(t *testing.T) {
		dial := sessionServer(t, time.Minute)
		ctx := t.Context()
		conn, err := dial(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = boxexec.RunSession(ctx, conn, boxexec.Request{Session: "never"}, boxexec.Streams{Stdout: io.Discard, Stderr: io.Discard}, boxexec.Resume{})
		if !errors.Is(err, boxexec.ErrSessionGone) {
			t.Errorf("RunSession = %v, want ErrSessionGone", err)
		}
	})

	t.Run("a retry after the end gets the code without running it again", func(t *testing.T) {
		dial := sessionServer(t, time.Minute)
		ctx := t.Context()
		count := filepath.Join(t.TempDir(), "count")
		req := boxexec.Request{Argv: []string{"/bin/sh", "-c", "echo x >> " + count + "; exit 3"}, Session: "again"}
		for range 2 {
			conn, err := dial(ctx)
			if err != nil {
				t.Fatal(err)
			}
			code, err := boxexec.RunSession(ctx, conn, req, boxexec.Streams{Stdout: io.Discard, Stderr: io.Discard}, boxexec.Resume{})
			if err != nil || code != 3 {
				t.Fatalf("RunSession = %d, %v, want 3", code, err)
			}
		}
		data, err := os.ReadFile(count)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(string(data), "x"); got != 1 {
			t.Errorf("ran %d times, want 1", got)
		}
	})
}

func TestClipped(t *testing.T) {
	const limit = 100
	for _, tt := range []struct {
		name   string
		writes []string
		want   string
	}{
		{name: "short", writes: []string{"abc", "def"}, want: "abcdef"},
		{name: "exactly the limit", writes: []string{strings.Repeat("a", limit)}, want: strings.Repeat("a", limit)},
		{
			name:   "one long write",
			writes: []string{strings.Repeat("a", 50) + strings.Repeat("b", 1000) + strings.Repeat("c", 50)},
			want:   strings.Repeat("a", 50) + "\n[… 1000 bytes left out here, for good: send long output to a file and read the part you need …]\n" + strings.Repeat("c", 50),
		},
		{
			name:   "small writes",
			writes: slices.Repeat([]string{"x"}, 1000),
			want:   strings.Repeat("x", 50) + "\n[… 900 bytes left out here, for good: send long output to a file and read the part you need …]\n" + strings.Repeat("x", 50),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := boxexec.NewClipped(limit)
			for _, w := range tt.writes {
				if _, err := c.Write([]byte(w)); err != nil {
					t.Fatal(err)
				}
			}
			if diff := cmp.Diff(tt.want, c.String()); diff != "" {
				t.Errorf("String() (-want +got):\n%s", diff)
			}
		})
	}
}

func TestClippedKeepsLittle(t *testing.T) {
	c := boxexec.NewClipped(1000)
	chunk := make([]byte, 1<<16)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range 1 << 14 { // 1 GiB
		_, _ = c.Write(chunk)
	}
	runtime.ReadMemStats(&after)
	if grown := int64(after.TotalAlloc) - int64(before.TotalAlloc); grown > 1<<20 {
		t.Errorf("allocated %d bytes for 1 GiB of output", grown)
	}
	runtime.KeepAlive(c)
}
