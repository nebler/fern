package observability

import (
	"encoding/json"
	"fmt"
	"net/http"
)

func (registry *Registry) LivenessHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !probeMethod(writer, request) {
			return
		}
		writeProbeJSON(writer, request, http.StatusOK, struct {
			Live bool `json:"live"`
		}{Live: true})
	})
}

func (registry *Registry) ReadinessHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !probeMethod(writer, request) {
			return
		}
		ready := registry.Snapshot().Ready
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		writeProbeJSON(writer, request, status, struct {
			Ready bool `json:"ready"`
		}{Ready: ready})
	})
}

func (registry *Registry) StatusHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !probeMethod(writer, request) {
			return
		}
		writeProbeJSON(writer, request, http.StatusOK, registry.Snapshot())
	})
}

func (registry *Registry) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !probeMethod(writer, request) {
			return
		}
		snapshot := registry.Snapshot()
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")
		if request.Method == http.MethodHead {
			return
		}
		ready := 0
		if snapshot.Ready {
			ready = 1
		}
		_, _ = fmt.Fprintf(writer, "# HELP fern_ready Whether Fern can serve durable control work.\n# TYPE fern_ready gauge\nfern_ready %d\n", ready)
		_, _ = fmt.Fprint(writer, "# HELP fern_component_ready Whether a fixed Fern component is ready.\n# TYPE fern_component_ready gauge\n")
		for _, status := range snapshot.Components {
			value := 0
			if status.Ready {
				value = 1
			}
			_, _ = fmt.Fprintf(writer, "fern_component_ready{component=%q} %d\n", status.Component, value)
		}
		_, _ = fmt.Fprint(writer, "# HELP fern_component_degraded Whether a fixed Fern component has a transient failure.\n# TYPE fern_component_degraded gauge\n")
		for _, status := range snapshot.Components {
			value := 0
			if status.State == StateDegraded {
				value = 1
			}
			_, _ = fmt.Fprintf(writer, "fern_component_degraded{component=%q} %d\n", status.Component, value)
		}
		_, _ = fmt.Fprint(writer, "# HELP fern_component_blocked Whether a fixed Fern component is waiting for a required dependency.\n# TYPE fern_component_blocked gauge\n")
		for _, status := range snapshot.Components {
			value := 0
			if status.State == StateBlocked {
				value = 1
			}
			_, _ = fmt.Fprintf(writer, "fern_component_blocked{component=%q} %d\n", status.Component, value)
		}
		_, _ = fmt.Fprint(writer, "# HELP fern_component_failures_total Total observed failures for a fixed Fern component.\n# TYPE fern_component_failures_total counter\n")
		for _, status := range snapshot.Components {
			_, _ = fmt.Fprintf(writer, "fern_component_failures_total{component=%q} %d\n", status.Component, status.FailuresTotal)
		}
		_, _ = fmt.Fprint(writer, "# EOF\n")
	})
}

func probeMethod(writer http.ResponseWriter, request *http.Request) bool {
	if request.Method == http.MethodGet || request.Method == http.MethodHead {
		return true
	}
	writer.Header().Set("Allow", "GET, HEAD")
	http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func writeProbeJSON(writer http.ResponseWriter, request *http.Request, status int, value any) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if request.Method == http.MethodGet {
		_ = json.NewEncoder(writer).Encode(value)
	}
}
