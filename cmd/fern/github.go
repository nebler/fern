package main

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/nebler/fern/internal/api"
	"github.com/nebler/fern/internal/github"
)

// gitCredential is git's credential helper in T3's server (fern
// git-credential get): for a github.com URL, it answers with a token from
// the daemon's GitHub App, for that repository alone. When there is none it
// answers nothing, and git goes on without, as for a public repository.
// store and erase have nothing to do: the daemon keeps the tokens.
func gitCredential(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
	if c.args[0] != "get" {
		return 0, nil
	}
	attrs := make(map[string]string)
	sc := bufio.NewScanner(std.in)
	for sc.Scan() && sc.Text() != "" {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			attrs[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		return 1, err
	}
	// The path is there with credential.useHttpPath, which T3's unit sets.
	if attrs["protocol"] != "https" || attrs["host"] != "github.com" || attrs["path"] == "" {
		return 0, nil
	}
	token, err := cl.GitHubToken(ctx, strings.TrimSuffix(attrs["path"], ".git"))
	if err != nil {
		_, _ = fmt.Fprintf(std.err, "fern git-credential: no token for %s: %v\n", attrs["path"], err)
		return 0, nil
	}
	_, err = fmt.Fprintf(std.out, "username=x-access-token\npassword=%s\n", token)
	return 0, err
}

// githubToken prints a token for a GitHub repository, owner/repo, or the
// one the current directory's origin remote names: fern github-token. T3's
// gh wrapper passes it on as GH_TOKEN. With --account it prints one for an
// account the App is installed on, the one named or the only one: what T3
// gets when it asks for its credential outside any repository.
func githubToken(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
	if c.account {
		account := ""
		if len(c.args) > 0 {
			account = c.args[0]
		}
		token, err := cl.GitHubAccountToken(ctx, account)
		if err != nil {
			return 1, err
		}
		_, err = fmt.Fprintln(std.out, token)
		return 0, err
	}
	repo := ""
	if len(c.args) > 0 {
		repo = c.args[0]
	} else {
		out, err := exec.CommandContext(ctx, "git", "remote", "get-url", "origin").Output()
		if err != nil {
			return 1, fmt.Errorf("the origin remote: %w", err)
		}
		owner, name, ok := github.Repository(string(out))
		if !ok {
			return 1, fmt.Errorf("origin isn't a GitHub repository: %s", out)
		}
		repo = owner + "/" + name
	}
	token, err := cl.GitHubToken(ctx, repo)
	if err != nil {
		return 1, err
	}
	_, err = fmt.Fprintln(std.out, token)
	return 0, err
}
