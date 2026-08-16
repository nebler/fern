package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/config"
)

func TestInitCreatesProtectedRunnableConfiguration(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "fern.yaml")
	envPath := filepath.Join(directory, "fern.env")
	if err := runInit([]string{"--config", configPath, "--env-file", envPath, "--repo", repository}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{configPath, envPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %o, want 600", path, got)
		}
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "OPENCODE_PASSWORD") {
		t.Fatal("generated YAML contains the OpenCode password")
	}
	values, err := readEnvFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(values["OPENCODE_PASSWORD"]) != 64 {
		t.Fatalf("password length = %d, want 64", len(values["OPENCODE_PASSWORD"]))
	}
	loaded, err := config.Load(configPath, directory, true, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	loaded.Workspace.Env = mergeEnvironment(loaded.Workspace.Env, values)
	if err := config.Validate(loaded); err != nil {
		t.Fatalf("generated configuration is invalid: %v", err)
	}
	if err := runInit([]string{"--config", configPath, "--env-file", envPath, "--repo", repository}); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("second init error = %v", err)
	}
}

func TestReadEnvFileRejectsMalformedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fern.env")
	if err := os.WriteFile(path, []byte("GOOD=value\nbad line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readEnvFile(path); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("malformed environment error = %v", err)
	}
}

func TestReadEnvFileRejectsExposedSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fern.env")
	if err := os.WriteFile(path, []byte("OPENCODE_PASSWORD=secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readEnvFile(path); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("exposed environment error = %v", err)
	}
}

func TestWriteQRDoesNotExposeCredentials(t *testing.T) {
	var output strings.Builder
	value := "https://fern.example.ts.net/fern/"
	if err := writeQR(&output, value); err != nil {
		t.Fatal(err)
	}
	if output.Len() == 0 || strings.Contains(output.String(), value) {
		t.Fatal("QR output is empty or contains the literal URL")
	}
}

func TestTailscaleOrigin(t *testing.T) {
	t.Parallel()
	output := "Available within your tailnet:\n\nhttps://fern-host.example.ts.net\n|-- / proxy http://127.0.0.1:8080\n"
	if got, err := tailscaleOrigin(output); err != nil || got != "https://fern-host.example.ts.net" {
		t.Fatalf("tailscale origin = %q, %v", got, err)
	}
	if _, err := tailscaleOrigin("https://one.ts.net https://two.ts.net"); err == nil {
		t.Fatal("accepted ambiguous Tailscale origins")
	}
}

func TestTailscaleLocalOrigin(t *testing.T) {
	t.Parallel()
	if got, err := tailscaleLocalOrigin([]byte(`{"Self":{"DNSName":"fern-host.example.ts.net."}}`)); err != nil || got != "https://fern-host.example.ts.net" {
		t.Fatalf("local origin = %q, %v", got, err)
	}
}
