package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
