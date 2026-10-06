package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type fileWorkspace struct {
	Name   yaml.Node `yaml:"name"`
	Repo   yaml.Node `yaml:"repo"`
	GitHub *struct {
		Mode           yaml.Node `yaml:"mode"`
		Hostname       yaml.Node `yaml:"hostname"`
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
	Proxy struct {
		Listen         yaml.Node `yaml:"listen"`
		OperatorListen yaml.Node `yaml:"operatorListen"`
		RemoteOrigin   yaml.Node `yaml:"remoteOrigin"`
	} `yaml:"proxy"`
}

type fileTaskPolicy struct {
	RuntimeStorageRoot yaml.Node `yaml:"runtimeStorageRoot"`
	Agent              yaml.Node `yaml:"agent"`
	Model              *struct {
		Provider yaml.Node `yaml:"provider"`
		ID       yaml.Node `yaml:"id"`
	} `yaml:"model"`
	AttemptTimeout    yaml.Node `yaml:"attemptTimeout"`
	BackgroundImage   yaml.Node `yaml:"backgroundImage"`
	BackgroundImageID yaml.Node `yaml:"backgroundImageID"`
	BackgroundRoute   *struct {
		Listen yaml.Node `yaml:"listen"`
		Origin yaml.Node `yaml:"origin"`
	} `yaml:"backgroundRoute"`
}

func applyFileWorkspace(workspace *Workspace, file fileWorkspace, overrides Overrides) error {
	fields := []struct {
		name     string
		node     yaml.Node
		override *string
		target   *string
	}{
		{"name", file.Name, overrides.Name, &workspace.Name},
		{"repo", file.Repo, overrides.Repo, &workspace.Repo},
	}
	for _, field := range fields {
		if field.override != nil || field.node.IsZero() {
			continue
		}
		value, err := decodeRequiredTaskString(field.node)
		if err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
		*field.target = value
	}
	if file.GitHub == nil {
		return errors.New("github is required")
	}
	if file.GitHub.Repository == nil {
		return errors.New("github.repository is required")
	}
	id, err := decodeCanonicalPositiveID(file.GitHub.Repository.ID)
	if err != nil {
		return fmt.Errorf("github.repository.id: %w", err)
	}
	fullName, err := decodeRequiredTaskString(file.GitHub.Repository.FullName)
	if err != nil {
		return fmt.Errorf("github.repository.fullName: %w", err)
	}
	if err := ValidateGitHubRepositoryFullName(fullName); err != nil {
		return fmt.Errorf("github.repository.fullName: %w", err)
	}
	modeText, err := decodeRequiredTaskString(file.GitHub.Mode)
	if err != nil {
		return fmt.Errorf("github.mode: %w", err)
	}
	if modeText != GitHubModeGitHubAppBroker {
		return errors.New("github.mode must be github-app-broker")
	}
	if !file.GitHub.Hostname.IsZero() {
		hostname, err := decodeRequiredTaskString(file.GitHub.Hostname)
		if err != nil {
			return fmt.Errorf("github.hostname: %w", err)
		}
		if hostname != "github.com" {
			return errors.New("github.hostname must be github.com")
		}
	}
	var installationID int64
	if !file.GitHub.InstallationID.IsZero() {
		installationID, err = decodeCanonicalPositiveID(file.GitHub.InstallationID)
		if err != nil {
			return fmt.Errorf("github.installationId: %w", err)
		}
	}
	workspace.GitHub = GitHubApp{InstallationID: installationID, Repository: GitHubRepository{ID: id, FullName: fullName}}
	return nil
}

func decodeCanonicalPositiveID(node yaml.Node) (int64, error) {
	if node.IsZero() {
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
	if err := decode(data, &file); err != nil {
		return nil, err
	}
	if file.Model == nil {
		return nil, errors.New("model is required")
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
	policy := &TaskPolicy{
		Agent: agent, Model: TaskModel{Provider: provider, ID: modelID},
		AttemptTimeout: attemptTimeout,
	}
	if !file.RuntimeStorageRoot.IsZero() {
		policy.RuntimeStorageRoot, err = decodeRequiredTaskString(file.RuntimeStorageRoot)
		if err != nil || policy.RuntimeStorageRoot == "" {
			if err == nil {
				err = errors.New("must be nonempty")
			}
			return nil, fmt.Errorf("runtimeStorageRoot: %w", err)
		}
	}
	if !file.BackgroundImage.IsZero() {
		policy.BackgroundImage, err = decodeRequiredTaskString(file.BackgroundImage)
		if err != nil || policy.BackgroundImage == "" {
			if err == nil {
				err = errors.New("must be nonempty")
			}
			return nil, fmt.Errorf("backgroundImage: %w", err)
		}
	}
	if !file.BackgroundImageID.IsZero() {
		policy.BackgroundImageID, err = decodeRequiredTaskString(file.BackgroundImageID)
		if err != nil || policy.BackgroundImageID == "" {
			if err == nil {
				err = errors.New("must be nonempty")
			}
			return nil, fmt.Errorf("backgroundImageID: %w", err)
		}
	}
	if file.BackgroundRoute != nil {
		listen, listenErr := decodeRequiredTaskString(file.BackgroundRoute.Listen)
		if listenErr != nil {
			return nil, fmt.Errorf("backgroundRoute.listen: %w", listenErr)
		}
		origin, originErr := decodeRequiredTaskString(file.BackgroundRoute.Origin)
		if originErr != nil {
			return nil, fmt.Errorf("backgroundRoute.origin: %w", originErr)
		}
		policy.BackgroundRoute = &BackgroundRoute{Listen: listen, Origin: origin}
	}
	return policy, nil
}

func decodeRequiredTaskString(node yaml.Node) (string, error) {
	if node.IsZero() {
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
