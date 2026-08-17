package publication

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
)

var componentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type Request struct {
	Operation  string
	Base       string
	Title      string
	Body       string
	DryRun     bool
	BeforePush func(Prepared) error
}

type Prepared struct {
	Repository string `json:"repository"`
	Base       string `json:"base"`
	Commit     string `json:"commit"`
	Branch     string `json:"branch"`
}

type Result struct {
	Prepared
	URL string `json:"url,omitempty"`
}

type Publisher struct {
	workspace string
	repo      string
}

func New(workspace, repo string) (*Publisher, error) {
	if !ValidComponent(workspace) {
		return nil, fmt.Errorf("workspace name %q is not safe for a GitHub branch", workspace)
	}
	if strings.TrimSpace(repo) == "" {
		return nil, fmt.Errorf("repository path is required")
	}
	return &Publisher{workspace: workspace, repo: repo}, nil
}

func (publisher *Publisher) Publish(ctx context.Context, request Request) (Result, error) {
	if err := ValidateRequest(request); err != nil {
		return Result{}, err
	}
	request.Title = strings.TrimSpace(request.Title)
	repository, err := InspectRepository(ctx, publisher.repo)
	if err != nil {
		return Result{}, err
	}
	if request.Operation == "" {
		request.Operation = repository.Head[:12]
	}
	if err := run(ctx, "", nil, "gh", "auth", "status", "--hostname", "github.com"); err != nil {
		return Result{}, fmt.Errorf("GitHub authentication is unavailable; run 'gh auth login --hostname github.com'")
	}
	base := request.Base
	if base == "" {
		output, err := output(ctx, "", nil, "gh", "repo", "view", repository.Name, "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name")
		if err != nil {
			return Result{}, fmt.Errorf("read GitHub default branch: %w", err)
		}
		base = strings.TrimSpace(output)
		if !ValidBranch(base) {
			return Result{}, fmt.Errorf("GitHub returned unsupported default branch %q", base)
		}
	}
	token, err := output(ctx, "", nil, "gh", "auth", "token", "--hostname", "github.com")
	if err != nil {
		return Result{}, fmt.Errorf("obtain host GitHub credential")
	}
	token = strings.TrimSpace(token)
	if len(token) < 20 || len(token) > 512 || !tokenPattern.MatchString(token) {
		return Result{}, fmt.Errorf("host GitHub credential is invalid")
	}
	gitEnv := []string{"FERN_GITHUB_TOKEN=" + token, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	canonical := "https://github.com/" + repository.Name + ".git"
	if err := run(ctx, publisher.repo, gitEnv, "git", secureGitArgs("fetch", "--no-tags", canonical, "refs/heads/"+base)...); err != nil {
		return Result{}, fmt.Errorf("fetch GitHub base branch: %w", err)
	}
	if err := run(ctx, publisher.repo, nil, "git", "merge-base", "--is-ancestor", "FETCH_HEAD", repository.Head); err != nil {
		return Result{}, fmt.Errorf("HEAD is not descended from the current GitHub base branch")
	}
	changed, err := output(ctx, publisher.repo, nil, "git", "diff", "--name-only", "FETCH_HEAD.."+repository.Head)
	if err != nil {
		return Result{}, fmt.Errorf("inspect publication changes: %w", err)
	}
	for _, path := range strings.Split(changed, "\n") {
		if strings.HasPrefix(path, ".github/workflows/") {
			return Result{}, fmt.Errorf("workflow changes require a separate reviewed publication path: %s", path)
		}
	}
	prepared := Prepared{
		Repository: repository.Name,
		Base:       base,
		Commit:     repository.Head,
		Branch:     "fern/" + publisher.workspace + "/" + request.Operation,
	}
	if request.DryRun {
		return Result{Prepared: prepared}, nil
	}
	if request.BeforePush != nil {
		if err := request.BeforePush(prepared); err != nil {
			return Result{}, fmt.Errorf("record publication intent: %w", err)
		}
	}
	if err := run(ctx, publisher.repo, gitEnv, "git", secureGitArgs("push", canonical, repository.Head+":refs/heads/"+prepared.Branch)...); err != nil {
		return Result{}, fmt.Errorf("push exact commit to %s: %w", prepared.Branch, err)
	}
	ghEnv := []string{"GH_TOKEN=" + token}
	existing, _ := output(ctx, "", ghEnv, "gh", "pr", "list", "--repo", repository.Name, "--head", prepared.Branch, "--base", base, "--state", "open", "--json", "url", "--jq", ".[0].url")
	prURL := strings.TrimSpace(existing)
	if prURL == "" {
		prURL, err = output(ctx, "", ghEnv, "gh", "pr", "create", "--draft", "--repo", repository.Name, "--head", prepared.Branch, "--base", base, "--title", request.Title, "--body", request.Body)
		if err != nil {
			return Result{}, fmt.Errorf("create draft pull request: %w", err)
		}
		prURL = strings.TrimSpace(prURL)
	}
	return Result{Prepared: prepared, URL: prURL}, nil
}

func ValidateRequest(request Request) error {
	title := strings.TrimSpace(request.Title)
	if title == "" || len(title) > 256 {
		return fmt.Errorf("title is required and must be at most 256 bytes")
	}
	if len(request.Body) > 16<<10 {
		return fmt.Errorf("body exceeds 16 KiB")
	}
	if request.Operation != "" && !ValidComponent(request.Operation) {
		return fmt.Errorf("operation is not safe for a GitHub branch")
	}
	if request.Base != "" && !ValidBranch(request.Base) {
		return fmt.Errorf("base is not a safe branch name")
	}
	return nil
}

type Repository struct {
	Name string
	Head string
}

func InspectRepository(ctx context.Context, path string) (Repository, error) {
	gitDirectory := filepath.Join(path, ".git")
	info, err := os.Lstat(gitDirectory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Repository{}, fmt.Errorf("repository must have a real .git directory")
	}
	if _, err := os.Stat(filepath.Join(path, ".gitmodules")); err == nil {
		return Repository{}, fmt.Errorf("publishing repositories with submodules is not supported")
	}
	configOutput, err := output(ctx, path, nil, "git", "config", "--local", "--null", "--list")
	if err != nil {
		return Repository{}, fmt.Errorf("inspect local Git configuration: %w", err)
	}
	if err := ValidateLocalGitConfig(configOutput); err != nil {
		return Repository{}, err
	}
	status, err := output(ctx, path, nil, "git", "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return Repository{}, fmt.Errorf("inspect worktree: %w", err)
	}
	if status != "" {
		return Repository{}, fmt.Errorf("worktree must be clean; commit all intended changes before publishing")
	}
	head, err := output(ctx, path, nil, "git", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return Repository{}, fmt.Errorf("resolve HEAD commit: %w", err)
	}
	head = strings.TrimSpace(head)
	if len(head) != 40 || !isHex(head) {
		return Repository{}, fmt.Errorf("only SHA-1 Git repositories are supported")
	}
	origin, err := output(ctx, path, nil, "git", "remote", "get-url", "origin")
	if err != nil {
		return Repository{}, fmt.Errorf("read origin: %w", err)
	}
	name, err := GitHubRepositoryName(strings.TrimSpace(origin))
	if err != nil {
		return Repository{}, err
	}
	return Repository{Name: name, Head: head}, nil
}

func ValidateLocalGitConfig(config string) error {
	for _, entry := range strings.Split(config, "\x00") {
		key, _, _ := strings.Cut(entry, "\n")
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		dangerousPrefix := strings.HasPrefix(key, "url.") || strings.HasPrefix(key, "credential.") || strings.HasPrefix(key, "include.") || strings.HasPrefix(key, "http.") || strings.HasPrefix(key, "https.") || strings.HasPrefix(key, "filter.") || strings.HasPrefix(key, "diff.") || strings.HasPrefix(key, "merge.") || strings.HasPrefix(key, "submodule.") || strings.HasPrefix(key, "protocol.") || strings.HasPrefix(key, "uploadpack.") || strings.HasPrefix(key, "receive.") || strings.HasPrefix(key, "pack.")
		dangerousCore := key == "core.sshcommand" || key == "core.hookspath" || key == "core.gitproxy" || key == "core.fsmonitor" || key == "core.attributesfile" || key == "core.excludesfile" || key == "core.alternaterefscommand"
		if dangerousPrefix || dangerousCore || strings.HasPrefix(key, "remote.") && strings.HasSuffix(key, ".pushurl") {
			return fmt.Errorf("repository Git configuration %q is unsafe for host publication", key)
		}
	}
	return nil
}

func GitHubRepositoryName(remote string) (string, error) {
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
	if len(parts) != 2 || !ownerPattern.MatchString(parts[0]) || !ValidComponent(parts[1]) {
		return "", fmt.Errorf("origin must identify one GitHub OWNER/REPOSITORY")
	}
	return parts[0] + "/" + parts[1], nil
}

func ValidComponent(value string) bool {
	return componentPattern.MatchString(value) && !strings.Contains(value, "..") && !strings.HasSuffix(value, ".lock") && !strings.HasSuffix(value, ".")
}

func ValidBranch(value string) bool {
	return value != "" && len(value) <= 255 && !strings.HasPrefix(value, ".") && !strings.HasPrefix(value, "/") && !strings.HasSuffix(value, ".") && !strings.HasSuffix(value, "/") && !strings.HasSuffix(value, ".lock") && !strings.Contains(value, "..") && !strings.Contains(value, "//") && !strings.ContainsAny(value, " ~^:?*[\\\x00-\x1f\x7f")
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

func run(ctx context.Context, directory string, extraEnv []string, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = directory
	command.Env = append(sanitizedEnvironment(), extraEnv...)
	var stderr bytes.Buffer
	command.Stdout = discard{}
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

func output(ctx context.Context, directory string, extraEnv []string, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = directory
	command.Env = append(sanitizedEnvironment(), extraEnv...)
	var stdout bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = discard{}
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("%s failed", name)
	}
	if stdout.Len() > 1<<20 {
		return "", fmt.Errorf("%s output exceeded 1 MiB", name)
	}
	return strings.TrimSuffix(stdout.String(), "\n"), nil
}

func sanitizedEnvironment() []string {
	allowed := []string{"HOME", "PATH", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "SSH_AUTH_SOCK", "LANG", "LC_ALL"}
	result := make([]string, 0, len(allowed))
	for _, key := range allowed {
		if value, ok := os.LookupEnv(key); ok {
			result = append(result, key+"="+value)
		}
	}
	return result
}

type discard struct{}

func (discard) Write(data []byte) (int, error) { return len(data), nil }
