package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/config"
)

func TestInitCreatesPendingConfigurationWithoutInstallationID(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "fern.yaml")
	envPath := filepath.Join(directory, "fern.env")
	runtimeRoot := filepath.Join(directory, "operator-provisioned-runtime")
	err := runInit([]string{
		"--config", configPath,
		"--env-file", envPath,
		"--repo", directory,
		"--runtime-storage-root", runtimeRoot,
		"--repository", "owner/repository",
		"--repository-id", "123",
		"--model-provider", "anthropic",
		"--model", "model",
		"--background-image-id", "sha256:" + strings.Repeat("b", 64),
		"--remote-origin", "https://fern.example.ts.net",
		"--background-origin", "https://fern.example.ts.net:8443",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "installationId") {
		t.Fatalf("pending installation ID was serialized:\n%s", data)
	}
	if strings.Contains(string(data), "budget") || strings.Contains(string(data), "maxTurns") || strings.Contains(string(data), "verification") {
		t.Fatalf("generated configuration contains retired settings:\n%s", data)
	}
	environment, err := readEnvFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(configPath, environment)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace.GitHub.InstallationID != 0 {
		t.Fatalf("pending GitHub binding = %+v", loaded.Workspace.GitHub)
	}
	if loaded.Runs.RuntimeStorageRoot != runtimeRoot {
		t.Fatalf("runtime storage root = %q", loaded.Runs.RuntimeStorageRoot)
	}
	if _, err := os.Stat(runtimeRoot); !os.IsNotExist(err) {
		t.Fatalf("init must not provision runtime storage: %v", err)
	}
	if err := config.ValidateBootstrap(loaded); err != nil {
		t.Fatalf("bootstrap validation: %v", err)
	}
	if err := config.Validate(loaded); err == nil {
		t.Fatal("pending configuration authorized execution")
	}
	report := diagnose(t.Context(), diagnoseOptions{ConfigPath: configPath, EnvPath: envPath})
	if report.Ready || len(report.Checks) != 2 || report.Checks[1].ID != "github" || report.Checks[1].Status != "fail" {
		t.Fatalf("pending doctor report = %+v", report)
	}
}
