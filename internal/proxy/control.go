package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/publication"
	"github.com/nebler/fern/internal/workspace"
)

func serveControlRoute(writer http.ResponseWriter, request *http.Request, controls Controls, publicationEnabled bool) bool {
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
	if path == "/fern/api/v1/workflows" {
		if store == nil {
			writeUnavailable(writer, "control store")
			return true
		}
		switch request.Method {
		case http.MethodGet:
			writeJSON(writer, store.Workflows(), nil)
		case http.MethodPost:
			if mediaType, _, _ := mime.ParseMediaType(request.Header.Get("Content-Type")); mediaType != "application/json" {
				http.Error(writer, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return true
			}
			var input struct {
				Title     string `json:"title"`
				SessionID string `json:"sessionId"`
			}
			if err := decodeControlJSON(writer, request, &input); err != nil {
				return true
			}
			workflow, err := store.CreateWorkflow(input.Title, input.SessionID, time.Now())
			if err != nil {
				// 422 validation feedback: this error text is user-facing by
				// design and names the rejected workflow input.
				http.Error(writer, err.Error(), http.StatusUnprocessableEntity)
				return true
			}
			writeJSONStatus(writer, http.StatusCreated, workflow, nil)
		default:
			methodNotAllowed(writer, "GET, POST")
		}
		return true
	}
	if path == "/fern/api/v1/publications" {
		if store == nil {
			writeUnavailable(writer, "control store")
			return true
		}
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, "GET")
			return true
		}
		writeJSON(writer, store.Publications(), nil)
		return true
	}
	if strings.HasPrefix(path, "/fern/api/v1/workflows/") && strings.HasSuffix(path, "/publish") {
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/fern/api/v1/workflows/"), "/publish")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(writer, request)
			return true
		}
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, "POST")
			return true
		}
		publishWorkflow(writer, request, controls, publicationEnabled, id, true)
		return true
	}
	if strings.HasPrefix(path, "/fern/api/v1/workflows/") {
		if store == nil {
			writeUnavailable(writer, "control store")
			return true
		}
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, "GET")
			return true
		}
		id := strings.TrimPrefix(path, "/fern/api/v1/workflows/")
		workflow, exists := store.Workflow(id)
		if !exists || strings.Contains(id, "/") {
			http.NotFound(writer, request)
			return true
		}
		writeJSON(writer, workflow, nil)
		return true
	}
	if strings.HasPrefix(path, "/fern/workflows/") && strings.HasSuffix(path, "/publish") {
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/fern/workflows/"), "/publish")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(writer, request)
			return true
		}
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, "POST")
			return true
		}
		publishWorkflow(writer, request, controls, publicationEnabled, id, false)
		return true
	}
	if path == "/fern/workflows" && request.Method == http.MethodPost {
		if store == nil {
			writeUnavailable(writer, "control store")
			return true
		}
		request.Body = http.MaxBytesReader(writer, request.Body, 16<<10)
		if err := request.ParseForm(); err != nil {
			http.Error(writer, "invalid workflow form", http.StatusBadRequest)
			return true
		}
		if _, err := store.CreateWorkflow(request.FormValue("title"), request.FormValue("sessionId"), time.Now()); err != nil {
			// 422 validation feedback: this error text is user-facing by design
			// and names the rejected workflow form.
			http.Error(writer, err.Error(), http.StatusUnprocessableEntity)
			return true
		}
		http.Redirect(writer, request, "/fern/control", http.StatusSeeOther)
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

func publishWorkflow(writer http.ResponseWriter, request *http.Request, controls Controls, enabled bool, workflowID string, jsonRequest bool) {
	if !enabled {
		http.Error(writer, "publication requires configured Fern authentication and GitHub repository binding", http.StatusServiceUnavailable)
		return
	}
	if controls.Store == nil || controls.Publications == nil {
		writeUnavailable(writer, "publication")
		return
	}
	workflow, exists := controls.Store.Workflow(workflowID)
	if !exists {
		http.NotFound(writer, request)
		return
	}
	var input struct {
		Operation string `json:"operation"`
		Base      string `json:"base"`
		Title     string `json:"title"`
		Body      string `json:"body"`
	}
	var publicationRecord control.Publication
	if workflow.PublicationID != "" {
		publicationRecord, exists = controls.Store.Publication(workflow.PublicationID)
		if !exists {
			writeUnavailable(writer, "publication state")
			return
		}
		if publicationRecord.State == control.PublicationPublished {
			writePublicationResponse(writer, request, publicationRecord, jsonRequest)
			return
		}
	} else {
		if jsonRequest {
			if mediaType, _, _ := mime.ParseMediaType(request.Header.Get("Content-Type")); mediaType != "application/json" {
				http.Error(writer, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
			if err := decodeControlJSON(writer, request, &input); err != nil {
				return
			}
		}
		if strings.TrimSpace(input.Title) == "" {
			input.Title = workflow.Title
		}
		id, err := randomCredential()
		if err != nil {
			http.Error(writer, "create publication operation", http.StatusInternalServerError)
			return
		}
		if input.Operation == "" {
			input.Operation = "op-" + id[:13]
		}
		if err := publication.ValidateRequest(publication.Request{
			Operation: input.Operation, Base: input.Base, Title: input.Title, Body: input.Body,
		}); err != nil {
			// 422 validation feedback: this error text is user-facing by design
			// and names the rejected publication request.
			http.Error(writer, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		publicationRecord, _, err = controls.Store.RequestPublication(workflowID, control.Publication{
			ID: id, Operation: input.Operation, RequestedBaseRef: input.Base, Title: input.Title, Body: input.Body,
		}, time.Now())
		if err != nil {
			http.Error(writer, "record publication request", http.StatusUnprocessableEntity)
			return
		}
	}
	publicationRecord, err := controls.Publications.Execute(request.Context(), publicationRecord.ID)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, workspace.ErrRequestsActive) || errors.Is(err, workspace.ErrSessionsActive) {
			status = http.StatusConflict
		} else if errors.Is(err, publication.ErrRunning) {
			status = http.StatusConflict
		}
		http.Error(writer, "publication is pending or failed and remains retryable", status)
		return
	}
	writePublicationResponse(writer, request, publicationRecord, jsonRequest)
}

func writePublicationResponse(writer http.ResponseWriter, request *http.Request, publicationRecord control.Publication, jsonRequest bool) {
	if jsonRequest {
		writeJSON(writer, publicationRecord, nil)
		return
	}
	http.Redirect(writer, request, "/fern/control", http.StatusSeeOther)
}

func decodeControlJSON(writer http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, 16<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		http.Error(writer, "invalid JSON body", http.StatusBadRequest)
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		http.Error(writer, "invalid trailing JSON", http.StatusBadRequest)
		return errors.New("trailing JSON")
	}
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
