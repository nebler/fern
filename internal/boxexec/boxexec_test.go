package boxexec_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
		_ = boxexec.Proxy(r.Context(), client, box)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

type result struct {
	code           int
	stdout, stderr string
}

// run runs req at url and returns what came back.
func run(t *testing.T, url, tok string, req boxexec.Request, stdin io.Reader) result {
	t.Helper()
	// A hang is a failure, not a test that runs until the global timeout.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	conn, err := boxexec.Dial(ctx, url, tok, nil)
	if err != nil {
		t.Fatalf("Dial(%s) error: %v", url, err)
	}
	defer func() { _ = conn.CloseNow() }()
	var stdout, stderr bytes.Buffer
	code, err := boxexec.Run(ctx, conn, req, stdin, &stdout, &stderr, nil)
	if err != nil {
		t.Fatalf("Run(%q) error: %v", req.Argv, err)
	}
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func TestRun(t *testing.T) {
	boxURL, dir := newBox(t)
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	targets := []struct {
		name string
		url  string
	}{
		{"direct", boxURL},
		{"proxy", newRelay(t, boxURL)},
	}
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
		{
			// The background process ignores the hangup and keeps the
			// terminal open; the exec must end anyway. The command waits
			// until the hangup is ignored.
			name: "tty with a process left behind",
			req: boxexec.Request{
				Argv: []string{"sh", "-c", `rm -f held; (trap "" HUP; touch held; exec sleep 600) & until [ -e held ]; do sleep 0.01; done; echo started`},
				TTY:  true,
			},
			wantStdout: "started\r\n",
		},
		{
			name:       "runs in dir",
			req:        boxexec.Request{Argv: []string{"pwd"}},
			wantStdout: dir + "\n",
		},
	}
	for _, target := range targets {
		for _, tt := range tests {
			t.Run(target.name+"/"+tt.name, func(t *testing.T) {
				var stdin io.Reader
				if tt.stdin != "" {
					stdin = strings.NewReader(tt.stdin)
				}
				got := run(t, target.url, token, tt.req, stdin)
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

// A client that goes away must stop the command, or it would run forever.
func TestRunCancelStopsCommand(t *testing.T) {
	url, _ := newBox(t)
	ctx, cancel := context.WithCancel(t.Context())
	conn, err := boxexec.Dial(ctx, url, token, nil)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	// The command says it started, then blocks; cancel once it has started.
	stdout := writerFunc(func(p []byte) (int, error) {
		cancel()
		return len(p), nil
	})
	req := boxexec.Request{Argv: []string{"sh", "-c", "echo started; exec sleep 600"}}
	if _, err := boxexec.Run(ctx, conn, req, nil, stdout, io.Discard, nil); err == nil {
		t.Errorf("Run(%q) after cancel succeeded, want an error", req.Argv)
	}
	// goleak in TestMain fails the run if the server's goroutines are stuck
	// waiting for the command.
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
	if _, err := boxexec.Run(ctx, conn, req, nil, stdout, io.Discard, nil); err == nil {
		t.Fatalf("Run(%q) after cancel succeeded, want an error", req.Argv)
	}
	_ = conn.CloseNow() // the client goes away
	waitGone(t, url, "bg.pid")
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
	if got := run(t, url, token, check, nil); got.code != 0 || got.stdout != "gone\n" {
		t.Errorf("process in %s: code %d, stdout %q, stderr %q, want it gone", pidFile, got.code, got.stdout, got.stderr)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
