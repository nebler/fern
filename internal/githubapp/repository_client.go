package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/nebler/fern/internal/gitref"
	"github.com/nebler/fern/internal/jsoncanon"
)

const maxJSONDepth = 64

var (
	ErrInvalidRepositoryRequest = errors.New("invalid GitHub repository request")
	ErrInvalidInstallationToken = errors.New("invalid GitHub installation token")
	ErrPaginationRefused        = errors.New("GitHub response requires unsupported pagination")
)

// RepositoryClient reads and validates the configured GitHub repository identity.
// It is safe for concurrent use when its dependencies are safe for concurrent use.
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
	return "GitHub repository identity client"
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

func (observation RepositoryObservation) Identity() RepositoryIdentity { return observation.identity }
func (observation RepositoryObservation) RepositoryID() int64 {
	return observation.identity.RepositoryID()
}
func (observation RepositoryObservation) FullName() string      { return observation.fullName }
func (observation RepositoryObservation) Owner() string         { return observation.owner }
func (observation RepositoryObservation) Name() string          { return observation.name }
func (observation RepositoryObservation) DefaultBranch() string { return observation.defaultBranch }

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
	if err := decodeGitHubJSON(payload, &decoded); err != nil || !validRepositoryResponse(decoded, identity.RepositoryID(), configuredFullName, owner, name) || gitref.ValidateRef(deref(decoded.DefaultBranch)) != nil {
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

type repositoryAPIResponse struct {
	ID            *int64  `json:"id"`
	FullName      *string `json:"full_name"`
	Name          *string `json:"name"`
	DefaultBranch *string `json:"default_branch"`
	Owner         *struct {
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
	if gitref.ValidateOwnerRepo(target) != nil {
		return "", "", ErrInvalidRepositoryRequest
	}
	owner, name, _ := strings.Cut(target, "/")
	return owner, name, nil
}

func validRepositoryResponse(response repositoryAPIResponse, repositoryID int64, fullName, owner, name string) bool {
	return response.ID != nil && *response.ID == repositoryID && response.FullName != nil && *response.FullName == fullName && response.Name != nil && *response.Name == name && response.Owner != nil && response.Owner.Login != nil && *response.Owner.Login == owner && response.DefaultBranch != nil
}

func validAPIBase(base string) bool {
	parsed, err := url.Parse(base)
	return err == nil && parsed.IsAbs() && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.RawPath == "" && parsed.Path == ""
}

func decodeGitHubJSON(payload []byte, destination any) error {
	if err := jsoncanon.Check(payload, maxJSONDepth); err != nil {
		return ErrInvalidResponse
	}
	if err := json.Unmarshal(payload, destination); err != nil {
		return ErrInvalidResponse
	}
	return nil
}

func deref[T comparable](pointer *T) T {
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

// isNilInterface reports whether value is a nil interface or a typed nil
// pointer, map, slice, channel, or function.
func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
