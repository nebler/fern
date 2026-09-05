// Package config loads and validates Fern's current Background Run configuration.
package config

import "time"

// GitHubRepository is the canonical GitHub authority binding.
type GitHubRepository struct {
	ID       int64
	FullName string
}

const GitHubModeGitHubAppBroker = "github-app-broker"

// GitHubApp holds the GitHub.com App installation and repository binding.
type GitHubApp struct {
	InstallationID int64
	Repository     GitHubRepository
}

type Workspace struct {
	Name   string
	Repo   string
	GitHub GitHubApp
}

type TaskModel struct {
	Provider string
	ID       string
}

type BackgroundRoute struct {
	Listen string
	Origin string
}

type TaskPolicy struct {
	Agent             string
	Model             TaskModel
	AttemptTimeout    time.Duration
	LeaseDuration     time.Duration
	BackgroundImage   string
	BackgroundImageID string
	BackgroundRoute   *BackgroundRoute
}

// Control contains only the host-side control-plane credential.
type Control struct {
	Password string
}

// Config is the single current configuration shape. ValidateBootstrap permits
// a pending App installation; Validate requires an installed execution binding.
type Config struct {
	Workspace      Workspace
	Control        Control
	Tasks          TaskPolicy
	Listen         string
	OperatorListen string
	RemoteOrigin   string
}

// Overrides contains only explicitly supplied, current CLI settings.
type Overrides struct {
	Name           *string
	Repo           *string
	Listen         *string
	OperatorListen *string
}

func Default(repo string) Config {
	return Config{Workspace: Workspace{Name: "demo", Repo: repo},
		Listen: "127.0.0.1:8080", OperatorListen: "127.0.0.1:8081"}
}
