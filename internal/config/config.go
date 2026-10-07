package config

import (
	"fmt"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the single current configuration shape and also the exact YAML
// document Fern reads and `fern init` writes. ValidateBootstrap permits a
// pending App installation; Validate requires an installed execution binding.
type Config struct {
	Workspace Workspace  `yaml:"workspace"`
	Tasks     TaskPolicy `yaml:"tasks"`
	Control   Control    `yaml:"control"`
	Proxy     Proxy      `yaml:"proxy"`
}

type Workspace struct {
	Name   string    `yaml:"name"`
	Repo   string    `yaml:"repo"`
	GitHub GitHubApp `yaml:"github"`
}

// GitHubApp holds the GitHub.com App installation and repository binding. A
// zero InstallationID means the App is not installed yet.
type GitHubApp struct {
	InstallationID GitHubID         `yaml:"installationId,omitempty"`
	Repository     GitHubRepository `yaml:"repository"`
}

// GitHubRepository is the canonical GitHub authority binding.
type GitHubRepository struct {
	ID       GitHubID `yaml:"id"`
	FullName string   `yaml:"fullName"`
}

type TaskPolicy struct {
	RuntimeStorageRoot string           `yaml:"runtimeStorageRoot,omitempty"`
	Agent              string           `yaml:"agent"`
	Model              TaskModel        `yaml:"model"`
	RunTimeout         time.Duration    `yaml:"runTimeout"`
	BackgroundImage    string           `yaml:"backgroundImage"`
	BackgroundImageID  string           `yaml:"backgroundImageID"`
	BackgroundRoute    *BackgroundRoute `yaml:"backgroundRoute"`
}

type TaskModel struct {
	Provider string `yaml:"provider"`
	ID       string `yaml:"id"`
}

type BackgroundRoute struct {
	Listen string `yaml:"listen"`
	Origin string `yaml:"origin"`
}

// Control contains only the host-side control-plane credential.
type Control struct {
	Password string `yaml:"password"`
}

type Proxy struct {
	Listen         string `yaml:"listen"`
	OperatorListen string `yaml:"operatorListen"`
	RemoteOrigin   string `yaml:"remoteOrigin,omitempty"`
}

// GitHubID is a GitHub numeric identifier. YAML accepts only its canonical
// positive decimal spelling, so octal, hex, or quoted values cannot silently
// bind a different installation or repository.
type GitHubID int64

func (id *GitHubID) UnmarshalYAML(node *yaml.Node) error {
	value, err := strconv.ParseInt(node.Value, 10, 64)
	if node.Kind != yaml.ScalarNode || node.ShortTag() != "!!int" || err != nil || value <= 0 || strconv.FormatInt(value, 10) != node.Value {
		return fmt.Errorf("line %d: %q must be a canonical positive decimal integer", node.Line, node.Value)
	}
	*id = GitHubID(value)
	return nil
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
		Proxy: Proxy{Listen: "127.0.0.1:8080", OperatorListen: "127.0.0.1:8081"}}
}
