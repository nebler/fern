package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/nebler/fern/internal/control"
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
		if _, err := request.Cookie(deviceCookieName); err == nil {
			t.Error("Fern device cookie reached OpenCode upstream")
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	waker := &countingWaker{endpoint: mustParseEndpoint(t, upstream.URL)}
	handler := NewWithControls(waker, runtime.ServerAuth{Password: "secret"}, Controls{ControlAuth: ControlAuth{Password: "control-secret"}}, testLogger())

	issue := httptest.NewRequest(http.MethodPost, "/fern/pair/new", nil)
	issue.SetBasicAuth("fern", "control-secret")
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
	if len(cookies) != 1 || cookies[0].Name != deviceCookieName || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/" || cookies[0].Domain != "" || cookies[0].MaxAge <= 0 || cookies[0].Expires.IsZero() {
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
	adminRequest := httptest.NewRequest(http.MethodGet, "/fern/api/v1/devices", nil)
	adminRequest.AddCookie(cookies[0])
	adminResponse := httptest.NewRecorder()
	handler.ServeHTTP(adminResponse, adminRequest)
	if adminResponse.Code != http.StatusUnauthorized {
		t.Fatalf("paired device received admin status %d", adminResponse.Code)
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
	NewWithControls(waker, runtime.ServerAuth{Password: "secret"}, Controls{ControlAuth: ControlAuth{Password: "control-secret"}}, testLogger()).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/fern/pair/new", nil))
	if response.Code != http.StatusUnauthorized || waker.wakes.Load() != 0 {
		t.Fatalf("status=%d wakes=%d", response.Code, waker.wakes.Load())
	}
}

func TestPairedDeviceSurvivesHandlerRestart(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "control")
	store, err := control.Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewWithControls(&countingWaker{}, runtime.ServerAuth{Password: "secret"}, Controls{Store: store, ControlAuth: ControlAuth{Password: "control-secret"}}, testLogger())
	issue := httptest.NewRequest(http.MethodPost, "/fern/pair/new", nil)
	issue.SetBasicAuth("fern", "control-secret")
	issued := httptest.NewRecorder()
	handler.ServeHTTP(issued, issue)
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	paired := httptest.NewRecorder()
	handler.ServeHTTP(paired, httptest.NewRequest(http.MethodGet, "/fern/pair?code="+payload.Code+"&name=Phone", nil))
	cookie := paired.Result().Cookies()[0]

	reopened, err := control.Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewWithControls(nil, runtime.ServerAuth{Password: "secret"}, Controls{Store: reopened, ControlAuth: ControlAuth{Password: "control-secret"}}, testLogger())
	request := httptest.NewRequest(http.MethodGet, "/fern/ready", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	restarted.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("persisted device status = %d", response.Code)
	}
}

func TestPairingStoreFailureLeavesCodeRetryable(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "control")
	store, err := control.Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewWithControls(nil, runtime.ServerAuth{Password: "secret"}, Controls{Store: store, ControlAuth: ControlAuth{Password: "control-secret"}}, testLogger())
	issue := httptest.NewRequest(http.MethodPost, "/fern/pair/new", nil)
	issue.SetBasicAuth("fern", "control-secret")
	issued := httptest.NewRecorder()
	handler.ServeHTTP(issued, issue)
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(directory, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	failed := httptest.NewRecorder()
	handler.ServeHTTP(failed, httptest.NewRequest(http.MethodGet, "/fern/pair?code="+payload.Code, nil))
	if failed.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed pairing status=%d", failed.Code)
	}
	if err := os.Remove(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	retried := httptest.NewRecorder()
	handler.ServeHTTP(retried, httptest.NewRequest(http.MethodGet, "/fern/pair?code="+payload.Code, nil))
	if retried.Code != http.StatusSeeOther || len(retried.Result().Cookies()) != 1 {
		t.Fatalf("retried pairing status=%d cookies=%+v", retried.Code, retried.Result().Cookies())
	}
}

func TestInvalidDeviceCookieIsStrippedOnBasicFallback(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if _, err := request.Cookie(deviceCookieName); err == nil {
			t.Error("invalid Fern device cookie reached OpenCode upstream")
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	request.AddCookie(&http.Cookie{Name: deviceCookieName, Value: "invalid"})
	request.SetBasicAuth("opencode", "secret")
	response := httptest.NewRecorder()
	New(&countingWaker{endpoint: mustParseEndpoint(t, upstream.URL)}, runtime.ServerAuth{Password: "secret"}, testLogger()).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d", response.Code)
	}
}
