package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseMemoryBytes(t *testing.T) {
	t.Parallel()
	tests := map[string]int64{
		"8Gi":   8 * 1024 * 1024 * 1024,
		"512Mi": 512 * 1024 * 1024,
		"2GB":   2_000_000_000,
		"1024":  1024 * 1024 * 1024,
	}
	for input, want := range tests {
		input, want := input, want
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got, err := ParseMemoryBytes(input)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("ParseMemoryBytes(%q) = %d, want %d", input, got, want)
			}
		})
	}
}

func TestValidateRejectsUnauthenticatedRemoteListen(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	config := Default(directory)
	config.Listen = "0.0.0.0:8080"
	if err := Validate(config); err == nil {
		t.Fatal("Validate accepted unauthenticated wildcard listener")
	}
	config.Workspace.Env["OPENCODE_SERVER_PASSWORD"] = "secret"
	if err := Validate(config); err != nil {
		t.Fatalf("authenticated listener rejected: %v", err)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  naem: demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, directory, true); err == nil {
		t.Fatal("Load accepted an unknown field")
	}
}

func TestLoadRejectsMissingEnvironmentReference(t *testing.T) {
	previous, existed := os.LookupEnv("FERN_TEST_MISSING")
	_ = os.Unsetenv("FERN_TEST_MISSING")
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv("FERN_TEST_MISSING", previous)
		}
	})
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  env:\n    TOKEN: ${FERN_TEST_MISSING}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, directory, true); err == nil {
		t.Fatal("Load accepted a missing environment variable")
	}
}

func TestLoadSupportsEscapedDollarAndRejectsTrailingDocument(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  env:\n    PRICE: '$$5'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, directory, true)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace.Env["PRICE"] != "$5" {
		t.Fatalf("escaped value = %q", loaded.Workspace.Env["PRICE"])
	}
	if err := os.WriteFile(path, []byte("workspace: {}\n---\nworkspace: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, directory, true); err == nil {
		t.Fatal("Load accepted multiple YAML documents")
	}
}

func TestLoadResolvesRepoRelativeToConfig(t *testing.T) {
	directory := t.TempDir()
	repo := filepath.Join(directory, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  repo: ./repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(path, "/wrong", true)
	if err != nil {
		t.Fatal(err)
	}
	if config.Workspace.Repo != repo {
		t.Fatalf("repo = %q, want %q", config.Workspace.Repo, repo)
	}
}
