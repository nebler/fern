package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/pluginauth"
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/workspace"
)

// Waker reconciles Docker workspace state on demand: AcquireRequest wakes the
// workspace and leases an endpoint for the request, and InvalidateEndpoint
// drops a cached endpoint that misbehaved so the next request re-wakes.
type Waker interface {
	AcquireRequest(context.Context, workspace.RequestIntent) (workspace.RequestTarget, func(), error)
	InvalidateEndpoint(workspace.RequestTarget)
}

// Controls bundles the optional control-plane dependencies a gateway handler
// may serve. A nil member disables its route.
type Controls struct {
	Store       *control.Store
	Tasks       http.Handler
	Onboarding  http.Handler
	WakeTrace   http.Handler
	Liveness    http.Handler
	Readiness   http.Handler
	Status      http.Handler
	Metrics     http.Handler
	ControlAuth ControlAuth
	PluginAuth  *pluginauth.Store
}

// Handlers holds the two production ingress surfaces built by NewHandlers.
type Handlers struct {
	Remote   http.Handler
	Operator http.Handler
}

type targetKey struct{}
type originKey struct{}

// TrustedOrigins carries the canonical scheme://host origins of the two
// listeners; both must parse strictly and remote must be HTTPS or loopback.
type TrustedOrigins struct {
	Remote   string
	Operator string
}

type trustedOrigin struct {
	raw       string
	scheme    string
	authority string
	port      string
	legacy    bool
}

type proxyTarget struct {
	url     *url.URL
	request workspace.RequestTarget
	intent  workspace.RequestIntent
}

// NewHandlers builds the two production ingress surfaces around one reverse
// proxy and one pairing-code state. The remote handler must be the only handler
// exposed through the private TLS edge.
func NewHandlers(waker Waker, auth runtime.ServerAuth, controls Controls, origins TrustedOrigins, log *slog.Logger) (Handlers, error) {
	pairing := newPairingState(controls.Store)
	pluginAuth := newPluginAuthHTTP(controls.PluginAuth)
	upstream := newUpstreamHandler(waker, log)
	remoteOrigin, err := parseTrustedOrigin(origins.Remote)
	if err != nil {
		return Handlers{}, err
	}
	operatorOrigin, err := parseTrustedOrigin(origins.Operator)
	if err != nil {
		return Handlers{}, err
	}
	if remoteOrigin.scheme == "http" && !trustedLoopbackOrigin(remoteOrigin) {
		return Handlers{}, errors.New("invalid trusted proxy origin: remote listener must be loopback HTTP or HTTPS")
	}
	if operatorOrigin.scheme != "http" || !trustedLoopbackOrigin(operatorOrigin) {
		return Handlers{}, errors.New("invalid trusted proxy origin: operator listener must be loopback HTTP")
	}
	remoteGateway := gatewayHandler(upstream, Controls{
		Tasks: controls.Tasks, Onboarding: controls.Onboarding, PluginAuth: controls.PluginAuth,
	})
	operatorGateway := gatewayHandler(upstream, controls)
	return Handlers{
		Remote:   trustedOriginHandler(pluginAuth.remoteHandler(pairing.remoteHandler(remoteGateway, auth), remoteGateway), remoteOrigin),
		Operator: trustedOriginHandler(pluginAuth.rejectBearerHandler(probeHandler(pairing.operatorHandler(operatorGateway, auth, controls.ControlAuth), controls)), operatorOrigin),
	}, nil
}

func probeHandler(next http.Handler, controls Controls) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.EscapedPath() == request.URL.Path {
			switch request.URL.Path {
			case "/fern/live":
				if controls.Liveness != nil {
					controls.Liveness.ServeHTTP(writer, request)
					return
				}
			case "/fern/ready":
				if controls.Readiness != nil {
					controls.Readiness.ServeHTTP(writer, request)
					return
				}
			}
		}
		next.ServeHTTP(writer, request)
	})
}

func trustedOriginHandler(next http.Handler, origin trustedOrigin) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx := context.WithValue(request.Context(), originKey{}, origin)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

// legacyOriginHandler preserves the concrete behavior of the non-production
// combined constructors by deriving their origin from each test request.
func legacyOriginHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		scheme := request.URL.Scheme
		if scheme == "" {
			scheme = "http"
		}
		host := request.Host
		if host == "" {
			host = request.URL.Host
		}
		origin := trustedOrigin{raw: scheme + "://" + host, scheme: scheme, authority: host, legacy: true}
		if scheme == "https" {
			origin.port = "443"
		} else {
			origin.port = "80"
		}
		if parsed, err := url.Parse(origin.raw); err == nil && parsed.Port() != "" {
			origin.port = parsed.Port()
		}
		ctx := context.WithValue(request.Context(), originKey{}, origin)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func parseTrustedOrigin(raw string) (trustedOrigin, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return trustedOrigin{}, fmt.Errorf("invalid trusted proxy origin %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return trustedOrigin{}, fmt.Errorf("invalid trusted proxy origin %q: scheme must be http or https", raw)
	}
	if parsed.Host == "" {
		return trustedOrigin{}, fmt.Errorf("invalid trusted proxy origin %q: host is missing", raw)
	}
	if parsed.User != nil {
		return trustedOrigin{}, fmt.Errorf("invalid trusted proxy origin %q: userinfo is not allowed", raw)
	}
	if parsed.Opaque != "" || parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return trustedOrigin{}, fmt.Errorf("invalid trusted proxy origin %q: path, query, and fragment are not allowed", raw)
	}
	port := parsed.Port()
	if port != "" {
		number, portErr := strconv.Atoi(port)
		if portErr != nil || number < 1 || number > 65535 || port != strconv.Itoa(number) {
			return trustedOrigin{}, fmt.Errorf("invalid trusted proxy origin %q: port %q is out of range", raw, port)
		}
	}
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if parsed.Hostname() == "" || raw != parsed.Scheme+"://"+parsed.Host {
		return trustedOrigin{}, fmt.Errorf("invalid trusted proxy origin %q: not in canonical scheme://host[:port] form", raw)
	}
	return trustedOrigin{raw: raw, scheme: parsed.Scheme, authority: parsed.Host, port: port}, nil
}

func trustedLoopbackOrigin(origin trustedOrigin) bool {
	ip := net.ParseIP(strings.Trim(origin.authority, "[]"))
	if host, _, err := net.SplitHostPort(origin.authority); err == nil {
		ip = net.ParseIP(host)
	}
	return ip != nil && ip.IsLoopback()
}

func newHandler(waker Waker, auth runtime.ServerAuth, controls Controls, log *slog.Logger) http.Handler {
	pairing := newPairingState(controls.Store)
	upstream := newUpstreamHandler(waker, log)
	gateway := gatewayHandler(upstream, controls)
	return newPluginAuthHTTP(controls.PluginAuth).remoteHandler(pairing.handler(gateway, auth, controls.ControlAuth), gateway)
}

func newUpstreamHandler(waker Waker, log *slog.Logger) http.Handler {
	if waker == nil {
		// Test-only path; production constructors always supply a waker.
		return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, "workspace manager unavailable", http.StatusServiceUnavailable)
		})
	}
	if log == nil {
		log = slog.Default()
	}
	reverseProxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			target, okTarget := request.In.Context().Value(targetKey{}).(proxyTarget)
			origin, okOrigin := request.In.Context().Value(originKey{}).(trustedOrigin)
			if !okTarget || !okOrigin {
				// The upstream handler always installs the routing context.
				// Without it the safest behavior is to forward the request
				// untouched and let transport failures surface through
				// ErrorHandler rather than panicking here.
				return
			}
			request.SetURL(target.url)
			for name := range request.Out.Header {
				if strings.EqualFold(name, "Forwarded") || strings.HasPrefix(strings.ToLower(name), "x-forwarded-") {
					delete(request.Out.Header, name)
				}
			}
			request.Out.Host = origin.authority
			request.Out.Header.Set("X-Forwarded-Host", origin.authority)
			request.Out.Header.Set("X-Forwarded-Proto", origin.scheme)
			request.Out.Header.Set("X-Forwarded-Port", origin.port)
		},
		FlushInterval: -1,
		ModifyResponse: func(response *http.Response) error {
			stripReservedSetCookies(response.Header)
			return nil
		},
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

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
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
}

func stripReservedSetCookies(header http.Header) {
	values := header.Values("Set-Cookie")
	if len(values) == 0 {
		return
	}
	header.Del("Set-Cookie")
	for _, value := range values {
		if containsReservedCookieAssignment(value) {
			continue
		}
		header.Add("Set-Cookie", value)
	}
}

func containsReservedCookieAssignment(value string) bool {
	for _, segment := range strings.FieldsFunc(value, func(character rune) bool { return character == ';' || character == ',' }) {
		name, _, valid := strings.Cut(strings.TrimSpace(segment), "=")
		if valid && (name == deviceCookieName || name == legacyDeviceCookieName) {
			return true
		}
	}
	return false
}

// OpenCode V2 routes with special wake-intent semantics, per docs/OPENCODE.md:
// the event stream is observation-only and survives pauses, while health checks
// and foreground-session polls are read-only probes that must not hold a full
// admission lease. Every other path, including WebSocket upgrades, is work.
const (
	opencodeEventPath         = "/api/event"
	opencodeHealthPath        = "/api/health"
	opencodeSessionActivePath = "/api/session/active"
)

func requestIntent(request *http.Request) workspace.RequestIntent {
	// Event streams are observation-only and intentionally survive until a
	// pause disconnects them. Every other request, including WebSocket upgrades,
	// holds an admission lease for its complete proxied lifetime.
	upgrade := strings.EqualFold(request.Header.Get("Upgrade"), "websocket")
	requestPath := request.URL.EscapedPath()
	if request.Method == http.MethodGet && !upgrade {
		if requestPath == opencodeEventPath {
			return workspace.RequestObserve
		}
	}
	if !upgrade && (request.Method == http.MethodGet || request.Method == http.MethodHead) {
		if requestPath == opencodeHealthPath || requestPath == opencodeSessionActivePath {
			return workspace.RequestRead
		}
	}
	return workspace.RequestWork
}
