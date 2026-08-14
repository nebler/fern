package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
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
		[]string{"PATH=/bin", "OPENCODE_SERVER_USERNAME=old", "OPENCODE_SERVER_PASSWORD=old"},
		map[string]string{"OPENCODE_SERVER_USERNAME": "agent", "OPENCODE_SERVER_PASSWORD": "secret"},
	)
	for _, want := range []string{"PATH=/bin", "OPENCODE_SERVER_USERNAME=agent", "OPENCODE_SERVER_PASSWORD=secret"} {
		if !slices.Contains(got, want) {
			t.Fatalf("environment %v does not contain %q", got, want)
		}
	}
	if slices.Contains(got, "OPENCODE_SERVER_PASSWORD=old") {
		t.Fatalf("environment retained old password: %v", got)
	}
}

func TestConfigSelectionSupportsLongFlags(t *testing.T) {
	t.Parallel()
	tests := [][]string{
		{"--config", "one.yaml"},
		{"--config=one.yaml"},
		{"-config", "one.yaml"},
		{"-config=one.yaml"},
	}
	for _, args := range tests {
		selection, err := configSelection(args)
		if err != nil {
			t.Fatal(err)
		}
		if selection.path != "one.yaml" || !selection.required {
			t.Fatalf("configSelection(%v) = %+v", args, selection)
		}
	}
}

func TestConfigSelectionRejectsDuplicatesAndIgnoresAfterSeparator(t *testing.T) {
	t.Parallel()
	if _, err := configSelection([]string{"--config", "one.yaml", "--config", "two.yaml"}); err == nil {
		t.Fatal("duplicate config flags were accepted")
	}
	selection, err := configSelection([]string{"--", "--config", "ignored.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if selection.path != "fern.yaml" || selection.required {
		t.Fatalf("selection after -- = %+v", selection)
	}
}

func TestExplicitNameBypassesBrokenUnrelatedConfig(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "broken.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  env:\n    TOKEN: ${MISSING_FOR_CLEANUP}\nunknown: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	name, flags, err := commandWorkspaceName("down", []string{"--config", path, "--name", "emergency"})
	if err != nil {
		t.Fatal(err)
	}
	if err := flags.Parse([]string{"--config", path, "--name", "emergency"}); err != nil {
		t.Fatal(err)
	}
	if *name != "emergency" {
		t.Fatalf("name = %q", *name)
	}
}
