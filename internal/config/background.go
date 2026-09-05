package config

import (
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
)

// BackgroundConfig is the only configuration exposed to the online
// Background Run control plane. Legacy persistent-workspace runtime fields are
// intentionally absent.
type BackgroundConfig struct {
	Workspace      BackgroundWorkspace
	Control        Control
	Tasks          TaskPolicy
	Listen         string
	OperatorListen string
	RemoteOrigin   string
}

type BackgroundWorkspace struct {
	Name   string
	Repo   string
	GitHub BackgroundGitHubApp
}

// BackgroundGitHubApp cannot represent the retired workspace-gh authority.
type BackgroundGitHubApp struct {
	InstallationID int64
	Repository     GitHubRepository
}

// BackgroundOverrides contains only command-line overrides meaningful to the
// disposable Background Run architecture.
type BackgroundOverrides struct {
	Name           *string
	Repo           *string
	Listen         *string
	OperatorListen *string
}

// LoadBackgroundWithEnvironment keeps legacy YAML decoding at the file
// boundary, then returns a production projection that cannot expose legacy
// workspace image, memory, environment, idle, or authentication fields.
func LoadBackgroundWithEnvironment(path, defaultRepo string, required bool, overrides BackgroundOverrides, environment map[string]string) (BackgroundConfig, error) {
	legacy, err := LoadWithEnvironment(path, defaultRepo, required, Overrides{
		Name: overrides.Name, Repo: overrides.Repo, Listen: overrides.Listen, OperatorListen: overrides.OperatorListen,
	}, environment)
	if err != nil {
		return BackgroundConfig{}, err
	}
	lookup := func(key string) (string, bool) {
		if value, exists := environment[key]; exists {
			return value, true
		}
		return os.LookupEnv(key)
	}
	return projectBackground(legacy, lookup)
}

// ProjectBackgroundBootstrap converts a decoded compatibility configuration
// into the narrow production shape and validates onboarding-only startup.
func ProjectBackgroundBootstrap(legacy Config) (BackgroundConfig, error) {
	return projectBackground(legacy, os.LookupEnv)
}

func projectBackground(legacy Config, lookup func(string) (string, bool)) (BackgroundConfig, error) {
	if legacy.Workspace.GitHub == nil || legacy.Workspace.GitHub.Mode != GitHubModeGitHubAppBroker || legacy.Workspace.GitHub.Hostname != "github.com" {
		return BackgroundConfig{}, errors.New("background runs require github-app-broker on github.com")
	}
	if legacy.Tasks == nil {
		return BackgroundConfig{}, errors.New("tasks is required for Background Runs")
	}
	tasks := cloneTaskPolicy(*legacy.Tasks)
	passwords := []string{legacy.Workspace.Env["OPENCODE_PASSWORD"]}
	for _, key := range []string{"OPENCODE_PASSWORD", "OPENCODE_SERVER_PASSWORD", "OPENCODE_SERVER_USERNAME"} {
		if value, exists := lookup(key); exists {
			passwords = append(passwords, value)
		}
	}
	if err := rejectOpenCodeCredentials(tasks, passwords); err != nil {
		return BackgroundConfig{}, err
	}
	projected := BackgroundConfig{
		Workspace: BackgroundWorkspace{Name: legacy.Workspace.Name, Repo: legacy.Workspace.Repo,
			GitHub: BackgroundGitHubApp{InstallationID: legacy.Workspace.GitHub.InstallationID, Repository: legacy.Workspace.GitHub.Repository}},
		Control: legacy.Control, Tasks: tasks, Listen: legacy.Listen, OperatorListen: legacy.OperatorListen, RemoteOrigin: legacy.RemoteOrigin,
	}
	if err := ValidateBackgroundBootstrap(projected); err != nil {
		return BackgroundConfig{}, err
	}
	return projected, nil
}

func cloneTaskPolicy(source TaskPolicy) TaskPolicy {
	result := source
	result.BackgroundEnvironment = maps.Clone(source.BackgroundEnvironment)
	if source.BackgroundRoute != nil {
		route := *source.BackgroundRoute
		result.BackgroundRoute = &route
	}
	if source.Verification != nil {
		verification := *source.Verification
		verification.Argv = slices.Clone(source.Verification.Argv)
		verification.Environment = maps.Clone(source.Verification.Environment)
		result.Verification = &verification
	}
	return result
}

func rejectOpenCodeCredentials(tasks TaskPolicy, passwords []string) error {
	check := func(field string, environment map[string]string) error {
		for name, value := range environment {
			switch name {
			case "OPENCODE_PASSWORD", "OPENCODE_SERVER_PASSWORD", "OPENCODE_SERVER_USERNAME":
				return errors.New(field + "." + name + " is a reserved credential")
			}
			if referencesOpenCodeCredential(value) {
				return errors.New(field + "." + name + " references an OpenCode credential")
			}
			for _, password := range passwords {
				if password != "" && strings.Contains(value, password) {
					return errors.New(field + "." + name + " contains an OpenCode credential")
				}
			}
		}
		return nil
	}
	if err := check("tasks.backgroundEnvironment", tasks.BackgroundEnvironment); err != nil {
		return err
	}
	if tasks.Verification != nil {
		return check("tasks.verification.environment", tasks.Verification.Environment)
	}
	return nil
}

func referencesOpenCodeCredential(value string) bool {
	found := false
	os.Expand(escapeDollars(value), func(key string) string {
		if key == "OPENCODE_PASSWORD" || key == "OPENCODE_SERVER_PASSWORD" || key == "OPENCODE_SERVER_USERNAME" {
			found = true
		}
		return ""
	})
	return found
}
