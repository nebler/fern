package main

import (
	"slices"
	"testing"

	"github.com/nebler/fern/internal/config"
)

func TestAttachTargetAcceptsExplicitOrigins(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"http://127.0.0.1:8080":           "http://127.0.0.1:8080",
		"https://host.tailnet.ts.net":     "https://host.tailnet.ts.net",
		"https://host.tailnet.ts.net/":    "https://host.tailnet.ts.net",
		"HTTPS://host.tailnet.ts.net:443": "https://host.tailnet.ts.net:443",
	}
	for input, want := range tests {
		input, want := input, want
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got, err := attachTarget(&input, "not a listener")
			if err != nil {
				t.Fatalf("attachTarget(%q): %v", input, err)
			}
			if got != want {
				t.Fatalf("attachTarget(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestAttachCommandUsesProtocolClient(t *testing.T) {
	t.Parallel()
	tests := []struct {
		protocol   config.OpenCodeProtocol
		executable string
		args       []string
	}{
		{protocol: config.OpenCodeV1, executable: "opencode", args: []string{"attach", "https://fern.example"}},
		{protocol: config.OpenCodeV2, executable: "opencode2", args: []string{"--server", "https://fern.example"}},
	}
	for _, test := range tests {
		executable, args := attachCommand(test.protocol, "https://fern.example")
		if executable != test.executable || !slices.Equal(args, test.args) {
			t.Fatalf("attachCommand(%q) = %q %v, want %q %v", test.protocol, executable, args, test.executable, test.args)
		}
	}
}

func TestAttachTargetRejectsInvalidExplicitOrigins(t *testing.T) {
	t.Parallel()
	tests := []string{
		"",
		"host.tailnet.ts.net",
		"https:/host.tailnet.ts.net",
		"https://",
		"https://:443",
		"ftp://host.tailnet.ts.net",
		"http://host.tailnet.ts.net",
		"http://100.64.0.1:8080",
		"https://user@host.tailnet.ts.net",
		"https://user:secret@host.tailnet.ts.net",
		"https://host.tailnet.ts.net#section",
		"https://host.tailnet.ts.net#",
		"https://host.tailnet.ts.net/api",
		"https://host.tailnet.ts.net/%61pi",
		"https://host.tailnet.ts.net?token=secret",
		"https://host.tailnet.ts.net?",
	}
	for _, input := range tests {
		input := input
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			if _, err := attachTarget(&input, "127.0.0.1:8080"); err == nil {
				t.Fatalf("attachTarget accepted %q", input)
			}
		})
	}
}

func TestAttachTargetFallsBackToListener(t *testing.T) {
	t.Parallel()
	got, err := attachTarget(nil, "0.0.0.0:8080")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:8080" {
		t.Fatalf("attachTarget(nil) = %q", got)
	}
}

func TestExplicitAttachOriginKeepsCredentialsInEnvironment(t *testing.T) {
	t.Parallel()
	origin := "https://host.tailnet.ts.net"
	target, err := attachTarget(&origin, "127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	if target != origin {
		t.Fatalf("target = %q, want %q", target, origin)
	}

	environment := attachEnvironment(nil, map[string]string{
		"OPENCODE_SERVER_USERNAME": "agent",
		"OPENCODE_SERVER_PASSWORD": "secret",
	})
	for _, value := range []string{
		"OPENCODE_SERVER_USERNAME=agent",
		"OPENCODE_SERVER_PASSWORD=secret",
	} {
		if !slices.Contains(environment, value) {
			t.Fatalf("environment %v does not contain %q", environment, value)
		}
	}
	if target == "https://agent:secret@host.tailnet.ts.net" {
		t.Fatal("credentials were placed in attach URL")
	}
}
