// Package workspacegithub provides token-free probes for the authenticated gh
// CLI installed inside an Amp-style workspace.
package workspacegithub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const maxOutputBytes = 64 << 10

var (
	ErrUnauthenticated = errors.New("workspace GitHub authentication is required")
	ErrUnavailable     = errors.New("workspace GitHub authentication could not be verified")
	ErrRepository      = errors.New("configured GitHub repository is unavailable")
)

type Executor interface {
	Run(context.Context, ...string) ([]byte, error)
}

type Client struct {
	executor Executor
	hostname string
}

type Status struct {
	Hostname    string
	Login       string
	Scopes      []string
	GitProtocol string
}

type Repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"fullName"`
}

func New(executor Executor, hostname string) (*Client, error) {
	if executor == nil || hostname != "github.com" {
		return nil, errors.New("workspace GitHub executor and github.com hostname are required")
	}
	return &Client{executor: executor, hostname: hostname}, nil
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	output, err := c.executor.Run(ctx, "auth", "status", "--active", "--hostname", c.hostname, "--json", "hosts")
	if err != nil {
		if ctx.Err() != nil {
			return Status{}, ctx.Err()
		}
		return Status{}, ErrUnavailable
	}
	var response struct {
		Hosts map[string][]struct {
			State       string `json:"state"`
			Active      bool   `json:"active"`
			Host        string `json:"host"`
			Login       string `json:"login"`
			Scopes      string `json:"scopes"`
			GitProtocol string `json:"gitProtocol"`
			Token       string `json:"token"`
		} `json:"hosts"`
	}
	if err := decodeBounded(output, &response); err != nil {
		return Status{}, fmt.Errorf("%w: invalid gh status output", ErrUnavailable)
	}
	entries := response.Hosts[c.hostname]
	if len(entries) == 0 {
		return Status{}, ErrUnauthenticated
	}
	var selected *Status
	for _, entry := range entries {
		if entry.Token != "" || entry.Host != c.hostname || !entry.Active {
			if entry.Token != "" {
				return Status{}, fmt.Errorf("%w: gh exposed credential material", ErrUnavailable)
			}
			continue
		}
		if entry.State != "success" {
			return Status{}, ErrUnavailable
		}
		if !validLogin(entry.Login) || entry.GitProtocol != "https" && entry.GitProtocol != "ssh" || selected != nil {
			return Status{}, fmt.Errorf("%w: ambiguous gh account", ErrUnavailable)
		}
		status := Status{Hostname: c.hostname, Login: entry.Login, GitProtocol: entry.GitProtocol}
		for _, scope := range strings.Split(entry.Scopes, ",") {
			if scope = strings.TrimSpace(scope); scope != "" {
				status.Scopes = append(status.Scopes, scope)
			}
		}
		selected = &status
	}
	if selected == nil {
		return Status{}, ErrUnauthenticated
	}
	return *selected, nil
}

func (c *Client) Repository(ctx context.Context, expectedID int64, fullName string) (Repository, error) {
	if expectedID <= 0 || !validRepository(fullName) {
		return Repository{}, ErrRepository
	}
	output, err := c.executor.Run(ctx, "api", "--hostname", c.hostname, "--method", "GET", "repos/"+fullName,
		"--jq", `{id: .id, fullName: .full_name}`)
	if err != nil {
		if ctx.Err() != nil {
			return Repository{}, ctx.Err()
		}
		return Repository{}, ErrUnavailable
	}
	var repository Repository
	if err := decodeBounded(output, &repository); err != nil {
		return Repository{}, ErrUnavailable
	}
	if repository.ID != expectedID || repository.FullName != fullName {
		return Repository{}, ErrRepository
	}
	return repository, nil
}

func decodeBounded(output []byte, target any) error {
	if len(output) == 0 || len(output) > maxOutputBytes {
		return errors.New("output size")
	}
	decoder := json.NewDecoder(io.LimitReader(bytes.NewReader(output), maxOutputBytes+1))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing output")
	}
	return nil
}

func validLogin(value string) bool {
	if len(value) < 1 || len(value) > 100 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, character := range value {
		if character != '-' && (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func validRepository(value string) bool {
	owner, repository, found := strings.Cut(value, "/")
	if !found || strings.Contains(repository, "/") || !validLogin(owner) || len(repository) < 1 || len(repository) > 100 || repository == "." || repository == ".." || strings.HasSuffix(strings.ToLower(repository), ".git") {
		return false
	}
	for _, character := range repository {
		if character != '-' && character != '_' && character != '.' &&
			(character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}
