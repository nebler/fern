package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
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

// Load reads the configuration file, the single source of truth for a Fern
// host. Protected environment-file values take precedence over process values
// during expansion. Loading does not authorize execution; callers must
// validate.
func Load(path string, environment map[string]string) (Config, error) {
	lookup := func(key string) (string, bool) {
		if value, exists := environment[key]; exists {
			return value, true
		}
		return os.LookupEnv(key)
	}
	data, err := readConfig(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	config := Config{Workspace: Workspace{Name: "demo"},
		Proxy: Proxy{Listen: "127.0.0.1:8080", OperatorListen: "127.0.0.1:8081"}}
	if err := decode(data, &config); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	if err := requireFields(config); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	config.Proxy.RemoteOrigin, err = ParseRemoteOrigin(config.Proxy.RemoteOrigin)
	if err != nil {
		return Config{}, fmt.Errorf("parse proxy.remoteOrigin: %w", err)
	}
	repo, err := expandRequired(config.Workspace.Repo, lookup)
	if err != nil {
		return Config{}, fmt.Errorf("expand workspace.repo: %w", err)
	}
	if strings.TrimSpace(repo) == "" {
		return Config{}, errors.New("workspace.repo is required")
	}
	if !filepath.IsAbs(repo) {
		repo, err = filepath.Abs(filepath.Join(filepath.Dir(path), repo))
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

// requireFields reports the first required key the file left unset.
func requireFields(config Config) error {
	for _, field := range []struct {
		name  string
		unset bool
	}{
		{"workspace.github.repository.id", config.Workspace.GitHub.Repository.ID == 0},
		{"workspace.github.repository.fullName", config.Workspace.GitHub.Repository.FullName == ""},
		{"tasks.agent", config.Tasks.Agent == ""},
		{"tasks.model.provider", config.Tasks.Model.Provider == ""},
		{"tasks.model.id", config.Tasks.Model.ID == ""},
		{"tasks.runTimeout", config.Tasks.RunTimeout == 0},
	} {
		if field.unset {
			return fmt.Errorf("%s is required", field.name)
		}
	}
	return nil
}

// decode reads exactly one YAML document, rejecting unknown keys.
func decode(data []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple YAML documents are not allowed")
		}
		return fmt.Errorf("parse trailing document: %w", err)
	}
	return nil
}
