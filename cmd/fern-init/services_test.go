package main

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// The test binary is also the service the on-demand tests start.
	if addr := os.Getenv("FERN_TEST_ECHO"); addr != "" {
		serveEcho(addr)
		return
	}
	goleak.VerifyTestMain(m)
}

func TestParseServices(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    map[string]service
		wantErr bool
	}{
		{
			name: "full",
			yaml: `
services:
  db:
    run: postgres -D .pg
    stop: pg_ctl stop -D .pg
    ready: pg_isready
  web:
    run: npm start
`,
			want: map[string]service{
				"db":  {Run: "postgres -D .pg", Stop: "pg_ctl stop -D .pg", Ready: "pg_isready"},
				"web": {Run: "npm start"},
			},
		},
		{name: "empty file", yaml: ""},
		{name: "no services", yaml: "services: {}\n"},
		{name: "missing run", yaml: "services:\n  db:\n    ready: x\n", wantErr: true},
		{name: "unknown field", yaml: "services:\n  db:\n    run: x\n    restart: always\n", wantErr: true},
		{
			name: "on demand",
			yaml: "services:\n  db:\n    run: x\n    listen: unix:/tmp/.s.PGSQL.5432\n  web:\n    run: npm start\n    listen: tcp:3000\n",
			want: map[string]service{"db": {Run: "x", Listen: "unix:/tmp/.s.PGSQL.5432"}, "web": {Run: "npm start", Listen: "tcp:3000"}},
		},
		{name: "bad listen", yaml: "services:\n  db:\n    run: x\n    listen: 5432\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseServices([]byte(tt.yaml))
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("parseServices(%q) error = %v, want error: %t", tt.yaml, err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("parseServices(%q) mismatch (-want +got):\n%s", tt.yaml, diff)
			}
		})
	}
}

// pathEnv is the whole environment of the scripts the tests run.
const pathEnv = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func newService(t *testing.T, svc service) (s *supervisor, dir string) {
	t.Helper()
	dir = t.TempDir()
	return startService(t.Context(), "svc", svc, dir, []string{pathEnv}, slog.New(slog.DiscardHandler)), dir
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// A service that keeps failing restarts after 1s, 2s, 4s, ...
func TestSupervisorBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, dir := newService(t, service{Run: "echo run >> runs; exit 1"})
		// The bubble's clock only moves while every goroutine waits, so the
		// service's real run time doesn't count: it runs at 0s, 1s, 3s, 7s.
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if got := countLines(t, filepath.Join(dir, "runs")); got != 4 {
			t.Errorf("service ran %d times in 10s, want 4", got)
		}
		s.stop(t.Context())
	})
}

// A stop command that ends the service must not get it restarted. It goes on
// past the shortest backoff, so a restart would have happened by then.
func TestSupervisorStopDoesNotRestart(t *testing.T) {
	s, dir := newService(t, service{
		Run:  "echo run >> runs; echo $$ > pid; exec sleep 600",
		Stop: `until [ -s pid ]; do sleep 0.01; done; kill "$(cat pid)"; sleep 1.5`,
	})
	s.stop(t.Context())
	if got := countLines(t, filepath.Join(dir, "runs")); got != 1 {
		t.Errorf("service ran %d times, want 1", got)
	}
}

// Without a stop command, stop sends SIGTERM to the service's process group,
// which ends it long before the SIGKILL that would follow.
func TestSupervisorStopTerminates(t *testing.T) {
	s, _ := newService(t, service{Run: "sleep 600 & wait"})
	start := time.Now()
	s.stop(t.Context())
	if took := time.Since(start); took >= killDelay {
		t.Errorf("stop took %v, want well under %v: SIGTERM didn't end the service", took, killDelay)
	}
}

// A service whose processes ignore SIGTERM is SIGKILLed, children included.
func TestSupervisorStopKillsGroup(t *testing.T) {
	s, dir := newService(t, service{
		Run: `trap "" TERM; sh -c 'trap "" TERM; echo $$ > child; exec sleep 600' & wait`,
		// Stop only once the child is there to be killed.
		Stop: `until [ -s child ]; do sleep 0.01; done`,
	})
	start := time.Now()
	s.stop(t.Context())
	if took := time.Since(start); took > killDelay+2*time.Second {
		t.Errorf("stop took %v, want about %v", took, killDelay)
	}
	checkGone(t, filepath.Join(dir, "child"))
}

// What a service's script leaves running when it exits is ended before the
// service is restarted: here a server that would otherwise run twice.
func TestSupervisorEndsLeftoversBeforeRestart(t *testing.T) {
	s, dir := newService(t, service{Run: `sh -c 'echo $$ >> servers; exec sleep 600' & sleep 0.2; exit 1`})
	// Past the first restart. Nothing reaps the tests' orphans, so the
	// leftover stays a zombie in the group until its SIGKILL.
	time.Sleep(killDelay + minBackoff + time.Second)
	s.stop(t.Context())
	data, err := os.ReadFile(filepath.Join(dir, "servers"))
	if err != nil {
		t.Fatal(err)
	}
	servers := strings.Fields(string(data))
	if len(servers) < 2 {
		t.Fatalf("the service ran %d times, want a restart", len(servers))
	}
	for i := range servers[:len(servers)-1] {
		path := filepath.Join(dir, "server"+strconv.Itoa(i))
		if err := os.WriteFile(path, []byte(servers[i]), 0o644); err != nil {
			t.Fatal(err)
		}
		checkGone(t, path)
	}
}

// A child that ignores SIGTERM is SIGKILLed after its script has exited.
func TestSupervisorStopKillsLeftovers(t *testing.T) {
	s, dir := newService(t, service{
		Run:  `sh -c 'trap "" TERM; echo $$ > child; exec sleep 600' & until [ -s child ]; do sleep 0.01; done`,
		Stop: `until [ -s child ]; do sleep 0.01; done`,
	})
	s.stop(t.Context())
	checkGone(t, filepath.Join(dir, "child"))
}

// checkGone fails t unless the process whose pid the file at path holds
// dies within 2s: SIGKILL is delivered asynchronously. A zombie counts as
// dead, since nothing in the tests reaps orphans.
func checkGone(t *testing.T, path string) {
	t.Helper()
	ps, err := exec.LookPath("ps")
	if err != nil {
		t.Fatalf("ps is needed to see the process: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	var state string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		// ps exits 1 when the pid is gone; the empty output says so.
		out, _ := exec.CommandContext(t.Context(), ps, "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		state = strings.TrimSpace(string(out))
		if state == "" || strings.HasPrefix(state, "Z") {
			return
		}
	}
	t.Errorf("process %d survived in state %q", pid, state)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// A failing script's error quotes the start of its output, however much it
// printed.
func TestShellQuotesBoundedOutput(t *testing.T) {
	err := shell(t.Context(), 10*time.Second, "echo first; head -c 1048576 /dev/zero; exit 3", t.TempDir(), []string{pathEnv})
	if err == nil {
		t.Fatal("shell() error = nil, want the script's exit status")
	}
	msg := err.Error()
	if !strings.Contains(msg, "first") || !strings.Contains(msg, "more bytes") || len(msg) > maxShellOutput+100 {
		t.Errorf("shell() error is %d bytes, want the first %d bytes of output and a count of the rest", len(msg), maxShellOutput)
	}
}
