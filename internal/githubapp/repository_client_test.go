package githubapp

import (
	"context"
	"encoding/json"
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

func TestRepositoryClientBranchReferenceWireContract(t *testing.T) {
	t.Parallel()
	now, identity, source := repositoryTestAuth(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertRepositoryRequest(t, request, http.MethodGet, "/repos/fern-inc/widget/git/ref/heads/release/v1", "", false)
		_, _ = io.WriteString(writer, `{"ref":"refs/heads/release/v1","node_id":"ignored","object":{"type":"commit","sha":"0123456789abcdef0123456789abcdef01234567","url":"ignored"}}`)
	}))
	defer server.Close()
	client := newRepositoryTestClient(t, server.Client(), server.URL, source, now)

	observation, err := client.BranchReference(repositoryTestContext(t), identity, "fern-inc/widget", "release/v1")
	if err != nil {
		t.Fatal(err)
	}
	if observation.Identity() != identity || observation.Ref() != "release/v1" || observation.SHA() != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("observation = %#v", observation)
	}
	if source.callCount() != 1 {
		t.Fatalf("token calls = %d", source.callCount())
	}
}

func TestRepositoryClientBranchReferenceRejectsUnprovenResponses(t *testing.T) {
	t.Parallel()
	responses := []string{
		`{"ref":"refs/heads/other","object":{"type":"commit","sha":"0123456789abcdef0123456789abcdef01234567"}}`,
		`{"ref":"refs/heads/main","object":{"type":"tag","sha":"0123456789abcdef0123456789abcdef01234567"}}`,
		`{"ref":"refs/heads/main","object":{"type":"commit","sha":"ABCDEF0123456789abcdef0123456789abcdef01"}}`,
		`{"ref":"refs/heads/main","object":{"type":"commit","sha":"short"}}`,
		`{"ref":"refs/heads/main","object":{"type":"commit","sha":"0123456789abcdef0123456789abcdef01234567","sha":"0123456789abcdef0123456789abcdef01234567"}}`,
		`null`,
	}
	for index, response := range responses {
		response := response
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			t.Parallel()
			now, identity, source := repositoryTestAuth(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(writer, response)
			}))
			defer server.Close()
			client := newRepositoryTestClient(t, server.Client(), server.URL, source, now)
			if _, err := client.BranchReference(repositoryTestContext(t), identity, "fern-inc/widget", "main"); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRepositoryClientFindOpenDraftPullRequestsWireAndIdentity(t *testing.T) {
	t.Parallel()
	now, identity, source := repositoryTestAuth(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertRepositoryRequest(t, request, http.MethodGet, "/repos/fern-inc/widget/pulls", "base=release%2Fv1&head=fern-inc%3Afern%2Fjob-1&per_page=2&state=open", false)
		_, _ = io.WriteString(writer, `[{"number":17,"state":"open","draft":true,"base":{"ref":"release/v1","repo":{"id":202,"full_name":"fern-inc/widget","name":"widget","owner":{"login":"fern-inc"}}},"head":{"ref":"fern/job-1","repo":{"id":202,"full_name":"fern-inc/widget","name":"widget","owner":{"login":"fern-inc"}}},"url":"ignored documented extra"}]`)
	}))
	defer server.Close()
	client := newRepositoryTestClient(t, server.Client(), server.URL, source, now)

	summaries, err := client.FindOpenDraftPullRequests(repositoryTestContext(t), identity, "fern-inc/widget", "release/v1", "fern/job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Identity() != identity || summaries[0].Target() != "fern-inc/widget" || summaries[0].Number() != 17 {
		t.Fatalf("summaries = %#v", summaries)
	}
}

func TestRepositoryClientFindRefusesAmbiguityPaginationAndConflicts(t *testing.T) {
	t.Parallel()
	valid := repositoryDiscoveryPullJSON(17, true, 202, "fern-inc/widget", "main", 202, "fern-inc/widget", "fern/job")
	tests := []struct {
		name  string
		body  string
		link  string
		want  error
		count int
	}{
		{name: "empty", body: `[]`},
		{name: "null", body: `null`, want: ErrInvalidResponse},
		{name: "pagination", body: `[]`, link: `<https://api.github.com/page=2>; rel="next"`, want: ErrPaginationRefused},
		{name: "ambiguity", body: `[` + valid + `,` + repositoryDiscoveryPullJSON(18, true, 202, "fern-inc/widget", "main", 202, "fern-inc/widget", "fern/job") + `]`, want: ErrAmbiguousPullRequests, count: 2},
		{name: "not draft", body: `[` + repositoryDiscoveryPullJSON(17, false, 202, "fern-inc/widget", "main", 202, "fern-inc/widget", "fern/job") + `]`, want: ErrPullRequestConflict},
		{name: "fork head", body: `[` + repositoryDiscoveryPullJSON(17, true, 202, "fern-inc/widget", "main", 303, "contributor/widget", "fern/job") + `]`, want: ErrPullRequestConflict},
		{name: "wrong base", body: `[` + repositoryDiscoveryPullJSON(17, true, 202, "fern-inc/widget", "other", 202, "fern-inc/widget", "fern/job") + `]`, want: ErrPullRequestConflict},
		{name: "duplicate nested key", body: `[{"number":17,"number":18}]`, want: ErrInvalidResponse},
		{name: "overfull ambiguous page", body: `[` + valid + `,` + valid + `,` + valid + `]`, want: ErrAmbiguousPullRequests, count: 3},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now, identity, source := repositoryTestAuth(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if test.link != "" {
					writer.Header().Set("Link", test.link)
				}
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()
			client := newRepositoryTestClient(t, server.Client(), server.URL, source, now)
			got, err := client.FindOpenDraftPullRequests(repositoryTestContext(t), identity, "fern-inc/widget", "main", "fern/job")
			if test.want == nil {
				if err != nil || len(got) != 0 {
					t.Fatalf("summaries = %v, error = %v", got, err)
				}
				return
			}
			if !errors.Is(err, test.want) || got != nil {
				t.Fatalf("summaries = %v, error = %v", got, err)
			}
			if test.count != 0 {
				var ambiguity *PullRequestAmbiguityError
				if !errors.As(err, &ambiguity) || ambiguity.Count() != test.count {
					t.Fatalf("ambiguity = %#v", ambiguity)
				}
			}
		})
	}
}

func TestRepositoryClientCreateDraftPullRequestWireContract(t *testing.T) {
	t.Parallel()
	now, identity, source := repositoryTestAuth(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertRepositoryRequest(t, request, http.MethodPost, "/repos/fern-inc/widget/pulls", "", true)
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 5 || body["title"] != "Publish verified result" || body["body"] != "Evidence\n\nDetails" || body["head"] != "fern-inc:fern/job" || body["base"] != "main" || body["draft"] != true {
			t.Fatalf("body = %#v", body)
		}
		writer.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(writer, `{"number":31,"title":"documented extra"}`)
	}))
	defer server.Close()
	client := newRepositoryTestClient(t, server.Client(), server.URL, source, now)
	number, err := client.CreateDraftPullRequest(repositoryTestContext(t), identity, "fern-inc/widget", "main", "fern/job", "Publish verified result", "Evidence\n\nDetails")
	if err != nil || number != 31 {
		t.Fatalf("number = %d, error = %v", number, err)
	}
}

func TestRepositoryClientCreateDoesNotRetryLostResponse(t *testing.T) {
	t.Parallel()
	now, identity, source := repositoryTestAuth(t)
	secret := "transport-secret-must-not-escape"
	var requests atomic.Int32
	transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New(secret)
	})
	client := newRepositoryTestClient(t, &http.Client{Transport: transport}, "https://api.github.test", source, now)
	number, err := client.CreateDraftPullRequest(repositoryTestContext(t), identity, "fern-inc/widget", "main", "fern/job", "Title", "Body")
	if number != 0 || !errors.Is(err, ErrRequestFailed) || requests.Load() != 1 || strings.Contains(fmt.Sprintf("%v %#v", err, err), secret) {
		t.Fatalf("number = %d, requests = %d, error = %v", number, requests.Load(), err)
	}
}

func TestRepositoryClientPullRequestReturnsCompleteForkObservation(t *testing.T) {
	t.Parallel()
	now, identity, source := repositoryTestAuth(t)
	baseSHA := strings.Repeat("a", 40)
	headSHA := strings.Repeat("b", 40)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertRepositoryRequest(t, request, http.MethodGet, "/repos/fern-inc/widget/pulls/31", "", false)
		_, _ = io.WriteString(writer, repositoryPullJSON(31, "https://github.com/fern-inc/widget/pull/31", "open", true,
			202, "fern-inc/widget", "main", baseSHA, 303, "contributor/widget-fork", "fern/job", headSHA))
	}))
	defer server.Close()
	client := newRepositoryTestClient(t, server.Client(), server.URL, source, now)

	observation, err := client.PullRequest(repositoryTestContext(t), identity, "fern-inc/widget", 31)
	if err != nil {
		t.Fatal(err)
	}
	base, head := observation.Base(), observation.Head()
	if observation.TargetRepositoryID() != 202 || observation.TargetRepositoryFullName() != "fern-inc/widget" || observation.Number() != 31 || observation.HTMLURL() != "https://github.com/fern-inc/widget/pull/31" || observation.State() != "open" || !observation.Draft() {
		t.Fatalf("observation = %#v", observation)
	}
	if base.RepositoryID() != 202 || base.RepositoryFullName() != "fern-inc/widget" || base.RepositoryOwner() != "fern-inc" || base.RepositoryName() != "widget" || base.Ref() != "main" || base.SHA() != baseSHA {
		t.Fatalf("base = %#v", base)
	}
	if head.RepositoryID() != 303 || head.RepositoryFullName() != "contributor/widget-fork" || head.RepositoryOwner() != "contributor" || head.RepositoryName() != "widget-fork" || head.Ref() != "fern/job" || head.SHA() != headSHA {
		t.Fatalf("head = %#v", head)
	}
}

func TestRepositoryClientPullRequestRejectsWrongTupleFields(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 40)
	valid := func() map[string]any {
		var result map[string]any
		if err := json.Unmarshal([]byte(repositoryPullJSON(31, "https://github.com/fern-inc/widget/pull/31", "open", true, 202, "fern-inc/widget", "main", sha, 202, "fern-inc/widget", "fern/job", sha)), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "number", mutate: func(value map[string]any) { value["number"] = float64(32) }},
		{name: "URL host casing is noncanonical", mutate: func(value map[string]any) { value["html_url"] = "https://GitHub.com/fern-inc/widget/pull/31" }},
		{name: "URL query", mutate: func(value map[string]any) {
			value["html_url"] = "https://github.com/fern-inc/widget/pull/31?token=secret"
		}},
		{name: "state", mutate: func(value map[string]any) { value["state"] = "merged" }},
		{name: "draft missing", mutate: func(value map[string]any) { delete(value, "draft") }},
		{name: "base repository id", mutate: func(value map[string]any) { nestedMap(value, "base", "repo")["id"] = float64(999) }},
		{name: "base full name", mutate: func(value map[string]any) { nestedMap(value, "base", "repo")["full_name"] = "other/widget" }},
		{name: "base owner composition", mutate: func(value map[string]any) { nestedMap(value, "base", "repo", "owner")["login"] = "other" }},
		{name: "head null repo", mutate: func(value map[string]any) { nestedMap(value, "head")["repo"] = nil }},
		{name: "head name composition", mutate: func(value map[string]any) { nestedMap(value, "head", "repo")["name"] = "other" }},
		{name: "head ref", mutate: func(value map[string]any) { nestedMap(value, "head")["ref"] = "bad..ref" }},
		{name: "short SHA", mutate: func(value map[string]any) { nestedMap(value, "head")["sha"] = "abc" }},
		{name: "uppercase SHA", mutate: func(value map[string]any) { nestedMap(value, "base")["sha"] = strings.Repeat("A", 40) }},
		{name: "missing SHA", mutate: func(value map[string]any) { delete(nestedMap(value, "base"), "sha") }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := valid()
			test.mutate(response)
			payload, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			now, identity, source := repositoryTestAuth(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write(payload) }))
			defer server.Close()
			client := newRepositoryTestClient(t, server.Client(), server.URL, source, now)
			if _, err := client.PullRequest(repositoryTestContext(t), identity, "fern-inc/widget", 31); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRepositoryClientValidatesCallerInputsBeforeTokenOrNetwork(t *testing.T) {
	t.Parallel()
	now, identity, _ := repositoryTestAuth(t)
	invalidUTF8 := string([]byte{0xff})
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
		{name: "bad base", call: func(client *RepositoryClient) error {
			_, err := client.FindOpenDraftPullRequests(repositoryTestContext(t), identity, "fern-inc/widget", "bad..ref", "fern/job")
			return err
		}, want: ErrInvalidRepositoryRequest},
		{name: "bad head", call: func(client *RepositoryClient) error {
			_, err := client.FindOpenDraftPullRequests(repositoryTestContext(t), identity, "fern-inc/widget", "main", "refs/@{job")
			return err
		}, want: ErrInvalidRepositoryRequest},
		{name: "zero number", call: func(client *RepositoryClient) error {
			_, err := client.PullRequest(repositoryTestContext(t), identity, "fern-inc/widget", 0)
			return err
		}, want: ErrInvalidRepositoryRequest},
		{name: "blank title", call: func(client *RepositoryClient) error {
			_, err := client.CreateDraftPullRequest(repositoryTestContext(t), identity, "fern-inc/widget", "main", "fern/job", " title ", "")
			return err
		}, want: ErrInvalidRepositoryRequest},
		{name: "invalid title UTF8", call: func(client *RepositoryClient) error {
			_, err := client.CreateDraftPullRequest(repositoryTestContext(t), identity, "fern-inc/widget", "main", "fern/job", invalidUTF8, "")
			return err
		}, want: ErrInvalidRepositoryRequest},
		{name: "invalid body UTF8", call: func(client *RepositoryClient) error {
			_, err := client.CreateDraftPullRequest(repositoryTestContext(t), identity, "fern-inc/widget", "main", "fern/job", "title", invalidUTF8)
			return err
		}, want: ErrInvalidRepositoryRequest},
		{name: "oversized body", call: func(client *RepositoryClient) error {
			_, err := client.CreateDraftPullRequest(repositoryTestContext(t), identity, "fern-inc/widget", "main", "fern/job", "title", strings.Repeat("x", maxPullBodyBytes+1))
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

func repositoryDiscoveryPullJSON(number int64, draft bool, baseID int64, baseName, baseRef string, headID int64, headName, headRef string) string {
	return fmt.Sprintf(`{"number":%d,"state":"open","draft":%t,"base":{"ref":%q,"repo":%s},"head":{"ref":%q,"repo":%s}}`, number, draft, baseRef, repositoryRepoJSON(baseID, baseName), headRef, repositoryRepoJSON(headID, headName))
}

func repositoryPullJSON(number int64, htmlURL, state string, draft bool, baseID int64, baseName, baseRef, baseSHA string, headID int64, headName, headRef, headSHA string) string {
	return fmt.Sprintf(`{"number":%d,"html_url":%q,"state":%q,"draft":%t,"base":{"ref":%q,"sha":%q,"repo":%s},"head":{"ref":%q,"sha":%q,"repo":%s}}`, number, htmlURL, state, draft, baseRef, baseSHA, repositoryRepoJSON(baseID, baseName), headRef, headSHA, repositoryRepoJSON(headID, headName))
}

func repositoryRepoJSON(id int64, fullName string) string {
	owner, name, _ := strings.Cut(fullName, "/")
	return fmt.Sprintf(`{"id":%d,"full_name":%q,"name":%q,"owner":{"login":%q}}`, id, fullName, name, owner)
}

func nestedMap(value map[string]any, path ...string) map[string]any {
	current := value
	for _, component := range path {
		current = current[component].(map[string]any)
	}
	return current
}

var _ InstallationTokenSource = (*repositoryTokenSource)(nil)
