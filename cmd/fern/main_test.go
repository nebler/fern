package main

import (
	"net"
	"strings"
	"testing"
)

func TestLoopbackURLUsesReachableAddress(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"127.0.0.1:8080": "http://127.0.0.1:8080",
		"[::1]:8080":     "http://[::1]:8080",
	}
	for input, want := range tests {
		got, err := loopbackURL(input)
		if err != nil {
			t.Fatalf("loopbackURL(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("loopbackURL(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := loopbackURL("127.0.0.1:0"); err == nil {
		t.Fatal("loopbackURL accepted a dynamic port")
	}
	for _, address := range []string{"0.0.0.0:8080", ":8080", "[::]:8080", "100.64.0.1:8080", "localhost:8080"} {
		if _, err := loopbackURL(address); err == nil {
			t.Fatalf("loopbackURL accepted non-loopback address %q", address)
		}
	}
}

func TestTrackedConnectionRemovesItselfOnClose(t *testing.T) {
	t.Parallel()
	tracker := newConnectionTracker()
	left, right := net.Pipe()
	defer right.Close()
	tracked := &trackedConnection{Conn: left, tracker: tracker}
	tracker.mu.Lock()
	tracker.conns[tracked] = struct{}{}
	tracker.mu.Unlock()
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

func TestPersistentWorkspaceCommandsAreNotRegistered(t *testing.T) {
	t.Parallel()
	for _, retired := range []string{"status", "logs", "down", "github"} {
		for _, command := range commands {
			if command.name == retired {
				t.Fatalf("retired persistent workspace command %q is registered", retired)
			}
		}
	}
	for _, retained := range []string{"init", "doctor", "up", "runs", "attach", "backup", "credentials", "debug", "version"} {
		found := false
		for _, command := range commands {
			found = found || command.name == retained
		}
		if !found {
			t.Fatalf("required command %q is not registered", retained)
		}
	}
}

func TestInitRequiresQualifiedLocalImageID(t *testing.T) {
	t.Parallel()
	err := runInit([]string{"--config", t.TempDir() + "/fern.yaml", "--env-file", t.TempDir() + "/fern.env"})
	if err == nil || !strings.Contains(err.Error(), "-background-image-id is required") {
		t.Fatalf("init without qualified image ID = %v", err)
	}
}
