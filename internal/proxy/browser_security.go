package proxy

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nebler/fern/internal/control"
)

const (
	csrfHeaderName            = "X-Fern-CSRF-Token"
	csrfTokenPath             = "/fern/api/v1/csrf"
	csrfTokenTTL              = 10 * time.Minute
	pairingCodeTTL            = 5 * time.Minute
	deviceCredentialTTL       = 30 * 24 * time.Hour
	pairingFailureWindow      = 5 * time.Minute
	pairingIssueInterval      = time.Second
	pairingSuccessInterval    = time.Second
	maxGlobalPairingFailures  = 32
	maxPairingCodeAttempts    = 5
	maxPersistedPairingBytes  = 64 << 10
	pairingPersistenceVersion = 1
)

type csrfCredentialKey struct{}

type pairingAttempt struct {
	Count     int
	ExpiresAt time.Time
}

type persistedPairingState struct {
	Version         int                                `json:"version"`
	Codes           map[string]time.Time               `json:"codes"`
	Attempts        map[string]persistedPairingAttempt `json:"attempts,omitempty"`
	InvalidAttempts []time.Time                        `json:"invalidAttempts,omitempty"`
	LastIssued      time.Time                          `json:"lastIssued,omitempty"`
	LastSuccess     time.Time                          `json:"lastSuccess,omitempty"`
}

type persistedPairingAttempt struct {
	Count     int       `json:"count"`
	ExpiresAt time.Time `json:"expiresAt"`
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
	_, _ = mac.Write([]byte(csrfMessage(expiry, method, csrfRoute(path))))
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
	_, _ = mac.Write([]byte(csrfMessage(expiryText, method, csrfRoute(path))))
	return subtle.ConstantTimeCompare(provided, mac.Sum(nil)) == 1
}

func csrfMessage(expiry, method, route string) string {
	return "fern-csrf-v1\n" + expiry + "\n" + method + "\n" + route
}

func csrfRoute(path string) string {
	if strings.HasPrefix(path, "/fern/api/v1/results/") && strings.HasSuffix(path, "/publications") {
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/fern/api/v1/results/"), "/publications")
		if id != "" && !strings.Contains(id, "/") {
			return "/fern/api/v1/results/:id/publications"
		}
	}
	if strings.HasPrefix(path, "/fern/api/v1/tasks/") && strings.HasSuffix(path, "/cancel") {
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/fern/api/v1/tasks/"), "/cancel")
		if id != "" && !strings.Contains(id, "/") {
			return "/fern/api/v1/tasks/:id/cancel"
		}
	}
	return path
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

func pairingPersistencePath(store *control.Store) string {
	if store == nil {
		return ""
	}
	path, err := store.AuxiliaryStatePath("pairing")
	if err != nil {
		return ""
	}
	return path
}

func (state *pairingState) loadPersisted() error {
	if state.persistencePath == "" {
		return nil
	}
	file, err := os.Open(state.persistencePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPersistedPairingBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxPersistedPairingBytes {
		return errors.New("pairing limiter state is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var persisted persistedPairingState
	if err := decoder.Decode(&persisted); err != nil {
		return fmt.Errorf("read pairing limiter state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("invalid trailing pairing limiter state")
	}
	if persisted.Version != pairingPersistenceVersion {
		return errors.New("unsupported pairing limiter state version")
	}
	if len(persisted.Codes) > maxOutstandingPairings || len(persisted.InvalidAttempts) > maxGlobalPairingFailures || len(persisted.Attempts) > maxGlobalPairingFailures+maxOutstandingPairings {
		return errors.New("pairing limiter state exceeds bounds")
	}
	for encoded, expiry := range persisted.Codes {
		digest, ok := decodePairingDigest(encoded)
		if !ok {
			return errors.New("invalid pairing code digest")
		}
		state.codes[digest] = expiry
	}
	for encoded, persistedAttempt := range persisted.Attempts {
		digest, ok := decodePairingDigest(encoded)
		if !ok || persistedAttempt.Count < 1 || persistedAttempt.Count > maxPairingCodeAttempts || persistedAttempt.ExpiresAt.IsZero() {
			return errors.New("invalid pairing attempt state")
		}
		state.attempts[digest] = pairingAttempt{Count: persistedAttempt.Count, ExpiresAt: persistedAttempt.ExpiresAt}
	}
	state.invalidAttempts = append([]time.Time(nil), persisted.InvalidAttempts...)
	state.lastIssued = persisted.LastIssued
	state.lastSuccess = persisted.LastSuccess
	state.prune(state.now())
	return nil
}

func (state *pairingState) persistLocked() error {
	if state.persistencePath == "" {
		return nil
	}
	persisted := persistedPairingState{
		Version: pairingPersistenceVersion, Codes: make(map[string]time.Time, len(state.codes)),
		Attempts:        make(map[string]persistedPairingAttempt, len(state.attempts)),
		InvalidAttempts: state.invalidAttempts, LastIssued: state.lastIssued, LastSuccess: state.lastSuccess,
	}
	for digest, expiry := range state.codes {
		persisted.Codes[hex.EncodeToString(digest[:])] = expiry
	}
	for digest, attempt := range state.attempts {
		persisted.Attempts[hex.EncodeToString(digest[:])] = persistedPairingAttempt{Count: attempt.Count, ExpiresAt: attempt.ExpiresAt}
	}
	directory := filepath.Dir(state.persistencePath)
	temporary, err := os.CreateTemp(directory, ".pairing-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err == nil {
		err = json.NewEncoder(temporary).Encode(persisted)
	}
	if err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporaryName, state.persistencePath)
	}
	if err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr = dir.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func decodePairingDigest(encoded string) ([sha256.Size]byte, bool) {
	var digest [sha256.Size]byte
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != len(digest) {
		return digest, false
	}
	copy(digest[:], decoded)
	return digest, true
}
