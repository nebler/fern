package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nebler/fern/internal/config"
	"gopkg.in/yaml.v3"
)

func runInit(args []string) error {
	flags := newFlagSet("init", "Create a Background Run host configuration.")
	configPath, envPath := addConfigFlags(flags)
	repo := flags.String("repo", ".", "repository path")
	installationID := flags.Int64("installation-id", 0, "GitHub App installation ID (may be added to the file later)")
	repositoryID := flags.Int64("repository-id", 0, "GitHub repository ID")
	repositoryName := flags.String("repository", "", "GitHub owner/repository")
	modelProvider := flags.String("model-provider", "", "OpenCode model provider ID")
	model := flags.String("model", "", "OpenCode model ID")
	runtimeStorageRoot := flags.String("runtime-storage-root", "/var/lib/fern-runtime", "operator-provisioned Linux XFS project-quota root (not created by init)")
	backgroundImage := flags.String("background-image", "fern/opencode-background-source:dev", "qualified Background Run image")
	backgroundImageID := flags.String("background-image-id", "", "qualified local Background Run image ID (required)")
	remoteOrigin := flags.String("remote-origin", "", "canonical private HTTPS control-plane origin")
	backgroundOrigin := flags.String("background-origin", "", "canonical private HTTPS live-run origin")
	if err := parseFlags(flags, args); err != nil {
		return err
	}
	if *configPath == *envPath {
		return invocationError{message: "--config and --env-file must name different files"}
	}
	if *backgroundImageID == "" {
		return invocationError{message: "--background-image-id is required; obtain it from docker image inspect after qualification"}
	}
	absRepo, err := filepath.Abs(*repo)
	if err != nil {
		return fmt.Errorf("resolve repository: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("generate control password: %w", err)
	}
	values := config.Default()
	values.ControlPassword = hex.EncodeToString(secret)
	values.Workspace.Repo = absRepo
	values.Workspace.GitHub = config.GitHubApp{InstallationID: config.GitHubID(*installationID),
		Repository: config.GitHubRepository{ID: config.GitHubID(*repositoryID), FullName: *repositoryName}}
	values.Proxy.RemoteOrigin = *remoteOrigin
	values.Runs.Model = config.RunModel{Provider: *modelProvider, ID: *model}
	values.Runs.RuntimeStorageRoot = *runtimeStorageRoot
	values.Runs.BackgroundImage, values.Runs.BackgroundImageID = *backgroundImage, *backgroundImageID
	values.Runs.BackgroundRoute = &config.BackgroundRoute{Listen: "127.0.0.1:8443", Origin: *backgroundOrigin}
	if err := config.ValidateBootstrap(values); err != nil {
		return err
	}
	var configData bytes.Buffer
	encoder := yaml.NewEncoder(&configData)
	encoder.SetIndent(2)
	if err := errors.Join(encoder.Encode(values), encoder.Close()); err != nil {
		return err
	}
	if err := writeNewFile(*configPath, configData.Bytes(), 0o600); err != nil {
		return err
	}
	environment := "# Keep this file on the Fern host.\n" + config.ControlPasswordVariable + "=" + values.ControlPassword + "\n"
	if err := writeNewFile(*envPath, []byte(environment), 0o600); err != nil {
		_ = os.Remove(*configPath)
		return err
	}
	printInitNextSteps(os.Stdout, values, *configPath, *envPath)
	return nil
}

// printInitNextSteps tells the operator what init deliberately left undone.
func printInitNextSteps(output io.Writer, values config.Config, configPath, envPath string) {
	fmt.Fprintf(output, "Fern Background Run configuration created\n\nconfig: %s\nsecrets: %s\nrepository: %s\n\n", configPath, envPath, values.Workspace.Repo)
	fmt.Fprintf(output, "Before execution: provision %s on Linux XFS with enforced project quotas for both bytes and inodes, enclosing clone and state-volume directories. Keep durable DB/CAS storage outside this root and reserve host capacity. init does not create or configure quotas. Docker Desktop execution is unsupported.\n\n", values.Runs.RuntimeStorageRoot)
	steps := []string{fmt.Sprintf("Create a private GitHub App (permissions: metadata read, contents write, pull requests write; no webhook), generate a private key, and install the App on %s.", values.Workspace.GitHub.Repository.FullName)}
	if values.Workspace.GitHub.InstallationID == 0 {
		steps = append(steps, fmt.Sprintf("Set workspace.github.installationId in %s from the installation URL.", configPath))
	}
	steps = append(steps,
		fmt.Sprintf("Run: fern credentials set --config %s --env-file %s --app-id <id> --private-key <app.pem>", configPath, envPath),
		fmt.Sprintf("Configure private TLS routing for %s and %s.", values.Proxy.Listen, values.Runs.BackgroundRoute.Listen),
		fmt.Sprintf("Run: fern up --config %s --env-file %s", configPath, envPath))
	fmt.Fprintln(output, "Next:")
	for index, step := range steps {
		fmt.Fprintf(output, "  %d. %s\n", index+1, step)
	}
}

func writeNewFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create parent for %q: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to overwrite existing file %q", path)
		}
		return fmt.Errorf("create %q: %w", path, err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write %q: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("sync %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close %q: %w", path, err)
	}
	return nil
}
