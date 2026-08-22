package githubapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const testOnboardingOrigin = "https://fern.example"

func TestOnboardingHTTPSeparatesLoopbackSetupFromHTTPSCallbackAuthority(t *testing.T) {
	store, _ := newTestOnboardingStateStore(t)
	handler, err := NewOnboardingHTTPWithSetupOrigin(
		testOnboardingOrigin, "http://127.0.0.1:8081", "Fern Test App", store,
		&fakeManifestExchanger{}, &fakeCredentialPersistence{}, testOnboardingRandom(1, 2, 3), testOnboardingTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	request := newOnboardingRequest(http.MethodGet, GitHubAppSetupPath+"?return=%2Ffern%2Fcontrol", nil)
	request.Host = "127.0.0.1:8081"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("loopback setup = %d %s", response.Code, response.Body.String())
	}
	request = newOnboardingRequest(http.MethodGet, GitHubAppCallbackPath+"?code=x&state=y", nil)
	request.Host = "127.0.0.1:8081"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("loopback callback = %d", response.Code)
	}
	request = newOnboardingRequest(http.MethodGet, GitHubAppCallbackPath+"?code=manifest-code&state="+testHTTPRandomValue(1), nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "http://127.0.0.1:8081/fern/control" {
		t.Fatalf("callback redirect = %d %q", response.Code, response.Header().Get("Location"))
	}
}

func TestOnboardingHTTPSetupPersistsBeforeRenderingBoundManifest(t *testing.T) {
	store, directory := newTestOnboardingStateStore(t)
	events := &onboardingHTTPEvents{}
	states := &recordingOnboardingStates{delegate: store, events: events}
	handler := newTestOnboardingHTTP(t, states, &fakeManifestExchanger{}, &fakeCredentialPersistence{}, testOnboardingRandom(1, 2, 3))

	response := serveOnboarding(handler, http.MethodGet, GitHubAppSetupPath+"?return="+url.QueryEscape("/fern/control?connected=1"), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	assertOnboardingHeaders(t, response.Header())
	if response.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", response.Header().Get("Content-Type"))
	}

	state := testHTTPRandomValue(1)
	flowID := testHTTPRandomValue(2)
	body := response.Body.String()
	if !strings.Contains(body, `method="post"`) || !strings.Contains(body, `https://github.com/settings/apps/new?state=`+state) || !strings.Contains(body, `.submit()`) {
		t.Fatalf("setup page did not contain the expected auto-submit form: %s", body)
	}
	if strings.Contains(body, flowID) || strings.Contains(body, "/fern/control?connected=1") {
		t.Fatal("setup page exposed its private flow binding")
	}
	match := regexp.MustCompile(`name="manifest" value="([^"]+)"`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatal("manifest input not found")
	}
	manifestText := html.UnescapeString(match[1])
	var manifest struct {
		Name        string `json:"name"`
		URL         string `json:"url"`
		RedirectURL string `json:"redirect_url"`
		Public      bool   `json:"public"`
	}
	if err := json.Unmarshal([]byte(manifestText), &manifest); err != nil {
		t.Fatalf("manifest = %q: %v", manifestText, err)
	}
	if manifest.Name != "Fern Test App" || manifest.URL != testOnboardingOrigin || manifest.RedirectURL != testOnboardingOrigin+GitHubAppCallbackPath || manifest.Public {
		t.Fatalf("manifest = %#v", manifest)
	}

	entries := readTestOnboardingEntries(t, directory)
	if len(entries) != 1 || entries[0].status != onboardingStateStatusPending || entries[0].flowID != flowID || entries[0].returnPath != "/fern/control?connected=1" || !entries[0].issuedAt.Equal(testOnboardingTime()) || !entries[0].expiresAt.Equal(testOnboardingTime().Add(onboardingHTTPStateLifetime)) {
		t.Fatalf("stored flow = %#v", entries)
	}
	if strings.Contains(string(readTestOnboardingPayload(t, directory)), state) {
		t.Fatal("state store persisted the raw callback state")
	}
	if got := events.snapshot(); strings.Join(got, ",") != "begin" {
		t.Fatalf("events = %v", got)
	}
}

func TestOnboardingHTTPCallbackClaimsExchangesSavesCompletesAndRedirects(t *testing.T) {
	store, directory := newTestOnboardingStateStore(t)
	events := &onboardingHTTPEvents{}
	credentials := testStoredCredentials(t, 701, "onboarding-http")
	exchanger := &fakeManifestExchanger{credentials: credentials, events: events}
	saver := &fakeCredentialPersistence{events: events}
	states := &recordingOnboardingStates{delegate: store, events: events}
	handler := newTestOnboardingHTTP(t, states, exchanger, saver, testOnboardingRandom(10, 11, 12))
	setup := serveOnboarding(handler, http.MethodGet, GitHubAppSetupPath+"?return="+url.QueryEscape("/fern/control?connected=1"), nil)
	if setup.Code != http.StatusOK {
		t.Fatal(setup.Body.String())
	}

	state := testHTTPRandomValue(10)
	code := "manifest-code-secret"
	callback := GitHubAppCallbackPath + "?code=" + code + "&state=" + state
	response := serveOnboarding(handler, http.MethodGet, callback, nil)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != testOnboardingOrigin+"/fern/control?connected=1" {
		t.Fatalf("status = %d, location = %q, body = %q", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	assertOnboardingHeaders(t, response.Header())
	if exchanger.calls() != 1 || exchanger.code() != code || !exchanger.sawDeadline() {
		t.Fatalf("exchange calls = %d, code = %q, deadline = %t", exchanger.calls(), exchanger.code(), exchanger.sawDeadline())
	}
	if saver.calls() != 1 || saver.saved().AppID() != credentials.AppID() {
		t.Fatalf("save calls = %d, credentials = %v", saver.calls(), saver.saved())
	}
	if got := events.snapshot(); strings.Join(got, ",") != "begin,claim,exchange,save,complete" {
		t.Fatalf("events = %v", got)
	}
	entry := readTestOnboardingEntries(t, directory)[0]
	if entry.status != onboardingStateStatusCompleted || entry.codeHash != sha256.Sum256([]byte(code)) || entry.claimHash != sha256.Sum256([]byte(testHTTPRandomValue(12))) {
		t.Fatalf("completed entry = %#v", entry)
	}
	for _, secret := range []string{code, testHTTPRandomValue(12), credentials.ClientSecret(), credentials.WebhookSecret(), "PRIVATE KEY"} {
		if strings.Contains(response.Body.String(), secret) || strings.Contains(response.Header().Get("Location"), secret) {
			t.Fatalf("callback response exposed %q", secret)
		}
	}

	replay := serveOnboarding(handler, http.MethodGet, callback, nil)
	if replay.Code != http.StatusConflict || exchanger.calls() != 1 || saver.calls() != 1 {
		t.Fatalf("replay status = %d, exchanges = %d, saves = %d", replay.Code, exchanger.calls(), saver.calls())
	}
}

func TestOnboardingHTTPReconcileOnlyQuarantinesWithoutEffects(t *testing.T) {
	store, directory := newTestOnboardingStateStore(t)
	events := &onboardingHTTPEvents{}
	exchanger := &fakeManifestExchanger{events: events}
	saver := &fakeCredentialPersistence{events: events}
	states := &recordingOnboardingStates{delegate: store, events: events}
	handler := newTestOnboardingHTTP(t, states, exchanger, saver, testOnboardingRandom(20, 21, 22))
	if response := serveOnboarding(handler, http.MethodGet, GitHubAppSetupPath+"?return=%2Ffern%2Fcontrol", nil); response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	state := testHTTPRandomValue(20)
	flow := handler.flows[state]
	flow.claimID = testHTTPRandomValue(22)
	code := "reconcile-code-secret"
	if _, err := states.Claim(context.Background(), state, flow.binding, sha256.Sum256([]byte(code)), flow.claimID, testOnboardingTime()); err != nil {
		t.Fatal(err)
	}

	response := serveOnboarding(handler, http.MethodGet, GitHubAppCallbackPath+"?code="+code+"&state="+state, nil)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "recovery is required") {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if exchanger.calls() != 0 || saver.calls() != 0 {
		t.Fatalf("reconcile performed effects: exchange = %d, save = %d", exchanger.calls(), saver.calls())
	}
	entry := readTestOnboardingEntries(t, directory)[0]
	if entry.status != onboardingStateStatusQuarantined || entry.quarantineReason != string(CallbackQuarantineReconcileAmbiguous) {
		t.Fatalf("entry = %#v", entry)
	}
	if got := events.snapshot(); strings.Join(got, ",") != "begin,claim,claim,quarantine:reconcile_ambiguous" {
		t.Fatalf("events = %v", got)
	}
}

func TestOnboardingHTTPFailuresAfterClaimAlwaysFailClosed(t *testing.T) {
	tests := []struct {
		name             string
		exchangeErr      error
		saveErr          error
		completeErr      error
		wantReason       CallbackQuarantineReason
		wantEvents       string
		wantExchangeCall int
		wantSaveCall     int
	}{
		{name: "exchange", exchangeErr: errors.New("remote secret must not escape"), wantReason: CallbackQuarantineExchangeAmbiguous, wantEvents: "begin,claim,exchange,quarantine:exchange_ambiguous", wantExchangeCall: 1},
		{name: "save", saveErr: errors.New("disk secret must not escape"), wantReason: CallbackQuarantineCoordinatorAborted, wantEvents: "begin,claim,exchange,save,quarantine:coordinator_aborted", wantExchangeCall: 1, wantSaveCall: 1},
		{name: "complete", completeErr: errors.New("state secret must not escape"), wantReason: CallbackQuarantineCoordinatorAborted, wantEvents: "begin,claim,exchange,save,complete,quarantine:coordinator_aborted", wantExchangeCall: 1, wantSaveCall: 1},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, directory := newTestOnboardingStateStore(t)
			events := &onboardingHTTPEvents{}
			states := &recordingOnboardingStates{delegate: store, events: events, completeErr: test.completeErr}
			exchanger := &fakeManifestExchanger{credentials: testStoredCredentials(t, int64(800+index), test.name), err: test.exchangeErr, events: events}
			saver := &fakeCredentialPersistence{err: test.saveErr, events: events}
			handler := newTestOnboardingHTTP(t, states, exchanger, saver, testOnboardingRandom(byte(30+index*3), byte(31+index*3), byte(32+index*3)))
			if response := serveOnboarding(handler, http.MethodGet, GitHubAppSetupPath+"?return=%2Ffern%2Fcontrol", nil); response.Code != http.StatusOK {
				t.Fatal(response.Body.String())
			}
			state := testHTTPRandomValue(byte(30 + index*3))
			code := "failure-code-secret"
			response := serveOnboarding(handler, http.MethodGet, GitHubAppCallbackPath+"?code="+code+"&state="+state, nil)
			if response.Code != http.StatusConflict || response.Header().Get("Location") != "" || strings.Contains(response.Body.String(), code) || strings.Contains(response.Body.String(), "secret must not escape") {
				t.Fatalf("status = %d, location = %q, body = %q", response.Code, response.Header().Get("Location"), response.Body.String())
			}
			if exchanger.calls() != test.wantExchangeCall || saver.calls() != test.wantSaveCall {
				t.Fatalf("exchange = %d, save = %d", exchanger.calls(), saver.calls())
			}
			entry := readTestOnboardingEntries(t, directory)[0]
			if entry.status != onboardingStateStatusQuarantined || entry.quarantineReason != string(test.wantReason) {
				t.Fatalf("entry = %#v", entry)
			}
			if got := events.snapshot(); strings.Join(got, ",") != test.wantEvents {
				t.Fatalf("events = %v", got)
			}
		})
	}
}

func TestOnboardingHTTPPreEffectFailuresDoNotExchange(t *testing.T) {
	t.Run("invalid clock", func(t *testing.T) {
		store, _ := newTestOnboardingStateStore(t)
		events := &onboardingHTTPEvents{}
		exchanger := &fakeManifestExchanger{}
		handler := newTestOnboardingHTTPWithClock(t, &recordingOnboardingStates{delegate: store, events: events}, exchanger, &fakeCredentialPersistence{}, testOnboardingRandom(40, 41, 42), func() time.Time { return time.Time{} })
		response := serveOnboarding(handler, http.MethodGet, GitHubAppSetupPath+"?return=%2Ffern%2Fcontrol", nil)
		if response.Code != http.StatusServiceUnavailable || len(events.snapshot()) != 0 || exchanger.calls() != 0 {
			t.Fatalf("status = %d, events = %v, exchanges = %d", response.Code, events.snapshot(), exchanger.calls())
		}
	})

	t.Run("begin failure", func(t *testing.T) {
		store, _ := newTestOnboardingStateStore(t)
		events := &onboardingHTTPEvents{}
		exchanger := &fakeManifestExchanger{}
		states := &recordingOnboardingStates{delegate: store, events: events, beginErr: ErrOnboardingStateStoreIO}
		handler := newTestOnboardingHTTP(t, states, exchanger, &fakeCredentialPersistence{}, testOnboardingRandom(43, 44, 45))
		response := serveOnboarding(handler, http.MethodGet, GitHubAppSetupPath+"?return=%2Ffern%2Fcontrol", nil)
		if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "manifest") || exchanger.calls() != 0 {
			t.Fatalf("status = %d, body = %q, exchanges = %d", response.Code, response.Body.String(), exchanger.calls())
		}
	})

	for _, test := range []struct {
		name       string
		claimErr   error
		wantStatus int
	}{
		{name: "claim rejected", claimErr: ErrOnboardingStateRejected, wantStatus: http.StatusBadRequest},
		{name: "claim unavailable", claimErr: ErrOnboardingStateStoreIO, wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _ := newTestOnboardingStateStore(t)
			events := &onboardingHTTPEvents{}
			exchanger := &fakeManifestExchanger{}
			states := &recordingOnboardingStates{delegate: store, events: events, claimErr: test.claimErr}
			handler := newTestOnboardingHTTP(t, states, exchanger, &fakeCredentialPersistence{}, testOnboardingRandom(46, 47, 48))
			if setup := serveOnboarding(handler, http.MethodGet, GitHubAppSetupPath+"?return=%2Ffern%2Fcontrol", nil); setup.Code != http.StatusOK {
				t.Fatal(setup.Body.String())
			}
			response := serveOnboarding(handler, http.MethodGet, GitHubAppCallbackPath+"?code=never-exchange&state="+testHTTPRandomValue(46), nil)
			if response.Code != test.wantStatus || exchanger.calls() != 0 || strings.Contains(response.Body.String(), "never-exchange") {
				t.Fatalf("status = %d, exchanges = %d, body = %q", response.Code, exchanger.calls(), response.Body.String())
			}
		})
	}

	t.Run("claim ID random failure", func(t *testing.T) {
		store, directory := newTestOnboardingStateStore(t)
		events := &onboardingHTTPEvents{}
		exchanger := &fakeManifestExchanger{}
		handler := newTestOnboardingHTTP(t, &recordingOnboardingStates{delegate: store, events: events}, exchanger, &fakeCredentialPersistence{}, testOnboardingRandom(49, 50))
		if setup := serveOnboarding(handler, http.MethodGet, GitHubAppSetupPath+"?return=%2Ffern%2Fcontrol", nil); setup.Code != http.StatusOK {
			t.Fatal(setup.Body.String())
		}
		response := serveOnboarding(handler, http.MethodGet, GitHubAppCallbackPath+"?code=never-exchange&state="+testHTTPRandomValue(49), nil)
		if response.Code != http.StatusServiceUnavailable || exchanger.calls() != 0 {
			t.Fatalf("status = %d, exchanges = %d", response.Code, exchanger.calls())
		}
		if entry := readTestOnboardingEntries(t, directory)[0]; entry.status != onboardingStateStatusPending {
			t.Fatalf("entry = %#v", entry)
		}
	})
}

func TestOnboardingHTTPStrictRequestSurface(t *testing.T) {
	store, _ := newTestOnboardingStateStore(t)
	events := &onboardingHTTPEvents{}
	handler := newTestOnboardingHTTP(t, &recordingOnboardingStates{delegate: store, events: events}, &fakeManifestExchanger{}, &fakeCredentialPersistence{}, testOnboardingRandom(50, 51, 52))
	oversizedReturn := "/" + strings.Repeat("a", maxOnboardingReturnPath)
	validState := testHTTPRandomValue(50)
	tests := []struct {
		name   string
		method string
		target string
		body   io.Reader
		mutate func(*http.Request)
		status int
	}{
		{name: "unknown path", method: http.MethodGet, target: "/fern/github/app/other", status: http.StatusNotFound},
		{name: "setup method", method: http.MethodPost, target: GitHubAppSetupPath + "?return=%2Ffern%2Fcontrol", status: http.StatusMethodNotAllowed},
		{name: "callback method", method: http.MethodHead, target: GitHubAppCallbackPath + "?code=x&state=" + validState, status: http.StatusMethodNotAllowed},
		{name: "escaped path", method: http.MethodGet, target: GitHubAppSetupPath + "?return=%2Ffern%2Fcontrol", mutate: func(request *http.Request) { request.URL.RawPath = "/fern/github/app/%73etup" }, status: http.StatusNotFound},
		{name: "wrong authority", method: http.MethodGet, target: GitHubAppSetupPath + "?return=%2Ffern%2Fcontrol", mutate: func(request *http.Request) { request.Host = "attacker.example" }, status: http.StatusNotFound},
		{name: "absolute request target", method: http.MethodGet, target: GitHubAppSetupPath + "?return=%2Ffern%2Fcontrol", mutate: func(request *http.Request) { request.URL.Scheme, request.URL.Host = "https", "fern.example" }, status: http.StatusNotFound},
		{name: "fragment", method: http.MethodGet, target: GitHubAppSetupPath + "?return=%2Ffern%2Fcontrol", mutate: func(request *http.Request) { request.URL.Fragment = "fragment" }, status: http.StatusNotFound},
		{name: "GET body", method: http.MethodGet, target: GitHubAppSetupPath + "?return=%2Ffern%2Fcontrol", body: strings.NewReader("body"), status: http.StatusNotFound},
		{name: "missing setup query", method: http.MethodGet, target: GitHubAppSetupPath, status: http.StatusBadRequest},
		{name: "force setup query", method: http.MethodGet, target: GitHubAppSetupPath + "?", mutate: func(request *http.Request) { request.URL.ForceQuery = true }, status: http.StatusBadRequest},
		{name: "duplicate return", method: http.MethodGet, target: GitHubAppSetupPath + "?return=%2Fone&return=%2Ftwo", status: http.StatusBadRequest},
		{name: "extra setup key", method: http.MethodGet, target: GitHubAppSetupPath + "?return=%2Fone&next=%2Ftwo", status: http.StatusBadRequest},
		{name: "absolute return", method: http.MethodGet, target: GitHubAppSetupPath + "?return=https%3A%2F%2Fattacker.example", status: http.StatusBadRequest},
		{name: "network return", method: http.MethodGet, target: GitHubAppSetupPath + "?return=%2F%2Fattacker.example", status: http.StatusBadRequest},
		{name: "return fragment", method: http.MethodGet, target: GitHubAppSetupPath + "?return=%2Ffern%2Fcontrol%23secret", status: http.StatusBadRequest},
		{name: "oversized return", method: http.MethodGet, target: GitHubAppSetupPath + "?return=" + url.QueryEscape(oversizedReturn), status: http.StatusBadRequest},
		{name: "oversized setup query", method: http.MethodGet, target: GitHubAppSetupPath + "?return=%2F" + strings.Repeat("a", maxOnboardingSetupQueryBytes), status: http.StatusBadRequest},
		{name: "missing callback state", method: http.MethodGet, target: GitHubAppCallbackPath + "?code=x", status: http.StatusBadRequest},
		{name: "missing callback code", method: http.MethodGet, target: GitHubAppCallbackPath + "?state=" + validState, status: http.StatusBadRequest},
		{name: "duplicate callback code", method: http.MethodGet, target: GitHubAppCallbackPath + "?code=x&code=y&state=" + validState, status: http.StatusBadRequest},
		{name: "duplicate callback state", method: http.MethodGet, target: GitHubAppCallbackPath + "?code=x&state=" + validState + "&state=" + validState, status: http.StatusBadRequest},
		{name: "extra callback key", method: http.MethodGet, target: GitHubAppCallbackPath + "?code=x&state=" + validState + "&return=%2F", status: http.StatusBadRequest},
		{name: "invalid code", method: http.MethodGet, target: GitHubAppCallbackPath + "?code=path%2Fsecret&state=" + validState, status: http.StatusBadRequest},
		{name: "oversized code", method: http.MethodGet, target: GitHubAppCallbackPath + "?code=" + strings.Repeat("a", maxManifestCodeBytes+1) + "&state=" + validState, status: http.StatusBadRequest},
		{name: "invalid state", method: http.MethodGet, target: GitHubAppCallbackPath + "?code=x&state=invalid", status: http.StatusBadRequest},
		{name: "oversized callback query", method: http.MethodGet, target: GitHubAppCallbackPath + "?code=x&state=" + validState + strings.Repeat("a", maxOnboardingCallbackQueryBytes), status: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := newOnboardingRequest(test.method, test.target, test.body)
			if test.mutate != nil {
				test.mutate(request)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d, body = %q", response.Code, test.status, response.Body.String())
			}
			assertOnboardingHeaders(t, response.Header())
		})
	}
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("invalid requests caused effects: %v", got)
	}
}

func TestNewOnboardingHTTPRejectsInvalidConfiguration(t *testing.T) {
	store, _ := newTestOnboardingStateStore(t)
	exchanger := &fakeManifestExchanger{}
	saver := &fakeCredentialPersistence{}
	random := testOnboardingRandom(60, 61, 62)
	now := func() time.Time { return testOnboardingTime() }
	for _, origin := range []string{"", "http://fern.example", "HTTPS://fern.example", "https://Fern.example", "https://fern.example/", "https://user@fern.example", "https://fern.example/path", "https://fern.example?query", "https://fern.example#fragment", "https://"} {
		if handler, err := NewOnboardingHTTP(origin, "Fern", store, exchanger, saver, random, now); handler != nil || !errors.Is(err, ErrInvalidOnboardingHTTPConfiguration) {
			t.Fatalf("origin %q: handler = %v, error = %v", origin, handler, err)
		}
	}
	if handler, err := NewOnboardingHTTP("https://fern.example:443", "Fern", store, exchanger, saver, random, now); handler != nil || !errors.Is(err, ErrInvalidOnboardingHTTPConfiguration) {
		t.Fatalf("default HTTPS port: handler = %v, error = %v", handler, err)
	}
	var nilStore *OnboardingStateStore
	var nilExchanger *fakeManifestExchanger
	var nilSaver *fakeCredentialPersistence
	var nilReader *bytes.Reader
	dependencies := []struct {
		states    onboardingStatePersistence
		exchanger manifestExchanger
		saver     credentialPersistence
		random    io.Reader
		now       func() time.Time
	}{
		{nil, exchanger, saver, random, now},
		{nilStore, exchanger, saver, random, now},
		{store, nil, saver, random, now},
		{store, nilExchanger, saver, random, now},
		{store, exchanger, nil, random, now},
		{store, exchanger, nilSaver, random, now},
		{store, exchanger, saver, nil, now},
		{store, exchanger, saver, nilReader, now},
		{store, exchanger, saver, random, nil},
	}
	for index, test := range dependencies {
		if handler, err := NewOnboardingHTTP(testOnboardingOrigin, "Fern", test.states, test.exchanger, test.saver, test.random, test.now); handler != nil || !errors.Is(err, ErrInvalidOnboardingHTTPConfiguration) {
			t.Fatalf("dependency %d: handler = %v, error = %v", index, handler, err)
		}
	}
	if handler, err := NewOnboardingHTTP(testOnboardingOrigin, "", store, exchanger, saver, random, now); handler != nil || !errors.Is(err, ErrInvalidOnboardingHTTPConfiguration) {
		t.Fatalf("empty app name: handler = %v, error = %v", handler, err)
	}
}

func TestOnboardingHTTPFormattingRedactsInMemorySecrets(t *testing.T) {
	store, _ := newTestOnboardingStateStore(t)
	handler := newTestOnboardingHTTP(t, store, &fakeManifestExchanger{}, &fakeCredentialPersistence{}, testOnboardingRandom(90, 91, 92))
	if setup := serveOnboarding(handler, http.MethodGet, GitHubAppSetupPath+"?return=%2Fprivate-return", nil); setup.Code != http.StatusOK {
		t.Fatal(setup.Body.String())
	}
	handler.flows[testHTTPRandomValue(90)].claimID = testHTTPRandomValue(92)
	formatted := fmt.Sprintf("%v %#v", handler, handler)
	for _, secret := range []string{testHTTPRandomValue(90), testHTTPRandomValue(91), testHTTPRandomValue(92), "/private-return"} {
		if strings.Contains(formatted, secret) {
			t.Fatalf("formatted handler exposed %q: %s", secret, formatted)
		}
	}
}

func TestOnboardingHTTPRandomFailureAndPreCallbackRestartRecovery(t *testing.T) {
	store, directory := newTestOnboardingStateStore(t)
	events := &onboardingHTTPEvents{}
	states := &recordingOnboardingStates{delegate: store, events: events}
	handler := newTestOnboardingHTTP(t, states, &fakeManifestExchanger{}, &fakeCredentialPersistence{}, bytes.NewReader(make([]byte, sha256.Size+1)))
	response := serveOnboarding(handler, http.MethodGet, GitHubAppSetupPath+"?return=%2Ffern%2Fcontrol", nil)
	if response.Code != http.StatusServiceUnavailable || len(events.snapshot()) != 0 {
		t.Fatalf("random failure status = %d, events = %v", response.Code, events.snapshot())
	}
	if _, err := os.Stat(filepath.Join(directory, onboardingStateFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state file after random failure = %v", err)
	}

	active := newTestOnboardingHTTPWithClock(t, states, &fakeManifestExchanger{}, &fakeCredentialPersistence{}, testOnboardingRandom(70, 71, 72), func() time.Time { return testOnboardingTime() })
	if setup := serveOnboarding(active, http.MethodGet, GitHubAppSetupPath+"?return=%2Ffern%2Fcontrol", nil); setup.Code != http.StatusOK {
		t.Fatal(setup.Body.String())
	}
	restartedExchanger := &fakeManifestExchanger{}
	restarted := newTestOnboardingHTTP(t, states, restartedExchanger, &fakeCredentialPersistence{}, testOnboardingRandom(73, 74, 75))
	callback := serveOnboarding(restarted, http.MethodGet, GitHubAppCallbackPath+"?code=restart-code&state="+testHTTPRandomValue(70), nil)
	if callback.Code != http.StatusSeeOther || callback.Header().Get("Location") != testOnboardingOrigin+"/fern/control" || restartedExchanger.calls() != 1 {
		t.Fatalf("restart status = %d, exchanges = %d", callback.Code, restartedExchanger.calls())
	}
}

func TestOnboardingHTTPConcurrentCallbackExchangesExactlyOnce(t *testing.T) {
	store, _ := newTestOnboardingStateStore(t)
	events := &onboardingHTTPEvents{}
	exchanger := &fakeManifestExchanger{credentials: testStoredCredentials(t, 901, "concurrent"), events: events}
	saver := &fakeCredentialPersistence{events: events}
	handler := newTestOnboardingHTTP(t, &recordingOnboardingStates{delegate: store, events: events}, exchanger, saver, testOnboardingRandom(80, 81, 82))
	if setup := serveOnboarding(handler, http.MethodGet, GitHubAppSetupPath+"?return=%2Ffern%2Fcontrol", nil); setup.Code != http.StatusOK {
		t.Fatal(setup.Body.String())
	}
	target := GitHubAppCallbackPath + "?code=concurrent-code&state=" + testHTTPRandomValue(80)
	statuses := make(chan int, 24)
	var wait sync.WaitGroup
	for range 24 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			statuses <- serveOnboarding(handler, http.MethodGet, target, nil).Code
		}()
	}
	wait.Wait()
	close(statuses)
	successes, recovery := 0, 0
	for status := range statuses {
		switch status {
		case http.StatusSeeOther:
			successes++
		case http.StatusConflict:
			recovery++
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	if successes != 1 || recovery != 23 || exchanger.calls() != 1 || saver.calls() != 1 {
		t.Fatalf("success = %d, recovery = %d, exchange = %d, save = %d", successes, recovery, exchanger.calls(), saver.calls())
	}
}

func TestOnboardingHTTPRestartNeverReauthorizesClaimedCallback(t *testing.T) {
	store, _ := newTestOnboardingStateStore(t)
	active := newTestOnboardingHTTP(t, store, &fakeManifestExchanger{}, &fakeCredentialPersistence{}, testOnboardingRandom(100, 101))
	if setup := serveOnboarding(active, http.MethodGet, GitHubAppSetupPath+"?return=%2Ffern%2Fcontrol", nil); setup.Code != http.StatusOK {
		t.Fatal(setup.Body.String())
	}
	state := testHTTPRandomValue(100)
	code := "claimed-before-restart"
	flow := active.flows[state]
	if _, err := store.Claim(context.Background(), state, flow.binding, sha256.Sum256([]byte(code)), testHTTPRandomValue(102), testOnboardingTime()); err != nil {
		t.Fatal(err)
	}
	exchanger := &fakeManifestExchanger{}
	restarted := newTestOnboardingHTTP(t, store, exchanger, &fakeCredentialPersistence{}, testOnboardingRandom(103))
	response := serveOnboarding(restarted, http.MethodGet, GitHubAppCallbackPath+"?code="+code+"&state="+state, nil)
	if response.Code != http.StatusConflict || exchanger.calls() != 0 {
		t.Fatalf("status = %d, exchanges = %d", response.Code, exchanger.calls())
	}
}

type onboardingHTTPEvents struct {
	mu     sync.Mutex
	values []string
}

func (events *onboardingHTTPEvents) add(value string) {
	if events == nil {
		return
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	events.values = append(events.values, value)
}

func (events *onboardingHTTPEvents) snapshot() []string {
	if events == nil {
		return nil
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	return append([]string(nil), events.values...)
}

type recordingOnboardingStates struct {
	delegate    onboardingStatePersistence
	events      *onboardingHTTPEvents
	beginErr    error
	claimErr    error
	completeErr error
}

func (states *recordingOnboardingStates) Begin(ctx context.Context, state string, binding OnboardingFlowBinding, now, expiresAt time.Time) error {
	states.events.add("begin")
	if states.beginErr != nil {
		return states.beginErr
	}
	return states.delegate.Begin(ctx, state, binding, now, expiresAt)
}

func (states *recordingOnboardingStates) Claim(ctx context.Context, state string, binding OnboardingFlowBinding, code [sha256.Size]byte, claimID string, now time.Time) (CallbackClaim, error) {
	states.events.add("claim")
	if states.claimErr != nil {
		return CallbackClaim{}, states.claimErr
	}
	return states.delegate.Claim(ctx, state, binding, code, claimID, now)
}

func (states *recordingOnboardingStates) Complete(ctx context.Context, claim CallbackClaim, now time.Time) error {
	states.events.add("complete")
	if states.completeErr != nil {
		return states.completeErr
	}
	return states.delegate.Complete(ctx, claim, now)
}

func (states *recordingOnboardingStates) Quarantine(ctx context.Context, claim CallbackClaim, reason CallbackQuarantineReason, now time.Time) error {
	states.events.add("quarantine:" + reason.String())
	return states.delegate.Quarantine(ctx, claim, reason, now)
}

func (states *recordingOnboardingStates) ResolvePending(ctx context.Context, state string, now time.Time) (OnboardingFlowBinding, time.Time, error) {
	resolver, ok := states.delegate.(onboardingStateResolver)
	if !ok {
		return OnboardingFlowBinding{}, time.Time{}, ErrOnboardingStateRejected
	}
	return resolver.ResolvePending(ctx, state, now)
}

type fakeManifestExchanger struct {
	mu          sync.Mutex
	credentials AppCredentials
	err         error
	events      *onboardingHTTPEvents
	callCount   int
	codeValue   string
	deadline    bool
}

func (exchanger *fakeManifestExchanger) Exchange(ctx context.Context, code ManifestCode) (AppCredentials, error) {
	exchanger.events.add("exchange")
	_, deadline := ctx.Deadline()
	exchanger.mu.Lock()
	defer exchanger.mu.Unlock()
	exchanger.callCount++
	exchanger.deadline = deadline
	if code.state != nil {
		exchanger.codeValue = code.state.value
	}
	return exchanger.credentials, exchanger.err
}

func (exchanger *fakeManifestExchanger) calls() int {
	exchanger.mu.Lock()
	defer exchanger.mu.Unlock()
	return exchanger.callCount
}

func (exchanger *fakeManifestExchanger) code() string {
	exchanger.mu.Lock()
	defer exchanger.mu.Unlock()
	return exchanger.codeValue
}

func (exchanger *fakeManifestExchanger) sawDeadline() bool {
	exchanger.mu.Lock()
	defer exchanger.mu.Unlock()
	return exchanger.deadline
}

type fakeCredentialPersistence struct {
	mu          sync.Mutex
	err         error
	events      *onboardingHTTPEvents
	callCount   int
	credentials AppCredentials
}

func (store *fakeCredentialPersistence) Save(credentials AppCredentials) error {
	store.events.add("save")
	store.mu.Lock()
	defer store.mu.Unlock()
	store.callCount++
	store.credentials = credentials
	return store.err
}

func (store *fakeCredentialPersistence) calls() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.callCount
}

func (store *fakeCredentialPersistence) saved() AppCredentials {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.credentials
}

func newTestOnboardingHTTP(t *testing.T, states onboardingStatePersistence, exchanger manifestExchanger, credentials credentialPersistence, random io.Reader) *OnboardingHTTP {
	t.Helper()
	return newTestOnboardingHTTPWithClock(t, states, exchanger, credentials, random, func() time.Time {
		return testOnboardingTime().In(time.FixedZone("test-local", -7*60*60))
	})
}

func newTestOnboardingHTTPWithClock(t *testing.T, states onboardingStatePersistence, exchanger manifestExchanger, credentials credentialPersistence, random io.Reader, now func() time.Time) *OnboardingHTTP {
	t.Helper()
	handler, err := NewOnboardingHTTP(testOnboardingOrigin, "Fern Test App", states, exchanger, credentials, random, now)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func testOnboardingRandom(values ...byte) io.Reader {
	var payload []byte
	for _, value := range values {
		payload = append(payload, bytes.Repeat([]byte{value}, sha256.Size)...)
	}
	return bytes.NewReader(payload)
}

func testHTTPRandomValue(value byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{value}, sha256.Size))
}

func newOnboardingRequest(method, target string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, target, body)
	request.Host = "fern.example"
	return request
}

func serveOnboarding(handler http.Handler, method, target string, body io.Reader) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newOnboardingRequest(method, target, body))
	return response
}

func assertOnboardingHeaders(t *testing.T, header http.Header) {
	t.Helper()
	if header.Get("Cache-Control") != "no-store" || header.Get("Content-Security-Policy") == "" || header.Get("Referrer-Policy") != "no-referrer" || header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers = %#v", header)
	}
}
