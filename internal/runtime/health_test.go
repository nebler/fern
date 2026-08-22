package runtime

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitHealthyUsesBasicAuth(t *testing.T) {
	t.Parallel()
	type credentials struct {
		username string
		password string
		present  bool
	}
	requests := make(chan credentials, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/health" {
			http.NotFound(writer, request)
			return
		}
		username, password, ok := request.BasicAuth()
		requests <- credentials{username: username, password: password, present: ok}
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
	missing := <-requests
	wrong := <-requests
	correct := <-requests
	if missing.present {
		t.Fatalf("missing-credential probe sent credentials for %q", missing.username)
	}
	if !wrong.present || wrong.username != "opencode" || wrong.password == "secret" {
		t.Fatalf("wrong-credential probe = {username: %q, present: %t, correct password: %t}", wrong.username, wrong.present, wrong.password == "secret")
	}
	if !correct.present || correct.username != "opencode" || correct.password != "secret" {
		t.Fatalf("authenticated probe = {username: %q, present: %t, correct password: %t}", correct.username, correct.present, correct.password == "secret")
	}
	select {
	case extra := <-requests:
		t.Fatalf("unexpected extra health probe: %+v", extra)
	default:
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

func TestWaitHealthyFailsClosedWhenBackendAcceptsNegativeAuthProbe(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		acceptedProbe int
		status        int
	}{
		{name: "missing credentials with 200", acceptedProbe: 1, status: http.StatusOK},
		{name: "missing credentials with other success", acceptedProbe: 1, status: http.StatusNoContent},
		{name: "wrong credentials", acceptedProbe: 2, status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				request := int(requests.Add(1))
				if request == test.acceptedProbe {
					writer.WriteHeader(test.status)
					if test.status != http.StatusNoContent {
						_, _ = writer.Write([]byte(`{"healthy":true}`))
					}
					return
				}
				http.Error(writer, "unauthorized", http.StatusUnauthorized)
			}))
			defer server.Close()

			err := WaitHealthyURL(context.Background(), server.URL, ServerAuth{Password: "secret"}, time.Second)
			if !errors.Is(err, errUnsafeHealthAuth) {
				t.Fatalf("error = %v, want permanent unsafe-auth error", err)
			}
			if got := int(requests.Load()); got != test.acceptedProbe {
				t.Fatalf("requests = %d, want immediate failure after %d", got, test.acceptedProbe)
			}
		})
	}
}

func TestWaitHealthyRetriesTransientAuthStartupThenSucceeds(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		call := requests.Add(1)
		if call == 1 {
			http.Error(writer, "starting", http.StatusServiceUnavailable)
			return
		}
		username, password, ok := request.BasicAuth()
		if !ok || username != "opencode" || password != "secret" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = writer.Write([]byte(`{"healthy":true}`))
	}))
	defer server.Close()

	if err := WaitHealthyURL(context.Background(), server.URL, ServerAuth{Password: "secret"}, time.Second); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 4 {
		t.Fatalf("requests = %d, want one transient probe followed by all three auth probes", got)
	}
}

func TestHealthRequestsRejectRedirects(t *testing.T) {
	t.Parallel()
	var redirectedRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		redirectedRequests.Add(1)
		_, _ = writer.Write([]byte(`{"healthy":true}`))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/api/health", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	status, _, err := requestHealth(context.Background(), redirect.URL, ServerAuth{Password: "redirect-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want redirect response", status)
	}
	if got := redirectedRequests.Load(); got != 0 {
		t.Fatalf("redirect target received %d requests", got)
	}
}

func TestHealthResponseIsBoundedAndRedacted(t *testing.T) {
	t.Parallel()
	const responseSecret = "response-body-must-not-appear"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(strings.Repeat(responseSecret, maxHealthBytes/len(responseSecret)+2)))
	}))
	defer server.Close()

	err := checkHealth(context.Background(), server.URL, ServerAuth{})
	if err == nil || !strings.Contains(err.Error(), "exceeds 64 KiB") {
		t.Fatalf("error = %v, want bounded-response error", err)
	}
	if strings.Contains(err.Error(), responseSecret) {
		t.Fatalf("error disclosed response body: %v", err)
	}
}

func TestUnsafeAuthErrorRedactsCredentialsAndResponse(t *testing.T) {
	t.Parallel()
	const password = "configured-password-must-not-appear"
	const responseSecret = "unsafe-response-must-not-appear"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = writer.Write([]byte(responseSecret + password))
	}))
	defer server.Close()

	err := WaitHealthyURL(context.Background(), server.URL, ServerAuth{Password: password}, time.Second)
	if !errors.Is(err, errUnsafeHealthAuth) {
		t.Fatalf("error = %v, want permanent unsafe-auth error", err)
	}
	if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), responseSecret) {
		t.Fatalf("error disclosed credentials or response body: %v", err)
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

func TestWaitHealthyCancelsInFlightResponse(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- WaitHealthyURL(ctx, server.URL, ServerAuth{}, time.Minute)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("health request did not reach server")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("health check did not honor cancellation")
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

func TestWorkspaceGHModeIsExplicitAndFingerprintSeparated(t *testing.T) {
	t.Parallel()
	base := Spec{Name: "demo", Image: "image:one", RepoPath: "/repo", MemoryBytes: 1024}
	baseFingerprint, err := specFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := specEnvironment(base)[githubConfigEnv]; exists {
		t.Fatal("default mode unexpectedly sets GH_CONFIG_DIR")
	}
	if got := len(specMounts(base)); got != 2 {
		t.Fatalf("default mounts = %d, want 2", got)
	}
	if got := specGHVolumeName(base); got != "" {
		t.Fatalf("default GitHub CLI volume = %q, want none", got)
	}

	workspaceGH := base
	workspaceGH.WorkspaceGH = true
	workspaceFingerprint, err := specFingerprint(workspaceGH)
	if err != nil {
		t.Fatal(err)
	}
	if workspaceFingerprint == baseFingerprint {
		t.Fatal("workspace gh mode did not change spec fingerprint")
	}
	if got := specEnvironment(workspaceGH)[githubConfigEnv]; got != githubConfigDir {
		t.Fatalf("GH_CONFIG_DIR = %q, want %q", got, githubConfigDir)
	}
	mounts := specMounts(workspaceGH)
	if len(mounts) != 3 || mounts[2].Source != "fern-demo-v1-gh-config" || mounts[2].Target != githubConfigDir {
		t.Fatalf("workspace gh mounts = %+v", mounts)
	}

	workspaceGH.Env = map[string]string{githubConfigEnv: "/tmp/gh"}
	if err := workspaceGH.Validate(); err == nil {
		t.Fatal("workspace gh accepted a conflicting GH_CONFIG_DIR")
	}
	workspaceGH.Env = map[string]string{githubConfigEnv: githubConfigDir}
	if err := workspaceGH.Validate(); err == nil {
		t.Fatal("workspace gh accepted a caller-managed GH_CONFIG_DIR")
	}
}
