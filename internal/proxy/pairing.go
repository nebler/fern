package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nebler/fern/internal/auth"
	"github.com/nebler/fern/internal/domain"
)

const (
	deviceCookieName       = "__Host-fern_device"
	maxDeviceNameBytes     = 80
	maxOutstandingPairings = 64
)

var pairingTemplate = newPage("pairing", "Pair with Fern", dialogCSS+`
h1{margin:24px 0 8px;font-size:34px;letter-spacing:-.04em}p{margin:0;color:#bdcbb5;font-size:16px;line-height:1.55}
label{display:block;margin-top:24px;color:#dce8d3;font-size:14px;font-weight:700}input[type=text]{display:block;width:100%;margin-top:8px;padding:13px 14px;border:1px solid #52664a;border-radius:13px;background:#11180f;color:#f3f7e9;font:inherit}button{display:block;width:100%;margin-top:24px;padding:15px 18px;border:0;border-radius:15px;background:#b9ef86;color:#15200f;font:inherit;font-weight:750;cursor:pointer}`,
	`<main><div class="mark">F</div><h1>Pair this phone?</h1><p>This gives this browser private access to your Fern workspace for 30 days.</p><form method="post" action="/fern/pair"><input type="hidden" name="code" value="{{.Code}}"><label for="device-name">Device name</label><input id="device-name" type="text" name="name" value="{{.Name}}" maxlength="80" autocomplete="nickname"><button type="submit">Pair this phone</button></form></main>`)

type pairingPage struct {
	Code string
	Name string
}

type pairingState struct {
	mu              sync.Mutex
	codes           map[[sha256.Size]byte]time.Time
	attempts        map[[sha256.Size]byte]pairingAttempt
	invalidAttempts []time.Time
	lastIssued      time.Time
	lastSuccess     time.Time
	now             func() time.Time
	store           *auth.DeviceStore
}

func newPairingState(store *auth.DeviceStore) *pairingState {
	return &pairingState{
		codes:    make(map[[sha256.Size]byte]time.Time),
		attempts: make(map[[sha256.Size]byte]pairingAttempt),
		now:      time.Now,
		store:    store,
	}
}

func (state *pairingState) issue(writer http.ResponseWriter, _ *http.Request) {
	now := state.now()
	state.mu.Lock()
	state.prune(now)
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
	code := auth.NewSecret()
	digest := sha256.Sum256([]byte(code))
	state.codes[digest] = now.Add(pairingCodeTTL)
	state.lastIssued = now
	state.mu.Unlock()
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]string{"code": code, "expiresIn": pairingCodeTTL.String()})
}

func (state *pairingState) pair(writer http.ResponseWriter, request *http.Request) {
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
	// Consume the one-time code before creating a durable device grant. If a
	// later effect fails, the operator issues a new code rather than risking
	// reuse after a lost response.
	delete(state.codes, hash)
	delete(state.attempts, hash)
	state.lastSuccess = now
	session := auth.NewSecret()
	_, pairErr := state.store.AddDevice(session, name, now, now.Add(deviceCredentialTTL))
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

// authenticate is the paired-device realm: a durable __Host- cookie whose
// request is registered against the device so revocation cancels it. A device
// revoked between authentication and registration is rejected as
// unauthenticated.
func (state *pairingState) authenticate(writer http.ResponseWriter, request *http.Request) (*http.Request, func(), bool) {
	cookie, err := request.Cookie(deviceCookieName)
	if err != nil || cookie.Value == "" {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return nil, nil, false
	}
	device, valid, err := state.store.AuthenticateDeviceIdentity(cookie.Value, state.now())
	if err != nil || !valid {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return nil, nil, false
	}
	return state.admit(writer, request, device, cookie.Value)
}

func (state *pairingState) admit(writer http.ResponseWriter, request *http.Request, device auth.Device, credential string) (*http.Request, func(), bool) {
	ctx, cancel := context.WithDeadline(request.Context(), device.ExpiresAt)
	unregister, admitted := state.store.RegisterDeviceRequest(device.ID, cancel)
	if !admitted {
		cancel()
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return nil, nil, false
	}
	release := func() {
		unregister()
		cancel()
	}
	actor := domain.ActorSnapshot{
		Type: domain.ActorDevice, ID: device.ID, DisplayName: device.Name, CredentialID: device.ID,
		Authentication: "fern_device_cookie", RequestID: rand.Text(),
	}
	ctx = context.WithValue(ctx, csrfCredentialKey{}, credential)
	request = request.WithContext(domain.WithActor(ctx, actor))
	stripCredentials(request)
	return request, release, true
}

func (state *pairingState) prune(now time.Time) {
	for code, expiry := range state.codes {
		if !now.Before(expiry) {
			delete(state.codes, code)
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
