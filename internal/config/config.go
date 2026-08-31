// Package config loads, merges, and validates Fern's fern.yaml configuration:
// defaults, then the strict YAML file, then explicit overrides, followed by
// environment expansion and the validation gauntlet.
package config

import "time"

// GitHubRepository identifies a GitHub binding by numeric repository ID and
// canonical owner/repository full name.
type GitHubRepository struct {
	ID       int64
	FullName string
}

// GitHubMode selects how a workspace obtains GitHub credentials.
type GitHubMode string

const (
	// GitHubModeWorkspaceGH lets the workspace authenticate with its own gh CLI
	// credentials.
	GitHubModeWorkspaceGH GitHubMode = "workspace-gh"
	// GitHubModeGitHubAppBroker routes workspace GitHub access through Fern's
	// GitHub App installation.
	GitHubModeGitHubAppBroker GitHubMode = "github-app-broker"
)

// WorkspaceGitHub is the optional GitHub authority binding of a workspace.
type WorkspaceGitHub struct {
	Mode           GitHubMode
	Hostname       string
	InstallationID int64
	Repository     GitHubRepository
}

// TaskModel names the agent model that executes durable tasks.
type TaskModel struct {
	Provider string
	ID       string
}

// TaskBudget bounds the cost of a single task attempt.
type TaskBudget struct {
	MaxTurns int
}

// TaskVerificationPolicy describes the repository-defined check that must pass
// before a task attempt counts as verified.
type TaskVerificationPolicy struct {
	CheckName        string
	Argv             []string
	WorkingDirectory string
	Timeout          time.Duration
	Environment      map[string]string
	OutputBytes      int
}

// TaskPolicy is the complete durable-task configuration from the tasks section
// of fern.yaml. It is optional.
type TaskPolicy struct {
	Agent             string
	Model             TaskModel
	AttemptTimeout    time.Duration
	LeaseDuration     time.Duration
	Budget            TaskBudget
	BackgroundImage   string
	BackgroundImageID string
	Verification      *TaskVerificationPolicy
}

// Workspace is the container image, repository, memory reservation, and
// forwarded environment of the supervised OpenCode workspace.
type Workspace struct {
	Name   string
	Image  string
	Repo   string
	Memory string
	Env    map[string]string
	GitHub *WorkspaceGitHub
}

// Control carries the host-only Fern control-plane password. It never reaches
// the workspace.
type Control struct {
	Password string
}

// Idle suspension mechanisms. Stop is a graceful docker stop; freeze uses the
// cgroup freezer so the OpenCode process stays resident and wakes in
// milliseconds.
const (
	IdleModeStop   = "stop"
	IdleModeFreeze = "freeze"
)

// Config is the fully merged supervisor configuration after defaults, file,
// and overrides have been applied and normalized.
type Config struct {
	Workspace      Workspace
	Control        Control
	IdleAfter      time.Duration
	IdleMode       string
	Listen         string
	OperatorListen string
	RemoteOrigin   string
	Tasks          *TaskPolicy
}

// Overrides holds explicitly provided CLI values. A non-nil pointer always wins
// over both defaults and file contents, even when the underlying file value is
// invalid.
type Overrides struct {
	Name           *string
	Image          *string
	Repo           *string
	Memory         *string
	IdleAfter      *string
	IdleMode       *string
	Listen         *string
	OperatorListen *string
}

// Client is the narrow configuration projection consumed by attach and event
// clients that talk to an already-running supervisor.
type Client struct {
	Name   string
	Listen string
	Env    map[string]string
}

// Default returns the built-in configuration for repo before any file or
// override merging.
func Default(repo string) Config {
	var config Config
	config.Workspace.Name = "demo"
	config.Workspace.Image = "fern/opencode:dev"
	config.Workspace.Repo = repo
	config.Workspace.Memory = "8Gi"
	config.Workspace.Env = make(map[string]string)
	config.IdleAfter = 10 * time.Minute
	config.IdleMode = IdleModeStop
	config.Listen = "127.0.0.1:8080"
	config.OperatorListen = "127.0.0.1:8081"
	return config
}
