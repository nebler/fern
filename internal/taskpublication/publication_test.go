package taskpublication

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/task"
)

const (
	testToken   = "github_installation_token_1234567890"
	baseSHA     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	resultSHA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	conflictSHA = "cccccccccccccccccccccccccccccccccccccccc"
)

type staticAppToken struct{}

func (staticAppToken) AppToken(time.Time) (string, error) { return "header.payload.signature", nil }

type githubFixture struct {
	mu             sync.Mutex
	branchFile     string
	baseSHA        string
	hasPull        bool
	duplicatePulls bool
	loseCreate     bool
	requestCount   int
	createCount    int
}

func (fixture *githubFixture) RoundTrip(request *http.Request) (*http.Response, error) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	fixture.requestCount++
	path := request.URL.Path
	switch {
	case path == "/app/installations/7/access_tokens":
		return jsonResponse(http.StatusCreated, fmt.Sprintf(`{"token":%q,"expires_at":%q,"permissions":{"contents":"write","pull_requests":"write"}}`, testToken, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))), nil
	case path == "/repositories/123":
		return jsonResponse(http.StatusOK, `{"id":123,"full_name":"owner/repo","name":"repo","default_branch":"main","owner":{"login":"owner"}}`), nil
	case strings.HasPrefix(path, "/repos/owner/repo/git/ref/heads/"):
		branch := strings.TrimPrefix(path, "/repos/owner/repo/git/ref/heads/")
		if branch == "main" {
			return referenceResponse("main", fixture.baseSHA), nil
		}
		value, err := os.ReadFile(fixture.branchFile)
		if err != nil || strings.TrimSpace(string(value)) == "" {
			return jsonResponse(http.StatusNotFound, `{}`), nil
		}
		return referenceResponse(branch, strings.TrimSpace(string(value))), nil
	case path == "/repos/owner/repo/pulls" && request.Method == http.MethodGet:
		if !fixture.hasPull {
			return jsonResponse(http.StatusOK, `[]`), nil
		}
		pull := fixture.pullJSON(false)
		if fixture.duplicatePulls {
			return jsonResponse(http.StatusOK, "["+pull+","+pull+"]"), nil
		}
		return jsonResponse(http.StatusOK, "["+pull+"]"), nil
	case path == "/repos/owner/repo/pulls" && request.Method == http.MethodPost:
		fixture.createCount++
		fixture.hasPull = true
		if fixture.loseCreate {
			return nil, errors.New("simulated lost response")
		}
		return jsonResponse(http.StatusCreated, `{"number":1}`), nil
	case path == "/repos/owner/repo/pulls/1":
		return jsonResponse(http.StatusOK, fixture.pullJSON(true)), nil
	default:
		return jsonResponse(http.StatusNotFound, `{}`), nil
	}
}

func (fixture *githubFixture) pullJSON(includeURL bool) string {
	urlField := ""
	if includeURL {
		urlField = `,"html_url":"https://github.com/owner/repo/pull/1"`
	}
	return fmt.Sprintf(`{"number":1%s,"state":"open","draft":true,"base":{"ref":"main","sha":%q,"repo":{"id":123,"full_name":"owner/repo","name":"repo","owner":{"login":"owner"}}},"head":{"ref":%q,"sha":%q,"repo":{"id":123,"full_name":"owner/repo","name":"repo","owner":{"login":"owner"}}}}`, urlField, fixture.baseSHA, testPublication().Branch, resultSHA)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func referenceResponse(branch, sha string) *http.Response {
	return jsonResponse(http.StatusOK, fmt.Sprintf(`{"ref":%q,"object":{"type":"commit","sha":%q}}`, "refs/heads/"+branch, sha))
}

type testHarness struct {
	publisher  *Publisher
	fixture    *githubFixture
	request    Request
	logPath    string
	tempRoot   string
	branchFile string
}

func newHarness(t *testing.T, scriptMode string) testHarness {
	t.Helper()
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(realRoot, "repository")
	tempRoot := filepath.Join(realRoot, "credentials")
	if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tempRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	branchFile := filepath.Join(realRoot, "branch")
	logPath := filepath.Join(realRoot, "git.log")
	gitPath := filepath.Join(realRoot, "fake-git")
	installFakeGit(t, gitPath, branchFile, logPath, scriptMode)
	fixture := &githubFixture{branchFile: branchFile, baseSHA: baseSHA}
	httpClient := &http.Client{Transport: fixture}
	tokens, err := githubapp.NewClient(httpClient, staticAppToken{})
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := githubapp.NewRepositoryClient(httpClient, tokens, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := New(Config{
		RepositoryPath: repository, GitExecutable: gitPath, TempRoot: tempRoot,
		Timeout: 10 * time.Second, OutputLimit: 8,
	}, tokens, repositories)
	if err != nil {
		t.Fatal(err)
	}
	return testHarness{publisher: publisher, fixture: fixture, request: testRequest(), logPath: logPath, tempRoot: tempRoot, branchFile: branchFile}
}

func installFakeGit(t *testing.T, path, branchFile, logPath, mode string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
# Resolve a file mode portably: BSD stat first, then GNU stat. The fallback
# must be exclusive because GNU stat -f <format> is filesystem mode: it fails
# with exit 1 while still writing to stdout, which would pollute "$( ... )".
file_mode() {
  mode=$(stat -f '%%Lp' "$1" 2>/dev/null) && { printf '%%s\n' "$mode"; return; }
  stat -c '%%a' "$1"
}
printf 'argv:' >> %s
printf ' [%%s]' "$@" >> %s
printf '\nenv:' >> %s
env | sort >> %s
printf '\n' >> %s
case " $* " in
  *" cat-file "*) exit 0 ;;
  *" config "*)
    case %s in
      unsafeconfig) printf 'url.https://attacker.invalid/.insteadof\000' ;;
    esac
    exit 0
    ;;
esac
[ "$(file_mode "${GIT_ASKPASS%%/*}")" = 700 ] || exit 78
[ "$(file_mode "$GIT_ASKPASS")" = 700 ] || exit 79
[ "$(file_mode "${GIT_ASKPASS%%/*}/credential")" = 600 ] || exit 80
"$GIT_ASKPASS" "Username for 'https://github.com':" >/dev/null || exit 81
password=$("$GIT_ASKPASS" "Password for 'https://github.com':") || exit 82
[ "$password" = %s ] || exit 83
[ ! -e "${GIT_ASKPASS%%/*}/credential" ] || exit 84
last=
for argument do last=$argument; done
commit=${last%%%%:*}
printf 'push-output-more-than-limit\n'
printf 'push-error-more-than-limit\n' >&2
case %s in
  timeout) sleep 30 ;;
esac
printf '%%s\n' "$commit" > %s
case %s in
  lost) exit 1 ;;
esac
exit 0
`, shellQuote(logPath), shellQuote(logPath), shellQuote(logPath), shellQuote(logPath), shellQuote(logPath), shellQuote(mode), shellQuote(testToken), shellQuote(mode), shellQuote(branchFile), shellQuote(mode))
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func testRequest() Request {
	publication := testPublication()
	taskTuple := task.RepositoryTuple{RepositoryID: 123, BaseSHA: baseSHA}
	result := task.ResultTuple{RepositoryTuple: taskTuple, ResultCommit: resultSHA, Outcome: task.ResultChanged, ManifestEntries: 1, WorktreeClean: true}
	return Request{
		WorkspaceRepository: 123, Task: taskTuple, Result: result,
		Verification: task.VerificationTuple{State: task.VerificationSucceeded, VerifiedCommit: resultSHA},
		Publication:  publication, Title: "Verified change", Body: "Body",
	}
}

func testPublication() task.PublicationTuple {
	operation := task.PublicationOperationID("op_0198d34d-5e40-7c5a-8e3f-6bfad471ae12")
	return task.PublicationTuple{
		OperationID: operation, InstallationID: 7, RepositoryID: 123,
		RepositoryFullName: "owner/repo", WorkspaceName: "demo", BaseRef: "main",
		BaseSHA: baseSHA, ResultCommit: resultSHA, Branch: task.PublicationBranch("demo", operation),
	}
}

func publicationContext(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}

func TestPublishReconcilesLostPushAndCreateWithoutCredentialLeak(t *testing.T) {
	harness := newHarness(t, "lost")
	harness.fixture.loseCreate = true
	proof, err := harness.publisher.PublishOrReconcile(publicationContext(t, 5*time.Second), harness.request)
	if err != nil {
		t.Fatalf("%v: %+v", err, proof)
	}
	if proof.Observation.RemoteSHA != resultSHA || proof.Observation.PullRequest.Number != 1 || !proof.Push.Attempted || proof.Push.ExitCode == 0 || proof.PullRequestCreateConfirmed {
		t.Fatalf("unexpected proof: %+v", proof)
	}
	if proof.Push.Stdout.Bytes != proof.Push.Stdout.HashedBytes || !proof.Push.Stdout.Truncated || proof.Push.Stdout.HashedBytes <= 8 {
		t.Fatalf("stdout evidence not bounded: %+v", proof.Push.Stdout)
	}
	logBytes, err := os.ReadFile(harness.logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	if strings.Contains(log, testToken) || strings.Contains(log, "GH_TOKEN=") || !strings.Contains(log, "https://github.com/owner/repo.git") || !strings.Contains(log, "--force-with-lease=refs/heads/"+harness.request.Publication.Branch+":") {
		t.Fatalf("unsafe Git invocation log:\n%s", log)
	}
	entries, err := os.ReadDir(harness.tempRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("credential directory cleanup: entries=%v err=%v", entries, err)
	}
	if harness.fixture.createCount != 1 {
		t.Fatalf("pull request creates = %d", harness.fixture.createCount)
	}
}

func TestPublishRejectsBaseMovementBeforeGit(t *testing.T) {
	harness := newHarness(t, "success")
	harness.fixture.baseSHA = conflictSHA
	_, err := harness.publisher.PublishOrReconcile(publicationContext(t, 5*time.Second), harness.request)
	if !errors.Is(err, ErrBaseMoved) {
		t.Fatalf("error = %v", err)
	}
	if log, readErr := os.ReadFile(harness.logPath); readErr != nil || strings.Contains(string(log), " push ") {
		t.Fatalf("push ran before base proof: %s, %v", log, readErr)
	}
}

func TestPublishRefusesDuplicatePullRequests(t *testing.T) {
	harness := newHarness(t, "success")
	if err := os.WriteFile(harness.branchFile, []byte(resultSHA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	harness.fixture.hasPull = true
	harness.fixture.duplicatePulls = true
	_, err := harness.publisher.PublishOrReconcile(publicationContext(t, 5*time.Second), harness.request)
	if !errors.Is(err, githubapp.ErrAmbiguousPullRequests) {
		t.Fatalf("error = %v", err)
	}
	if harness.fixture.createCount != 0 {
		t.Fatalf("created %d pull requests after ambiguity", harness.fixture.createCount)
	}
}

func TestPublishTimesOutProcessGroupAndCleansCredentials(t *testing.T) {
	harness := newHarness(t, "timeout")
	harness.publisher.timeout = 100 * time.Millisecond
	started := time.Now()
	_, err := harness.publisher.PublishOrReconcile(publicationContext(t, 5*time.Second), harness.request)
	if !errors.Is(err, ErrGitTimeout) {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	entries, readErr := os.ReadDir(harness.tempRoot)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("credential cleanup after timeout: entries=%v err=%v", entries, readErr)
	}
}

func TestPublishRejectsBranchConflictWithoutPush(t *testing.T) {
	harness := newHarness(t, "success")
	if err := os.WriteFile(harness.branchFile, []byte(conflictSHA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := harness.publisher.PublishOrReconcile(publicationContext(t, 5*time.Second), harness.request)
	if !errors.Is(err, ErrBranchConflict) {
		t.Fatalf("error = %v", err)
	}
	if log, readErr := os.ReadFile(harness.logPath); readErr != nil || strings.Contains(string(log), " push ") {
		t.Fatalf("push ran for branch conflict: %s, %v", log, readErr)
	}
}

func TestPublishRejectsURLRewriteConfigBeforeNetwork(t *testing.T) {
	harness := newHarness(t, "unsafeconfig")
	_, err := harness.publisher.PublishOrReconcile(publicationContext(t, 5*time.Second), harness.request)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v", err)
	}
	if harness.fixture.requestCount != 0 {
		t.Fatalf("GitHub requests = %d", harness.fixture.requestCount)
	}
}

func TestPublishUsesExactExpectedOldSHA(t *testing.T) {
	harness := newHarness(t, "success")
	if err := os.WriteFile(harness.branchFile, []byte(conflictSHA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	harness.request.Publication.ExpectedRemoteOldSHA = conflictSHA
	proof, err := harness.publisher.PublishOrReconcile(publicationContext(t, 5*time.Second), harness.request)
	if err != nil {
		t.Fatalf("%v: %+v", err, proof)
	}
	if !proof.Push.Attempted {
		t.Fatal("expected push")
	}
	logBytes, err := os.ReadFile(harness.logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "--force-with-lease=refs/heads/" + harness.request.Publication.Branch + ":" + conflictSHA
	if !strings.Contains(string(logBytes), want) {
		t.Fatalf("missing exact lease %q in:\n%s", want, logBytes)
	}
}

func TestInvalidVerificationHasNoEffect(t *testing.T) {
	harness := newHarness(t, "success")
	harness.request.Verification.State = task.VerificationFailed
	_, err := harness.publisher.PublishOrReconcile(publicationContext(t, 5*time.Second), harness.request)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v", err)
	}
	if harness.fixture.requestCount != 0 {
		t.Fatalf("GitHub requests = %d", harness.fixture.requestCount)
	}
}

func TestRemoteURLCannotBeSuppliedByCaller(t *testing.T) {
	harness := newHarness(t, "success")
	proof, err := harness.publisher.PublishOrReconcile(publicationContext(t, 5*time.Second), harness.request)
	if err != nil {
		t.Fatalf("%v: %+v", err, proof)
	}
	if err := proof.Observation.ValidateAgainst(harness.request.Publication); err != nil {
		t.Fatal(err)
	}
	logBytes, err := os.ReadFile(harness.logPath)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse("https://github.com/owner/repo.git")
	if parsed.Host != "github.com" || !strings.Contains(string(logBytes), parsed.String()) {
		t.Fatalf("canonical remote absent: %s", logBytes)
	}
}

func TestNewRejectsUnsafePublicationPaths(t *testing.T) {
	t.Run("symlinked executable", func(t *testing.T) {
		harness := newHarness(t, "success")
		link := filepath.Join(filepath.Dir(harness.publisher.gitExecutable), "git-link")
		if err := os.Symlink(harness.publisher.gitExecutable, link); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Config{RepositoryPath: harness.publisher.repositoryPath, GitExecutable: link, TempRoot: harness.publisher.tempRoot}, harness.publisher.tokens, harness.publisher.repositories); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("writable executable", func(t *testing.T) {
		harness := newHarness(t, "success")
		if err := os.Chmod(harness.publisher.gitExecutable, 0o722); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Config{RepositoryPath: harness.publisher.repositoryPath, GitExecutable: harness.publisher.gitExecutable, TempRoot: harness.publisher.tempRoot}, harness.publisher.tokens, harness.publisher.repositories); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("writable repository", func(t *testing.T) {
		harness := newHarness(t, "success")
		if err := os.Chmod(harness.publisher.repositoryPath, 0o777); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Config{RepositoryPath: harness.publisher.repositoryPath, GitExecutable: harness.publisher.gitExecutable, TempRoot: harness.publisher.tempRoot}, harness.publisher.tokens, harness.publisher.repositories); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("non-private temporary root", func(t *testing.T) {
		harness := newHarness(t, "success")
		if err := os.Chmod(harness.publisher.tempRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Config{RepositoryPath: harness.publisher.repositoryPath, GitExecutable: harness.publisher.gitExecutable, TempRoot: harness.publisher.tempRoot}, harness.publisher.tokens, harness.publisher.repositories); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("error = %v", err)
		}
	})
}
