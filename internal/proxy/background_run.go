package proxy

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/nebler/fern/internal/control"
)

// NewPairedBackgroundRunHandler applies only durable paired-device
// authentication and browser same-origin checks to a disposable run proxy. It
// deliberately exposes no Fern control, task, plugin, or pairing routes.
func NewPairedBackgroundRunHandler(store *control.Store, origin string, next http.Handler) (http.Handler, error) {
	if store == nil || next == nil {
		return nil, errors.New("paired background run store and handler are required")
	}
	trusted, err := parseTrustedOrigin(origin)
	if err != nil || trusted.scheme != "https" || trusted.port == "443" {
		return nil, errors.New("canonical private HTTPS background run origin with a non-443 port is required")
	}
	state := newPairingState(store)
	paired := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		if isFernRoute(request) {
			http.NotFound(writer, request)
			return
		}
		device, credential, valid := state.authenticatedDevice(request)
		if !valid {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		if dedicatedOriginCheckRequired(request) && !dedicatedSameOrigin(request, trusted.raw) {
			http.Error(writer, "cross-origin run request rejected", http.StatusForbidden)
			return
		}
		state.servePaired(writer, request, next, device, credential)
	})
	return trustedOriginHandler(paired, trusted), nil
}

func dedicatedOriginCheckRequired(request *http.Request) bool {
	return isMutation(request) || strings.EqualFold(request.Header.Get("Upgrade"), "websocket")
}

func dedicatedSameOrigin(request *http.Request, origin string) bool {
	if request.Header.Get("Sec-Fetch-Site") != "same-origin" {
		return false
	}
	if values := request.Header.Values("Origin"); len(values) != 0 {
		return len(values) == 1 && values[0] == origin
	}
	values := request.Header.Values("Referer")
	if len(values) != 1 {
		return false
	}
	referer, err := url.Parse(values[0])
	if err != nil || referer.User != nil {
		return false
	}
	return referer.Scheme+"://"+referer.Host == origin
}
