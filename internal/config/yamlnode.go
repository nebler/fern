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
	Image  yaml.Node `yaml:"image"`
	Repo   yaml.Node `yaml:"repo"`
	Memory yaml.Node `yaml:"memory"`
	Env    yaml.Node `yaml:"env"`
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
	Idle struct {
		After yaml.Node `yaml:"after"`
		Mode  yaml.Node `yaml:"mode"`
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
	AttemptTimeout        yaml.Node         `yaml:"attemptTimeout"`
	LeaseDuration         yaml.Node         `yaml:"leaseDuration"`
	BackgroundImage       yaml.Node         `yaml:"backgroundImage"`
	BackgroundImageID     yaml.Node         `yaml:"backgroundImageID"`
	BackgroundEnvironment map[string]string `yaml:"backgroundEnvironment"`
	Budget                *struct {
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
		if field.override != nil || field.node.IsZero() {
			continue
		}
		value, err := decodeString(field.node)
		if err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
		*field.target = value
	}
	if !file.Env.IsZero() {
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
		modeText, err := decodeString(file.GitHub.Mode)
		if err != nil {
			return fmt.Errorf("github.mode: %w", err)
		}
		mode := GitHubMode(modeText)
		if mode != GitHubModeWorkspaceGH && mode != GitHubModeGitHubAppBroker {
			return errors.New("github.mode must be workspace-gh or github-app-broker")
		}
		hostname := "github.com"
		if !file.GitHub.Hostname.IsZero() {
			hostname, err = decodeString(file.GitHub.Hostname)
			if err != nil {
				return fmt.Errorf("github.hostname: %w", err)
			}
		}
		if hostname != "github.com" {
			return errors.New("github.hostname must be github.com")
		}
		var installationID int64
		if mode == GitHubModeGitHubAppBroker {
			installationID, err = decodeCanonicalPositiveID(file.GitHub.InstallationID)
			if err != nil {
				return fmt.Errorf("github.installationId: %w", err)
			}
		} else if !file.GitHub.InstallationID.IsZero() {
			return errors.New("github.installationId is forbidden in workspace-gh mode")
		}
		workspace.GitHub = &WorkspaceGitHub{Mode: mode, Hostname: hostname, InstallationID: installationID, Repository: GitHubRepository{ID: id, FullName: fullName}}
	}
	return nil
}

func decodeCanonicalRepositoryID(node yaml.Node) (int64, error) {
	return decodeCanonicalPositiveID(node)
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
		Budget:                TaskBudget{MaxTurns: int(maxTurns)},
		BackgroundEnvironment: cloneStrings(file.BackgroundEnvironment),
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
