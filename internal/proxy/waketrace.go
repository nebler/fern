package proxy

import (
	"log/slog"
	"net/http"

	"github.com/nebler/fern/internal/workspace"
)

// NewWakeTraceHandler serves the operator-only wake-trace diagnostic.
//
// POST performs one synchronous work-intent request through the normal
// admission and wake path, then reports the phases recorded for that coalesced
// wake. GET reports the most recent completed wake without triggering one. The
// handler must only be mounted on the operator listener; on any other surface
// leave Controls.WakeTrace nil so the route resolves to 404.
func NewWakeTraceHandler(waker Waker, last func() (workspace.WakeTrace, bool), log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPost:
			if waker == nil {
				http.Error(writer, "wake tracing unavailable", http.StatusServiceUnavailable)
				return
			}
			_, release, err := waker.AcquireRequest(request.Context(), workspace.RequestWork)
			if err != nil {
				log.Error("wake trace failed", "err", err)
				http.Error(writer, "failed to wake workspace", http.StatusServiceUnavailable)
				return
			}
			release()
		case http.MethodGet:
		default:
			methodNotAllowed(writer, "GET, POST")
			return
		}
		trace, ok := last()
		if !ok {
			writeJSONStatus(writer, http.StatusNotFound, map[string]string{
				"error": "no completed wake recorded; the workspace may already be running",
			}, nil)
			return
		}
		writeJSON(writer, trace, nil)
	})
}
