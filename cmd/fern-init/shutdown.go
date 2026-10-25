package main

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strconv"
)

// shutdown starts the same service shutdown as SIGTERM. The daemon sends it
// only after Kubernetes has durable intent to remove this box.
func shutdown(token, uid string, cancel context.CancelFunc) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// An environment keeps its token across wakes. A delayed request to
		// an IP reused by its next pod must not stop that replacement.
		if uid == "" || r.Header.Get("If-Match") != strconv.Quote(uid) {
			http.Error(w, "pod changed", http.StatusPreconditionFailed)
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
