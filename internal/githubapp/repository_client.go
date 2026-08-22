package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxRepositoryRefBytes = 255
	maxPullTitleBytes     = 256
	maxPullBodyBytes      = 60 << 10
	maxJSONDepth          = 64
)

var (
	ErrInvalidRepositoryRequest = errors.New("invalid GitHub repository request")
	ErrInvalidInstallationToken = errors.New("invalid GitHub installation token")
	ErrPullRequestConflict      = errors.New("GitHub pull request conflicts with the requested publication")
	ErrPaginationRefused        = errors.New("GitHub response requires unsupported pagination")
	ErrAmbiguousPullRequests    = errors.New("multiple GitHub pull requests match the requested publication")
)

// PullRequestAmbiguityError reports a count only. It deliberately omits all
// remote response data.
type PullRequestAmbiguityError struct {
	count int
}

func (err *PullRequestAmbiguityError) Error() string {
	return fmt.Sprintf("multiple GitHub pull requests match the requested publication (count %d)", err.count)
}

func (err *PullRequestAmbiguityError) Is(target error) bool {
	return target == ErrAmbiguousPullRequests
}

func (err *PullRequestAmbiguityError) Count() int {
	if err == nil {
		return 0
	}
	return err.count
}

func (err *PullRequestAmbiguityError) GoString() string {
	return err.Error()
}

// RepositoryClient performs only the GitHub REST reads and draft pull request
// creation needed to prove a publication. It is safe for concurrent use when
// its dependencies are safe for concurrent use.
type RepositoryClient struct {
	httpClient  *http.Client
	tokenSource InstallationTokenSource
	apiBase     string
	now         func() time.Time
}

func NewRepositoryClient(httpClient *http.Client, tokenSource InstallationTokenSource, now func() time.Time) (*RepositoryClient, error) {
	if httpClient == nil || tokenSource == nil || isNilInterface(tokenSource) || now == nil {
		return nil, ErrInvalidConfiguration
	}
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &RepositoryClient{
		httpClient:  &clientCopy,
		tokenSource: tokenSource,
		apiBase:     githubAPIBase,
		now:         now,
	}, nil
}

func (client *RepositoryClient) String() string {
	return "GitHub repository publication client"
}

func (client *RepositoryClient) GoString() string {
	return client.String()
}

// RepositoryObservation is an immutable proof of the configured repository.
type RepositoryObservation struct {
	identity      RepositoryIdentity
	fullName      string
	owner         string
	name          string
	defaultBranch string
}

// GitReferenceObservation is the exact immutable commit selected by one
// repository branch reference at the time of admission.
type GitReferenceObservation struct {
	identity RepositoryIdentity
	ref      string
	sha      string
}

func (observation GitReferenceObservation) Identity() RepositoryIdentity { return observation.identity }
func (observation GitReferenceObservation) Ref() string                  { return observation.ref }
func (observation GitReferenceObservation) SHA() string                  { return observation.sha }

func (observation RepositoryObservation) Identity() RepositoryIdentity { return observation.identity }
func (observation RepositoryObservation) RepositoryID() int64 {
	return observation.identity.RepositoryID()
}
func (observation RepositoryObservation) FullName() string      { return observation.fullName }
func (observation RepositoryObservation) Owner() string         { return observation.owner }
func (observation RepositoryObservation) Name() string          { return observation.name }
func (observation RepositoryObservation) DefaultBranch() string { return observation.defaultBranch }

// PullRequestSummary contains only an identity-proven pull request number.
type PullRequestSummary struct {
	identity RepositoryIdentity
	target   string
	number   int64
}

func (summary PullRequestSummary) Identity() RepositoryIdentity { return summary.identity }
func (summary PullRequestSummary) Target() string               { return summary.target }
func (summary PullRequestSummary) Number() int64                { return summary.number }

// PullRequestRefObservation is an immutable observation of one side of a pull
// request. Head observations preserve fork repository identity.
type PullRequestRefObservation struct {
	repositoryID       int64
	repositoryFullName string
	repositoryOwner    string
	repositoryName     string
	ref                string
	sha                string
}

func (observation PullRequestRefObservation) RepositoryID() int64 { return observation.repositoryID }
func (observation PullRequestRefObservation) RepositoryFullName() string {
	return observation.repositoryFullName
}
func (observation PullRequestRefObservation) RepositoryOwner() string {
	return observation.repositoryOwner
}
func (observation PullRequestRefObservation) RepositoryName() string {
	return observation.repositoryName
}
func (observation PullRequestRefObservation) Ref() string { return observation.ref }
func (observation PullRequestRefObservation) SHA() string { return observation.sha }

// PullRequestObservation is the complete remote tuple needed by a publication
// coordinator to compare the PR with its persisted publication intent.
type PullRequestObservation struct {
	targetRepositoryID       int64
	targetRepositoryFullName string
	number                   int64
	htmlURL                  string
	state                    string
	draft                    bool
	base                     PullRequestRefObservation
	head                     PullRequestRefObservation
}

func (observation PullRequestObservation) TargetRepositoryID() int64 {
	return observation.targetRepositoryID
}
func (observation PullRequestObservation) TargetRepositoryFullName() string {
	return observation.targetRepositoryFullName
}
func (observation PullRequestObservation) Number() int64                   { return observation.number }
func (observation PullRequestObservation) HTMLURL() string                 { return observation.htmlURL }
func (observation PullRequestObservation) State() string                   { return observation.state }
func (observation PullRequestObservation) Draft() bool                     { return observation.draft }
func (observation PullRequestObservation) Base() PullRequestRefObservation { return observation.base }
func (observation PullRequestObservation) Head() PullRequestRefObservation { return observation.head }

// RepositoryByID reads the stable numeric repository route and proves that its
// response is exactly the configured owner/name target.
func (client *RepositoryClient) RepositoryByID(ctx context.Context, identity RepositoryIdentity, configuredFullName string) (RepositoryObservation, error) {
	owner, name, err := validateRepositoryCall(ctx, identity, configuredFullName)
	if err != nil {
		return RepositoryObservation{}, err
	}
	payload, _, err := client.request(ctx, identity, http.MethodGet, "/repositories/"+strconv.FormatInt(identity.RepositoryID(), 10), "", nil, http.StatusOK)
	if err != nil {
		return RepositoryObservation{}, err
	}
	var decoded repositoryAPIResponse
	if err := decodeGitHubJSON(payload, &decoded); err != nil || !validRepositoryResponse(decoded, identity.RepositoryID(), configuredFullName, owner, name) || !validGitRef(value(decoded.DefaultBranch)) {
		return RepositoryObservation{}, ErrInvalidResponse
	}
	return RepositoryObservation{
		identity:      identity,
		fullName:      configuredFullName,
		owner:         owner,
		name:          name,
		defaultBranch: *decoded.DefaultBranch,
	}, nil
}

// BranchReference reads one exact refs/heads reference and accepts only a
// direct SHA-1 commit object owned by the configured repository identity.
func (client *RepositoryClient) BranchReference(ctx context.Context, identity RepositoryIdentity, target, branch string) (GitReferenceObservation, error) {
	if _, _, err := validateRepositoryCall(ctx, identity, target); err != nil {
		return GitReferenceObservation{}, err
	}
	if !validGitRef(branch) {
		return GitReferenceObservation{}, ErrInvalidRepositoryRequest
	}
	qualified := "refs/heads/" + branch
	payload, _, err := client.request(ctx, identity, http.MethodGet, repositoryRoute(target)+"git/ref/heads/"+url.PathEscape(branch), "", nil, http.StatusOK)
	if err != nil {
		return GitReferenceObservation{}, err
	}
	var decoded struct {
		Ref    *string `json:"ref"`
		Object *struct {
			Type *string `json:"type"`
			SHA  *string `json:"sha"`
		} `json:"object"`
	}
	if err := decodeGitHubJSON(payload, &decoded); err != nil || decoded.Ref == nil || *decoded.Ref != qualified ||
		decoded.Object == nil || decoded.Object.Type == nil || *decoded.Object.Type != "commit" ||
		decoded.Object.SHA == nil || !validGitSHA1(*decoded.Object.SHA) {
		return GitReferenceObservation{}, ErrInvalidResponse
	}
	return GitReferenceObservation{identity: identity, ref: branch, sha: *decoded.Object.SHA}, nil
}

// FindOpenDraftPullRequests uses one two-item page. A pagination link is
// refused, and two proven matches produce PullRequestAmbiguityError.
func (client *RepositoryClient) FindOpenDraftPullRequests(ctx context.Context, identity RepositoryIdentity, target, base, head string) ([]PullRequestSummary, error) {
	owner, _, err := validateRepositoryCall(ctx, identity, target)
	if err != nil || !validGitRef(base) || !validGitRef(head) {
		return nil, firstError(err, ErrInvalidRepositoryRequest)
	}
	query := url.Values{}
	query.Set("state", "open")
	query.Set("base", base)
	query.Set("head", owner+":"+head)
	query.Set("per_page", "2")
	payload, header, err := client.request(ctx, identity, http.MethodGet, repositoryRoute(target)+"pulls", query.Encode(), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	if header.Get("Link") != "" {
		return nil, ErrPaginationRefused
	}
	var decoded *[]pullRequestAPIResponse
	if err := decodeGitHubJSON(payload, &decoded); err != nil || decoded == nil {
		return nil, ErrInvalidResponse
	}
	summaries := make([]PullRequestSummary, 0, len(*decoded))
	for _, pull := range *decoded {
		if !validDiscoveryPull(pull, identity.RepositoryID(), target, base, head) {
			return nil, ErrPullRequestConflict
		}
		summaries = append(summaries, PullRequestSummary{identity: identity, target: target, number: *pull.Number})
	}
	if len(summaries) > 1 {
		return nil, &PullRequestAmbiguityError{count: len(summaries)}
	}
	return summaries, nil
}

// CreateDraftPullRequest performs exactly one POST and never retries an
// ambiguous or lost response.
func (client *RepositoryClient) CreateDraftPullRequest(ctx context.Context, identity RepositoryIdentity, target, base, head, title, body string) (int64, error) {
	owner, _, err := validateRepositoryCall(ctx, identity, target)
	if err != nil || !validGitRef(base) || !validGitRef(head) || !validPullTitle(title) || !validPullBody(body) {
		return 0, firstError(err, ErrInvalidRepositoryRequest)
	}
	requestBody, err := json.Marshal(struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Draft bool   `json:"draft"`
	}{Title: title, Body: body, Head: owner + ":" + head, Base: base, Draft: true})
	if err != nil {
		return 0, ErrInvalidRepositoryRequest
	}
	payload, _, err := client.request(ctx, identity, http.MethodPost, repositoryRoute(target)+"pulls", "", requestBody, http.StatusCreated)
	if err != nil {
		return 0, err
	}
	var decoded struct {
		Number *int64 `json:"number"`
	}
	if err := decodeGitHubJSON(payload, &decoded); err != nil || decoded.Number == nil || *decoded.Number <= 0 {
		return 0, ErrInvalidResponse
	}
	return *decoded.Number, nil
}

// PullRequest re-reads one exact PR and returns the complete observed tuple.
// Expected publication refs and SHAs are intentionally left to the coordinator.
func (client *RepositoryClient) PullRequest(ctx context.Context, identity RepositoryIdentity, target string, number int64) (PullRequestObservation, error) {
	if _, _, err := validateRepositoryCall(ctx, identity, target); err != nil {
		return PullRequestObservation{}, err
	}
	if number <= 0 {
		return PullRequestObservation{}, ErrInvalidRepositoryRequest
	}
	payload, _, err := client.request(ctx, identity, http.MethodGet, repositoryRoute(target)+"pulls/"+strconv.FormatInt(number, 10), "", nil, http.StatusOK)
	if err != nil {
		return PullRequestObservation{}, err
	}
	var decoded pullRequestAPIResponse
	if err := decodeGitHubJSON(payload, &decoded); err != nil {
		return PullRequestObservation{}, ErrInvalidResponse
	}
	observation, ok := validatePullRequestObservation(decoded, identity.RepositoryID(), target, number)
	if !ok {
		return PullRequestObservation{}, ErrInvalidResponse
	}
	return observation, nil
}

type repositoryAPIResponse struct {
	ID            *int64  `json:"id"`
	FullName      *string `json:"full_name"`
	Name          *string `json:"name"`
	DefaultBranch *string `json:"default_branch"`
	Owner         *struct {
		Login *string `json:"login"`
	} `json:"owner"`
}

func validGitSHA1(value string) bool {
	if len(value) != 40 || value != strings.ToLower(value) {
		return false
	}
	for _, character := range []byte(value) {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

type pullRequestAPIResponse struct {
	Number  *int64                     `json:"number"`
	HTMLURL *string                    `json:"html_url"`
	State   *string                    `json:"state"`
	Draft   *bool                      `json:"draft"`
	Base    *pullRequestRefAPIResponse `json:"base"`
	Head    *pullRequestRefAPIResponse `json:"head"`
}

type pullRequestRefAPIResponse struct {
	Ref  *string                     `json:"ref"`
	SHA  *string                     `json:"sha"`
	Repo *pullRequestRepoAPIResponse `json:"repo"`
}

type pullRequestRepoAPIResponse struct {
	ID       *int64  `json:"id"`
	FullName *string `json:"full_name"`
	Name     *string `json:"name"`
	Owner    *struct {
		Login *string `json:"login"`
	} `json:"owner"`
}

func (client *RepositoryClient) request(ctx context.Context, identity RepositoryIdentity, method, route, rawQuery string, body []byte, expectedStatus int) ([]byte, http.Header, error) {
	if client == nil || client.httpClient == nil || client.tokenSource == nil || client.now == nil || !validAPIBase(client.apiBase) {
		return nil, nil, ErrInvalidConfiguration
	}
	now := client.now().UTC()
	if now.IsZero() || now.Unix() <= 0 {
		return nil, nil, ErrInvalidConfiguration
	}
	token, err := client.tokenSource.InstallationToken(ctx, identity)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		return nil, nil, ErrRequestFailed
	}
	if token.Identity() != identity {
		return nil, nil, ErrInvalidInstallationToken
	}
	permissions := token.Permissions()
	if permissions.Contents() != "write" || permissions.PullRequests() != "write" {
		return nil, nil, ErrInsufficientPermissions
	}
	credential, err := token.Value(now)
	if err != nil {
		return nil, nil, ErrTokenExpired
	}
	if !validAccessToken(credential) {
		return nil, nil, ErrInvalidInstallationToken
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	endpoint := strings.TrimSuffix(client.apiBase, "/") + route
	if rawQuery != "" {
		endpoint += "?" + rawQuery
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, ErrInvalidConfiguration
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "fern-githubapp")
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		return nil, nil, ErrRequestFailed
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, nil, ErrRequestFailed
	}
	if len(payload) > maxResponseBytes {
		return nil, nil, ErrResponseTooLarge
	}
	if response.StatusCode != expectedStatus {
		return nil, nil, &HTTPError{statusCode: response.StatusCode}
	}
	return payload, response.Header.Clone(), nil
}

func validateRepositoryCall(ctx context.Context, identity RepositoryIdentity, target string) (string, string, error) {
	if ctx == nil {
		return "", "", ErrDeadlineRequired
	}
	if _, ok := ctx.Deadline(); !ok {
		return "", "", ErrDeadlineRequired
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if err := identity.validate(); err != nil {
		return "", "", err
	}
	owner, name, ok := splitCanonicalRepository(target)
	if !ok {
		return "", "", ErrInvalidRepositoryRequest
	}
	return owner, name, nil
}

func splitCanonicalRepository(target string) (string, string, bool) {
	if len(target) < 3 || len(target) > 140 || strings.Count(target, "/") != 1 || !asciiString(target) {
		return "", "", false
	}
	owner, name, _ := strings.Cut(target, "/")
	if len(owner) < 1 || len(owner) > 39 || len(name) < 1 || len(name) > 100 || strings.EqualFold(name, ".git") || strings.HasSuffix(strings.ToLower(name), ".git") {
		return "", "", false
	}
	if owner[0] == '-' || owner[len(owner)-1] == '-' || name == "." || name == ".." {
		return "", "", false
	}
	for i := range len(owner) {
		char := owner[i]
		if !asciiAlphaNumeric(char) && char != '-' {
			return "", "", false
		}
	}
	for i := range len(name) {
		char := name[i]
		if !asciiAlphaNumeric(char) && char != '-' && char != '_' && char != '.' {
			return "", "", false
		}
	}
	return owner, name, true
}

func validGitRef(ref string) bool {
	if len(ref) == 0 || len(ref) > maxRepositoryRefBytes || !asciiString(ref) || ref == "@" || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") || strings.Contains(ref, "//") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.ContainsAny(ref, " ~^:?*[\\") {
		return false
	}
	for _, component := range strings.Split(ref, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(strings.ToLower(component), ".lock") {
			return false
		}
	}
	for i := range len(ref) {
		if ref[i] < 0x21 || ref[i] == 0x7f {
			return false
		}
	}
	return true
}

func validPullTitle(title string) bool {
	if title == "" || len(title) > maxPullTitleBytes || !utf8.ValidString(title) || strings.TrimSpace(title) != title {
		return false
	}
	for _, char := range title {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func validPullBody(body string) bool {
	return len(body) <= maxPullBodyBytes && utf8.ValidString(body) && !strings.ContainsRune(body, 0)
}

func validRepositoryResponse(response repositoryAPIResponse, repositoryID int64, fullName, owner, name string) bool {
	return response.ID != nil && *response.ID == repositoryID && response.FullName != nil && *response.FullName == fullName && response.Name != nil && *response.Name == name && response.Owner != nil && response.Owner.Login != nil && *response.Owner.Login == owner && response.DefaultBranch != nil
}

func validDiscoveryPull(pull pullRequestAPIResponse, repositoryID int64, target, base, head string) bool {
	if pull.Number == nil || *pull.Number <= 0 || pull.State == nil || *pull.State != "open" || pull.Draft == nil || !*pull.Draft || pull.Base == nil || pull.Head == nil {
		return false
	}
	return validPullRef(pull.Base, repositoryID, target, base, false) && validPullRef(pull.Head, repositoryID, target, head, false)
}

func validatePullRequestObservation(pull pullRequestAPIResponse, repositoryID int64, target string, number int64) (PullRequestObservation, bool) {
	if pull.Number == nil || *pull.Number != number || pull.HTMLURL == nil || *pull.HTMLURL != canonicalPullURL(target, number) || pull.State == nil || (*pull.State != "open" && *pull.State != "closed") || pull.Draft == nil || pull.Base == nil || pull.Head == nil {
		return PullRequestObservation{}, false
	}
	if pull.Head.Repo == nil || pull.Head.Repo.FullName == nil {
		return PullRequestObservation{}, false
	}
	if !validPullRef(pull.Base, repositoryID, target, value(pull.Base.Ref), true) || !validPullRef(pull.Head, 0, *pull.Head.Repo.FullName, value(pull.Head.Ref), true) {
		return PullRequestObservation{}, false
	}
	base := makePullRefObservation(pull.Base)
	head := makePullRefObservation(pull.Head)
	return PullRequestObservation{
		targetRepositoryID:       repositoryID,
		targetRepositoryFullName: target,
		number:                   number,
		htmlURL:                  *pull.HTMLURL,
		state:                    *pull.State,
		draft:                    *pull.Draft,
		base:                     base,
		head:                     head,
	}, true
}

func validPullRef(ref *pullRequestRefAPIResponse, repositoryID int64, fullName, expectedRef string, requireSHA bool) bool {
	if ref == nil || ref.Ref == nil || *ref.Ref != expectedRef || !validGitRef(*ref.Ref) || ref.Repo == nil || ref.Repo.ID == nil || *ref.Repo.ID <= 0 || ref.Repo.FullName == nil || *ref.Repo.FullName != fullName || ref.Repo.Name == nil || ref.Repo.Owner == nil || ref.Repo.Owner.Login == nil {
		return false
	}
	if repositoryID > 0 && *ref.Repo.ID != repositoryID {
		return false
	}
	owner, name, ok := splitCanonicalRepository(*ref.Repo.FullName)
	if !ok || *ref.Repo.Owner.Login != owner || *ref.Repo.Name != name {
		return false
	}
	if requireSHA && (ref.SHA == nil || !validGitSHA(*ref.SHA)) {
		return false
	}
	return true
}

func makePullRefObservation(ref *pullRequestRefAPIResponse) PullRequestRefObservation {
	return PullRequestRefObservation{
		repositoryID:       *ref.Repo.ID,
		repositoryFullName: *ref.Repo.FullName,
		repositoryOwner:    *ref.Repo.Owner.Login,
		repositoryName:     *ref.Repo.Name,
		ref:                *ref.Ref,
		sha:                *ref.SHA,
	}
}

func validGitSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for i := range len(sha) {
		if (sha[i] < '0' || sha[i] > '9') && (sha[i] < 'a' || sha[i] > 'f') {
			return false
		}
	}
	return true
}

func repositoryRoute(target string) string {
	owner, name, _ := strings.Cut(target, "/")
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/"
}

func canonicalPullURL(target string, number int64) string {
	return "https://github.com/" + target + "/pull/" + strconv.FormatInt(number, 10)
}

func validAPIBase(base string) bool {
	parsed, err := url.Parse(base)
	return err == nil && parsed.IsAbs() && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.RawPath == "" && parsed.Path == ""
}

func decodeGitHubJSON(payload []byte, destination any) error {
	if len(payload) == 0 || !utf8.Valid(payload) {
		return ErrInvalidResponse
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := validateJSONValue(decoder, 0); err != nil {
		return ErrInvalidResponse
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrInvalidResponse
	}
	if err := json.Unmarshal(payload, destination); err != nil {
		return ErrInvalidResponse
	}
	return nil
}

func validateJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return ErrInvalidResponse
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
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return ErrInvalidResponse
			}
			canonicalKey := strings.ToLower(key)
			if _, duplicate := keys[canonicalKey]; duplicate {
				return ErrInvalidResponse
			}
			keys[canonicalKey] = struct{}{}
			if err := validateJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return ErrInvalidResponse
		}
	case '[':
		for decoder.More() {
			if err := validateJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return ErrInvalidResponse
		}
	default:
		return ErrInvalidResponse
	}
	return nil
}

func value[T comparable](pointer *T) T {
	if pointer == nil {
		var zero T
		return zero
	}
	return *pointer
}

func firstError(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func asciiString(value string) bool {
	for i := range len(value) {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func asciiAlphaNumeric(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
}

func isNilInterface(value any) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
