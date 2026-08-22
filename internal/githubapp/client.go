package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	githubAPIBase       = "https://api.github.com"
	maxResponseBytes    = 64 << 10
	maxAppTokenBytes    = 8 << 10
	maxAccessTokenBytes = 512
	minAccessTokenBytes = 20
	minimumTokenLife    = 30 * time.Second
	maximumTokenLife    = 65 * time.Minute
)

// InstallationTokenSource is the narrow credential contract needed by a
// future host-side Git transport or publication coordinator.
type InstallationTokenSource interface {
	InstallationToken(context.Context, RepositoryIdentity) (InstallationToken, error)
}

// Client mints repository-scoped installation tokens. It has no package-level
// state and is safe for concurrent use when its dependencies are safe.
type Client struct {
	httpClient *http.Client
	appTokens  AppTokenSource
	apiBase    string
	now        func() time.Time
}

func NewClient(httpClient *http.Client, appTokens AppTokenSource) (*Client, error) {
	if httpClient == nil || appTokens == nil {
		return nil, ErrInvalidConfiguration
	}
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		httpClient: &clientCopy,
		appTokens:  appTokens,
		apiBase:    githubAPIBase,
		now:        time.Now,
	}, nil
}

// InstallationToken is an opaque, immutable host credential. Value refuses to
// return credentials that are expired or too close to expiry.
type InstallationToken struct {
	value       string
	expiresAt   time.Time
	identity    RepositoryIdentity
	permissions RepositoryPermissions
}

func (InstallationToken) String() string         { return "GitHub installation token" }
func (token InstallationToken) GoString() string { return token.String() }

func (token InstallationToken) Value(now time.Time) (string, error) {
	if token.value == "" || now.IsZero() || !now.Add(minimumTokenLife).Before(token.expiresAt) {
		return "", ErrTokenExpired
	}
	return token.value, nil
}

func (token InstallationToken) ExpiresAt() time.Time {
	return token.expiresAt
}

func (token InstallationToken) Identity() RepositoryIdentity {
	return token.identity
}

func (token InstallationToken) Permissions() RepositoryPermissions {
	return token.permissions
}

func (client *Client) InstallationToken(ctx context.Context, identity RepositoryIdentity) (InstallationToken, error) {
	if err := identity.validate(); err != nil {
		return InstallationToken{}, err
	}
	body, err := json.Marshal(struct {
		RepositoryIDs []int64           `json:"repository_ids"`
		Permissions   map[string]string `json:"permissions"`
	}{
		RepositoryIDs: []int64{identity.repositoryID},
		Permissions: map[string]string{
			"contents":      "write",
			"pull_requests": "write",
		},
	})
	if err != nil {
		return InstallationToken{}, ErrInvalidConfiguration
	}
	minted, err := client.mintInstallationAccessToken(ctx, identity.installationID, body)
	if err != nil {
		return InstallationToken{}, err
	}
	permissions, err := ValidateRepositoryPermissions(minted.permissions)
	if err != nil {
		return InstallationToken{}, err
	}
	return InstallationToken{
		value:       minted.value,
		expiresAt:   minted.expiresAt,
		identity:    identity,
		permissions: permissions,
	}, nil
}

// InstallationDiscoveryToken mints a distinct installation-wide credential
// used only by InstallationClient to enumerate repositories for operator
// selection. It is never accepted by repository publication APIs.
func (client *Client) InstallationDiscoveryToken(ctx context.Context, installationID int64) (InstallationDiscoveryToken, error) {
	if installationID <= 0 {
		return InstallationDiscoveryToken{}, ErrInvalidInstallationRequest
	}
	body, err := json.Marshal(struct {
		Permissions map[string]string `json:"permissions"`
	}{Permissions: map[string]string{"contents": "write", "pull_requests": "write"}})
	if err != nil {
		return InstallationDiscoveryToken{}, ErrInvalidConfiguration
	}
	minted, err := client.mintInstallationAccessToken(ctx, installationID, body)
	if err != nil {
		return InstallationDiscoveryToken{}, err
	}
	permissions, err := ValidateInstallationDiscoveryPermissions(minted.permissions)
	if err != nil {
		return InstallationDiscoveryToken{}, err
	}
	return InstallationDiscoveryToken{
		value: minted.value, expiresAt: minted.expiresAt,
		installationID: installationID, permissions: permissions,
	}, nil
}

type mintedInstallationAccessToken struct {
	value       string
	expiresAt   time.Time
	permissions map[string]string
}

func (client *Client) mintInstallationAccessToken(ctx context.Context, installationID int64, body []byte) (mintedInstallationAccessToken, error) {
	if client == nil || client.httpClient == nil || client.appTokens == nil || client.now == nil || client.apiBase == "" || installationID <= 0 || len(body) == 0 {
		return mintedInstallationAccessToken{}, ErrInvalidConfiguration
	}
	if _, ok := ctx.Deadline(); !ok {
		return mintedInstallationAccessToken{}, ErrDeadlineRequired
	}
	if err := ctx.Err(); err != nil {
		return mintedInstallationAccessToken{}, err
	}
	now := client.now().UTC()
	if now.IsZero() || now.Unix() <= 0 {
		return mintedInstallationAccessToken{}, ErrInvalidConfiguration
	}
	appToken, err := client.appTokens.AppToken(now)
	if err != nil || !validCompactToken(appToken, maxAppTokenBytes) {
		return mintedInstallationAccessToken{}, ErrSigningFailed
	}
	endpoint := strings.TrimRight(client.apiBase, "/") + "/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return mintedInstallationAccessToken{}, ErrInvalidConfiguration
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+appToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "fern-githubapp")
	response, err := client.httpClient.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return mintedInstallationAccessToken{}, ctxErr
		}
		return mintedInstallationAccessToken{}, ErrRequestFailed
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return mintedInstallationAccessToken{}, ErrRequestFailed
	}
	if len(payload) > maxResponseBytes {
		return mintedInstallationAccessToken{}, ErrResponseTooLarge
	}
	if response.StatusCode != http.StatusCreated {
		return mintedInstallationAccessToken{}, &HTTPError{statusCode: response.StatusCode}
	}
	var decoded struct {
		Token       string            `json:"token"`
		ExpiresAt   string            `json:"expires_at"`
		Permissions map[string]string `json:"permissions"`
	}
	if json.Unmarshal(payload, &decoded) != nil || !validAccessToken(decoded.Token) {
		return mintedInstallationAccessToken{}, ErrInvalidResponse
	}
	now = client.now().UTC()
	expiresAt, err := time.Parse(time.RFC3339, decoded.ExpiresAt)
	if err != nil || !now.Add(minimumTokenLife).Before(expiresAt) || expiresAt.After(now.Add(maximumTokenLife)) {
		return mintedInstallationAccessToken{}, ErrInvalidResponse
	}
	permissions := make(map[string]string, len(decoded.Permissions))
	for name, value := range decoded.Permissions {
		permissions[name] = value
	}
	return mintedInstallationAccessToken{value: decoded.Token, expiresAt: expiresAt.UTC(), permissions: permissions}, nil
}

func validCompactToken(value string, limit int) bool {
	if value == "" || len(value) > limit || strings.Count(value, ".") != 2 {
		return false
	}
	for _, segment := range strings.Split(value, ".") {
		if segment == "" {
			return false
		}
		for _, char := range segment {
			if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
				continue
			}
			return false
		}
	}
	return true
}

func validAccessToken(value string) bool {
	if len(value) < minAccessTokenBytes || len(value) > maxAccessTokenBytes {
		return false
	}
	for _, char := range []byte(value) {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}
