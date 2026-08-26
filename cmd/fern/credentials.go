package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/credentialbundle"
	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/registry"
	fernruntime "github.com/nebler/fern/internal/runtime"
	"gopkg.in/yaml.v3"
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

type credentialDocker interface {
	Status(context.Context, string) (fernruntime.Observation, error)
	ExportWorkspaceGH(context.Context, fernruntime.Spec) ([]byte, error)
	ReplaceWorkspaceGH(context.Context, fernruntime.Spec, []byte, string) error
	Close() error
}

var openCredentialDocker = func(log *slog.Logger) (credentialDocker, error) { return newDocker(log) }

type credentialCandidateValidator func(context.Context, config.Config, *githubapp.AppCredentials, string) error

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
	if len(rollbackRecipients) == 0 {
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
	if err := operation.activate(ctx, candidate, *rollbackOutput, rollbackRecipients); err != nil {
		return err
	}
	fingerprint, _ := candidate.Fingerprint()
	fmt.Fprintf(os.Stdout, "activated credential generation %s\nfingerprint: sha256:%s\nencrypted rollback: %s\n", candidate.Epoch, fingerprint, *rollbackOutput)
	if rotation {
		fmt.Fprintln(os.Stdout, "external revocation required: revoke the superseded GitHub key or token after verification; Fern cannot automate revocation")
	}
	return nil
}

type credentialOperation struct {
	options credentialOptions
	config  config.Config
	spec    fernruntime.Spec
	lease   *registry.Lease
	docker  credentialDocker
	store   *githubapp.CredentialStore
}

func openCredentialOperation(options credentialOptions, log *slog.Logger) (*credentialOperation, error) {
	if options.stateDirectory == "" {
		return nil, errors.New("cannot determine Fern state directory")
	}
	cfg, spec, err := loadBackupSpec(backupOptions{configPath: options.configPath, envPath: options.envPath, stateDirectory: options.stateDirectory})
	if err != nil {
		return nil, err
	}
	lease, err := registry.Acquire(filepath.Join(options.stateDirectory, "locks"), spec.Name)
	if err != nil {
		return nil, fmt.Errorf("credential operation requires the offline workspace lease: %w", err)
	}
	operation := &credentialOperation{options: options, config: cfg, spec: spec, lease: lease}
	docker, err := openCredentialDocker(log)
	if err != nil {
		operation.close()
		return nil, err
	}
	operation.docker = docker
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	observation, err := docker.Status(ctx, spec.Name)
	if err != nil {
		operation.close()
		return nil, fmt.Errorf("inspect workspace before credential operation: %w", err)
	}
	if observation.State != fernruntime.StateAbsent {
		operation.close()
		return nil, fmt.Errorf("credential operation requires absent compute; run 'fern down' first (state: %s)", observation.State)
	}
	store, err := githubapp.NewCredentialStore(filepath.Join(options.stateDirectory, "github-app"))
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
	if operation.docker != nil {
		_ = operation.docker.Close()
	}
	if operation.lease != nil {
		_ = operation.lease.Release()
	}
}

func (operation *credentialOperation) requireAbsent(ctx context.Context) error {
	observation, err := operation.docker.Status(ctx, operation.spec.Name)
	if err != nil {
		return errors.New("workspace state could not be verified before credential mutation")
	}
	if observation.State != fernruntime.StateAbsent {
		return fmt.Errorf("credential mutation requires absent compute (state: %s)", observation.State)
	}
	return nil
}

func (operation *credentialOperation) snapshot(ctx context.Context, generation string) (credentialbundle.Bundle, error) {
	binding, err := credentialBinding(operation.config)
	if err != nil {
		return credentialbundle.Bundle{}, err
	}
	bundle := credentialbundle.Bundle{Version: credentialbundle.Version, Epoch: generation, CreatedAt: time.Now().UTC(), Binding: binding}
	switch operation.config.Workspace.GitHub.Mode {
	case config.GitHubModeGitHubAppBroker:
		credentials, err := operation.store.Load()
		if err != nil {
			return credentialbundle.Bundle{}, err
		}
		bundle.GitHubApp, err = githubapp.MarshalStoredCredentials(credentials)
		if err != nil {
			return credentialbundle.Bundle{}, err
		}
		bundle.Binding.AppID = credentials.AppID()
	case config.GitHubModeWorkspaceGH:
		bundle.WorkspaceGH, err = operation.docker.ExportWorkspaceGH(ctx, operation.spec)
		if err != nil {
			return credentialbundle.Bundle{}, err
		}
	default:
		return credentialbundle.Bundle{}, errors.New("configured GitHub credential mode is unsupported")
	}
	return bundle, nil
}

func (operation *credentialOperation) activate(ctx context.Context, candidate credentialbundle.Bundle, rollbackPath string, recipients []age.Recipient) error {
	want, err := credentialBinding(operation.config)
	if err != nil {
		return err
	}
	if candidate.Binding.Workspace != want.Workspace || candidate.Binding.Mode != want.Mode || candidate.Binding.Hostname != want.Hostname ||
		candidate.Binding.InstallationID != want.InstallationID || candidate.Binding.RepositoryID != want.RepositoryID || candidate.Binding.Repository != want.Repository {
		return errors.New("credential bundle does not match the configured workspace and GitHub identity")
	}
	var appCandidate *githubapp.AppCredentials
	var ghToken string
	switch operation.config.Workspace.GitHub.Mode {
	case config.GitHubModeGitHubAppBroker:
		if len(candidate.WorkspaceGH) != 0 || len(candidate.GitHubApp) == 0 {
			return errors.New("credential bundle component does not match github-app-broker mode")
		}
		parsed, err := githubapp.ParseStoredCredentials(candidate.GitHubApp)
		if err != nil || parsed.AppID() != candidate.Binding.AppID {
			return errors.New("GitHub App credential candidate is invalid")
		}
		if current, err := operation.store.Load(); err == nil && current.AppID() != parsed.AppID() {
			return errors.New("GitHub App rotation cannot change the configured App identity")
		} else if err != nil && !errors.Is(err, githubapp.ErrCredentialsNotFound) {
			return err
		}
		appCandidate = &parsed
	case config.GitHubModeWorkspaceGH:
		if len(candidate.GitHubApp) != 0 || len(candidate.WorkspaceGH) == 0 || candidate.Binding.AppID != 0 {
			return errors.New("credential bundle component does not match workspace-gh mode")
		}
		hosts, err := fernruntime.WorkspaceGHFile(candidate.WorkspaceGH, "hosts.yml")
		if err != nil {
			return err
		}
		ghToken, err = parseWorkspaceGHToken(hosts, want.Hostname)
		if err != nil {
			return err
		}
	}
	validationCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err = validateCredentialCandidates(validationCtx, operation.config, appCandidate, ghToken)
	cancel()
	if err != nil {
		return errors.New("credential candidate failed live GitHub identity or permission validation")
	}
	if err := operation.requireAbsent(ctx); err != nil {
		return err
	}
	rollbackGeneration, err := newBackupGeneration()
	if err != nil {
		return err
	}
	rollback, err := operation.snapshot(ctx, rollbackGeneration)
	if err != nil {
		return fmt.Errorf("capture credential rollback generation: %w", err)
	}
	if err := credentialbundle.WriteFile(rollbackPath, rollback, recipients); err != nil {
		return fmt.Errorf("write encrypted credential rollback artifact: %w", err)
	}
	if err := operation.requireAbsent(ctx); err != nil {
		return fmt.Errorf("%w; encrypted rollback retained at %s", err, rollbackPath)
	}
	if appCandidate != nil {
		if err := operation.store.Save(*appCandidate); err != nil {
			prior, parseErr := githubapp.ParseStoredCredentials(rollback.GitHubApp)
			if parseErr != nil {
				return errors.New("GitHub App replacement failed; use the retained encrypted rollback artifact")
			}
			rollbackErr := operation.store.Save(prior)
			if rollbackErr != nil {
				return errors.New("GitHub App replacement and automatic rollback failed; use the retained encrypted rollback artifact")
			}
			return errors.New("GitHub App replacement failed and the prior generation was restored")
		}
		return nil
	}
	if err := operation.docker.ReplaceWorkspaceGH(ctx, operation.spec, candidate.WorkspaceGH, candidate.Epoch); err != nil {
		return fmt.Errorf("replace workspace-gh credentials; rollback artifact retained at %s: %w", rollbackPath, err)
	}
	return nil
}

func credentialBinding(cfg config.Config) (credentialbundle.Binding, error) {
	if cfg.Workspace.GitHub == nil {
		return credentialbundle.Binding{}, errors.New("GitHub credentials are not configured")
	}
	github := cfg.Workspace.GitHub
	return credentialbundle.Binding{
		Workspace: cfg.Workspace.Name, Mode: string(github.Mode), Hostname: github.Hostname,
		InstallationID: github.InstallationID, RepositoryID: github.Repository.ID, Repository: github.Repository.FullName,
	}, nil
}

type ghHostsFile map[string]struct {
	User       string `yaml:"user"`
	OAuthToken string `yaml:"oauth_token"`
	Users      map[string]struct {
		OAuthToken string `yaml:"oauth_token"`
	} `yaml:"users"`
}

func parseWorkspaceGHToken(payload []byte, hostname string) (string, error) {
	if len(payload) == 0 || len(payload) > 1<<20 {
		return "", errors.New("workspace-gh hosts.yml is invalid")
	}
	var hosts ghHostsFile
	decoder := yaml.NewDecoder(strings.NewReader(string(payload)))
	if err := decoder.Decode(&hosts); err != nil || len(hosts) != 1 {
		return "", errors.New("workspace-gh hosts.yml is invalid")
	}
	host, ok := hosts[hostname]
	if !ok {
		return "", errors.New("workspace-gh hosts.yml does not match the configured hostname")
	}
	token := host.OAuthToken
	if host.User != "" {
		user, ok := host.Users[host.User]
		if !ok {
			return "", errors.New("workspace-gh hosts.yml active user is missing")
		}
		token = user.OAuthToken
	}
	if len(token) < 20 || len(token) > 512 || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\x00\r\n") {
		return "", errors.New("workspace-gh token is invalid")
	}
	return token, nil
}

func liveCredentialValidator(ctx context.Context, cfg config.Config, app *githubapp.AppCredentials, ghToken string) error {
	github := cfg.Workspace.GitHub
	if github == nil {
		return errors.New("GitHub configuration is missing")
	}
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
	if ghToken == "" {
		return errors.New("workspace-gh token is missing")
	}
	endpoint := "https://api.github.com/repos/" + strings.ReplaceAll(url.PathEscape(github.Repository.FullName), "%2F", "/")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("construct GitHub repository validation request")
	}
	request.Header.Set("Authorization", "Bearer "+ghToken)
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return errors.New("GitHub repository validation request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return fmt.Errorf("GitHub repository validation returned status %d", response.StatusCode)
	}
	var result struct {
		ID          int64  `json:"id"`
		FullName    string `json:"full_name"`
		Permissions struct {
			Pull bool `json:"pull"`
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	if err := decoder.Decode(&result); err != nil || result.ID != github.Repository.ID || result.FullName != github.Repository.FullName || !result.Permissions.Pull || !result.Permissions.Push {
		return errors.New("workspace-gh credential does not match the configured repository identity or permissions")
	}
	return nil
}
