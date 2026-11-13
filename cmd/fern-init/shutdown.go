package main

import (
	"context"
	"net/http"
	"strconv"

	"github.com/nebler/fern/internal/boxexec"
)

// shutdown starts the same service shutdown as SIGTERM, for a request that
// names this wake of the box, wakeID.
func shutdown(token, wakeID string, cancel context.CancelFunc) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !boxexec.Authorized(w, r, token) {
			return
		}
		// A second fence behind the token, which is this wake's own: a delayed
		// request to an address the next wake reuses must not stop that one.
		if wakeID == "" || r.Header.Get("If-Match") != strconv.Quote(wakeID) {
			http.Error(w, "another wake of the box", http.StatusPreconditionFailed)
			return
		}
		defer cancel()
		w.WriteHeader(http.StatusNoContent)
		// With no services, cancellation can close the server immediately.
		// Flush the acknowledgement before allowing that shutdown to start.
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
	})
}
