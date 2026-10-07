package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/githubapp"
)

func TestCredentialSetValidatesBeforeStoring(t *testing.T) {
	directory := t.TempDir()
	configPath, envPath := filepath.Join(directory, "fern.yaml"), filepath.Join(directory, "fern.env")
	if err := runInit([]string{"--config", configPath, "--env-file", envPath, "--repo", directory,
		"--installation-id", "77", "--repository", "owner/repository", "--repository-id", "123",
		"--model-provider", "anthropic", "--model", "model",
		"--background-image-id", "sha256:" + strings.Repeat("b", 64),
		"--remote-origin", "https://fern.example.ts.net", "--background-origin", "https://fern.example.ts.net:8443"}); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(directory, "app.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	garbagePath := filepath.Join(directory, "garbage.pem")
	if err := os.WriteFile(garbagePath, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(directory, "home")
	t.Setenv("HOME", home)
	stateDirectory := filepath.Join(home, ".fern")
	set := func(appID, path string) error {
		return runCredentialSet([]string{"--config", configPath, "--env-file", envPath,
			"--app-id", appID, "--private-key", path})
	}

	var validated []int64
	validation := errors.New("installation not visible")
	original := validateCredentials
	t.Cleanup(func() { validateCredentials = original })
	validateCredentials = func(_ context.Context, cfg config.Config, app githubapp.AppCredentials) error {
		if cfg.Workspace.GitHub.InstallationID != 77 {
			t.Errorf("validated against installation %d", cfg.Workspace.GitHub.InstallationID)
		}
		validated = append(validated, app.AppID())
		return validation
	}

	var invocation invocationError
	if err := set("0", keyPath); !errors.As(err, &invocation) {
		t.Fatalf("missing app ID error = %v", err)
	}
	if err := set("42", garbagePath); !errors.Is(err, githubapp.ErrInvalidPrivateKey) {
		t.Fatalf("invalid key error = %v", err)
	}
	if err := set("42", keyPath); !errors.Is(err, validation) {
		t.Fatalf("failed validation error = %v", err)
	}
	store, err := githubapp.NewCredentialStore(filepath.Join(stateDirectory, "github-app"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, githubapp.ErrCredentialsNotFound) {
		t.Fatalf("rejected credentials were stored: %v", err)
	}

	validation = nil
	if err := set("42", keyPath); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Load()
	if err != nil || stored.AppID() != 42 || len(validated) != 2 {
		t.Fatalf("stored app ID = %d, err = %v, validations = %v", stored.AppID(), err, validated)
	}
}

func TestLiveCredentialValidatorRequiresInstallationID(t *testing.T) {
	t.Parallel()
	if err := liveCredentialValidator(t.Context(), config.Config{}, githubapp.AppCredentials{}); err == nil ||
		!strings.Contains(err.Error(), "installationId") {
		t.Fatalf("error = %v", err)
	}
}
