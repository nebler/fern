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

const (
	installationTestAppToken   = "header.payload.signature"
	installationTestCredential = "github_installation_discovery_token_12345"
)

type installationAppSource struct {
	mu    sync.Mutex
	token string
	err   error
	calls int
}

func (source *installationAppSource) AppToken(time.Time) (string, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.calls++
	return source.token, source.err
}

func (source *installationAppSource) callCount() int {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.calls
}

type installationDiscoverySource struct {
	mu    sync.Mutex
	token InstallationDiscoveryToken
	err   error
	calls int
	ids   []int64
}

func (source *installationDiscoverySource) InstallationDiscoveryToken(_ context.Context, installationID int64) (InstallationDiscoveryToken, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.calls++
	source.ids = append(source.ids, installationID)
	return source.token, source.err
}

func (source *installationDiscoverySource) snapshot() (int, []int64) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.calls, append([]int64(nil), source.ids...)
}

type installationRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip installationRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestInstallationClientListsInstallationsWithExactPagination(t *testing.T) {
	t.Parallel()
	now := installationTestNow()
	app := &installationAppSource{token: installationTestAppToken}
	discovery := installationTestDiscoverySource(t, now, 101)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertInstallationGET(t, request, "/app/installations", "per_page=100&page="+request.URL.Query().Get("page"), installationTestAppToken)
		switch request.URL.Query().Get("page") {
		case "1":
			writer.Header().Set("Link", "<"+server.URL+"/app/installations?per_page=100&page=2>; rel=\"next\", <"+server.URL+"/app/installations?per_page=100&page=2>; rel=\"last\"")
			_, _ = io.WriteString(writer, `[`+installationJSON(101, 1001, "fern-inc", "Organization", "selected")+`]`)
		case "2":
			writer.Header().Set("Link", "<"+server.URL+"/app/installations?per_page=100&page=1>; rel=\"prev\"")
			_, _ = io.WriteString(writer, `[`+installationJSON(102, 1002, "fern-user", "User", "all")+`]`)
		default:
			t.Fatalf("unexpected page %q", request.URL.Query().Get("page"))
		}
	}))
	defer server.Close()
	client := newInstallationTestClient(t, server.Client(), server.URL, app, discovery, now)

	observations, err := client.ListAppInstallations(installationTestContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 || observations[0].InstallationID() != 101 || observations[0].AccountLogin() != "fern-inc" || observations[0].AccountID() != 1001 || observations[0].AccountType() != "Organization" || observations[0].TargetType() != "Organization" || observations[0].RepositorySelection() != "selected" || observations[1].RepositorySelection() != "all" {
		t.Fatalf("observations = %#v", observations)
	}
	if app.callCount() != 1 {
		t.Fatalf("app token calls = %d", app.callCount())
	}
	if calls, _ := discovery.snapshot(); calls != 0 {
		t.Fatalf("discovery token calls = %d", calls)
	}
}

func TestInstallationClientListsRepositoriesWithInstallationToken(t *testing.T) {
	t.Parallel()
	now := installationTestNow()
	app := &installationAppSource{token: installationTestAppToken}
	discovery := installationTestDiscoverySource(t, now, 101)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertInstallationGET(t, request, "/installation/repositories", "per_page=100&page="+request.URL.Query().Get("page"), installationTestCredential)
		if request.URL.Query().Get("page") == "1" {
			writer.Header().Set("Link", "<"+server.URL+"/installation/repositories?per_page=100&page=2>; rel=\"next\"")
			_, _ = io.WriteString(writer, `{"total_count":2,"repositories":[`+repositoryInstallationJSON(201, 1001, "fern-inc/widget", false, false)+`]}`)
			return
		}
		fineGrained := strings.Replace(repositoryInstallationJSON(202, 1001, "fern-inc/private-widget", true, false), `"permissions":{"pull":true,"push":true}`, `"permissions":{"metadata":"read","contents":"write","pull_requests":"write"}`, 1)
		_, _ = io.WriteString(writer, `{"total_count":2,"repositories":[`+fineGrained+`]}`)
	}))
	defer server.Close()
	client := newInstallationTestClient(t, server.Client(), server.URL, app, discovery, now)

	repositories, err := client.ListInstallationRepositories(installationTestContext(t), 101)
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 2 || repositories[0].InstallationID() != 101 || repositories[0].RepositoryID() != 201 || repositories[0].FullName() != "fern-inc/widget" || repositories[0].OwnerLogin() != "fern-inc" || repositories[0].OwnerID() != 1001 || repositories[0].OwnerType() != "Organization" || repositories[0].Name() != "widget" || repositories[0].Private() || repositories[0].Archived() || repositories[0].Disabled() || repositories[0].DefaultBranch() != "main" || !repositories[0].CanPull() || !repositories[0].CanPush() {
		t.Fatalf("repositories = %#v", repositories)
	}
	permissions := repositories[0].Permissions()
	if permissions.Metadata() != "read" || permissions.Contents() != "write" || permissions.PullRequests() != "write" {
		t.Fatalf("permissions = %#v", permissions)
	}
	if calls, ids := discovery.snapshot(); calls != 1 || len(ids) != 1 || ids[0] != 101 {
		t.Fatalf("token calls = %d, ids = %v", calls, ids)
	}
	if app.callCount() != 0 {
		t.Fatalf("app token calls = %d", app.callCount())
	}
}

func TestInstallationClientRejectsAppJWTFailure(t *testing.T) {
	t.Parallel()
	secret := "signing-secret-must-not-escape"
	app := &installationAppSource{err: errors.New(secret)}
	discovery := installationTestDiscoverySource(t, installationTestNow(), 101)
	var network atomic.Int32
	client := newInstallationTestClient(t, &http.Client{Transport: installationRoundTripper(func(*http.Request) (*http.Response, error) {
		network.Add(1)
		return nil, errors.New("network must not run")
	})}, "https://api.github.test", app, discovery, installationTestNow())
	_, err := client.ListAppInstallations(installationTestContext(t))
	if !errors.Is(err, ErrSigningFailed) || network.Load() != 0 || strings.Contains(fmt.Sprintf("%v %#v", err, err), secret) {
		t.Fatalf("error = %v, network = %d", err, network.Load())
	}
}

func TestInstallationClientValidatesInputsBeforeCredentialsOrNetwork(t *testing.T) {
	t.Parallel()
	now := installationTestNow()
	app := &installationAppSource{token: installationTestAppToken}
	discovery := installationTestDiscoverySource(t, now, 101)
	var network atomic.Int32
	client := newInstallationTestClient(t, &http.Client{Transport: installationRoundTripper(func(*http.Request) (*http.Response, error) {
		network.Add(1)
		return nil, errors.New("network must not run")
	})}, "https://api.github.test", app, discovery, now)

	if _, err := client.ListAppInstallations(context.Background()); !errors.Is(err, ErrDeadlineRequired) {
		t.Fatalf("missing deadline error = %v", err)
	}
	if _, err := client.ListInstallationRepositories(installationTestContext(t), 0); !errors.Is(err, ErrInvalidInstallationRequest) {
		t.Fatalf("invalid ID error = %v", err)
	}
	canceled, cancel := context.WithTimeout(context.Background(), time.Minute)
	cancel()
	if _, err := client.ListInstallationRepositories(canceled, 101); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
	if app.callCount() != 0 || network.Load() != 0 {
		t.Fatalf("app calls = %d, network = %d", app.callCount(), network.Load())
	}
	if calls, _ := discovery.snapshot(); calls != 0 {
		t.Fatalf("discovery calls = %d", calls)
	}
}

func TestInstallationClientRejectsDiscoveryTokenIdentityExpiryAndPermissions(t *testing.T) {
	t.Parallel()
	now := installationTestNow()
	validPermissions := InstallationDiscoveryPermissions{metadata: "read", contents: "write", pullRequests: "write"}
	tests := []struct {
		name  string
		token InstallationDiscoveryToken
		err   error
		want  error
	}{
		{name: "source", err: errors.New("source-secret"), want: ErrRequestFailed},
		{name: "identity", token: InstallationDiscoveryToken{value: installationTestCredential, expiresAt: now.Add(time.Hour), installationID: 999, permissions: validPermissions}, want: ErrInvalidDiscoveryToken},
		{name: "expiry", token: InstallationDiscoveryToken{value: installationTestCredential, expiresAt: now.Add(30 * time.Second), installationID: 101, permissions: validPermissions}, want: ErrTokenExpired},
		{name: "implausible expiry", token: InstallationDiscoveryToken{value: installationTestCredential, expiresAt: now.Add(66 * time.Minute), installationID: 101, permissions: validPermissions}, want: ErrInvalidDiscoveryToken},
		{name: "permissions", token: InstallationDiscoveryToken{value: installationTestCredential, expiresAt: now.Add(time.Hour), installationID: 101}, want: ErrInsufficientPermissions},
		{name: "value", token: InstallationDiscoveryToken{value: "unsafe\ncredential_that_is_long_enough", expiresAt: now.Add(time.Hour), installationID: 101, permissions: validPermissions}, want: ErrInvalidDiscoveryToken},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := &installationDiscoverySource{token: test.token, err: test.err}
			var network atomic.Int32
			client := newInstallationTestClient(t, &http.Client{Transport: installationRoundTripper(func(*http.Request) (*http.Response, error) {
				network.Add(1)
				return nil, nil
			})}, "https://api.github.test", &installationAppSource{token: installationTestAppToken}, source, now)
			_, err := client.ListInstallationRepositories(installationTestContext(t), 101)
			if !errors.Is(err, test.want) || network.Load() != 0 || strings.Contains(fmt.Sprintf("%v %#v", err, err), "secret") || strings.Contains(fmt.Sprintf("%v %#v", err, err), installationTestCredential) {
				t.Fatalf("error = %v, network = %d", err, network.Load())
			}
		})
	}
}

func TestInstallationClientRejectsMalformedDuplicateOversizedAndUnsafePagination(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body []byte
		link func(string) string
		want error
	}{
		{name: "malformed", body: []byte(`[{"id":`)},
		{name: "null", body: []byte(`null`)},
		{name: "duplicate", body: []byte(`[{"id":101,"ID":101}]`)},
		{name: "invalid UTF8", body: []byte{'[', '"', 0xff, '"', ']'}},
		{name: "trailing", body: []byte(`[] {}`)},
		{name: "missing required", body: []byte(`[{"id":101,"account":null,"target_type":"Organization","repository_selection":"selected"}]`)},
		{name: "unsafe value", body: []byte(`[` + installationJSON(101, 1001, "fern-inc", "Bot", "selected") + `]`)},
		{name: "oversized", body: []byte(strings.Repeat("x", maxResponseBytes+1)), want: ErrResponseTooLarge},
		{name: "cross origin next", body: []byte(`[]`), link: func(string) string {
			return `<https://attacker.invalid/app/installations?per_page=100&page=2>; rel="next"`
		}, want: ErrPaginationRefused},
		{name: "skipped page", body: []byte(`[]`), link: func(base string) string { return `<` + base + `/app/installations?per_page=100&page=3>; rel="next"` }, want: ErrPaginationRefused},
		{name: "extra query", body: []byte(`[]`), link: func(base string) string {
			return `<` + base + `/app/installations?per_page=100&page=2&secret=x>; rel="next"`
		}, want: ErrPaginationRefused},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if test.link != nil {
					writer.Header().Set("Link", test.link(server.URL))
				}
				_, _ = writer.Write(test.body)
			}))
			defer server.Close()
			client := newInstallationTestClient(t, server.Client(), server.URL, &installationAppSource{token: installationTestAppToken}, installationTestDiscoverySource(t, installationTestNow(), 101), installationTestNow())
			_, err := client.ListAppInstallations(installationTestContext(t))
			want := test.want
			if want == nil {
				want = ErrInvalidResponse
			}
			if !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
		})
	}
}

func TestInstallationClientRejectsStatusesRedirectsAndRedacts(t *testing.T) {
	t.Parallel()
	secret := "remote-body-and-url-secret"
	tests := []struct {
		name   string
		status int
		want   error
	}{
		{name: "status", status: http.StatusForbidden, want: ErrRequestFailed},
		{name: "redirect", status: http.StatusTemporaryRedirect, want: ErrRequestFailed},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var destinationHit atomic.Bool
			destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destinationHit.Store(true) }))
			defer destination.Close()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if test.status == http.StatusTemporaryRedirect {
					writer.Header().Set("Location", destination.URL+"/"+secret)
				}
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, secret)
			}))
			defer server.Close()
			client := newInstallationTestClient(t, server.Client(), server.URL, &installationAppSource{token: installationTestAppToken}, installationTestDiscoverySource(t, installationTestNow(), 101), installationTestNow())
			_, err := client.ListAppInstallations(installationTestContext(t))
			var httpErr *HTTPError
			if !errors.Is(err, test.want) || !errors.As(err, &httpErr) || httpErr.StatusCode() != test.status || destinationHit.Load() || strings.Contains(fmt.Sprintf("%v %#v", err, err), secret) || strings.Contains(fmt.Sprintf("%v %#v", err, err), installationTestAppToken) {
				t.Fatalf("error = %v, destination hit = %t", err, destinationHit.Load())
			}
		})
	}
}

func TestInstallationClientRepositoryResponseValidation(t *testing.T) {
	t.Parallel()
	valid := repositoryInstallationJSON(201, 1001, "fern-inc/widget", false, false)
	tests := []string{
		strings.Replace(valid, `"id":201`, `"id":0`, 1),
		strings.Replace(valid, `"full_name":"fern-inc/widget"`, `"full_name":"other/widget"`, 1),
		strings.Replace(valid, `"name":"widget"`, `"name":"other"`, 1),
		strings.Replace(valid, `"login":"fern-inc"`, `"login":"other"`, 1),
		strings.Replace(valid, `"default_branch":"main"`, `"default_branch":"bad..ref"`, 1),
		strings.Replace(valid, `"push":true`, `"push":false`, 1),
		strings.Replace(valid, `"permissions":{"pull":true,"push":true}`, `"permissions":null`, 1),
		strings.Replace(valid, `"private":false`, `"private":null`, 1),
		strings.Replace(valid, `"id":201`, `"id":201,"ID":201`, 1),
	}
	for index, repository := range tests {
		repository := repository
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(writer, `{"total_count":1,"repositories":[`+repository+`]}`)
			}))
			defer server.Close()
			client := newInstallationTestClient(t, server.Client(), server.URL, &installationAppSource{token: installationTestAppToken}, installationTestDiscoverySource(t, installationTestNow(), 101), installationTestNow())
			if _, err := client.ListInstallationRepositories(installationTestContext(t), 101); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSelectRepositoryProvesExactTupleForSelectedAndAll(t *testing.T) {
	t.Parallel()
	permissions := InstallationDiscoveryPermissions{metadata: "read", contents: "write", pullRequests: "write"}
	for _, selection := range []string{"selected", "all"} {
		installation := InstallationObservation{installationID: 101, accountLogin: "fern-inc", accountID: 1001, accountType: "Organization", targetType: "Organization", repositorySelection: selection}
		repository := InstallationRepositoryObservation{installationID: 101, repositoryID: 201, fullName: "fern-inc/widget", ownerLogin: "fern-inc", ownerID: 1001, ownerType: "Organization", name: "widget", private: true, defaultBranch: "main", permissions: permissions, canPull: true, canPush: true}
		identity, metadata, err := SelectRepository([]InstallationObservation{installation}, []InstallationRepositoryObservation{repository}, 101, 201, "fern-inc/widget")
		if err != nil {
			t.Fatal(err)
		}
		if identity.InstallationID() != 101 || identity.RepositoryID() != 201 || metadata.InstallationID() != 101 || metadata.RepositoryID() != 201 || metadata.FullName() != "fern-inc/widget" || metadata.OwnerLogin() != "fern-inc" || metadata.OwnerID() != 1001 || metadata.OwnerType() != "Organization" || metadata.Name() != "widget" || !metadata.Private() || metadata.DefaultBranch() != "main" || metadata.RepositorySelection() != selection || metadata.Permissions() != permissions {
			t.Fatalf("identity = %#v, metadata = %#v", identity, metadata)
		}
	}
}

func TestSelectRepositoryRejectsMismatchArchivedDisabledPermissionsAndAmbiguity(t *testing.T) {
	t.Parallel()
	permissions := InstallationDiscoveryPermissions{metadata: "read", contents: "write", pullRequests: "write"}
	installation := InstallationObservation{installationID: 101, accountLogin: "fern-inc", accountID: 1001, accountType: "Organization", targetType: "Organization", repositorySelection: "selected"}
	repository := InstallationRepositoryObservation{installationID: 101, repositoryID: 201, fullName: "fern-inc/widget", ownerLogin: "fern-inc", ownerID: 1001, ownerType: "Organization", name: "widget", defaultBranch: "main", permissions: permissions, canPull: true, canPush: true}
	tests := []struct {
		name          string
		installations []InstallationObservation
		repositories  []InstallationRepositoryObservation
		id            int64
		fullName      string
		want          error
	}{
		{name: "missing installation", repositories: []InstallationRepositoryObservation{repository}, id: 201, fullName: "fern-inc/widget", want: ErrRepositorySelection},
		{name: "duplicate installation", installations: []InstallationObservation{installation, installation}, repositories: []InstallationRepositoryObservation{repository}, id: 201, fullName: "fern-inc/widget", want: ErrRepositorySelection},
		{name: "duplicate repository", installations: []InstallationObservation{installation}, repositories: []InstallationRepositoryObservation{repository, repository}, id: 201, fullName: "fern-inc/widget", want: ErrRepositorySelection},
		{name: "ID mismatch", installations: []InstallationObservation{installation}, repositories: []InstallationRepositoryObservation{repository}, id: 202, fullName: "fern-inc/widget", want: ErrRepositorySelection},
		{name: "name mismatch", installations: []InstallationObservation{installation}, repositories: []InstallationRepositoryObservation{repository}, id: 201, fullName: "fern-inc/other", want: ErrRepositorySelection},
		{name: "noncanonical", installations: []InstallationObservation{installation}, repositories: []InstallationRepositoryObservation{repository}, id: 201, fullName: "https://github.com/fern-inc/widget", want: ErrInvalidInstallationRequest},
		{name: "archived", installations: []InstallationObservation{installation}, repositories: []InstallationRepositoryObservation{mutateInstallationRepository(repository, func(value *InstallationRepositoryObservation) { value.archived = true })}, id: 201, fullName: "fern-inc/widget", want: ErrRepositorySelection},
		{name: "disabled", installations: []InstallationObservation{installation}, repositories: []InstallationRepositoryObservation{mutateInstallationRepository(repository, func(value *InstallationRepositoryObservation) { value.disabled = true })}, id: 201, fullName: "fern-inc/widget", want: ErrRepositorySelection},
		{name: "permissions", installations: []InstallationObservation{installation}, repositories: []InstallationRepositoryObservation{mutateInstallationRepository(repository, func(value *InstallationRepositoryObservation) { value.permissions = InstallationDiscoveryPermissions{} })}, id: 201, fullName: "fern-inc/widget", want: ErrRepositorySelection},
		{name: "owner tuple", installations: []InstallationObservation{installation}, repositories: []InstallationRepositoryObservation{mutateInstallationRepository(repository, func(value *InstallationRepositoryObservation) { value.ownerID = 2002 })}, id: 201, fullName: "fern-inc/widget", want: ErrRepositorySelection},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := SelectRepository(test.installations, test.repositories, 101, test.id, test.fullName)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if strings.Contains(fmt.Sprintf("%v %#v", err, err), "fern-inc") {
				t.Fatalf("selection error exposed candidate: %v", err)
			}
		})
	}
}

func TestInstallationTypesRedactStringAndGoString(t *testing.T) {
	t.Parallel()
	secret := "remote-secret-account"
	permissions := InstallationDiscoveryPermissions{metadata: "read", contents: "write", pullRequests: "write"}
	values := []any{
		&InstallationClient{},
		InstallationDiscoveryToken{value: secret, installationID: 1},
		InstallationObservation{accountLogin: secret},
		InstallationRepositoryObservation{fullName: secret, ownerLogin: secret},
		SelectedRepositoryMetadata{fullName: secret, ownerLogin: secret},
		&InstallationConflictError{count: 2},
		&RepositorySelectionError{},
	}
	for _, value := range values {
		formatted := fmt.Sprintf("%s %#v", value, value)
		if strings.Contains(formatted, secret) {
			t.Fatalf("formatting exposed secret for %T: %s", value, formatted)
		}
	}
	token, err := NewInstallationDiscoveryToken(installationTestCredential, installationTestNow().Add(time.Hour), 101, map[string]string{"metadata": "read", "contents": "write", "pull_requests": "write"})
	if err != nil {
		t.Fatal(err)
	}
	if token.InstallationID() != 101 || token.ExpiresAt() != installationTestNow().Add(time.Hour) || token.Permissions() != permissions {
		t.Fatalf("token metadata = %#v", token)
	}
}

func TestInstallationClientConcurrentCallsUseFreshCredentials(t *testing.T) {
	t.Parallel()
	now := installationTestNow()
	app := &installationAppSource{token: installationTestAppToken}
	discovery := installationTestDiscoverySource(t, now, 101)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/app/installations" {
			_, _ = io.WriteString(writer, `[]`)
			return
		}
		_, _ = io.WriteString(writer, `{"total_count":0,"repositories":[]}`)
	}))
	defer server.Close()
	client := newInstallationTestClient(t, server.Client(), server.URL, app, discovery, now)

	const count = 24
	var group sync.WaitGroup
	group.Add(count * 2)
	for range count {
		go func() {
			defer group.Done()
			if _, err := client.ListAppInstallations(installationTestContext(t)); err != nil {
				t.Errorf("ListAppInstallations: %v", err)
			}
		}()
		go func() {
			defer group.Done()
			if _, err := client.ListInstallationRepositories(installationTestContext(t), 101); err != nil {
				t.Errorf("ListInstallationRepositories: %v", err)
			}
		}()
	}
	group.Wait()
	if app.callCount() != count {
		t.Fatalf("app token calls = %d", app.callCount())
	}
	if calls, _ := discovery.snapshot(); calls != count {
		t.Fatalf("discovery token calls = %d", calls)
	}
}

func TestNewInstallationClientIsStrictAndCopiesHTTPClient(t *testing.T) {
	t.Parallel()
	now := installationTestNow()
	app := &installationAppSource{token: installationTestAppToken}
	discovery := installationTestDiscoverySource(t, now, 101)
	for _, arguments := range []struct {
		httpClient *http.Client
		app        AppTokenSource
		discovery  InstallationDiscoveryTokenSource
		now        func() time.Time
	}{
		{nil, app, discovery, func() time.Time { return now }},
		{http.DefaultClient, nil, discovery, func() time.Time { return now }},
		{http.DefaultClient, app, nil, func() time.Time { return now }},
		{http.DefaultClient, app, discovery, nil},
	} {
		if _, err := NewInstallationClient(arguments.httpClient, arguments.app, arguments.discovery, arguments.now); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("error = %v", err)
		}
	}
	originalRedirect := func(*http.Request, []*http.Request) error { return nil }
	httpClient := &http.Client{CheckRedirect: originalRedirect}
	client, err := NewInstallationClient(httpClient, app, discovery, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if client.apiBase != "https://api.github.com" || client.httpClient == httpClient || client.httpClient.CheckRedirect == nil || httpClient.CheckRedirect == nil {
		t.Fatalf("client = %#v", client)
	}
}

func installationTestNow() time.Time {
	return time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
}

func installationTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func installationTestDiscoverySource(t *testing.T, now time.Time, installationID int64) *installationDiscoverySource {
	t.Helper()
	token, err := NewInstallationDiscoveryToken(installationTestCredential, now.Add(time.Hour), installationID, map[string]string{"metadata": "read", "contents": "write", "pull_requests": "write"})
	if err != nil {
		t.Fatal(err)
	}
	return &installationDiscoverySource{token: token}
}

func newInstallationTestClient(t *testing.T, httpClient *http.Client, apiBase string, app AppTokenSource, discovery InstallationDiscoveryTokenSource, now time.Time) *InstallationClient {
	t.Helper()
	client, err := NewInstallationClient(httpClient, app, discovery, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	client.apiBase = apiBase
	return client
}

func assertInstallationGET(t *testing.T, request *http.Request, path, rawQuery, credential string) {
	t.Helper()
	if request.Method != http.MethodGet || request.URL.Path != path || request.URL.RawQuery != rawQuery {
		t.Errorf("request = %s %s?%s", request.Method, request.URL.Path, request.URL.RawQuery)
	}
	if request.Header.Get("Authorization") != "Bearer "+credential || request.Header.Get("Accept") != "application/vnd.github+json" || request.Header.Get("X-GitHub-Api-Version") != "2022-11-28" || request.Header.Get("User-Agent") != "fern-githubapp" {
		t.Errorf("headers = %#v", request.Header)
	}
	if request.Header.Get("Content-Type") != "" || request.ContentLength > 0 {
		t.Errorf("GET unexpectedly has a body or Content-Type")
	}
}

func installationJSON(installationID, accountID int64, login, accountType, selection string) string {
	return fmt.Sprintf(`{"id":%d,"account":{"login":%q,"id":%d,"type":%q},"target_type":%q,"repository_selection":%q}`, installationID, login, accountID, accountType, accountType, selection)
}

func repositoryInstallationJSON(repositoryID, ownerID int64, fullName string, private, archived bool) string {
	owner, name, _ := strings.Cut(fullName, "/")
	return fmt.Sprintf(`{"id":%d,"full_name":%q,"name":%q,"owner":{"login":%q,"id":%d,"type":"Organization"},"private":%t,"archived":%t,"disabled":false,"default_branch":"main","permissions":{"pull":true,"push":true}}`, repositoryID, fullName, name, owner, ownerID, private, archived)
}

func mutateInstallationRepository(value InstallationRepositoryObservation, mutate func(*InstallationRepositoryObservation)) InstallationRepositoryObservation {
	mutate(&value)
	return value
}

var _ AppTokenSource = (*installationAppSource)(nil)
var _ InstallationDiscoveryTokenSource = (*installationDiscoverySource)(nil)
