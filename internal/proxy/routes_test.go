package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/pluginauth"
	"github.com/nebler/fern/internal/task"
)

const (
	testRemoteOrigin   = "https://fern.example.ts.net"
	testOperatorOrigin = "http://127.0.0.1:8081"
	testPassword       = "control-password"
	testDeviceToken    = "device-token-0123456789"
)

type credential int

const (
	none credential = iota
	device
	deviceCSRF // device cookie plus a valid CSRF token for the exact method and path
	plugin
	badPlugin
	twoAuthorizations
	operator
	badOperator
)

type routeFixture struct {
	handlers     Handlers
	bearer       string
	authID       string
	userCode     string
	credentialID string
	victimID     string
	phoneID      string
	block        chan struct{}
}

func newRouteFixture(t *testing.T) *routeFixture {
	t.Helper()
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "workspace")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	phone, err := store.AddDevice(testDeviceToken, "phone", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	victim, err := store.AddDevice("victim-token-0123456789", "tablet", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	plugins, err := pluginauth.Open(store, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	approver := task.ActorSnapshot{Type: task.ActorOperator, ID: "local-operator", DisplayName: "Local operator",
		CredentialID: "control-test", Authentication: "basic", RequestID: "request-test"}
	approved, err := plugins.Start(now.Add(-2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	credential, err := plugins.Approve(context.Background(), approved.AuthorizationID, approved.UserCode, approver, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := plugins.Start(now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &routeFixture{bearer: approved.DeviceCode, authID: pending.AuthorizationID, userCode: pending.UserCode,
		credentialID: credential.ID, victimID: victim.ID, phoneID: phone.ID}
	stub := func(name string) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("X-Stub", name)
			if fixture.block != nil {
				close(fixture.block)
				<-request.Context().Done()
				return
			}
			writer.WriteHeader(http.StatusOK)
		})
	}
	fixture.handlers, err = NewHandlers(Controls{
		Store: store, PluginAuth: plugins, ControlAuth: ControlAuth{Password: testPassword},
		Runs: stub("runs"), RunClients: stub("run-clients"), Onboarding: stub("onboarding"),
		Liveness: stub("live"), Readiness: stub("ready"), Status: stub("status"), Metrics: stub("metrics"),
	}, TrustedOrigins{Remote: testRemoteOrigin, Operator: testOperatorOrigin})
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture *routeFixture) expand(text string) string {
	return strings.NewReplacer("{auth}", fixture.authID, "{code}", fixture.userCode, "{cred}", fixture.credentialID,
		"{victim}", fixture.victimID, "{phone}", fixture.phoneID).Replace(text)
}

func (fixture *routeFixture) request(remote bool, method, target string, cred credential, crossOrigin bool, body string) *http.Request {
	origin := testOperatorOrigin
	if remote {
		origin = testRemoteOrigin
	}
	request := httptest.NewRequest(method, origin+fixture.expand(target), strings.NewReader(fixture.expand(body)))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	switch cred {
	case device, deviceCSRF:
		request.AddCookie(&http.Cookie{Name: deviceCookieName, Value: testDeviceToken})
		if cred == deviceCSRF {
			request.Header.Set(csrfHeaderName, mintCSRFToken(testDeviceToken, method, request.URL.EscapedPath(), time.Now().Add(time.Minute)))
		}
	case plugin:
		request.Header.Set("Authorization", "Bearer "+fixture.bearer)
	case badPlugin:
		request.Header.Set("Authorization", "Bearer not-a-credential")
	case twoAuthorizations:
		request.Header.Add("Authorization", "Bearer "+fixture.bearer)
		request.Header.Add("Authorization", "Basic Zm9vOmJhcg==")
	case operator:
		request.SetBasicAuth("fern", testPassword)
	case badOperator:
		request.SetBasicAuth("fern", "wrong")
	}
	if crossOrigin {
		request.Header.Set("Origin", "https://evil.example")
	} else if method != http.MethodGet && method != http.MethodHead {
		request.Header.Set("Origin", origin)
	}
	return request
}

// TestRouteTable pins every listener's method x path x credential decision.
// Each case uses a fresh fixture so state-changing routes stay independent.
func TestRouteTable(t *testing.T) {
	const R, O = true, false
	cases := []struct {
		remote bool
		method string
		path   string
		cred   credential
		cross  bool
		body   string
		status int
		stub   string
		header string // "Name: value" that must be present
	}{
		// Remote: unauthenticated plugin authorization and pairing.
		{R, "POST", "/fern/api/plugin-auth/start", none, false, "{}", 201, "", ""},
		{R, "GET", "/fern/api/plugin-auth/start", none, false, "", 405, "", "Allow: POST"},
		{R, "POST", "/fern/api/plugin-auth/poll", none, false, `{"device_code":"x"}`, 401, "", ""},
		{R, "POST", "/fern/api/plugin-auth/start", plugin, false, "{}", 201, "", ""},
		{R, "GET", "/fern/pair?code=x", none, false, "", 401, "", ""},
		{R, "PUT", "/fern/pair", none, false, "", 405, "", ""},
		{R, "GET", "/fern/github/app/callback", none, false, "", 200, "onboarding", ""},
		{R, "GET", "/fern/github/app/callback", plugin, false, "", 200, "onboarding", ""},

		// Remote: paired device.
		{R, "GET", "/fern/", none, false, "", 401, "", ""},
		{R, "GET", "/fern/", device, false, "", 200, "", "Cache-Control: no-store"},
		{R, "HEAD", "/fern/", device, false, "", 200, "", ""},
		{R, "GET", "/fern", device, false, "", 308, "", "Location: /fern/"},
		{R, "POST", "/fern/", deviceCSRF, false, "", 405, "", ""},
		{R, "GET", "/fern/control", device, false, "", 404, "", ""},
		{R, "GET", "/fern/api/v1/csrf?method=POST&path=/fern/api/runs", device, false, "", 200, "", ""},
		{R, "GET", "/fern/api/v1/csrf?method=POST&path=/fern/api/runs", none, false, "", 401, "", ""},
		{R, "GET", "/fern/plugin-auth/authorize?id={auth}&code={code}", device, false, "", 200, "", ""},
		{R, "GET", "/fern/plugin-auth/authorize?id={auth}&code={code}", plugin, false, "", 404, "", ""},
		{R, "POST", "/fern/api/plugin-auth/requests/{auth}/approve", deviceCSRF, false, `{"user_code":"{code}"}`, 204, "", ""},
		{R, "POST", "/fern/api/plugin-auth/requests/{auth}/deny", deviceCSRF, false, `{"user_code":"{code}"}`, 204, "", ""},
		{R, "POST", "/fern/api/plugin-auth/requests/{auth}/approve", device, false, `{"user_code":"{code}"}`, 403, "", ""},
		{R, "POST", "/fern/api/plugin-auth/requests/{auth}/approve", deviceCSRF, true, `{"user_code":"{code}"}`, 403, "", ""},
		{R, "POST", "/fern/api/plugin-auth/requests/{auth}/approve", plugin, false, `{"user_code":"{code}"}`, 404, "", ""},
		{R, "GET", "/fern/api/runs", device, false, "", 200, "runs", ""},
		{R, "POST", "/fern/api/runs", deviceCSRF, false, "", 200, "runs", ""},
		{R, "POST", "/fern/api/runs", device, false, "", 403, "", ""},
		{R, "GET", "/fern/api/runs/%61bc", device, false, "", 404, "", ""},
		{R, "GET", "/fern/api/v1/runs", device, false, "", 404, "", ""},
		{R, "GET", "/fern/api/v1/devices", device, false, "", 404, "", ""},
		{R, "GET", "/fern/api/plugin-auth/credentials", device, false, "", 404, "", ""},
		{R, "GET", "/fern/github/app/setup", device, false, "", 404, "", ""},
		{R, "GET", "/fern/status", device, false, "", 404, "", ""},
		{R, "GET", "/fern/live", device, false, "", 404, "", ""},
		{R, "GET", "/fern/pair/new", device, false, "", 404, "", ""},
		{R, "GET", "/elsewhere", device, false, "", 404, "", ""},

		// Remote: plugin bearer.
		{R, "GET", "/fern/api/runs", plugin, false, "", 200, "runs", ""},
		{R, "POST", "/fern/api/runs/run/stop", plugin, false, "", 200, "runs", ""},
		{R, "POST", "/fern/api/runs", plugin, true, "", 403, "", ""},
		{R, "GET", "/fern/api/v1/runs", plugin, false, "", 200, "run-clients", ""},
		{R, "GET", "/fern/api/v1/runs/run/attach", plugin, false, "", 200, "run-clients", ""},
		{R, "POST", "/fern/api/plugin-auth/self/revoke", plugin, false, "", 204, "", ""},
		{R, "GET", "/fern/api/plugin-auth/self/revoke", plugin, false, "", 405, "", "Allow: POST"},
		{R, "POST", "/fern/api/plugin-auth/self/revoke", deviceCSRF, false, "", 404, "", ""},
		{R, "GET", "/fern/", plugin, false, "", 404, "", ""},
		{R, "GET", "/fern/api/runs/%61bc", plugin, false, "", 404, "", ""},
		{R, "GET", "/fern/api/runs", badPlugin, false, "", 401, "", `WWW-Authenticate: Bearer realm="fern-plugin"`},
		{R, "GET", "/fern/api/runs", twoAuthorizations, false, "", 401, "", ""},

		// Operator: probes precede authentication.
		{O, "GET", "/fern/live", none, false, "", 200, "live", ""},
		{O, "GET", "/fern/ready", none, false, "", 200, "ready", ""},
		{O, "GET", "/fern/status", none, false, "", 401, "", `WWW-Authenticate: Basic realm="fern-control"`},
		{O, "GET", "/fern/status", badOperator, false, "", 401, "", ""},
		{O, "GET", "/fern/status", operator, false, "", 200, "status", ""},
		{O, "GET", "/fern/metrics", operator, false, "", 200, "metrics", ""},
		{O, "GET", "/fern/status", plugin, false, "", 404, "", ""},
		{O, "GET", "/fern/live", plugin, false, "", 404, "", ""},
		{O, "GET", "/fern/status", device, false, "", 401, "", ""},

		// Operator: pages, pairing, devices.
		{O, "GET", "/fern/", operator, false, "", 200, "", ""},
		{O, "GET", "/fern/control", operator, false, "", 200, "", ""},
		{O, "GET", "/fern", operator, false, "", 308, "", ""},
		{O, "POST", "/fern/pair/new", operator, false, "", 200, "", ""},
		{O, "GET", "/fern/pair/new", operator, false, "", 405, "", "Allow: POST"},
		{O, "GET", "/fern/pair?code=x", operator, false, "", 404, "", ""},
		{O, "GET", "/fern/pair?code=x", none, false, "", 404, "", ""},
		{O, "GET", "/fern/api/v1/devices", operator, false, "", 200, "", ""},
		{O, "POST", "/fern/api/v1/devices", operator, false, "", 405, "", ""},
		{O, "DELETE", "/fern/api/v1/devices/{victim}", operator, false, "", 204, "", ""},
		{O, "DELETE", "/fern/api/v1/devices/{victim}", operator, true, "", 403, "", ""},
		{O, "DELETE", "/fern/api/v1/devices/missing", operator, false, "", 404, "", ""},
		{O, "GET", "/fern/api/v1/devices/%2e", operator, false, "", 404, "", ""},
		{O, "POST", "/fern/devices/{victim}/revoke", operator, false, "", 200, "", ""},
		{O, "POST", "/fern/devices/{victim}/revoke", operator, true, "", 403, "", ""},
		{O, "GET", "/fern/api/v1/csrf?method=POST&path=/x", operator, false, "", 404, "", ""},

		// Operator: plugin credential administration.
		{O, "GET", "/fern/api/plugin-auth/credentials", operator, false, "", 200, "", ""},
		{O, "DELETE", "/fern/api/plugin-auth/credentials/{cred}", operator, false, "", 204, "", ""},
		{O, "DELETE", "/fern/api/plugin-auth/credentials/{cred}", operator, true, "", 403, "", ""},
		{O, "POST", "/fern/api/plugin-auth/requests/{auth}/approve", operator, false, `{"user_code":"{code}"}`, 204, "", ""},
		{O, "GET", "/fern/plugin-auth/authorize?id={auth}&code={code}", operator, false, "", 404, "", ""},

		// Operator: run APIs and onboarding.
		{O, "GET", "/fern/api/runs", operator, false, "", 200, "runs", ""},
		{O, "GET", "/fern/api/v1/runs", operator, false, "", 200, "run-clients", ""},
		{O, "GET", "/fern/api/v1/runs/run/attach", operator, false, "", 200, "run-clients", ""},
		{O, "GET", "/fern/github/app/setup", operator, false, "", 200, "onboarding", ""},
		{O, "GET", "/fern/github/app/setup", operator, true, "", 403, "", ""},
		{O, "GET", "/fern/github/app/callback", operator, false, "", 200, "onboarding", ""},
		{O, "GET", "/fern/github/app/callback", none, false, "", 401, "", ""},
		{O, "GET", "/elsewhere", operator, false, "", 404, "", ""},
	}
	for _, tc := range cases {
		listener := "operator"
		if tc.remote {
			listener = "remote"
		}
		name := listener + " " + tc.method + " " + tc.path
		t.Run(name, func(t *testing.T) {
			fixture := newRouteFixture(t)
			handler := fixture.handlers.Operator
			if tc.remote {
				handler = fixture.handlers.Remote
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, fixture.request(tc.remote, tc.method, tc.path, tc.cred, tc.cross, tc.body))
			if response.Code != tc.status || response.Header().Get("X-Stub") != tc.stub {
				t.Fatalf("cred=%d cross=%t: status=%d stub=%q, want %d %q (body %q)", tc.cred, tc.cross,
					response.Code, response.Header().Get("X-Stub"), tc.status, tc.stub, response.Body.String())
			}
			if name, value, found := strings.Cut(tc.header, ": "); found && response.Header().Get(name) != value {
				t.Fatalf("%s=%q, want %q", name, response.Header().Get(name), value)
			}
		})
	}
}

// TestRevocationCancelsInFlightRequests proves both remote realms fence
// in-flight work against durable revocation.
func TestRevocationCancelsInFlightRequests(t *testing.T) {
	for _, tc := range []struct {
		cred   credential
		revoke string
	}{
		{plugin, "/fern/api/plugin-auth/credentials/{cred}"},
		{device, "/fern/api/v1/devices/{phone}"},
	} {
		t.Run(tc.revoke, func(t *testing.T) {
			fixture := newRouteFixture(t)
			fixture.block = make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				fixture.handlers.Remote.ServeHTTP(httptest.NewRecorder(), fixture.request(true, "GET", "/fern/api/runs", tc.cred, false, ""))
			}()
			<-fixture.block
			response := httptest.NewRecorder()
			fixture.handlers.Operator.ServeHTTP(response, fixture.request(false, "DELETE", tc.revoke, operator, false, ""))
			if response.Code != http.StatusNoContent {
				t.Fatalf("revoke status=%d", response.Code)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("in-flight request was not cancelled by revocation")
			}
		})
	}
}
