package publication

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var componentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type Request struct {
	Operation string
	Base      string
	Title     string
	Body      string
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
	git       string
	gh        string
}

func New(workspace, repo string) (*Publisher, error) {
	if !ValidComponent(workspace) {
		return nil, fmt.Errorf("workspace name %q is not safe for a GitHub branch", workspace)
	}
	if strings.TrimSpace(repo) == "" {
		return nil, fmt.Errorf("repository path is required")
	}
	repo, err := canonicalPath(repo)
	if err != nil {
		return nil, fmt.Errorf("resolve repository path: %w", err)
	}
	git, err := trustedExecutable("git", repo)
	if err != nil {
		return nil, err
	}
	gh, err := trustedExecutable("gh", repo)
	if err != nil {
		return nil, err
	}
	return &Publisher{workspace: workspace, repo: repo, git: git, gh: gh}, nil
}

func (publisher *Publisher) Prepare(ctx context.Context, request Request) (Prepared, error) {
	if err := ValidateRequest(request); err != nil {
		return Prepared{}, err
	}
	request.Title = strings.TrimSpace(request.Title)
	repository, err := inspectRepository(ctx, publisher.repo, publisher.git)
	if err != nil {
		return Prepared{}, err
	}
	if request.Operation == "" {
		request.Operation = repository.Head[:12]
	}
	if err := run(ctx, "", nil, publisher.gh, "auth", "status", "--hostname", "github.com"); err != nil {
		return Prepared{}, fmt.Errorf("GitHub authentication is unavailable; run 'gh auth login --hostname github.com'")
	}
	base := request.Base
	if base == "" {
		output, err := output(ctx, "", nil, publisher.gh, "repo", "view", repository.Name, "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name")
		if err != nil {
			return Prepared{}, fmt.Errorf("read GitHub default branch: %w", err)
		}
		base = strings.TrimSpace(output)
		if !ValidBranch(base) {
			return Prepared{}, fmt.Errorf("GitHub returned unsupported default branch %q", base)
		}
	}
	token, err := githubCredential(ctx, publisher.gh)
	if err != nil {
		return Prepared{}, err
	}
	gitEnv := []string{"FERN_GITHUB_TOKEN=" + token, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	canonical := "https://github.com/" + repository.Name + ".git"
	if err := run(ctx, publisher.repo, gitEnv, publisher.git, secureGitArgs("fetch", "--no-tags", canonical, "refs/heads/"+base)...); err != nil {
		return Prepared{}, fmt.Errorf("fetch GitHub base branch: %w", err)
	}
	if err := run(ctx, publisher.repo, nil, publisher.git, "merge-base", "--is-ancestor", "FETCH_HEAD", repository.Head); err != nil {
		return Prepared{}, fmt.Errorf("HEAD is not descended from the current GitHub base branch")
	}
	changed, err := output(ctx, publisher.repo, nil, publisher.git, "diff", "--name-only", "-z", "FETCH_HEAD.."+repository.Head)
	if err != nil {
		return Prepared{}, fmt.Errorf("inspect publication changes: %w", err)
	}
	if err := validatePublicationPaths(changed); err != nil {
		return Prepared{}, err
	}
	return Prepared{
		Repository: repository.Name,
		Base:       base,
		Commit:     repository.Head,
		Branch:     "fern/" + publisher.workspace + "/" + request.Operation,
	}, nil
}

func validatePublicationPaths(changed string) error {
	for _, path := range strings.Split(changed, "\x00") {
		if strings.HasPrefix(path, ".github/workflows/") {
			return fmt.Errorf("workflow changes require a separate reviewed publication path: %q", path)
		}
	}
	return nil
}

func (publisher *Publisher) PublishPrepared(ctx context.Context, prepared Prepared, title, body string) (Result, error) {
	if err := validatePrepared(prepared); err != nil {
		return Result{}, err
	}
	if err := ValidateRequest(Request{Operation: strings.TrimPrefix(prepared.Branch, "fern/"+publisher.workspace+"/"), Base: prepared.Base, Title: title, Body: body}); err != nil {
		return Result{}, err
	}
	if err := run(ctx, publisher.repo, nil, publisher.git, "cat-file", "-e", prepared.Commit+"^{commit}"); err != nil {
		return Result{}, fmt.Errorf("recorded publication commit is unavailable locally")
	}
	if err := run(ctx, "", nil, publisher.gh, "auth", "status", "--hostname", "github.com"); err != nil {
		return Result{}, fmt.Errorf("GitHub authentication is unavailable; run 'gh auth login --hostname github.com'")
	}
	token, err := githubCredential(ctx, publisher.gh)
	if err != nil {
		return Result{}, err
	}
	gitEnv := []string{"FERN_GITHUB_TOKEN=" + token, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	canonical := "https://github.com/" + prepared.Repository + ".git"
	remoteCommit, err := remoteBranchCommit(ctx, publisher.repo, gitEnv, publisher.git, canonical, prepared.Branch)
	if err != nil {
		return Result{}, err
	}
	if remoteCommit != "" && remoteCommit != prepared.Commit {
		return Result{}, fmt.Errorf("remote Fern branch conflicts with recorded commit")
	}
	if remoteCommit == "" {
		pushErr := run(ctx, publisher.repo, gitEnv, publisher.git, secureGitArgs("push", canonical, prepared.Commit+":refs/heads/"+prepared.Branch)...)
		remoteCommit, err = remoteBranchCommit(ctx, publisher.repo, gitEnv, publisher.git, canonical, prepared.Branch)
		if err != nil || remoteCommit != prepared.Commit {
			if pushErr != nil {
				return Result{}, fmt.Errorf("push exact commit to %s: %w", prepared.Branch, pushErr)
			}
			return Result{}, fmt.Errorf("remote Fern branch does not contain the recorded commit")
		}
	}
	ghEnv := []string{"GH_TOKEN=" + token}
	prURL, found, err := findPullRequest(ctx, ghEnv, publisher.gh, prepared)
	if err != nil {
		return Result{}, err
	}
	if !found {
		created, createErr := output(ctx, "", ghEnv, publisher.gh, "pr", "create", "--draft", "--repo", prepared.Repository, "--head", prepared.Branch, "--base", prepared.Base, "--title", strings.TrimSpace(title), "--body", body)
		if createErr == nil {
			prURL = strings.TrimSpace(created)
			if err := validatePullURL(prURL, prepared.Repository); err != nil {
				return Result{}, err
			}
		} else {
			prURL, found, err = findPullRequest(ctx, ghEnv, publisher.gh, prepared)
			if err != nil || !found {
				return Result{}, fmt.Errorf("create draft pull request: response was not reconciled")
			}
		}
	}
	return Result{Prepared: prepared, URL: prURL}, nil
}

func githubCredential(ctx context.Context, gh string) (string, error) {
	token, err := output(ctx, "", nil, gh, "auth", "token", "--hostname", "github.com")
	if err != nil {
		return "", fmt.Errorf("obtain host GitHub credential")
	}
	token = strings.TrimSpace(token)
	if len(token) < 20 || len(token) > 512 || !tokenPattern.MatchString(token) {
		return "", fmt.Errorf("host GitHub credential is invalid")
	}
	return token, nil
}

func remoteBranchCommit(ctx context.Context, repo string, env []string, git, canonical, branch string) (string, error) {
	value, err := output(ctx, repo, env, git, secureGitArgs("ls-remote", "--heads", canonical, "refs/heads/"+branch)...)
	if err != nil {
		return "", fmt.Errorf("inspect remote Fern branch: %w", err)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	fields := strings.Fields(value)
	if len(fields) != 2 || len(fields[0]) != 40 || !isHex(fields[0]) || fields[1] != "refs/heads/"+branch {
		return "", fmt.Errorf("GitHub returned an invalid remote branch response")
	}
	return fields[0], nil
}

func findPullRequest(ctx context.Context, env []string, gh string, prepared Prepared) (string, bool, error) {
	value, err := output(ctx, "", env, gh, "pr", "list", "--repo", prepared.Repository, "--head", prepared.Branch, "--base", prepared.Base, "--state", "all", "--limit", "20", "--json", "number,url,state,isDraft,headRefOid,headRefName,baseRefName")
	if err != nil {
		return "", false, fmt.Errorf("inspect existing pull request: %w", err)
	}
	var pulls []struct {
		Number      int    `json:"number"`
		URL         string `json:"url"`
		State       string `json:"state"`
		Draft       bool   `json:"isDraft"`
		HeadOID     string `json:"headRefOid"`
		HeadRefName string `json:"headRefName"`
		BaseRefName string `json:"baseRefName"`
	}
	if err := json.Unmarshal([]byte(value), &pulls); err != nil {
		return "", false, fmt.Errorf("decode existing pull request: %w", err)
	}
	for _, pull := range pulls {
		if pull.HeadRefName != prepared.Branch || pull.BaseRefName != prepared.Base {
			continue
		}
		if pull.HeadOID != prepared.Commit || pull.State != "OPEN" || !pull.Draft {
			return "", false, fmt.Errorf("existing pull request conflicts with the recorded publication")
		}
		if pull.Number <= 0 {
			return "", false, fmt.Errorf("GitHub returned an invalid pull request number")
		}
		if err := validatePullURL(pull.URL, prepared.Repository); err != nil {
			return "", false, err
		}
		return pull.URL, true, nil
	}
	return "", false, nil
}

func validatePrepared(prepared Prepared) error {
	if _, err := GitHubRepositoryName("https://github.com/" + prepared.Repository + ".git"); err != nil {
		return err
	}
	if !ValidBranch(prepared.Base) {
		return fmt.Errorf("recorded publication base is invalid")
	}
	if !ValidBranch(prepared.Branch) {
		return fmt.Errorf("recorded publication branch is invalid")
	}
	if len(prepared.Commit) != 40 || !isHex(prepared.Commit) {
		return fmt.Errorf("recorded publication commit is invalid")
	}
	return nil
}

func validatePullURL(value, repository string) error {
	parsed, err := url.Parse(value)
	wantPrefix := "/" + repository + "/pull/"
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") || !strings.HasPrefix(parsed.Path, wantPrefix) || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("GitHub returned an invalid pull request URL")
	}
	return nil
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
	repo, err := canonicalPath(path)
	if err != nil {
		return Repository{}, fmt.Errorf("resolve repository path: %w", err)
	}
	git, err := trustedExecutable("git", repo)
	if err != nil {
		return Repository{}, err
	}
	return inspectRepository(ctx, repo, git)
}

func inspectRepository(ctx context.Context, path, git string) (Repository, error) {
	gitDirectory := filepath.Join(path, ".git")
	info, err := os.Lstat(gitDirectory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Repository{}, fmt.Errorf("repository must have a real .git directory")
	}
	if _, err := os.Stat(filepath.Join(path, ".gitmodules")); err == nil {
		return Repository{}, fmt.Errorf("publishing repositories with submodules is not supported")
	}
	configOutput, err := output(ctx, path, nil, git, "config", "--local", "--null", "--list")
	if err != nil {
		return Repository{}, fmt.Errorf("inspect local Git configuration: %w", err)
	}
	if err := ValidateLocalGitConfig(configOutput); err != nil {
		return Repository{}, err
	}
	status, err := output(ctx, path, nil, git, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return Repository{}, fmt.Errorf("inspect worktree: %w", err)
	}
	if status != "" {
		return Repository{}, fmt.Errorf("worktree must be clean; commit all intended changes before publishing")
	}
	head, err := output(ctx, path, nil, git, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return Repository{}, fmt.Errorf("resolve HEAD commit: %w", err)
	}
	head = strings.TrimSpace(head)
	if len(head) != 40 || !isHex(head) {
		return Repository{}, fmt.Errorf("only SHA-1 Git repositories are supported")
	}
	origin, err := output(ctx, path, nil, git, "remote", "get-url", "origin")
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
	if value == "" || len(value) > 255 || strings.HasPrefix(value, ".") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, ".") || strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".lock") || strings.Contains(value, "..") || strings.Contains(value, "//") || strings.ContainsAny(value, " ~^:?*[\\") {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
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

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func trustedExecutable(name, repo string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("resolve trusted %s executable: %w", name, err)
	}
	path, err = canonicalPath(path)
	if err != nil {
		return "", fmt.Errorf("resolve trusted %s executable: %w", name, err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("trusted %s executable must be an executable regular file", name)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("trusted %s executable must not be group or world writable", name)
	}
	if relative, err := filepath.Rel(repo, path); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("trusted %s executable must not be inside the repository", name)
	}
	return path, nil
}

func run(ctx context.Context, directory string, extraEnv []string, name string, args ...string) error {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(commandCtx, name, args...)
	command.Dir = directory
	command.Env = append(sanitizedEnvironment(), extraEnv...)
	stderr := &limitedBuffer{limit: 64 << 10, cancel: cancel}
	command.Stdout = discard{}
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if stderr.exceeded {
			return fmt.Errorf("%s output exceeded limit", filepath.Base(name))
		}
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
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(commandCtx, name, args...)
	command.Dir = directory
	command.Env = append(sanitizedEnvironment(), extraEnv...)
	stdout := &limitedBuffer{limit: 1 << 20, cancel: cancel}
	stderr := &limitedBuffer{limit: 64 << 10, cancel: cancel}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if stdout.exceeded || stderr.exceeded {
			return "", fmt.Errorf("%s output exceeded limit", filepath.Base(name))
		}
		return "", fmt.Errorf("%s failed", filepath.Base(name))
	}
	if stdout.exceeded || stderr.exceeded {
		return "", fmt.Errorf("%s output exceeded limit", filepath.Base(name))
	}
	return strings.TrimSuffix(stdout.String(), "\n"), nil
}

func sanitizedEnvironment() []string {
	allowed := []string{"HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "LANG", "LC_ALL"}
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

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
	cancel   context.CancelFunc
	once     sync.Once
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	available := buffer.limit - buffer.Len()
	if available > 0 {
		if available > len(data) {
			available = len(data)
		}
		_, _ = buffer.Buffer.Write(data[:available])
	}
	if available < len(data) {
		buffer.exceeded = true
		if buffer.cancel != nil {
			buffer.once.Do(buffer.cancel)
		}
	}
	// Report the complete write so a noisy child cannot turn the bounded
	// diagnostic capture into an os/exec pipe error or deadlock.
	return len(data), nil
}
