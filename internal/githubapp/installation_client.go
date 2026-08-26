package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nebler/fern/internal/gitref"
)

const (
	installationPageSize = 100
	installationMaxPages = 10
)

var (
	ErrInvalidInstallationRequest = errors.New("invalid GitHub installation discovery request")
	ErrInvalidDiscoveryToken      = errors.New("invalid GitHub installation discovery token")
	ErrInstallationConflict       = errors.New("conflicting GitHub installation observations")
	ErrRepositorySelection        = errors.New("GitHub repository selection could not be proven")
)

// InstallationConflictError reports only the number of conflicting
// observations. Remote account and repository names are deliberately omitted.
type InstallationConflictError struct {
	count int
}

func (err *InstallationConflictError) Error() string {
	return fmt.Sprintf("conflicting GitHub installation observations (count %d)", err.Count())
}

func (err *InstallationConflictError) GoString() string { return err.Error() }

func (err *InstallationConflictError) Is(target error) bool {
	return target == ErrInstallationConflict
}

func (err *InstallationConflictError) Count() int {
	if err == nil {
		return 0
	}
	return err.count
}

// RepositorySelectionError intentionally carries no remote values.
type RepositorySelectionError struct{}

func (*RepositorySelectionError) Error() string        { return ErrRepositorySelection.Error() }
func (err *RepositorySelectionError) GoString() string { return err.Error() }
func (*RepositorySelectionError) Is(target error) bool { return target == ErrRepositorySelection }

// InstallationDiscoveryTokenSource supplies an installation-wide credential.
// It is deliberately separate from repository-scoped publication credentials.
type InstallationDiscoveryTokenSource interface {
	InstallationDiscoveryToken(context.Context, int64) (InstallationDiscoveryToken, error)
}

// InstallationDiscoveryPermissions is an immutable snapshot of the minimum
// permissions required to discover and later publish to a repository.
type InstallationDiscoveryPermissions struct {
	metadata     string
	contents     string
	pullRequests string
}

func ValidateInstallationDiscoveryPermissions(values map[string]string) (InstallationDiscoveryPermissions, error) {
	for name, level := range values {
		if !validPermissionName(name) || (level != "read" && level != "write") {
			return InstallationDiscoveryPermissions{}, ErrInsufficientPermissions
		}
	}
	if values["metadata"] != "read" || values["contents"] != "write" || values["pull_requests"] != "write" {
		return InstallationDiscoveryPermissions{}, ErrInsufficientPermissions
	}
	return InstallationDiscoveryPermissions{metadata: "read", contents: "write", pullRequests: "write"}, nil
}

func (permissions InstallationDiscoveryPermissions) Metadata() string { return permissions.metadata }
func (permissions InstallationDiscoveryPermissions) Contents() string { return permissions.contents }
func (permissions InstallationDiscoveryPermissions) PullRequests() string {
	return permissions.pullRequests
}

func (permissions InstallationDiscoveryPermissions) valid() bool {
	return permissions.metadata == "read" && permissions.contents == "write" && permissions.pullRequests == "write"
}

// InstallationDiscoveryToken is an opaque installation-wide credential.
type InstallationDiscoveryToken struct {
	value          string
	expiresAt      time.Time
	installationID int64
	permissions    InstallationDiscoveryPermissions
}

func NewInstallationDiscoveryToken(value string, expiresAt time.Time, installationID int64, permissions map[string]string) (InstallationDiscoveryToken, error) {
	validated, err := ValidateInstallationDiscoveryPermissions(permissions)
	if err != nil || !validAccessToken(value) || installationID <= 0 || expiresAt.IsZero() || expiresAt.Unix() <= 0 {
		return InstallationDiscoveryToken{}, firstError(err, ErrInvalidDiscoveryToken)
	}
	return InstallationDiscoveryToken{
		value:          value,
		expiresAt:      expiresAt.UTC(),
		installationID: installationID,
		permissions:    validated,
	}, nil
}

func (token InstallationDiscoveryToken) Value(now time.Time) (string, error) {
	if !validAccessToken(token.value) || token.installationID <= 0 || !token.permissions.valid() || now.IsZero() || token.expiresAt.After(now.Add(maximumTokenLife)) {
		return "", ErrInvalidDiscoveryToken
	}
	if !now.Add(minimumTokenLife).Before(token.expiresAt) {
		return "", ErrTokenExpired
	}
	return token.value, nil
}

func (token InstallationDiscoveryToken) ExpiresAt() time.Time  { return token.expiresAt }
func (token InstallationDiscoveryToken) InstallationID() int64 { return token.installationID }
func (token InstallationDiscoveryToken) Permissions() InstallationDiscoveryPermissions {
	return token.permissions
}
func (InstallationDiscoveryToken) String() string         { return "GitHub installation discovery token" }
func (token InstallationDiscoveryToken) GoString() string { return token.String() }

// InstallationClient discovers installations and repositories without storing
// credentials or applying onboarding selection policy.
type InstallationClient struct {
	httpClient      *http.Client
	appTokens       AppTokenSource
	discoveryTokens InstallationDiscoveryTokenSource
	apiBase         string
	now             func() time.Time
}

func NewInstallationClient(httpClient *http.Client, appTokens AppTokenSource, discoveryTokens InstallationDiscoveryTokenSource, now func() time.Time) (*InstallationClient, error) {
	if httpClient == nil || appTokens == nil || isNilInterface(appTokens) || discoveryTokens == nil || isNilInterface(discoveryTokens) || now == nil {
		return nil, ErrInvalidConfiguration
	}
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &InstallationClient{
		httpClient:      &clientCopy,
		appTokens:       appTokens,
		discoveryTokens: discoveryTokens,
		apiBase:         githubAPIBase,
		now:             now,
	}, nil
}

func (*InstallationClient) String() string          { return "GitHub installation discovery client" }
func (client *InstallationClient) GoString() string { return client.String() }

// InstallationObservation is an immutable installation/account tuple.
type InstallationObservation struct {
	installationID      int64
	accountLogin        string
	accountID           int64
	accountType         string
	targetType          string
	repositorySelection string
}

func (observation InstallationObservation) InstallationID() int64 { return observation.installationID }
func (observation InstallationObservation) AccountLogin() string  { return observation.accountLogin }
func (observation InstallationObservation) AccountID() int64      { return observation.accountID }
func (observation InstallationObservation) AccountType() string   { return observation.accountType }
func (observation InstallationObservation) TargetType() string    { return observation.targetType }
func (observation InstallationObservation) RepositorySelection() string {
	return observation.repositorySelection
}
func (InstallationObservation) String() string               { return "GitHub installation observation" }
func (observation InstallationObservation) GoString() string { return observation.String() }

// InstallationRepositoryObservation is an immutable repository tuple returned
// under one exact installation-wide token identity.
type InstallationRepositoryObservation struct {
	installationID int64
	repositoryID   int64
	fullName       string
	ownerLogin     string
	ownerID        int64
	ownerType      string
	name           string
	private        bool
	archived       bool
	disabled       bool
	defaultBranch  string
	permissions    InstallationDiscoveryPermissions
	canPull        bool
	canPush        bool
}

func (observation InstallationRepositoryObservation) InstallationID() int64 {
	return observation.installationID
}
func (observation InstallationRepositoryObservation) RepositoryID() int64 {
	return observation.repositoryID
}
func (observation InstallationRepositoryObservation) FullName() string { return observation.fullName }
func (observation InstallationRepositoryObservation) OwnerLogin() string {
	return observation.ownerLogin
}
func (observation InstallationRepositoryObservation) OwnerID() int64    { return observation.ownerID }
func (observation InstallationRepositoryObservation) OwnerType() string { return observation.ownerType }
func (observation InstallationRepositoryObservation) Name() string      { return observation.name }
func (observation InstallationRepositoryObservation) Private() bool     { return observation.private }
func (observation InstallationRepositoryObservation) Archived() bool    { return observation.archived }
func (observation InstallationRepositoryObservation) Disabled() bool    { return observation.disabled }
func (observation InstallationRepositoryObservation) DefaultBranch() string {
	return observation.defaultBranch
}
func (observation InstallationRepositoryObservation) Permissions() InstallationDiscoveryPermissions {
	return observation.permissions
}
func (observation InstallationRepositoryObservation) CanPull() bool { return observation.canPull }
func (observation InstallationRepositoryObservation) CanPush() bool { return observation.canPush }
func (InstallationRepositoryObservation) String() string {
	return "GitHub installation repository observation"
}
func (observation InstallationRepositoryObservation) GoString() string { return observation.String() }

// SelectedRepositoryMetadata is an immutable, coordinator-proven repository
// snapshot. RepositorySelection records GitHub's explicit selected/all value;
// this package does not infer policy from it.
type SelectedRepositoryMetadata struct {
	installationID      int64
	repositoryID        int64
	fullName            string
	ownerLogin          string
	ownerID             int64
	ownerType           string
	name                string
	private             bool
	archived            bool
	disabled            bool
	defaultBranch       string
	repositorySelection string
	permissions         InstallationDiscoveryPermissions
}

func (metadata SelectedRepositoryMetadata) InstallationID() int64 { return metadata.installationID }
func (metadata SelectedRepositoryMetadata) RepositoryID() int64   { return metadata.repositoryID }
func (metadata SelectedRepositoryMetadata) FullName() string      { return metadata.fullName }
func (metadata SelectedRepositoryMetadata) OwnerLogin() string    { return metadata.ownerLogin }
func (metadata SelectedRepositoryMetadata) OwnerID() int64        { return metadata.ownerID }
func (metadata SelectedRepositoryMetadata) OwnerType() string     { return metadata.ownerType }
func (metadata SelectedRepositoryMetadata) Name() string          { return metadata.name }
func (metadata SelectedRepositoryMetadata) Private() bool         { return metadata.private }
func (metadata SelectedRepositoryMetadata) Archived() bool        { return metadata.archived }
func (metadata SelectedRepositoryMetadata) Disabled() bool        { return metadata.disabled }
func (metadata SelectedRepositoryMetadata) DefaultBranch() string { return metadata.defaultBranch }
func (metadata SelectedRepositoryMetadata) RepositorySelection() string {
	return metadata.repositorySelection
}
func (metadata SelectedRepositoryMetadata) Permissions() InstallationDiscoveryPermissions {
	return metadata.permissions
}
func (SelectedRepositoryMetadata) String() string            { return "selected GitHub repository metadata" }
func (metadata SelectedRepositoryMetadata) GoString() string { return metadata.String() }

type installationAPIResponse struct {
	ID                  *int64  `json:"id"`
	TargetType          *string `json:"target_type"`
	RepositorySelection *string `json:"repository_selection"`
	Account             *struct {
		Login *string `json:"login"`
		ID    *int64  `json:"id"`
		Type  *string `json:"type"`
	} `json:"account"`
}

type installationRepositoriesAPIResponse struct {
	TotalCount   *int                                 `json:"total_count"`
	Repositories *[]installationRepositoryAPIResponse `json:"repositories"`
}

type installationRepositoryAPIResponse struct {
	ID            *int64                     `json:"id"`
	FullName      *string                    `json:"full_name"`
	Name          *string                    `json:"name"`
	Private       *bool                      `json:"private"`
	Archived      *bool                      `json:"archived"`
	Disabled      *bool                      `json:"disabled"`
	DefaultBranch *string                    `json:"default_branch"`
	Permissions   map[string]json.RawMessage `json:"permissions"`
	Owner         *struct {
		Login *string `json:"login"`
		ID    *int64  `json:"id"`
		Type  *string `json:"type"`
	} `json:"owner"`
}

// ListAppInstallations signs once per call and follows at most ten exact,
// same-origin GitHub next-page links.
func (client *InstallationClient) ListAppInstallations(ctx context.Context) ([]InstallationObservation, error) {
	if err := validateDiscoveryContext(ctx); err != nil {
		return nil, err
	}
	if err := client.validate(); err != nil {
		return nil, err
	}
	now, err := client.currentTime()
	if err != nil {
		return nil, err
	}
	credential, err := client.appTokens.AppToken(now)
	if err != nil || !validCompactToken(credential, maxAppTokenBytes) {
		return nil, ErrSigningFailed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	observations := make([]InstallationObservation, 0)
	installationIDs := make(map[int64]struct{})
	accountIDs := make(map[int64]struct{})
	for page := 1; page <= installationMaxPages; page++ {
		payload, link, err := client.getPage(ctx, credential, "/app/installations", page)
		if err != nil {
			return nil, err
		}
		var decoded *[]installationAPIResponse
		if err := decodeGitHubJSON(payload, &decoded); err != nil || decoded == nil || len(*decoded) > installationPageSize {
			return nil, ErrInvalidResponse
		}
		for _, response := range *decoded {
			observation, ok := makeInstallationObservation(response)
			if !ok {
				return nil, ErrInvalidResponse
			}
			if _, duplicate := installationIDs[observation.installationID]; duplicate {
				return nil, &InstallationConflictError{count: 2}
			}
			if _, duplicate := accountIDs[observation.accountID]; duplicate {
				return nil, &InstallationConflictError{count: 2}
			}
			installationIDs[observation.installationID] = struct{}{}
			accountIDs[observation.accountID] = struct{}{}
			observations = append(observations, observation)
			if len(observations) > installationPageSize*installationMaxPages {
				return nil, ErrPaginationRefused
			}
		}
		hasNext, err := client.validateNextLink(link, "/app/installations", page)
		if err != nil {
			return nil, err
		}
		if !hasNext {
			return observations, nil
		}
		if page == installationMaxPages {
			return nil, ErrPaginationRefused
		}
	}
	return nil, ErrPaginationRefused
}

// ListInstallationRepositories obtains one fresh installation-wide discovery
// token and lists only repositories visible to that exact installation.
func (client *InstallationClient) ListInstallationRepositories(ctx context.Context, installationID int64) ([]InstallationRepositoryObservation, error) {
	if err := validateDiscoveryContext(ctx); err != nil {
		return nil, err
	}
	if installationID <= 0 {
		return nil, ErrInvalidInstallationRequest
	}
	if err := client.validate(); err != nil {
		return nil, err
	}
	now, err := client.currentTime()
	if err != nil {
		return nil, err
	}
	token, err := client.discoveryTokens.InstallationDiscoveryToken(ctx, installationID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, ErrRequestFailed
	}
	if token.installationID != installationID || !token.permissions.valid() {
		return nil, firstError(discoveryPermissionError(token.permissions), ErrInvalidDiscoveryToken)
	}
	credential, err := token.Value(now)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	observations := make([]InstallationRepositoryObservation, 0)
	repositoryIDs := make(map[int64]struct{})
	fullNames := make(map[string]struct{})
	totalCount := -1
	for page := 1; page <= installationMaxPages; page++ {
		payload, link, err := client.getPage(ctx, credential, "/installation/repositories", page)
		if err != nil {
			return nil, err
		}
		var decoded installationRepositoriesAPIResponse
		if err := decodeGitHubJSON(payload, &decoded); err != nil || decoded.TotalCount == nil || *decoded.TotalCount < 0 || *decoded.TotalCount > installationPageSize*installationMaxPages || decoded.Repositories == nil || len(*decoded.Repositories) > installationPageSize {
			return nil, ErrInvalidResponse
		}
		if totalCount < 0 {
			totalCount = *decoded.TotalCount
		} else if totalCount != *decoded.TotalCount {
			return nil, ErrInvalidResponse
		}
		for _, response := range *decoded.Repositories {
			observation, ok := makeInstallationRepositoryObservation(response, installationID, token.permissions)
			if !ok {
				return nil, ErrInvalidResponse
			}
			if _, duplicate := repositoryIDs[observation.repositoryID]; duplicate {
				return nil, &InstallationConflictError{count: 2}
			}
			if _, duplicate := fullNames[observation.fullName]; duplicate {
				return nil, &InstallationConflictError{count: 2}
			}
			repositoryIDs[observation.repositoryID] = struct{}{}
			fullNames[observation.fullName] = struct{}{}
			observations = append(observations, observation)
		}
		hasNext, err := client.validateNextLink(link, "/installation/repositories", page)
		if err != nil {
			return nil, err
		}
		if hasNext {
			if page == installationMaxPages || len(observations) >= totalCount {
				return nil, ErrPaginationRefused
			}
			continue
		}
		if len(observations) != totalCount {
			return nil, ErrPaginationRefused
		}
		return observations, nil
	}
	return nil, ErrPaginationRefused
}

// SelectRepository proves the requested installation and repository tuples.
// Both GitHub repository-selection modes are accepted and preserved for the
// coordinator, which remains responsible for applying onboarding policy.
func SelectRepository(installations []InstallationObservation, repositories []InstallationRepositoryObservation, installationID, repositoryID int64, fullName string) (RepositoryIdentity, SelectedRepositoryMetadata, error) {
	owner, name, hasName := "", "", false
	if gitref.ValidateOwnerRepo(fullName) == nil {
		owner, name, _ = strings.Cut(fullName, "/")
		hasName = true
	}
	if installationID <= 0 || repositoryID <= 0 || !hasName {
		return RepositoryIdentity{}, SelectedRepositoryMetadata{}, ErrInvalidInstallationRequest
	}

	var installation InstallationObservation
	installationMatches := 0
	seenInstallationIDs := make(map[int64]struct{}, len(installations))
	seenAccountIDs := make(map[int64]struct{}, len(installations))
	for _, candidate := range installations {
		if !candidate.valid() {
			return selectionFailure()
		}
		if _, duplicate := seenInstallationIDs[candidate.installationID]; duplicate {
			return selectionFailure()
		}
		if _, duplicate := seenAccountIDs[candidate.accountID]; duplicate {
			return selectionFailure()
		}
		seenInstallationIDs[candidate.installationID] = struct{}{}
		seenAccountIDs[candidate.accountID] = struct{}{}
		if candidate.installationID == installationID {
			installation = candidate
			installationMatches++
		}
	}
	if installationMatches != 1 {
		return selectionFailure()
	}

	var repository InstallationRepositoryObservation
	repositoryMatches := 0
	seenRepositoryIDs := make(map[int64]struct{}, len(repositories))
	seenFullNames := make(map[string]struct{}, len(repositories))
	for _, candidate := range repositories {
		if !candidate.valid() || candidate.installationID != installationID {
			return selectionFailure()
		}
		if _, duplicate := seenRepositoryIDs[candidate.repositoryID]; duplicate {
			return selectionFailure()
		}
		if _, duplicate := seenFullNames[candidate.fullName]; duplicate {
			return selectionFailure()
		}
		seenRepositoryIDs[candidate.repositoryID] = struct{}{}
		seenFullNames[candidate.fullName] = struct{}{}
		idMatch := candidate.repositoryID == repositoryID
		nameMatch := candidate.fullName == fullName
		if idMatch != nameMatch {
			return selectionFailure()
		}
		if idMatch {
			repository = candidate
			repositoryMatches++
		}
	}
	if repositoryMatches != 1 || repository.archived || repository.disabled || repository.ownerLogin != owner || repository.name != name || repository.ownerID != installation.accountID || repository.ownerLogin != installation.accountLogin || repository.ownerType != installation.accountType {
		return selectionFailure()
	}
	identity, err := NewRepositoryIdentity(installationID, repositoryID)
	if err != nil {
		return RepositoryIdentity{}, SelectedRepositoryMetadata{}, ErrInvalidInstallationRequest
	}
	return identity, SelectedRepositoryMetadata{
		installationID:      installationID,
		repositoryID:        repositoryID,
		fullName:            repository.fullName,
		ownerLogin:          repository.ownerLogin,
		ownerID:             repository.ownerID,
		ownerType:           repository.ownerType,
		name:                repository.name,
		private:             repository.private,
		archived:            repository.archived,
		disabled:            repository.disabled,
		defaultBranch:       repository.defaultBranch,
		repositorySelection: installation.repositorySelection,
		permissions:         repository.permissions,
	}, nil
}

func (client *InstallationClient) validate() error {
	if client == nil || client.httpClient == nil || client.appTokens == nil || isNilInterface(client.appTokens) || client.discoveryTokens == nil || isNilInterface(client.discoveryTokens) || client.now == nil || !validAPIBase(client.apiBase) {
		return ErrInvalidConfiguration
	}
	return nil
}

func (client *InstallationClient) currentTime() (time.Time, error) {
	now := client.now().UTC()
	if now.IsZero() || now.Unix() <= 0 {
		return time.Time{}, ErrInvalidConfiguration
	}
	return now, nil
}

func validateDiscoveryContext(ctx context.Context) error {
	if ctx == nil {
		return ErrDeadlineRequired
	}
	if _, ok := ctx.Deadline(); !ok {
		return ErrDeadlineRequired
	}
	return ctx.Err()
}

func (client *InstallationClient) getPage(ctx context.Context, credential, route string, page int) ([]byte, string, error) {
	endpoint := client.apiBase + route + "?per_page=100&page=" + strconv.Itoa(page)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", ErrInvalidConfiguration
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "fern-githubapp")
	response, err := client.httpClient.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, "", ctxErr
		}
		return nil, "", ErrRequestFailed
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, "", ErrRequestFailed
	}
	if len(payload) > maxResponseBytes {
		return nil, "", ErrResponseTooLarge
	}
	if response.StatusCode != http.StatusOK {
		return nil, "", &HTTPError{statusCode: response.StatusCode}
	}
	return payload, strings.Join(response.Header.Values("Link"), ","), nil
}

func (client *InstallationClient) validateNextLink(header, route string, page int) (bool, error) {
	if header == "" {
		return false, nil
	}
	next := ""
	for _, entry := range strings.Split(header, ",") {
		entry = strings.TrimSpace(entry)
		if !strings.HasPrefix(entry, "<") {
			return false, ErrPaginationRefused
		}
		end := strings.IndexByte(entry, '>')
		if end < 2 {
			return false, ErrPaginationRefused
		}
		reference := entry[1:end]
		parameters := strings.Split(entry[end+1:], ";")
		relations := ""
		for _, parameter := range parameters {
			parameter = strings.TrimSpace(parameter)
			if parameter == "" {
				continue
			}
			name, value, found := strings.Cut(parameter, "=")
			if !found || !strings.EqualFold(strings.TrimSpace(name), "rel") {
				continue
			}
			value = strings.TrimSpace(value)
			if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
				value = value[1 : len(value)-1]
			}
			relations = value
		}
		isNext := false
		for _, relation := range strings.Fields(relations) {
			if relation == "next" {
				isNext = true
			}
		}
		if !isNext {
			continue
		}
		if next != "" {
			return false, ErrPaginationRefused
		}
		next = reference
	}
	if next == "" {
		return false, nil
	}
	parsed, err := url.Parse(next)
	base, baseErr := url.Parse(client.apiBase)
	if err != nil || baseErr != nil || parsed.Scheme != base.Scheme || parsed.Host != base.Host || parsed.User != nil || parsed.Fragment != "" || parsed.RawPath != "" || parsed.Path != route {
		return false, ErrPaginationRefused
	}
	query := parsed.Query()
	if len(query) != 2 || len(query["per_page"]) != 1 || query.Get("per_page") != "100" || len(query["page"]) != 1 || query.Get("page") != strconv.Itoa(page+1) {
		return false, ErrPaginationRefused
	}
	return true, nil
}

func makeInstallationObservation(response installationAPIResponse) (InstallationObservation, bool) {
	if response.ID == nil || *response.ID <= 0 || response.Account == nil || response.Account.Login == nil || response.Account.ID == nil || *response.Account.ID <= 0 || response.Account.Type == nil || response.TargetType == nil || response.RepositorySelection == nil {
		return InstallationObservation{}, false
	}
	if !validAccountLogin(*response.Account.Login) || !validAccountType(*response.Account.Type) || *response.TargetType != *response.Account.Type || (*response.RepositorySelection != "selected" && *response.RepositorySelection != "all") {
		return InstallationObservation{}, false
	}
	return InstallationObservation{
		installationID:      *response.ID,
		accountLogin:        *response.Account.Login,
		accountID:           *response.Account.ID,
		accountType:         *response.Account.Type,
		targetType:          *response.TargetType,
		repositorySelection: *response.RepositorySelection,
	}, true
}

func makeInstallationRepositoryObservation(response installationRepositoryAPIResponse, installationID int64, permissions InstallationDiscoveryPermissions) (InstallationRepositoryObservation, bool) {
	if response.ID == nil || *response.ID <= 0 || response.FullName == nil || response.Name == nil || response.Owner == nil || response.Owner.Login == nil || response.Owner.ID == nil || *response.Owner.ID <= 0 || response.Owner.Type == nil || response.Private == nil || response.Archived == nil || response.Disabled == nil || response.DefaultBranch == nil || response.Permissions == nil || !permissions.valid() {
		return InstallationRepositoryObservation{}, false
	}
	if gitref.ValidateOwnerRepo(*response.FullName) != nil {
		return InstallationRepositoryObservation{}, false
	}
	owner, name, _ := strings.Cut(*response.FullName, "/")
	if owner != *response.Owner.Login || name != *response.Name || !validAccountType(*response.Owner.Type) || gitref.ValidateRef(*response.DefaultBranch) != nil {
		return InstallationRepositoryObservation{}, false
	}
	if !validRepositoryAPIPermissions(response.Permissions) {
		return InstallationRepositoryObservation{}, false
	}
	return InstallationRepositoryObservation{
		installationID: installationID,
		repositoryID:   *response.ID,
		fullName:       *response.FullName,
		ownerLogin:     *response.Owner.Login,
		ownerID:        *response.Owner.ID,
		ownerType:      *response.Owner.Type,
		name:           *response.Name,
		private:        *response.Private,
		archived:       *response.Archived,
		disabled:       *response.Disabled,
		defaultBranch:  *response.DefaultBranch,
		permissions:    permissions,
		canPull:        true,
		canPush:        true,
	}, true
}

func (observation InstallationObservation) valid() bool {
	return observation.installationID > 0 && observation.accountID > 0 && validAccountLogin(observation.accountLogin) && validAccountType(observation.accountType) && observation.targetType == observation.accountType && (observation.repositorySelection == "selected" || observation.repositorySelection == "all")
}

func (observation InstallationRepositoryObservation) valid() bool {
	if gitref.ValidateOwnerRepo(observation.fullName) != nil {
		return false
	}
	owner, name, _ := strings.Cut(observation.fullName, "/")
	return observation.installationID > 0 && observation.repositoryID > 0 && observation.ownerID > 0 && owner == observation.ownerLogin && name == observation.name && validAccountType(observation.ownerType) && gitref.ValidateRef(observation.defaultBranch) == nil && observation.permissions.valid() && observation.canPull && observation.canPush
}

func validAccountLogin(login string) bool {
	composite := login + "/repository"
	if gitref.ValidateOwnerRepo(composite) != nil {
		return false
	}
	owner, _, _ := strings.Cut(composite, "/")
	return owner == login
}

func validAccountType(accountType string) bool {
	return accountType == "Organization" || accountType == "User"
}

func validPermissionName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for index := range len(name) {
		char := name[index]
		if char >= 'a' && char <= 'z' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func validRepositoryAPIPermissions(values map[string]json.RawMessage) bool {
	if len(values) == 0 {
		return false
	}
	boolValues := make(map[string]bool, len(values))
	levelValues := make(map[string]string, len(values))
	kind := byte(0)
	for name, raw := range values {
		if !validPermissionName(name) {
			return false
		}
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "" {
			return false
		}
		switch trimmed[0] {
		case 't', 'f':
			if kind == 's' {
				return false
			}
			kind = 'b'
			var allowed bool
			if json.Unmarshal(raw, &allowed) != nil {
				return false
			}
			boolValues[name] = allowed
		case '"':
			if kind == 'b' {
				return false
			}
			kind = 's'
			var level string
			if json.Unmarshal(raw, &level) != nil {
				return false
			}
			levelValues[name] = level
		default:
			return false
		}
	}
	if kind == 'b' {
		return boolValues["pull"] && boolValues["push"]
	}
	_, err := ValidateInstallationDiscoveryPermissions(levelValues)
	return err == nil
}

func discoveryPermissionError(permissions InstallationDiscoveryPermissions) error {
	if !permissions.valid() {
		return ErrInsufficientPermissions
	}
	return nil
}

func selectionFailure() (RepositoryIdentity, SelectedRepositoryMetadata, error) {
	return RepositoryIdentity{}, SelectedRepositoryMetadata{}, &RepositorySelectionError{}
}
