package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskapi"
)

const (
	deviceCookieName       = "__Host-fern_device"
	legacyDeviceCookieName = "fern_device"
	maxDeviceNameBytes     = 80
	maxOutstandingPairings = 64
)

var pairingTemplate = template.Must(template.New("pairing").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="color-scheme" content="dark">
<title>Pair with Fern</title>
<style>
:root{font-family:ui-rounded,"SF Pro Rounded","Avenir Next",system-ui,sans-serif;color:#f3f7e9;background:#11180f}
*{box-sizing:border-box}body{display:grid;min-height:100dvh;margin:0;padding:24px 18px;background:radial-gradient(circle at 15% 0,#314829 0,transparent 42%),#11180f}
main{width:min(100%,520px);margin:auto;padding:28px;border:1px solid #52664a;border-radius:26px;background:#182116e8;box-shadow:0 24px 80px #0005}
.mark{display:grid;place-items:center;width:54px;height:54px;border-radius:18px;background:#b9ef86;color:#162210;font-size:28px;font-weight:800;transform:rotate(-3deg)}
h1{margin:24px 0 8px;font-size:34px;letter-spacing:-.04em}p{margin:0;color:#bdcbb5;font-size:16px;line-height:1.55}
label{display:block;margin-top:24px;color:#dce8d3;font-size:14px;font-weight:700}input[type=text]{display:block;width:100%;margin-top:8px;padding:13px 14px;border:1px solid #52664a;border-radius:13px;background:#11180f;color:#f3f7e9;font:inherit}button{display:block;width:100%;margin-top:24px;padding:15px 18px;border:0;border-radius:15px;background:#b9ef86;color:#15200f;font:inherit;font-weight:750;cursor:pointer}
</style>
</head>
<body><main><div class="mark">F</div><h1>Pair this phone?</h1><p>This gives this browser private access to your Fern workspace for 30 days.</p><form method="post" action="/fern/pair"><input type="hidden" name="code" value="{{.Code}}"><label for="device-name">Device name</label><input id="device-name" type="text" name="name" value="{{.Name}}" maxlength="80" autocomplete="nickname"><button type="submit">Pair this phone</button></form></main></body></html>`))

type pairingPage struct {
	Code string
	Name string
}

type pairingState struct {
	mu                    sync.Mutex
	codes                 map[[sha256.Size]byte]time.Time
	attempts              map[[sha256.Size]byte]pairingAttempt
	invalidAttempts       []time.Time
	lastIssued            time.Time
	lastSuccess           time.Time
	sessions              map[[sha256.Size]byte]time.Time
	now                   func() time.Time
	store                 *control.Store
	persistencePath       string
	persistenceErr        error
	operatorCredentialID  string
	operatorCredentialErr error
}

// processOperatorCredentialID lazily mints one random credential identifier per
// process. It backs pairing states built without a control store (tests): such
// a state has nowhere durable to record the identifier, and deriving it from
// the control password would put an offline brute-force oracle of that secret
// into audit snapshots.
var processOperatorCredentialID = sync.OnceValues(func() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return control.OperatorCredentialIDPrefix + base64.RawURLEncoding.EncodeToString(value), nil
})

func newPairingState(stores ...*control.Store) *pairingState {
	state := &pairingState{
		codes:    make(map[[sha256.Size]byte]time.Time),
		attempts: make(map[[sha256.Size]byte]pairingAttempt),
		sessions: make(map[[sha256.Size]byte]time.Time),
		now:      time.Now,
	}
	if len(stores) != 0 {
		state.store = stores[0]
		state.persistencePath = pairingPersistencePath(stores[0])
		state.persistenceErr = state.loadPersisted()
		if stores[0] != nil {
			// Resolved once so every operator request in this process attributes
			// itself with the same stable, persisted random identifier.
			state.operatorCredentialID, state.operatorCredentialErr = stores[0].EnsureOperatorCredentialID()
		} else {
			state.operatorCredentialID, state.operatorCredentialErr = processOperatorCredentialID()
		}
	} else {
		state.operatorCredentialID, state.operatorCredentialErr = processOperatorCredentialID()
	}
	return state
}

func (state *pairingState) handler(next http.Handler, auth runtime.ServerAuth, control ControlAuth) http.Handler {
	upstreamAuth := newBasicAuthenticator("opencode", auth.Password, "opencode")
	controlAuth := newBasicAuthenticator("fern", control.Password, "fern-control")
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/fern/pair/new" && request.URL.EscapedPath() == "/fern/pair/new":
			if !controlAuth.valid(request) {
				controlAuth.reject(writer)
				return
			}
			request.Header.Del("Authorization")
			state.issue(writer, request)
		case request.URL.Path == "/fern/pair" && request.URL.EscapedPath() == "/fern/pair":
			state.pair(writer, request)
		case requiresControlAuth(request):
			if !controlAuth.valid(request) {
				controlAuth.reject(writer)
				return
			}
			stripDeviceCookie(request)
			request.Header.Del("Authorization")
			next.ServeHTTP(writer, request)
		default:
			if device, credential, valid := state.authenticatedDevice(request); valid {
				if !state.authorizeDeviceMutation(writer, request, credential) {
					return
				}
				if state.servePaired(writer, request, next, auth, device, credential) {
					return
				}
			}
			if isFernRoute(request) && controlAuth.valid(request) {
				request.Header.Del("Authorization")
				next.ServeHTTP(writer, request)
				return
			}
			if !upstreamAuth.enabled {
				stripDeviceCookie(request)
				next.ServeHTTP(writer, request)
				return
			}
			if !upstreamAuth.valid(request) {
				upstreamAuth.reject(writer)
				return
			}
			stripDeviceCookie(request)
			next.ServeHTTP(writer, request)
		}
	})
}

func (state *pairingState) remoteHandler(next http.Handler, auth runtime.ServerAuth) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/fern/pair" && request.URL.EscapedPath() == "/fern/pair" {
			request.Header.Del("Authorization")
			request.Header.Del("Cookie")
			state.pair(writer, request)
			return
		}
		if request.URL.Path == "/fern/github/app/callback" && request.URL.EscapedPath() == request.URL.Path {
			request.Header.Del("Authorization")
			request.Header.Del("Cookie")
			next.ServeHTTP(writer, request)
			return
		}
		device, credential, valid := state.authenticatedDevice(request)
		if !valid {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		if isFernRoute(request) && request.URL.Path != "/fern" && request.URL.Path != "/fern/" && !isTaskAPIPath(request.URL.Path) && !isTaskUIPath(request.URL.Path) {
			if request.URL.Path == csrfTokenPath && request.URL.EscapedPath() == csrfTokenPath {
				state.serveCSRFToken(writer, request, credential)
				return
			}
			http.NotFound(writer, request)
			return
		}
		if !state.authorizeDeviceMutation(writer, request, credential) {
			return
		}
		state.servePaired(writer, request, next, auth, device, credential)
	})
}

func isTaskUIPath(path string) bool {
	return path == "/fern/tasks" || path == "/fern/assets/tasks.js"
}

func (state *pairingState) operatorHandler(next http.Handler, auth runtime.ServerAuth, control ControlAuth) http.Handler {
	// This loopback-only surface uses explicit Basic credentials rather than
	// ambient browser cookies, so device CSRF tokens do not apply here.
	upstreamAuth := newBasicAuthenticator("opencode", auth.Password, "opencode")
	controlAuth := newBasicAuthenticator("fern", control.Password, "fern-control")
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if isFernRoute(request) {
			if request.URL.Path == "/fern/pair" {
				http.NotFound(writer, request)
				return
			}
			if !controlAuth.valid(request) {
				controlAuth.reject(writer)
				return
			}
			request.Header.Del("Authorization")
			stripAllCookies(request)
			actor, err := state.operatorActor()
			if err != nil {
				http.Error(writer, "operator identity unavailable", http.StatusInternalServerError)
				return
			}
			request = request.WithContext(taskapi.WithActor(request.Context(), actor))
			if request.URL.Path == "/fern/pair/new" && request.URL.EscapedPath() == "/fern/pair/new" {
				state.issue(writer, request)
				return
			}
			next.ServeHTTP(writer, request)
			return
		}
		if !upstreamAuth.valid(request) {
			upstreamAuth.reject(writer)
			return
		}
		request.Header.Del("Authorization")
		stripAllCookies(request)
		if auth.Password != "" {
			request.SetBasicAuth("opencode", auth.Password)
		}
		next.ServeHTTP(writer, request)
	})
}

func isFernRoute(request *http.Request) bool {
	path := request.URL.Path
	return path == "/fern" || path == "/fern/" || strings.HasPrefix(path, "/fern/")
}

func requiresControlAuth(request *http.Request) bool {
	path := request.URL.Path
	escaped := request.URL.EscapedPath()
	if path != escaped {
		return strings.HasPrefix(path, "/fern/api/") || strings.HasPrefix(path, "/fern/workflows") || strings.HasPrefix(path, "/fern/devices/") || strings.HasPrefix(path, "/fern/control")
	}
	return path == "/fern/control" || strings.HasPrefix(path, "/fern/control/") ||
		strings.HasPrefix(path, "/fern/api/v1/") || path == "/fern/workflows" || strings.HasPrefix(path, "/fern/workflows/") ||
		strings.HasPrefix(path, "/fern/devices/")
}

func (state *pairingState) issue(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", "POST")
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	now := state.now()
	state.mu.Lock()
	state.prune(now)
	if state.persistenceErr != nil {
		state.mu.Unlock()
		writeUnavailable(writer, "pairing state")
		return
	}
	if !state.lastIssued.IsZero() && now.Sub(state.lastIssued) < pairingIssueInterval {
		state.mu.Unlock()
		writer.Header().Set("Retry-After", "1")
		http.Error(writer, "pairing code issuance is temporarily limited", http.StatusTooManyRequests)
		return
	}
	if len(state.codes) >= maxOutstandingPairings {
		state.mu.Unlock()
		writer.Header().Set("Retry-After", "300")
		http.Error(writer, "too many outstanding pairing codes", http.StatusTooManyRequests)
		return
	}
	code, err := randomCredential()
	if err != nil {
		state.mu.Unlock()
		http.Error(writer, "failed to create pairing code", http.StatusInternalServerError)
		return
	}
	digest := sha256.Sum256([]byte(code))
	state.codes[digest] = now.Add(pairingCodeTTL)
	previousIssued := state.lastIssued
	state.lastIssued = now
	if err := state.persistLocked(); err != nil {
		delete(state.codes, digest)
		state.lastIssued = previousIssued
		state.mu.Unlock()
		writeUnavailable(writer, "pairing state")
		return
	}
	state.mu.Unlock()
	setFernHeaders(writer.Header())
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]string{"code": code, "expiresIn": pairingCodeTTL.String()})
}

func (state *pairingState) pair(writer http.ResponseWriter, request *http.Request) {
	setFernHeaders(writer.Header())
	if request.Method != http.MethodGet && request.Method != http.MethodPost {
		writer.Header().Set("Allow", "GET, POST")
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	code := request.URL.Query().Get("code")
	name := request.URL.Query().Get("name")
	if request.Method == http.MethodPost {
		request.Body = http.MaxBytesReader(writer, request.Body, 4<<10)
		if err := request.ParseForm(); err != nil {
			http.Error(writer, "invalid pairing form", http.StatusBadRequest)
			return
		}
		code = request.PostFormValue("code")
		name = request.PostFormValue("name")
	}
	name, validName := pairingDeviceName(name)
	hash := sha256.Sum256([]byte(code))
	now := state.now()
	state.mu.Lock()
	if state.persistenceErr != nil {
		state.mu.Unlock()
		writeUnavailable(writer, "pairing state")
		return
	}
	state.prune(now)
	if len(state.invalidAttempts) >= maxGlobalPairingFailures {
		state.mu.Unlock()
		writer.Header().Set("Retry-After", "300")
		http.Error(writer, "pairing attempts are temporarily limited", http.StatusTooManyRequests)
		return
	}
	expires, valid := state.codes[hash]
	valid = valid && code != "" && now.Before(expires)
	if attempt := state.attempts[hash]; attempt.Count >= maxPairingCodeAttempts {
		valid = false
	}
	if valid && validName && request.Method == http.MethodGet {
		state.mu.Unlock()
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := pairingTemplate.Execute(writer, pairingPage{Code: code, Name: name}); err != nil {
			http.Error(writer, "render pairing page", http.StatusInternalServerError)
		}
		return
	}
	if !valid || !validName {
		state.recordInvalidLocked(hash, now)
		if err := state.persistLocked(); err != nil {
			state.mu.Unlock()
			writeUnavailable(writer, "pairing state")
			return
		}
		state.mu.Unlock()
		if !validName {
			http.Error(writer, "invalid device name", http.StatusBadRequest)
			return
		}
		http.Error(writer, "pairing link is invalid or expired", http.StatusUnauthorized)
		return
	}
	if !state.lastSuccess.IsZero() && now.Sub(state.lastSuccess) < pairingSuccessInterval {
		state.mu.Unlock()
		writer.Header().Set("Retry-After", "1")
		http.Error(writer, "pairing is temporarily limited", http.StatusTooManyRequests)
		return
	}
	session, pairErr := randomCredential()
	if pairErr == nil {
		// Consume the one-time code durably before creating a durable device
		// grant. If a later effect fails, the operator issues a new code rather
		// than risking reuse after a lost response.
		delete(state.codes, hash)
		delete(state.attempts, hash)
		state.lastSuccess = now
		pairErr = state.persistLocked()
	}
	if pairErr == nil {
		if state.store == nil {
			state.sessions[sha256.Sum256([]byte(session))] = now.Add(deviceCredentialTTL)
		} else {
			_, pairErr = state.store.AddDevice(session, name, now, now.Add(deviceCredentialTTL))
		}
	}
	state.mu.Unlock()
	if pairErr != nil {
		writeUnavailable(writer, "pairing state")
		return
	}
	http.SetCookie(writer, &http.Cookie{
		Name: deviceCookieName, Value: session, Path: "/", MaxAge: int(deviceCredentialTTL.Seconds()),
		Expires: now.Add(deviceCredentialTTL), HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(writer, request, "/fern/", http.StatusSeeOther)
}

func pairingDeviceName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if len(name) > maxDeviceNameBytes || !utf8.ValidString(name) {
		return "", false
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return "", false
		}
	}
	return name, true
}

func (state *pairingState) authenticatedDevice(request *http.Request) (control.Device, string, bool) {
	cookie, err := request.Cookie(deviceCookieName)
	if err != nil || cookie.Value == "" {
		return control.Device{}, "", false
	}
	now := state.now()
	if state.store != nil {
		device, valid, err := state.store.AuthenticateDeviceIdentity(cookie.Value, now)
		return device, cookie.Value, err == nil && valid
	}
	hash := sha256.Sum256([]byte(cookie.Value))
	state.mu.Lock()
	expires, valid := state.sessions[hash]
	if valid && !now.Before(expires) {
		delete(state.sessions, hash)
		valid = false
	}
	state.mu.Unlock()
	return control.Device{}, cookie.Value, valid
}

func (state *pairingState) servePaired(writer http.ResponseWriter, request *http.Request, next http.Handler, auth runtime.ServerAuth, device control.Device, credential string) bool {
	if state.store != nil {
		ctx, cancel := context.WithDeadline(request.Context(), device.ExpiresAt)
		unregister, admitted := state.store.RegisterDeviceRequest(device.ID, cancel)
		if !admitted {
			cancel()
			return false
		}
		defer func() {
			unregister()
			cancel()
		}()
		request = request.WithContext(ctx)
	}
	requestID, err := randomCredential()
	if err != nil {
		http.Error(writer, "device identity unavailable", http.StatusInternalServerError)
		return true
	}
	actor := task.ActorSnapshot{
		Type: task.ActorDevice, ID: device.ID, DisplayName: device.Name, CredentialID: device.ID,
		Authentication: "fern_device_cookie", RequestID: requestID,
	}
	request = request.WithContext(context.WithValue(request.Context(), csrfCredentialKey{}, credential))
	request = request.WithContext(taskapi.WithActor(request.Context(), actor))
	stripAllCookies(request)
	if isFernRoute(request) {
		request.Header.Del("Authorization")
	} else if auth.Password != "" {
		request.SetBasicAuth("opencode", auth.Password)
	} else {
		request.Header.Del("Authorization")
	}
	next.ServeHTTP(writer, request)
	return true
}

// operatorActor builds the audit identity stamped on loopback control-surface
// requests. Its credential identifier is the stable random value persisted by
// the control store — never anything derived from the control password — so no
// offline brute-force oracle of that secret reaches durable snapshots.
func (state *pairingState) operatorActor() (task.ActorSnapshot, error) {
	if state.operatorCredentialErr != nil {
		return task.ActorSnapshot{}, state.operatorCredentialErr
	}
	requestID, err := randomCredential()
	if err != nil {
		return task.ActorSnapshot{}, err
	}
	return task.ActorSnapshot{
		Type: task.ActorOperator, ID: "local-operator", DisplayName: "Local operator",
		CredentialID:   state.operatorCredentialID,
		Authentication: "basic", RequestID: requestID,
	}, nil
}

func stripAllCookies(request *http.Request) {
	request.Header.Del("Cookie")
}

func stripDeviceCookie(request *http.Request) {
	cookies := request.Cookies()
	request.Header.Del("Cookie")
	for _, cookie := range cookies {
		if cookie.Name != deviceCookieName && cookie.Name != legacyDeviceCookieName {
			request.AddCookie(cookie)
		}
	}
}

func (state *pairingState) prune(now time.Time) {
	for code, expiry := range state.codes {
		if !now.Before(expiry) {
			delete(state.codes, code)
		}
	}
	for session, expiry := range state.sessions {
		if !now.Before(expiry) {
			delete(state.sessions, session)
		}
	}
	for digest, attempt := range state.attempts {
		if !now.Before(attempt.ExpiresAt) {
			delete(state.attempts, digest)
		}
	}
	first := 0
	for first < len(state.invalidAttempts) && !now.Before(state.invalidAttempts[first].Add(pairingFailureWindow)) {
		first++
	}
	state.invalidAttempts = append([]time.Time(nil), state.invalidAttempts[first:]...)
}

func randomCredential() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
