package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// The kubelet probes /healthz every second. Once the box has been ready, a
// result is good for readyCacheFor, so the ready scripts don't run that often.
const readyCacheFor = 5 * time.Second

// readiness answers the kubelet's readiness probe on /healthz. Until the box
// is first ready every probe runs the checks; after that a result is reused
// for readyCacheFor. Probes that arrive while the checks run share that run.
type readiness struct {
	check func(context.Context) error // runs the ready checks

	mu      sync.Mutex
	running *checkRun // nil when no run is in flight
	last    *checkRun // the last finished run; nil before the first
	passed  bool      // whether any run has passed
}

// checkRun is one run of the checks.
type checkRun struct {
	done     chan struct{} // closed when err and finished are set
	err      error
	finished time.Time
}

func newReadiness(check func(context.Context) error) *readiness {
	return &readiness{check: check}
}

func (r *readiness) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if err := r.ready(req.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = fmt.Fprintln(w, "ok")
}

// ready returns the cached result if it is fresh, else joins or starts a run.
func (r *readiness) ready(ctx context.Context) error {
	r.mu.Lock()
	if r.passed && time.Since(r.last.finished) < readyCacheFor {
		defer r.mu.Unlock()
		return r.last.err
	}
	if run := r.running; run != nil {
		r.mu.Unlock()
		select {
		case <-run.done:
			return run.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	run := &checkRun{done: make(chan struct{})}
	r.running = run
	r.mu.Unlock()

	// Other probes wait for this run, so a probe that gives up must not
	// cancel it. The checks bound themselves.
	run.err = r.check(context.WithoutCancel(ctx))
	run.finished = time.Now()
	close(run.done)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.running, r.last = nil, run
	r.passed = r.passed || run.err == nil
	return run.err
}
