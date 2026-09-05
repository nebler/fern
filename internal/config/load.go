package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxConfigBytes bounds raw YAML before parsing or environment expansion.
const MaxConfigBytes = 1 << 20

func readConfig(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxConfigBytes {
		return nil, errors.New("configuration exceeds 1 MiB limit")
	}
	return data, nil
}

// Load merges defaults, strict YAML, and explicit overrides, then expands
// repository and control-password references from the process environment.
func Load(path, defaultRepo string, required bool, overrides Overrides) (Config, error) {
	return load(path, defaultRepo, required, overrides, os.LookupEnv)
}

// LoadWithEnvironment gives protected environment-file values precedence over
// process values. Loading does not authorize execution; callers must validate.
func LoadWithEnvironment(path, defaultRepo string, required bool, overrides Overrides, environment map[string]string) (Config, error) {
	return load(path, defaultRepo, required, overrides, func(key string) (string, bool) {
		if value, exists := environment[key]; exists {
			return value, true
		}
		return os.LookupEnv(key)
	})
}

func load(path, defaultRepo string, required bool, overrides Overrides, lookup func(string) (string, bool)) (Config, error) {
	config := Default(defaultRepo)
	data, err := readConfig(path)
	if err != nil {
		if !os.IsNotExist(err) || required {
			return Config{}, fmt.Errorf("read config %q: %w", path, err)
		}
	} else {
		var file fileConfig
		if err := decode(data, &file); err != nil {
			return Config{}, fmt.Errorf("parse config %q: %w", path, err)
		}
		if err := applyFileWorkspace(&config.Workspace, file.Workspace, overrides); err != nil {
			return Config{}, fmt.Errorf("parse workspace: %w", err)
		}
		if overrides.Listen == nil && !file.Proxy.Listen.IsZero() {
			config.Listen, err = decodeRequiredTaskString(file.Proxy.Listen)
			if err != nil {
				return Config{}, fmt.Errorf("parse proxy.listen: %w", err)
			}
		}
		if overrides.OperatorListen == nil && !file.Proxy.OperatorListen.IsZero() {
			config.OperatorListen, err = decodeRequiredTaskString(file.Proxy.OperatorListen)
			if err != nil {
				return Config{}, fmt.Errorf("parse proxy.operatorListen: %w", err)
			}
		}
		if !file.Proxy.RemoteOrigin.IsZero() {
			config.RemoteOrigin, err = decodeRequiredTaskString(file.Proxy.RemoteOrigin)
			if err != nil {
				return Config{}, fmt.Errorf("parse proxy.remoteOrigin: %w", err)
			}
			config.RemoteOrigin, err = ParseRemoteOrigin(config.RemoteOrigin)
			if err != nil {
				return Config{}, fmt.Errorf("parse proxy.remoteOrigin: %w", err)
			}
		}
		if !file.Control.Password.IsZero() {
			config.Control.Password, err = decodeRequiredTaskString(file.Control.Password)
			if err != nil {
				return Config{}, fmt.Errorf("parse control.password: %w", err)
			}
		}
		if file.Tasks.IsZero() {
			return Config{}, errors.New("tasks is required")
		}
		policy, err := parseTaskPolicy(file.Tasks)
		if err != nil {
			return Config{}, fmt.Errorf("parse tasks: %w", err)
		}
		config.Tasks = *policy
	}
	if overrides.Name != nil {
		config.Workspace.Name = *overrides.Name
	}
	if overrides.Repo != nil {
		config.Workspace.Repo = *overrides.Repo
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
	config.Control.Password, err = expandRequired(config.Control.Password, lookup)
	if err != nil {
		return Config{}, fmt.Errorf("expand control.password: %w", err)
	}
	return config, nil
}
