package main

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nebler/fern/internal/config"
)

var githubComponentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)

func runGitHub(args []string) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(os.Stdout, "Publish committed work through host GitHub credentials.\n\nUsage:\n  fern github publish [flags]")
		return nil
	}
	if len(args) == 0 || args[0] != "publish" {
		return unknownCommand(append([]string{"github"}, args...))
	}
	return runGitHubPublish(args[1:])
}

func runGitHubPublish(args []string) error {
	flags := newFlagSet("github publish", "Push committed work to a Fern branch and open a draft pull request.")
	configPath := flags.String("config", "fern.yaml", "configuration file")
	operation := flags.String("operation", time.Now().UTC().Format("20060102-150405"), "publication identifier")
	base := flags.String("base", "", "base branch (defaults to repository default)")
	title := flags.String("title", "", "draft pull request title")
	body := flags.String("body", "Created from a private Fern workspace.", "draft pull request body")
	dryRun := flags.Bool("dry-run", false, "validate without credentials, push, or pull request creation")
	if err := parseFlags(flags, args); err != nil {
		return err
	}
	if strings.TrimSpace(*title) == "" || len(*title) > 256 {
		return invocationError{message: "--title is required and must be at most 256 bytes"}
	}
	if !validGitHubComponent(*operation) {
		return invocationError{message: "--operation must contain only letters, numbers, underscore, or hyphen"}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	workspace, err := config.LoadWorkspace(*configPath, cwd, true, config.Overrides{})
	if err != nil {
		return err
	}
	if !validGitHubComponent(workspace.Workspace.Name) {
		return fmt.Errorf("workspace name %q is not safe for a GitHub branch", workspace.Workspace.Name)
	}
	repository, err := inspectPublishRepository(workspace.Workspace.Repo)
	if err != nil {
		return err
	}
	baseBranch := *base
	if baseBranch != "" && !validGitHubComponent(baseBranch) {
		return invocationError{message: "--base must be one simple branch component"}
	}
	branch := "fern/" + workspace.Workspace.Name + "/" + *operation
	if *dryRun {
		fmt.Printf("GitHub publication is valid\nrepository: %s\ncommit: %s\nbranch: %s\n", repository.Name, repository.Head, branch)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := runHostCommand(ctx, "", nil, "gh", "auth", "status", "--hostname", "github.com"); err != nil {
		return fmt.Errorf("GitHub authentication is unavailable; run 'gh auth login --hostname github.com'")
	}
	if baseBranch == "" {
		output, err := outputHostCommand(ctx, "", nil, "gh", "repo", "view", repository.Name, "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name")
		if err != nil {
			return fmt.Errorf("read GitHub default branch: %w", err)
		}
		baseBranch = strings.TrimSpace(output)
		if !validGitHubComponent(baseBranch) {
			return fmt.Errorf("GitHub returned unsupported default branch %q; pass --base", baseBranch)
		}
	}
	token, err := outputHostCommand(ctx, "", nil, "gh", "auth", "token", "--hostname", "github.com")
	if err != nil {
		return fmt.Errorf("obtain host GitHub credential")
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t\r\n\x00") {
		return fmt.Errorf("host GitHub credential is invalid")
	}
	defer func() { token = strings.Repeat("0", len(token)) }()
	gitEnv := []string{"FERN_GITHUB_TOKEN=" + token, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	canonical := "https://github.com/" + repository.Name + ".git"
	if err := runHostCommand(ctx, workspace.Workspace.Repo, gitEnv, "git", secureGitArgs("fetch", "--no-tags", canonical, "refs/heads/"+baseBranch)...); err != nil {
		return fmt.Errorf("fetch GitHub base branch: %w", err)
	}
	if err := runHostCommand(ctx, workspace.Workspace.Repo, nil, "git", "merge-base", "--is-ancestor", "FETCH_HEAD", repository.Head); err != nil {
		return fmt.Errorf("HEAD is not descended from the current GitHub base branch")
	}
	changed, err := outputHostCommand(ctx, workspace.Workspace.Repo, nil, "git", "diff", "--name-only", "FETCH_HEAD.."+repository.Head)
	if err != nil {
		return fmt.Errorf("inspect publication changes: %w", err)
	}
	for _, path := range strings.Split(changed, "\n") {
		if strings.HasPrefix(path, ".github/workflows/") {
			return fmt.Errorf("workflow changes require a separate reviewed publication path: %s", path)
		}
	}
	gitArgs := secureGitArgs("push", canonical, repository.Head+":refs/heads/"+branch)
	if err := runHostCommand(ctx, workspace.Workspace.Repo, gitEnv, "git", gitArgs...); err != nil {
		return fmt.Errorf("push exact commit to %s: %w", branch, err)
	}
	ghEnv := []string{"GH_TOKEN=" + token}
	existing, _ := outputHostCommand(ctx, "", ghEnv, "gh", "pr", "list", "--repo", repository.Name, "--head", branch, "--base", baseBranch, "--state", "open", "--json", "url", "--jq", ".[0].url")
	prURL := strings.TrimSpace(existing)
	if prURL == "" {
		prURL, err = outputHostCommand(ctx, "", ghEnv, "gh", "pr", "create", "--draft", "--repo", repository.Name, "--head", branch, "--base", baseBranch, "--title", *title, "--body", *body)
		if err != nil {
			return fmt.Errorf("create draft pull request: %w", err)
		}
		prURL = strings.TrimSpace(prURL)
	}
	fmt.Printf("Draft pull request ready\nrepository: %s\nbranch: %s\ncommit: %s\nurl: %s\n", repository.Name, branch, repository.Head, prURL)
	return nil
}

type publishRepository struct {
	Name string
	Head string
}

func inspectPublishRepository(path string) (publishRepository, error) {
	gitDirectory := filepath.Join(path, ".git")
	info, err := os.Lstat(gitDirectory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return publishRepository{}, fmt.Errorf("repository must have a real .git directory")
	}
	if _, err := os.Stat(filepath.Join(path, ".gitmodules")); err == nil {
		return publishRepository{}, fmt.Errorf("publishing repositories with submodules is not supported")
	}
	configOutput, err := outputHostCommand(context.Background(), path, nil, "git", "config", "--local", "--null", "--list")
	if err != nil {
		return publishRepository{}, fmt.Errorf("inspect local Git configuration: %w", err)
	}
	if err := validateLocalGitConfig(configOutput); err != nil {
		return publishRepository{}, err
	}
	status, err := outputHostCommand(context.Background(), path, nil, "git", "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return publishRepository{}, fmt.Errorf("inspect worktree: %w", err)
	}
	if status != "" {
		return publishRepository{}, fmt.Errorf("worktree must be clean; commit all intended changes before publishing")
	}
	head, err := outputHostCommand(context.Background(), path, nil, "git", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return publishRepository{}, fmt.Errorf("resolve HEAD commit: %w", err)
	}
	head = strings.TrimSpace(head)
	if len(head) != 40 || !isHex(head) {
		return publishRepository{}, fmt.Errorf("only SHA-1 Git repositories are supported")
	}
	origin, err := outputHostCommand(context.Background(), path, nil, "git", "remote", "get-url", "origin")
	if err != nil {
		return publishRepository{}, fmt.Errorf("read origin: %w", err)
	}
	name, err := githubRepositoryName(strings.TrimSpace(origin))
	if err != nil {
		return publishRepository{}, err
	}
	return publishRepository{Name: name, Head: head}, nil
}

func validateLocalGitConfig(output string) error {
	for _, entry := range strings.Split(output, "\x00") {
		key, _, _ := strings.Cut(entry, "\n")
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		dangerousPrefix := strings.HasPrefix(key, "url.") || strings.HasPrefix(key, "credential.") || strings.HasPrefix(key, "include.") || strings.HasPrefix(key, "http.") || strings.HasPrefix(key, "https.") || strings.HasPrefix(key, "filter.") || strings.HasPrefix(key, "diff.") || strings.HasPrefix(key, "merge.") || strings.HasPrefix(key, "submodule.") || strings.HasPrefix(key, "protocol.")
		dangerousCore := key == "core.sshcommand" || key == "core.hookspath" || key == "core.gitproxy" || key == "core.fsmonitor" || key == "core.attributesfile" || key == "core.excludesfile"
		if dangerousPrefix || dangerousCore || strings.HasPrefix(key, "remote.") && strings.HasSuffix(key, ".pushurl") {
			return fmt.Errorf("repository Git configuration %q is unsafe for host publication", key)
		}
	}
	return nil
}

func githubRepositoryName(remote string) (string, error) {
	var path string
	switch {
	case strings.HasPrefix(remote, "git@github.com:"):
		path = strings.TrimPrefix(remote, "git@github.com:")
	default:
		parsed, err := url.Parse(remote)
		if err != nil {
			return "", fmt.Errorf("origin must be a credential-free github.com URL")
		}
		allowedSSHUser := false
		if parsed.User != nil && parsed.Scheme == "ssh" && parsed.User.Username() == "git" {
			_, hasPassword := parsed.User.Password()
			allowedSSHUser = !hasPassword
		}
		if parsed.User != nil && !allowedSSHUser || !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", fmt.Errorf("origin must be a credential-free github.com URL")
		}
		path = strings.TrimPrefix(parsed.Path, "/")
	}
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || !validGitHubComponent(parts[0]) || !validGitHubComponent(parts[1]) {
		return "", fmt.Errorf("origin must identify one GitHub OWNER/REPOSITORY")
	}
	return parts[0] + "/" + parts[1], nil
}

func validGitHubComponent(value string) bool {
	return githubComponentPattern.MatchString(value) && !strings.Contains(value, "..") && !strings.HasSuffix(value, ".lock")
}

func isHex(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func secureGitArgs(args ...string) []string {
	prefix := []string{"-c", "core.hooksPath=/dev/null", "-c", "credential.helper=", "-c", `credential.helper=!f() { echo username=x-access-token; echo password=$FERN_GITHUB_TOKEN; }; f`, "-c", "protocol.file.allow=never"}
	return append(prefix, args...)
}

func runHostCommand(ctx context.Context, directory string, extraEnv []string, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = directory
	command.Env = append(sanitizedHostEnvironment(), extraEnv...)
	var stderr bytes.Buffer
	command.Stdout = ioDiscard{}
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 512 {
			message = message[:512]
		}
		if message == "" {
			return err
		}
		return fmt.Errorf("%s", message)
	}
	return nil
}

func outputHostCommand(ctx context.Context, directory string, extraEnv []string, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = directory
	command.Env = append(sanitizedHostEnvironment(), extraEnv...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("%s failed", name)
	}
	if stdout.Len() > 1<<20 {
		return "", fmt.Errorf("%s output exceeded 1 MiB", name)
	}
	return strings.TrimSuffix(stdout.String(), "\n"), nil
}

func sanitizedHostEnvironment() []string {
	allowed := []string{"HOME", "PATH", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "SSH_AUTH_SOCK", "LANG", "LC_ALL"}
	result := make([]string, 0, len(allowed))
	for _, key := range allowed {
		if value, ok := os.LookupEnv(key); ok {
			result = append(result, key+"="+value)
		}
	}
	return result
}

type ioDiscard struct{}

func (ioDiscard) Write(data []byte) (int, error) { return len(data), nil }
