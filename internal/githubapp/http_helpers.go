package githubapp

import (
	"encoding/json"
	"net/url"

	"github.com/nebler/fern/internal/safeio"
)

const maxJSONDepth = 64

func validAPIBase(base string) bool {
	parsed, err := url.Parse(base)
	return err == nil && parsed.IsAbs() && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.RawPath == "" && parsed.Path == ""
}

func decodeGitHubJSON(payload []byte, destination any) error {
	if err := safeio.CheckJSON(payload, maxJSONDepth); err != nil {
		return ErrInvalidResponse
	}
	if err := json.Unmarshal(payload, destination); err != nil {
		return ErrInvalidResponse
	}
	return nil
}
