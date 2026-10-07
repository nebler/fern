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
	Liveness    http.Handler
	Readiness   http.Handler
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
}

// parseTrustedOrigins enforces the listener topology: the remote origin is
// HTTPS or loopback HTTP, and the operator origin is always loopback HTTP.
func parseTrustedOrigins(origins TrustedOrigins) (trustedOrigin, trustedOrigin, error) {
	remote, err := parseTrustedOrigin(origins.Remote)
	if err != nil {
		return trustedOrigin{}, trustedOrigin{}, err
	}
	operator, err := parseTrustedOrigin(origins.Operator)
	if err != nil {
		return trustedOrigin{}, trustedOrigin{}, err
	}
	if remote.scheme == "http" && !trustedLoopbackOrigin(remote) {
		return trustedOrigin{}, trustedOrigin{}, errors.New("invalid trusted proxy origin: remote listener must be loopback HTTP or HTTPS")
	}
	if operator.scheme != "http" || !trustedLoopbackOrigin(operator) {
		return trustedOrigin{}, trustedOrigin{}, errors.New("invalid trusted proxy origin: operator listener must be loopback HTTP")
	}
	return remote, operator, nil
}

func withTrustedOrigin(ctx context.Context, origin trustedOrigin) context.Context {
	return context.WithValue(ctx, originKey{}, origin)
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
