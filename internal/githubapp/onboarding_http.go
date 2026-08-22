package githubapp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"
)

const (
	GitHubAppSetupPath    = "/fern/github/app/setup"
	GitHubAppCallbackPath = "/fern/github/app/callback"

	onboardingHTTPStateLifetime     = 10 * time.Minute
	onboardingHTTPExchangeTimeout   = 30 * time.Second
	onboardingHTTPCloseTimeout      = 5 * time.Second
	maxOnboardingSetupQueryBytes    = 4 << 10
	maxOnboardingCallbackQueryBytes = 1 << 10
)

var ErrInvalidOnboardingHTTPConfiguration = errors.New("invalid GitHub App onboarding HTTP configuration")

type onboardingStatePersistence interface {
	Begin(context.Context, string, OnboardingFlowBinding, time.Time, time.Time) error
	Claim(context.Context, string, OnboardingFlowBinding, [sha256.Size]byte, string, time.Time) (CallbackClaim, error)
	Complete(context.Context, CallbackClaim, time.Time) error
	Quarantine(context.Context, CallbackClaim, CallbackQuarantineReason, time.Time) error
}

type manifestExchanger interface {
	Exchange(context.Context, ManifestCode) (AppCredentials, error)
}

type credentialPersistence interface {
	Save(AppCredentials) error
}

type onboardingStateResolver interface {
	ResolvePending(context.Context, string, time.Time) (OnboardingFlowBinding, time.Time, error)
}

var (
	_ onboardingStatePersistence = (*OnboardingStateStore)(nil)
	_ manifestExchanger          = (*ManifestClient)(nil)
	_ credentialPersistence      = (*CredentialStore)(nil)
)

type onboardingHTTPFlow struct {
	mu        sync.Mutex
	binding   OnboardingFlowBinding
	expiresAt time.Time
	claimID   string
}

// OnboardingHTTP coordinates the browser-facing portion of GitHub's App
// Manifest flow. The embedding gateway authenticates setup on Fern's loopback
// operator surface; callback authority comes from its exact one-use state.
type OnboardingHTTP struct {
	origin      *url.URL
	setupOrigin *url.URL
	manifest    []byte
	states      onboardingStatePersistence
	exchanger   manifestExchanger
	credentials credentialPersistence
	random      io.Reader
	now         func() time.Time

	mu       sync.Mutex
	randomMu sync.Mutex
	flows    map[string]*onboardingHTTPFlow
}

func (handler *OnboardingHTTP) String() string {
	return "GitHub App onboarding HTTP coordinator"
}

func (handler *OnboardingHTTP) GoString() string {
	return handler.String()
}

var onboardingManifestPage = template.Must(template.New("github-app-manifest").Parse(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Connect GitHub</title></head>
<body>
<form id="manifest" method="post" action="{{.Action}}"><input type="hidden" name="manifest" value="{{.Manifest}}"></form>
<script>document.getElementById('manifest').submit()</script>
</body>
</html>`))

// NewOnboardingHTTP validates fixed onboarding policy and constructs a handler.
// rootOrigin must be a canonical HTTPS origin without a trailing slash.
func NewOnboardingHTTP(rootOrigin, appName string, states onboardingStatePersistence, exchanger manifestExchanger, credentials credentialPersistence, random io.Reader, now func() time.Time) (*OnboardingHTTP, error) {
	return NewOnboardingHTTPWithSetupOrigin(rootOrigin, rootOrigin, appName, states, exchanger, credentials, random, now)
}

// NewOnboardingHTTPWithSetupOrigin allows the authenticated setup page to be
// served on an exact loopback HTTP origin while retaining the private HTTPS
// origin as the only accepted GitHub callback authority.
func NewOnboardingHTTPWithSetupOrigin(rootOrigin, setupRootOrigin, appName string, states onboardingStatePersistence, exchanger manifestExchanger, credentials credentialPersistence, random io.Reader, now func() time.Time) (*OnboardingHTTP, error) {
	origin, err := parseCanonicalRootOrigin(rootOrigin)
	setupOrigin, setupErr := parseCanonicalSetupOrigin(setupRootOrigin)
	if err != nil || setupErr != nil || nilDependency(states) || nilDependency(exchanger) || nilDependency(credentials) || nilDependency(random) || now == nil {
		return nil, ErrInvalidOnboardingHTTPConfiguration
	}
	manifest, err := GenerateAppManifest(appName, rootOrigin, rootOrigin+GitHubAppCallbackPath)
	if err != nil {
		return nil, ErrInvalidOnboardingHTTPConfiguration
	}
	return &OnboardingHTTP{
		origin:      origin,
		setupOrigin: setupOrigin,
		manifest:    manifest,
		states:      states,
		exchanger:   exchanger,
		credentials: credentials,
		random:      random,
		now:         now,
		flows:       make(map[string]*onboardingHTTPFlow),
	}, nil
}

func (handler *OnboardingHTTP) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	setOnboardingHTTPHeaders(writer.Header())
	if handler == nil || handler.origin == nil || request == nil {
		http.Error(writer, "GitHub App onboarding is unavailable", http.StatusServiceUnavailable)
		return
	}
	if !handler.validRequestTarget(request) {
		http.NotFound(writer, request)
		return
	}

	switch request.URL.Path {
	case GitHubAppSetupPath:
		if request.Method != http.MethodGet {
			methodNotAllowedOnboarding(writer, http.MethodGet)
			return
		}
		handler.setup(writer, request)
	case GitHubAppCallbackPath:
		if request.Method != http.MethodGet {
			methodNotAllowedOnboarding(writer, http.MethodGet)
			return
		}
		handler.callback(writer, request)
	default:
		http.NotFound(writer, request)
	}
}

func (handler *OnboardingHTTP) setup(writer http.ResponseWriter, request *http.Request) {
	returnPath, ok := exactOnboardingQuery(request.URL, "return", maxOnboardingSetupQueryBytes)
	if !ok || !validOnboardingHTTPReturnPath(returnPath) {
		onboardingBadRequest(writer)
		return
	}
	now, ok := handler.currentTime()
	if !ok {
		onboardingUnavailable(writer)
		return
	}
	state, err := handler.randomValue()
	if err != nil {
		onboardingUnavailable(writer)
		return
	}
	flowID, err := handler.randomValue()
	if err != nil {
		onboardingUnavailable(writer)
		return
	}
	binding := OnboardingFlowBinding{FlowID: flowID, ReturnPath: returnPath}
	expiresAt := now.Add(onboardingHTTPStateLifetime)
	if err := handler.states.Begin(request.Context(), state, binding, now, expiresAt); err != nil {
		onboardingUnavailable(writer)
		return
	}

	handler.mu.Lock()
	handler.pruneFlows(now)
	handler.flows[state] = &onboardingHTTPFlow{binding: binding, expiresAt: expiresAt}
	handler.mu.Unlock()

	action := "https://github.com/settings/apps/new?state=" + url.QueryEscape(state)
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = onboardingManifestPage.Execute(writer, struct {
		Action   string
		Manifest string
	}{Action: action, Manifest: string(handler.manifest)})
}

func (handler *OnboardingHTTP) callback(writer http.ResponseWriter, request *http.Request) {
	values, ok := exactCallbackQuery(request.URL)
	if !ok {
		onboardingBadRequest(writer)
		return
	}
	codeValue, state := values[0], values[1]
	if _, err := onboardingStateHash(state); err != nil {
		onboardingBadRequest(writer)
		return
	}
	code, err := NewManifestCode(codeValue)
	if err != nil {
		onboardingBadRequest(writer)
		return
	}

	now, ok := handler.currentTime()
	if !ok {
		onboardingUnavailable(writer)
		return
	}
	handler.mu.Lock()
	flow := handler.flows[state]
	handler.mu.Unlock()
	if flow == nil {
		resolver, supportsRecovery := handler.states.(onboardingStateResolver)
		if !supportsRecovery {
			onboardingBadRequest(writer)
			return
		}
		binding, expiresAt, resolveErr := resolver.ResolvePending(request.Context(), state, now)
		if resolveErr != nil {
			if errors.Is(resolveErr, ErrOnboardingStateRecoveryRequired) {
				onboardingRecoveryRequired(writer)
				return
			}
			if errors.Is(resolveErr, ErrOnboardingStateRejected) || errors.Is(resolveErr, ErrInvalidOnboardingState) {
				onboardingBadRequest(writer)
				return
			}
			onboardingUnavailable(writer)
			return
		}
		recovered := &onboardingHTTPFlow{binding: binding, expiresAt: expiresAt}
		handler.mu.Lock()
		if flow = handler.flows[state]; flow == nil {
			handler.pruneFlows(now)
			handler.flows[state] = recovered
			flow = recovered
		}
		handler.mu.Unlock()
	}

	flow.mu.Lock()
	defer flow.mu.Unlock()
	if flow.claimID == "" {
		flow.claimID, err = handler.randomValue()
		if err != nil {
			onboardingUnavailable(writer)
			return
		}
	}
	claim, err := handler.states.Claim(request.Context(), state, flow.binding, sha256.Sum256([]byte(codeValue)), flow.claimID, now)
	if err != nil {
		if errors.Is(err, ErrOnboardingStateRecoveryRequired) {
			onboardingRecoveryRequired(writer)
			return
		}
		if errors.Is(err, ErrOnboardingStateRejected) || errors.Is(err, ErrInvalidOnboardingState) {
			onboardingBadRequest(writer)
			return
		}
		onboardingUnavailable(writer)
		return
	}
	if claim.Disposition() == CallbackClaimReconcileOnly {
		handler.quarantine(claim, CallbackQuarantineReconcileAmbiguous, now)
		onboardingRecoveryRequired(writer)
		return
	}
	if claim.Disposition() != CallbackClaimExchangeOnce {
		handler.quarantine(claim, CallbackQuarantineCoordinatorAborted, now)
		onboardingRecoveryRequired(writer)
		return
	}

	exchangeContext, cancel := context.WithTimeout(request.Context(), onboardingHTTPExchangeTimeout)
	credentials, exchangeErr := handler.exchanger.Exchange(exchangeContext, code)
	cancel()
	if exchangeErr != nil {
		handler.quarantine(claim, CallbackQuarantineExchangeAmbiguous, now)
		onboardingRecoveryRequired(writer)
		return
	}
	if err := handler.credentials.Save(credentials); err != nil {
		handler.quarantine(claim, CallbackQuarantineCoordinatorAborted, now)
		onboardingRecoveryRequired(writer)
		return
	}
	closeContext, closeCancel := context.WithTimeout(context.Background(), onboardingHTTPCloseTimeout)
	err = handler.states.Complete(closeContext, claim, handler.safeNow(now))
	closeCancel()
	if err != nil {
		handler.quarantine(claim, CallbackQuarantineCoordinatorAborted, now)
		onboardingRecoveryRequired(writer)
		return
	}
	http.Redirect(writer, request, handler.setupOrigin.String()+claim.Binding().ReturnPath, http.StatusSeeOther)
}

func (handler *OnboardingHTTP) validRequestTarget(request *http.Request) bool {
	if request.URL == nil || request.URL.IsAbs() || request.URL.Opaque != "" || request.URL.User != nil || request.URL.Host != "" || request.URL.Scheme != "" || request.URL.Fragment != "" || request.URL.RawFragment != "" || request.URL.EscapedPath() != request.URL.Path {
		return false
	}
	wantHost := handler.origin.Host
	if request.URL.Path == GitHubAppSetupPath {
		wantHost = handler.setupOrigin.Host
	}
	if request.Host != wantHost || request.ContentLength > 0 || len(request.TransferEncoding) != 0 {
		return false
	}
	return true
}

func parseCanonicalSetupOrigin(value string) (*url.URL, error) {
	if origin, err := parseCanonicalRootOrigin(value); err == nil {
		return origin, nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" || parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || parsed.String() != value {
		return nil, ErrInvalidOnboardingHTTPConfiguration
	}
	host := parsed.Hostname()
	if host == "" || parsed.Port() == "" || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return nil, ErrInvalidOnboardingHTTPConfiguration
	}
	return parsed, nil
}

func (handler *OnboardingHTTP) randomValue() (string, error) {
	var value [sha256.Size]byte
	handler.randomMu.Lock()
	_, err := io.ReadFull(handler.random, value[:])
	handler.randomMu.Unlock()
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func (handler *OnboardingHTTP) safeNow(fallback time.Time) time.Time {
	now, ok := handler.currentTime()
	if !ok || now.Before(fallback) {
		return fallback
	}
	return now
}

func (handler *OnboardingHTTP) currentTime() (time.Time, bool) {
	now := handler.now()
	if now.IsZero() {
		return time.Time{}, false
	}
	now = now.UTC()
	return now, validUTC(now)
}

func (handler *OnboardingHTTP) quarantine(claim CallbackClaim, reason CallbackQuarantineReason, fallback time.Time) {
	closeContext, cancel := context.WithTimeout(context.Background(), onboardingHTTPCloseTimeout)
	defer cancel()
	_ = handler.states.Quarantine(closeContext, claim, reason, handler.safeNow(fallback))
}

func (handler *OnboardingHTTP) pruneFlows(now time.Time) {
	for state, flow := range handler.flows {
		if !flow.expiresAt.Add(maxOnboardingReplayWindow).After(now) {
			delete(handler.flows, state)
		}
	}
}

func parseCanonicalRootOrigin(value string) (*url.URL, error) {
	if value == "" || len(value) > maxManifestURLBytes || !strings.HasPrefix(value, "https://") {
		return nil, ErrInvalidOnboardingHTTPConfiguration
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" || parsed.Port() == "443" || parsed.User != nil || parsed.Opaque != "" || parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || parsed.Host != strings.ToLower(parsed.Host) || parsed.String() != value {
		return nil, ErrInvalidOnboardingHTTPConfiguration
	}
	return parsed, nil
}

func nilDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func exactOnboardingQuery(target *url.URL, key string, maximum int) (string, bool) {
	if target == nil || target.ForceQuery || target.RawQuery == "" || len(target.RawQuery) > maximum {
		return "", false
	}
	query, err := url.ParseQuery(target.RawQuery)
	if err != nil || len(query) != 1 || len(query[key]) != 1 {
		return "", false
	}
	return query[key][0], true
}

func validOnboardingHTTPReturnPath(value string) bool {
	if strings.Contains(value, "#") || !validOnboardingBinding(OnboardingFlowBinding{FlowID: "flow", ReturnPath: value}) {
		return false
	}
	parsed, err := url.ParseRequestURI(value)
	return err == nil && parsed.RawPath == "" && parsed.EscapedPath() == parsed.Path
}

func exactCallbackQuery(target *url.URL) ([2]string, bool) {
	var result [2]string
	if target == nil || target.ForceQuery || target.RawQuery == "" || len(target.RawQuery) > maxOnboardingCallbackQueryBytes {
		return result, false
	}
	query, err := url.ParseQuery(target.RawQuery)
	if err != nil || len(query) != 2 || len(query["code"]) != 1 || len(query["state"]) != 1 {
		return result, false
	}
	result[0], result[1] = query["code"][0], query["state"][0]
	return result, true
}

func setOnboardingHTTPHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Security-Policy", "default-src 'none'; script-src 'sha256-INSjigJHN5NWmw7+9FKI9BK1pabUb2oNIcLrpX0O6g4='; base-uri 'none'; frame-ancestors 'none'; form-action https://github.com")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
}

func methodNotAllowedOnboarding(writer http.ResponseWriter, method string) {
	writer.Header().Set("Allow", method)
	http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
}

func onboardingBadRequest(writer http.ResponseWriter) {
	http.Error(writer, "invalid GitHub App onboarding request", http.StatusBadRequest)
}

func onboardingUnavailable(writer http.ResponseWriter) {
	http.Error(writer, "GitHub App onboarding is unavailable", http.StatusServiceUnavailable)
}

func onboardingRecoveryRequired(writer http.ResponseWriter) {
	http.Error(writer, "GitHub App onboarding recovery is required", http.StatusConflict)
}
