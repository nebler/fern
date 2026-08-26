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
	registry.Blocked(ComponentGitHubTaskDependency, errors.New("credentials unavailable"))

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

func TestMetricsAreOpenMetricsWithFixedComponentLabels(t *testing.T) {
	registry := NewRegistry()
	registry.Degraded(ComponentTaskExecution, errors.New("secret response"))
	response := httptest.NewRecorder()
	registry.MetricsHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/fern/metrics", nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/openmetrics-text; version=1.0.0; charset=utf-8" {
		t.Fatalf("metrics response = %d %q", response.Code, response.Header().Get("Content-Type"))
	}
	body := response.Body.String()
	if strings.Count(body, "fern_component_ready{") != len(components) || strings.Count(body, "fern_component_blocked{") != len(components) ||
		!strings.HasSuffix(body, "# EOF\n") || strings.Contains(body, "secret response") {
		t.Fatalf("metrics body = %q", body)
	}
}
