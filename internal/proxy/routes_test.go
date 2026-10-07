package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/pluginauth"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore/taskstoretest"
)

const (
	testRemoteOrigin   = "https://fern.example.ts.net"
	testOperatorOrigin = "http://127.0.0.1:8081"
	testPassword       = "control-password"
	testDeviceToken    = "device-token-0123456789"
)

type credential int

const (
	anon credential = iota
	cookie
	withCSRF // device cookie plus a valid CSRF token for the exact method and path
	bearer
	badBearer
	twoHeaders
	basic
	badBasic
)

// R and O select the remote and operator listener.
const R, O = true, false

type routeFixture struct {
	store        *control.Store
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
	database, _ := taskstoretest.Open(t)
	store := control.New(database.DB())
	now := time.Now()
	phone, err := store.AddDevice(testDeviceToken, "phone", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	victim, err := store.AddDevice("victim-token-0123456789", "tablet", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	plugins := pluginauth.New(database.DB())
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
	fixture := &routeFixture{store: store, bearer: approved.DeviceCode, authID: pending.AuthorizationID, userCode: pending.UserCode,
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
		Runs:     stub("runs"),
		Liveness: stub("live"), Readiness: stub("ready"),
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
	case cookie, withCSRF:
		request.AddCookie(&http.Cookie{Name: deviceCookieName, Value: testDeviceToken})
		if cred == withCSRF {
			request.Header.Set(csrfHeaderName, mintCSRFToken(testDeviceToken, method, request.URL.EscapedPath(), time.Now().Add(time.Minute)))
		}
	case bearer:
		request.Header.Set("Authorization", "Bearer "+fixture.bearer)
	case badBearer:
		request.Header.Set("Authorization", "Bearer not-a-credential")
	case twoHeaders:
		request.Header.Add("Authorization", "Bearer "+fixture.bearer)
		request.Header.Add("Authorization", "Basic Zm9vOmJhcg==")
	case basic:
		request.SetBasicAuth("fern", testPassword)
	case badBasic:
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
		{R, "POST", "/fern/api/plugin-auth/start", anon, false, "{}", 201, "", ""},
		{R, "GET", "/fern/api/plugin-auth/start", anon, false, "", 405, "", "Allow: POST"},
		{R, "POST", "/fern/api/plugin-auth/poll", anon, false, `{"device_code":"x"}`, 401, "", ""},
		{R, "POST", "/fern/api/plugin-auth/start", bearer, false, "{}", 201, "", ""},
		{R, "GET", "/fern/pair?code=x", anon, false, "", 401, "", ""},
		{R, "PUT", "/fern/pair", anon, false, "", 405, "", ""},
		// The removed GitHub App manifest onboarding routes stay unrouted.
		{R, "GET", "/fern/github/app/callback", anon, false, "", 404, "", ""},

		// Remote: paired device.
		{R, "GET", "/fern/", anon, false, "", 401, "", ""},
		{R, "GET", "/fern/", cookie, false, "", 200, "", "Cache-Control: no-store"},
		{R, "HEAD", "/fern/", cookie, false, "", 200, "", ""},
		{R, "GET", "/fern", cookie, false, "", 308, "", "Location: /fern/"},
		{R, "POST", "/fern/", withCSRF, false, "", 405, "", ""},
		{R, "GET", "/fern/control", cookie, false, "", 404, "", ""},
		{R, "GET", "/fern/api/v1/csrf?method=POST&path=/fern/api/runs", cookie, false, "", 200, "", ""},
		{R, "GET", "/fern/api/v1/csrf?method=POST&path=/fern/api/runs", anon, false, "", 401, "", ""},
		{R, "GET", "/fern/plugin-auth/authorize?id={auth}&code={code}", cookie, false, "", 200, "", ""},
		{R, "GET", "/fern/plugin-auth/authorize?id={auth}&code={code}", bearer, false, "", 404, "", ""},
		{R, "POST", "/fern/api/plugin-auth/requests/{auth}/approve", withCSRF, false, `{"user_code":"{code}"}`, 204, "", ""},
		{R, "POST", "/fern/api/plugin-auth/requests/{auth}/deny", withCSRF, false, `{"user_code":"{code}"}`, 204, "", ""},
		{R, "POST", "/fern/api/plugin-auth/requests/{auth}/approve", cookie, false, `{"user_code":"{code}"}`, 403, "", ""},
		{R, "POST", "/fern/api/plugin-auth/requests/{auth}/approve", withCSRF, true, `{"user_code":"{code}"}`, 403, "", ""},
		{R, "POST", "/fern/api/plugin-auth/requests/{auth}/approve", bearer, false, `{"user_code":"{code}"}`, 404, "", ""},
		// runapi only admits plugin actors, so devices and the operator used to
		// reach it just to be refused there; the route table now says so.
		{R, "GET", "/fern/api/runs", cookie, false, "", 404, "", ""},
		{R, "POST", "/fern/api/runs", withCSRF, false, "", 404, "", ""},
		{R, "GET", "/fern/api/runs/%61bc", cookie, false, "", 404, "", ""},
		{R, "GET", "/fern/api/runs/run/attach", cookie, false, "", 404, "", ""},
		{R, "GET", "/fern/api/v1/devices", cookie, false, "", 404, "", ""},
		{R, "GET", "/fern/api/plugin-auth/credentials", cookie, false, "", 404, "", ""},
		{R, "GET", "/fern/github/app/setup", cookie, false, "", 404, "", ""},
		{R, "GET", "/fern/live", cookie, false, "", 404, "", ""},
		{R, "GET", "/fern/pair/new", cookie, false, "", 404, "", ""},
		{R, "GET", "/elsewhere", cookie, false, "", 404, "", ""},
		// Unknown routes and methods are answered before authentication (they
		// used to need a device cookie first, answering 401).
		{R, "GET", "/elsewhere", anon, false, "", 404, "", ""},
		{R, "POST", "/fern/", anon, false, "", 405, "", "Allow: GET, HEAD"},

		// Remote: bearer bearer.
		{R, "GET", "/fern/api/runs", bearer, false, "", 200, "runs", ""},
		{R, "POST", "/fern/api/runs/run/stop", bearer, false, "", 200, "runs", ""},
		{R, "POST", "/fern/api/runs", bearer, true, "", 403, "", ""},
		{R, "GET", "/fern/api/runs/run/attach", bearer, false, "", 200, "runs", ""},
		{R, "GET", "/fern/api/v1/runs", bearer, false, "", 404, "", ""},
		{R, "POST", "/fern/api/plugin-auth/self/revoke", bearer, false, "", 204, "", ""},
		{R, "GET", "/fern/api/plugin-auth/self/revoke", bearer, false, "", 405, "", "Allow: POST"},
		{R, "POST", "/fern/api/plugin-auth/self/revoke", withCSRF, false, "", 404, "", ""},
		{R, "GET", "/fern/", bearer, false, "", 404, "", ""},
		{R, "GET", "/fern/api/runs/%61bc", bearer, false, "", 404, "", ""},
		{R, "GET", "/fern/api/runs", badBearer, false, "", 401, "", `WWW-Authenticate: Bearer realm="fern-plugin"`},
		{R, "GET", "/fern/api/runs", twoHeaders, false, "", 401, "", ""},

		// Operator: probes precede authentication.
		{O, "GET", "/fern/live", anon, false, "", 200, "live", ""},
		{O, "GET", "/fern/ready", anon, false, "", 200, "ready", ""},
		{O, "GET", "/fern/control", anon, false, "", 401, "", `WWW-Authenticate: Basic realm="fern-control"`},
		{O, "GET", "/fern/control", badBasic, false, "", 401, "", ""},
		{O, "GET", "/fern/control", bearer, false, "", 404, "", ""},
		{O, "GET", "/fern/live", bearer, false, "", 404, "", ""},
		{O, "GET", "/fern/control", cookie, false, "", 401, "", ""},
		{O, "GET", "/fern/status", basic, false, "", 404, "", ""},
		{O, "GET", "/fern/metrics", basic, false, "", 404, "", ""},

		// Operator: pages, pairing, devices.
		{O, "GET", "/fern/", basic, false, "", 200, "", ""},
		{O, "GET", "/fern/control", basic, false, "", 200, "", ""},
		{O, "GET", "/fern", basic, false, "", 308, "", ""},
		{O, "POST", "/fern/pair/new", basic, false, "", 200, "", ""},
		{O, "GET", "/fern/pair/new", basic, false, "", 405, "", "Allow: POST"},
		{O, "GET", "/fern/pair?code=x", basic, false, "", 404, "", ""},
		{O, "GET", "/fern/pair?code=x", anon, false, "", 404, "", ""},
		{O, "GET", "/fern/api/v1/devices", basic, false, "", 200, "", ""},
		{O, "POST", "/fern/api/v1/devices", basic, false, "", 405, "", ""},
		{O, "DELETE", "/fern/api/v1/devices/{victim}", basic, false, "", 204, "", ""},
		{O, "DELETE", "/fern/api/v1/devices/{victim}", basic, true, "", 403, "", ""},
		{O, "DELETE", "/fern/api/v1/devices/missing", basic, false, "", 404, "", ""},
		{O, "GET", "/fern/api/v1/devices/%2e", basic, false, "", 404, "", ""},
		{O, "POST", "/fern/devices/{victim}/revoke", basic, false, "", 200, "", ""},
		{O, "POST", "/fern/devices/{victim}/revoke", basic, true, "", 403, "", ""},
		{O, "GET", "/fern/api/v1/csrf?method=POST&path=/x", basic, false, "", 404, "", ""},

		// Operator: bearer credential administration.
		{O, "GET", "/fern/api/plugin-auth/credentials", basic, false, "", 200, "", ""},
		{O, "DELETE", "/fern/api/plugin-auth/credentials/{cred}", basic, false, "", 204, "", ""},
		{O, "DELETE", "/fern/api/plugin-auth/credentials/{cred}", basic, true, "", 403, "", ""},
		{O, "POST", "/fern/api/plugin-auth/requests/{auth}/approve", basic, false, `{"user_code":"{code}"}`, 204, "", ""},
		{O, "GET", "/fern/plugin-auth/authorize?id={auth}&code={code}", basic, false, "", 404, "", ""},

		// Operator: run APIs.
		{O, "GET", "/fern/api/runs", basic, false, "", 200, "runs", ""},
		{O, "GET", "/fern/api/runs/run", basic, false, "", 200, "runs", ""},
		{O, "GET", "/fern/api/runs/run/attach", basic, false, "", 200, "runs", ""},
		{O, "GET", "/fern/api/runs", anon, false, "", 401, "", ""},
		{O, "POST", "/fern/api/runs", basic, false, "{}", 405, "", "Allow: GET, HEAD"},
		{O, "POST", "/fern/api/runs/run/stop", basic, false, "{}", 404, "", ""},
		{O, "GET", "/fern/api/runs/run/result", basic, false, "", 404, "", ""},
		{O, "GET", "/fern/api/v1/runs", basic, false, "", 404, "", ""},
		{O, "GET", "/fern/github/app/setup", basic, false, "", 404, "", ""},
		{O, "GET", "/fern/github/app/callback", basic, false, "", 404, "", ""},
		{O, "GET", "/elsewhere", basic, false, "", 404, "", ""},
		{O, "GET", "/fern/api/v1/results/res_x/publications", basic, false, "", 404, "", ""},
		// Plugin authorization is remote-only (this used to fall through to 405).
		{O, "POST", "/fern/api/plugin-auth/start", basic, false, "{}", 404, "", ""},
		// Every operator mutation now requires the same origin, not just /fern/api
		// and /fern/devices paths.
		{O, "POST", "/fern/pair/new", basic, true, "", 403, "", ""},
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
	t.Run("plugin", func(t *testing.T) {
		fixture := newRouteFixture(t)
		fixture.block = make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			fixture.handlers.Remote.ServeHTTP(httptest.NewRecorder(), fixture.request(R, "GET", "/fern/api/runs", bearer, false, ""))
		}()
		<-fixture.block
		fixture.revoke(t, "/fern/api/plugin-auth/credentials/{cred}")
		awaitDone(t, done)
	})
	t.Run("device", func(t *testing.T) {
		fixture := newRouteFixture(t)
		request, release, ok := newPairingState(fixture.store).authenticate(httptest.NewRecorder(), fixture.request(R, "GET", "/fern/", cookie, false, ""))
		if !ok {
			t.Fatal("device was not admitted")
		}
		defer release()
		fixture.revoke(t, "/fern/api/v1/devices/{phone}")
		awaitDone(t, request.Context().Done())
	})
}

func (fixture *routeFixture) revoke(t *testing.T, path string) {
	t.Helper()
	response := httptest.NewRecorder()
	fixture.handlers.Operator.ServeHTTP(response, fixture.request(O, "DELETE", path, basic, false, ""))
	if response.Code != http.StatusNoContent {
		t.Fatalf("revoke status=%d", response.Code)
	}
}

func awaitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request was not cancelled by revocation")
	}
}
