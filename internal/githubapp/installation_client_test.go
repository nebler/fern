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

func TestInstallationClientListsInstallationsUntilAShortPage(t *testing.T) {
	t.Parallel()
	now := installationTestNow()
	app := &installationAppSource{token: installationTestAppToken}
	discovery := installationTestDiscoverySource(t, now, 101)
	fullPage := []string{installationJSON(101, 1001, "fern-inc", "Organization", "selected")}
	for index := 1; index < installationPageSize; index++ {
		fullPage = append(fullPage, installationJSON(int64(5000+index), int64(9000+index), fmt.Sprintf("org-%d", index), "Organization", "all"))
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertInstallationGET(t, request, "/app/installations", "per_page=100&page="+request.URL.Query().Get("page"), installationTestAppToken)
		// A server-supplied next link is never followed.
		writer.Header().Set("Link", `<https://attacker.invalid/app/installations?page=2>; rel="next"`)
		switch request.URL.Query().Get("page") {
		case "1":
			_, _ = io.WriteString(writer, `[`+strings.Join(fullPage, ",")+`]`)
		case "2":
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
	last := observations[len(observations)-1]
	if len(observations) != installationPageSize+1 || observations[0].installationID != 101 || observations[0].accountLogin != "fern-inc" || observations[0].accountID != 1001 || observations[0].accountType != "Organization" || observations[0].targetType != "Organization" || observations[0].repositorySelection != "selected" || last.installationID != 102 || last.repositorySelection != "all" {
		t.Fatalf("observations = %d, first = %#v, last = %#v", len(observations), observations[0], last)
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
	fullPage := []string{repositoryInstallationJSON(201, 1001, "fern-inc/widget", false, false)}
	for index := 1; index < installationPageSize; index++ {
		fullPage = append(fullPage, repositoryInstallationJSON(int64(3000+index), 1001, fmt.Sprintf("fern-inc/repo-%d", index), false, false))
	}
	total := strconv.Itoa(installationPageSize + 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertInstallationGET(t, request, "/installation/repositories", "per_page=100&page="+request.URL.Query().Get("page"), installationTestCredential)
		if request.URL.Query().Get("page") == "1" {
			_, _ = io.WriteString(writer, `{"total_count":`+total+`,"repositories":[`+strings.Join(fullPage, ",")+`]}`)
			return
		}
		fineGrained := strings.Replace(repositoryInstallationJSON(202, 1001, "fern-inc/private-widget", true, false), `"permissions":{"pull":true,"push":true}`, `"permissions":{"metadata":"read","contents":"write","pull_requests":"write"}`, 1)
		_, _ = io.WriteString(writer, `{"total_count":`+total+`,"repositories":[`+fineGrained+`]}`)
	}))
	defer server.Close()
	client := newInstallationTestClient(t, server.Client(), server.URL, app, discovery, now)

	repositories, err := client.ListInstallationRepositories(installationTestContext(t), 101)
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != installationPageSize+1 || repositories[0].installationID != 101 || repositories[0].repositoryID != 201 || repositories[0].fullName != "fern-inc/widget" || repositories[0].ownerLogin != "fern-inc" || repositories[0].ownerID != 1001 || repositories[0].ownerType != "Organization" || repositories[0].name != "widget" || repositories[0].private || repositories[0].archived || repositories[0].disabled || repositories[0].defaultBranch != "main" || !repositories[installationPageSize].private {
		t.Fatalf("repositories = %d, first = %#v", len(repositories), repositories[0])
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

func TestInstallationClientRejectsDiscoveryTokenIdentityAndExpiry(t *testing.T) {
	t.Parallel()
	now := installationTestNow()
	tests := []struct {
		name  string
		token InstallationDiscoveryToken
		err   error
		want  error
	}{
		{name: "source", err: errors.New("source-secret"), want: ErrRequestFailed},
		{name: "identity", token: InstallationDiscoveryToken{value: installationTestCredential, expiresAt: now.Add(time.Hour), installationID: 999}, want: ErrInvalidDiscoveryToken},
		{name: "expiry", token: InstallationDiscoveryToken{value: installationTestCredential, expiresAt: now.Add(30 * time.Second), installationID: 101}, want: ErrTokenExpired},
		{name: "implausible expiry", token: InstallationDiscoveryToken{value: installationTestCredential, expiresAt: now.Add(66 * time.Minute), installationID: 101}, want: ErrInvalidDiscoveryToken},
		{name: "value", token: InstallationDiscoveryToken{value: "unsafe\ncredential_that_is_long_enough", expiresAt: now.Add(time.Hour), installationID: 101}, want: ErrInvalidDiscoveryToken},
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

func TestInstallationClientAcceptsFullPageOfRealisticallySizedRepositories(t *testing.T) {
	t.Parallel()
	now := installationTestNow()
	// Real GitHub repository objects carry ~80 URL and metadata fields
	// (~5-6 KiB each); a full per_page=100 page is far larger than token
	// responses.
	padding := `,"description":"` + strings.Repeat("d", 5500) + `"`
	repositories := make([]string, 0, installationPageSize)
	for index := range installationPageSize {
		object := repositoryInstallationJSON(int64(1000+index), 1001, fmt.Sprintf("fern-inc/repo-%d", index), false, false)
		repositories = append(repositories, strings.TrimSuffix(object, "}")+padding+"}")
	}
	body := `{"total_count":` + strconv.Itoa(installationPageSize) + `,"repositories":[` + strings.Join(repositories, ",") + `]}`
	if len(body) <= 500<<10 {
		t.Fatalf("fixture too small: %d bytes", len(body))
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, body)
	}))
	defer server.Close()
	client := newInstallationTestClient(t, server.Client(), server.URL, &installationAppSource{token: installationTestAppToken}, installationTestDiscoverySource(t, now, 101), now)

	listed, err := client.ListInstallationRepositories(installationTestContext(t), 101)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != installationPageSize {
		t.Fatalf("repositories = %d, want %d", len(listed), installationPageSize)
	}
}

func TestInstallationClientRejectsMalformedDuplicateAndOversizedPages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body []byte
		want error
	}{
		{name: "malformed", body: []byte(`[{"id":`)},
		{name: "null", body: []byte(`null`)},
		{name: "duplicate", body: []byte(`[{"id":101,"ID":101}]`)},
		{name: "invalid UTF8", body: []byte{'[', '"', 0xff, '"', ']'}},
		{name: "trailing", body: []byte(`[] {}`)},
		{name: "missing required", body: []byte(`[{"id":101,"account":null,"target_type":"Organization","repository_selection":"selected"}]`)},
		{name: "unsafe value", body: []byte(`[` + installationJSON(101, 1001, "fern-inc", "Bot", "selected") + `]`)},
		{name: "oversized", body: []byte(strings.Repeat("x", maxListPageBytes+1)), want: ErrResponseTooLarge},
		{name: "duplicate installation", body: []byte(`[` + installationJSON(101, 1001, "fern-inc", "Organization", "all") + `,` + installationJSON(101, 1002, "other", "Organization", "all") + `]`), want: ErrInstallationConflict},
		{name: "duplicate account", body: []byte(`[` + installationJSON(101, 1001, "fern-inc", "Organization", "all") + `,` + installationJSON(102, 1001, "fern-inc", "Organization", "all") + `]`), want: ErrInstallationConflict},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
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
	for _, selection := range []string{"selected", "all"} {
		installation := InstallationObservation{installationID: 101, accountLogin: "fern-inc", accountID: 1001, accountType: "Organization", targetType: "Organization", repositorySelection: selection}
		repository := InstallationRepositoryObservation{installationID: 101, repositoryID: 201, fullName: "fern-inc/widget", ownerLogin: "fern-inc", ownerID: 1001, ownerType: "Organization", name: "widget", private: true, defaultBranch: "main"}
		if err := SelectRepository([]InstallationObservation{installation}, []InstallationRepositoryObservation{repository}, 101, 201, "fern-inc/widget"); err != nil {
			t.Fatalf("%s: %v", selection, err)
		}
	}
}

func TestSelectRepositoryRejectsMismatchArchivedDisabledAndAmbiguity(t *testing.T) {
	t.Parallel()
	installation := InstallationObservation{installationID: 101, accountLogin: "fern-inc", accountID: 1001, accountType: "Organization", targetType: "Organization", repositorySelection: "selected"}
	repository := InstallationRepositoryObservation{installationID: 101, repositoryID: 201, fullName: "fern-inc/widget", ownerLogin: "fern-inc", ownerID: 1001, ownerType: "Organization", name: "widget", defaultBranch: "main"}
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
		{name: "owner tuple", installations: []InstallationObservation{installation}, repositories: []InstallationRepositoryObservation{mutateInstallationRepository(repository, func(value *InstallationRepositoryObservation) { value.ownerID = 2002 })}, id: 201, fullName: "fern-inc/widget", want: ErrRepositorySelection},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := SelectRepository(test.installations, test.repositories, 101, test.id, test.fullName)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if strings.Contains(fmt.Sprintf("%v %#v", err, err), "fern-inc") {
				t.Fatalf("selection error exposed candidate: %v", err)
			}
		})
	}
}

func TestInstallationDiscoveryTokenFormattingIsRedacted(t *testing.T) {
	t.Parallel()
	token := InstallationDiscoveryToken{value: installationTestCredential, installationID: 1}
	if formatted := fmt.Sprintf("%s %v %+v %#v", token, token, token, token); strings.Contains(formatted, installationTestCredential) {
		t.Fatalf("formatting exposed the token: %s", formatted)
	}
}

func TestInstallationClientRefusesUnboundedOrInconsistentPagination(t *testing.T) {
	t.Parallel()
	var page []string
	for index := range installationPageSize {
		page = append(page, repositoryInstallationJSON(int64(4000+index), 1001, fmt.Sprintf("fern-inc/r-%d", index), false, false))
	}
	tests := map[string]func(page int) string{
		// Every page full: more than ten pages of installations are refused.
		"installations": nil,
		"count overshoot": func(int) string {
			return `{"total_count":2,"repositories":[` + strings.Join(page[:3], ",") + `]}`
		},
		"short page before total": func(int) string {
			return `{"total_count":5,"repositories":[` + strings.Join(page[:3], ",") + `]}`
		},
	}
	for name, repositories := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				number := int(requests.Add(1))
				if repositories != nil {
					_, _ = io.WriteString(writer, repositories(number))
					return
				}
				var installations []string
				for index := range installationPageSize {
					id := int64(number*1000 + index + 1)
					installations = append(installations, installationJSON(id, id, fmt.Sprintf("org-%d", id), "Organization", "all"))
				}
				_, _ = io.WriteString(writer, `[`+strings.Join(installations, ",")+`]`)
			}))
			defer server.Close()
			client := newInstallationTestClient(t, server.Client(), server.URL, &installationAppSource{token: installationTestAppToken}, installationTestDiscoverySource(t, installationTestNow(), 101), installationTestNow())
			var err error
			if repositories == nil {
				_, err = client.ListAppInstallations(installationTestContext(t))
			} else {
				_, err = client.ListInstallationRepositories(installationTestContext(t), 101)
			}
			if !errors.Is(err, ErrPaginationRefused) || requests.Load() > installationMaxPages {
				t.Fatalf("error = %v after %d requests", err, requests.Load())
			}
		})
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
	return &installationDiscoverySource{token: InstallationDiscoveryToken{value: installationTestCredential, expiresAt: now.Add(time.Hour).UTC(), installationID: installationID}}
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
