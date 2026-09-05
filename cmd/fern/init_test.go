package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/config"
)

func TestInitCreatesOnboardingOnlyConfigurationWithoutInstallationID(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "fern.yaml")
	envPath := filepath.Join(directory, "fern.env")
	err := runInit([]string{
		"--config", configPath,
		"--env-file", envPath,
		"--repo", directory,
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
	environment, err := readEnvFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadWithEnvironment(configPath, directory, true, config.Overrides{}, environment)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace.GitHub == nil || loaded.Workspace.GitHub.InstallationID != 0 {
		t.Fatalf("pending GitHub binding = %+v", loaded.Workspace.GitHub)
	}
	projected, err := config.ProjectBackgroundBootstrap(loaded)
	if err != nil {
		t.Fatalf("bootstrap validation: %v", err)
	}
	if err := config.ValidateBackground(projected); err == nil {
		t.Fatal("onboarding-only configuration authorized execution")
	}
	if _, err := loadUpConfig(upOptions{configPath: configPath, envPath: envPath, configRequired: true}); err != nil {
		t.Fatalf("up rejected onboarding-only configuration: %v", err)
	}
	report := diagnose(t.Context(), diagnoseOptions{ConfigPath: configPath, EnvPath: envPath})
	if report.Ready || len(report.Checks) != 3 || report.Checks[2].ID != "github" || report.Checks[2].Status != "fail" {
		t.Fatalf("onboarding doctor report = %+v", report)
	}
}
