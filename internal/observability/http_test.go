package observability

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadinessAndLivenessHaveDistinctFailureSemantics(t *testing.T) {
	registry := NewRegistry()
	registry.Blocked(ComponentGitHubDependency, errors.New("credentials unavailable"))

	live := httptest.NewRecorder()
	registry.LivenessHandler().ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/fern/live", nil))
	if live.Code != http.StatusOK || !strings.Contains(live.Body.String(), `"live":true`) {
		t.Fatalf("liveness = %d %q", live.Code, live.Body.String())
	}
	ready := httptest.NewRecorder()
	registry.ReadinessHandler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/fern/ready", nil))
	if ready.Code != http.StatusServiceUnavailable || !strings.Contains(ready.Body.String(), `"ready":false`) {
		t.Fatalf("readiness = %d %q", ready.Code, ready.Body.String())
	}
}
