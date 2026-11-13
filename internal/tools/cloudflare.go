package tools

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Secrets sets a GitHub repository's Actions secrets: the daemon's GitHub
// App (github.App).
type Secrets interface {
	SetSecret(ctx context.Context, owner, repo, secret, value string) error
}

// cloudflareSecret is the repository secret Fern's deploy workflow reads
// (images/skills/fern-deploy).
const cloudflareSecret = "CLOUDFLARE_API_TOKEN"

// githubRemote matches a GitHub remote, over HTTPS or SSH: its owner and
// repository.
var githubRemote = regexp.MustCompile(`^(?:https://(?:[^@/]+@)?github\.com/|git@github\.com:|ssh://git@github\.com/)([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// githubRepo returns the owner and repository of a GitHub remote's URL.
func githubRepo(remote string) (owner, repo string, ok bool) {
	m := githubRemote.FindStringSubmatch(strings.TrimSpace(remote))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// addCloudflareTools adds to srv the tool with which environment id's
// Claude connects its repository to Cloudflare: the daemon sets the
// repository's CLOUDFLARE_API_TOKEN secret to the node's token through the
// GitHub App, so the deploy workflow can use it, and neither the token nor
// GitHub's reaches a box. Without a Cloudflare token or a GitHub App on the
// node there is no tool.
func (s *Server) addCloudflareTools(srv *mcp.Server, id string) {
	if s.Cloudflare == nil || s.GitHub == nil {
		return
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "cloudflare_connect",
		Description: "Connects this repository to the person's Cloudflare account for deploys from GitHub Actions: " +
			"sets its " + cloudflareSecret + " secret on GitHub to the token this server keeps. You never see the token. " +
			"Use it once per repository, when setting up deploys (the fern-deploy skill); a repository that doesn't deploy to Cloudflare is refused.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		repo, err := s.connectCloudflare(ctx, id)
		if err != nil {
			return toolError(err), nil, nil
		}
		return toolText(fmt.Sprintf("Set %s on %s. A push now runs its deploy workflow: main goes live, other branches get preview URLs.", cloudflareSecret, repo)), nil, nil
	})
}

// connectCloudflare sets environment id's repository's Cloudflare secret,
// and returns the repository's owner/name.
func (s *Server) connectCloudflare(ctx context.Context, id string) (string, error) {
	token := s.cloudflareToken(ctx, id)
	if token == "" {
		return "", errors.New("no Cloudflare token for this repository: either this server has none (install.sh cloudflare), or the repository doesn't deploy to Cloudflare (no wrangler config or Cloudflare adapter)")
	}
	// The remote is read in the box, where the repository's git folder is,
	// read-only: the node can't resolve a T3 worktree's. git by its full
	// path, as the workspace's tools come first on the box's PATH.
	out, code, err := s.bash(ctx, id, bashInput{Command: "/usr/bin/git remote get-url origin"}, "")
	if err != nil {
		return "", fmt.Errorf("reading the repository's remote: %w", err)
	}
	if code != 0 {
		return "", fmt.Errorf("the repository has no remote named origin: %s", strings.TrimSpace(out))
	}
	owner, repo, ok := githubRepo(out)
	if !ok {
		return "", fmt.Errorf("origin %q isn't a GitHub repository", strings.TrimSpace(out))
	}
	if err := s.GitHub.SetSecret(ctx, owner, repo, cloudflareSecret, token); err != nil {
		return "", fmt.Errorf("setting %s on %s/%s: %w (the GitHub App needs Secrets: Read and write, installed on this repository)", cloudflareSecret, owner, repo, err)
	}
	return owner + "/" + repo, nil
}
