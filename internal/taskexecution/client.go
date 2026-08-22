package taskexecution

import (
	"errors"
	"net/http"
	"time"

	"github.com/nebler/fern/internal/opencodeapi"
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/workspace"
)

func LocalClientFactory(auth runtime.ServerAuth) (ClientFactory, error) {
	if auth.Password == "" {
		return nil, errors.New("OpenCode authentication is required")
	}
	return func(target workspace.RequestTarget) (OpenCode, error) {
		if !runtime.ValidImageID(target.ImageID) || target.Endpoint.Host == "" || target.Endpoint.Port <= 0 {
			return nil, errors.New("invalid attested OpenCode target")
		}
		return opencodeapi.New(opencodeapi.Config{
			BaseURL: target.Endpoint.URL(), Username: "opencode", Password: auth.Password,
			HTTPClient: &http.Client{Timeout: 35 * time.Second},
		})
	}, nil
}
