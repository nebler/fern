package main

import (
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
	values := config.BackgroundConfig{
		Workspace: config.BackgroundWorkspace{Name: *name, Repo: absRepo, GitHub: config.BackgroundGitHubApp{
			InstallationID: *installationID, Repository: config.GitHubRepository{ID: *repositoryID, FullName: *repositoryName}}},
		Control: config.Control{Password: controlSecret}, Listen: *listen, OperatorListen: *operatorListen, RemoteOrigin: *remoteOrigin,
		Tasks: config.TaskPolicy{Agent: "build", Model: config.TaskModel{Provider: *modelProvider, ID: *model},
			AttemptTimeout: 30 * time.Minute, LeaseDuration: 2 * time.Minute, Budget: config.TaskBudget{MaxTurns: 100},
			BackgroundImage: *backgroundImage, BackgroundImageID: *backgroundImageID,
			BackgroundRoute: &config.BackgroundRoute{Listen: *backgroundListen, Origin: *backgroundOrigin}, BackgroundEnvironment: map[string]string{}},
	}
	if err := config.ValidateBackgroundBootstrap(values); err != nil {
		return err
	}
	type initFile struct {
		Workspace struct {
			Name   string `yaml:"name"`
			Repo   string `yaml:"repo"`
			GitHub struct {
				Mode           config.GitHubMode `yaml:"mode"`
				Hostname       string            `yaml:"hostname"`
				InstallationID int64             `yaml:"installationId,omitempty"`
				Repository     struct {
					ID       int64  `yaml:"id"`
					FullName string `yaml:"fullName"`
				} `yaml:"repository"`
			} `yaml:"github"`
		} `yaml:"workspace"`
		Tasks struct {
			Agent string `yaml:"agent"`
			Model struct {
				Provider string `yaml:"provider"`
				ID       string `yaml:"id"`
			} `yaml:"model"`
			AttemptTimeout    string `yaml:"attemptTimeout"`
			LeaseDuration     string `yaml:"leaseDuration"`
			BackgroundImage   string `yaml:"backgroundImage"`
			BackgroundImageID string `yaml:"backgroundImageID"`
			BackgroundRoute   struct {
				Listen string `yaml:"listen"`
				Origin string `yaml:"origin"`
			} `yaml:"backgroundRoute"`
			Budget struct {
				MaxTurns int `yaml:"maxTurns"`
			} `yaml:"budget"`
		} `yaml:"tasks"`
		Control struct {
			Password string `yaml:"password"`
		} `yaml:"control"`
		Proxy struct {
			Listen         string `yaml:"listen"`
			OperatorListen string `yaml:"operatorListen"`
			RemoteOrigin   string `yaml:"remoteOrigin"`
		} `yaml:"proxy"`
	}
	var output initFile
	output.Workspace.Name, output.Workspace.Repo = values.Workspace.Name, values.Workspace.Repo
	output.Workspace.GitHub.Mode, output.Workspace.GitHub.Hostname = config.GitHubModeGitHubAppBroker, "github.com"
	output.Workspace.GitHub.InstallationID = values.Workspace.GitHub.InstallationID
	output.Workspace.GitHub.Repository.ID = values.Workspace.GitHub.Repository.ID
	output.Workspace.GitHub.Repository.FullName = values.Workspace.GitHub.Repository.FullName
	output.Tasks.Agent = values.Tasks.Agent
	output.Tasks.Model.Provider, output.Tasks.Model.ID = values.Tasks.Model.Provider, values.Tasks.Model.ID
	output.Tasks.AttemptTimeout, output.Tasks.LeaseDuration = values.Tasks.AttemptTimeout.String(), values.Tasks.LeaseDuration.String()
	output.Tasks.BackgroundImage, output.Tasks.BackgroundImageID = values.Tasks.BackgroundImage, values.Tasks.BackgroundImageID
	output.Tasks.BackgroundRoute.Listen, output.Tasks.BackgroundRoute.Origin = values.Tasks.BackgroundRoute.Listen, values.Tasks.BackgroundRoute.Origin
	output.Tasks.Budget.MaxTurns = values.Tasks.Budget.MaxTurns
	output.Control.Password = "${FERN_CONTROL_PASSWORD}"
	output.Proxy.Listen, output.Proxy.OperatorListen, output.Proxy.RemoteOrigin = values.Listen, values.OperatorListen, values.RemoteOrigin
	configData, err := yaml.Marshal(output)
	if err != nil {
		return err
	}
	if err := writeNewFile(*configPath, configData, 0o600); err != nil {
		return err
	}
	if err := writeNewFile(*envPath, []byte("# Keep this file on the Fern host.\nFERN_CONTROL_PASSWORD="+controlSecret+"\n"), 0o600); err != nil {
		_ = os.Remove(*configPath)
		return err
	}
	fmt.Printf("Fern Background Run configuration created\n\nconfig: %s\nsecrets: %s\nrepository: %s\n\n", *configPath, *envPath, absRepo)
	if *installationID == 0 {
		fmt.Printf("Next:\n  1. Configure private TLS routing for %s and %s.\n  2. Run: fern up --config %s --env-file %s\n  3. Open http://%s/fern/control, create the GitHub App, and install it on %s.\n  4. Set workspace.github.installationId in %s from the GitHub installation URL, then restart Fern.\n",
			*listen, *backgroundListen, *configPath, *envPath, *operatorListen, *repositoryName, *configPath)
	} else {
		fmt.Printf("Next:\n  1. Import the matching GitHub App credentials with fern credentials import.\n  2. Configure private TLS routing for %s and %s.\n  3. Run: fern up --config %s --env-file %s\n",
			*listen, *backgroundListen, *configPath, *envPath)
	}
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
