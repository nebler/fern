package taskdelivery

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/nebler/fern/internal/opencodeapi"
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/workspace"
)

// LocalClientFactory creates direct host-to-container clients. It deliberately
// ignores proxy environment variables and permits no redirects.
func LocalClientFactory(auth runtime.ServerAuth) (ClientFactory, error) {
	if auth.Password == "" {
		return nil, errors.New("OpenCode server authentication is required for task delivery")
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	httpClient := &http.Client{Transport: transport}
	return func(target workspace.RequestTarget) (OpenCode, error) {
		return opencodeapi.New(opencodeapi.Config{
			BaseURL: target.Endpoint.URL(), Username: "opencode", Password: auth.Password, HTTPClient: httpClient,
		})
	}, nil
}
