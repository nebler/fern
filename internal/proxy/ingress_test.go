package proxy

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPairedResultPublicationRequiresBoundCSRF(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	state := newPairingState()
	state.now = func() time.Time { return now }
	credential := "paired-device-secret"
	state.sessions[sha256.Sum256([]byte(credential))] = now.Add(time.Hour)
	called := false
	handler := state.remoteHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		called = true
		writer.WriteHeader(http.StatusAccepted)
	}))
	path := "/fern/api/v1/results/res_0198d34d-6a50-75fb-b1f2-b4a14d70ec59/publications"

	tokenRequest := httptest.NewRequest(http.MethodGet, csrfTokenPath+"?method=POST&path="+path, nil)
	tokenRequest.AddCookie(&http.Cookie{Name: deviceCookieName, Value: credential})
	tokenResponse := httptest.NewRecorder()
	handler.ServeHTTP(tokenResponse, tokenRequest)
	var token struct {
		Token string `json:"token"`
	}
	if tokenResponse.Code != http.StatusOK || json.Unmarshal(tokenResponse.Body.Bytes(), &token) != nil || token.Token == "" {
		t.Fatalf("token response=%d %s", tokenResponse.Code, tokenResponse.Body.String())
	}

	publication := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	publication.AddCookie(&http.Cookie{Name: deviceCookieName, Value: credential})
	publication.Header.Set(csrfHeaderName, token.Token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, publication)
	if response.Code != http.StatusAccepted || !called {
		t.Fatalf("publication response=%d called=%t", response.Code, called)
	}
}

func TestPairedRunControlRequiresBoundCSRFAndInjectsDeviceActor(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	state := newPairingState()
	state.now = func() time.Time { return now }
	credential := "paired-run-control-secret"
	state.sessions[sha256.Sum256([]byte(credential))] = now.Add(time.Hour)
	path := "/fern/api/v1/runs/tsk_0198d34d-6a50-75fb-b1f2-000000000201/takeover"
	called := false
	handler := state.remoteHandler(gatewayHandler(Controls{RunControl: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		called = true
		writer.WriteHeader(http.StatusAccepted)
	})}))

	tokenRequest := httptest.NewRequest(http.MethodGet, csrfTokenPath+"?method=POST&path="+path, nil)
	tokenRequest.AddCookie(&http.Cookie{Name: deviceCookieName, Value: credential})
	tokenResponse := httptest.NewRecorder()
	handler.ServeHTTP(tokenResponse, tokenRequest)
	var token struct {
		Token string `json:"token"`
	}
	if tokenResponse.Code != http.StatusOK || json.Unmarshal(tokenResponse.Body.Bytes(), &token) != nil || token.Token == "" {
		t.Fatalf("token response=%d %s", tokenResponse.Code, tokenResponse.Body.String())
	}

	missing := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	missing.AddCookie(&http.Cookie{Name: deviceCookieName, Value: credential})
	missingResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingResponse, missing)
	if missingResponse.Code != http.StatusForbidden || called {
		t.Fatalf("missing CSRF response=%d called=%t", missingResponse.Code, called)
	}

	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	request.AddCookie(&http.Cookie{Name: deviceCookieName, Value: credential})
	request.Header.Set(csrfHeaderName, token.Token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !called {
		t.Fatalf("run control response=%d called=%t", response.Code, called)
	}
}

func TestGatewayDispatchesResultAPI(t *testing.T) {
	called := false
	handler := gatewayHandler(Controls{Results: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		called = true
		writer.WriteHeader(http.StatusAccepted)
	})})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/fern/api/v1/results/res_0198d34d-6a50-75fb-b1f2-b4a14d70ec59", nil))
	if response.Code != http.StatusAccepted || !called {
		t.Fatalf("result response=%d called=%t", response.Code, called)
	}
}

func TestGatewayDispatchesRunControl(t *testing.T) {
	called := false
	handler := gatewayHandler(Controls{RunControl: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		called = true
		writer.WriteHeader(http.StatusAccepted)
	})})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/fern/api/v1/runs", nil))
	if response.Code != http.StatusAccepted || !called {
		t.Fatalf("run control response=%d called=%t", response.Code, called)
	}
}
