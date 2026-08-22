package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

var workspaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

type GitHubRepository struct {
	ID       int64
	FullName string
}

type WorkspaceGitHub struct {
	InstallationID int64
	Repository     GitHubRepository
}

type TaskModel struct {
	Provider string
	ID       string
}

type TaskBudget struct {
	MaxTurns int
}

type TaskVerificationPolicy struct {
	CheckName        string
	Argv             []string
	WorkingDirectory string
	Timeout          time.Duration
	Environment      map[string]string
	OutputBytes      int
}

type TaskPolicy struct {
	Agent          string
	Model          TaskModel
	AttemptTimeout time.Duration
	LeaseDuration  time.Duration
	Budget         TaskBudget
	Verification   *TaskVerificationPolicy
}

type Workspace struct {
	Name   string
	Image  string
	Repo   string
	Memory string
	Env    map[string]string
	GitHub *WorkspaceGitHub
}

type Control struct {
	Password string
}

type Config struct {
	Workspace      Workspace
	Control        Control
	IdleAfter      time.Duration
	Listen         string
	OperatorListen string
	RemoteOrigin   string
	Tasks          *TaskPolicy
}

type Overrides struct {
	Name           *string
	Image          *string
	Repo           *string
	Memory         *string
	IdleAfter      *string
	Listen         *string
	OperatorListen *string
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
	GitHub *struct {
		InstallationID yaml.Node `yaml:"installationId"`
		Repository     *struct {
			ID       yaml.Node `yaml:"id"`
			FullName yaml.Node `yaml:"fullName"`
		} `yaml:"repository"`
	} `yaml:"github"`
}

type fileConfig struct {
	Workspace fileWorkspace `yaml:"workspace"`
	Tasks     yaml.Node     `yaml:"tasks"`
	Control   struct {
		Password yaml.Node `yaml:"password"`
	} `yaml:"control"`
	Idle struct {
		After yaml.Node `yaml:"after"`
	} `yaml:"idle"`
	Proxy struct {
		Listen         yaml.Node `yaml:"listen"`
		OperatorListen yaml.Node `yaml:"operatorListen"`
		RemoteOrigin   yaml.Node `yaml:"remoteOrigin"`
	} `yaml:"proxy"`
}

type fileTaskPolicy struct {
	Agent yaml.Node `yaml:"agent"`
	Model *struct {
		Provider yaml.Node `yaml:"provider"`
		ID       yaml.Node `yaml:"id"`
	} `yaml:"model"`
	AttemptTimeout yaml.Node `yaml:"attemptTimeout"`
	LeaseDuration  yaml.Node `yaml:"leaseDuration"`
	Budget         *struct {
		MaxTurns yaml.Node `yaml:"maxTurns"`
	} `yaml:"budget"`
	Verification *struct {
		CheckName        yaml.Node         `yaml:"checkName"`
		Argv             []string          `yaml:"argv"`
		WorkingDirectory yaml.Node         `yaml:"workingDirectory"`
		Timeout          yaml.Node         `yaml:"timeout"`
		Environment      map[string]string `yaml:"environment"`
		OutputBytes      yaml.Node         `yaml:"outputBytes"`
	} `yaml:"verification"`
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
	config.OperatorListen = "127.0.0.1:8081"
	return config
}

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
			if overrides.OperatorListen == nil && file.Proxy.OperatorListen.Kind != 0 {
				config.OperatorListen, err = decodeString(file.Proxy.OperatorListen)
				if err != nil {
					return Config{}, fmt.Errorf("parse proxy.operatorListen: %w", err)
				}
			}
			if file.Proxy.RemoteOrigin.Kind != 0 {
				value, decodeErr := decodeString(file.Proxy.RemoteOrigin)
				if decodeErr != nil {
					return Config{}, fmt.Errorf("parse proxy.remoteOrigin: %w", decodeErr)
				}
				config.RemoteOrigin, err = ParseRemoteOrigin(value)
				if err != nil {
					return Config{}, fmt.Errorf("parse proxy.remoteOrigin: %w", err)
				}
			}
			if file.Control.Password.Kind != 0 {
				config.Control.Password, err = decodeString(file.Control.Password)
				if err != nil {
					return Config{}, fmt.Errorf("parse control.password: %w", err)
				}
			}
		}
		if err := applyFileWorkspace(&config.Workspace, file.Workspace, overrides); err != nil {
			return Config{}, fmt.Errorf("parse workspace: %w", err)
		}
		if !workspaceOnly && file.Tasks.Kind != 0 {
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
	}
	return config, nil
}

func LoadAttach(path string, required bool, listenOverride *string) (Client, error) {
	return LoadAttachWithEnvironment(path, required, listenOverride, os.LookupEnv)
}

func LoadAttachWithEnvironment(path string, required bool, listenOverride *string, lookup func(string) (string, bool)) (Client, error) {
	client := Client{Name: "demo", Listen: "127.0.0.1:8081", Env: make(map[string]string)}
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
		if listen, exists := fields["operatorListen"]; exists {
			client.Listen, err = decodeString(listen)
			if err != nil {
				return Client{}, fmt.Errorf("parse proxy.operatorListen: %w", err)
			}
		}
	}
	if workspace, exists := sections["workspace"]; exists {
		if err := loadClientWorkspace(workspace, &client, lookup); err != nil {
			return Client{}, err
		}
	}
	return client, nil
}

func LoadEvents(path string, required bool, nameOverride *string) (Client, error) {
	return LoadEventsWithEnvironment(path, required, nameOverride, os.LookupEnv)
}

func LoadEventsWithEnvironment(path string, required bool, nameOverride *string, lookup func(string) (string, bool)) (Client, error) {
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
	if err := loadAuthFields(fields, client.Env, lookup); err != nil {
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

func loadClientWorkspace(workspace yaml.Node, client *Client, lookup func(string) (string, bool)) error {
	fields, err := decodeNodeMap(workspace)
	if err != nil {
		return fmt.Errorf("parse workspace: %w", err)
	}
	return loadAuthFields(fields, client.Env, lookup)
}

func loadAuthFields(workspace map[string]yaml.Node, env map[string]string, lookup func(string) (string, bool)) error {
	envNode, exists := workspace["env"]
	if !exists {
		return nil
	}
	values, err := decodeNodeMap(envNode)
	if err != nil {
		return fmt.Errorf("parse workspace.env: %w", err)
	}
	for _, key := range []string{"OPENCODE_PASSWORD"} {
		node, exists := values[key]
		if !exists {
			continue
		}
		value, err := decodeString(node)
		if err != nil {
			return fmt.Errorf("parse %s: %w", key, err)
		}
		env[key], err = expandRequired(value, lookup)
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
	if file.GitHub != nil {
		if file.GitHub.Repository == nil {
			return errors.New("github.repository is required when workspace.github is configured")
		}
		id, err := decodeCanonicalRepositoryID(file.GitHub.Repository.ID)
		if err != nil {
			return fmt.Errorf("github.repository.id: %w", err)
		}
		fullName, err := decodeString(file.GitHub.Repository.FullName)
		if err != nil {
			return fmt.Errorf("github.repository.fullName: %w", err)
		}
		if err := ValidateGitHubRepositoryFullName(fullName); err != nil {
			return fmt.Errorf("github.repository.fullName: %w", err)
		}
		var installationID int64
		if file.GitHub.InstallationID.Kind != 0 {
			installationID, err = decodeCanonicalPositiveID(file.GitHub.InstallationID)
			if err != nil {
				return fmt.Errorf("github.installationId: %w", err)
			}
		}
		workspace.GitHub = &WorkspaceGitHub{InstallationID: installationID, Repository: GitHubRepository{ID: id, FullName: fullName}}
	}
	return nil
}

func decodeCanonicalRepositoryID(node yaml.Node) (int64, error) {
	return decodeCanonicalPositiveID(node)
}

func decodeCanonicalPositiveID(node yaml.Node) (int64, error) {
	if node.Kind == 0 {
		return 0, errors.New("is required")
	}
	if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return 0, errors.New("must be a canonical positive decimal integer")
	}
	id, err := strconv.ParseInt(node.Value, 10, 64)
	if err != nil || id <= 0 || node.Value != strconv.FormatInt(id, 10) {
		return 0, errors.New("must be a canonical positive signed-64 decimal integer")
	}
	return id, nil
}

func parseTaskPolicy(node yaml.Node) (*TaskPolicy, error) {
	if node.Kind != yaml.MappingNode {
		return nil, errors.New("must be an object")
	}
	data, err := yaml.Marshal(&node)
	if err != nil {
		return nil, err
	}
	var file fileTaskPolicy
	if err := decode(data, &file, true); err != nil {
		return nil, err
	}
	if file.Model == nil {
		return nil, errors.New("model is required")
	}
	if file.Budget == nil {
		return nil, errors.New("budget is required")
	}
	agent, err := decodeRequiredTaskString(file.Agent)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	provider, err := decodeRequiredTaskString(file.Model.Provider)
	if err != nil {
		return nil, fmt.Errorf("model.provider: %w", err)
	}
	modelID, err := decodeRequiredTaskString(file.Model.ID)
	if err != nil {
		return nil, fmt.Errorf("model.id: %w", err)
	}
	attemptTimeout, err := decodeTaskDuration(file.AttemptTimeout)
	if err != nil {
		return nil, fmt.Errorf("attemptTimeout: %w", err)
	}
	leaseDuration, err := decodeTaskDuration(file.LeaseDuration)
	if err != nil {
		return nil, fmt.Errorf("leaseDuration: %w", err)
	}
	maxTurns, err := decodeCanonicalPositiveID(file.Budget.MaxTurns)
	if err != nil {
		return nil, fmt.Errorf("budget.maxTurns: %w", err)
	}
	policy := &TaskPolicy{
		Agent: agent, Model: TaskModel{Provider: provider, ID: modelID},
		AttemptTimeout: attemptTimeout, LeaseDuration: leaseDuration,
		Budget: TaskBudget{MaxTurns: int(maxTurns)},
	}
	if file.Verification != nil {
		checkName, err := decodeRequiredTaskString(file.Verification.CheckName)
		if err != nil {
			return nil, fmt.Errorf("verification.checkName: %w", err)
		}
		workingDirectory, err := decodeRequiredTaskString(file.Verification.WorkingDirectory)
		if err != nil {
			return nil, fmt.Errorf("verification.workingDirectory: %w", err)
		}
		timeout, err := decodeTaskDuration(file.Verification.Timeout)
		if err != nil {
			return nil, fmt.Errorf("verification.timeout: %w", err)
		}
		outputBytes, err := decodeCanonicalPositiveID(file.Verification.OutputBytes)
		if err != nil {
			return nil, fmt.Errorf("verification.outputBytes: %w", err)
		}
		policy.Verification = &TaskVerificationPolicy{
			CheckName: checkName, Argv: append([]string(nil), file.Verification.Argv...),
			WorkingDirectory: workingDirectory, Timeout: timeout,
			Environment: cloneStrings(file.Verification.Environment), OutputBytes: int(outputBytes),
		}
	}
	return policy, nil
}

func cloneStrings(input map[string]string) map[string]string {
	if input == nil {
		return map[string]string{}
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func decodeRequiredTaskString(node yaml.Node) (string, error) {
	if node.Kind == 0 {
		return "", errors.New("is required")
	}
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return "", errors.New("must be a string")
	}
	return node.Value, nil
}

func decodeTaskDuration(node yaml.Node) (time.Duration, error) {
	value, err := decodeRequiredTaskString(node)
	if err != nil {
		return 0, err
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}
	return duration, nil
}

func ValidateGitHubRepositoryFullName(value string) error {
	if len(value) < 3 || len(value) > 140 || strings.Count(value, "/") != 1 {
		return errors.New("must be canonical owner/repository")
	}
	owner, repository, _ := strings.Cut(value, "/")
	if len(owner) == 0 || len(owner) > 39 || owner[0] == '-' || owner[len(owner)-1] == '-' {
		return errors.New("owner must be 1-39 ASCII letters, digits, or non-edge hyphens")
	}
	for index := range len(owner) {
		character := owner[index]
		if !asciiAlphaNumeric(character) && character != '-' {
			return errors.New("owner must be 1-39 ASCII letters, digits, or non-edge hyphens")
		}
	}
	if len(repository) == 0 || len(repository) > 100 || repository == "." || repository == ".." || strings.HasSuffix(strings.ToLower(repository), ".git") {
		return errors.New("repository must be 1-100 safe ASCII characters and must not end in .git")
	}
	for index := range len(repository) {
		character := repository[index]
		if !asciiAlphaNumeric(character) && character != '.' && character != '_' && character != '-' {
			return errors.New("repository must contain only ASCII letters, digits, period, underscore, or hyphen")
		}
	}
	return nil
}

func asciiAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
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
	if config.Workspace.Env["OPENCODE_PASSWORD"] == "" {
		return errors.New("OPENCODE_PASSWORD is required")
	}
	if config.Control.Password == "" {
		return errors.New("FERN_CONTROL_PASSWORD is required through control.password")
	}
	if len(config.Control.Password) < 32 {
		return errors.New("FERN_CONTROL_PASSWORD must be at least 32 characters")
	}
	if config.Control.Password == config.Workspace.Env["OPENCODE_PASSWORD"] {
		return errors.New("Fern control and OpenCode passwords must be different")
	}
	for key, value := range config.Workspace.Env {
		if value == config.Control.Password {
			return fmt.Errorf("workspace.env.%s duplicates the host-only Fern control credential", key)
		}
	}
	if err := validateTasks(config); err != nil {
		return err
	}
	if config.IdleAfter <= 0 {
		return fmt.Errorf("idle duration must be positive")
	}
	if err := validateListen("proxy.listen", config.Listen); err != nil {
		return err
	}
	if err := validateListen("proxy.operatorListen", config.OperatorListen); err != nil {
		return err
	}
	if sameListenPort(config.Listen, config.OperatorListen) {
		return errors.New("proxy.listen and proxy.operatorListen must use different ports")
	}
	if _, err := ParseRemoteOrigin(config.RemoteOrigin); err != nil {
		return fmt.Errorf("invalid proxy.remoteOrigin: %w", err)
	}
	return nil
}

func validateTasks(config Config) error {
	if config.Tasks == nil {
		return nil
	}
	if config.Workspace.GitHub == nil {
		return errors.New("workspace.github is required when tasks are configured")
	}
	if config.Workspace.GitHub.InstallationID <= 0 {
		return errors.New("workspace.github.installationId must be positive when tasks are configured")
	}
	if !validTaskText(config.Tasks.Agent, 1, 128) {
		return errors.New("tasks.agent must be 1-128 bytes of valid text")
	}
	if !validTaskText(config.Tasks.Model.Provider, 1, 128) {
		return errors.New("tasks.model.provider must be 1-128 bytes of valid text")
	}
	if !validTaskText(config.Tasks.Model.ID, 1, 256) {
		return errors.New("tasks.model.id must be 1-256 bytes of valid text")
	}
	if config.Tasks.AttemptTimeout < time.Minute || config.Tasks.AttemptTimeout > 24*time.Hour {
		return errors.New("tasks.attemptTimeout must be between 1m and 24h")
	}
	if config.Tasks.LeaseDuration <= 0 || config.Tasks.LeaseDuration > 5*time.Minute {
		return errors.New("tasks.leaseDuration must be greater than zero and at most 5m")
	}
	if config.Tasks.LeaseDuration > config.Tasks.AttemptTimeout {
		return errors.New("tasks.leaseDuration must not exceed tasks.attemptTimeout")
	}
	if config.Tasks.Budget.MaxTurns < 1 || config.Tasks.Budget.MaxTurns > 1000 {
		return errors.New("tasks.budget.maxTurns must be between 1 and 1000")
	}
	if verification := config.Tasks.Verification; verification != nil {
		if !validPolicyName(verification.CheckName) {
			return errors.New("tasks.verification.checkName must be 1-64 lowercase policy characters")
		}
		if len(verification.Argv) < 1 || len(verification.Argv) > 256 || !filepath.IsAbs(verification.Argv[0]) || filepath.Clean(verification.Argv[0]) != verification.Argv[0] {
			return errors.New("tasks.verification.argv must start with an absolute clean executable and contain at most 256 arguments")
		}
		if executable, err := filepath.EvalSymlinks(verification.Argv[0]); err == nil && pathWithin(config.Workspace.Repo, executable) {
			return errors.New("tasks.verification executable must be outside the writable workspace repository")
		}
		argumentBytes := 0
		for _, argument := range verification.Argv {
			argumentBytes += len(argument)
			if strings.IndexByte(argument, 0) >= 0 {
				return errors.New("tasks.verification.argv must not contain NUL")
			}
		}
		if argumentBytes > 32<<10 {
			return errors.New("tasks.verification.argv exceeds 32768 bytes")
		}
		if verification.WorkingDirectory != "" && verification.WorkingDirectory != "." &&
			(filepath.IsAbs(verification.WorkingDirectory) || filepath.Clean(verification.WorkingDirectory) != verification.WorkingDirectory || verification.WorkingDirectory == ".." || strings.HasPrefix(verification.WorkingDirectory, ".."+string(filepath.Separator))) {
			return errors.New("tasks.verification.workingDirectory must remain within the repository")
		}
		if verification.Timeout <= 0 || verification.Timeout > time.Hour {
			return errors.New("tasks.verification.timeout must be positive and at most 1h")
		}
		if verification.OutputBytes < 1 || verification.OutputBytes > 1<<20 {
			return errors.New("tasks.verification.outputBytes must be between 1 and 1048576")
		}
		if len(verification.Environment) > 64 {
			return errors.New("tasks.verification.environment has too many entries")
		}
		environmentBytes := 0
		for name, value := range verification.Environment {
			environmentBytes += len(name) + len(value) + 1
			if !validEnvironmentName(name) || strings.IndexByte(value, 0) >= 0 {
				return errors.New("tasks.verification.environment contains an invalid entry")
			}
			switch name {
			case "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "GIT_TERMINAL_PROMPT", "HOME", "LANG", "LC_ALL", "PATH":
				return fmt.Errorf("tasks.verification.environment.%s is reserved by the runner", name)
			}
		}
		if environmentBytes > 32<<10 {
			return errors.New("tasks.verification.environment exceeds 32768 bytes")
		}
	}
	return nil
}

func pathWithin(parent, candidate string) bool {
	parent, parentErr := filepath.EvalSymlinks(parent)
	if parentErr != nil {
		return false
	}
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validPolicyName(value string) bool {
	if len(value) < 1 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range []byte(value) {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func validEnvironmentName(value string) bool {
	if value == "" || (value[0] < 'A' || value[0] > 'Z') && (value[0] < 'a' || value[0] > 'z') && value[0] != '_' {
		return false
	}
	for _, character := range []byte(value[1:]) {
		if (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func validTaskText(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

// ParseRemoteOrigin validates the single canonical spelling accepted for a
// remotely published Fern origin. An empty value preserves local-only mode.
func ParseRemoteOrigin(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" {
		return "", errors.New("must be an absolute HTTPS root origin")
	}
	if parsed.Path != "" || parsed.RawPath != "" {
		return "", errors.New("must not contain a path, including a trailing slash")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(value, "#") {
		return "", errors.New("must not contain a query or fragment")
	}
	hostname := parsed.Hostname()
	if hostname == "" || strings.HasSuffix(hostname, ".") {
		return "", errors.New("must contain a canonical DNS name or IP address")
	}
	canonicalHost := ""
	if ip := net.ParseIP(hostname); ip != nil {
		canonicalHost = ip.String()
		if strings.Contains(canonicalHost, ":") {
			canonicalHost = "[" + canonicalHost + "]"
		}
	} else {
		if !validDNSName(hostname) {
			return "", errors.New("must contain a valid DNS name or IP address")
		}
		canonicalHost = strings.ToLower(hostname)
	}
	port := parsed.Port()
	if port != "" {
		number, portErr := strconv.Atoi(port)
		if portErr != nil || number <= 0 || number > 65535 {
			return "", errors.New("contains an invalid port")
		}
		if number == 443 {
			return "", errors.New("must omit the default HTTPS port 443")
		}
		if port != strconv.Itoa(number) {
			return "", errors.New("port is not canonical")
		}
		canonicalHost += ":" + port
	}
	canonical := "https://" + canonicalHost
	if value != canonical {
		return "", fmt.Errorf("must use canonical spelling %q", canonical)
	}
	return canonical, nil
}

func validDNSName(host string) bool {
	if len(host) > 253 || host != strings.ToLower(host) {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if character != '-' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
				return false
			}
		}
	}
	return true
}

func sameListenPort(first, second string) bool {
	_, firstPort, firstErr := net.SplitHostPort(first)
	_, secondPort, secondErr := net.SplitHostPort(second)
	if firstErr != nil || secondErr != nil {
		return false
	}
	firstNumber, firstErr := strconv.Atoi(firstPort)
	secondNumber, secondErr := strconv.Atoi(secondPort)
	return firstErr == nil && secondErr == nil && firstNumber == secondNumber
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
	if config.Workspace.GitHub != nil {
		if config.Workspace.GitHub.Repository.ID <= 0 {
			return errors.New("workspace GitHub repository ID must be positive")
		}
		if err := ValidateGitHubRepositoryFullName(config.Workspace.GitHub.Repository.FullName); err != nil {
			return fmt.Errorf("invalid workspace GitHub repository full name: %w", err)
		}
	}
	stat, err := os.Stat(config.Workspace.Repo)
	if err != nil {
		return fmt.Errorf("inspect repository path %q: %w", config.Workspace.Repo, err)
	}
	if !stat.IsDir() {
		return fmt.Errorf("repository path %q is not a directory", config.Workspace.Repo)
	}
	if password, exists := config.Workspace.Env["OPENCODE_PASSWORD"]; exists && password == "" {
		return fmt.Errorf("OPENCODE_PASSWORD must not be explicitly empty")
	}
	for key := range config.Workspace.Env {
		switch key {
		case "FERN_CONTROL_PASSWORD", "FERN_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN":
			return fmt.Errorf("%s is host-only and cannot be forwarded to the workspace", key)
		}
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

func validateListen(field, address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid %s address %q: %w", field, address, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("invalid %s port %q", field, portText)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%s must use a numeric loopback IP", field)
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

func expandRequired(value string, lookup func(string) (string, bool)) (string, error) {
	const literalDollar = "\x00fern-dollar\x00"
	value = strings.ReplaceAll(value, "$$", literalDollar)
	var missing string
	expanded := os.Expand(value, func(key string) string {
		result, ok := lookup(key)
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

func referencedHostOnlySecret(value string) string {
	const literalDollar = "\x00fern-dollar\x00"
	value = strings.ReplaceAll(value, "$$", literalDollar)
	var found string
	os.Expand(value, func(key string) string {
		if found != "" {
			return ""
		}
		switch key {
		case "FERN_CONTROL_PASSWORD", "FERN_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN":
			found = key
		}
		return ""
	})
	return found
}

func embeddedHostOnlySecret(value string, lookup func(string) (string, bool)) string {
	for _, key := range []string{"FERN_CONTROL_PASSWORD", "FERN_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		secret, exists := lookup(key)
		if exists && len(secret) >= 16 && strings.Contains(value, secret) {
			return key
		}
	}
	return ""
}
