package main

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strconv"
)

// shutdown starts the same service shutdown as SIGTERM, for a request that
// names this wake of the box, wakeID.
func shutdown(token, wakeID string, cancel context.CancelFunc) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// An environment keeps its token across wakes. A delayed request to
		// an address its next wake reuses must not stop that one.
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
