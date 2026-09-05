package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Load merges defaults, a strict YAML file, and explicit CLI overrides before
// normalizing values. Invalid file values do not block a valid higher-priority
// override.
func Load(path, defaultRepo string, required bool, overrides Overrides) (Config, error) {
	return load(path, defaultRepo, required, overrides, false, os.LookupEnv)
}

// LoadWithEnvironment expands YAML references from explicit protected values
// before falling back to the process environment.
func LoadWithEnvironment(path, defaultRepo string, required bool, overrides Overrides, environment map[string]string) (Config, error) {
	lookup := func(key string) (string, bool) {
		if value, exists := environment[key]; exists {
			return value, true
		}
		return os.LookupEnv(key)
	}
	return load(path, defaultRepo, required, overrides, false, lookup)
}

// LoadWorkspace ignores supervisor and proxy values that are irrelevant to an
// explicit runtime resume, while keeping the workspace section strict.
func LoadWorkspace(path, defaultRepo string, required bool, overrides Overrides) (Config, error) {
	return load(path, defaultRepo, required, overrides, true, os.LookupEnv)
}

func load(path, defaultRepo string, required bool, overrides Overrides, workspaceOnly bool, lookup func(string) (string, bool)) (Config, error) {
	config := Default(defaultRepo)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) || required {
			return Config{}, fmt.Errorf("read config %q: %w", path, err)
		}
	} else {
		var file fileConfig
		if workspaceOnly {
			if err := decodeWorkspace(data, &file.Workspace); err != nil {
				return Config{}, fmt.Errorf("parse config %q: %w", path, err)
			}
		} else {
			if err := decode(data, &file, true); err != nil {
				return Config{}, fmt.Errorf("parse config %q: %w", path, err)
			}
			if overrides.IdleAfter == nil && !file.Idle.After.IsZero() {
				value, err := decodeString(file.Idle.After)
				if err != nil {
					return Config{}, fmt.Errorf("parse idle.after: %w", err)
				}
				config.IdleAfter, err = time.ParseDuration(value)
				if err != nil {
					return Config{}, fmt.Errorf("parse idle.after: %w", err)
				}
			}
			if overrides.IdleMode == nil && !file.Idle.Mode.IsZero() {
				value, err := decodeString(file.Idle.Mode)
				if err != nil {
					return Config{}, fmt.Errorf("parse idle.mode: %w", err)
				}
				config.IdleMode = value
			}
			if overrides.Listen == nil && !file.Proxy.Listen.IsZero() {
				config.Listen, err = decodeString(file.Proxy.Listen)
				if err != nil {
					return Config{}, fmt.Errorf("parse proxy.listen: %w", err)
				}
			}
			if overrides.OperatorListen == nil && !file.Proxy.OperatorListen.IsZero() {
				config.OperatorListen, err = decodeString(file.Proxy.OperatorListen)
				if err != nil {
					return Config{}, fmt.Errorf("parse proxy.operatorListen: %w", err)
				}
			}
			if !file.Proxy.RemoteOrigin.IsZero() {
				value, decodeErr := decodeString(file.Proxy.RemoteOrigin)
				if decodeErr != nil {
					return Config{}, fmt.Errorf("parse proxy.remoteOrigin: %w", decodeErr)
				}
				config.RemoteOrigin, err = ParseRemoteOrigin(value)
				if err != nil {
					return Config{}, fmt.Errorf("parse proxy.remoteOrigin: %w", err)
				}
			}
			if !file.Control.Password.IsZero() {
				config.Control.Password, err = decodeString(file.Control.Password)
				if err != nil {
					return Config{}, fmt.Errorf("parse control.password: %w", err)
				}
			}
		}
		if err := applyFileWorkspace(&config.Workspace, file.Workspace, overrides); err != nil {
			return Config{}, fmt.Errorf("parse workspace: %w", err)
		}
		if !workspaceOnly && !file.Tasks.IsZero() {
			config.Tasks, err = parseTaskPolicy(file.Tasks)
			if err != nil {
				return Config{}, fmt.Errorf("parse tasks: %w", err)
			}
		}
	}
	if config.Workspace.Env == nil {
		config.Workspace.Env = make(map[string]string)
	}
	if overrides.Name != nil {
		config.Workspace.Name = *overrides.Name
	}
	if overrides.Image != nil {
		config.Workspace.Image = *overrides.Image
	}
	if overrides.Repo != nil {
		config.Workspace.Repo = *overrides.Repo
	}
	if overrides.Memory != nil {
		config.Workspace.Memory = *overrides.Memory
	}
	if overrides.IdleAfter != nil {
		config.IdleAfter, err = time.ParseDuration(*overrides.IdleAfter)
		if err != nil {
			return Config{}, fmt.Errorf("parse idle duration: %w", err)
		}
	}
	if overrides.IdleMode != nil {
		config.IdleMode = *overrides.IdleMode
	}
	if overrides.Listen != nil {
		config.Listen = *overrides.Listen
	}
	if overrides.OperatorListen != nil {
		config.OperatorListen = *overrides.OperatorListen
	}

	repo, err := expandRequired(config.Workspace.Repo, lookup)
	if err != nil {
		return Config{}, fmt.Errorf("expand workspace.repo: %w", err)
	}
	if strings.TrimSpace(repo) == "" {
		return Config{}, errors.New("workspace repository is required")
	}
	if !filepath.IsAbs(repo) {
		base := filepath.Dir(path)
		if overrides.Repo != nil {
			base = defaultRepo
		}
		repo, err = filepath.Abs(filepath.Join(base, repo))
		if err != nil {
			return Config{}, fmt.Errorf("resolve repository path: %w", err)
		}
	}
	config.Workspace.Repo = filepath.Clean(repo)
	for key, value := range config.Workspace.Env {
		if secret := referencedHostOnlySecret(value); secret != "" {
			return Config{}, fmt.Errorf("workspace.env.%s references host-only %s", key, secret)
		}
		expanded, err := expandRequired(value, lookup)
		if err != nil {
			return Config{}, fmt.Errorf("expand workspace.env.%s: %w", key, err)
		}
		if secret := embeddedHostOnlySecret(expanded, lookup); secret != "" {
			return Config{}, fmt.Errorf("workspace.env.%s contains host-only %s", key, secret)
		}
		config.Workspace.Env[key] = expanded
	}
	if !workspaceOnly {
		config.Control.Password, err = expandRequired(config.Control.Password, lookup)
		if err != nil {
			return Config{}, fmt.Errorf("expand control.password: %w", err)
		}
		if config.Tasks != nil {
			for key, value := range config.Tasks.BackgroundEnvironment {
				if secret := referencedHostOnlySecret(value); secret != "" {
					return Config{}, fmt.Errorf("tasks.backgroundEnvironment.%s references host-only %s", key, secret)
				}
				expanded, expandErr := expandRequired(value, lookup)
				if expandErr != nil {
					return Config{}, fmt.Errorf("expand tasks.backgroundEnvironment.%s: %w", key, expandErr)
				}
				if secret := embeddedHostOnlySecret(expanded, lookup); secret != "" {
					return Config{}, fmt.Errorf("tasks.backgroundEnvironment.%s contains host-only %s", key, secret)
				}
				workspacePassword := config.Workspace.Env["OPENCODE_PASSWORD"]
				if (workspacePassword != "" && strings.Contains(expanded, workspacePassword)) ||
					(config.Control.Password != "" && strings.Contains(expanded, config.Control.Password)) {
					return Config{}, fmt.Errorf("tasks.backgroundEnvironment.%s contains a workspace or control password", key)
				}
				config.Tasks.BackgroundEnvironment[key] = expanded
			}
		}
	}
	return config, nil
}

// LoadWorkspaceName is intentionally narrow so emergency status/down/logs can
// operate even when unrelated full configuration is broken. An explicit -name
// flag should bypass this function entirely.
func LoadWorkspaceName(path string, required bool) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !required {
			return "demo", nil
		}
		return "", fmt.Errorf("read config %q: %w", path, err)
	}
	var value struct {
		Workspace struct {
			Name string `yaml:"name"`
		} `yaml:"workspace"`
	}
	if err := decode(data, &value, false); err != nil {
		return "", fmt.Errorf("read workspace name from %q: %w", path, err)
	}
	if value.Workspace.Name == "" {
		return "demo", nil
	}
	return value.Workspace.Name, nil
}
