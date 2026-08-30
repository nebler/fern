package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/pluginauth"
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskapi"
)

func TestPluginBearerInstallsFixedAuthorityAndCannotEscapeReservedRoutes(t *testing.T) {
	store, credential, bearer, now := testPluginCredential(t)
	handler := newPluginAuthHTTP(store)
	handler.now = func() time.Time { return now }
	var forwarded atomic.Int64
	next := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		forwarded.Add(1)
		actor, err := taskapi.ContextActor(request.Context())
		if err != nil || actor.Type != task.ActorOpenCode || actor.ID != credential.ID || actor.CredentialID != credential.ID || actor.Authentication != "fern_plugin_bearer" {
			t.Errorf("plugin actor = %+v, %v", actor, err)
		}
		authorization, ok := pluginauth.RequestAuthorizationFromContext(request.Context())
		if !ok || !authorization.HasScope("run:create") || authorization.HasScope("control:admin") {
			t.Errorf("plugin scope context = %+v, %t", authorization, ok)
		}
		if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
			t.Errorf("credentials leaked downstream: %v", request.Header)
		}
		if deadline, ok := request.Context().Deadline(); !ok || !deadline.Equal(credential.ExpiresAt) {
			t.Errorf("request deadline = %v, %t; want %v", deadline, ok, credential.ExpiresAt)
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	paired := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("bearer fell through to paired authentication") })
	remote := handler.remoteHandler(paired, next)

	request := httptest.NewRequest(http.MethodPost, "/fern/api/runs", strings.NewReader(`{"instruction":"secret"}`))
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Cookie", "ambient=secret")
	response := httptest.NewRecorder()
	remote.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || forwarded.Load() != 1 {
		t.Fatalf("reserved run route = %d, forwarded=%d", response.Code, forwarded.Load())
	}

	for _, path := range []string{"/", "/api/health", "/fern/control", "/fern/api/v1/tasks", "/fern/pair/new", "/fern/api/plugin-auth/credentials"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+bearer)
		response := httptest.NewRecorder()
		remote.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("bearer route %s status = %d", path, response.Code)
		}
	}
	if forwarded.Load() != 1 {
		t.Fatalf("denied routes reached handler %d times", forwarded.Load())
	}

	for _, value := range []string{"bearer " + bearer, "Bearer  " + bearer, "Bearer " + bearer + " ", "Bearer " + bearer + ",x"} {
		request := httptest.NewRequest(http.MethodGet, "/fern/api/runs", nil)
		request.Header.Set("Authorization", value)
		response := httptest.NewRecorder()
		remote.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("non-exact Authorization %q status = %d", value, response.Code)
		}
		if response.Header().Get("WWW-Authenticate") != `Bearer realm="fern-plugin"` {
			t.Fatalf("non-exact Authorization %q challenge = %q", value, response.Header().Get("WWW-Authenticate"))
		}
	}
	multiple := httptest.NewRequest(http.MethodGet, "/fern/api/runs", nil)
	multiple.Header["Authorization"] = []string{"Bearer " + bearer, "Basic ambient"}
	multipleResponse := httptest.NewRecorder()
	remote.ServeHTTP(multipleResponse, multiple)
	if multipleResponse.Code != http.StatusUnauthorized || multipleResponse.Header().Get("WWW-Authenticate") != `Bearer realm="fern-plugin"` {
		t.Fatalf("multiple Authorization headers = %d %q", multipleResponse.Code, multipleResponse.Header().Get("WWW-Authenticate"))
	}
}

func TestPluginPublicOperatorAndSelfRevokeRoutes(t *testing.T) {
	controlStore, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	pluginStore, err := pluginauth.Open(controlStore, "demo")
	if err != nil {
		t.Fatal(err)
	}
	waker := &countingWaker{}
	var runRequests atomic.Int64
	runs := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if actor, err := taskapi.ContextActor(request.Context()); err != nil || actor.Type != task.ActorOpenCode {
			t.Errorf("run actor = %+v, error = %v", actor, err)
		}
		runRequests.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	})
	handlers, err := NewHandlers(waker, runtime.ServerAuth{Password: "backend-secret"}, Controls{
		Store: controlStore, PluginAuth: pluginStore, Runs: runs, ControlAuth: ControlAuth{Password: "control-secret"},
	}, TrustedOrigins{Remote: "https://fern.example.ts.net", Operator: "http://127.0.0.1:8081"}, testLogger())
	if err != nil {
		t.Fatal(err)
	}

	start := httptest.NewRequest(http.MethodPost, pluginAuthStartPath, strings.NewReader(`{}`))
	start.Header.Set("Content-Type", "application/json")
	start.Header.Set("Cookie", "ambient=secret")
	start.Header.Set("Authorization", "Basic ambient")
	start.Host = "spoofed.example"
	startedResponse := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(startedResponse, start)
	var started struct {
		AuthorizationID string   `json:"authorization_id"`
		DeviceCode      string   `json:"device_code"`
		UserCode        string   `json:"user_code"`
		VerificationURI string   `json:"verification_uri"`
		CompleteURI     string   `json:"verification_uri_complete"`
		Scopes          []string `json:"scopes"`
	}
	if startedResponse.Code != http.StatusCreated || json.Unmarshal(startedResponse.Body.Bytes(), &started) != nil || started.DeviceCode == "" || started.UserCode == "" || len(started.Scopes) != len(pluginauth.Scopes()) || started.VerificationURI != "https://fern.example.ts.net"+pluginAuthorizePath || !strings.HasPrefix(started.CompleteURI, started.VerificationURI+"?") {
		t.Fatalf("start = %d %s", startedResponse.Code, startedResponse.Body.String())
	}
	rapidStart := httptest.NewRequest(http.MethodPost, pluginAuthStartPath, strings.NewReader(`{}`))
	rapidStart.Header.Set("Content-Type", "application/json")
	rapidStartResponse := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(rapidStartResponse, rapidStart)
	if rapidStartResponse.Code != http.StatusTooManyRequests || rapidStartResponse.Header().Get("Retry-After") != "1" {
		t.Fatalf("rapid start = %d retry=%q", rapidStartResponse.Code, rapidStartResponse.Header().Get("Retry-After"))
	}

	approvePath := "/fern/api/plugin-auth/requests/" + started.AuthorizationID + "/approve"
	approveBody := `{"user_code":"` + started.UserCode + `"}`
	crossOrigin := httptest.NewRequest(http.MethodPost, approvePath, strings.NewReader(approveBody))
	crossOrigin.Header.Set("Content-Type", "application/json")
	crossOrigin.Header.Set("Origin", "http://evil.example")
	crossOrigin.SetBasicAuth("fern", "control-secret")
	crossOriginResponse := httptest.NewRecorder()
	handlers.Operator.ServeHTTP(crossOriginResponse, crossOrigin)
	if crossOriginResponse.Code != http.StatusForbidden {
		t.Fatalf("cross-origin approval = %d", crossOriginResponse.Code)
	}

	approve := httptest.NewRequest(http.MethodPost, approvePath, strings.NewReader(approveBody))
	approve.Header.Set("Content-Type", "application/json")
	approve.Header.Set("Origin", "http://127.0.0.1:8081")
	approve.SetBasicAuth("fern", "control-secret")
	approved := httptest.NewRecorder()
	handlers.Operator.ServeHTTP(approved, approve)
	if approved.Code != http.StatusNoContent {
		t.Fatalf("approval = %d %s", approved.Code, approved.Body.String())
	}

	poll := httptest.NewRequest(http.MethodPost, pluginAuthPollPath, strings.NewReader(`{"device_code":"`+started.DeviceCode+`"}`))
	poll.Header.Set("Content-Type", "application/json")
	polled := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(polled, poll)
	var token struct {
		AccessToken  string `json:"access_token"`
		CredentialID string `json:"credential_id"`
	}
	if polled.Code != http.StatusOK || json.Unmarshal(polled.Body.Bytes(), &token) != nil || token.AccessToken != started.DeviceCode || token.CredentialID == "" {
		t.Fatalf("approved poll = %d %s", polled.Code, polled.Body.String())
	}
	rapidPoll := httptest.NewRequest(http.MethodPost, pluginAuthPollPath, strings.NewReader(`{"device_code":"`+started.DeviceCode+`"}`))
	rapidPoll.Header.Set("Content-Type", "application/json")
	rapidPollResponse := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(rapidPollResponse, rapidPoll)
	if rapidPollResponse.Code != http.StatusTooManyRequests || rapidPollResponse.Header().Get("Retry-After") != "5" {
		t.Fatalf("rapid poll = %d retry=%q", rapidPollResponse.Code, rapidPollResponse.Header().Get("Retry-After"))
	}

	requestRun := httptest.NewRequest(http.MethodGet, "/fern/api/runs", nil)
	requestRun.Header.Set("Authorization", "Bearer "+token.AccessToken)
	runResponse := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(runResponse, requestRun)
	if runResponse.Code != http.StatusNoContent {
		t.Fatalf("implemented run route = %d", runResponse.Code)
	}
	for _, path := range []string{"/api/health", "/fern/control"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+token.AccessToken)
		response := httptest.NewRecorder()
		handlers.Remote.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("unimplemented/restricted bearer route %s = %d", path, response.Code)
		}
	}
	unimplementedPost := httptest.NewRequest(http.MethodPost, "/fern/api/runs", strings.NewReader(`{"instruction":"not-read"}`))
	unimplementedPost.Header.Set("Authorization", "Bearer "+token.AccessToken)
	unimplementedPostResponse := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(unimplementedPostResponse, unimplementedPost)
	if unimplementedPostResponse.Code != http.StatusNoContent || runRequests.Load() != 2 {
		t.Fatalf("implemented reserved POST = %d requests=%d", unimplementedPostResponse.Code, runRequests.Load())
	}
	if waker.wakes.Load() != 0 {
		t.Fatalf("plugin auth routes woke persistent workspace %d times", waker.wakes.Load())
	}

	list := httptest.NewRequest(http.MethodGet, "/fern/api/plugin-auth/credentials", nil)
	list.SetBasicAuth("fern", "control-secret")
	listed := httptest.NewRecorder()
	handlers.Operator.ServeHTTP(listed, list)
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), started.DeviceCode) || strings.Contains(listed.Body.String(), started.UserCode) {
		t.Fatalf("credential list exposed protocol secrets: %d %s", listed.Code, listed.Body.String())
	}

	self := httptest.NewRequest(http.MethodPost, pluginAuthSelfPath, nil)
	self.Header.Set("Authorization", "Bearer "+token.AccessToken)
	revoked := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(revoked, self)
	if revoked.Code != http.StatusNoContent {
		t.Fatalf("self revoke = %d %s", revoked.Code, revoked.Body.String())
	}
	reconnect := httptest.NewRequest(http.MethodGet, "/fern/api/runs", nil)
	reconnect.Header.Set("Authorization", "Bearer "+token.AccessToken)
	rejected := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(rejected, reconnect)
	if rejected.Code != http.StatusUnauthorized {
		t.Fatalf("revoked reconnect = %d", rejected.Code)
	}
	if rejected.Header().Get("WWW-Authenticate") != `Bearer realm="fern-plugin"` {
		t.Fatalf("revoked reconnect challenge = %q", rejected.Header().Get("WWW-Authenticate"))
	}
}

func TestPairedDeviceCanApproveWithExistingCSRFBoundary(t *testing.T) {
	controlStore, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := controlStore.AddDevice("device-token", "Owner phone", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	pluginStore, err := pluginauth.Open(controlStore, "demo")
	if err != nil {
		t.Fatal(err)
	}
	started, err := pluginStore.Start(now)
	if err != nil {
		t.Fatal(err)
	}
	handlers, err := NewHandlers(nil, runtime.ServerAuth{}, Controls{Store: controlStore, PluginAuth: pluginStore}, TrustedOrigins{
		Remote: "https://fern.example.ts.net", Operator: "http://127.0.0.1:8081",
	}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	path := "/fern/api/plugin-auth/requests/" + started.AuthorizationID + "/approve"
	newRequest := func() *http.Request {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"user_code":"`+started.UserCode+`"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "https://fern.example.ts.net")
		request.Header.Set(csrfHeaderName, mintCSRFToken("device-token", http.MethodPost, path, now.Add(time.Minute)))
		request.AddCookie(&http.Cookie{Name: deviceCookieName, Value: "device-token"})
		return request
	}
	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	canceledRequest := newRequest().WithContext(canceledContext)
	canceledResponse := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(canceledResponse, canceledRequest)
	if credentials, err := pluginStore.Credentials(now.Add(time.Second)); err != nil || len(credentials) != 0 {
		t.Fatalf("canceled paired approval activated credentials: %+v, %v", credentials, err)
	}
	request := newRequest()
	response := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("paired approval = %d %s", response.Code, response.Body.String())
	}
	if result, err := pluginStore.Poll(started.DeviceCode, now.Add(5*time.Second)); err != nil || result.State != pluginauth.PollApproved {
		t.Fatalf("paired approval poll = %+v, %v", result, err)
	}
}

func TestPairedVerificationPageUsesTrustedOriginAndSafeHTML(t *testing.T) {
	controlStore, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := controlStore.AddDevice("device-token", "Owner phone", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	pluginStore, err := pluginauth.Open(controlStore, "demo")
	if err != nil {
		t.Fatal(err)
	}
	handlers, err := NewHandlers(nil, runtime.ServerAuth{}, Controls{Store: controlStore, PluginAuth: pluginStore}, TrustedOrigins{
		Remote: "https://fern.example.ts.net", Operator: "http://127.0.0.1:8081",
	}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	start := httptest.NewRequest(http.MethodPost, pluginAuthStartPath, strings.NewReader(`{}`))
	start.Header.Set("Content-Type", "application/json")
	start.Host = "attacker.example"
	startedResponse := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(startedResponse, start)
	var started struct {
		DeviceCode  string `json:"device_code"`
		UserCode    string `json:"user_code"`
		CompleteURI string `json:"verification_uri_complete"`
	}
	if startedResponse.Code != http.StatusCreated || json.Unmarshal(startedResponse.Body.Bytes(), &started) != nil {
		t.Fatalf("start = %d %s", startedResponse.Code, startedResponse.Body.String())
	}
	if !strings.HasPrefix(started.CompleteURI, "https://fern.example.ts.net"+pluginAuthorizePath+"?") || strings.Contains(started.CompleteURI, "attacker.example") {
		t.Fatalf("verification URI = %q", started.CompleteURI)
	}
	unauthenticated := httptest.NewRequest(http.MethodGet, started.CompleteURI, nil)
	unauthenticatedResponse := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(unauthenticatedResponse, unauthenticated)
	if unauthenticatedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated verification page = %d", unauthenticatedResponse.Code)
	}
	pageRequest := httptest.NewRequest(http.MethodGet, started.CompleteURI, nil)
	pageRequest.AddCookie(&http.Cookie{Name: deviceCookieName, Value: "device-token"})
	page := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(page, pageRequest)
	body := page.Body.String()
	if page.Code != http.StatusOK || page.Header().Get("Cache-Control") != "no-store" || page.Header().Get("Referrer-Policy") != "no-referrer" || page.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(page.Header().Get("Content-Security-Policy"), "script-src 'nonce-") {
		t.Fatalf("verification page status/headers = %d %v", page.Code, page.Header())
	}
	for _, required := range append([]string{pluginClientName, started.UserCode, csrfTokenPath, `JSON.stringify({user_code:root.dataset.code})`, "/approve", "/deny"}, pluginauth.Scopes()...) {
		if !strings.Contains(body, required) {
			t.Fatalf("verification page missing %q", required)
		}
	}
	if strings.Contains(body, started.DeviceCode) || strings.Contains(body, "access_token") {
		t.Fatal("verification page exposed device code or bearer")
	}
	invalid := httptest.NewRequest(http.MethodGet, started.CompleteURI+"&extra=1", nil)
	invalid.AddCookie(&http.Cookie{Name: deviceCookieName, Value: "device-token"})
	invalidResponse := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("non-strict verification query = %d", invalidResponse.Code)
	}
}

func TestOperatorCredentialRevokeRoute(t *testing.T) {
	store, credential, bearer, now := testPluginCredential(t)
	handler := newPluginAuthHTTP(store)
	handler.now = func() time.Time { return now }
	actor := task.ActorSnapshot{Type: task.ActorOperator, ID: "local-operator", CredentialID: "control-test", Authentication: "basic", RequestID: "revoke-test"}
	request := httptest.NewRequest(http.MethodDelete, "/fern/api/plugin-auth/credentials/"+credential.ID, nil)
	request = request.WithContext(taskapi.WithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	if !handler.serveTrusted(response, request) || response.Code != http.StatusNoContent {
		t.Fatalf("operator revoke = handled status %d", response.Code)
	}
	if _, ok, err := store.Authenticate(bearer, now.Add(time.Second)); err != nil || ok {
		t.Fatalf("operator-revoked bearer authenticated: %t, %v", ok, err)
	}
	credentials, err := store.Credentials(now.Add(time.Second))
	if err != nil || len(credentials) != 1 || credentials[0].RevokedBy == nil || credentials[0].RevokedBy.Type != task.ActorOperator {
		t.Fatalf("operator revoke attribution = %+v, %v", credentials, err)
	}
}

func TestSelfRevokeReturnsBeforeCancelingOtherRequests(t *testing.T) {
	store, _, bearer, now := testPluginCredential(t)
	handler := newPluginAuthHTTP(store)
	handler.now = func() time.Time { return now }
	started, canceled := make(chan struct{}), make(chan struct{})
	next := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		close(canceled)
	})
	remote := handler.remoteHandler(http.NotFoundHandler(), next)
	runDone := make(chan struct{})
	go func() {
		request := httptest.NewRequest(http.MethodGet, "/fern/api/runs/active", nil)
		request.Header.Set("Authorization", "Bearer "+bearer)
		remote.ServeHTTP(httptest.NewRecorder(), request)
		close(runDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("other plugin request was not registered")
	}
	self := httptest.NewRequest(http.MethodPost, pluginAuthSelfPath, nil)
	self.Header.Set("Authorization", "Bearer "+bearer)
	response := httptest.NewRecorder()
	remote.ServeHTTP(response, self)
	if response.Code != http.StatusNoContent {
		t.Fatalf("self revoke = %d %s", response.Code, response.Body.String())
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("self revoke did not cancel the other request")
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("canceled request did not finish")
	}
}

func TestAmbientAuthorizationStillUsesPublicAndPairingRoutes(t *testing.T) {
	controlStore, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	pluginStore, err := pluginauth.Open(controlStore, "demo")
	if err != nil {
		t.Fatal(err)
	}
	onboarding := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
			t.Errorf("callback received ambient credentials: %v", request.Header)
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	handlers, err := NewHandlers(nil, runtime.ServerAuth{}, Controls{Store: controlStore, PluginAuth: pluginStore, Onboarding: onboarding}, TrustedOrigins{
		Remote: "https://fern.example.ts.net", Operator: "http://127.0.0.1:8081",
	}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	callback := httptest.NewRequest(http.MethodGet, "/fern/github/app/callback?code=x&state=y", nil)
	callback.Header.Set("Authorization", "Basic ambient")
	callback.Header.Set("Cookie", "ambient=secret")
	callbackResponse := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(callbackResponse, callback)
	if callbackResponse.Code != http.StatusNoContent {
		t.Fatalf("callback with ambient Authorization = %d", callbackResponse.Code)
	}
	pair := httptest.NewRequest(http.MethodGet, "/fern/pair?code=invalid", nil)
	pair.Header.Set("Authorization", "Basic ambient")
	pairResponse := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(pairResponse, pair)
	if pairResponse.Code != http.StatusUnauthorized || pairResponse.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("pairing ambient Authorization = %d challenge=%q", pairResponse.Code, pairResponse.Header().Get("WWW-Authenticate"))
	}
	start := httptest.NewRequest(http.MethodPost, pluginAuthStartPath, strings.NewReader(`{}`))
	start.Header.Set("Content-Type", "application/json")
	start.Header.Set("Authorization", "Bearer  malformed")
	startResponse := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(startResponse, start)
	if startResponse.Code != http.StatusCreated {
		t.Fatalf("public start did not strip ambient Authorization: %d %s", startResponse.Code, startResponse.Body.String())
	}
	var started struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.Unmarshal(startResponse.Body.Bytes(), &started); err != nil || started.DeviceCode == "" {
		t.Fatalf("decode public start: %v %s", err, startResponse.Body.String())
	}
	poll := httptest.NewRequest(http.MethodPost, pluginAuthPollPath, strings.NewReader(`{"device_code":"`+started.DeviceCode+`"}`))
	poll.Header.Set("Content-Type", "application/json")
	poll.Header.Set("Authorization", "Basic ambient")
	pollResponse := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(pollResponse, poll)
	if pollResponse.Code != http.StatusAccepted || pollResponse.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("public poll did not strip ambient Authorization: %d %q", pollResponse.Code, pollResponse.Header().Get("WWW-Authenticate"))
	}
	ordinary := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	ordinary.Header.Set("Authorization", "Basic ambient")
	ordinaryResponse := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(ordinaryResponse, ordinary)
	if ordinaryResponse.Code != http.StatusUnauthorized || ordinaryResponse.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("ordinary Basic was classified as plugin bearer: %d %q", ordinaryResponse.Code, ordinaryResponse.Header().Get("WWW-Authenticate"))
	}
}

func TestPluginJSONIsBoundedAndStrict(t *testing.T) {
	for _, payload := range []string{
		`{"unexpected":true}`,
		`{"device_code":"one","DEVICE_CODE":"two"}`,
		`{} trailing`,
		`{"device_code":"` + strings.Repeat("x", maxPluginAuthBody) + `"}`,
	} {
		store, _, _, _ := testPluginCredential(t)
		handler := newPluginAuthHTTP(store).remoteHandler(http.NotFoundHandler(), http.NotFoundHandler())
		request := httptest.NewRequest(http.MethodPost, pluginAuthPollPath, strings.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("payload status = %d for %.80q", response.Code, payload)
		}
	}
}

func testPluginCredential(t *testing.T) (*pluginauth.Store, pluginauth.Credential, string, time.Time) {
	t.Helper()
	controlStore, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	store, err := pluginauth.Open(controlStore, "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	started, err := store.Start(now.Add(-2 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	actor := task.ActorSnapshot{Type: task.ActorOperator, ID: "local-operator", CredentialID: "control-test", Authentication: "basic", RequestID: "request-test"}
	credential, err := store.Approve(started.AuthorizationID, started.UserCode, actor, now.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return store, credential, started.DeviceCode, now
}
