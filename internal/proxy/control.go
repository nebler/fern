package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nebler/fern/internal/control"
)

func serveControlRoute(writer http.ResponseWriter, request *http.Request, controls Controls) bool {
	store := controls.Store
	path := request.URL.Path
	if request.URL.EscapedPath() != path {
		if strings.HasPrefix(path, "/fern/api/") || strings.HasPrefix(path, "/fern/devices/") || path == "/fern/workflows" {
			http.NotFound(writer, request)
			return true
		}
		return false
	}
	mutation := request.Method == http.MethodPost || request.Method == http.MethodDelete || request.Method == http.MethodPatch || request.Method == http.MethodPut
	controlPath := strings.HasPrefix(path, "/fern/api/v1/") || strings.HasPrefix(path, "/fern/workflows") || strings.HasPrefix(path, "/fern/devices/")
	if mutation && controlPath && !sameOrigin(request) {
		http.Error(writer, "cross-origin control request rejected", http.StatusForbidden)
		return true
	}
	if retiredLegacyControlPath(path) {
		http.Error(writer, "legacy workflow and publication control is retired", http.StatusGone)
		return true
	}
	if isTaskAPIPath(path) {
		if controls.Tasks == nil {
			http.NotFound(writer, request)
			return true
		}
		controls.Tasks.ServeHTTP(writer, request)
		return true
	}
	if path == "/fern/api/v1/debug/wake-trace" {
		// Operator-only diagnostic. The remote surface builds Controls without
		// WakeTrace, so the route is absent there rather than exposed.
		if controls.WakeTrace == nil {
			http.NotFound(writer, request)
			return true
		}
		if request.Method != http.MethodPost && request.Method != http.MethodGet {
			methodNotAllowed(writer, "GET, POST")
			return true
		}
		controls.WakeTrace.ServeHTTP(writer, request)
		return true
	}
	if path == "/fern/status" || path == "/fern/metrics" {
		handler := controls.Status
		if path == "/fern/metrics" {
			handler = controls.Metrics
		}
		if handler == nil {
			http.NotFound(writer, request)
			return true
		}
		handler.ServeHTTP(writer, request)
		return true
	}
	if path == "/fern/api/v1/devices" {
		if store == nil {
			writeUnavailable(writer, "control store")
			return true
		}
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, "GET")
			return true
		}
		devices, err := store.Devices(time.Now())
		writeJSON(writer, devices, err)
		return true
	}
	if strings.HasPrefix(path, "/fern/api/v1/devices/") {
		if store == nil {
			writeUnavailable(writer, "control store")
			return true
		}
		if request.Method != http.MethodDelete {
			methodNotAllowed(writer, "DELETE")
			return true
		}
		id := strings.TrimPrefix(path, "/fern/api/v1/devices/")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(writer, request)
			return true
		}
		if err := revokeDevice(store, id, store.CancelDeviceRequests); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.NotFound(writer, request)
			} else {
				writeUnavailable(writer, "control state")
			}
			return true
		}
		writer.WriteHeader(http.StatusNoContent)
		return true
	}
	if strings.HasPrefix(path, "/fern/devices/") && strings.HasSuffix(path, "/revoke") && request.Method == http.MethodPost {
		if store == nil {
			writeUnavailable(writer, "control store")
			return true
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/fern/devices/"), "/revoke")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(writer, request)
			return true
		}
		if err := revokeDevice(store, id, store.CancelDeviceRequests); err != nil && !errors.Is(err, os.ErrNotExist) {
			writeUnavailable(writer, "control state")
			return true
		}
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(writer, `<!doctype html><html lang="en"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Device revoked</title><body><main><h1>Device revoked</h1><p>This browser may now be closed.</p></main></body></html>`)
		return true
	}
	return false
}

func retiredLegacyControlPath(path string) bool {
	if path == "/fern/api/v1/workflows" || path == "/fern/api/v1/publications" || path == "/fern/workflows" {
		return true
	}
	if suffix, found := strings.CutPrefix(path, "/fern/api/v1/workflows/"); found {
		parts := strings.Split(suffix, "/")
		return parts[0] != "" && (len(parts) == 1 || len(parts) == 2 && parts[1] == "publish")
	}
	if suffix, found := strings.CutPrefix(path, "/fern/workflows/"); found {
		parts := strings.Split(suffix, "/")
		return len(parts) == 2 && parts[0] != "" && parts[1] == "publish"
	}
	return false
}

func isTaskAPIPath(path string) bool {
	return path == "/fern/api/v1/tasks" || path == "/fern/api/v1/events" || strings.HasPrefix(path, "/fern/api/v1/tasks/")
}

func revokeDevice(store *control.Store, id string, onRevoked func(string)) error {
	if err := store.RevokeDevice(id); err != nil {
		return err
	}
	onRevoked(id)
	return nil
}

func writeJSON(writer http.ResponseWriter, value any, err error) {
	writeJSONStatus(writer, http.StatusOK, value, err)
}

func writeJSONStatus(writer http.ResponseWriter, status int, value any, err error) {
	if err != nil {
		writeUnavailable(writer, "control state")
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	var buffer bytes.Buffer
	if encodeErr := json.NewEncoder(&buffer).Encode(value); encodeErr != nil {
		http.Error(writer, "encode control response", http.StatusInternalServerError)
		return
	}
	writer.WriteHeader(status)
	_, _ = writer.Write(buffer.Bytes())
}

// writeUnavailable answers 503 with the single "<scope> unavailable" vocabulary
// shared by every Fern gateway dependency failure, so clients and tests can
// match one family of messages instead of per-route phrasings.
func writeUnavailable(writer http.ResponseWriter, scope string) {
	http.Error(writer, scope+" unavailable", http.StatusServiceUnavailable)
}

func sameOrigin(request *http.Request) bool {
	// Modern browsers provide Fetch Metadata even when Origin is omitted. A
	// same-site sibling is not equivalent to this exact private Fern origin.
	if site := request.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if trusted, ok := request.Context().Value(originKey{}).(trustedOrigin); ok && !trusted.legacy {
		return origin == trusted.raw
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return (parsed.Scheme == "https" || parsed.Scheme == "http") && strings.EqualFold(parsed.Host, request.Host)
}

func methodNotAllowed(writer http.ResponseWriter, allow string) {
	writer.Header().Set("Allow", allow)
	http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
}
