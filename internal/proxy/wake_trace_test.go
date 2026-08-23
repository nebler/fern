package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/workspace"
)

type traceStubWaker struct {
	acquireN int
}

const traceTestImageID = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func (w *traceStubWaker) AcquireRequest(_ context.Context, _ workspace.RequestIntent) (workspace.RequestTarget, func(), error) {
	w.acquireN++
	return workspace.RequestTarget{
		Endpoint:   runtime.Endpoint{Host: "127.0.0.1", Port: 4096},
		ImageID:    traceTestImageID,
		Generation: 1,
	}, func() {}, nil
}

func (w *traceStubWaker) InvalidateEndpoint(workspace.RequestTarget) {}

func newWakeTraceTestHandlers(t *testing.T, last func() (workspace.WakeTrace, bool)) Handlers {
	t.Helper()
	waker := &traceStubWaker{}
	handlers, err := NewHandlers(waker, runtime.ServerAuth{}, Controls{
		WakeTrace:   NewWakeTraceHandler(waker, last, nil),
		ControlAuth: ControlAuth{Password: "control-password"},
	}, TrustedOrigins{Remote: "http://127.0.0.1:8080", Operator: "http://127.0.0.1:8081"}, nil)
	if err != nil {
		t.Fatalf("build handlers: %v", err)
	}
	return handlers
}

func TestWakeTracePostTriggersWakeAndReturnsSpans(t *testing.T) {
	handlers := newWakeTraceTestHandlers(t, func() (workspace.WakeTrace, bool) {
		return workspace.WakeTrace{Workspace: "demo", TotalMillis: 42, Spans: []workspace.TraceSpan{
			{Name: "runtime_total", OffsetMs: 0, Millis: 42},
		}}, true
	})
	request := httptest.NewRequest(http.MethodPost, "/fern/api/v1/debug/wake-trace", nil)
	request.SetBasicAuth("fern", "control-password")
	recorder := httptest.NewRecorder()
	handlers.Operator.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"runtime_total"`) || !strings.Contains(recorder.Body.String(), `"total_ms":42`) {
		t.Fatalf("unexpected trace body: %q", recorder.Body.String())
	}
}

func TestWakeTraceGetReturnsLastTrace(t *testing.T) {
	handlers := newWakeTraceTestHandlers(t, func() (workspace.WakeTrace, bool) { return workspace.WakeTrace{}, false })
	request := httptest.NewRequest(http.MethodGet, "/fern/api/v1/debug/wake-trace", nil)
	request.SetBasicAuth("fern", "control-password")
	recorder := httptest.NewRecorder()
	handlers.Operator.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("GET without a recorded wake: status = %d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestWakeTraceGatedOnRemoteSurface(t *testing.T) {
	handlers := newWakeTraceTestHandlers(t, func() (workspace.WakeTrace, bool) { return workspace.WakeTrace{}, true })
	request := httptest.NewRequest(http.MethodPost, "/fern/api/v1/debug/wake-trace", nil)
	recorder := httptest.NewRecorder()
	handlers.Remote.ServeHTTP(recorder, request)
	// The remote listener rejects everything without a paired-device grant
	// before route dispatch, so an unpaired caller must see 401 — never the
	// diagnostic. Post-authentication absence is enforced by the remote
	// Controls carrying no WakeTrace handler (nil resolves to 404).
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("remote surface status = %d, want 401", recorder.Code)
	}
}

func TestWakeTraceRejectsWrongControlCredential(t *testing.T) {
	handlers := newWakeTraceTestHandlers(t, func() (workspace.WakeTrace, bool) { return workspace.WakeTrace{}, true })
	request := httptest.NewRequest(http.MethodPost, "/fern/api/v1/debug/wake-trace", nil)
	request.SetBasicAuth("fern", "wrong-password")
	recorder := httptest.NewRecorder()
	handlers.Operator.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d want 401", recorder.Code)
	}
}

func TestWakeTraceRejectsUnsupportedMethod(t *testing.T) {
	handlers := newWakeTraceTestHandlers(t, func() (workspace.WakeTrace, bool) { return workspace.WakeTrace{}, true })
	request := httptest.NewRequest(http.MethodPut, "/fern/api/v1/debug/wake-trace", nil)
	request.SetBasicAuth("fern", "control-password")
	recorder := httptest.NewRecorder()
	handlers.Operator.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d want 405", recorder.Code)
	}
}
