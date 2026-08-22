package publication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
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
	RepositoryID       int64  `json:"repositoryId"`
	RepositoryFullName string `json:"repositoryFullName"`
	BaseSHA            string `json:"baseSha"`
	BaseRef            string `json:"baseRef"`
	ResultCommit       string `json:"resultCommit"`
	Branch             string `json:"branch"`
}

type Result struct {
	Prepared
	PullRequest PullRequestObservation `json:"pullRequest"`
}

type RepositoryBinding struct {
	ID       int64
	FullName string
}

type PullRequestRefObservation struct {
	RepositoryID       int64  `json:"repositoryId"`
	RepositoryFullName string `json:"repositoryFullName"`
	RepositoryOwner    string `json:"repositoryOwner"`
	RepositoryName     string `json:"repositoryName"`
	Ref                string `json:"ref"`
	SHA                string `json:"sha"`
}

type PullRequestObservation struct {
	TargetRepositoryID       int64                     `json:"targetRepositoryId"`
	TargetRepositoryFullName string                    `json:"targetRepositoryFullName"`
	Number                   int64                     `json:"number"`
	URL                      string                    `json:"url"`
	State                    string                    `json:"state"`
	Draft                    bool                      `json:"draft"`
	Base                     PullRequestRefObservation `json:"base"`
	Head                     PullRequestRefObservation `json:"head"`
}

type Publisher struct {
	workspace string
	repo      string
	git       string
	gh        string
	binding   RepositoryBinding
}

func New(workspace, repo string, binding RepositoryBinding) (*Publisher, error) {
	if !ValidComponent(workspace) {
		return nil, fmt.Errorf("workspace name %q is not safe for a GitHub branch", workspace)
	}
	if strings.TrimSpace(repo) == "" {
		return nil, fmt.Errorf("repository path is required")
	}
	if err := validateRepositoryBinding(binding); err != nil {
		return nil, err
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
	return &Publisher{workspace: workspace, repo: repo, git: git, gh: gh, binding: binding}, nil
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
	if repository.Name != publisher.binding.FullName {
		return Prepared{}, fmt.Errorf("checkout origin does not match the configured GitHub repository")
	}
	if request.Operation == "" {
		request.Operation = repository.Head[:12]
	}
	token, err := githubCredential(ctx, publisher.gh)
	if err != nil {
		return Prepared{}, err
	}
	remoteRepository, err := publisher.inspectGitHubRepository(ctx, token)
	if err != nil {
		return Prepared{}, err
	}
	base := request.Base
	if base == "" {
		base = remoteRepository.DefaultBranch
		if !ValidBranch(base) {
			return Prepared{}, fmt.Errorf("GitHub returned unsupported default branch %q", base)
		}
	}
	gitEnv := []string{"FERN_GITHUB_TOKEN=" + token, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	canonical := "https://github.com/" + publisher.binding.FullName + ".git"
	if err := run(ctx, publisher.repo, gitEnv, publisher.git, secureGitArgs("fetch", "--no-tags", canonical, "refs/heads/"+base)...); err != nil {
		return Prepared{}, fmt.Errorf("fetch GitHub base branch: %w", err)
	}
	baseSHA, err := output(ctx, publisher.repo, nil, publisher.git, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	baseSHA = strings.TrimSpace(baseSHA)
	if err != nil || !validSHA(baseSHA) {
		return Prepared{}, fmt.Errorf("resolve fetched GitHub base commit")
	}
	if err := run(ctx, publisher.repo, nil, publisher.git, "merge-base", "--is-ancestor", baseSHA, repository.Head); err != nil {
		return Prepared{}, fmt.Errorf("HEAD is not descended from the current GitHub base branch")
	}
	changed, err := output(ctx, publisher.repo, nil, publisher.git, "diff", "--name-only", "-z", baseSHA+".."+repository.Head)
	if err != nil {
		return Prepared{}, fmt.Errorf("inspect publication changes: %w", err)
	}
	if err := validatePublicationPaths(changed); err != nil {
		return Prepared{}, err
	}
	prepared := Prepared{
		RepositoryID:       publisher.binding.ID,
		RepositoryFullName: publisher.binding.FullName,
		BaseSHA:            baseSHA,
		BaseRef:            base,
		ResultCommit:       repository.Head,
		Branch:             "fern/" + publisher.workspace + "/" + request.Operation,
	}
	if err := validatePrepared(prepared, publisher.workspace); err != nil {
		return Prepared{}, err
	}
	return prepared, nil
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
	if err := validatePrepared(prepared, publisher.workspace); err != nil {
		return Result{}, err
	}
	if prepared.RepositoryID != publisher.binding.ID || prepared.RepositoryFullName != publisher.binding.FullName {
		return Result{}, fmt.Errorf("recorded publication repository does not match the configured binding")
	}
	if err := ValidateRequest(Request{Operation: strings.TrimPrefix(prepared.Branch, "fern/"+publisher.workspace+"/"), Base: prepared.BaseRef, Title: title, Body: body}); err != nil {
		return Result{}, err
	}
	repository, err := inspectRepositoryName(ctx, publisher.repo, publisher.git)
	if err != nil {
		return Result{}, err
	}
	if repository != publisher.binding.FullName {
		return Result{}, fmt.Errorf("checkout origin does not match the configured GitHub repository")
	}
	if err := run(ctx, publisher.repo, nil, publisher.git, "cat-file", "-e", prepared.ResultCommit+"^{commit}"); err != nil {
		return Result{}, fmt.Errorf("recorded publication commit is unavailable locally")
	}
	tree, err := output(ctx, publisher.repo, nil, publisher.git, "ls-tree", "-r", "--full-tree", "-z", prepared.ResultCommit)
	if err != nil {
		return Result{}, fmt.Errorf("inspect recorded publication tree: %w", err)
	}
	if err := validateNoSubmodules(tree); err != nil {
		return Result{}, err
	}
	token, err := githubCredential(ctx, publisher.gh)
	if err != nil {
		return Result{}, err
	}
	if _, err := publisher.inspectGitHubRepository(ctx, token); err != nil {
		return Result{}, err
	}
	gitEnv := []string{"FERN_GITHUB_TOKEN=" + token, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	canonical := "https://github.com/" + prepared.RepositoryFullName + ".git"
	if err := run(ctx, publisher.repo, gitEnv, publisher.git, secureGitArgs("fetch", "--no-tags", canonical, "refs/heads/"+prepared.BaseRef)...); err != nil {
		return Result{}, fmt.Errorf("revalidate GitHub base branch: %w", err)
	}
	baseSHA, resolveErr := output(ctx, publisher.repo, nil, publisher.git, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if resolveErr != nil || strings.TrimSpace(baseSHA) != prepared.BaseSHA {
		return Result{}, fmt.Errorf("GitHub base branch moved since publication was prepared")
	}
	remoteCommit, err := remoteBranchCommit(ctx, publisher.repo, gitEnv, publisher.git, canonical, prepared.Branch)
	if err != nil {
		return Result{}, err
	}
	if remoteCommit != "" && remoteCommit != prepared.ResultCommit {
		return Result{}, fmt.Errorf("remote Fern branch conflicts with recorded commit")
	}
	if remoteCommit == "" {
		pushErr := run(ctx, publisher.repo, gitEnv, publisher.git, secureGitArgs("push", canonical, prepared.ResultCommit+":refs/heads/"+prepared.Branch)...)
		remoteCommit, err = remoteBranchCommit(ctx, publisher.repo, gitEnv, publisher.git, canonical, prepared.Branch)
		if err != nil {
			return Result{}, fmt.Errorf("reconcile remote Fern branch after push: %w", err)
		}
		if remoteCommit != prepared.ResultCommit {
			if remoteCommit != "" {
				return Result{}, fmt.Errorf("remote Fern branch conflicts with recorded commit")
			}
			if pushErr == nil {
				return Result{}, fmt.Errorf("remote Fern branch does not contain the recorded commit")
			}
			return Result{}, fmt.Errorf("push exact commit to %s: %w", prepared.Branch, pushErr)
		}
	}
	ghEnv := []string{"GH_TOKEN=" + token}
	number, found, err := findPullRequest(ctx, ghEnv, publisher.gh, prepared)
	if err != nil {
		return Result{}, err
	}
	if !found {
		createdNumber, createErr := createPullRequest(ctx, ghEnv, publisher.gh, prepared, strings.TrimSpace(title), body)
		number, found, err = findPullRequest(ctx, ghEnv, publisher.gh, prepared)
		if err != nil {
			return Result{}, err
		}
		if !found {
			return Result{}, fmt.Errorf("create draft pull request: response was not reconciled")
		}
		if createErr == nil && createdNumber != number {
			return Result{}, fmt.Errorf("created pull request conflicts with discovery")
		}
	}
	observation, err := getPullRequest(ctx, ghEnv, publisher.gh, prepared, number)
	if err != nil {
		return Result{}, err
	}
	return Result{Prepared: prepared, PullRequest: observation}, nil
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

type apiRepository struct {
	ID            *int64  `json:"id"`
	FullName      *string `json:"full_name"`
	Name          *string `json:"name"`
	DefaultBranch *string `json:"default_branch"`
	Owner         *struct {
		Login *string `json:"login"`
	} `json:"owner"`
}

type apiPullRequest struct {
	Number  *int64      `json:"number"`
	HTMLURL *string     `json:"html_url"`
	State   *string     `json:"state"`
	Draft   *bool       `json:"draft"`
	Base    *apiPullRef `json:"base"`
	Head    *apiPullRef `json:"head"`
}

type apiPullRef struct {
	Ref  *string      `json:"ref"`
	SHA  *string      `json:"sha"`
	Repo *apiPullRepo `json:"repo"`
}

type apiPullRepo struct {
	ID       *int64  `json:"id"`
	FullName *string `json:"full_name"`
	Name     *string `json:"name"`
	Owner    *struct {
		Login *string `json:"login"`
	} `json:"owner"`
}

type repositoryObservation struct {
	DefaultBranch string
}

func (publisher *Publisher) inspectGitHubRepository(ctx context.Context, token string) (repositoryObservation, error) {
	value, err := ghAPI(ctx, []string{"GH_TOKEN=" + token}, publisher.gh, "GET", "repositories/"+strconv.FormatInt(publisher.binding.ID, 10), nil)
	if err != nil {
		return repositoryObservation{}, fmt.Errorf("verify configured GitHub repository: %w", err)
	}
	var response apiRepository
	if err := decodeAPIJSON(value, &response); err != nil {
		return repositoryObservation{}, fmt.Errorf("GitHub returned an invalid repository response")
	}
	owner, name, _ := strings.Cut(publisher.binding.FullName, "/")
	if response.ID == nil || *response.ID != publisher.binding.ID || response.FullName == nil || *response.FullName != publisher.binding.FullName || response.Owner == nil || response.Owner.Login == nil || *response.Owner.Login != owner || response.Name == nil || *response.Name != name || response.DefaultBranch == nil || !ValidBranch(*response.DefaultBranch) {
		return repositoryObservation{}, fmt.Errorf("GitHub repository identity does not match the configured binding")
	}
	return repositoryObservation{DefaultBranch: *response.DefaultBranch}, nil
}

func findPullRequest(ctx context.Context, env []string, gh string, prepared Prepared) (int64, bool, error) {
	owner, _, _ := strings.Cut(prepared.RepositoryFullName, "/")
	query := url.Values{}
	query.Set("state", "open")
	query.Set("base", prepared.BaseRef)
	query.Set("head", owner+":"+prepared.Branch)
	query.Set("per_page", "2")
	endpoint := "repos/" + prepared.RepositoryFullName + "/pulls?" + query.Encode()
	value, err := ghAPI(ctx, env, gh, "GET", endpoint, nil)
	if err != nil {
		return 0, false, fmt.Errorf("inspect existing pull request: %w", err)
	}
	var pulls *[]apiPullRequest
	if err := decodeAPIJSON(value, &pulls); err != nil || pulls == nil {
		return 0, false, fmt.Errorf("GitHub returned an invalid pull request discovery response")
	}
	if len(*pulls) > 1 {
		return 0, false, fmt.Errorf("multiple GitHub pull requests match the recorded publication")
	}
	if len(*pulls) == 0 {
		return 0, false, nil
	}
	pull := (*pulls)[0]
	if pull.Number == nil || *pull.Number <= 0 || !validDiscoveryRef(pull.Base, prepared, false) || !validDiscoveryRef(pull.Head, prepared, true) {
		return 0, false, fmt.Errorf("existing pull request conflicts with the recorded publication")
	}
	return *pull.Number, true, nil
}

func validDiscoveryRef(ref *apiPullRef, prepared Prepared, head bool) bool {
	wantRef := prepared.BaseRef
	wantSHA := prepared.BaseSHA
	if head {
		wantRef, wantSHA = prepared.Branch, prepared.ResultCommit
	}
	owner, name, _ := strings.Cut(prepared.RepositoryFullName, "/")
	return ref != nil && ref.Ref != nil && *ref.Ref == wantRef && ref.SHA != nil && *ref.SHA == wantSHA && validAPIRepository(ref.Repo, prepared.RepositoryID, prepared.RepositoryFullName, owner, name)
}

func createPullRequest(ctx context.Context, env []string, gh string, prepared Prepared, title, body string) (int64, error) {
	owner, _, _ := strings.Cut(prepared.RepositoryFullName, "/")
	payload, err := json.Marshal(struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Draft bool   `json:"draft"`
	}{Title: title, Body: body, Head: owner + ":" + prepared.Branch, Base: prepared.BaseRef, Draft: true})
	if err != nil {
		return 0, fmt.Errorf("encode draft pull request")
	}
	value, err := ghAPI(ctx, env, gh, "POST", "repos/"+prepared.RepositoryFullName+"/pulls", payload)
	if err != nil {
		return 0, err
	}
	var response struct {
		Number *int64 `json:"number"`
	}
	if err := decodeAPIJSON(value, &response); err != nil || response.Number == nil || *response.Number <= 0 {
		return 0, fmt.Errorf("GitHub returned an invalid pull request creation response")
	}
	return *response.Number, nil
}

func getPullRequest(ctx context.Context, env []string, gh string, prepared Prepared, number int64) (PullRequestObservation, error) {
	value, err := ghAPI(ctx, env, gh, "GET", "repos/"+prepared.RepositoryFullName+"/pulls/"+strconv.FormatInt(number, 10), nil)
	if err != nil {
		return PullRequestObservation{}, fmt.Errorf("read exact pull request: %w", err)
	}
	var pull apiPullRequest
	if err := decodeAPIJSON(value, &pull); err != nil {
		return PullRequestObservation{}, fmt.Errorf("GitHub returned an invalid pull request response")
	}
	if pull.Number == nil || *pull.Number != number || pull.HTMLURL == nil || pull.State == nil || *pull.State != "open" || pull.Draft == nil || !*pull.Draft || !validDiscoveryRef(pull.Base, prepared, false) || !validDiscoveryRef(pull.Head, prepared, true) {
		return PullRequestObservation{}, fmt.Errorf("pull request proof conflicts with the recorded publication")
	}
	if err := validatePullURL(*pull.HTMLURL, prepared.RepositoryFullName); err != nil || *pull.HTMLURL != "https://github.com/"+prepared.RepositoryFullName+"/pull/"+strconv.FormatInt(number, 10) {
		return PullRequestObservation{}, fmt.Errorf("GitHub returned an invalid pull request URL")
	}
	return PullRequestObservation{
		TargetRepositoryID: prepared.RepositoryID, TargetRepositoryFullName: prepared.RepositoryFullName,
		Number: number, URL: *pull.HTMLURL, State: *pull.State, Draft: *pull.Draft,
		Base: makePullRefObservation(pull.Base), Head: makePullRefObservation(pull.Head),
	}, nil
}

func validAPIRepository(repository *apiPullRepo, id int64, fullName, owner, name string) bool {
	return repository != nil && repository.ID != nil && *repository.ID == id && repository.FullName != nil && *repository.FullName == fullName && repository.Owner != nil && repository.Owner.Login != nil && *repository.Owner.Login == owner && repository.Name != nil && *repository.Name == name
}

func makePullRefObservation(ref *apiPullRef) PullRequestRefObservation {
	return PullRequestRefObservation{
		RepositoryID: *ref.Repo.ID, RepositoryFullName: *ref.Repo.FullName,
		RepositoryOwner: *ref.Repo.Owner.Login, RepositoryName: *ref.Repo.Name,
		Ref: *ref.Ref, SHA: *ref.SHA,
	}
}

func decodeAPIJSON(value string, destination any) error {
	if !utf8.ValidString(value) || len(value) == 0 {
		return fmt.Errorf("invalid JSON encoding")
	}
	tokens := json.NewDecoder(strings.NewReader(value))
	tokens.UseNumber()
	if err := validateUniqueJSONValue(tokens, 0); err != nil {
		return err
	}
	if token, err := tokens.Token(); err != io.EOF || token != nil {
		return fmt.Errorf("invalid trailing JSON")
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid trailing JSON")
	}
	return nil
}

func validateUniqueJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return fmt.Errorf("JSON nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			key, ok := keyToken.(string)
			if err != nil || !ok {
				return fmt.Errorf("invalid JSON object key")
			}
			canonical := strings.ToLower(key)
			if _, exists := keys[canonical]; exists {
				return fmt.Errorf("duplicate JSON object key")
			}
			keys[canonical] = struct{}{}
			if err := validateUniqueJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if err := validateUniqueJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("invalid JSON array")
		}
	default:
		return fmt.Errorf("invalid JSON delimiter")
	}
	return nil
}

func validatePrepared(prepared Prepared, workspace string) error {
	if err := validateRepositoryBinding(RepositoryBinding{ID: prepared.RepositoryID, FullName: prepared.RepositoryFullName}); err != nil {
		return err
	}
	if !ValidBranch(prepared.BaseRef) {
		return fmt.Errorf("recorded publication base is invalid")
	}
	prefix := "fern/" + workspace + "/"
	operation := strings.TrimPrefix(prepared.Branch, prefix)
	if !strings.HasPrefix(prepared.Branch, prefix) || !ValidComponent(operation) || prepared.Branch != prefix+operation {
		return fmt.Errorf("recorded publication branch must be %s<operation>", prefix)
	}
	if prepared.Branch == prepared.BaseRef {
		return fmt.Errorf("recorded publication branch must differ from its base")
	}
	if !validSHA(prepared.BaseSHA) || !validSHA(prepared.ResultCommit) {
		return fmt.Errorf("recorded publication commits are invalid")
	}
	return nil
}

func validateRepositoryBinding(binding RepositoryBinding) error {
	if binding.ID <= 0 {
		return fmt.Errorf("configured GitHub repository ID must be positive")
	}
	if !validRepositoryFullName(binding.FullName) {
		return fmt.Errorf("configured GitHub repository full name is invalid")
	}
	return nil
}

func validRepositoryFullName(value string) bool {
	if len(value) < 3 || len(value) > 140 || strings.Count(value, "/") != 1 {
		return false
	}
	owner, repository, _ := strings.Cut(value, "/")
	if len(owner) == 0 || len(owner) > 39 || owner[0] == '-' || owner[len(owner)-1] == '-' || len(repository) == 0 || len(repository) > 100 || repository == "." || repository == ".." || strings.HasSuffix(strings.ToLower(repository), ".git") {
		return false
	}
	for index := range len(owner) {
		if character := owner[index]; !asciiAlphaNumeric(character) && character != '-' {
			return false
		}
	}
	for index := range len(repository) {
		if character := repository[index]; !asciiAlphaNumeric(character) && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func asciiAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
}

func validatePullURL(value, repository string) error {
	parsed, err := url.Parse(value)
	wantPrefix := "/" + repository + "/pull/"
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || !strings.EqualFold(parsed.Host, "github.com") || parsed.Opaque != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || !strings.HasPrefix(parsed.Path, wantPrefix) {
		return fmt.Errorf("GitHub returned an invalid pull request URL")
	}
	number := strings.TrimPrefix(parsed.Path, wantPrefix)
	parsedNumber, err := strconv.Atoi(number)
	if err != nil || parsedNumber <= 0 || strconv.Itoa(parsedNumber) != number {
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

func inspectRepositoryName(ctx context.Context, path, git string) (string, error) {
	gitEnv := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	configOutput, err := output(ctx, path, gitEnv, git, "config", "--local", "--null", "--list")
	if err != nil {
		return "", fmt.Errorf("inspect local Git configuration: %w", err)
	}
	if err := ValidateLocalGitConfig(configOutput); err != nil {
		return "", err
	}
	origin, err := output(ctx, path, gitEnv, git, "remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("read origin: %w", err)
	}
	return GitHubRepositoryName(strings.TrimSpace(origin))
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
	index, err := output(ctx, path, nil, git, "ls-files", "--stage", "-z")
	if err != nil {
		return Repository{}, fmt.Errorf("inspect repository index: %w", err)
	}
	if err := validateNoSubmodules(index); err != nil {
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

func validateNoSubmodules(entries string) error {
	for _, entry := range strings.Split(entries, "\x00") {
		_, path, _ := strings.Cut(entry, "\t")
		if strings.HasPrefix(entry, "160000 ") || path == ".gitmodules" {
			return fmt.Errorf("publishing repositories with submodules is not supported")
		}
	}
	return nil
}

func ValidateLocalGitConfig(config string) error {
	for _, entry := range strings.Split(config, "\x00") {
		key, _, _ := strings.Cut(entry, "\n")
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		dangerousPrefix := strings.HasPrefix(key, "url.") || strings.HasPrefix(key, "credential.") || strings.HasPrefix(key, "include.") || strings.HasPrefix(key, "http.") || strings.HasPrefix(key, "https.") || strings.HasPrefix(key, "filter.") || strings.HasPrefix(key, "diff.") || strings.HasPrefix(key, "merge.") || strings.HasPrefix(key, "submodule.") || strings.HasPrefix(key, "protocol.") || strings.HasPrefix(key, "uploadpack.") || strings.HasPrefix(key, "receive.") || strings.HasPrefix(key, "pack.")
		dangerousCore := key == "core.sshcommand" || key == "core.hookspath" || key == "core.gitproxy" || key == "core.fsmonitor" || key == "core.attributesfile" || key == "core.excludesfile" || key == "core.alternaterefscommand" || key == "core.worktree"
		dangerousWorktree := key == "extensions.worktreeconfig"
		if dangerousPrefix || dangerousCore || dangerousWorktree || strings.HasPrefix(key, "remote.") && strings.HasSuffix(key, ".pushurl") {
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
		if err != nil || parsed.Port() != "" || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", fmt.Errorf("origin must be a credential-free github.com URL")
		}
		allowedUser := parsed.User == nil && parsed.Scheme == "https"
		if parsed.User != nil && parsed.Scheme == "ssh" && parsed.User.Username() == "git" {
			_, hasPassword := parsed.User.Password()
			allowedUser = !hasPassword
		}
		if !allowedUser || !strings.EqualFold(parsed.Hostname(), "github.com") {
			return "", fmt.Errorf("origin must be a credential-free github.com URL")
		}
		path = strings.TrimPrefix(parsed.Path, "/")
	}
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	name := strings.Join(parts, "/")
	if len(parts) != 2 || !validRepositoryFullName(name) {
		return "", fmt.Errorf("origin must identify one GitHub OWNER/REPOSITORY")
	}
	return name, nil
}

func ValidComponent(value string) bool {
	return componentPattern.MatchString(value) && !strings.Contains(value, "..") && !strings.HasSuffix(value, ".lock") && !strings.HasSuffix(value, ".")
}

func ValidBranch(value string) bool {
	if value == "" || value == "@" || len(value) > 255 || strings.HasPrefix(value, "/") || strings.HasSuffix(value, ".") || strings.HasSuffix(value, "/") || strings.Contains(value, "..") || strings.Contains(value, "//") || strings.Contains(value, "@{") || strings.ContainsAny(value, " ~^:?*[\\") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
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

func validSHA(value string) bool {
	return len(value) == 40 && isHex(value)
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
		if containsCredential(extraEnv) {
			return fmt.Errorf("%s failed", filepath.Base(name))
		}
		return fmt.Errorf("%s", message)
	}
	return nil
}

func containsCredential(environment []string) bool {
	for _, value := range environment {
		if strings.HasPrefix(value, "FERN_GITHUB_TOKEN=") || strings.HasPrefix(value, "GH_TOKEN=") || strings.HasPrefix(value, "GITHUB_TOKEN=") {
			return true
		}
	}
	return false
}

func output(ctx context.Context, directory string, extraEnv []string, name string, args ...string) (string, error) {
	return outputInput(ctx, directory, extraEnv, nil, name, args...)
}

func outputInput(ctx context.Context, directory string, extraEnv []string, input []byte, name string, args ...string) (string, error) {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(commandCtx, name, args...)
	command.Dir = directory
	command.Env = append(sanitizedEnvironment(), extraEnv...)
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
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

func ghAPI(ctx context.Context, env []string, gh, method, endpoint string, input []byte) (string, error) {
	args := []string{"api", "--hostname", "github.com", "--method", method, endpoint}
	if input != nil {
		args = append(args, "--input", "-")
	}
	return outputInput(ctx, "", env, input, gh, args...)
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
