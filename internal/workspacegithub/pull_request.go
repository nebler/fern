package workspacegithub

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	ErrPullRequestConflict = errors.New("GitHub pull request conflicts with the authorized tuple")
	ErrPullRequestUnknown  = errors.New("GitHub pull request effect could not be observed")
)

const pullRequestProjection = `{number: .number, url: .html_url, state: .state, draft: .draft, baseRef: .base.ref, baseSha: .base.sha, headRef: .head.ref, headSha: .head.sha, baseRepositoryId: .base.repo.id, headRepositoryId: .head.repo.id}`

type PullRequest struct {
	Number           int64  `json:"number"`
	URL              string `json:"url"`
	State            string `json:"state"`
	Draft            bool   `json:"draft"`
	BaseRef          string `json:"baseRef"`
	BaseSHA          string `json:"baseSha"`
	HeadRef          string `json:"headRef"`
	HeadSHA          string `json:"headSha"`
	BaseRepositoryID int64  `json:"baseRepositoryId"`
	HeadRepositoryID int64  `json:"headRepositoryId"`
}

type CreatePullRequest struct {
	RepositoryFullName string
	RepositoryID       int64
	BaseRef            string
	BaseSHA            string
	HeadRef            string
	HeadSHA            string
	Title              string
	Body               string
}

// FindDraftPullRequest is read-only reconciliation for an already-started
// create effect. Zero matches is not proof that a prior mutation did not occur.
func (c *Client) FindDraftPullRequest(ctx context.Context, request CreatePullRequest) (PullRequest, bool, error) {
	owner, _, valid := validatePullRequest(request)
	if !valid {
		return PullRequest{}, false, ErrPullRequestConflict
	}
	query := url.Values{"state": {"open"}, "head": {owner + ":" + request.HeadRef}, "base": {request.BaseRef}, "per_page": {"100"}}
	endpoint := "repos/" + request.RepositoryFullName + "/pulls?" + query.Encode()
	output, err := c.executor.Run(ctx, "api", "--hostname", c.hostname, "--method", "GET", endpoint,
		"--jq", `[.[] | `+pullRequestProjection+`]`)
	if err != nil {
		return PullRequest{}, false, ErrPullRequestUnknown
	}
	var candidates []PullRequest
	if err := decodeBounded(output, &candidates); err != nil {
		return PullRequest{}, false, ErrPullRequestUnknown
	}
	if len(candidates) == 0 {
		return PullRequest{}, false, nil
	}
	if len(candidates) != 1 || !matchesPullRequest(candidates[0], request) {
		return PullRequest{}, false, ErrPullRequestConflict
	}
	return candidates[0], true, nil
}

// CreateDraftPullRequest performs exactly one create call. Callers must commit
// durable create-started intent first and reconcile any returned error by read.
func (c *Client) CreateDraftPullRequest(ctx context.Context, request CreatePullRequest) (PullRequest, error) {
	if _, _, valid := validatePullRequest(request); !valid {
		return PullRequest{}, ErrPullRequestConflict
	}
	output, err := c.executor.Run(ctx, "api", "--hostname", c.hostname, "--method", "POST",
		"repos/"+request.RepositoryFullName+"/pulls", "--raw-field", "title="+request.Title,
		"--raw-field", "body="+request.Body, "--raw-field", "head="+request.HeadRef,
		"--raw-field", "base="+request.BaseRef, "--raw-field", "draft=true", "--jq", pullRequestProjection)
	if err != nil {
		return PullRequest{}, ErrPullRequestUnknown
	}
	var observed PullRequest
	if err := decodeBounded(output, &observed); err != nil {
		return PullRequest{}, ErrPullRequestUnknown
	}
	if !matchesPullRequest(observed, request) {
		return PullRequest{}, ErrPullRequestConflict
	}
	return observed, nil
}

func validatePullRequest(request CreatePullRequest) (string, string, bool) {
	owner, repository, found := strings.Cut(request.RepositoryFullName, "/")
	if !found || !validRepository(request.RepositoryFullName) || request.RepositoryID <= 0 ||
		!validGitRef(request.BaseRef) || !validGitRef(request.HeadRef) || request.BaseRef == request.HeadRef ||
		!validGitOID(request.BaseSHA) || !validGitOID(request.HeadSHA) || request.BaseSHA == request.HeadSHA ||
		!validBoundedText(request.Title, 1, 256) || !validBoundedText(request.Body, 0, 64<<10) {
		return "", "", false
	}
	return owner, repository, true
}

func matchesPullRequest(observed PullRequest, request CreatePullRequest) bool {
	expectedURL := "https://github.com/" + request.RepositoryFullName + "/pull/" + strconv.FormatInt(observed.Number, 10)
	return observed.Number > 0 && validURL(observed.URL) && observed.URL == expectedURL && observed.State == "open" && observed.Draft &&
		observed.BaseRepositoryID == request.RepositoryID && observed.HeadRepositoryID == request.RepositoryID &&
		observed.BaseRef == request.BaseRef && observed.BaseSHA == request.BaseSHA &&
		observed.HeadRef == request.HeadRef && observed.HeadSHA == request.HeadSHA
}

func validGitOID(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := strconv.ParseUint(value[:16], 16, 64)
	if err != nil {
		return false
	}
	for _, character := range value[16:] {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return value == strings.ToLower(value)
}

func validGitRef(value string) bool {
	if len(value) < 1 || len(value) > 255 || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") ||
		strings.Contains(value, "..") || strings.Contains(value, "//") || strings.Contains(value, "@{") || strings.HasSuffix(value, ".lock") {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character == 0x7f || strings.ContainsRune(`~^:?*[\`, character) {
			return false
		}
	}
	return true
}

func validURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host == "github.com" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func validBoundedText(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character == 0 || character == 0x7f {
			return false
		}
	}
	return true
}
