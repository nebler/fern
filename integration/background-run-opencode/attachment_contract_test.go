package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nebler/fern/internal/opencode"
)

func TestSerialAPIProjectionsRequireOwnedSessionEnvelopes(t *testing.T) {
	const session = "ses_00000000000000000000000000000001"
	validList := `{"data":[{"id":"` + session + `"}]}`
	validActive := `{"data":{"` + session + `":{"type":"running"}}}`
	for _, test := range []struct {
		name, list, active string
		status             int
		valid              bool
	}{
		{"owned", validList, validActive, http.StatusOK, true},
		{"bare list", `[{"id":"` + session + `"}]`, validActive, http.StatusOK, false},
		{"missing data", `{}`, validActive, http.StatusOK, false},
		{"null data", `{"data":null}`, validActive, http.StatusOK, false},
		{"empty list", `{"data":[]}`, validActive, http.StatusOK, false},
		{"foreign list", `{"data":[{"id":"foreign"}]}`, validActive, http.StatusOK, false},
		{"extra session", `{"data":[{"id":"` + session + `"},{"id":"foreign"}]}`, validActive, http.StatusOK, false},
		{"bare active", validList, `{"` + session + `":{"type":"running"}}`, http.StatusOK, false},
		{"empty active", validList, `{"data":{}}`, http.StatusOK, false},
		{"foreign active", validList, `{"data":{"foreign":{"type":"running"}}}`, http.StatusOK, false},
		{"inactive", validList, `{"data":{"` + session + `":{"type":"idle"}}}`, http.StatusOK, false},
		{"extra active", validList, `{"data":{"` + session + `":{"type":"running"},"foreign":{"type":"running"}}}`, http.StatusOK, false},
		{"malformed", validList, `{`, http.StatusOK, false},
		{"status", validList, validActive, http.StatusForbidden, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				username, password, ok := r.BasicAuth()
				if !ok || username != opencode.AttachmentUsername || password != "test-secret" || r.Method != http.MethodGet {
					t.Error("attachment request lost its authentication or method")
				}
				w.WriteHeader(test.status)
				switch r.URL.Path {
				case "/api/session":
					_, _ = fmt.Fprint(w, test.list)
				case "/api/session/active":
					_, _ = fmt.Fprint(w, test.active)
				default:
					t.Errorf("unexpected path %q", r.URL.Path)
				}
			}))
			defer server.Close()
			err := verifySerialAPIProjections(context.Background(), server.URL, "test-secret", session)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%t error=%v", test.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "test-secret") {
				t.Fatal("diagnostic disclosed attachment credentials")
			}
		})
	}
}

func TestSerialDeniedMethodsRequireForbidden(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusOK, http.StatusNotFound, http.StatusMethodNotAllowed} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(status)
			}))
			defer server.Close()
			err := verifySerialDeniedMethods(context.Background(), server.URL, "test-secret", "owned")
			if (err == nil) != (status == http.StatusForbidden) {
				t.Fatalf("status=%d error=%v", status, err)
			}
			if status == http.StatusForbidden && requests.Load() != 45 {
				t.Fatalf("checked %d denied requests, want 45", requests.Load())
			}
		})
	}
}

func TestSerialAttachmentRequestDoesNotFollowRedirects(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, "/redirect-target", http.StatusFound)
	}))
	defer server.Close()
	status, _, err := serialAttachmentRequest(context.Background(), server.URL, "test-secret", http.MethodGet, "/api/session")
	if err != nil || status != http.StatusFound || requests.Load() != 1 {
		t.Fatalf("status=%d requests=%d error=%v", status, requests.Load(), err)
	}
}

func TestSerialAttachmentRequestBoundsBodiesAndRedactsFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, strings.Repeat("x", (4<<20)+1))
	}))
	defer server.Close()
	_, body, err := serialAttachmentRequest(context.Background(), server.URL, "test-secret", http.MethodGet, "/api/session")
	if err == nil || body != nil || strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("oversized body length=%d error=%v", len(body), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, body, err = serialAttachmentRequest(ctx, server.URL, "test-secret", http.MethodGet, "/api/session")
	if err == nil || body != nil || strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("canceled request body length=%d error=%v", len(body), err)
	}
}
