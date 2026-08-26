// Package workspacegithub provides token-free probes for the authenticated gh
// CLI installed inside an Amp-style workspace.
package workspacegithub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"

	"github.com/nebler/fern/internal/gitref"
)

const maxOutputBytes = 64 << 10

var (
	ErrUnavailable = errors.New("workspace GitHub authentication could not be verified")
	ErrRepository  = errors.New("configured GitHub repository is unavailable")
)

type Executor interface {
	Run(context.Context, ...string) ([]byte, error)
}

type Client struct {
	executor Executor
	hostname string
}

type Repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"fullName"`
}

type Branch struct {
	Name         string `json:"name"`
	SHA          string `json:"sha"`
	RepositoryID int64  `json:"repositoryId"`
	FullName     string `json:"fullName"`
}

func New(executor Executor, hostname string) (*Client, error) {
	if executor == nil || hostname != "github.com" {
		return nil, errors.New("workspace GitHub executor and github.com hostname are required")
	}
	return &Client{executor: executor, hostname: hostname}, nil
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

// Branch resolves one exact branch in the configured repository and rejects a
// response that does not prove repository identity, branch name, and SHA-1.
func (c *Client) Branch(ctx context.Context, expectedID int64, fullName, branch string) (Branch, error) {
	if expectedID <= 0 || !validRepository(fullName) || gitref.ValidateRef(branch) != nil {
		return Branch{}, ErrRepository
	}
	if _, err := c.Repository(ctx, expectedID, fullName); err != nil {
		return Branch{}, err
	}
	output, err := c.executor.Run(ctx, "api", "--hostname", c.hostname, "--method", "GET",
		"repos/"+fullName+"/branches/"+url.PathEscape(branch), "--jq",
		`{name: .name, sha: .commit.sha}`)
	if err != nil {
		if ctx.Err() != nil {
			return Branch{}, ctx.Err()
		}
		return Branch{}, ErrUnavailable
	}
	var observed Branch
	if err := decodeBounded(output, &observed); err != nil {
		return Branch{}, ErrUnavailable
	}
	if observed.Name != branch || !gitref.ValidSHA1(observed.SHA) {
		return Branch{}, ErrRepository
	}
	observed.RepositoryID = expectedID
	observed.FullName = fullName
	return observed, nil
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

// validRepository delegates to the canonical owner/repository validator.
func validRepository(value string) bool {
	return gitref.ValidateOwnerRepo(value) == nil
}
