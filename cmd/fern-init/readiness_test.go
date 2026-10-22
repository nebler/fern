package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nebler/fern/internal/boxexec"
)

func TestCheckServices(t *testing.T) {
	// A round of checks must end within 2.25s, however its scripts behave.
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
			err := checkServices(t.Context(), services, t.TempDir(), []string{pathEnv})
			if took := time.Since(start); took > budget {
				t.Errorf("checkServices() took %v, want at most %v", took, budget)
			}
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Errorf("checkServices() error = %v, want error: %t", err, tt.wantErr)
			}
		})
	}
}

// A check that hangs is killed with what it started, or every round of
// checks would leave a process behind.
func TestCheckKillsGroup(t *testing.T) {
	dir := t.TempDir()
	services := map[string]service{"db": {Run: "x", Ready: `sleep 600 & echo $! > child; wait`}}
	if err := checkServices(t.Context(), services, dir, []string{pathEnv}); err == nil {
		t.Fatal("checkServices() with a hanging check succeeded, want an error")
	}
	checkGone(t, filepath.Join(dir, "child"))
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
		{name: "ready", upAfter: 0, wait: "", wantCode: http.StatusOK, wantTook: 0, wantChecks: 1},
		{name: "not ready, no wait", upAfter: time.Hour, wait: "", wantCode: http.StatusServiceUnavailable, wantTook: 0, wantChecks: 1},
		{name: "ready during the wait", upAfter: time.Second, wait: "5s", wantCode: http.StatusOK, wantTook: time.Second, wantChecks: 11},
		{name: "not ready in time", upAfter: time.Hour, wait: "1s", wantCode: http.StatusServiceUnavailable, wantTook: time.Second, wantChecks: 11},
		{name: "wait capped", upAfter: time.Hour, wait: "1h", wantCode: http.StatusServiceUnavailable, wantTook: maxWait, wantChecks: 101},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var checks atomic.Int32
				start := time.Now()
				h := readiness(func(context.Context) error {
					checks.Add(1)
					if time.Since(start) < tt.upAfter {
						return errors.New("not yet")
					}
					return nil
				})
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, boxexec.ReadyPath+"?wait="+tt.wait, nil))
				if w.Code != tt.wantCode || time.Since(start) != tt.wantTook || int(checks.Load()) != tt.wantChecks {
					t.Errorf("GET %s?wait=%s = %d after %v with %d checks, want %d after %v with %d", boxexec.ReadyPath,
						tt.wait, w.Code, time.Since(start), checks.Load(), tt.wantCode, tt.wantTook, tt.wantChecks)
				}
			})
		})
	}
}
