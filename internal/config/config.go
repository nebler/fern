package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var workspaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

type Config struct {
	Workspace struct {
		Name   string
		Image  string
		Repo   string
		Memory string
		Env    map[string]string
	}
	IdleAfter time.Duration
	Listen    string
}

type fileConfig struct {
	Workspace struct {
		Name   string            `yaml:"name"`
		Image  string            `yaml:"image"`
		Repo   string            `yaml:"repo"`
		Memory string            `yaml:"memory"`
		Env    map[string]string `yaml:"env"`
	} `yaml:"workspace"`
	Idle struct {
		After string `yaml:"after"`
	} `yaml:"idle"`
	Proxy struct {
		Listen string `yaml:"listen"`
	} `yaml:"proxy"`
}

func Default(repo string) Config {
	var config Config
	config.Workspace.Name = "demo"
	config.Workspace.Image = "fern/opencode:dev"
	config.Workspace.Repo = repo
	config.Workspace.Memory = "8Gi"
	config.Workspace.Env = make(map[string]string)
	config.IdleAfter = 10 * time.Minute
	config.Listen = "127.0.0.1:8080"
	return config
}

// Load decodes a strict YAML file and normalizes paths and environment values.
// A missing default file is allowed; a missing explicitly selected file is not.
func Load(path, defaultRepo string, required bool) (Config, error) {
	config := Default(defaultRepo)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !required {
			return config, nil
		}
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	var file fileConfig
	file.Workspace.Name = config.Workspace.Name
	file.Workspace.Image = config.Workspace.Image
	file.Workspace.Repo = config.Workspace.Repo
	file.Workspace.Memory = config.Workspace.Memory
	file.Workspace.Env = config.Workspace.Env
	file.Idle.After = config.IdleAfter.String()
	file.Proxy.Listen = config.Listen
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, fmt.Errorf("parse config %q: multiple YAML documents are not allowed", path)
		}
		return Config{}, fmt.Errorf("parse trailing config %q: %w", path, err)
	}

	config.Workspace.Name = file.Workspace.Name
	config.Workspace.Image = file.Workspace.Image
	config.Workspace.Memory = file.Workspace.Memory
	config.Workspace.Env = file.Workspace.Env
	if config.Workspace.Env == nil {
		config.Workspace.Env = make(map[string]string)
	}
	config.IdleAfter, err = time.ParseDuration(file.Idle.After)
	if err != nil {
		return Config{}, fmt.Errorf("parse idle.after: %w", err)
	}
	config.Listen = file.Proxy.Listen

	repo, err := expandRequired(file.Workspace.Repo)
	if err != nil {
		return Config{}, fmt.Errorf("expand workspace.repo: %w", err)
	}
	if !filepath.IsAbs(repo) {
		repo, err = filepath.Abs(filepath.Join(filepath.Dir(path), repo))
		if err != nil {
			return Config{}, fmt.Errorf("resolve repository path: %w", err)
		}
	}
	config.Workspace.Repo = filepath.Clean(repo)
	for key, value := range config.Workspace.Env {
		expanded, err := expandRequired(value)
		if err != nil {
			return Config{}, fmt.Errorf("expand workspace.env.%s: %w", key, err)
		}
		config.Workspace.Env[key] = expanded
	}
	return config, nil
}

func Validate(config Config) error {
	if err := ValidateWorkspace(config); err != nil {
		return err
	}
	if config.IdleAfter <= 0 {
		return fmt.Errorf("idle duration must be positive")
	}
	if err := validateListen(config.Listen, config.Workspace.Env["OPENCODE_SERVER_PASSWORD"] != ""); err != nil {
		return err
	}
	return nil
}

func ValidateWorkspace(config Config) error {
	if !workspaceNamePattern.MatchString(config.Workspace.Name) {
		return fmt.Errorf("invalid workspace name %q", config.Workspace.Name)
	}
	if strings.TrimSpace(config.Workspace.Image) == "" {
		return fmt.Errorf("workspace image is required")
	}
	if _, err := ParseMemoryBytes(config.Workspace.Memory); err != nil {
		return err
	}
	stat, err := os.Stat(config.Workspace.Repo)
	if err != nil {
		return fmt.Errorf("inspect repository path %q: %w", config.Workspace.Repo, err)
	}
	if !stat.IsDir() {
		return fmt.Errorf("repository path %q is not a directory", config.Workspace.Repo)
	}
	if password, exists := config.Workspace.Env["OPENCODE_SERVER_PASSWORD"]; exists && password == "" {
		return fmt.Errorf("OPENCODE_SERVER_PASSWORD must not be explicitly empty")
	}
	for key := range config.Workspace.Env {
		if key == "" || strings.ContainsAny(key, "=\x00\r\n") {
			return fmt.Errorf("invalid environment key %q", key)
		}
	}
	for key, value := range config.Workspace.Env {
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("environment value %q contains NUL", key)
		}
	}
	return nil
}

func ValidateWorkspaceName(name string) error {
	if !workspaceNamePattern.MatchString(name) {
		return fmt.Errorf("invalid workspace name %q", name)
	}
	return nil
}

func validateListen(address string, authenticated bool) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid proxy listen address %q: %w", address, err)
	}
	if authenticated {
		return nil
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("proxy may listen beyond loopback only when OPENCODE_SERVER_PASSWORD is configured")
	}
	return nil
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
	if err := yaml.Unmarshal(data, &value); err != nil {
		return "", fmt.Errorf("read workspace name from %q: %w", path, err)
	}
	if value.Workspace.Name == "" {
		return "demo", nil
	}
	return value.Workspace.Name, nil
}

// ParseMemoryBytes preserves decimal and binary unit semantics. A bare value is
// interpreted as MiB for CLI compatibility.
func ParseMemoryBytes(value string) (int64, error) {
	normalized := strings.TrimSpace(strings.ToLower(value))
	units := []struct {
		suffix     string
		multiplier int64
	}{
		{"gib", 1024 * 1024 * 1024}, {"gb", 1000 * 1000 * 1000},
		{"gi", 1024 * 1024 * 1024}, {"g", 1000 * 1000 * 1000},
		{"mib", 1024 * 1024}, {"mb", 1000 * 1000},
		{"mi", 1024 * 1024}, {"m", 1000 * 1000},
	}
	for _, unit := range units {
		if strings.HasSuffix(normalized, unit.suffix) {
			amount, err := positiveInt(strings.TrimSpace(strings.TrimSuffix(normalized, unit.suffix)), value)
			if err != nil {
				return 0, err
			}
			if amount > math.MaxInt64/unit.multiplier {
				return 0, fmt.Errorf("memory %q overflows bytes", value)
			}
			return amount * unit.multiplier, nil
		}
	}
	amount, err := positiveInt(normalized, value)
	if err != nil {
		return 0, err
	}
	if amount > math.MaxInt64/(1024*1024) {
		return 0, fmt.Errorf("memory %q overflows bytes", value)
	}
	return amount * 1024 * 1024, nil
}

func positiveInt(number, original string) (int64, error) {
	amount, err := strconv.ParseInt(number, 10, 64)
	if err != nil || amount <= 0 {
		return 0, fmt.Errorf("invalid memory %q", original)
	}
	return amount, nil
}

func expandRequired(value string) (string, error) {
	const literalDollar = "\x00fern-dollar\x00"
	value = strings.ReplaceAll(value, "$$", literalDollar)
	var missing string
	expanded := os.Expand(value, func(key string) string {
		result, ok := os.LookupEnv(key)
		if !ok && missing == "" {
			missing = key
		}
		return result
	})
	if missing != "" {
		return "", fmt.Errorf("environment variable %s is not set", missing)
	}
	return strings.ReplaceAll(expanded, literalDollar, "$"), nil
}
