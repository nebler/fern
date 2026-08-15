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

type Workspace struct {
	Name   string
	Image  string
	Repo   string
	Memory string
	Env    map[string]string
}

type Config struct {
	Workspace Workspace
	IdleAfter time.Duration
	Listen    string
}

type Overrides struct {
	Name      *string
	Image     *string
	Repo      *string
	Memory    *string
	IdleAfter *string
	Listen    *string
}

type Client struct {
	Name   string
	Listen string
	Env    map[string]string
}

type fileWorkspace struct {
	Name   yaml.Node `yaml:"name"`
	Image  yaml.Node `yaml:"image"`
	Repo   yaml.Node `yaml:"repo"`
	Memory yaml.Node `yaml:"memory"`
	Env    yaml.Node `yaml:"env"`
}

type fileConfig struct {
	Workspace fileWorkspace `yaml:"workspace"`
	Idle      struct {
		After yaml.Node `yaml:"after"`
	} `yaml:"idle"`
	Proxy struct {
		Listen yaml.Node `yaml:"listen"`
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

// Load merges defaults, a strict YAML file, and explicit CLI overrides before
// normalizing values. Invalid file values do not block a valid higher-priority
// override.
func Load(path, defaultRepo string, required bool, overrides Overrides) (Config, error) {
	return load(path, defaultRepo, required, overrides, false)
}

// LoadWorkspace ignores supervisor and proxy values that are irrelevant to an
// explicit runtime resume, while keeping the workspace section strict.
func LoadWorkspace(path, defaultRepo string, required bool, overrides Overrides) (Config, error) {
	return load(path, defaultRepo, required, overrides, true)
}

func load(path, defaultRepo string, required bool, overrides Overrides, workspaceOnly bool) (Config, error) {
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
			if overrides.IdleAfter == nil && file.Idle.After.Kind != 0 {
				value, err := decodeString(file.Idle.After)
				if err != nil {
					return Config{}, fmt.Errorf("parse idle.after: %w", err)
				}
				config.IdleAfter, err = time.ParseDuration(value)
				if err != nil {
					return Config{}, fmt.Errorf("parse idle.after: %w", err)
				}
			}
			if overrides.Listen == nil && file.Proxy.Listen.Kind != 0 {
				config.Listen, err = decodeString(file.Proxy.Listen)
				if err != nil {
					return Config{}, fmt.Errorf("parse proxy.listen: %w", err)
				}
			}
		}
		if err := applyFileWorkspace(&config.Workspace, file.Workspace, overrides); err != nil {
			return Config{}, fmt.Errorf("parse workspace: %w", err)
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
	if overrides.Listen != nil {
		config.Listen = *overrides.Listen
	}

	repo, err := expandRequired(config.Workspace.Repo)
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
		expanded, err := expandRequired(value)
		if err != nil {
			return Config{}, fmt.Errorf("expand workspace.env.%s: %w", key, err)
		}
		config.Workspace.Env[key] = expanded
	}
	return config, nil
}

func LoadAttach(path string, required bool, listenOverride *string) (Client, error) {
	client := Client{Name: "demo", Listen: "127.0.0.1:8080", Env: make(map[string]string)}
	if listenOverride != nil {
		client.Listen = *listenOverride
	}
	sections, err := loadSections(path, required)
	if err != nil || sections == nil {
		return client, err
	}
	if proxy, exists := sections["proxy"]; exists && listenOverride == nil {
		fields, err := decodeNodeMap(proxy)
		if err != nil {
			return Client{}, fmt.Errorf("parse proxy: %w", err)
		}
		if listen, exists := fields["listen"]; exists {
			client.Listen, err = decodeString(listen)
			if err != nil {
				return Client{}, fmt.Errorf("parse proxy.listen: %w", err)
			}
		}
	}
	if workspace, exists := sections["workspace"]; exists {
		if err := loadClientAuth(workspace, client.Env); err != nil {
			return Client{}, err
		}
	}
	return client, nil
}

func LoadEvents(path string, required bool, nameOverride *string) (Client, error) {
	client := Client{Name: "demo", Env: make(map[string]string)}
	if nameOverride != nil {
		client.Name = *nameOverride
	}
	sections, err := loadSections(path, required)
	if err != nil || sections == nil {
		return client, err
	}
	workspace, exists := sections["workspace"]
	if !exists {
		return client, nil
	}
	fields, err := decodeNodeMap(workspace)
	if err != nil {
		return Client{}, fmt.Errorf("parse workspace: %w", err)
	}
	if name, exists := fields["name"]; exists && nameOverride == nil {
		client.Name, err = decodeString(name)
		if err != nil {
			return Client{}, fmt.Errorf("parse workspace.name: %w", err)
		}
	}
	if err := loadAuthFields(fields, client.Env); err != nil {
		return Client{}, err
	}
	return client, nil
}

func loadSections(path string, required bool) (map[string]yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !required {
			return nil, nil
		}
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var sections map[string]yaml.Node
	if err := decode(data, &sections, false); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	return sections, nil
}

func loadClientAuth(workspace yaml.Node, env map[string]string) error {
	fields, err := decodeNodeMap(workspace)
	if err != nil {
		return fmt.Errorf("parse workspace: %w", err)
	}
	return loadAuthFields(fields, env)
}

func loadAuthFields(workspace map[string]yaml.Node, env map[string]string) error {
	envNode, exists := workspace["env"]
	if !exists {
		return nil
	}
	values, err := decodeNodeMap(envNode)
	if err != nil {
		return fmt.Errorf("parse workspace.env: %w", err)
	}
	for _, key := range []string{"OPENCODE_SERVER_USERNAME", "OPENCODE_SERVER_PASSWORD"} {
		node, exists := values[key]
		if !exists {
			continue
		}
		value, err := decodeString(node)
		if err != nil {
			return fmt.Errorf("parse %s: %w", key, err)
		}
		env[key], err = expandRequired(value)
		if err != nil {
			return fmt.Errorf("expand %s: %w", key, err)
		}
	}
	return nil
}

func decodeWorkspace(data []byte, workspace *fileWorkspace) error {
	var sections map[string]yaml.Node
	if err := decode(data, &sections, false); err != nil {
		return err
	}
	section, exists := sections["workspace"]
	if !exists {
		return nil
	}
	data, err := yaml.Marshal(&section)
	if err != nil {
		return err
	}
	return decode(data, workspace, true)
}

func applyFileWorkspace(workspace *Workspace, file fileWorkspace, overrides Overrides) error {
	fields := []struct {
		name     string
		node     yaml.Node
		override *string
		target   *string
	}{
		{"name", file.Name, overrides.Name, &workspace.Name},
		{"image", file.Image, overrides.Image, &workspace.Image},
		{"repo", file.Repo, overrides.Repo, &workspace.Repo},
		{"memory", file.Memory, overrides.Memory, &workspace.Memory},
	}
	for _, field := range fields {
		if field.override != nil || field.node.Kind == 0 {
			continue
		}
		value, err := decodeString(field.node)
		if err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
		*field.target = value
	}
	if file.Env.Kind != 0 {
		if err := file.Env.Decode(&workspace.Env); err != nil {
			return fmt.Errorf("env: %w", err)
		}
	}
	return nil
}

func decodeNodeMap(node yaml.Node) (map[string]yaml.Node, error) {
	var values map[string]yaml.Node
	if err := node.Decode(&values); err != nil {
		return nil, err
	}
	return values, nil
}

func decodeString(node yaml.Node) (string, error) {
	var value string
	if err := node.Decode(&value); err != nil {
		return "", err
	}
	return value, nil
}

func decode(data []byte, target any, strict bool) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(strict)
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
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid proxy listen address %q: %w", address, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("invalid proxy port %q", portText)
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
	if err := decode(data, &value, false); err != nil {
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
