package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/nebler/fern/internal/runtime"
)

const deviceCookieName = "fern_device"

type pairingState struct {
	mu       sync.Mutex
	codes    map[[sha256.Size]byte]time.Time
	sessions map[[sha256.Size]byte]time.Time
	now      func() time.Time
}

func newPairingState() *pairingState {
	return &pairingState{
		codes:    make(map[[sha256.Size]byte]time.Time),
		sessions: make(map[[sha256.Size]byte]time.Time),
		now:      time.Now,
	}
}

func (state *pairingState) handler(next http.Handler, auth runtime.ServerAuth) http.Handler {
	basic := requireServerAuth(next, auth)
	issue := requireServerAuth(http.HandlerFunc(state.issue), auth)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/fern/pair/new" && request.URL.EscapedPath() == "/fern/pair/new":
			issue.ServeHTTP(writer, request)
		case request.URL.Path == "/fern/pair" && request.URL.EscapedPath() == "/fern/pair":
			state.pair(writer, request)
		case state.authenticated(request):
			stripDeviceCookie(request)
			if auth.Password != "" {
				request.SetBasicAuth("opencode", auth.Password)
			}
			next.ServeHTTP(writer, request)
		default:
			basic.ServeHTTP(writer, request)
		}
	})
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
	if valid {
		delete(state.codes, hash)
	}
	valid = valid && code != "" && now.Before(expires)
	var session string
	if valid {
		var err error
		session, err = randomCredential()
		valid = err == nil
		if valid {
			state.sessions[sha256.Sum256([]byte(session))] = now.Add(30 * 24 * time.Hour)
		}
	}
	state.prune(now)
	state.mu.Unlock()
	if !valid {
		http.Error(writer, "pairing link is invalid or expired", http.StatusUnauthorized)
		return
	}
	http.SetCookie(writer, &http.Cookie{
		Name: deviceCookieName, Value: session, Path: "/", MaxAge: 30 * 24 * 60 * 60,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(writer, request, "/fern/", http.StatusSeeOther)
}

func (state *pairingState) authenticated(request *http.Request) bool {
	cookie, err := request.Cookie(deviceCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	now := state.now()
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
