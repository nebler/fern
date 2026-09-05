package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/credentialbundle"
	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/hostlease"
)

type repeatedFlag []string

func (values *repeatedFlag) String() string { return strings.Join(*values, ",") }
func (values *repeatedFlag) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("value must not be empty")
	}
	*values = append(*values, value)
	return nil
}

type appCredentialStore interface {
	Load() (githubapp.AppCredentials, error)
	Save(githubapp.AppCredentials) error
	Delete() error
}

var openAppCredentialStore = func(directory string) (appCredentialStore, error) {
	return githubapp.NewCredentialStore(directory)
}

type credentialCandidateValidator func(context.Context, config.BackgroundConfig, *githubapp.AppCredentials) error

var validateCredentialCandidates credentialCandidateValidator = liveCredentialValidator

type credentialOptions struct {
	configPath, envPath, stateDirectory string
}

func credentialFlags(command, description string) (*flag.FlagSet, *credentialOptions) {
	fs := newFlagSet(command, description)
	options := &credentialOptions{}
	fs.StringVar(&options.configPath, "config", defaultBackupConfig, "configuration file")
	fs.StringVar(&options.envPath, "env-file", defaultBackupEnv, "protected environment file")
	if state, err := statePath(""); err == nil {
		options.stateDirectory = filepath.Clean(state)
	}
	fs.StringVar(&options.stateDirectory, "state-dir", options.stateDirectory, "Fern state directory")
	return fs, options
}

func runCredentialExport(args []string, log *slog.Logger) error {
	fs, options := credentialFlags("credentials export", "Export active GitHub credentials to an encrypted age bundle.")
	output := fs.String("output", "", "encrypted bundle output path (required)")
	generation := fs.String("generation", "", "credential generation identifier")
	var recipientFlags repeatedFlag
	fs.Var(&recipientFlags, "recipient", "age X25519 recipient (repeatable, required)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *output == "" || len(recipientFlags) == 0 {
		return invocationError{message: "--output and at least one --recipient are required"}
	}
	recipients, err := credentialbundle.ParseRecipients(recipientFlags)
	if err != nil {
		return err
	}
	if *generation == "" {
		*generation, err = newBackupGeneration()
		if err != nil {
			return err
		}
	}
	operation, err := openCredentialOperation(*options, log)
	if err != nil {
		return err
	}
	defer operation.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	bundle, err := operation.snapshot(ctx, *generation)
	if err != nil {
		return err
	}
	if err := credentialbundle.WriteFile(*output, bundle, recipients); err != nil {
		return err
	}
	fingerprint, _ := bundle.Fingerprint()
	fmt.Fprintf(os.Stdout, "exported encrypted credential generation %s\nfingerprint: sha256:%s\n", bundle.Epoch, fingerprint)
	return nil
}

func runCredentialImport(args []string, log *slog.Logger, rotation bool) error {
	name, description := "credentials import", "Validate and activate an encrypted GitHub credential bundle."
	if rotation {
		name, description = "credentials rotate", "Rollback-safely rotate GitHub credentials from an encrypted bundle."
	}
	fs, options := credentialFlags(name, description)
	input := fs.String("input", "", "encrypted bundle input path (required)")
	rollbackOutput := fs.String("rollback-output", "", "encrypted prior-generation rollback artifact")
	acknowledge := fs.Bool("acknowledge-external-revocation", false, "acknowledge that old credentials must be revoked externally")
	var identityPaths, rollbackRecipientFlags repeatedFlag
	fs.Var(&identityPaths, "identity", "private age X25519 identity file (repeatable, required)")
	fs.Var(&rollbackRecipientFlags, "rollback-recipient", "age recipient for the rollback artifact (repeatable)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *input == "" || len(identityPaths) == 0 {
		return invocationError{message: "--input and at least one --identity are required"}
	}
	if rotation && !*acknowledge {
		return invocationError{message: "rotation requires --acknowledge-external-revocation; Fern cannot revoke the old GitHub key or token"}
	}
	identities, err := credentialbundle.LoadIdentities(identityPaths)
	if err != nil {
		return err
	}
	candidate, err := credentialbundle.ReadFile(*input, identities)
	if err != nil {
		return err
	}
	rollbackRecipients := credentialbundle.RecipientsForIdentities(identities)
	if len(rollbackRecipientFlags) > 0 {
		rollbackRecipients, err = credentialbundle.ParseRecipients(rollbackRecipientFlags)
		if err != nil {
			return err
		}
	}
	if rotation && len(rollbackRecipients) == 0 {
		return invocationError{message: "a --rollback-recipient is required for identities without a derivable X25519 recipient"}
	}
	if *rollbackOutput == "" {
		generation, generationErr := newBackupGeneration()
		if generationErr != nil {
			return generationErr
		}
		*rollbackOutput = *input + ".rollback-" + generation + ".age"
	}
	operation, err := openCredentialOperation(*options, log)
	if err != nil {
		return err
	}
	defer operation.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rollbackPath, err := operation.activate(ctx, candidate, rotation, *rollbackOutput, rollbackRecipients)
	if err != nil {
		return err
	}
	fingerprint, _ := candidate.Fingerprint()
	fmt.Fprintf(os.Stdout, "activated credential generation %s\nfingerprint: sha256:%s\n", candidate.Epoch, fingerprint)
	if rollbackPath != "" {
		fmt.Fprintf(os.Stdout, "encrypted rollback: %s\n", rollbackPath)
	}
	if rotation {
		fmt.Fprintln(os.Stdout, "external revocation required: revoke the superseded GitHub key or token after verification; Fern cannot automate revocation")
	}
	return nil
}

type credentialOperation struct {
	options credentialOptions
	config  config.BackgroundConfig
	lease   *hostlease.Lease
	store   appCredentialStore
}

func openCredentialOperation(options credentialOptions, log *slog.Logger) (*credentialOperation, error) {
	if options.stateDirectory == "" {
		return nil, errors.New("cannot determine Fern state directory")
	}
	cfg, err := loadBackgroundCommandConfig(options.configPath, true, options.envPath, config.BackgroundOverrides{})
	if err != nil {
		return nil, err
	}
	lease, err := hostlease.Acquire(filepath.Join(options.stateDirectory, "locks"), cfg.Workspace.Name)
	if err != nil {
		return nil, fmt.Errorf("credential operation requires the offline Fern lease: %w", err)
	}
	operation := &credentialOperation{options: options, config: cfg, lease: lease}
	store, err := openAppCredentialStore(filepath.Join(options.stateDirectory, "github-app"))
	if err != nil {
		operation.close()
		return nil, err
	}
	operation.store = store
	return operation, nil
}

func (operation *credentialOperation) close() {
	if operation == nil {
		return
	}
	if operation.lease != nil {
		_ = operation.lease.Release()
	}
}

func (operation *credentialOperation) snapshot(ctx context.Context, generation string) (credentialbundle.Bundle, error) {
	binding, err := credentialBinding(operation.config)
	if err != nil {
		return credentialbundle.Bundle{}, err
	}
	bundle := credentialbundle.Bundle{Version: credentialbundle.Version, Epoch: generation, CreatedAt: time.Now().UTC(), Binding: binding}
	credentials, err := operation.store.Load()
	if err != nil {
		return credentialbundle.Bundle{}, err
	}
	bundle.GitHubApp, err = githubapp.MarshalStoredCredentials(credentials)
	if err != nil {
		return credentialbundle.Bundle{}, err
	}
	bundle.Binding.AppID = credentials.AppID()
	return bundle, nil
}

func (operation *credentialOperation) activate(ctx context.Context, candidate credentialbundle.Bundle, rotation bool, rollbackPath string, recipients []age.Recipient) (string, error) {
	want, err := credentialBinding(operation.config)
	if err != nil {
		return "", err
	}
	if candidate.Binding.Workspace != want.Workspace || candidate.Binding.Mode != want.Mode || candidate.Binding.Hostname != want.Hostname ||
		candidate.Binding.InstallationID != want.InstallationID || candidate.Binding.RepositoryID != want.RepositoryID || candidate.Binding.Repository != want.Repository {
		return "", errors.New("credential bundle does not match the configured workspace and GitHub identity")
	}
	if len(candidate.WorkspaceGH) != 0 || len(candidate.GitHubApp) == 0 {
		return "", errors.New("credential bundle component does not match github-app-broker mode")
	}
	parsed, err := githubapp.ParseStoredCredentials(candidate.GitHubApp)
	if err != nil || parsed.AppID() != candidate.Binding.AppID {
		return "", errors.New("GitHub App credential candidate is invalid")
	}
	if current, err := operation.store.Load(); err == nil && current.AppID() != parsed.AppID() {
		return "", errors.New("GitHub App rotation cannot change the configured App identity")
	} else if err != nil && !errors.Is(err, githubapp.ErrCredentialsNotFound) {
		return "", err
	}
	validationCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err = validateCredentialCandidates(validationCtx, operation.config, &parsed)
	cancel()
	if err != nil {
		return "", errors.New("credential candidate failed live GitHub identity or permission validation")
	}
	rollbackGeneration, err := newBackupGeneration()
	if err != nil {
		return "", err
	}
	rollback, err := operation.snapshot(ctx, rollbackGeneration)
	priorExists := err == nil
	if err != nil && !errors.Is(err, githubapp.ErrCredentialsNotFound) {
		return "", fmt.Errorf("capture credential rollback generation: %w", err)
	}
	if rotation && !priorExists {
		return "", errors.New("rotation requires an active prior credential generation")
	}
	if priorExists {
		if len(recipients) == 0 {
			return "", invocationError{message: "a --rollback-recipient is required for identities without a derivable X25519 recipient"}
		}
		if err := credentialbundle.WriteFile(rollbackPath, rollback, recipients); err != nil {
			return "", fmt.Errorf("write encrypted credential rollback artifact: %w", err)
		}
	}
	if err := operation.store.Save(parsed); err != nil {
		if !priorExists {
			if rollbackErr := operation.store.Delete(); rollbackErr != nil {
				return "", errors.New("GitHub App bootstrap and restoration of the empty store failed")
			}
			return "", errors.New("GitHub App bootstrap failed; the credential store remains empty")
		}
		prior, parseErr := githubapp.ParseStoredCredentials(rollback.GitHubApp)
		if parseErr != nil {
			return "", errors.New("GitHub App replacement failed; use the retained encrypted rollback artifact")
		}
		rollbackErr := operation.store.Save(prior)
		if rollbackErr != nil {
			return "", errors.New("GitHub App replacement and automatic rollback failed; use the retained encrypted rollback artifact")
		}
		return "", errors.New("GitHub App replacement failed and the prior generation was restored")
	}
	if priorExists {
		return rollbackPath, nil
	}
	return "", nil
}

func credentialBinding(cfg config.BackgroundConfig) (credentialbundle.Binding, error) {
	github := cfg.Workspace.GitHub
	if cfg.Workspace.Name == "" || github.InstallationID <= 0 || github.Repository.ID <= 0 || github.Repository.FullName == "" {
		return credentialbundle.Binding{}, errors.New("GitHub credentials are not configured")
	}
	return credentialbundle.Binding{
		Workspace: cfg.Workspace.Name, Mode: string(config.GitHubModeGitHubAppBroker), Hostname: "github.com",
		InstallationID: github.InstallationID, RepositoryID: github.Repository.ID, Repository: github.Repository.FullName,
	}, nil
}

func liveCredentialValidator(ctx context.Context, cfg config.BackgroundConfig, app *githubapp.AppCredentials) error {
	github := cfg.Workspace.GitHub
	if app != nil {
		signer, err := githubapp.NewJWTSigner(app.AppID(), app.PrivateKey())
		if err != nil {
			return errors.New("GitHub App signing validation failed")
		}
		client, err := githubapp.NewClient(http.DefaultClient, signer)
		if err != nil {
			return errors.New("GitHub App client validation failed")
		}
		discovery, err := githubapp.NewInstallationClient(http.DefaultClient, signer, client, time.Now)
		if err != nil {
			return errors.New("GitHub App discovery validation failed")
		}
		installations, err := discovery.ListAppInstallations(ctx)
		if err != nil {
			return err
		}
		repositories, err := discovery.ListInstallationRepositories(ctx, github.InstallationID)
		if err != nil {
			return err
		}
		_, _, err = githubapp.SelectRepository(installations, repositories, github.InstallationID, github.Repository.ID, github.Repository.FullName)
		return err
	}
	return errors.New("GitHub App credential is missing")
}
