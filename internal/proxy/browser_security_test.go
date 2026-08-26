package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/runtime"
)

func TestDeviceCSRFTokensAreCredentialMethodRouteAndExpiryBound(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	credential := "device-a"
	valid := mintCSRFToken(credential, http.MethodPost, "/api/mutate", now.Add(time.Minute))
	tests := []struct {
		name       string
		token      string
		credential string
		method     string
		path       string
		want       bool
	}{
		{name: "valid", token: valid, credential: credential, method: http.MethodPost, path: "/api/mutate", want: true},
		{name: "missing", credential: credential, method: http.MethodPost, path: "/api/mutate"},
		{name: "malformed", token: "not-a-token", credential: credential, method: http.MethodPost, path: "/api/mutate"},
		{name: "cross-device", token: valid, credential: "device-b", method: http.MethodPost, path: "/api/mutate"},
		{name: "wrong method", token: valid, credential: credential, method: http.MethodDelete, path: "/api/mutate"},
		{name: "wrong path", token: valid, credential: credential, method: http.MethodPost, path: "/api/other"},
		{name: "expired", token: mintCSRFToken(credential, http.MethodPost, "/api/mutate", now), credential: credential, method: http.MethodPost, path: "/api/mutate"},
		{name: "unbounded future", token: mintCSRFToken(credential, http.MethodPost, "/api/mutate", now.Add(csrfTokenTTL+time.Minute)), credential: credential, method: http.MethodPost, path: "/api/mutate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validCSRFToken(test.token, test.credential, test.method, test.path, now); got != test.want {
				t.Fatalf("validCSRFToken() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestDeviceMutationCSRFRejectsBeforeHandler(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	state := newPairingState()
	state.now = func() time.Time { return now }
	credential := "device-token"
	state.sessions[sha256.Sum256([]byte(credential))] = now.Add(time.Hour)
	var handled atomic.Int32
	handler := state.remoteHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		handled.Add(1)
	}), runtime.ServerAuth{})

	for _, test := range []struct {
		name   string
		token  string
		origin string
		site   string
		want   int
	}{
		{name: "missing", want: http.StatusForbidden},
		{name: "malformed", token: "bad", want: http.StatusForbidden},
		{name: "cross origin", token: mintCSRFToken(credential, http.MethodPost, "/api/mutate", now.Add(time.Minute)), origin: "https://evil.example", site: "cross-site", want: http.StatusForbidden},
		{name: "valid", token: mintCSRFToken(credential, http.MethodPost, "/api/mutate", now.Add(time.Minute)), want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/mutate", nil)
			request.AddCookie(&http.Cookie{Name: deviceCookieName, Value: credential})
			request.Header.Set(csrfHeaderName, test.token)
			request.Header.Set("Origin", test.origin)
			request.Header.Set("Sec-Fetch-Site", test.site)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
	if handled.Load() != 1 {
		t.Fatalf("downstream handled %d requests, want only the valid mutation", handled.Load())
	}
}

func TestTraceRequiresCSRFAndTokenEndpointRejectsAmbiguousQuery(t *testing.T) {
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	state := newPairingState()
	state.now = func() time.Time { return now }
	credential := "device-token"
	state.sessions[sha256.Sum256([]byte(credential))] = now.Add(time.Hour)
	handler := state.remoteHandler(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}), runtime.ServerAuth{})

	trace := httptest.NewRequest(http.MethodTrace, "/api/mutate", nil)
	trace.AddCookie(&http.Cookie{Name: deviceCookieName, Value: credential})
	traceResponse := httptest.NewRecorder()
	handler.ServeHTTP(traceResponse, trace)
	if traceResponse.Code != http.StatusForbidden {
		t.Fatalf("TRACE without CSRF status=%d", traceResponse.Code)
	}

	for _, query := range []string{
		"method=POST&method=DELETE&path=%2Ffern%2Fapi%2Fv1%2Ftasks",
		"method=POST&path=%2Ffern%2Fapi%2Fv1%2Ftasks&extra=1",
	} {
		request := httptest.NewRequest(http.MethodGet, csrfTokenPath+"?"+query, nil)
		request.AddCookie(&http.Cookie{Name: deviceCookieName, Value: credential})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("ambiguous token query %q status=%d", query, response.Code)
		}
	}
}

func TestExplicitBasicMutationIsCSRFExempt(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		handler  http.Handler
		username string
		password string
		path     string
	}{
		{name: "non-browser upstream", username: "opencode", password: "secret", path: "/api/mutate"},
		{name: "loopback operator", username: "fern", password: "control", path: "/fern/api/v1/mutate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				called = true
				writer.WriteHeader(http.StatusNoContent)
			})
			if test.username == "fern" {
				test.handler = newPairingState().operatorHandler(next, runtime.ServerAuth{Password: "secret"}, ControlAuth{Password: "control"})
			} else {
				test.handler = newPairingState().handler(next, runtime.ServerAuth{Password: "secret"}, ControlAuth{})
			}
			request := httptest.NewRequest(http.MethodPost, test.path, nil)
			request.SetBasicAuth(test.username, test.password)
			response := httptest.NewRecorder()
			test.handler.ServeHTTP(response, request)
			if response.Code != http.StatusNoContent || !called {
				t.Fatalf("Basic mutation status=%d called=%t", response.Code, called)
			}
		})
	}
}

func TestTaskPageContainsTokenAndScriptSendsHeader(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/fern/tasks", nil)
	request = request.WithContext(context.WithValue(request.Context(), csrfCredentialKey{}, "device-token"))
	response := httptest.NewRecorder()
	serveFern(response, request, Controls{Tasks: http.NotFoundHandler()}, false)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `name="_csrf" id="csrf" value=""`) || !strings.Contains(response.Body.String(), `name="_csrf"`) {
		t.Fatalf("task form token status=%d body=%q", response.Code, response.Body.String())
	}
	if !strings.Contains(durableTaskUIJS, "X-Fern-CSRF-Token") || !strings.Contains(durableTaskUIJS, csrfTokenPath) {
		t.Fatal("task JavaScript does not refresh and send the CSRF header")
	}
	if !strings.Contains(durableTaskUIJS, "fern.pending-task-submission.v1") ||
		!strings.Contains(durableTaskUIJS, "localStorage.setItem") || !strings.Contains(durableTaskUIJS, "localStorage.removeItem") ||
		!strings.Contains(durableTaskUIJS, "/fern/api/v1/tasks?limit=100") || !strings.Contains(durableTaskUIJS, "/seal-preview") {
		t.Fatal("task JavaScript does not use durable server listing and seal controls")
	}
}

func TestPairingLimiterPersistsCodesCadenceAndOutstandingCap(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	directory := filepath.Join(t.TempDir(), "control")
	store, err := control.Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	state := newPairingState(store)
	state.now = func() time.Time { return now }
	issued := httptest.NewRecorder()
	state.issue(issued, httptest.NewRequest(http.MethodPost, "/fern/pair/new", nil))
	var result struct {
		Code string `json:"code"`
	}
	if issued.Code != http.StatusOK || json.Unmarshal(issued.Body.Bytes(), &result) != nil || result.Code == "" {
		t.Fatalf("issue=%d %s", issued.Code, issued.Body.String())
	}

	reopened, err := control.Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	restarted := newPairingState(reopened)
	restarted.now = func() time.Time { return now }
	limited := httptest.NewRecorder()
	restarted.issue(limited, httptest.NewRequest(http.MethodPost, "/fern/pair/new", nil))
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("restart issuance cadence status=%d", limited.Code)
	}
	paired := httptest.NewRecorder()
	restarted.pair(paired, pairingRequest(result.Code))
	if paired.Code != http.StatusSeeOther {
		t.Fatalf("persisted code status=%d body=%q", paired.Code, paired.Body.String())
	}

	now = now.Add(pairingSuccessInterval)
	for index := range maxOutstandingPairings {
		restarted.codes[sha256.Sum256([]byte("outstanding-"+strconv.Itoa(index)))] = now.Add(pairingCodeTTL)
	}
	restarted.mu.Lock()
	if err := restarted.persistLocked(); err != nil {
		restarted.mu.Unlock()
		t.Fatal(err)
	}
	restarted.mu.Unlock()
	reopenedAgain, err := control.Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	capped := newPairingState(reopenedAgain)
	capped.now = func() time.Time { return now }
	response := httptest.NewRecorder()
	capped.issue(response, httptest.NewRequest(http.MethodPost, "/fern/pair/new", nil))
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("restart outstanding cap status=%d codes=%d", response.Code, len(capped.codes))
	}
}

func TestSuccessfulPairingCadenceAndInvalidLimitSurviveRestart(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	directory := filepath.Join(t.TempDir(), "control")
	store, err := control.Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	state := newPairingState(store)
	state.now = func() time.Time { return now }
	firstCode, secondCode, protectedCode := "first", "second", "protected"
	state.mu.Lock()
	for _, code := range []string{firstCode, secondCode, protectedCode} {
		state.codes[sha256.Sum256([]byte(code))] = now.Add(pairingCodeTTL)
	}
	if err := state.persistLocked(); err != nil {
		state.mu.Unlock()
		t.Fatal(err)
	}
	state.mu.Unlock()
	first := httptest.NewRecorder()
	state.pair(first, pairingRequest(firstCode))
	if first.Code != http.StatusSeeOther {
		t.Fatalf("first pairing status=%d", first.Code)
	}

	reopened, err := control.Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	restarted := newPairingState(reopened)
	restarted.now = func() time.Time { return now }
	cadenceBlocked := httptest.NewRecorder()
	restarted.pair(cadenceBlocked, pairingRequest(secondCode))
	if cadenceBlocked.Code != http.StatusTooManyRequests {
		t.Fatalf("restart success cadence status=%d", cadenceBlocked.Code)
	}
	now = now.Add(pairingSuccessInterval)
	second := httptest.NewRecorder()
	restarted.pair(second, pairingRequest(secondCode))
	if second.Code != http.StatusSeeOther {
		t.Fatalf("pairing after cadence status=%d", second.Code)
	}

	for index := 0; index < maxGlobalPairingFailures; index++ {
		response := httptest.NewRecorder()
		request := pairingRequest("invalid-" + strconv.Itoa(index))
		request.Header.Set("Forwarded", "for=192.0.2."+strconv.Itoa(index))
		request.Header.Set("X-Forwarded-For", "198.51.100."+strconv.Itoa(index))
		restarted.pair(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("invalid attempt %d status=%d", index, response.Code)
		}
	}
	reopenedAgain, err := control.Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	limited := newPairingState(reopenedAgain)
	limited.now = func() time.Time { return now }
	protected := httptest.NewRecorder()
	limited.pair(protected, pairingRequest(protectedCode))
	if protected.Code != http.StatusTooManyRequests {
		t.Fatalf("restart global invalid limit status=%d", protected.Code)
	}
}

func TestPairingAttemptsAreConcurrentOracleFreeAndDoNotPoisonOtherCodes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	unknown := newPairingState()
	unknown.now = func() time.Time { return now }
	unknownResponse := httptest.NewRecorder()
	unknown.pair(unknownResponse, pairingRequest("unknown"))
	expired := newPairingState()
	expired.now = func() time.Time { return now }
	expired.codes[sha256.Sum256([]byte("expired"))] = now
	expiredResponse := httptest.NewRecorder()
	expired.pair(expiredResponse, pairingRequest("expired"))
	if unknownResponse.Code != expiredResponse.Code || unknownResponse.Body.String() != expiredResponse.Body.String() {
		t.Fatalf("unknown=%d %q expired=%d %q", unknownResponse.Code, unknownResponse.Body.String(), expiredResponse.Code, expiredResponse.Body.String())
	}

	state := newPairingState()
	state.now = func() time.Time { return now }
	validCode := "still-valid"
	state.codes[sha256.Sum256([]byte(validCode))] = now.Add(pairingCodeTTL)
	for index := 0; index < 8; index++ {
		response := httptest.NewRecorder()
		state.pair(response, pairingRequest("unrelated-"+strconv.Itoa(index)))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unrelated attempt %d status=%d", index, response.Code)
		}
	}
	validResponse := httptest.NewRecorder()
	state.pair(validResponse, pairingRequest(validCode))
	if validResponse.Code != http.StatusSeeOther {
		t.Fatalf("unrelated abuse poisoned valid code: %d %s", validResponse.Code, validResponse.Body.String())
	}

	attemptLimited := newPairingState()
	attemptLimited.now = func() time.Time { return now }
	attemptCode := "attempt-limited"
	attemptLimited.codes[sha256.Sum256([]byte(attemptCode))] = now.Add(pairingCodeTTL)
	for range maxPairingCodeAttempts {
		response := httptest.NewRecorder()
		attemptLimited.pair(response, pairingRequestWithName(attemptCode, strings.Repeat("x", maxDeviceNameBytes+1)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid code detail status=%d", response.Code)
		}
	}
	blocked := httptest.NewRecorder()
	attemptLimited.pair(blocked, pairingRequest(attemptCode))
	if blocked.Code != http.StatusUnauthorized {
		t.Fatalf("per-code digest limit status=%d", blocked.Code)
	}

	concurrent := newPairingState()
	concurrent.now = func() time.Time { return now }
	code := "one-use"
	concurrent.codes[sha256.Sum256([]byte(code))] = now.Add(pairingCodeTTL)
	var successes atomic.Int32
	var wait sync.WaitGroup
	for range 24 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			response := httptest.NewRecorder()
			concurrent.pair(response, pairingRequest(code))
			if response.Code == http.StatusSeeOther {
				successes.Add(1)
			}
		}()
	}
	wait.Wait()
	if successes.Load() != 1 {
		t.Fatalf("concurrent pairing successes=%d", successes.Load())
	}
}
