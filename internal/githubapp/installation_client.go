package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nebler/fern/internal/domain"
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

// InstallationDiscoveryTokenSource supplies an installation-wide credential.
// It is deliberately separate from repository-scoped publication credentials.
type InstallationDiscoveryTokenSource interface {
	InstallationDiscoveryToken(context.Context, int64) (InstallationDiscoveryToken, error)
}

// validateDiscoveryPermissions requires the minimum permissions needed to
// discover and later publish to a repository, and nothing unexpected.
func validateDiscoveryPermissions(values map[string]string) error {
	for name, level := range values {
		if !validPermissionName(name) || (level != "read" && level != "write") {
			return ErrInsufficientPermissions
		}
	}
	if values["metadata"] != "read" || values["contents"] != "write" || values["pull_requests"] != "write" {
		return ErrInsufficientPermissions
	}
	return nil
}

// InstallationDiscoveryToken is an opaque installation-wide credential.
type InstallationDiscoveryToken struct {
	value          string
	expiresAt      time.Time
	installationID int64
}

func (token InstallationDiscoveryToken) Value(now time.Time) (string, error) {
	if !validAccessToken(token.value) || token.installationID <= 0 || now.IsZero() || token.expiresAt.After(now.Add(maximumTokenLife)) {
		return "", ErrInvalidDiscoveryToken
	}
	if !now.Add(minimumTokenLife).Before(token.expiresAt) {
		return "", ErrTokenExpired
	}
	return token.value, nil
}

func (token InstallationDiscoveryToken) ExpiresAt() time.Time  { return token.expiresAt }
func (token InstallationDiscoveryToken) InstallationID() int64 { return token.installationID }
func (InstallationDiscoveryToken) String() string              { return "GitHub installation discovery token" }
func (token InstallationDiscoveryToken) GoString() string      { return token.String() }

// InstallationClient discovers installations and repositories without storing
// credentials or applying selection policy.
type InstallationClient struct {
	httpClient      *http.Client
	appTokens       AppTokenSource
	discoveryTokens InstallationDiscoveryTokenSource
	apiBase         string
	now             func() time.Time
}

func NewInstallationClient(httpClient *http.Client, appTokens AppTokenSource, discoveryTokens InstallationDiscoveryTokenSource, now func() time.Time) (*InstallationClient, error) {
	if httpClient == nil || appTokens == nil || discoveryTokens == nil || now == nil {
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

// InstallationObservation is an immutable installation/account tuple.
type InstallationObservation struct {
	installationID      int64
	accountLogin        string
	accountID           int64
	accountType         string
	targetType          string
	repositorySelection string
}

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
}

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

// ListAppInstallations signs once per call and requests numbered pages until
// one is short, refusing more than ten. Server-supplied URLs are never followed.
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
		payload, err := client.getPage(ctx, credential, "/app/installations", page)
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
				return nil, ErrInstallationConflict
			}
			if _, duplicate := accountIDs[observation.accountID]; duplicate {
				return nil, ErrInstallationConflict
			}
			installationIDs[observation.installationID] = struct{}{}
			accountIDs[observation.accountID] = struct{}{}
			observations = append(observations, observation)
		}
		if len(*decoded) < installationPageSize {
			return observations, nil
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
	if token.installationID != installationID {
		return nil, ErrInvalidDiscoveryToken
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
		payload, err := client.getPage(ctx, credential, "/installation/repositories", page)
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
			observation, ok := makeInstallationRepositoryObservation(response, installationID)
			if !ok {
				return nil, ErrInvalidResponse
			}
			if _, duplicate := repositoryIDs[observation.repositoryID]; duplicate {
				return nil, ErrInstallationConflict
			}
			if _, duplicate := fullNames[observation.fullName]; duplicate {
				return nil, ErrInstallationConflict
			}
			repositoryIDs[observation.repositoryID] = struct{}{}
			fullNames[observation.fullName] = struct{}{}
			observations = append(observations, observation)
		}
		switch {
		case len(observations) == totalCount:
			return observations, nil
		case len(observations) > totalCount || len(*decoded.Repositories) < installationPageSize:
			// GitHub's own count disagrees with the pages it returned.
			return nil, ErrPaginationRefused
		}
	}
	return nil, ErrPaginationRefused
}

// SelectRepository proves the requested installation and repository tuples.
// Both GitHub repository-selection modes are accepted. The observations come
// from the List methods, which already validated and de-duplicated them.
func SelectRepository(installations []InstallationObservation, repositories []InstallationRepositoryObservation, installationID, repositoryID int64, fullName string) error {
	owner, name, hasName := "", "", false
	if domain.ValidateOwnerRepo(fullName) == nil {
		owner, name, _ = strings.Cut(fullName, "/")
		hasName = true
	}
	if installationID <= 0 || repositoryID <= 0 || !hasName {
		return ErrInvalidInstallationRequest
	}

	var installation InstallationObservation
	installationMatches := 0
	for _, candidate := range installations {
		if candidate.installationID == installationID {
			installation = candidate
			installationMatches++
		}
	}
	if installationMatches != 1 {
		return ErrRepositorySelection
	}

	var repository InstallationRepositoryObservation
	repositoryMatches := 0
	for _, candidate := range repositories {
		if candidate.installationID != installationID {
			return ErrRepositorySelection
		}
		idMatch := candidate.repositoryID == repositoryID
		nameMatch := candidate.fullName == fullName
		if idMatch != nameMatch {
			return ErrRepositorySelection
		}
		if idMatch {
			repository = candidate
			repositoryMatches++
		}
	}
	if repositoryMatches != 1 || repository.archived || repository.disabled || repository.ownerLogin != owner || repository.name != name || repository.ownerID != installation.accountID || repository.ownerLogin != installation.accountLogin || repository.ownerType != installation.accountType {
		return ErrRepositorySelection
	}
	return nil
}

func (client *InstallationClient) validate() error {
	if client == nil || client.httpClient == nil || client.appTokens == nil || client.discoveryTokens == nil || client.now == nil || !validAPIBase(client.apiBase) {
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

func (client *InstallationClient) getPage(ctx context.Context, credential, route string, page int) ([]byte, error) {
	endpoint := client.apiBase + route + "?per_page=100&page=" + strconv.Itoa(page)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, ErrInvalidConfiguration
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "fern-githubapp")
	response, err := client.httpClient.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, ErrRequestFailed
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxListPageBytes+1))
	if err != nil {
		return nil, ErrRequestFailed
	}
	if len(payload) > maxListPageBytes {
		return nil, ErrResponseTooLarge
	}
	if response.StatusCode != http.StatusOK {
		return nil, &HTTPError{statusCode: response.StatusCode}
	}
	return payload, nil
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

func makeInstallationRepositoryObservation(response installationRepositoryAPIResponse, installationID int64) (InstallationRepositoryObservation, bool) {
	if response.ID == nil || *response.ID <= 0 || response.FullName == nil || response.Name == nil || response.Owner == nil || response.Owner.Login == nil || response.Owner.ID == nil || *response.Owner.ID <= 0 || response.Owner.Type == nil || response.Private == nil || response.Archived == nil || response.Disabled == nil || response.DefaultBranch == nil || response.Permissions == nil {
		return InstallationRepositoryObservation{}, false
	}
	if domain.ValidateOwnerRepo(*response.FullName) != nil {
		return InstallationRepositoryObservation{}, false
	}
	owner, name, _ := strings.Cut(*response.FullName, "/")
	if owner != *response.Owner.Login || name != *response.Name || !validAccountType(*response.Owner.Type) || domain.ValidateRef(*response.DefaultBranch) != nil {
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
	}, true
}

func validAccountLogin(login string) bool {
	composite := login + "/repository"
	if domain.ValidateOwnerRepo(composite) != nil {
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
	return validateDiscoveryPermissions(levelValues) == nil
}
