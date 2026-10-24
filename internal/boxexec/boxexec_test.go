package boxexec_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/nebler/fern/internal/boxexec"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
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
		boxexec.Proxy(r.Context(), client, box)
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
	tests := []struct {
		name string
		req  boxexec.Request
		want string
	}{
		{"default dir", boxexec.Request{Argv: []string{"pwd", "-P"}}, resolved + "\n"},
		{"dir", boxexec.Request{Argv: []string{"pwd", "-P"}, Dir: filepath.Join(dir, "sub")}, filepath.Join(resolved, "sub") + "\n"},
		{"env added", boxexec.Request{Argv: []string{"sh", "-c", `echo "$A $PATH"`}, Environ: []string{"A=1"}}, "1 /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n"},
		{"env replaces", boxexec.Request{Argv: []string{"sh", "-c", `echo "$PATH"`}, Environ: []string{"PATH=/bin"}}, "/bin\n"},
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
