package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/runtime"
)

func TestFernRoutesDoNotWakeWorkspace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		method string
		path   string
		status int
	}{
		{method: http.MethodGet, path: "/fern/", status: http.StatusOK},
		{method: http.MethodHead, path: "/fern/", status: http.StatusOK},
		{method: http.MethodGet, path: "/fern/ready", status: http.StatusOK},
		{method: http.MethodGet, path: "/fern/missing", status: http.StatusNotFound},
		{method: http.MethodPost, path: "/fern/ready", status: http.StatusMethodNotAllowed},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			waker := &countingWaker{}
			request := httptest.NewRequest(test.method, test.path, nil)
			request.SetBasicAuth("opencode", "secret")
			response := httptest.NewRecorder()
			New(waker, runtime.ServerAuth{Password: "secret"}, testLogger()).ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			if got := waker.wakes.Load(); got != 0 {
				t.Fatalf("wake count = %d, want 0", got)
			}
		})
	}
}

func TestFernLandingLinksToOfficialUI(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/fern/", nil)
	response := httptest.NewRecorder()
	New(nil, runtime.ServerAuth{}, testLogger()).ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `href="/"`) {
		t.Fatalf("landing response status=%d body=%q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Security-Policy"); got == "" {
		t.Fatal("landing response has no content security policy")
	}
	if got := response.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Fatalf("Referrer-Policy = %q, want same-origin for same-origin control forms", got)
	}
}

func TestPairedLandingDoesNotExposeControlSurface(t *testing.T) {
	t.Parallel()
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/fern/", nil)
	request.SetBasicAuth("opencode", "open-secret")
	response := httptest.NewRecorder()
	NewWithControls(nil, runtime.ServerAuth{Password: "open-secret"}, Controls{
		Store: store, ControlAuth: ControlAuth{Password: "control-secret"},
	}, testLogger()).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("landing status=%d", response.Code)
	}
	for _, forbidden := range []string{"Tracked work", "Paired devices", "Publish draft PR", "Revoke"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("paired landing exposed %q", forbidden)
		}
	}
}

func TestControlSurfaceRequiresDistinctCredential(t *testing.T) {
	t.Parallel()
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewWithControls(nil, runtime.ServerAuth{Password: "open-secret"}, Controls{
		Store: store, ControlAuth: ControlAuth{Password: "control-secret"},
	}, testLogger())
	for _, credentials := range [][2]string{{}, {"opencode", "open-secret"}, {"fern", "wrong"}} {
		request := httptest.NewRequest(http.MethodGet, "/fern/control", nil)
		if credentials[0] != "" {
			request.SetBasicAuth(credentials[0], credentials[1])
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("credentials %q status=%d", credentials[0], response.Code)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/fern/control", nil)
	request.SetBasicAuth("fern", "control-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Paired devices") {
		t.Fatalf("control status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestFernReadyWorksWithoutWorkspaceManager(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/fern/ready", "/api/health"} {
		response := httptest.NewRecorder()
		New(nil, runtime.ServerAuth{}, testLogger()).ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		want := http.StatusOK
		if path == "/api/health" {
			want = http.StatusServiceUnavailable
		}
		if response.Code != want {
			t.Fatalf("%s status = %d, want %d", path, response.Code, want)
		}
	}
}

func TestFernNamespaceUsesSegmentBoundary(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, request.URL.Path)
	}))
	defer upstream.Close()
	waker := &countingWaker{endpoint: mustParseEndpoint(t, upstream.URL)}
	for _, path := range []string{"/fernish", "/fern-smoke/direct-navigation"} {
		response := httptest.NewRecorder()
		New(waker, runtime.ServerAuth{}, testLogger()).ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || response.Body.String() != path {
			t.Fatalf("%s was not proxied: status=%d body=%q", path, response.Code, response.Body.String())
		}
	}
	if got := waker.wakes.Load(); got != 2 {
		t.Fatalf("wake count = %d, want 2", got)
	}
}

func TestFernRoutesRequireAuthenticationBeforeHandling(t *testing.T) {
	t.Parallel()
	waker := &countingWaker{}
	response := httptest.NewRecorder()
	New(waker, runtime.ServerAuth{Password: "secret"}, testLogger()).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/fern/", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if got := waker.wakes.Load(); got != 0 {
		t.Fatalf("wake count = %d, want 0", got)
	}
}

func TestFernUnsupportedMethodDoesNotReadBody(t *testing.T) {
	t.Parallel()
	body := &trackingBody{}
	request := httptest.NewRequest(http.MethodPost, "/fern/ready", body)
	response := httptest.NewRecorder()
	New(nil, runtime.ServerAuth{}, testLogger()).ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed || body.read.Load() {
		t.Fatalf("status=%d bodyRead=%t", response.Code, body.read.Load())
	}
}
