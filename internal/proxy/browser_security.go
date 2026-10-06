package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	csrfHeaderName           = "X-Fern-CSRF-Token"
	csrfTokenPath            = "/fern/api/v1/csrf"
	csrfTokenTTL             = 10 * time.Minute
	pairingCodeTTL           = 5 * time.Minute
	deviceCredentialTTL      = 30 * 24 * time.Hour
	pairingFailureWindow     = 5 * time.Minute
	pairingIssueInterval     = time.Second
	pairingSuccessInterval   = time.Second
	maxGlobalPairingFailures = 32
	maxPairingCodeAttempts   = 5
)

type csrfCredentialKey struct{}

type pairingAttempt struct {
	Count     int
	ExpiresAt time.Time
}

func isMutation(request *http.Request) bool {
	return request.Method != http.MethodGet && request.Method != http.MethodHead && request.Method != http.MethodOptions
}

func (state *pairingState) authorizeDeviceMutation(writer http.ResponseWriter, request *http.Request, credential string) bool {
	if !isMutation(request) {
		return true
	}
	if !sameOrigin(request) {
		http.Error(writer, "cross-origin device request rejected", http.StatusForbidden)
		return false
	}
	if !validCSRFToken(request.Header.Get(csrfHeaderName), credential, request.Method, request.URL.EscapedPath(), state.now()) {
		http.Error(writer, "invalid device CSRF token", http.StatusForbidden)
		return false
	}
	return true
}

func (state *pairingState) serveCSRFToken(writer http.ResponseWriter, request *http.Request, credential string) {
	setFernHeaders(writer.Header())
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, "GET")
		return
	}
	method := strings.ToUpper(request.URL.Query().Get("method"))
	path := request.URL.Query().Get("path")
	values := request.URL.Query()
	if len(values) != 2 || len(values["method"]) != 1 || len(values["path"]) != 1 || !validCSRFMethod(method) ||
		len(path) == 0 || len(path) > 2048 || path[0] != '/' || strings.ContainsAny(path, "\r\n?#") {
		http.Error(writer, "invalid CSRF token target", http.StatusBadRequest)
		return
	}
	token := mintCSRFToken(credential, method, path, state.now().Add(csrfTokenTTL))
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]string{"token": token})
}

func validCSRFMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func mintCSRFToken(credential, method, path string, expires time.Time) string {
	expiry := strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(credential))
	_, _ = mac.Write([]byte(csrfMessage(expiry, method, path)))
	return expiry + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func validCSRFToken(token, credential, method, path string, now time.Time) bool {
	expiryText, signatureText, found := strings.Cut(token, ".")
	if !found || expiryText == "" || signatureText == "" || strings.Contains(signatureText, ".") {
		return false
	}
	expiryUnix, err := strconv.ParseInt(expiryText, 10, 64)
	if err != nil {
		return false
	}
	expires := time.Unix(expiryUnix, 0)
	if !now.Before(expires) || expires.After(now.Add(csrfTokenTTL)) {
		return false
	}
	provided, err := base64.RawURLEncoding.Strict().DecodeString(signatureText)
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, []byte(credential))
	_, _ = mac.Write([]byte(csrfMessage(expiryText, method, path)))
	return subtle.ConstantTimeCompare(provided, mac.Sum(nil)) == 1
}

func csrfMessage(expiry, method, route string) string {
	return "fern-csrf-v1\n" + expiry + "\n" + method + "\n" + route
}

func (state *pairingState) recordInvalidLocked(digest [sha256.Size]byte, now time.Time) {
	attempt := state.attempts[digest]
	if attempt.ExpiresAt.IsZero() || !now.Before(attempt.ExpiresAt) {
		attempt = pairingAttempt{ExpiresAt: now.Add(pairingFailureWindow)}
	}
	if attempt.Count < maxPairingCodeAttempts {
		attempt.Count++
	}
	state.attempts[digest] = attempt
	state.invalidAttempts = append(state.invalidAttempts, now.UTC())
}
