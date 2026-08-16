package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nebler/fern/internal/runtime"
)

func TestPairingCreatesCookieAndInjectsUpstreamAuth(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "opencode" || password != "secret" {
			http.Error(writer, "missing injected auth", http.StatusUnauthorized)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	waker := &countingWaker{endpoint: mustParseEndpoint(t, upstream.URL)}
	handler := New(waker, runtime.ServerAuth{Password: "secret"}, testLogger())

	issue := httptest.NewRequest(http.MethodPost, "/fern/pair/new", nil)
	issue.SetBasicAuth("opencode", "secret")
	issued := httptest.NewRecorder()
	handler.ServeHTTP(issued, issue)
	if issued.Code != http.StatusOK {
		t.Fatalf("issue status = %d", issued.Code)
	}
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &payload); err != nil || payload.Code == "" {
		t.Fatalf("pairing payload = %q, err=%v", issued.Body.String(), err)
	}

	paired := httptest.NewRecorder()
	handler.ServeHTTP(paired, httptest.NewRequest(http.MethodGet, "/fern/pair?code="+payload.Code, nil))
	if paired.Code != http.StatusSeeOther {
		t.Fatalf("pair status = %d", paired.Code)
	}
	cookies := paired.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != deviceCookieName || !cookies[0].HttpOnly || !cookies[0].Secure {
		t.Fatalf("pairing cookies = %+v", cookies)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	request.AddCookie(cookies[0])
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("paired request status = %d", response.Code)
	}
	if got := waker.wakes.Load(); got != 1 {
		t.Fatalf("wake count = %d, want 1", got)
	}

	replay := httptest.NewRecorder()
	handler.ServeHTTP(replay, httptest.NewRequest(http.MethodGet, "/fern/pair?code="+payload.Code, nil))
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("replayed pair status = %d", replay.Code)
	}
}

func TestPairingIssuanceRequiresBasicAuthWithoutWake(t *testing.T) {
	t.Parallel()
	waker := &countingWaker{}
	response := httptest.NewRecorder()
	New(waker, runtime.ServerAuth{Password: "secret"}, testLogger()).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/fern/pair/new", nil))
	if response.Code != http.StatusUnauthorized || waker.wakes.Load() != 0 {
		t.Fatalf("status=%d wakes=%d", response.Code, waker.wakes.Load())
	}
}
