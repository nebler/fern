package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/noah/fern/internal/runtime"
	"github.com/noah/fern/internal/workspace"
)

type Waker interface {
	AcquireRequest(context.Context, workspace.RequestIntent) (runtime.Endpoint, func(), error)
}

type targetKey struct{}

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
			target := request.In.Context().Value(targetKey{}).(*url.URL)
			request.SetURL(target)
			request.Out.Host = request.In.Host
		},
		FlushInterval: -1,
		ErrorHandler: func(writer http.ResponseWriter, request *http.Request, err error) {
			log.Error("proxy error", "err", err, "path", request.URL.Path)
			http.Error(writer, "upstream unavailable", http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		start := time.Now()
		ep, release, err := waker.AcquireRequest(request.Context(), requestIntent(request))
		if err != nil {
			log.Error("wake failed", "err", err)
			http.Error(writer, "failed to wake workspace", http.StatusServiceUnavailable)
			return
		}
		defer release()
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			log.Info("workspace ready", "wake_ms", elapsed.Milliseconds(), "path", request.URL.Path)
		}
		target, err := url.Parse(ep.URL())
		if err != nil {
			http.Error(writer, "invalid workspace endpoint", http.StatusInternalServerError)
			return
		}
		ctx := context.WithValue(request.Context(), targetKey{}, target)
		reverseProxy.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func requestIntent(request *http.Request) workspace.RequestIntent {
	// Event streams are observation-only and intentionally survive until a
	// pause disconnects them. Every other request, including WebSocket upgrades,
	// holds an admission lease for its complete proxied lifetime.
	cleanPath := path.Clean(request.URL.Path)
	switch cleanPath {
	case "/event", "/global/event", "/api/event":
		return workspace.RequestIntent{}
	}
	upgrade := strings.EqualFold(request.Header.Get("Upgrade"), "websocket")
	if !upgrade && (request.Method == http.MethodGet || request.Method == http.MethodHead) {
		switch cleanPath {
		case "/global/health", "/session/status":
			return workspace.RequestIntent{Hold: true}
		}
	}
	return workspace.RequestIntent{Hold: true, MayStartWork: true}
}
