package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAppToken = "header.payload.signature"

type staticAppTokenSource struct {
	token string
	err   error
}

func (source staticAppTokenSource) AppToken(time.Time) (string, error) {
	return source.token, source.err
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestClientRequestsRepositoryScopedInstallationToken(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	accessToken := "github_installation_token_12345"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/app/installations/101/access_tokens" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer "+testAppToken {
			t.Errorf("Authorization = %q", got)
		}
		if request.Header.Get("Accept") != "application/vnd.github+json" || request.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Errorf("GitHub headers = %#v", request.Header)
		}
		var body struct {
			RepositoryIDs []int64           `json:"repository_ids"`
			Permissions   map[string]string `json:"permissions"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 202 {
			t.Errorf("repository IDs = %v", body.RepositoryIDs)
		}
		if len(body.Permissions) != 2 || body.Permissions["contents"] != "write" || body.Permissions["pull_requests"] != "write" {
			t.Errorf("permissions = %v", body.Permissions)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		fmt.Fprintf(writer, `{"token":%q,"expires_at":%q,"permissions":{"contents":"write","pull_requests":"write","metadata":"read"}}`, accessToken, now.Add(time.Hour).Format(time.RFC3339))
	}))
	defer server.Close()

	client := newTestClient(t, server, now, staticAppTokenSource{token: testAppToken})
	identity, err := NewRepositoryIdentity(101, 202)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	token, err := client.InstallationToken(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	value, err := token.Value(now)
	if err != nil {
		t.Fatal(err)
	}
	if value != accessToken || token.ExpiresAt() != now.Add(time.Hour) || token.Identity() != identity {
		t.Fatalf("unexpected installation token metadata")
	}
	if token.Permissions().Contents() != "write" || token.Permissions().PullRequests() != "write" {
		t.Fatalf("token permissions = %+v", token.Permissions())
	}
}

func TestInstallationTokenFormattingIsRedacted(t *testing.T) {
	t.Parallel()
	secret := "github_installation_secret_12345"
	token := InstallationToken{value: secret, expiresAt: time.Now().Add(time.Hour)}
	formatted := fmt.Sprintf("%v %#v", token, token)
	if strings.Contains(formatted, secret) {
		t.Fatalf("formatted token exposed credential: %s", formatted)
	}
}

func TestClientRequestsInstallationWideDiscoveryToken(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	accessToken := "github_discovery_token_123456"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/app/installations/101/access_tokens" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if _, exists := body["repository_ids"]; exists || len(body) != 1 {
			t.Errorf("discovery body = %v", body)
		}
		var permissions map[string]string
		if err := json.Unmarshal(body["permissions"], &permissions); err != nil || permissions["contents"] != "write" || permissions["pull_requests"] != "write" {
			t.Errorf("permissions = %v, %v", permissions, err)
		}
		writer.WriteHeader(http.StatusCreated)
		fmt.Fprintf(writer, `{"token":%q,"expires_at":%q,"permissions":{"contents":"write","pull_requests":"write","metadata":"read"}}`, accessToken, now.Add(time.Hour).Format(time.RFC3339))
	}))
	defer server.Close()
	client := newTestClient(t, server, now, staticAppTokenSource{token: testAppToken})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	token, err := client.InstallationDiscoveryToken(ctx, 101)
	if err != nil {
		t.Fatal(err)
	}
	value, err := token.Value(now)
	if err != nil {
		t.Fatal(err)
	}
	if value != accessToken || token.InstallationID() != 101 || token.ExpiresAt() != now.Add(time.Hour) || token.Permissions().Metadata() != "read" {
		t.Fatalf("discovery token metadata = %v", token)
	}
}

func TestClientRejectsMissingDeadlineBeforeRequest(t *testing.T) {
	t.Parallel()
	var requested bool
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requested = true
	}))
	defer server.Close()
	client := newTestClient(t, server, time.Now(), staticAppTokenSource{token: testAppToken})
	identity, _ := NewRepositoryIdentity(1, 2)
	_, err := client.InstallationToken(context.Background(), identity)
	if !errors.Is(err, ErrDeadlineRequired) || requested {
		t.Fatalf("error = %v, requested = %t", err, requested)
	}
}

func TestClientRejectsInvalidIdentityBeforeSigning(t *testing.T) {
	t.Parallel()
	secret := "source-secret-must-not-escape"
	client, err := NewClient(http.DefaultClient, staticAppTokenSource{err: errors.New(secret)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err = client.InstallationToken(ctx, RepositoryIdentity{})
	if !errors.Is(err, ErrInvalidIdentity) || strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %v", err)
	}
}

func TestClientChecksPermissionsAndExpiry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		expiresAt   time.Time
		permissions string
		want        error
	}{
		{name: "missing contents write", expiresAt: now.Add(time.Hour), permissions: `{"contents":"read","pull_requests":"write"}`, want: ErrInsufficientPermissions},
		{name: "missing pulls write", expiresAt: now.Add(time.Hour), permissions: `{"contents":"write","pull_requests":"read"}`, want: ErrInsufficientPermissions},
		{name: "already expired", expiresAt: now.Add(-time.Second), permissions: `{"contents":"write","pull_requests":"write"}`, want: ErrInvalidResponse},
		{name: "insufficient lifetime", expiresAt: now.Add(30 * time.Second), permissions: `{"contents":"write","pull_requests":"write"}`, want: ErrInvalidResponse},
		{name: "implausibly long lifetime", expiresAt: now.Add(66 * time.Minute), permissions: `{"contents":"write","pull_requests":"write"}`, want: ErrInvalidResponse},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusCreated)
				fmt.Fprintf(writer, `{"token":"github_installation_token_12345","expires_at":%q,"permissions":%s}`, test.expiresAt.Format(time.RFC3339), test.permissions)
			}))
			defer server.Close()
			client := newTestClient(t, server, now, staticAppTokenSource{token: testAppToken})
			identity, _ := NewRepositoryIdentity(1, 2)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, err := client.InstallationToken(ctx, identity)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestInstallationTokenRefusesNearExpiry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	token := InstallationToken{value: "github_installation_token_12345", expiresAt: now.Add(time.Minute)}
	if _, err := token.Value(now); err != nil {
		t.Fatalf("fresh token: %v", err)
	}
	if value, err := token.Value(now.Add(30 * time.Second)); value != "" || !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("near-expiry value = %q, error = %v", value, err)
	}
	if value, err := (InstallationToken{}).Value(now); value != "" || !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("zero value = %q, error = %v", value, err)
	}
}

func TestClientBoundsResponsesAndRedactsErrors(t *testing.T) {
	t.Parallel()
	remoteSecret := "remote-secret-must-not-escape"
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{name: "HTTP error", status: http.StatusForbidden, body: `{"message":"` + remoteSecret + `"}`, want: ErrRequestFailed},
		{name: "oversized error", status: http.StatusBadRequest, body: strings.Repeat(remoteSecret, maxResponseBytes), want: ErrResponseTooLarge},
		{name: "malformed success", status: http.StatusCreated, body: `{"token":"` + remoteSecret + `"}`, want: ErrInvalidResponse},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()
			client := newTestClient(t, server, time.Now().UTC(), staticAppTokenSource{token: testAppToken})
			identity, _ := NewRepositoryIdentity(1, 2)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, err := client.InstallationToken(ctx, identity)
			if !errors.Is(err, test.want) || strings.Contains(err.Error(), remoteSecret) || strings.Contains(err.Error(), testAppToken) {
				t.Fatalf("error = %v", err)
			}
			if test.status == http.StatusForbidden {
				var httpErr *HTTPError
				if !errors.As(err, &httpErr) || httpErr.StatusCode() != http.StatusForbidden {
					t.Fatalf("HTTP error = %v", err)
				}
			}
		})
	}
}

func TestClientDoesNotForwardAppAuthorizationThroughRedirects(t *testing.T) {
	t.Parallel()
	var redirected bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected = true
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", destination.URL)
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client := newTestClient(t, origin, time.Now().UTC(), staticAppTokenSource{token: testAppToken})
	identity, _ := NewRepositoryIdentity(1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := client.InstallationToken(ctx, identity)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode() != http.StatusTemporaryRedirect || redirected {
		t.Fatalf("error = %v, redirected = %t", err, redirected)
	}
}

func TestClientRedactsCredentialBearingDependencyErrors(t *testing.T) {
	t.Parallel()
	secret := "dependency-secret-must-not-escape"
	identity, _ := NewRepositoryIdentity(1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	client, err := NewClient(http.DefaultClient, staticAppTokenSource{err: errors.New(secret)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.InstallationToken(ctx, identity); !errors.Is(err, ErrSigningFailed) || strings.Contains(err.Error(), secret) {
		t.Fatalf("signing error = %v", err)
	}

	transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New(secret)
	})
	client, err = NewClient(&http.Client{Transport: transport}, staticAppTokenSource{token: testAppToken})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.InstallationToken(ctx, identity); !errors.Is(err, ErrRequestFailed) || strings.Contains(err.Error(), secret) {
		t.Fatalf("transport error = %v", err)
	}
}

func TestClientSupportsConcurrentTokenRequests(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusCreated)
		fmt.Fprintf(writer, `{"token":"github_installation_token_12345","expires_at":%q,"permissions":{"contents":"write","pull_requests":"write"}}`, now.Add(time.Hour).Format(time.RFC3339))
	}))
	defer server.Close()
	client := newTestClient(t, server, now, staticAppTokenSource{token: testAppToken})
	identity, _ := NewRepositoryIdentity(1, 2)

	const requestCount = 20
	var group sync.WaitGroup
	group.Add(requestCount)
	for range requestCount {
		go func() {
			defer group.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if _, err := client.InstallationToken(ctx, identity); err != nil {
				t.Errorf("InstallationToken: %v", err)
			}
		}()
	}
	group.Wait()
}

func newTestClient(t *testing.T, server *httptest.Server, now time.Time, source AppTokenSource) *Client {
	t.Helper()
	client, err := NewClient(server.Client(), source)
	if err != nil {
		t.Fatal(err)
	}
	client.apiBase = server.URL
	client.now = func() time.Time { return now }
	return client
}

var _ AppTokenSource = staticAppTokenSource{}
var _ InstallationTokenSource = (*Client)(nil)
var _ InstallationDiscoveryTokenSource = (*Client)(nil)
