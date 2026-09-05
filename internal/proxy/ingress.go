package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/pluginauth"
)

// Controls contains the durable control-plane handlers exposed by Fern.
type Controls struct {
	Store       *control.Store
	Runs        http.Handler
	RunControl  http.Handler
	Results     http.Handler
	Onboarding  http.Handler
	Liveness    http.Handler
	Readiness   http.Handler
	Status      http.Handler
	Metrics     http.Handler
	ControlAuth ControlAuth
	PluginAuth  *pluginauth.Store
}

type Handlers struct {
	Remote   http.Handler
	Operator http.Handler
}

type TrustedOrigins struct {
	Remote   string
	Operator string
}

type originKey struct{}

type trustedOrigin struct {
	raw       string
	scheme    string
	authority string
	port      string
	legacy    bool
}

// NewHandlers builds the paired remote and Basic-authenticated loopback
// control surfaces. Neither surface proxies to a persistent OpenCode runtime.
func NewHandlers(controls Controls, origins TrustedOrigins) (Handlers, error) {
	if controls.Store == nil || controls.PluginAuth == nil || controls.Runs == nil || controls.Results == nil {
		return Handlers{}, errors.New("control, plugin authorization, run, and result handlers are required")
	}
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
	pairing := newPairingState(controls.Store)
	pluginAuth := newPluginAuthHTTP(controls.PluginAuth)
	remoteGateway := gatewayHandler(Controls{Store: controls.Store, Runs: controls.Runs, RunControl: controls.RunControl, Results: controls.Results, Onboarding: controls.Onboarding, PluginAuth: controls.PluginAuth})
	operatorGateway := gatewayHandler(controls)
	return Handlers{
		Remote:   trustedOriginHandler(pluginAuth.remoteHandler(pairing.remoteHandler(remoteGateway), remoteGateway), remoteOrigin),
		Operator: trustedOriginHandler(pluginAuth.rejectBearerHandler(probeHandler(pairing.operatorHandler(operatorGateway, controls.ControlAuth), controls)), operatorOrigin),
	}, nil
}

func probeHandler(next http.Handler, controls Controls) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() == r.URL.Path {
			switch r.URL.Path {
			case "/fern/live":
				if controls.Liveness != nil {
					controls.Liveness.ServeHTTP(w, r)
					return
				}
			case "/fern/ready":
				if controls.Readiness != nil {
					controls.Readiness.ServeHTTP(w, r)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func trustedOriginHandler(next http.Handler, origin trustedOrigin) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), originKey{}, origin)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func parseTrustedOrigin(raw string) (trustedOrigin, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.Opaque != "" || parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return trustedOrigin{}, fmt.Errorf("invalid trusted proxy origin %q", raw)
	}
	port := parsed.Port()
	if port != "" {
		number, portErr := strconv.Atoi(port)
		if portErr != nil || number < 1 || number > 65535 || port != strconv.Itoa(number) {
			return trustedOrigin{}, fmt.Errorf("invalid trusted proxy origin %q", raw)
		}
	} else if parsed.Scheme == "https" {
		port = "443"
	} else {
		port = "80"
	}
	if parsed.Hostname() == "" || raw != parsed.Scheme+"://"+parsed.Host {
		return trustedOrigin{}, fmt.Errorf("invalid trusted proxy origin %q", raw)
	}
	return trustedOrigin{raw: raw, scheme: parsed.Scheme, authority: parsed.Host, port: port}, nil
}

func trustedLoopbackOrigin(origin trustedOrigin) bool {
	host := strings.Trim(origin.authority, "[]")
	if split, _, err := net.SplitHostPort(origin.authority); err == nil {
		host = split
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
