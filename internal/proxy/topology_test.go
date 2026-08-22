package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskapi"
)

func TestHandlersSharePairingAndRegenerateRemoteBackendAuth(t *testing.T) {
	t.Parallel()
	received := make(chan *http.Request, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received <- request.Clone(request.Context())
		origin := request.Header.Get("X-Forwarded-Proto") + "://" + request.Header.Get("X-Forwarded-Host")
		writer.Header().Set("Location", origin+"/generated-location")
		writer.Header().Set("Link", "<"+origin+"/generated-link>; rel=next")
		writer.Header().Add("Set-Cookie", "fern_device=attacker; Path=/; Secure; HttpOnly")
		writer.Header().Add("Set-Cookie", "__Host-fern_device=attacker; Path=/; Secure; HttpOnly")
		writer.Header().Add("Set-Cookie", "ordinary=one, fern_device=attacker; Path=/; Secure")
		writer.Header().Add("Set-Cookie", "opencode_preference=dark; Path=/; Secure")
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	waker := &countingWaker{endpoint: mustParseEndpoint(t, backend.URL)}
	handlers := NewHandlers(waker, runtime.ServerAuth{Password: "backend-secret"}, Controls{
		ControlAuth: ControlAuth{Password: "control-secret"},
	}, TrustedOrigins{Remote: "https://fern.example.ts.net", Operator: "http://127.0.0.1:8081"}, testLogger())

	issue := httptest.NewRequest(http.MethodPost, "/fern/pair/new", nil)
	issue.SetBasicAuth("fern", "control-secret")
	issued := httptest.NewRecorder()
	handlers.Operator.ServeHTTP(issued, issue)
	if issued.Code != http.StatusOK {
		t.Fatalf("issue status = %d", issued.Code)
	}
	var result struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &result); err != nil || result.Code == "" {
		t.Fatalf("issued pairing response = %q, err=%v", issued.Body.String(), err)
	}

	preview := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(preview, httptest.NewRequest(http.MethodGet, "/fern/pair?code="+url.QueryEscape(result.Code), nil))
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "Pair this phone?") {
		t.Fatalf("preview status=%d body=%q", preview.Code, preview.Body.String())
	}
	form := url.Values{"code": {result.Code}, "name": {"Test phone"}}
	confirmRequest := httptest.NewRequest(http.MethodPost, "/fern/pair", strings.NewReader(form.Encode()))
	confirmRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	confirmed := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(confirmed, confirmRequest)
	if confirmed.Code != http.StatusSeeOther || len(confirmed.Result().Cookies()) != 1 {
		t.Fatalf("confirm status=%d cookies=%v", confirmed.Code, confirmed.Result().Cookies())
	}

	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	request.Host = "evil.example:9999"
	request.Header["fOrWaRdEd"] = []string{"for=attacker"}
	request.Header["x-FoRwArDeD-For"] = []string{"attacker"}
	request.Header["X-Forwarded-Proto"] = []string{"http"}
	request.Header["X-Forwarded-Port"] = []string{"1"}
	request.Header["X-Forwarded-Evil"] = []string{"preserve-me"}
	request.AddCookie(confirmed.Result().Cookies()[0])
	request.AddCookie(&http.Cookie{Name: "upstream_cookie", Value: "untrusted"})
	request.SetBasicAuth("fern", "control-secret")
	response := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("paired request status = %d", response.Code)
	}
	if response.Header().Get("Location") != "https://fern.example.ts.net/generated-location" || response.Header().Get("Link") != "<https://fern.example.ts.net/generated-link>; rel=next" {
		t.Fatalf("absolute response headers = %v", response.Header())
	}
	if cookies := response.Header().Values("Set-Cookie"); len(cookies) != 1 || !strings.HasPrefix(cookies[0], "opencode_preference=") {
		t.Fatalf("upstream Set-Cookie filtering = %v", cookies)
	}
	proxied := <-received
	username, password, ok := proxied.BasicAuth()
	if !ok || username != "opencode" || password != "backend-secret" {
		t.Fatalf("backend auth = %q %q %t", username, password, ok)
	}
	if got := proxied.Header.Get("Cookie"); got != "" {
		t.Fatalf("backend Cookie = %q", got)
	}
	if proxied.Host != "fern.example.ts.net" || proxied.Header.Get("X-Forwarded-Host") != "fern.example.ts.net" || proxied.Header.Get("X-Forwarded-Proto") != "https" || proxied.Header.Get("X-Forwarded-Port") != "443" {
		t.Fatalf("remote trusted forwarding = host %q headers %v", proxied.Host, proxied.Header)
	}
	for name := range proxied.Header {
		if strings.EqualFold(name, "Forwarded") || strings.EqualFold(name, "X-Forwarded-For") || strings.EqualFold(name, "X-Forwarded-Evil") {
			t.Fatalf("spoofed forwarding header survived as %q", name)
		}
	}
	for _, path := range []string{"/fern/control", "/fern/ready", "/fern/pair/new", "/fern/api/v1/devices"} {
		deniedRequest := httptest.NewRequest(http.MethodGet, path, nil)
		deniedRequest.AddCookie(confirmed.Result().Cookies()[0])
		denied := httptest.NewRecorder()
		handlers.Remote.ServeHTTP(denied, deniedRequest)
		if denied.Code != http.StatusNotFound {
			t.Fatalf("paired remote route %s status = %d", path, denied.Code)
		}
	}
	if got := waker.wakes.Load(); got != 1 {
		t.Fatalf("denied Fern routes changed wake count to %d", got)
	}
}

func TestOnboardingSetupIsOperatorAuthenticatedAndCallbackIsStateAuthenticated(t *testing.T) {
	t.Parallel()
	onboarding := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
			t.Errorf("onboarding received ambient credentials")
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	handlers := NewHandlers(nil, runtime.ServerAuth{Password: "backend-secret"}, Controls{
		Onboarding: onboarding, ControlAuth: ControlAuth{Password: "control-secret"},
	}, TrustedOrigins{Remote: "https://fern.example.ts.net", Operator: "http://127.0.0.1:8081"}, testLogger())

	callback := httptest.NewRequest(http.MethodGet, "/fern/github/app/callback?code=x&state=y", nil)
	callback.Header.Set("Cookie", "ambient=secret")
	callback.SetBasicAuth("fern", "control-secret")
	response := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(response, callback)
	if response.Code != http.StatusNoContent {
		t.Fatalf("remote callback = %d %s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	handlers.Remote.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/fern/github/app/setup?return=%2Ffern%2Fcontrol", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("remote setup = %d", response.Code)
	}

	setup := httptest.NewRequest(http.MethodGet, "/fern/github/app/setup?return=%2Ffern%2Fcontrol", nil)
	setup.SetBasicAuth("fern", "control-secret")
	response = httptest.NewRecorder()
	handlers.Operator.ServeHTTP(response, setup)
	if response.Code != http.StatusNoContent {
		t.Fatalf("operator setup = %d %s", response.Code, response.Body.String())
	}
}

func TestRemoteRejectsBasicCredentialsAndControlRoutesBeforeWake(t *testing.T) {
	t.Parallel()
	waker := &countingWaker{}
	handlers := NewHandlers(waker, runtime.ServerAuth{Password: "backend-secret"}, Controls{
		ControlAuth: ControlAuth{Password: "control-secret"},
	}, TrustedOrigins{Remote: "https://fern.example.ts.net", Operator: "http://127.0.0.1:8081"}, testLogger())
	for _, credentials := range [][2]string{{}, {"opencode", "backend-secret"}, {"fern", "control-secret"}} {
		request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		if credentials[0] != "" {
			request.SetBasicAuth(credentials[0], credentials[1])
		}
		response := httptest.NewRecorder()
		handlers.Remote.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("remote credentials %q status = %d", credentials[0], response.Code)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/fern/pair/new", nil)
	request.SetBasicAuth("fern", "control-secret")
	response := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("remote pairing issuance status = %d", response.Code)
	}
	if got := waker.wakes.Load(); got != 0 {
		t.Fatalf("remote credential attempts woke workspace %d times", got)
	}
}

func TestTaskRoutesCarryServerOwnedDeviceAndOperatorActors(t *testing.T) {
	t.Parallel()
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	tasks := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		actor, err := taskapi.ContextActor(request.Context())
		if err != nil {
			http.Error(writer, "missing actor", http.StatusUnauthorized)
			return
		}
		if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
			http.Error(writer, "credentials leaked", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(writer).Encode(struct {
			Type task.ActorType `json:"type"`
			ID   string         `json:"id"`
			Auth string         `json:"auth"`
		}{actor.Type, actor.ID, actor.Authentication})
	})
	handlers := NewHandlers(nil, runtime.ServerAuth{Password: "backend-secret"}, Controls{
		Store: store, Tasks: tasks, ControlAuth: ControlAuth{Password: "control-secret"},
	}, TrustedOrigins{Remote: "https://fern.example.ts.net", Operator: "http://127.0.0.1:8081"}, testLogger())

	issue := httptest.NewRequest(http.MethodPost, "/fern/pair/new", nil)
	issue.SetBasicAuth("fern", "control-secret")
	issued := httptest.NewRecorder()
	handlers.Operator.ServeHTTP(issued, issue)
	var pair struct {
		Code string `json:"code"`
	}
	if issued.Code != http.StatusOK || json.Unmarshal(issued.Body.Bytes(), &pair) != nil {
		t.Fatalf("pair issue = %d %s", issued.Code, issued.Body.String())
	}
	form := url.Values{"code": {pair.Code}, "name": {"Task phone"}}
	confirm := httptest.NewRequest(http.MethodPost, "/fern/pair", strings.NewReader(form.Encode()))
	confirm.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	confirmed := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(confirmed, confirm)
	if confirmed.Code != http.StatusSeeOther || len(confirmed.Result().Cookies()) != 1 {
		t.Fatalf("pair confirm = %d", confirmed.Code)
	}

	remoteRequest := httptest.NewRequest(http.MethodGet, "/fern/api/v1/events", nil)
	remoteRequest.AddCookie(confirmed.Result().Cookies()[0])
	remote := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(remote, remoteRequest)
	var remoteActor struct {
		Type task.ActorType `json:"type"`
		ID   string         `json:"id"`
		Auth string         `json:"auth"`
	}
	if remote.Code != http.StatusOK || json.Unmarshal(remote.Body.Bytes(), &remoteActor) != nil || remoteActor.Type != task.ActorDevice || remoteActor.ID == "" || remoteActor.Auth != "fern_device_cookie" {
		t.Fatalf("remote actor = %d %+v %s", remote.Code, remoteActor, remote.Body.String())
	}

	operatorRequest := httptest.NewRequest(http.MethodGet, "/fern/api/v1/events", nil).WithContext(context.Background())
	operatorRequest.SetBasicAuth("fern", "control-secret")
	operator := httptest.NewRecorder()
	handlers.Operator.ServeHTTP(operator, operatorRequest)
	var operatorActor struct {
		Type task.ActorType `json:"type"`
		ID   string         `json:"id"`
		Auth string         `json:"auth"`
	}
	if operator.Code != http.StatusOK || json.Unmarshal(operator.Body.Bytes(), &operatorActor) != nil || operatorActor.Type != task.ActorOperator || operatorActor.ID != "local-operator" || operatorActor.Auth != "basic" {
		t.Fatalf("operator actor = %d %+v %s", operator.Code, operatorActor, operator.Body.String())
	}

	deniedRequest := httptest.NewRequest(http.MethodGet, "/fern/api/v1/devices", nil)
	deniedRequest.AddCookie(confirmed.Result().Cookies()[0])
	denied := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(denied, deniedRequest)
	if denied.Code != http.StatusNotFound {
		t.Fatalf("remote operator route = %d", denied.Code)
	}
}

func TestOperatorRegeneratesCanonicalBackendAuth(t *testing.T) {
	t.Parallel()
	received := make(chan *http.Request, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received <- request.Clone(request.Context())
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	handlers := NewHandlers(&countingWaker{endpoint: mustParseEndpoint(t, backend.URL)}, runtime.ServerAuth{Password: "backend-secret"}, Controls{}, TrustedOrigins{Remote: "https://fern.example.ts.net:8443", Operator: "http://127.0.0.1:8081"}, testLogger())
	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	request.Host = "spoofed.example"
	request.Header.Set("Origin", "http://127.0.0.1:8081")
	request.Header.Set("Connection", "upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.SetBasicAuth("opencode", "backend-secret")
	request.AddCookie(&http.Cookie{Name: "fern_device", Value: "not-operator-auth"})
	response := httptest.NewRecorder()
	handlers.Operator.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("operator status = %d", response.Code)
	}
	proxied := <-received
	username, password, ok := proxied.BasicAuth()
	if !ok || username != "opencode" || password != "backend-secret" || proxied.Header.Get("Cookie") != "" {
		t.Fatalf("proxied operator auth/cookie = %q %q %t %q", username, password, ok, proxied.Header.Get("Cookie"))
	}
	if proxied.Host != "127.0.0.1:8081" || proxied.Header.Get("X-Forwarded-Host") != "127.0.0.1:8081" || proxied.Header.Get("X-Forwarded-Proto") != "http" || proxied.Header.Get("X-Forwarded-Port") != "8081" {
		t.Fatalf("operator trusted forwarding = host %q headers %v", proxied.Host, proxied.Header)
	}
	if proxied.Header.Get("Origin") != "http://127.0.0.1:8081" {
		t.Fatalf("browser Origin was changed to %q", proxied.Header.Get("Origin"))
	}
}

func TestTrustedOriginEffectivePorts(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		raw  string
		port string
	}{
		{raw: "https://fern.example.ts.net", port: "443"},
		{raw: "https://fern.example.ts.net:8443", port: "8443"},
		{raw: "http://127.0.0.1:8081", port: "8081"},
	} {
		if got := parseTrustedOrigin(test.raw); got.port != test.port {
			t.Errorf("parseTrustedOrigin(%q).port = %q, want %q", test.raw, got.port, test.port)
		}
	}
}

func TestProductionHandlersRejectUntrustedOriginConfiguration(t *testing.T) {
	t.Parallel()
	for _, origins := range []TrustedOrigins{
		{Remote: "http://192.0.2.1:8080", Operator: "http://127.0.0.1:8081"},
		{Remote: "https://fern.example/path", Operator: "http://127.0.0.1:8081"},
		{Remote: "https://fern.example", Operator: "https://127.0.0.1:8081"},
		{Remote: "https://fern.example", Operator: "http://192.0.2.1:8081"},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewHandlers accepted origins %+v", origins)
				}
			}()
			NewHandlers(nil, runtime.ServerAuth{Password: "secret"}, Controls{}, origins, testLogger())
		}()
	}
}
