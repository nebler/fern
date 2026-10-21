package main

import (
	"context"
	"fmt"
	"net/http"
)

// readiness answers the kubelet's readiness probe on /healthz.
type readiness struct {
	check func(context.Context) error // runs the ready checks
}

func newReadiness(check func(context.Context) error) *readiness {
	return &readiness{check: check}
}

func (r *readiness) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if err := r.check(req.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = fmt.Fprintln(w, "ok")
}
