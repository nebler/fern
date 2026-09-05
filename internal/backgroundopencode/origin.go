package backgroundopencode

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// TrustedOrigin is a canonical credential-free HTTPS origin selected by
// Fern's trusted routing configuration.
type TrustedOrigin struct{ value string }

func ParseTrustedOrigin(value string) (TrustedOrigin, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || parsed.Path != "" || parsed.Hostname() == "" || parsed.String() != value {
		return TrustedOrigin{}, ErrInvalidConfig
	}
	if parsed.Port() != "" {
		port, err := strconv.Atoi(parsed.Port())
		if err != nil || port < 1 || port > 65535 || port == 443 {
			return TrustedOrigin{}, ErrInvalidConfig
		}
	}
	if strings.ContainsAny(parsed.Host, "@\\") {
		return TrustedOrigin{}, ErrInvalidConfig
	}
	host := parsed.Hostname()
	if host != strings.ToLower(host) || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".") {
		return TrustedOrigin{}, ErrInvalidConfig
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsUnspecified() || host != ip.String() {
			return TrustedOrigin{}, ErrInvalidConfig
		}
		return TrustedOrigin{value: value}, nil
	}
	if !canonicalDNSName(host) {
		return TrustedOrigin{}, ErrInvalidConfig
	}
	return TrustedOrigin{value: value}, nil
}

func canonicalDNSName(host string) bool {
	if len(host) > 253 || !strings.Contains(host, ".") {
		return false
	}
	allNumeric := true
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			char := label[i]
			if char < '0' || char > '9' {
				allNumeric = false
			}
			if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-') {
				return false
			}
		}
	}
	return !allNumeric
}
