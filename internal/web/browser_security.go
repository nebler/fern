package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
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

// validDeviceCSRF checks a device mutation's token against the credential the
// device realm installed, the method, and the exact escaped path.
func validDeviceCSRF(request *http.Request) bool {
	credential, _ := request.Context().Value(csrfCredentialKey{}).(string)
	return credential != "" &&
		validCSRFToken(request.Header.Get(csrfHeaderName), credential, request.Method, request.URL.EscapedPath(), time.Now())
}

func serveCSRFToken(writer http.ResponseWriter, request *http.Request) {
	credential, _ := request.Context().Value(csrfCredentialKey{}).(string)
	values := request.URL.Query()
	method, path := strings.ToUpper(values.Get("method")), values.Get("path")
	if credential == "" || !exactQuery(values, "method", "path") || !validCSRFMethod(method) || !validCSRFPath(path) {
		http.Error(writer, "invalid CSRF token target", http.StatusBadRequest)
		return
	}
	token := mintCSRFToken(credential, method, path, time.Now().Add(csrfTokenTTL))
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]string{"token": token})
}

// exactQuery reports whether values holds exactly keys, each once and non-empty.
func exactQuery(values url.Values, keys ...string) bool {
	if len(values) != len(keys) {
		return false
	}
	for _, key := range keys {
		if len(values[key]) != 1 || values[key][0] == "" {
			return false
		}
	}
	return true
}

// validCSRFPath accepts a bounded absolute path without a query, fragment, or
// line break.
func validCSRFPath(path string) bool {
	return len(path) <= 2048 && strings.HasPrefix(path, "/") && !strings.ContainsAny(path, "\r\n?#")
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
