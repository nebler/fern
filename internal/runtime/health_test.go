package runtime

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWaitHealthyUsesBasicAuth(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/health" {
			http.NotFound(writer, request)
			return
		}
		username, password, ok := request.BasicAuth()
		if !ok || username != "opencode" || password != "secret" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = writer.Write([]byte(`{"healthy":true,"version":"1.18.16"}`))
	}))
	defer server.Close()
	endpoint := Endpoint{Host: "127.0.0.1", Port: server.Listener.Addr().(*net.TCPAddr).Port}
	if err := WaitHealthy(context.Background(), endpoint, ServerAuth{Password: "secret"}, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestWaitHealthyReusesConnectionAfterUnhealthyResponse(t *testing.T) {
	var requests, connections int
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write(make([]byte, 1024))
			return
		}
		_, _ = writer.Write([]byte(`{"healthy":true}`))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections++
		}
	}
	server.Start()
	defer server.Close()
	endpoint := Endpoint{Host: "127.0.0.1", Port: server.Listener.Addr().(*net.TCPAddr).Port}
	if err := WaitHealthy(context.Background(), endpoint, ServerAuth{}, time.Second); err != nil {
		t.Fatal(err)
	}
	if connections != 1 {
		t.Fatalf("health checks used %d connections, want 1", connections)
	}
}

func TestWaitHealthyURLUsesV2Auth(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/health" {
			http.NotFound(writer, request)
			return
		}
		username, password, ok := request.BasicAuth()
		if !ok || username != "opencode" || password != "v2-secret" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = writer.Write([]byte(`{"healthy":true,"version":"2.0.0-beta"}`))
	}))
	defer server.Close()
	endpoint := Endpoint{Host: "127.0.0.1", Port: server.Listener.Addr().(*net.TCPAddr).Port}
	if err := WaitHealthy(context.Background(), endpoint, ServerAuth{Password: "v2-secret"}, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestWaitHealthyRejectsFalseOrMalformedHealth(t *testing.T) {
	t.Parallel()
	for _, body := range []string{`{"healthy":false}`, `{}`, `not-json`, `{"healthy":true} {}`} {
		body := body
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(body))
			}))
			defer server.Close()
			endpoint := Endpoint{Host: "127.0.0.1", Port: server.Listener.Addr().(*net.TCPAddr).Port}
			if err := WaitHealthy(context.Background(), endpoint, ServerAuth{}, 50*time.Millisecond); err == nil {
				t.Fatal("invalid health response was accepted")
			}
		})
	}
}

func TestWaitHealthyReportsParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WaitHealthy(ctx, Endpoint{Host: "127.0.0.1", Port: 1}, ServerAuth{}, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Fatalf("cancellation reported as timeout: %v", err)
	}
}

func TestEndpointURLSupportsIPv6(t *testing.T) {
	t.Parallel()
	if got := (Endpoint{Host: "::1", Port: 4096}).URL(); got != "http://[::1]:4096" {
		t.Fatalf("IPv6 URL = %q", got)
	}
}

func TestSpecFingerprintIsStableAndDetectsChanges(t *testing.T) {
	t.Parallel()
	first := Spec{Name: "demo", Image: "image:one", RepoPath: "/repo", MemoryBytes: 1024, Env: map[string]string{"B": "2", "A": "1"}}
	second := Spec{Name: "demo", Image: "image:one", RepoPath: "/repo", MemoryBytes: 1024, Env: map[string]string{"A": "1", "B": "2"}}
	left, err := specFingerprint(first)
	if err != nil {
		t.Fatal(err)
	}
	right, err := specFingerprint(second)
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatal("map iteration order changed spec fingerprint")
	}
	second.Image = "image:two"
	changed, _ := specFingerprint(second)
	if changed == left {
		t.Fatal("image change did not change spec fingerprint")
	}
	second = first
	second.Env = map[string]string{"A": "changed", "B": "changed"}
	valueOnly, _ := specFingerprint(second)
	if valueOnly != left {
		t.Fatal("secret values changed the Docker-visible spec fingerprint")
	}
	second.Env["C"] = "3"
	keyAdded, _ := specFingerprint(second)
	if keyAdded == left {
		t.Fatal("environment key change did not change spec fingerprint")
	}
}

func TestSpecUsesV2DataVolume(t *testing.T) {
	t.Parallel()
	spec := Spec{Name: "demo", Image: "image:one", RepoPath: "/repo", MemoryBytes: 1024}
	if got := specDataVolumeName(spec); got != "fern-demo-v2-data" {
		t.Fatalf("data volume = %q", got)
	}
}
