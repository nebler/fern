package main

import (
	"slices"
	"testing"
)

func TestAttachTargetAcceptsExplicitOrigins(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"http://127.0.0.1:8080":  "http://127.0.0.1:8080",
		"https://127.0.0.1:8443": "https://127.0.0.1:8443",
		"http://[::1]:8081/":     "http://[::1]:8081",
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

func TestAttachCommandUsesV2Client(t *testing.T) {
	t.Parallel()
	executable, args := attachCommand("https://fern.example")
	if executable != "opencode2" || !slices.Equal(args, []string{"--server", "https://fern.example"}) {
		t.Fatalf("attachCommand() = %q %v", executable, args)
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
		"https://host.tailnet.ts.net",
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
	if _, err := attachTarget(nil, "0.0.0.0:8080"); err == nil {
		t.Fatal("attach accepted a non-loopback listener")
	}
}

func TestExplicitAttachOriginKeepsCredentialsInEnvironment(t *testing.T) {
	t.Parallel()
	origin := "http://127.0.0.1:8081"
	target, err := attachTarget(&origin, "127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	if target != origin {
		t.Fatalf("target = %q, want %q", target, origin)
	}

	environment := attachEnvironment(nil, map[string]string{
		"OPENCODE_PASSWORD": "secret",
	})
	for _, value := range []string{"OPENCODE_PASSWORD=secret"} {
		if !slices.Contains(environment, value) {
			t.Fatalf("environment %v does not contain %q", environment, value)
		}
	}
	if target == "http://opencode:secret@127.0.0.1:8081" {
		t.Fatal("credentials were placed in attach URL")
	}
}
