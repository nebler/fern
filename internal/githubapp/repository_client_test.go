package githubapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const repositoryTestCredential = "github_installation_token_repository_12345"

type repositoryTokenSource struct {
	mu    sync.Mutex
	token InstallationToken
	err   error
	calls int
}

func (source *repositoryTokenSource) InstallationToken(context.Context, RepositoryIdentity) (InstallationToken, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.calls++
	return source.token, source.err
}

func (source *repositoryTokenSource) callCount() int {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.calls
}

func TestRepositoryClientRepositoryByIDWireContract(t *testing.T) {
	t.Parallel()
	now, identity, source := repositoryTestAuth(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertRepositoryRequest(t, request, http.MethodGet, "/repositories/202", "", false)
		_, _ = io.WriteString(writer, `{"id":202,"full_name":"fern-inc/widget","name":"widget","owner":{"login":"fern-inc","type":"Organization"},"default_branch":"main","private":true}`)
	}))
	defer server.Close()
	client := newRepositoryTestClient(t, server.Client(), server.URL, source, now)

	observation, err := client.RepositoryByID(repositoryTestContext(t), identity, "fern-inc/widget")
	if err != nil {
		t.Fatal(err)
	}
	if observation.Identity() != identity || observation.RepositoryID() != 202 || observation.FullName() != "fern-inc/widget" || observation.Owner() != "fern-inc" || observation.Name() != "widget" || observation.DefaultBranch() != "main" {
		t.Fatalf("observation = %#v", observation)
	}
	if source.callCount() != 1 {
		t.Fatalf("token calls = %d", source.callCount())
	}
}

func TestRepositoryClientRepositoryByIDRejectsUnprovenResponses(t *testing.T) {
	t.Parallel()
	tests := []string{
		`{"id":203,"full_name":"fern-inc/widget","name":"widget","owner":{"login":"fern-inc"},"default_branch":"main"}`,
		`{"id":202,"full_name":"other/widget","name":"widget","owner":{"login":"other"},"default_branch":"main"}`,
		`{"id":202,"full_name":"fern-inc/widget","name":"other","owner":{"login":"fern-inc"},"default_branch":"main"}`,
		`{"id":202,"full_name":"fern-inc/widget","name":"widget","owner":{"login":"other"},"default_branch":"main"}`,
		`{"id":202,"full_name":"fern-inc/widget","name":"widget","owner":{"login":"fern-inc"},"default_branch":null}`,
		`{"id":202,"id":202,"full_name":"fern-inc/widget","name":"widget","owner":{"login":"fern-inc"},"default_branch":"main"}`,
		`{"id":202,"full_name":"fern-inc/widget","name":"widget","owner":{"login":"fern-inc"},"default_branch":"bad..ref"}`,
		`null`,
	}
	for index, response := range tests {
		response := response
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			t.Parallel()
			now, identity, source := repositoryTestAuth(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(writer, response)
			}))
			defer server.Close()
			client := newRepositoryTestClient(t, server.Client(), server.URL, source, now)
			if _, err := client.RepositoryByID(repositoryTestContext(t), identity, "fern-inc/widget"); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRepositoryClientValidatesCallerInputsBeforeTokenOrNetwork(t *testing.T) {
	t.Parallel()
	now, identity, _ := repositoryTestAuth(t)
	tests := []struct {
		name string
		call func(*RepositoryClient) error
		want error
	}{
		{name: "missing deadline", call: func(client *RepositoryClient) error {
			_, err := client.RepositoryByID(context.Background(), identity, "fern-inc/widget")
			return err
		}, want: ErrDeadlineRequired},
		{name: "invalid identity", call: func(client *RepositoryClient) error {
			_, err := client.RepositoryByID(repositoryTestContext(t), RepositoryIdentity{}, "fern-inc/widget")
			return err
		}, want: ErrInvalidIdentity},
		{name: "repository URL", call: func(client *RepositoryClient) error {
			_, err := client.RepositoryByID(repositoryTestContext(t), identity, "https://github.com/fern-inc/widget")
			return err
		}, want: ErrInvalidRepositoryRequest},
		{name: "repository dot git", call: func(client *RepositoryClient) error {
			_, err := client.RepositoryByID(repositoryTestContext(t), identity, "fern-inc/widget.git")
			return err
		}, want: ErrInvalidRepositoryRequest},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := &repositoryTokenSource{err: errors.New("must not be called")}
			var network atomic.Int32
			transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
				network.Add(1)
				return nil, errors.New("must not be called")
			})
			client := newRepositoryTestClient(t, &http.Client{Transport: transport}, "https://api.github.test", source, now)
			if err := test.call(client); !errors.Is(err, test.want) || source.callCount() != 0 || network.Load() != 0 {
				t.Fatalf("error = %v, token calls = %d, network = %d", err, source.callCount(), network.Load())
			}
		})
	}

	canceled, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute))
	cancel()
	source := &repositoryTokenSource{}
	client := newRepositoryTestClient(t, http.DefaultClient, "https://api.github.test", source, now)
	if _, err := client.RepositoryByID(canceled, identity, "fern-inc/widget"); !errors.Is(err, context.Canceled) || source.callCount() != 0 {
		t.Fatalf("canceled error = %v, calls = %d", err, source.callCount())
	}
}

func TestRepositoryClientRevalidatesEveryInstallationToken(t *testing.T) {
	t.Parallel()
	now, identity, _ := repositoryTestAuth(t)
	otherIdentity, _ := NewRepositoryIdentity(101, 999)
	permissions, _ := ValidateRepositoryPermissions(map[string]string{"contents": "write", "pull_requests": "write"})
	tests := []struct {
		name  string
		token InstallationToken
		err   error
		want  error
	}{
		{name: "source", err: errors.New("source-secret"), want: ErrRequestFailed},
		{name: "identity", token: InstallationToken{value: repositoryTestCredential, expiresAt: now.Add(time.Hour), identity: otherIdentity, permissions: permissions}, want: ErrInvalidInstallationToken},
		{name: "permissions", token: InstallationToken{value: repositoryTestCredential, expiresAt: now.Add(time.Hour), identity: identity}, want: ErrInsufficientPermissions},
		{name: "expired", token: InstallationToken{value: repositoryTestCredential, expiresAt: now.Add(30 * time.Second), identity: identity, permissions: permissions}, want: ErrTokenExpired},
		{name: "unsafe value", token: InstallationToken{value: "unsafe\ncredential_that_is_long_enough", expiresAt: now.Add(time.Hour), identity: identity, permissions: permissions}, want: ErrInvalidInstallationToken},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := &repositoryTokenSource{token: test.token, err: test.err}
			var requested atomic.Bool
			transport := roundTripperFunc(func(*http.Request) (*http.Response, error) { requested.Store(true); return nil, nil })
			client := newRepositoryTestClient(t, &http.Client{Transport: transport}, "https://api.github.test", source, now)
			_, err := client.RepositoryByID(repositoryTestContext(t), identity, "fern-inc/widget")
			if !errors.Is(err, test.want) || source.callCount() != 1 || requested.Load() || strings.Contains(fmt.Sprintf("%v %#v", err, err), "secret") || strings.Contains(fmt.Sprintf("%v %#v", err, err), repositoryTestCredential) {
				t.Fatalf("error = %v, calls = %d, requested = %t", err, source.callCount(), requested.Load())
			}
		})
	}
}

func TestRepositoryClientBoundsStatusesRedirectsAndRedacts(t *testing.T) {
	t.Parallel()
	secret := "remote-message-secret"
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{name: "HTTP status", status: http.StatusForbidden, body: `{"message":"` + secret + `"}`, want: ErrRequestFailed},
		{name: "oversized success", status: http.StatusOK, body: strings.Repeat("x", maxResponseBytes+1), want: ErrResponseTooLarge},
		{name: "oversized error", status: http.StatusBadRequest, body: strings.Repeat("x", maxResponseBytes+1), want: ErrResponseTooLarge},
		{name: "malformed", status: http.StatusOK, body: `{"id":`, want: ErrInvalidResponse},
		{name: "trailing JSON", status: http.StatusOK, body: `{}` + `{}`, want: ErrInvalidResponse},
		{name: "redirect", status: http.StatusTemporaryRedirect, body: secret, want: ErrRequestFailed},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now, identity, source := repositoryTestAuth(t)
			var destinationHit atomic.Bool
			destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destinationHit.Store(true) }))
			defer destination.Close()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if test.status == http.StatusTemporaryRedirect {
					writer.Header().Set("Location", destination.URL)
				}
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()
			client := newRepositoryTestClient(t, server.Client(), server.URL, source, now)
			_, err := client.RepositoryByID(repositoryTestContext(t), identity, "fern-inc/widget")
			if !errors.Is(err, test.want) || destinationHit.Load() || strings.Contains(fmt.Sprintf("%v %#v", err, err), secret) || strings.Contains(fmt.Sprintf("%v %#v", err, err), repositoryTestCredential) {
				t.Fatalf("error = %v, destination hit = %t", err, destinationHit.Load())
			}
			if test.status == http.StatusForbidden || test.status == http.StatusTemporaryRedirect {
				var httpErr *HTTPError
				if !errors.As(err, &httpErr) || httpErr.StatusCode() != test.status {
					t.Fatalf("HTTP error = %#v", err)
				}
			}
		})
	}
}

func TestRepositoryClientFreshTokenAndConcurrentUse(t *testing.T) {
	t.Parallel()
	now, identity, source := repositoryTestAuth(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"id":202,"full_name":"fern-inc/widget","name":"widget","owner":{"login":"fern-inc"},"default_branch":"main"}`)
	}))
	defer server.Close()
	client := newRepositoryTestClient(t, server.Client(), server.URL, source, now)

	const count = 32
	var group sync.WaitGroup
	group.Add(count)
	for range count {
		go func() {
			defer group.Done()
			if _, err := client.RepositoryByID(repositoryTestContext(t), identity, "fern-inc/widget"); err != nil {
				t.Errorf("RepositoryByID: %v", err)
			}
		}()
	}
	group.Wait()
	if source.callCount() != count {
		t.Fatalf("token calls = %d", source.callCount())
	}
}

func TestNewRepositoryClientIsStrictAndCopiesHTTPClient(t *testing.T) {
	t.Parallel()
	now, _, source := repositoryTestAuth(t)
	for _, arguments := range []struct {
		httpClient *http.Client
		source     InstallationTokenSource
		now        func() time.Time
	}{{nil, source, func() time.Time { return now }}, {http.DefaultClient, nil, func() time.Time { return now }}, {http.DefaultClient, source, nil}} {
		if _, err := NewRepositoryClient(arguments.httpClient, arguments.source, arguments.now); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("error = %v", err)
		}
	}
	originalRedirect := func(*http.Request, []*http.Request) error { return nil }
	httpClient := &http.Client{CheckRedirect: originalRedirect}
	client, err := NewRepositoryClient(httpClient, source, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if client.apiBase != "https://api.github.com" || client.httpClient == httpClient || client.httpClient.CheckRedirect == nil || httpClient.CheckRedirect == nil {
		t.Fatalf("client = %#v", client)
	}
	if strings.Contains(fmt.Sprintf("%s %#v", client, client), repositoryTestCredential) {
		t.Fatal("client formatting exposed a credential")
	}
}

func repositoryTestAuth(t *testing.T) (time.Time, RepositoryIdentity, *repositoryTokenSource) {
	t.Helper()
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	identity, err := NewRepositoryIdentity(101, 202)
	if err != nil {
		t.Fatal(err)
	}
	permissions, err := ValidateRepositoryPermissions(map[string]string{"contents": "write", "pull_requests": "write"})
	if err != nil {
		t.Fatal(err)
	}
	return now, identity, &repositoryTokenSource{token: InstallationToken{
		value: repositoryTestCredential, expiresAt: now.Add(time.Hour), identity: identity, permissions: permissions,
	}}
}

func newRepositoryTestClient(t *testing.T, httpClient *http.Client, apiBase string, source InstallationTokenSource, now time.Time) *RepositoryClient {
	t.Helper()
	client, err := NewRepositoryClient(httpClient, source, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	client.apiBase = apiBase
	return client
}

func repositoryTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func assertRepositoryRequest(t *testing.T, request *http.Request, method, path, rawQuery string, post bool) {
	t.Helper()
	if request.Method != method || request.URL.Path != path || request.URL.RawQuery != rawQuery {
		t.Errorf("request = %s %s?%s", request.Method, request.URL.Path, request.URL.RawQuery)
	}
	if request.Header.Get("Authorization") != "Bearer "+repositoryTestCredential || request.Header.Get("Accept") != "application/vnd.github+json" || request.Header.Get("X-GitHub-Api-Version") != "2022-11-28" || request.Header.Get("User-Agent") != "fern-githubapp" {
		t.Errorf("headers = %#v", request.Header)
	}
	if post && request.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", request.Header.Get("Content-Type"))
	}
	if !post && request.Header.Get("Content-Type") != "" {
		t.Errorf("unexpected Content-Type = %q", request.Header.Get("Content-Type"))
	}
}

var _ InstallationTokenSource = (*repositoryTokenSource)(nil)
