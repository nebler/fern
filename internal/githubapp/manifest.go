package githubapp

import (
	"encoding/json"
	"net/url"
	"strings"
)

const (
	maxManifestNameBytes = 100
	maxManifestURLBytes  = 2048
	maxManifestBytes     = 8 << 10
)

// GenerateAppManifest returns the bounded JSON submitted to GitHub's App
// Manifest flow. The callback is the one-time conversion redirect endpoint.
func GenerateAppManifest(name, homepageURL, callbackURL string) ([]byte, error) {
	if !validManifestName(name) || !validHTTPSURL(homepageURL) || !validHTTPSURL(callbackURL) {
		return nil, ErrInvalidManifest
	}
	payload, err := json.Marshal(struct {
		Name           string `json:"name"`
		URL            string `json:"url"`
		RedirectURL    string `json:"redirect_url"`
		Public         bool   `json:"public"`
		HookAttributes struct {
			Active bool `json:"active"`
		} `json:"hook_attributes"`
		DefaultPermissions map[string]string `json:"default_permissions"`
		DefaultEvents      []string          `json:"default_events"`
	}{
		Name:        name,
		URL:         homepageURL,
		RedirectURL: callbackURL,
		Public:      false,
		DefaultPermissions: map[string]string{
			"metadata":      "read",
			"contents":      "write",
			"pull_requests": "write",
		},
		DefaultEvents: []string{},
	})
	if err != nil || len(payload) > maxManifestBytes {
		return nil, ErrInvalidManifest
	}
	return payload, nil
}

func validManifestName(value string) bool {
	if value == "" || len(value) > maxManifestNameBytes || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range []byte(value) {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func validHTTPSURL(value string) bool {
	if value == "" || len(value) > maxManifestURLBytes {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.IsAbs() && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil && parsed.Fragment == ""
}
