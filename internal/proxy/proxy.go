package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/workspace"
)

type Waker interface {
	AcquireRequest(context.Context, workspace.RequestIntent) (workspace.RequestTarget, func(), error)
	InvalidateEndpoint(workspace.RequestTarget)
}

type targetKey struct{}

type proxyTarget struct {
	url     *url.URL
	request workspace.RequestTarget
	intent  workspace.RequestIntent
}

func New(waker Waker, auth runtime.ServerAuth, log *slog.Logger) http.Handler {
	if waker == nil {
		unavailable := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, "workspace manager unavailable", http.StatusServiceUnavailable)
		})
		return requireServerAuth(unavailable, auth)
	}
	if log == nil {
		log = slog.Default()
	}
	reverseProxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			target := request.In.Context().Value(targetKey{}).(proxyTarget)
			request.SetURL(target.url)
			request.Out.Host = request.In.Host
		},
		FlushInterval: -1,
		ErrorHandler: func(writer http.ResponseWriter, request *http.Request, err error) {
			if target, ok := request.Context().Value(targetKey{}).(proxyTarget); ok {
				// An SSE observer disconnect is not evidence that OpenCode failed. A
				// canceled held request may instead be a frozen backend, so invalidate
				// it and let the next request reconcile Docker state.
				if request.Context().Err() == nil || target.intent != workspace.RequestObserve {
					waker.InvalidateEndpoint(target.request)
				}
			}
			log.Error("proxy error", "err", err, "path", request.URL.Path)
			http.Error(writer, "upstream unavailable", http.StatusBadGateway)
		},
	}

	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		start := time.Now()
		intent := requestIntent(request)
		target, release, err := waker.AcquireRequest(request.Context(), intent)
		if err != nil {
			log.Error("wake failed", "err", err)
			http.Error(writer, "failed to wake workspace", http.StatusServiceUnavailable)
			return
		}
		defer release()
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			log.Info("workspace ready", "wake_ms", elapsed.Milliseconds(), "path", request.URL.Path)
		}
		targetURL, err := url.Parse(target.Endpoint.URL())
		if err != nil {
			http.Error(writer, "invalid workspace endpoint", http.StatusInternalServerError)
			return
		}
		ctx := context.WithValue(request.Context(), targetKey{}, proxyTarget{url: targetURL, request: target, intent: intent})
		reverseProxy.ServeHTTP(writer, request.WithContext(ctx))
	})
	return requireServerAuth(handler, auth)
}

func requestIntent(request *http.Request) workspace.RequestIntent {
	// Event streams are observation-only and intentionally survive until a
	// pause disconnects them. Every other request, including WebSocket upgrades,
	// holds an admission lease for its complete proxied lifetime.
	upgrade := strings.EqualFold(request.Header.Get("Upgrade"), "websocket")
	requestPath := request.URL.EscapedPath()
	if request.Method == http.MethodGet && !upgrade {
		if requestPath == "/api/event" {
			return workspace.RequestObserve
		}
	}
	if !upgrade && (request.Method == http.MethodGet || request.Method == http.MethodHead) {
		if requestPath == "/api/health" || requestPath == "/api/session/active" {
			return workspace.RequestRead
		}
	}
	return workspace.RequestWork
}
