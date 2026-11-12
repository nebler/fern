package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nebler/fern/internal/commit"
)

// commitInput is a commit call.
type commitInput struct {
	Message string   `json:"message" jsonschema:"the commit message: a subject line and, if it needs one, a blank line and a body"`
	Paths   []string `json:"paths,omitempty" jsonschema:"the files or folders to commit, relative to the workspace; every change if left out"`
}

// addCommitTool adds to srv the tool with which environment id's Claude
// commits: its box has the repository's git folder read-only (architecture
// §13), so git there can't, and the daemon makes the commit on the node
// (package commit). It doesn't push: T3's server does, with the person's
// GitHub App. Without Git on the Server there is no tool.
func (s *Server) addCommitTool(srv *mcp.Server, id string) {
	if s.Git == nil {
		return
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "commit",
		Description: "Commits this thread's changes to its branch. `git commit` in Bash fails here, because the box has the " +
			"repository's git folder read-only: use this instead. It stages the paths given, or every change if none " +
			"(as `git add --all`, so .gitignore applies), and commits them with the message; git's hooks don't run. " +
			"`git status` and `git diff` in Bash show what there is to commit. It doesn't push: the person pushes from T3.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in commitInput) (*mcp.CallToolResult, any, error) {
		text, err := s.commit(ctx, id, in)
		if err != nil {
			return toolError(err), nil, nil
		}
		return toolText(text), nil, nil
	})
}

// commit commits in's changes in environment id's worktree, and returns
// what to tell Claude.
func (s *Server) commit(ctx context.Context, id string, in commitInput) (string, error) {
	env, err := s.store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if env.Git == "" {
		return "", errors.New("this thread's folder isn't a worktree of a repository on this server, so there is nothing to commit to")
	}
	res, err := s.Git.Commit(ctx, commit.Request{Dir: env.Workspace, GitDir: env.Git, Message: in.Message, Paths: in.Paths})
	switch {
	case errors.Is(err, commit.ErrNothing) && len(in.Paths) > 0:
		return "Nothing to commit: no changes in those paths.", nil
	case errors.Is(err, commit.ErrNothing):
		return "Nothing to commit: the worktree has no changes.", nil
	case errors.Is(err, commit.ErrNoAuthor):
		return "", errors.New("nothing was committed: git has no author name and e-mail on this server. " +
			"The person sets them once, by running Fern's install again from their computer, which copies theirs, " +
			"or on the server as T3's user: git config --global user.name and user.email")
	case errors.Is(err, commit.ErrNoGit):
		return "", errors.New("nothing was committed: this server has no git. The person installs it (apt-get install git), or runs Fern's install again")
	case err != nil:
		return "", fmt.Errorf("nothing was committed: %w", err)
	}
	files := "files"
	if res.Files == 1 {
		files = "file"
	}
	return fmt.Sprintf("Committed %d %s to %s as %s: %s\nNot pushed: the person pushes from T3.", res.Files, files, res.Branch, res.Hash, res.Subject), nil
}
