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
    port: 5432
    env:
      PGDATA: .pg
  web:
    run: npm start
`,
			want: map[string]service{
				"db": {
					Run: "postgres -D .pg", Stop: "pg_ctl stop -D .pg", Ready: "pg_isready",
					Port: 5432, Env: map[string]string{"PGDATA": ".pg"},
				},
				"web": {Run: "npm start"},
			},
		},
		{name: "empty file", yaml: ""},
		{name: "no services", yaml: "services: {}\n"},
		{name: "missing run", yaml: "services:\n  db:\n    port: 5432\n", wantErr: true},
		{name: "unknown field", yaml: "services:\n  db:\n    run: x\n    restart: always\n", wantErr: true},
		{name: "unknown top-level field", yaml: "version: 1\nservices: {}\n", wantErr: true},
		{name: "bad port", yaml: "services:\n  db:\n    run: x\n    port: high\n", wantErr: true},
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

func TestServiceEnviron(t *testing.T) {
	tests := []struct {
		name string
		svc  service
		want []string
	}{
		{name: "nothing added", svc: service{Run: "x"}, want: []string{"PATH=/bin"}},
		{
			name: "port and env",
			svc:  service{Run: "x", Port: 8080, Env: map[string]string{"A": "1", "B": "2"}},
			want: []string{"PATH=/bin", "PORT=8080", "A=1", "B=2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Spare capacity would let an append write into the caller's array.
			base := make([]string, 1, 8)
			base[0] = "PATH=/bin"
			got := tt.svc.environ(base)
			// Env comes from a map, so its order is not defined.
			sortEnv := cmpopts.SortSlices(func(a, b string) bool { return a < b })
			if diff := cmp.Diff(tt.want, got, sortEnv); diff != "" {
				t.Errorf("environ() mismatch (-want +got):\n%s", diff)
			}
			if got := base[:cap(base)][1]; got != "" {
				t.Errorf("environ() wrote %q into the caller's slice", got)
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

// A stop command that ends the service must not get it restarted.
func TestSupervisorStopDoesNotRestart(t *testing.T) {
	s, dir := newService(t, service{
		Run:  "echo run >> runs; echo $$ > pid; exec sleep 600",
		Stop: `until [ -s pid ]; do sleep 0.01; done; kill "$(cat pid)"`,
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
	ps, err := exec.LookPath("ps")
	if err != nil {
		t.Fatalf("ps is needed to see the service's child: %v", err)
	}
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

	data, err := os.ReadFile(filepath.Join(dir, "child"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	// SIGKILL is delivered asynchronously, so the child may take a moment to
	// die. A zombie counts as dead: nothing in the test reaps orphans.
	var state string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		// ps exits 1 when the pid is gone; the empty output says so.
		out, _ := exec.CommandContext(t.Context(), ps, "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		state = strings.TrimSpace(string(out))
		if state == "" || strings.HasPrefix(state, "Z") {
			return
		}
	}
	t.Errorf("the service's child (pid %d) survived stop in state %q", pid, state)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
