package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/nebler/fern/internal/control"
)

var deviceRevokedTemplate = newPage("device-revoked", "Device revoked", dialogCSS+`
h1{margin:24px 0 8px;font-size:34px;letter-spacing:-.04em}p{margin:0;color:#bdcbb5;font-size:16px;line-height:1.55}`,
	`<main><div class="mark">F</div><h1>Device revoked</h1><p>This browser may now be closed.</p></main>`)

type deviceControls struct{ store *control.Store }

func (devices deviceControls) list(writer http.ResponseWriter, _ *http.Request) {
	list, err := devices.store.Devices(time.Now())
	writeJSON(writer, list, err)
}

func (devices deviceControls) revoke(writer http.ResponseWriter, request *http.Request) {
	if err := devices.revokeID(request.PathValue("id")); errors.Is(err, os.ErrNotExist) {
		http.NotFound(writer, request)
		return
	} else if err != nil {
		writeUnavailable(writer, "control state")
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// revokeFromPage is the control page's form post; revoking an already absent
// device still renders the confirmation.
func (devices deviceControls) revokeFromPage(writer http.ResponseWriter, request *http.Request) {
	if err := devices.revokeID(request.PathValue("id")); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeUnavailable(writer, "control state")
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = deviceRevokedTemplate.Execute(writer, nil)
}

// revokeID persists the revocation before cancelling the device's in-flight
// requests.
func (devices deviceControls) revokeID(id string) error {
	if err := devices.store.RevokeDevice(id); err != nil {
		return err
	}
	devices.store.CancelDeviceRequests(id)
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
	trusted, ok := request.Context().Value(originKey{}).(trustedOrigin)
	return ok && origin == trusted.raw
}
