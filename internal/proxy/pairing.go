package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/runtime"
)

const deviceCookieName = "fern_device"

type pairingState struct {
	mu       sync.Mutex
	codes    map[[sha256.Size]byte]time.Time
	sessions map[[sha256.Size]byte]time.Time
	now      func() time.Time
	store    *control.Store
}

func newPairingState(stores ...*control.Store) *pairingState {
	state := &pairingState{
		codes:    make(map[[sha256.Size]byte]time.Time),
		sessions: make(map[[sha256.Size]byte]time.Time),
		now:      time.Now,
	}
	if len(stores) != 0 {
		state.store = stores[0]
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
		case state.authenticated(request):
			stripDeviceCookie(request)
			if auth.Password != "" {
				request.SetBasicAuth("opencode", auth.Password)
			} else {
				request.Header.Del("Authorization")
			}
			next.ServeHTTP(writer, request)
		default:
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
	code, err := randomCredential()
	if err != nil {
		http.Error(writer, "failed to create pairing code", http.StatusInternalServerError)
		return
	}
	now := state.now()
	state.mu.Lock()
	state.prune(now)
	state.codes[sha256.Sum256([]byte(code))] = now.Add(5 * time.Minute)
	state.mu.Unlock()
	setFernHeaders(writer.Header())
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]string{"code": code, "expiresIn": "5m"})
}

func (state *pairingState) pair(writer http.ResponseWriter, request *http.Request) {
	setFernHeaders(writer.Header())
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", "GET")
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	code := request.URL.Query().Get("code")
	hash := sha256.Sum256([]byte(code))
	now := state.now()
	state.mu.Lock()
	expires, valid := state.codes[hash]
	valid = valid && code != "" && now.Before(expires)
	if !valid {
		delete(state.codes, hash)
	}
	var session string
	var pairErr error
	if valid {
		session, pairErr = randomCredential()
		valid = pairErr == nil
		if valid {
			if state.store == nil {
				state.sessions[sha256.Sum256([]byte(session))] = now.Add(30 * 24 * time.Hour)
			} else {
				_, pairErr = state.store.AddDevice(session, request.URL.Query().Get("name"), now, now.Add(30*24*time.Hour))
				valid = pairErr == nil
			}
			if valid {
				delete(state.codes, hash)
			}
		}
	}
	state.prune(now)
	state.mu.Unlock()
	if pairErr != nil {
		http.Error(writer, "pairing state unavailable", http.StatusServiceUnavailable)
		return
	}
	if !valid {
		http.Error(writer, "pairing link is invalid or expired", http.StatusUnauthorized)
		return
	}
	http.SetCookie(writer, &http.Cookie{
		Name: deviceCookieName, Value: session, Path: "/", MaxAge: 30 * 24 * 60 * 60,
		Expires: now.Add(30 * 24 * time.Hour), HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(writer, request, "/fern/", http.StatusSeeOther)
}

func (state *pairingState) authenticated(request *http.Request) bool {
	cookie, err := request.Cookie(deviceCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	now := state.now()
	if state.store != nil {
		valid, err := state.store.AuthenticateDevice(cookie.Value, now)
		return err == nil && valid
	}
	hash := sha256.Sum256([]byte(cookie.Value))
	state.mu.Lock()
	expires, valid := state.sessions[hash]
	if valid && !now.Before(expires) {
		delete(state.sessions, hash)
		valid = false
	}
	state.mu.Unlock()
	return valid
}

func stripDeviceCookie(request *http.Request) {
	cookies := request.Cookies()
	request.Header.Del("Cookie")
	for _, cookie := range cookies {
		if cookie.Name != deviceCookieName {
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
}

func randomCredential() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
