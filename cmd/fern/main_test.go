package main

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/config"
)

func TestAttachURLUsesReachableAddress(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"127.0.0.1:8080":  "http://127.0.0.1:8080",
		"0.0.0.0:8080":    "http://127.0.0.1:8080",
		":8080":           "http://127.0.0.1:8080",
		"[::]:8080":       "http://[::1]:8080",
		"100.64.0.1:8080": "http://100.64.0.1:8080",
	}
	for input, want := range tests {
		got, err := attachURL(input)
		if err != nil {
			t.Fatalf("attachURL(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("attachURL(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := attachURL("127.0.0.1:0"); err == nil {
		t.Fatal("attachURL accepted a dynamic port")
	}
}

func TestAttachEnvironmentReplacesAuthentication(t *testing.T) {
	t.Parallel()
	got := attachEnvironment(
		[]string{"PATH=/bin", "OPENCODE_SERVER_USERNAME=old", "OPENCODE_SERVER_PASSWORD=old", "OPENCODE_PASSWORD=old-v2"},
		map[string]string{"OPENCODE_SERVER_USERNAME": "agent", "OPENCODE_SERVER_PASSWORD": "secret", "OPENCODE_PASSWORD": "secret-v2"},
	)
	for _, want := range []string{"PATH=/bin", "OPENCODE_SERVER_USERNAME=agent", "OPENCODE_SERVER_PASSWORD=secret"} {
		if !slices.Contains(got, want) {
			t.Fatalf("environment %v does not contain %q", got, want)
		}
	}
	if slices.Contains(got, "OPENCODE_SERVER_PASSWORD=old") {
		t.Fatalf("environment retained old password: %v", got)
	}
	if slices.Contains(got, "OPENCODE_PASSWORD=old-v2") || slices.Contains(got, "OPENCODE_PASSWORD=secret-v2") {
		t.Fatalf("V1 environment retained a V2 password: %v", got)
	}
}

func TestV2AttachEnvironmentDropsV1Credentials(t *testing.T) {
	t.Parallel()
	got := attachEnvironmentFor(config.OpenCodeV2,
		[]string{"PATH=/bin", "OPENCODE_SERVER_USERNAME=old", "OPENCODE_SERVER_PASSWORD=old", "OPENCODE_PASSWORD=old-v2"},
		map[string]string{"OPENCODE_SERVER_USERNAME": "agent", "OPENCODE_SERVER_PASSWORD": "v1", "OPENCODE_PASSWORD": "v2"},
	)
	if !slices.Contains(got, "OPENCODE_PASSWORD=v2") {
		t.Fatalf("V2 environment = %v", got)
	}
	for _, value := range got {
		if strings.HasPrefix(value, "OPENCODE_SERVER_") {
			t.Fatalf("V2 environment retained a V1 credential: %v", got)
		}
	}
}

func TestImplicitAuthenticationForwardingIsProtocolSpecific(t *testing.T) {
	t.Setenv("OPENCODE_SERVER_USERNAME", "v1-user")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "v1-secret")
	t.Setenv("OPENCODE_PASSWORD", "v2-secret")
	tests := []struct {
		protocol config.OpenCodeProtocol
		want     []string
		reject   []string
	}{
		{protocol: config.OpenCodeV1, want: []string{"OPENCODE_SERVER_USERNAME", "OPENCODE_SERVER_PASSWORD"}, reject: []string{"OPENCODE_PASSWORD"}},
		{protocol: config.OpenCodeV2, want: []string{"OPENCODE_PASSWORD"}, reject: []string{"OPENCODE_SERVER_USERNAME", "OPENCODE_SERVER_PASSWORD"}},
		{protocol: config.OpenCodeAuto, want: []string{"OPENCODE_SERVER_USERNAME", "OPENCODE_SERVER_PASSWORD", "OPENCODE_PASSWORD"}},
	}
	for _, test := range tests {
		env := forwardedEnvironmentFor(test.protocol, nil)
		for _, key := range test.want {
			if env[key] == "" {
				t.Fatalf("protocol %s did not forward %s", test.protocol, key)
			}
		}
		for _, key := range test.reject {
			if _, exists := env[key]; exists {
				t.Fatalf("protocol %s unexpectedly forwarded %s", test.protocol, key)
			}
		}
	}
}

func TestExplicitEmptyAuthenticationSuppressesHostValue(t *testing.T) {
	t.Setenv("OPENCODE_SERVER_USERNAME", "host-user")
	configured := forwardedEnvironment(map[string]string{"OPENCODE_SERVER_USERNAME": ""})
	got := attachEnvironment([]string{"OPENCODE_SERVER_USERNAME=host-user"}, configured)
	for _, value := range got {
		if strings.HasPrefix(value, "OPENCODE_SERVER_USERNAME=") {
			t.Fatalf("explicit empty username was replaced: %v", got)
		}
	}
}

func TestExplicitNameBypassesBrokenUnrelatedConfig(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "broken.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  env:\n    TOKEN: ${MISSING_FOR_CLEANUP}\nunknown: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	flags, nameFlag, configPath := workspaceFlags("down")
	if err := flags.Parse([]string{"--config", path, "--name", "emergency"}); err != nil {
		t.Fatal(err)
	}
	name, err := workspaceName(flags, *nameFlag, *configPath)
	if err != nil {
		t.Fatal(err)
	}
	if name != "emergency" {
		t.Fatalf("name = %q", name)
	}
}

func TestTrackedConnectionRemovesItselfOnClose(t *testing.T) {
	t.Parallel()
	tracker := newConnectionTracker()
	left, right := net.Pipe()
	defer right.Close()
	tracked := &trackedConnection{Conn: left, tracker: tracker}
	tracker.add(tracked)
	if err := tracked.Close(); err != nil {
		t.Fatal(err)
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if len(tracker.conns) != 0 {
		t.Fatalf("tracker retained %d closed connections", len(tracker.conns))
	}
}
