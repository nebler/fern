package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/safeio"
)

// validateCredentials is replaced in tests to avoid live GitHub calls.
var validateCredentials = liveCredentialValidator

const maxPrivateKeyFileBytes = 64 << 10

// runCredentialSet stores the credentials of a GitHub App the operator created
// by hand, after proving them live against the configured installation and
// repository. It takes the host lease, so Fern must be stopped.
func runCredentialSet(args []string) error {
	fs := newFlagSet("credentials set", "Validate and store the GitHub App ID and private key while Fern is stopped.")
	configPath, envPath := addConfigFlags(fs)
	appID := fs.Int64("app-id", 0, "GitHub App ID (required)")
	keyPath := fs.String("private-key", "", "GitHub App private key PEM file (required)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *appID <= 0 || *keyPath == "" {
		return invocationError{message: "--app-id and --private-key are required"}
	}
	stateDirectory, err := statePath("")
	if err != nil {
		return fmt.Errorf("determine Fern state directory: %w", err)
	}
	privateKeyPEM, err := readPrivateKeyFile(*keyPath)
	if err != nil {
		return err
	}
	credentials, err := githubapp.NewAppCredentials(*appID, privateKeyPEM)
	if err != nil {
		return fmt.Errorf("read GitHub App private key %s: %w", *keyPath, err)
	}
	cfg, err := loadCommandConfig(*configPath, *envPath)
	if err != nil {
		return err
	}
	if err := config.ValidateBootstrap(cfg); err != nil {
		return err
	}
	lease, err := safeio.AcquireLease(filepath.Join(stateDirectory, "locks"), cfg.Workspace.Name)
	if err != nil {
		return fmt.Errorf("setting credentials requires Fern to be stopped: %w", err)
	}
	defer func() { _ = lease.Release() }()
	store, err := githubapp.NewCredentialStore(filepath.Join(stateDirectory, "github-app"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := validateCredentials(ctx, cfg, credentials); err != nil {
		return fmt.Errorf("GitHub App credentials failed live validation: %w", err)
	}
	if err := store.Save(credentials); err != nil {
		return err
	}
	_, err = fmt.Fprintf(os.Stdout, "stored GitHub App credentials for app ID %d; restart Fern to use them\n", *appID)
	return err
}

func readPrivateKeyFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read GitHub App private key: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPrivateKeyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read GitHub App private key: %w", err)
	}
	if len(data) > maxPrivateKeyFileBytes {
		return nil, fmt.Errorf("GitHub App private key %s exceeds %d bytes", path, maxPrivateKeyFileBytes)
	}
	return data, nil
}

// liveCredentialValidator proves the App can sign, sees the configured
// installation, and reaches the configured repository with the permissions
// Background Runs need.
func liveCredentialValidator(ctx context.Context, cfg config.Config, app githubapp.AppCredentials) error {
	github := cfg.Workspace.GitHub
	if github.InstallationID <= 0 {
		return errors.New("workspace.github.installationId is not configured; install the GitHub App and set it from the installation URL first")
	}
	signer, err := githubapp.NewJWTSigner(app.AppID(), app.PrivateKey())
	if err != nil {
		return fmt.Errorf("GitHub App signing validation failed: %w", err)
	}
	client, err := githubapp.NewClient(http.DefaultClient, signer)
	if err != nil {
		return fmt.Errorf("GitHub App client validation failed: %w", err)
	}
	discovery, err := githubapp.NewInstallationClient(http.DefaultClient, signer, client, time.Now)
	if err != nil {
		return fmt.Errorf("GitHub App discovery validation failed: %w", err)
	}
	installations, err := discovery.ListAppInstallations(ctx)
	if err != nil {
		return fmt.Errorf("list GitHub App installations: %w", err)
	}
	repositories, err := discovery.ListInstallationRepositories(ctx, int64(github.InstallationID))
	if err != nil {
		return fmt.Errorf("list repositories of installation %d: %w", github.InstallationID, err)
	}
	return githubapp.SelectRepository(installations, repositories, int64(github.InstallationID), int64(github.Repository.ID), github.Repository.FullName)
}
