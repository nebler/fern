package proxy

import (
	"log/slog"
	"net/http"

	"github.com/nebler/fern/internal/runtime"
)

// New returns the legacy combined test handler. Production startup must use
// NewHandlers so backend Basic authentication is never enabled remotely.
func New(waker Waker, auth runtime.ServerAuth, log *slog.Logger) http.Handler {
	return legacyOriginHandler(newHandler(waker, auth, Controls{}, log))
}

// NewWithControls returns the legacy combined test handler. Production startup
// must use NewHandlers.
func NewWithControls(waker Waker, auth runtime.ServerAuth, controls Controls, log *slog.Logger) http.Handler {
	return legacyOriginHandler(newHandler(waker, auth, controls, log))
}
