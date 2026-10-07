package observability

import (
	"encoding/json"
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
		ready := registry.Ready()
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		writeProbeJSON(writer, request, status, struct {
			Ready bool `json:"ready"`
		}{Ready: ready})
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
