package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParse(t *testing.T) {
	const sock = defaultSocket
	tests := []struct {
		name    string
		args    []string
		envSock string            // FERN_SOCKET
		envVars map[string]string // other variables to set
		want    command
		wantErr bool
	}{
		{
			name: "create with repo after env",
			args: []string{"create", "demo", "--repo", "dir"},
			want: command{name: "create", socket: sock, env: "demo", repo: "dir"},
		},
		{
			name: "create with repo before env",
			args: []string{"create", "--repo", "dir", "demo"},
			want: command{name: "create", socket: sock, env: "demo", repo: "dir"},
		},
		{
			name: "create from stdin",
			args: []string{"create", "demo", "--repo", "-"},
			want: command{name: "create", socket: sock, env: "demo", repo: "-"},
		},
		{
			name: "create with a path",
			args: []string{"create", "wt", "--path", "/var/lib/fern/worktrees/r/b"},
			want: command{name: "create", socket: sock, env: "wt", path: "/var/lib/fern/worktrees/r/b"},
		},
		{
			name: "ls",
			args: []string{"ls"},
			want: command{name: "ls", socket: sock},
		},
		{
			name: "which",
			args: []string{"which", "/var/lib/fern/worktrees/r/b/src"},
			want: command{name: "which", socket: sock, path: "/var/lib/fern/worktrees/r/b/src"},
		},
		{
			name: "snapshot --replace",
			args: []string{"snapshot", "--replace", "demo", "s1"},
			want: command{name: "snapshot", socket: sock, env: "demo", snap: "s1", again: true},
		},
		{
			name: "rmsnap",
			args: []string{"rmsnap", "demo", "s1", "s2"},
			want: command{name: "rmsnap", socket: sock, env: "demo", snaps: []string{"s1", "s2"}},
		},
		{
			name: "exec with --",
			args: []string{"exec", "demo", "--", "psql", "-c", "select 1"},
			want: command{name: "exec", socket: sock, env: "demo", argv: []string{"psql", "-c", "select 1"}},
		},
		{
			name: "exec -it",
			args: []string{"exec", "-it", "demo", "--", "bash"},
			want: command{name: "exec", socket: sock, env: "demo", stdin: true, tty: true, argv: []string{"bash"}},
		},
		{
			name: "exec -i -t",
			args: []string{"exec", "-i", "-t", "demo", "bash"},
			want: command{name: "exec", socket: sock, env: "demo", stdin: true, tty: true, argv: []string{"bash"}},
		},
		{
			name: "exec -i",
			args: []string{"exec", "-i", "demo", "cat"},
			want: command{name: "exec", socket: sock, env: "demo", stdin: true, argv: []string{"cat"}},
		},
		{
			name: "exec --here",
			args: []string{"exec", "-i", "--here", "--", "claude", "-p"},
			want: command{name: "exec", socket: sock, stdin: true, here: true, argv: []string{"claude", "-p"}},
		},
		{
			name:    "exec -e and --cwd",
			args:    []string{"exec", "-e", "FERN_TEST_SET", "-e", "FERN_TEST_UNSET", "-e", "A=b", "--cwd", "/w/src", "demo", "env"},
			envVars: map[string]string{"FERN_TEST_SET": "yes"},
			want: command{
				name: "exec", socket: sock, env: "demo", dir: "/w/src",
				vars: []string{"FERN_TEST_SET=yes", "A=b"}, argv: []string{"env"},
			},
		},
		{
			name: "exec without --",
			args: []string{"exec", "demo", "ls", "-la"},
			want: command{name: "exec", socket: sock, env: "demo", argv: []string{"ls", "-la"}},
		},
		{
			name: "exec flags after env belong to the command",
			args: []string{"exec", "demo", "-i", "x"},
			want: command{name: "exec", socket: sock, env: "demo", argv: []string{"-i", "x"}},
		},
		{
			name: "snapshot with id",
			args: []string{"snapshot", "demo", "s1"},
			want: command{name: "snapshot", socket: sock, env: "demo", snap: "s1"},
		},
		{
			name: "snapshot without id",
			args: []string{"snapshot", "demo"},
			want: command{name: "snapshot", socket: sock, env: "demo"},
		},
		{
			name: "restore",
			args: []string{"restore", "demo", "s1"},
			want: command{name: "restore", socket: sock, env: "demo", snap: "s1"},
		},
		{
			name: "socket flag",
			args: []string{"--socket", "/tmp/f.sock", "rm", "demo"},
			want: command{name: "rm", socket: "/tmp/f.sock", env: "demo"},
		},
		{
			name:    "FERN_SOCKET",
			args:    []string{"wake", "demo"},
			envSock: "/tmp/env.sock",
			want:    command{name: "wake", socket: "/tmp/env.sock", env: "demo"},
		},
		{
			name:    "socket flag beats FERN_SOCKET",
			args:    []string{"-socket", "/tmp/f.sock", "sleep", "demo"},
			envSock: "/tmp/env.sock",
			want:    command{name: "sleep", socket: "/tmp/f.sock", env: "demo"},
		},
		{name: "no command", args: nil, wantErr: true},
		{name: "unknown command", args: []string{"frobnicate"}, wantErr: true},
		{name: "unknown flag", args: []string{"create", "--nope", "demo"}, wantErr: true},
		{name: "restore needs a snapshot", args: []string{"restore", "demo"}, wantErr: true},
		{name: "snapshot needs an env", args: []string{"snapshot"}, wantErr: true},
		{name: "rm needs an env", args: []string{"rm"}, wantErr: true},
		{name: "too many args", args: []string{"rm", "a", "b"}, wantErr: true},
		{name: "ls takes no args", args: []string{"ls", "demo"}, wantErr: true},
		{name: "rmsnap needs a snapshot", args: []string{"rmsnap", "demo"}, wantErr: true},
		{name: "which needs a path", args: []string{"which"}, wantErr: true},
		{name: "exec needs a command", args: []string{"exec", "demo"}, wantErr: true},
		{name: "exec needs a command after --", args: []string{"exec", "demo", "--"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("FERN_SOCKET", tt.envSock)
			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}
			got, err := parse(tt.args)
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("parse(%q) error = %v, want error: %t", tt.args, err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.want, got, cmp.AllowUnexported(command{})); diff != "" {
				t.Errorf("parse(%q) mismatch (-want +got):\n%s", tt.args, diff)
			}
		})
	}
}

// newDaemon serves handler on a unix socket, as the daemon would, and returns
// the socket's path.
func newDaemon(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fern") // t.TempDir can be too long for a socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	ln, err := new(net.ListenConfig).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(handler)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return socket
}

// create --repo streams a tar. A daemon that answers without reading it all
// (the environment exists, or the request is refused) breaks tar's pipe;
// that must not turn into a failure, nor hide the daemon's own error.
func TestCreateFromDirUnreadUpload(t *testing.T) {
	repo := t.TempDir()
	// More than a pipe holds, so tar is still writing when the answer comes.
	big := make([]byte, 4<<20)
	if err := os.WriteFile(filepath.Join(repo, "big"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		status   int
		body     string
		wantCode int
		wantErr  string
	}{
		{name: "exists", status: http.StatusOK, body: `{"id":"demo","state":"asleep"}`, wantCode: 0},
		{name: "refused", status: http.StatusBadRequest, body: `{"error":"invalid","message":"bad id"}`, wantCode: 1, wantErr: "bad id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			socket := newDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), []string{"--socket", socket, "create", "demo", "--repo", repo}, nil, &stdout, &stderr)
			if code != tt.wantCode || !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("fern create --repo = %d, stderr %q; want %d and %q", code, stderr.String(), tt.wantCode, tt.wantErr)
			}
		})
	}
}
