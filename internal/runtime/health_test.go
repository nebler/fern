package runtime

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWaitHealthyUsesBasicAuth(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "agent" || password != "secret" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	endpoint := Endpoint{Host: "127.0.0.1", Port: server.Listener.Addr().(*net.TCPAddr).Port}
	if err := WaitHealthy(context.Background(), endpoint, ServerAuth{Username: "agent", Password: "secret"}, time.Second); err != nil {
		t.Fatal(err)
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
}
