package githubapp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestGenerateAppManifestUsesOnlyRequiredPermissions(t *testing.T) {
	t.Parallel()
	payload, err := GenerateAppManifest("Fern Appliance", "https://fern.example", "https://host.example/github/callback?state=opaque")
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > maxManifestBytes {
		t.Fatalf("manifest has %d bytes", len(payload))
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 7 {
		t.Fatalf("manifest keys = %v", manifest)
	}
	var permissions map[string]string
	if err := json.Unmarshal(manifest["default_permissions"], &permissions); err != nil {
		t.Fatal(err)
	}
	if len(permissions) != 3 || permissions["metadata"] != "read" || permissions["contents"] != "write" || permissions["pull_requests"] != "write" {
		t.Fatalf("permissions = %v", permissions)
	}
	var events []string
	if err := json.Unmarshal(manifest["default_events"], &events); err != nil || len(events) != 0 {
		t.Fatalf("events = %v, error = %v", events, err)
	}
	var hooks struct {
		Active bool `json:"active"`
	}
	if err := json.Unmarshal(manifest["hook_attributes"], &hooks); err != nil || hooks.Active {
		t.Fatalf("hooks = %+v, error = %v", hooks, err)
	}
	if string(manifest["public"]) != "false" || string(manifest["redirect_url"]) != `"https://host.example/github/callback?state=opaque"` {
		t.Fatalf("manifest = %s", payload)
	}
}

func TestGenerateAppManifestRejectsUnboundedOrUnsafeInputs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		appName  string
		homepage string
		callback string
	}{
		{name: "empty name", homepage: "https://fern.example", callback: "https://host.example/callback"},
		{name: "long name", appName: strings.Repeat("a", maxManifestNameBytes+1), homepage: "https://fern.example", callback: "https://host.example/callback"},
		{name: "control in name", appName: "Fern\nApp", homepage: "https://fern.example", callback: "https://host.example/callback"},
		{name: "HTTP callback", appName: "Fern", homepage: "https://fern.example", callback: "http://host.example/callback"},
		{name: "callback credentials", appName: "Fern", homepage: "https://fern.example", callback: "https://secret@host.example/callback"},
		{name: "callback fragment", appName: "Fern", homepage: "https://fern.example", callback: "https://host.example/callback#code"},
		{name: "HTTP homepage", appName: "Fern", homepage: "http://fern.example", callback: "https://host.example/callback"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if payload, err := GenerateAppManifest(test.appName, test.homepage, test.callback); payload != nil || !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("payload = %q, error = %v", payload, err)
			}
		})
	}
}
