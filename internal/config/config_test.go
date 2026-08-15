package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestValidateRejectsDynamicProxyPort(t *testing.T) {
	t.Parallel()
	config := Default(t.TempDir())
	config.Listen = "127.0.0.1:0"
	if err := Validate(config); err == nil {
		t.Fatal("Validate accepted proxy port 0")
	}
}

func TestLoadAppliesOverridesBeforeNormalization(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	repo := filepath.Join(directory, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  repo: ${MISSING_REPO}\nidle:\n  after: invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	idle := "1m"
	loaded, err := Load(path, directory, true, Overrides{Repo: &repo, IdleAfter: &idle})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace.Repo != repo || loaded.IdleAfter != time.Minute {
		t.Fatalf("loaded repo=%q idle=%s", loaded.Workspace.Repo, loaded.IdleAfter)
	}
}

func TestLoadOverridesInvalidYAMLTypes(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	repo := filepath.Join(directory, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "fern.yaml")
	data := []byte("workspace:\n  repo: [invalid]\nidle:\n  after: [invalid]\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	idle := "1m"
	if _, err := Load(path, directory, true, Overrides{Repo: &repo, IdleAfter: &idle}); err != nil {
		t.Fatalf("valid overrides did not replace invalid YAML types: %v", err)
	}
}

func TestLoadRejectsEmptyRepository(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  repo: ''\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, directory, true, Overrides{}); err == nil {
		t.Fatal("Load accepted an empty repository")
	}
	empty := ""
	if _, err := Load(path, directory, true, Overrides{Repo: &empty}); err == nil {
		t.Fatal("Load accepted an explicitly empty repository override")
	}
}

func TestLoadWorkspaceIgnoresInvalidIdleConfiguration(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  repo: .\nidle:\n  after: invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkspace(path, directory, true, Overrides{}); err != nil {
		t.Fatalf("workspace-only load rejected idle configuration: %v", err)
	}
	if err := os.WriteFile(path, []byte("workspace:\n  naem: demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkspace(path, directory, true, Overrides{}); err == nil {
		t.Fatal("workspace-only load accepted an unknown workspace field")
	}
}

func TestLoadWorkspaceRejectsDuplicateWorkspaceSections(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	data := []byte("workspace:\n  name: first\nworkspace:\n  name: second\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkspace(path, directory, true, Overrides{}); err == nil {
		t.Fatal("LoadWorkspace accepted duplicate workspace sections")
	}
}

func TestLoadAttachPreservesExplicitEmptyUsername(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  env:\n    OPENCODE_SERVER_USERNAME: ''\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := LoadAttach(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	username, exists := client.Env["OPENCODE_SERVER_USERNAME"]
	if !exists || username != "" {
		t.Fatalf("explicit username was not preserved: value=%q exists=%t", username, exists)
	}
}

func TestClientProjectionsIgnoreUnrelatedMalformedValues(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fern.yaml")
	data := []byte("workspace:\n  name: demo\n  env:\n    UNUSED:\n      nested: value\nproxy:\n  listen: [invalid]\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEvents(path, true, nil); err != nil {
		t.Fatalf("event projection parsed unrelated values: %v", err)
	}
	data = []byte("workspace:\n  name: [invalid]\n  env:\n    UNUSED:\n      nested: value\nproxy:\n  listen: 127.0.0.1:9090\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := LoadAttach(path, true, nil)
	if err != nil {
		t.Fatalf("attach projection parsed unrelated values: %v", err)
	}
	if client.Listen != "127.0.0.1:9090" {
		t.Fatalf("attach listen = %q", client.Listen)
	}
}

func TestClientOverridesSkipInvalidRelevantYAML(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fern.yaml")
	data := []byte("workspace:\n  name: [invalid]\nproxy:\n  listen: [invalid]\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	listen := "127.0.0.1:9090"
	if _, err := LoadAttach(path, true, &listen); err != nil {
		t.Fatalf("attach override did not skip invalid YAML: %v", err)
	}
	name := "demo"
	if _, err := LoadEvents(path, true, &name); err != nil {
		t.Fatalf("event override did not skip invalid YAML: %v", err)
	}
}

func TestLoadWorkspaceNameRejectsTrailingDocument(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fern.yaml")
	data := []byte("workspace:\n  name: production\n---\nworkspace:\n  name: staging\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkspaceName(path, true); err == nil {
		t.Fatal("LoadWorkspaceName accepted multiple YAML documents")
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  naem: demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, directory, true, Overrides{}); err == nil {
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
	if _, err := Load(path, directory, true, Overrides{}); err == nil {
		t.Fatal("Load accepted a missing environment variable")
	}
}

func TestLoadSupportsEscapedDollarAndRejectsTrailingDocument(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  env:\n    PRICE: '$$5'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, directory, true, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace.Env["PRICE"] != "$5" {
		t.Fatalf("escaped value = %q", loaded.Workspace.Env["PRICE"])
	}
	if err := os.WriteFile(path, []byte("workspace: {}\n---\nworkspace: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, directory, true, Overrides{}); err == nil {
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
	config, err := Load(path, "/wrong", true, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if config.Workspace.Repo != repo {
		t.Fatalf("repo = %q, want %q", config.Workspace.Repo, repo)
	}
}
