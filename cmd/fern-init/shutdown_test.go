package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nebler/fern/internal/boxexec"
)

func TestShutdown(t *testing.T) {
	for _, tt := range []struct {
		name, method, token, uid string
		want                     int
		noIdentity               bool
	}{
		{name: "missing token", method: http.MethodPost, want: http.StatusUnauthorized},
		{name: "wrong token", method: http.MethodPost, token: "Bearer wrong", want: http.StatusUnauthorized},
		{name: "wrong method", method: http.MethodGet, token: "Bearer tok", want: http.StatusMethodNotAllowed},
		{name: "missing pod", method: http.MethodPost, token: "Bearer tok", want: http.StatusPreconditionFailed},
		{name: "replaced pod", method: http.MethodPost, token: "Bearer tok", uid: `"old-pod"`, want: http.StatusPreconditionFailed},
		{name: "old template", method: http.MethodPost, token: "Bearer tok", uid: `""`, noIdentity: true, want: http.StatusPreconditionFailed},
		{name: "authorized", method: http.MethodPost, token: "Bearer tok", uid: `"pod-1"`, want: http.StatusNoContent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			mux := http.NewServeMux()
			uid := "pod-1"
			if tt.noIdentity {
				uid = ""
			}
			mux.Handle("POST "+boxexec.StopPath, shutdown("tok", uid, cancel))
			srv := httptest.NewServer(mux)
			defer srv.Close()
			// A repeated request must neither panic nor restart shutdown.
			for range 2 {
				req, err := http.NewRequestWithContext(t.Context(), tt.method, srv.URL+boxexec.StopPath, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", tt.token)
				req.Header.Set("If-Match", tt.uid)
				resp, err := srv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				if err := resp.Body.Close(); err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != tt.want {
					t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
				}
				if tt.want == http.StatusNoContent {
					<-ctx.Done()
				} else if ctx.Err() != nil {
					t.Errorf("unauthorized shutdown canceled context: %v", ctx.Err())
				}
			}
		})
	}
}
