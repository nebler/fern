package config

import (
	"fmt"
	"os"
	"strings"
)

// The sentinel temporarily replaces each "$$" escape while os.Expand runs, so a
// doubled dollar survives expansion as one literal dollar sign.
const escapedDollar = "\x00fern-dollar\x00"

// escapeDollars hides "$$" sequences from os.Expand behind the sentinel.
func escapeDollars(value string) string {
	return strings.ReplaceAll(value, "$$", escapedDollar)
}

// restoreDollars turns sentinels back into single literal dollars.
func restoreDollars(value string) string {
	return strings.ReplaceAll(value, escapedDollar, "$")
}

// expandRequired expands every $VAR and ${VAR} reference through lookup and
// fails when any referenced variable is unset.
func expandRequired(value string, lookup func(string) (string, bool)) (string, error) {
	var missing string
	expanded := os.Expand(escapeDollars(value), func(key string) string {
		result, ok := lookup(key)
		if !ok && missing == "" {
			missing = key
		}
		return result
	})
	if missing != "" {
		return "", fmt.Errorf("environment variable %s is not set", missing)
	}
	return restoreDollars(expanded), nil
}

// referencedHostOnlySecret reports the first host-only secret variable that
// value references, or "" when none is referenced.
func referencedHostOnlySecret(value string) string {
	var found string
	os.Expand(escapeDollars(value), func(key string) string {
		if found != "" {
			return ""
		}
		switch key {
		case "FERN_CONTROL_PASSWORD", "FERN_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN":
			found = key
		}
		return ""
	})
	return found
}

// embeddedHostOnlySecret reports the first host-only secret whose live value
// was pasted verbatim into value, or "" when none appears.
func embeddedHostOnlySecret(value string, lookup func(string) (string, bool)) string {
	for _, key := range []string{"FERN_CONTROL_PASSWORD", "FERN_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		secret, exists := lookup(key)
		if exists && len(secret) >= 16 && strings.Contains(value, secret) {
			return key
		}
	}
	return ""
}
