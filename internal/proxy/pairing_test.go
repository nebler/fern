package proxy

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	preview := httptest.NewRecorder()
	handler.ServeHTTP(preview, httptest.NewRequest(http.MethodGet, "/fern/pair?code="+payload.Code, nil))
	if preview.Code != http.StatusOK || len(preview.Result().Cookies()) != 0 || !strings.Contains(preview.Body.String(), "Pair this phone") {
		t.Fatalf("preview status=%d cookies=%+v body=%q", preview.Code, preview.Result().Cookies(), preview.Body.String())
	}

	paired := httptest.NewRecorder()
	handler.ServeHTTP(paired, pairingRequest(payload.Code))
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
	handler.ServeHTTP(replay, pairingRequest(payload.Code))
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
	handler.ServeHTTP(paired, pairingRequest(payload.Code))
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

func TestPairingStoreFailureConsumesCodeFailClosed(t *testing.T) {
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
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(directory, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	failed := httptest.NewRecorder()
	handler.ServeHTTP(failed, pairingRequest(payload.Code))
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
	handler.ServeHTTP(retried, pairingRequest(payload.Code))
	if retried.Code != http.StatusUnauthorized || len(retried.Result().Cookies()) != 0 {
		t.Fatalf("retried pairing status=%d cookies=%+v", retried.Code, retried.Result().Cookies())
	}
}

func TestPairingPreservesEscapedDeviceName(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	directory := filepath.Join(t.TempDir(), "control")
	store, err := control.Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	state := newPairingState(store)
	state.now = func() time.Time { return now }
	code := "known-pairing-code"
	state.codes[sha256.Sum256([]byte(code))] = now.Add(5 * time.Minute)
	name := `  <Noah's & "Phone">  `

	preview := httptest.NewRecorder()
	state.pair(preview, httptest.NewRequest(http.MethodGet, "/fern/pair?"+url.Values{"code": {code}, "name": {name}}.Encode(), nil))
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%q", preview.Code, preview.Body.String())
	}
	body := preview.Body.String()
	if strings.Contains(body, `<Noah's & "Phone">`) || !strings.Contains(body, `value="&lt;Noah&#39;s &amp; &#34;Phone&#34;&gt;"`) {
		t.Fatalf("device name was not safely preserved in form: %q", body)
	}

	paired := httptest.NewRecorder()
	state.pair(paired, pairingRequestWithName(code, name))
	if paired.Code != http.StatusSeeOther {
		t.Fatalf("pair status=%d body=%q", paired.Code, paired.Body.String())
	}
	reopened, err := control.Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	devices, err := reopened.Devices(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].Name != `<Noah's & "Phone">` {
		t.Fatalf("persisted devices=%+v", devices)
	}
}

func TestPairingDeviceNameBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
		want  string
		valid bool
	}{
		{name: "empty uses store default", value: "  ", want: "", valid: true},
		{name: "trimmed at byte limit", value: "  " + strings.Repeat("x", maxDeviceNameBytes) + "  ", want: strings.Repeat("x", maxDeviceNameBytes), valid: true},
		{name: "multi-byte at byte limit", value: strings.Repeat("é", maxDeviceNameBytes/2), want: strings.Repeat("é", maxDeviceNameBytes/2), valid: true},
		{name: "over byte limit", value: strings.Repeat("x", maxDeviceNameBytes+1), valid: false},
		{name: "multi-byte over byte limit", value: strings.Repeat("é", maxDeviceNameBytes/2+1), valid: false},
		{name: "invalid UTF-8", value: string([]byte{'p', 'h', 'o', 'n', 'e', 0xff}), valid: false},
		{name: "control character", value: "phone\x00name", valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, valid := pairingDeviceName(test.value)
			if valid != test.valid || got != test.want {
				t.Fatalf("pairingDeviceName(%q)=(%q, %t), want (%q, %t)", test.value, got, valid, test.want, test.valid)
			}
		})
	}

	state := newPairingState()
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	state.now = func() time.Time { return now }
	code := "retryable-code"
	state.codes[sha256.Sum256([]byte(code))] = now.Add(5 * time.Minute)
	rejected := httptest.NewRecorder()
	state.pair(rejected, pairingRequestWithName(code, strings.Repeat("x", maxDeviceNameBytes+1)))
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("oversized name status=%d body=%q", rejected.Code, rejected.Body.String())
	}
	accepted := httptest.NewRecorder()
	state.pair(accepted, pairingRequestWithName(code, strings.Repeat("x", maxDeviceNameBytes)))
	if accepted.Code != http.StatusSeeOther {
		t.Fatalf("bounded retry status=%d body=%q", accepted.Code, accepted.Body.String())
	}
}

func TestPairingCodeCapRecoversAfterExpiry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	state := newPairingState()
	state.now = func() time.Time { return now }
	for index := range maxOutstandingPairings {
		state.codes[sha256.Sum256([]byte(fmt.Sprintf("code-%d", index)))] = now.Add(time.Minute)
	}

	blocked := httptest.NewRecorder()
	state.issue(blocked, httptest.NewRequest(http.MethodPost, "/fern/pair/new", nil))
	if blocked.Code != http.StatusTooManyRequests || blocked.Header().Get("Retry-After") == "" || len(state.codes) != maxOutstandingPairings {
		t.Fatalf("blocked status=%d retry-after=%q codes=%d", blocked.Code, blocked.Header().Get("Retry-After"), len(state.codes))
	}

	now = now.Add(time.Minute)
	recovered := httptest.NewRecorder()
	state.issue(recovered, httptest.NewRequest(http.MethodPost, "/fern/pair/new", nil))
	if recovered.Code != http.StatusOK || len(state.codes) != 1 {
		t.Fatalf("recovered status=%d codes=%d body=%q", recovered.Code, len(state.codes), recovered.Body.String())
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

func TestRevocationCancelsOnlyTargetDeviceStreamAndDeniesReconnect(t *testing.T) {
	now := time.Now()
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	deviceA, err := store.AddDevice("token-a", "Device A", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	deviceB, err := store.AddDevice("token-b", "Device B", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	startedA, startedB := make(chan struct{}), make(chan struct{})
	canceledA, canceledB := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var started, canceled chan struct{}
		switch request.URL.Path {
		case "/stream/a":
			started, canceled = startedA, canceledA
		case "/stream/b":
			started, canceled = startedB, canceledB
		default:
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		close(started)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: connected\n\n"))
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	server := httptest.NewServer(NewWithControls(
		&countingWaker{endpoint: mustParseEndpoint(t, upstream.URL)},
		runtime.ServerAuth{Password: "upstream-secret"},
		Controls{Store: store, ControlAuth: ControlAuth{Password: "control-secret"}},
		testLogger(),
	))
	defer server.Close()
	client := server.Client()

	startStream := func(path, token string, started <-chan struct{}) *http.Response {
		t.Helper()
		request, requestErr := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		request.AddCookie(&http.Cookie{Name: deviceCookieName, Value: token})
		response, requestErr := client.Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		select {
		case <-started:
		case <-time.After(time.Second):
			response.Body.Close()
			t.Fatal("stream did not reach upstream")
		}
		return response
	}
	responseA := startStream("/stream/a", "token-a", startedA)
	defer responseA.Body.Close()
	responseB := startStream("/stream/b", "token-b", startedB)
	defer responseB.Body.Close()

	revokeA, err := http.NewRequest(http.MethodDelete, server.URL+"/fern/api/v1/devices/"+deviceA.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	revokeA.SetBasicAuth("fern", "control-secret")
	revokedA, err := client.Do(revokeA)
	if err != nil {
		t.Fatal(err)
	}
	revokedA.Body.Close()
	if revokedA.StatusCode != http.StatusNoContent {
		t.Fatalf("JSON revoke status=%d", revokedA.StatusCode)
	}
	select {
	case <-canceledA:
	case <-time.After(time.Second):
		t.Fatal("revoked device stream was not canceled")
	}
	select {
	case <-canceledB:
		t.Fatal("revoking device A canceled device B")
	default:
	}

	reconnect, err := http.NewRequest(http.MethodGet, server.URL+"/api/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	reconnect.AddCookie(&http.Cookie{Name: deviceCookieName, Value: "token-a"})
	reconnected, err := client.Do(reconnect)
	if err != nil {
		t.Fatal(err)
	}
	reconnected.Body.Close()
	if reconnected.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked reconnect status=%d", reconnected.StatusCode)
	}

	revokeB, err := http.NewRequest(http.MethodPost, server.URL+"/fern/devices/"+deviceB.ID+"/revoke", nil)
	if err != nil {
		t.Fatal(err)
	}
	revokeB.SetBasicAuth("fern", "control-secret")
	revokedB, err := client.Do(revokeB)
	if err != nil {
		t.Fatal(err)
	}
	revokedB.Body.Close()
	if revokedB.StatusCode != http.StatusOK {
		t.Fatalf("HTML revoke status=%d", revokedB.StatusCode)
	}
	select {
	case <-canceledB:
	case <-time.After(time.Second):
		t.Fatal("HTML revocation did not cancel device B stream")
	}
}

func TestPairedRequestShapesInheritRevocableContext(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		path    string
		body    string
		upgrade bool
	}{
		{name: "ordinary", method: http.MethodGet, path: "/api/health"},
		{name: "SSE", method: http.MethodGet, path: "/api/event"},
		{name: "upload", method: http.MethodPost, path: "/api/upload", body: strings.Repeat("x", 1024)},
		{name: "WebSocket-shaped upgrade", method: http.MethodGet, path: "/socket", upgrade: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now()
			store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
			if err != nil {
				t.Fatal(err)
			}
			device, err := store.AddDevice("device-token", "Device", now, now.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			started, canceled := make(chan struct{}), make(chan struct{})
			next := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				close(started)
				<-request.Context().Done()
				close(canceled)
			})
			handler := newPairingState(store).handler(next, runtime.ServerAuth{Password: "secret"}, ControlAuth{})
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			request.AddCookie(&http.Cookie{Name: deviceCookieName, Value: "device-token"})
			if isMutation(request) {
				request.Header.Set(csrfHeaderName, mintCSRFToken("device-token", request.Method, request.URL.EscapedPath(), time.Now().Add(csrfTokenTTL)))
			}
			if test.upgrade {
				request.Header.Set("Connection", "Upgrade")
				request.Header.Set("Upgrade", "websocket")
			}
			response := httptest.NewRecorder()
			served := make(chan struct{})
			go func() {
				handler.ServeHTTP(response, request)
				close(served)
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("paired request was not admitted")
			}
			if err := revokeDevice(store, device.ID, store.CancelDeviceRequests); err != nil {
				t.Fatal(err)
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("request context was not canceled")
			}
			select {
			case <-served:
			case <-time.After(time.Second):
				t.Fatal("handler did not complete after cancellation")
			}
		})
	}
}

func TestPairedRequestCannotOutliveDeviceGrant(t *testing.T) {
	now := time.Now()
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	expires := now.Add(100 * time.Millisecond)
	if _, err := store.AddDevice("device-token", "Device", now, expires); err != nil {
		t.Fatal(err)
	}
	started, canceled := make(chan struct{}), make(chan struct{})
	next := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		if deadline, ok := request.Context().Deadline(); !ok || !deadline.Equal(expires) {
			t.Errorf("request deadline = %v, %t; want %v", deadline, ok, expires)
		}
		close(canceled)
	})
	handler := newPairingState(store).remoteHandler(next, runtime.ServerAuth{Password: "secret"})
	request := httptest.NewRequest(http.MethodGet, "/api/event", nil)
	request.AddCookie(&http.Cookie{Name: deviceCookieName, Value: "device-token"})
	served := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), request)
		close(served)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("paired request was not admitted")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("request context survived device expiry")
	}
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("handler did not complete after device expiry")
	}

	reconnect := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	reconnect.AddCookie(&http.Cookie{Name: deviceCookieName, Value: "device-token"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, reconnect)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expired reconnect status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func pairingRequest(code string) *http.Request {
	return pairingRequestWithName(code, "")
}

func pairingRequestWithName(code, name string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/fern/pair", strings.NewReader(url.Values{"code": {code}, "name": {name}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return request
}
