package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGatewayRejectsRemovedResultPublicationAPI(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		response := httptest.NewRecorder()
		gatewayHandler(Controls{}).ServeHTTP(response, httptest.NewRequest(method,
			"/fern/api/v1/results/res_0198d34d-6a50-75fb-b1f2-b4a14d70ec59/publications", nil))
		if response.Code != http.StatusNotFound && response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s removed publication status=%d", method, response.Code)
		}
	}
}

func TestGatewayKeepsPluginAndTerminalRunSurfacesSeparate(t *testing.T) {
	var pluginCalls, clientCalls int
	handler := gatewayHandler(Controls{
		Runs: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			pluginCalls++
			writer.WriteHeader(http.StatusOK)
		}),
		RunClients: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			clientCalls++
			writer.WriteHeader(http.StatusOK)
		}),
	})
	for _, path := range []string{"/fern/api/runs", "/fern/api/v1/runs"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, response.Code)
		}
	}
	if pluginCalls != 1 || clientCalls != 1 {
		t.Fatalf("plugin calls=%d client calls=%d", pluginCalls, clientCalls)
	}
}
