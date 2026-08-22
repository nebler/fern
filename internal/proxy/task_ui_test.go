package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTaskUIIsMountedOnlyWhenTaskServiceIsEnabled(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/fern/tasks", nil)
	response := httptest.NewRecorder()
	serveFern(response, request, Controls{Tasks: http.NotFoundHandler()}, false)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Send the next move") || !strings.Contains(response.Body.String(), `/fern/assets/tasks.js`) {
		t.Fatalf("task page = %d %s", response.Code, response.Body.String())
	}
	if csp := response.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("CSP = %q", csp)
	}

	response = httptest.NewRecorder()
	serveFern(response, httptest.NewRequest(http.MethodGet, "/fern/tasks", nil), Controls{}, false)
	if response.Code != http.StatusNotFound {
		t.Fatalf("disabled task page = %d", response.Code)
	}
}

func TestTaskUIScriptAndMethodPolicy(t *testing.T) {
	t.Parallel()
	response := httptest.NewRecorder()
	serveFern(response, httptest.NewRequest(http.MethodGet, "/fern/assets/tasks.js", nil), Controls{Tasks: http.NotFoundHandler()}, false)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/javascript; charset=utf-8" ||
		!strings.Contains(response.Body.String(), "Idempotency-Key") || !strings.Contains(response.Body.String(), "openCodePath") {
		t.Fatalf("task script = %d %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	serveFern(response, httptest.NewRequest(http.MethodPost, "/fern/tasks", nil), Controls{Tasks: http.NotFoundHandler()}, false)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("method policy = %d %q", response.Code, response.Header().Get("Allow"))
	}
}

func TestTaskUILandingLinkTracksAvailability(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		controls Controls
		want     string
	}{
		{name: "enabled", controls: Controls{Tasks: http.NotFoundHandler()}, want: `href="/fern/tasks"`},
		{name: "disabled", controls: Controls{}, want: `href="/"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			serveFern(response, httptest.NewRequest(http.MethodGet, "/fern/", nil), test.controls, false)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("landing = %d %s", response.Code, response.Body.String())
			}
		})
	}
}
