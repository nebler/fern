package main

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAttachURLUsesReachableAddress(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"127.0.0.1:8080": "http://127.0.0.1:8080",
		"[::1]:8080":     "http://[::1]:8080",
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
	for _, address := range []string{"0.0.0.0:8080", ":8080", "[::]:8080", "100.64.0.1:8080", "localhost:8080"} {
		if _, err := attachURL(address); err == nil {
			t.Fatalf("attachURL accepted non-loopback address %q", address)
		}
	}
}

func TestAttachEnvironmentReplacesAuthentication(t *testing.T) {
	t.Parallel()
	got := attachEnvironment(
		[]string{"PATH=/bin", "OPENCODE_PASSWORD=old"},
		map[string]string{"OPENCODE_PASSWORD": "secret"},
	)
	for _, want := range []string{"PATH=/bin", "OPENCODE_PASSWORD=secret"} {
		if !slices.Contains(got, want) {
			t.Fatalf("environment %v does not contain %q", got, want)
		}
	}
	if slices.Contains(got, "OPENCODE_PASSWORD=old") {
		t.Fatalf("environment retained old password: %v", got)
	}
}

func TestAttachEnvironmentDropsUnrelatedSecrets(t *testing.T) {
	t.Parallel()
	got := attachEnvironment([]string{
		"PATH=/bin", "ANTHROPIC_API_KEY=anthropic", "OPENAI_API_KEY=openai",
		"AWS_SECRET_ACCESS_KEY=aws", "GH_TOKEN=gh", "GITHUB_TOKEN=github", "ARBITRARY_SECRET=value",
	}, map[string]string{"OPENCODE_PASSWORD": "secret"})
	if len(got) != 2 || !slices.Contains(got, "PATH=/bin") || !slices.Contains(got, "OPENCODE_PASSWORD=secret") {
		t.Fatalf("attach environment leaked or omitted values: %v", got)
	}
}

func TestImplicitAuthenticationForwardsOpenCodePassword(t *testing.T) {
	t.Setenv("OPENCODE_PASSWORD", "v2-secret")
	env := forwardedEnvironment(nil)
	if env["OPENCODE_PASSWORD"] != "v2-secret" {
		t.Fatalf("OPENCODE_PASSWORD = %q", env["OPENCODE_PASSWORD"])
	}
}

func TestExplicitEmptyAuthenticationSuppressesHostValue(t *testing.T) {
	t.Setenv("OPENCODE_PASSWORD", "host-secret")
	configured := forwardedEnvironment(map[string]string{"OPENCODE_PASSWORD": ""})
	got := attachEnvironment([]string{"OPENCODE_PASSWORD=host-secret"}, configured)
	for _, value := range got {
		if strings.HasPrefix(value, "OPENCODE_PASSWORD=") {
			t.Fatalf("explicit empty password was replaced: %v", got)
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

func TestResumeIsNotAStandaloneCommand(t *testing.T) {
	t.Parallel()
	if err := run([]string{"resume"}, nil); err == nil {
		t.Fatal("standalone resume command unexpectedly remained available")
	}
	if strings.Contains(usageText, "resume") {
		t.Fatalf("usage still advertises unsafe standalone resume: %s", usageText)
	}
}
