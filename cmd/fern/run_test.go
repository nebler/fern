package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/api"
)

// fakeDaemon answers the CLI's requests on a unix socket as the daemon
// would, for environment demo with snapshot s1 and a worktree at /wt, and
// records each request as "METHOD /path?query body".
type fakeDaemon struct {
	socket string
	mu     sync.Mutex
	got    []string
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{socket: filepath.Join(t.TempDir(), "fern.sock")}
	lis, err := new(net.ListenConfig).Listen(t.Context(), "unix", d.socket)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /envs", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("path") {
		case "":
			reply(w, []api.Env{{ID: "demo", Workspace: "/pool/envs/demo/live", Created: created, State: "asleep"}})
		case "/wt":
			reply(w, []api.Env{{ID: "wt", Workspace: "/wt", Created: created}})
		default:
			reply(w, []api.Env{})
		}
	})
	mux.HandleFunc("GET /envs/{id}/snapshots", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []api.Snapshot{{ID: "s1", Created: created}})
	})
	mux.HandleFunc("POST /github/token", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Repository string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Repository != "owner/app" {
			w.WriteHeader(http.StatusNotFound)
			reply(w, api.Error{Message: "the GitHub App isn't installed on this repository"})
			return
		}
		reply(w, map[string]string{"token": "ghs_x"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			w.WriteHeader(http.StatusNotFound)
			reply(w, api.Error{Message: `environment "missing": not found`})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.got = append(d.got, strings.TrimSpace(r.Method+" "+r.URL.RequestURI()+" "+string(bytes.TrimSpace(body))))
		d.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		mux.ServeHTTP(w, r)
	})}
	done := make(chan struct{})
	go func() { _ = srv.Serve(lis); close(done) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })
	return d
}

// Each command sends the daemon its request and prints what it answers; a
// failure is exit code 1 with the daemon's reason, a usage error 2.
func TestRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		stdin    string
		code     int
		requests []string
		stdout   string // a line the output must have
		stderr   string
	}{
		{name: "create", args: []string{"create", "demo"}, requests: []string{"PUT /envs/demo?path="}, stdout: "created environment demo"},
		{
			name: "create from a tar stream", args: []string{"create", "--seed", "demo"}, stdin: "a tar stream",
			requests: []string{"PUT /envs/demo?path= a tar stream"}, stdout: "created environment demo",
		},
		{
			name: "create at a path", args: []string{"create", "--path", "/pool/worktrees/r/b", "wt"},
			requests: []string{"PUT /envs/wt?path=%2Fpool%2Fworktrees%2Fr%2Fb"},
		},
		{name: "ls", args: []string{"ls"}, requests: []string{"GET /envs"}, stdout: "demo  asleep  2026-10-29T12:00:00Z  /pool/envs/demo/live"},
		{name: "which", args: []string{"which", "/wt"}, requests: []string{"GET /envs?path=%2Fwt"}, stdout: "wt"},
		{name: "which finds none", args: []string{"which", "/elsewhere"}, code: 1, requests: []string{"GET /envs?path=%2Felsewhere"}, stderr: "no environment at /elsewhere"},
		{name: "rm", args: []string{"rm", "demo"}, requests: []string{"DELETE /envs/demo"}},
		{name: "rm of an unknown environment", args: []string{"rm", "missing"}, code: 1, requests: []string{"DELETE /envs/missing"}, stderr: `fern rm: environment "missing": not found`},
		{
			name: "snapshot again", args: []string{"snapshot", "--replace", "demo", "s1"},
			requests: []string{`POST /envs/demo/snapshots {"id":"s1","replace":true}`}, stdout: "snapshot demo/s1 took",
		},
		{name: "snapshots", args: []string{"snapshots", "demo"}, requests: []string{"GET /envs/demo/snapshots"}, stdout: "s1        2026-10-29T12:00:00Z"},
		{name: "rmsnap", args: []string{"rmsnap", "demo", "a", "b"}, requests: []string{"DELETE /envs/demo/snapshots/a", "DELETE /envs/demo/snapshots/b"}},
		{name: "restore", args: []string{"restore", "demo", "s1"}, requests: []string{`POST /envs/demo/restore {"snapshot":"s1"}`}, stdout: "restore demo to s1 took"},
		{name: "sleep", args: []string{"sleep", "demo"}, requests: []string{"POST /envs/demo/sleep"}, stdout: "sleep demo took"},
		{name: "wake", args: []string{"wake", "demo"}, requests: []string{"POST /envs/demo/wake"}, stdout: "wake demo took"},
		{name: "prewake", args: []string{"prewake", "demo"}, requests: []string{"POST /envs/demo/prewake"}, stdout: "prewake demo took"},
		{name: "prebuild", args: []string{"prebuild", "app"}, stdin: "a checkout", requests: []string{"PUT /goldens/app a checkout"}},
		{name: "no command", args: nil, code: 2, stderr: "usage: fern <create|"},
		{name: "unknown command", args: []string{"frobnicate"}, code: 2, stderr: `unknown command "frobnicate"`},
		{name: "image without load", args: []string{"image", "save"}, code: 2, stderr: `unknown command "save": want load`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDaemon(t)
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), d.socket, tc.args, strings.NewReader(tc.stdin), &stdout, &stderr)
			if code != tc.code {
				t.Errorf("exit code %d, want %d (stderr %q)", code, tc.code, stderr.String())
			}
			d.mu.Lock()
			got := d.got
			d.mu.Unlock()
			if diff := cmp.Diff(tc.requests, got); diff != "" {
				t.Errorf("requests mismatch (-want +got):\n%s", diff)
			}
			if !strings.Contains(stdout.String(), tc.stdout) {
				t.Errorf("stdout %q lacks %q", stdout.String(), tc.stdout)
			}
			if !strings.Contains(stderr.String(), tc.stderr) {
				t.Errorf("stderr %q lacks %q", stderr.String(), tc.stderr)
			}
		})
	}
}
