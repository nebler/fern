package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

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
}

func New(waker Waker, log *slog.Logger) http.Handler {
	if waker == nil {
		return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, "workspace manager unavailable", http.StatusServiceUnavailable)
		})
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
				waker.InvalidateEndpoint(target.request)
			}
			log.Error("proxy error", "err", err, "path", request.URL.Path)
			http.Error(writer, "upstream unavailable", http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		start := time.Now()
		target, release, err := waker.AcquireRequest(request.Context(), requestIntent(request))
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
		ctx := context.WithValue(request.Context(), targetKey{}, proxyTarget{url: targetURL, request: target})
		reverseProxy.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func requestIntent(request *http.Request) workspace.RequestIntent {
	// Event streams are observation-only and intentionally survive until a
	// pause disconnects them. Every other request, including WebSocket upgrades,
	// holds an admission lease for its complete proxied lifetime.
	upgrade := strings.EqualFold(request.Header.Get("Upgrade"), "websocket")
	requestPath := request.URL.EscapedPath()
	if request.Method == http.MethodGet && !upgrade {
		switch requestPath {
		case "/event", "/global/event", "/api/event":
			return workspace.RequestObserve
		}
	}
	if !upgrade && (request.Method == http.MethodGet || request.Method == http.MethodHead) {
		switch requestPath {
		case "/global/health", "/session/status":
			return workspace.RequestRead
		}
	}
	return workspace.RequestWork
}
