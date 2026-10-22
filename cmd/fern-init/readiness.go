package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"
)

// The daemon asks a waking box whether its services are ready, and waits for
// the answer.
const (
	// retryEvery paces the checks while the daemon waits: back to back, they
	// slowed the boxes' own start (ten boxes starting Postgres at once took
	// 21s instead of 6s).
	retryEvery = 100 * time.Millisecond
	// maxWait bounds ?wait=.
	maxWait = 10 * time.Second
	// readyTimeout bounds each check. A timed-out check is killed and gets
	// shellWaitDelay to let go of its output, so a round takes at most 2.25s.
	readyTimeout = 2 * time.Second
)

// readiness answers whether the box's services are ready. With
// ?wait=DURATION (at most maxWait) it answers as soon as they are, or when the
// time is up.
func readiness(check func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wait, _ := time.ParseDuration(r.URL.Query().Get("wait"))
		deadline := time.Now().Add(min(wait, maxWait))
		err := check(r.Context())
		for err != nil && time.Now().Before(deadline) {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(retryEvery):
			}
			err = check(r.Context())
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintln(w, "ok")
	}
}

// checkServices runs every service's ready check at once and returns the
// failures.
func checkServices(ctx context.Context, services map[string]service, dir string, env []string) error {
	names := slices.Sorted(maps.Keys(services)) // a stable order for the errors
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		if services[name].Ready == "" {
			continue
		}
		wg.Go(func() {
			if err := shell(ctx, readyTimeout, services[name].Ready, dir, env); err != nil {
				errs[i] = fmt.Errorf("%s not ready: %w", name, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
