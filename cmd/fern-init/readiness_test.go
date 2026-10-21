package main

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
)

func probe(t *testing.T, r *readiness) int {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
	return w.Code
}

// Until the box is first ready a failure is reused for 100ms; after that a
// result is reused for 5s.
func TestReadinessCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		services := map[string]service{"db": {Run: "x", Ready: "echo check >> checks; test -e up"}}
		r := newReadiness(func(ctx context.Context) error {
			return ready(ctx, services, dir, []string{pathEnv})
		})
		up := filepath.Join(dir, "up")

		// The steps run in order, each after the ones before it.
		steps := []struct {
			name       string
			after      time.Duration // since the step before
			up         bool
			wantCode   int
			wantChecks int // checks run so far
		}{
			{name: "not ready", up: false, wantCode: http.StatusServiceUnavailable, wantChecks: 1},
			{name: "not ready, asked again at once", up: false, wantCode: http.StatusServiceUnavailable, wantChecks: 1},
			{name: "not ready, checked again", after: 100 * time.Millisecond, up: false, wantCode: http.StatusServiceUnavailable, wantChecks: 2},
			{name: "first ready", after: time.Second, up: true, wantCode: http.StatusOK, wantChecks: 3},
			{name: "cached", up: false, wantCode: http.StatusOK, wantChecks: 3},
			{name: "still cached", after: 4900 * time.Millisecond, up: false, wantCode: http.StatusOK, wantChecks: 3},
			{name: "expired", after: 100 * time.Millisecond, up: false, wantCode: http.StatusServiceUnavailable, wantChecks: 4},
			{name: "failure cached", after: time.Second, up: true, wantCode: http.StatusServiceUnavailable, wantChecks: 4},
			{name: "failure expired", after: 4 * time.Second, up: true, wantCode: http.StatusOK, wantChecks: 5},
		}
		for _, step := range steps {
			time.Sleep(step.after)
			if step.up {
				if err := os.WriteFile(up, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(up); err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
			if got := probe(t, r); got != step.wantCode {
				t.Errorf("%s: /healthz = %d, want %d", step.name, got, step.wantCode)
			}
			if got := countLines(t, filepath.Join(dir, "checks")); got != step.wantChecks {
				t.Errorf("%s: checks ran %d times, want %d", step.name, got, step.wantChecks)
			}
		}
	})
}

// Probes that arrive while the checks run wait for that run's result.
func TestReadinessSingleFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var checks atomic.Int32
		r := newReadiness(func(context.Context) error {
			checks.Add(1)
			<-release
			return nil
		})

		const probes = 5
		codes := make([]int, probes)
		var wg sync.WaitGroup
		for i := range probes {
			wg.Go(func() { codes[i] = probe(t, r) })
		}
		synctest.Wait() // every probe is in, and the checks are running
		if got := checks.Load(); got != 1 {
			t.Errorf("%d concurrent probes ran the checks %d times, want 1", probes, got)
		}
		close(release)
		wg.Wait()

		want := []int{http.StatusOK, http.StatusOK, http.StatusOK, http.StatusOK, http.StatusOK}
		if diff := cmp.Diff(want, codes); diff != "" {
			t.Errorf("/healthz codes mismatch (-want +got):\n%s", diff)
		}
		if got := checks.Load(); got != 1 {
			t.Errorf("checks ran %d times, want 1", got)
		}
	})
}

func TestReady(t *testing.T) {
	// The kubelet's probe times out after 3s; /healthz must answer before.
	const budget = 2500 * time.Millisecond
	tests := []struct {
		name    string
		ready   []string // one service per check
		wantErr bool
	}{
		{name: "no checks"},
		{name: "all ready", ready: []string{"true", "true"}},
		{name: "one not ready", ready: []string{"true", "exit 1"}, wantErr: true},
		{name: "checks run at once", ready: []string{"sleep 1", "sleep 1", "sleep 1"}},
		{name: "check hangs", ready: []string{"sleep 10"}, wantErr: true},
		{name: "check and its child hang", ready: []string{"sleep 10 & sleep 10"}, wantErr: true},
		{name: "child outlives a passing check", ready: []string{"sleep 5 & true"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			services := make(map[string]service)
			for i, script := range tt.ready {
				services["svc"+strconv.Itoa(i)] = service{Run: "x", Ready: script}
			}
			start := time.Now()
			err := ready(t.Context(), services, t.TempDir(), []string{pathEnv})
			if took := time.Since(start); took > budget {
				t.Errorf("ready() took %v, want at most %v", took, budget)
			}
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Errorf("ready() error = %v, want error: %t", err, tt.wantErr)
			}
		})
	}
}

// With ?wait= the answer comes as soon as the box is ready, and the checks
// run no more than every 100ms meanwhile.
func TestReadinessWait(t *testing.T) {
	tests := []struct {
		name       string
		upAfter    time.Duration // when the box becomes ready
		wait       string
		wantCode   int
		wantTook   time.Duration
		wantChecks int
	}{
		{name: "ready during the wait", upAfter: time.Second, wait: "5s", wantCode: http.StatusOK, wantTook: time.Second, wantChecks: 11},
		{name: "not ready in time", upAfter: time.Hour, wait: "1s", wantCode: http.StatusServiceUnavailable, wantTook: time.Second, wantChecks: 11},
		{name: "wait capped", upAfter: time.Hour, wait: "1h", wantCode: http.StatusServiceUnavailable, wantTook: maxWait, wantChecks: 101},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var checks atomic.Int32
				start := time.Now()
				r := newReadiness(func(context.Context) error {
					checks.Add(1)
					if time.Since(start) < tt.upAfter {
						return errors.New("not yet")
					}
					return nil
				})
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz?wait="+tt.wait, nil))
				if w.Code != tt.wantCode || time.Since(start) != tt.wantTook || int(checks.Load()) != tt.wantChecks {
					t.Errorf("GET /healthz?wait=%s = %d after %v with %d checks, want %d after %v with %d",
						tt.wait, w.Code, time.Since(start), checks.Load(), tt.wantCode, tt.wantTook, tt.wantChecks)
				}
			})
		})
	}
}
