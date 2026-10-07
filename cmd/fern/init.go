package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nebler/fern/internal/config"
	"gopkg.in/yaml.v3"
)

func runInit(args []string) error {
	flags := newFlagSet("init", "Create a Background Run host configuration.")
	configPath := flags.String("config", "fern.yaml", "configuration destination")
	envPath := flags.String("env-file", "fern.env", "protected environment destination")
	name := flags.String("name", "demo", "repository binding name")
	repo := flags.String("repo", ".", "repository path")
	installationID := flags.Int64("installation-id", 0, "GitHub App installation ID")
	repositoryID := flags.Int64("repository-id", 0, "GitHub repository ID")
	repositoryName := flags.String("repository", "", "GitHub owner/repository")
	modelProvider := flags.String("model-provider", "", "OpenCode model provider ID")
	model := flags.String("model", "", "OpenCode model ID")
	runtimeStorageRoot := flags.String("runtime-storage-root", "/var/lib/fern-runtime", "operator-provisioned Linux XFS project-quota root (not created by init)")
	backgroundImage := flags.String("background-image", "fern/opencode-background-source:dev", "qualified Background Run image")
	backgroundImageID := flags.String("background-image-id", "", "qualified local Background Run image ID")
	listen := flags.String("listen", "127.0.0.1:8080", "remote/device control-plane listen address")
	operatorListen := flags.String("operator-listen", "127.0.0.1:8081", "host/operator listen address")
	backgroundListen := flags.String("background-listen", "127.0.0.1:8443", "live-run loopback listen address")
	remoteOrigin := flags.String("remote-origin", "", "canonical private HTTPS control-plane origin")
	backgroundOrigin := flags.String("background-origin", "", "canonical private HTTPS live-run origin")
	if err := parseFlags(flags, args); err != nil {
		return err
	}
	if *configPath == *envPath {
		return invocationError{message: "configuration and environment destinations must differ"}
	}
	if *backgroundImageID == "" {
		return invocationError{message: "-background-image-id is required; obtain it from docker image inspect after qualification"}
	}
	absRepo, err := filepath.Abs(*repo)
	if err != nil {
		return fmt.Errorf("resolve repository: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("generate control password: %w", err)
	}
	controlSecret := hex.EncodeToString(secret)
	values := config.Config{
		Workspace: config.Workspace{Name: *name, Repo: absRepo, GitHub: config.GitHubApp{
			InstallationID: config.GitHubID(*installationID), Repository: config.GitHubRepository{ID: config.GitHubID(*repositoryID), FullName: *repositoryName}}},
		ControlPassword: controlSecret,
		Proxy:           config.Proxy{Listen: *listen, OperatorListen: *operatorListen, RemoteOrigin: *remoteOrigin},
		Runs: config.RunPolicy{Agent: "build", Model: config.RunModel{Provider: *modelProvider, ID: *model},
			RuntimeStorageRoot: *runtimeStorageRoot,
			RunTimeout:         30 * time.Minute,
			BackgroundImage:    *backgroundImage, BackgroundImageID: *backgroundImageID,
			BackgroundRoute: &config.BackgroundRoute{Listen: *backgroundListen, Origin: *backgroundOrigin}},
	}
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
	if err := writeNewFile(*envPath, []byte("# Keep this file on the Fern host.\nFERN_CONTROL_PASSWORD="+controlSecret+"\n"), 0o600); err != nil {
		_ = os.Remove(*configPath)
		return err
	}
	fmt.Printf("Fern Background Run configuration created\n\nconfig: %s\nsecrets: %s\nrepository: %s\n\n", *configPath, *envPath, absRepo)
	fmt.Printf("Before execution: provision %s on Linux XFS with enforced project quotas for both bytes and inodes, enclosing clone and state-volume directories. Keep durable DB/CAS storage outside this root and reserve host capacity. init does not create or configure quotas. Docker Desktop execution is unsupported.\n\n", *runtimeStorageRoot)
	fmt.Printf("Next:\n  1. Create a private GitHub App (permissions: metadata read, contents write, pull requests write; no webhook), generate a private key, and install the App on %s.\n", *repositoryName)
	step := 2
	if *installationID == 0 {
		fmt.Printf("  %d. Set workspace.github.installationId in %s from the installation URL.\n", step, *configPath)
		step++
	}
	fmt.Printf("  %d. Run: fern credentials set --config %s --env-file %s --app-id <id> --private-key <app.pem>\n  %d. Configure private TLS routing for %s and %s.\n  %d. Run: fern up --config %s --env-file %s\n",
		step, *configPath, *envPath, step+1, *listen, *backgroundListen, step+2, *configPath, *envPath)
	return nil
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
