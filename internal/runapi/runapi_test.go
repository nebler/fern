package runapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nebler/fern/internal/pluginauth"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	testWorkspace = task.WorkspaceID("wsp_0198d34d-6a50-75fb-b1f2-000000000001")
	testBase      = task.GitOID("0123456789abcdef0123456789abcdef01234567")
)

type countingVerifier struct {
	calls atomic.Int64
	err   error
}

func (v *countingVerifier) Verify(context.Context, task.GitOID) error { v.calls.Add(1); return v.err }

type apiFixture struct {
	store    *taskstore.Store
	handler  *Handler
	actor    task.ActorSnapshot
	verifier *countingVerifier
	path     string
}

func TestRunAPIAdmissionReplayOwnershipStopAndRestart(t *testing.T) {
	fixture := newAPIFixture(t, PluginOpenCodeProfile)
	created := fixture.request(http.MethodPost, PathPrefix, validCreateBody("Do the work"), "create-key")
	var response struct {
		RunID     string `json:"run_id"`
		Committed bool   `json:"committed"`
	}
	if created.Code != http.StatusAccepted || json.Unmarshal(created.Body.Bytes(), &response) != nil || !response.Committed || response.RunID == "" {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	replayed := fixture.request(http.MethodPost, PathPrefix, validCreateBody("Do the work"), "create-key")
	if replayed.Code != http.StatusAccepted || replayed.Header().Get("Idempotency-Replayed") != "true" || fixture.verifier.calls.Load() != 1 {
		t.Fatalf("replay = %d calls=%d %s", replayed.Code, fixture.verifier.calls.Load(), replayed.Body.String())
	}
	conflict := fixture.request(http.MethodPost, PathPrefix, validCreateBody("Different"), "create-key")
	if conflict.Code != http.StatusConflict || fixture.verifier.calls.Load() != 1 {
		t.Fatalf("conflict = %d calls=%d", conflict.Code, fixture.verifier.calls.Load())
	}
	listed := fixture.request(http.MethodGet, PathPrefix, "", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), response.RunID) {
		t.Fatalf("list = %d %s", listed.Code, listed.Body.String())
	}
	other := fixture.withActor(t, pluginActor("pc_other"))
	if got := other.request(http.MethodGet, PathPrefix+"/"+response.RunID, "", ""); got.Code != http.StatusNotFound {
		t.Fatalf("cross-credential get = %d", got.Code)
	}
	otherList := other.request(http.MethodGet, PathPrefix, "", "")
	if otherList.Code != http.StatusOK || otherList.Body.String() != "{\"runs\":[]}\n" {
		t.Fatalf("cross-credential list = %d %s", otherList.Code, otherList.Body.String())
	}
	for _, route := range []struct{ method, suffix string }{{http.MethodPost, "/open"}, {http.MethodGet, "/result"}} {
		result := fixture.request(route.method, PathPrefix+"/"+response.RunID+route.suffix, map[bool]string{true: "{}", false: ""}[route.method == http.MethodPost], "open-key")
		if result.Code != http.StatusConflict || !strings.Contains(result.Body.String(), "not_ready") {
			t.Fatalf("%s = %d %s", route.suffix, result.Code, result.Body.String())
		}
	}
	stopped := fixture.request(http.MethodPost, PathPrefix+"/"+response.RunID+"/stop", "{}", "stop-key")
	if stopped.Code != http.StatusAccepted || !strings.Contains(stopped.Body.String(), `"state":"failed"`) {
		t.Fatalf("stop = %d %s", stopped.Code, stopped.Body.String())
	}
	stopReplay := fixture.request(http.MethodPost, PathPrefix+"/"+response.RunID+"/stop", "{}", "stop-key")
	if stopReplay.Code != http.StatusAccepted || stopReplay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("stop replay = %d %s", stopReplay.Code, stopReplay.Body.String())
	}
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := taskstore.Open(context.Background(), fixture.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	restarted := fixture.withStore(t, store)
	got := restarted.request(http.MethodGet, PathPrefix+"/"+response.RunID, "", "")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"state":"failed"`) {
		t.Fatalf("restart get = %d %s", got.Code, got.Body.String())
	}
}

func TestRunAPIRejectsRepositoryBaseProfileScopeAndMalformedHTTP(t *testing.T) {
	fixture := newAPIFixture(t, PluginOpenCodeProfile)
	tests := []struct {
		name, body, key string
		status          int
	}{
		{"remote", strings.Replace(validCreateBody("Work"), "owner/repository", "owner/other", 1), "remote", 400},
		{"base", strings.Replace(validCreateBody("Work"), string(testBase), "ABC", 1), "base", 400},
		{"profile", strings.Replace(validCreateBody("Work"), PluginOpenCodeProfile, "opencode-latest", 1), "profile", 400},
		{"unknown field", strings.TrimSuffix(validCreateBody("Work"), "}") + `,"extra":true}`, "unknown", 400},
		{"duplicate", strings.Replace(validCreateBody("Work"), `"profile":`, `"profile":"x","profile":`, 1), "duplicate", 400},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := fixture.request(http.MethodPost, PathPrefix, test.body, test.key)
			if got.Code != test.status {
				t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
			}
		})
	}
	fixture.verifier.err = errors.New("unreachable")
	if got := fixture.request(http.MethodPost, PathPrefix, validCreateBody("Work"), "unreachable"); got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unreachable=%d %s", got.Code, got.Body.String())
	}
	unavailable := newAPIFixture(t, "")
	if got := unavailable.request(http.MethodPost, PathPrefix, validCreateBody("Work"), "profile-unavailable"); got.Code != http.StatusServiceUnavailable || !strings.Contains(got.Body.String(), "requires a configured image qualified") {
		t.Fatalf("unavailable=%d %s", got.Code, got.Body.String())
	}
	request := httptest.NewRequest(http.MethodGet, PathPrefix, nil)
	unauthenticated := httptest.NewRecorder()
	fixture.handler.ServeHTTP(unauthenticated, request)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("missing scope context=%d", unauthenticated.Code)
	}
	badContent := fixture.requestWithContentType(http.MethodPost, PathPrefix, validCreateBody("Work"), "content", "application/json; charset=utf-8")
	if badContent.Code != http.StatusBadRequest {
		t.Fatalf("content type=%d", badContent.Code)
	}
	if got := fixture.request(http.MethodGet, PathPrefix+"?limit=1", "", ""); got.Code != http.StatusBadRequest {
		t.Fatalf("query=%d", got.Code)
	}
	if got := fixture.request(http.MethodPost, PathPrefix, validCreateBody("Work"), ""); got.Code != http.StatusBadRequest {
		t.Fatalf("missing idempotency=%d", got.Code)
	}
	fixture.verifier.err = nil
	if got := fixture.request(http.MethodPost, PathPrefix, validCreateBody("first line\n\tsecond line"), "multiline"); got.Code != http.StatusAccepted {
		t.Fatalf("multiline instruction=%d %s", got.Code, got.Body.String())
	}
	if got := fixture.request(http.MethodPost, PathPrefix, validCreateBody("unsafe\rcontrol"), "control"); got.Code != http.StatusBadRequest {
		t.Fatalf("unsafe instruction=%d %s", got.Code, got.Body.String())
	}
	if got := fixture.request(http.MethodPost, PathPrefix, validCreateBody("unsafe\u0085control"), "unicode-control"); got.Code != http.StatusBadRequest {
		t.Fatalf("unsafe Unicode instruction=%d %s", got.Code, got.Body.String())
	}
}

func TestRunAPIMutationsRequireBoundedEmptyObject(t *testing.T) {
	fixture := newAPIFixture(t, PluginOpenCodeProfile)
	created := fixture.request(http.MethodPost, PathPrefix, validCreateBody("Work"), "create")
	var response struct {
		RunID string `json:"run_id"`
	}
	if created.Code != http.StatusAccepted || json.Unmarshal(created.Body.Bytes(), &response) != nil {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	for _, suffix := range []string{"/stop", "/open"} {
		for _, test := range []struct {
			name, path, body, contentType string
		}{
			{"query", PathPrefix + "/" + response.RunID + suffix + "?x=1", "{}", "application/json"},
			{"content type", PathPrefix + "/" + response.RunID + suffix, "{}", "application/json; charset=utf-8"},
			{"null", PathPrefix + "/" + response.RunID + suffix, "null", "application/json"},
			{"field", PathPrefix + "/" + response.RunID + suffix, `{"x":1}`, "application/json"},
			{"large", PathPrefix + "/" + response.RunID + suffix, strings.Repeat(" ", maxEmptyBodyBytes) + "{}", "application/json"},
		} {
			t.Run(strings.TrimPrefix(suffix, "/")+"/"+test.name, func(t *testing.T) {
				got := fixture.requestWithContentType(http.MethodPost, test.path, test.body, "mutation-"+suffix+test.name, test.contentType)
				if got.Code != http.StatusBadRequest || got.Body.Len() > 512 || !strings.Contains(got.Body.String(), `"error"`) {
					t.Fatalf("response=%d len=%d %s", got.Code, got.Body.Len(), got.Body.String())
				}
			})
		}
	}
}

func TestRunAPIStopReplayDoesNotNeedFreshEntropyOrTime(t *testing.T) {
	fixture := newAPIFixture(t, PluginOpenCodeProfile)
	created := fixture.request(http.MethodPost, PathPrefix, validCreateBody("Work"), "create")
	var response struct {
		RunID string `json:"run_id"`
	}
	if created.Code != http.StatusAccepted || json.Unmarshal(created.Body.Bytes(), &response) != nil {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	if got := fixture.request(http.MethodPost, PathPrefix+"/"+response.RunID+"/stop", "{}", "stop"); got.Code != http.StatusAccepted {
		t.Fatalf("stop=%d %s", got.Code, got.Body.String())
	}
	failedGenerator, err := task.NewGenerator(strings.NewReader(""), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	fixture.handler.config.Generator = failedGenerator
	fixture.handler.config.Now = func() time.Time { panic("stop replay read time") }
	if got := fixture.request(http.MethodPost, PathPrefix+"/"+response.RunID+"/stop", "{}", "stop"); got.Code != http.StatusAccepted || got.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay=%d %s", got.Code, got.Body.String())
	}
}

func TestGitBaseVerifierRequiresAllowedReachability(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repo")
	for _, args := range [][]string{{"init", repository}, {"-C", repository, "config", "user.email", "test@example.com"}, {"-C", repository, "config", "user.name", "Test"}} {
		if output, err := execGit(args...); err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "file"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", repository, "add", "file"}, {"-C", repository, "commit", "-m", "one"}} {
		if output, err := execGit(args...); err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
	}
	headRaw, err := execGit("-C", repository, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	head, err := task.ParseGitOID(strings.TrimSpace(headRaw))
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewGitBaseVerifier(repository, "/usr/bin/git", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(context.Background(), head); err != nil {
		t.Fatalf("reachable head: %v", err)
	}
	if err := verifier.Verify(context.Background(), testBase); err == nil {
		t.Fatal("missing object accepted")
	}
	if output, err := execGit("-C", repository, "tag", "-a", "annotated", "-m", "tag"); err != nil {
		t.Fatalf("annotated tag: %s: %v", output, err)
	}
	tagRaw, err := execGit("-C", repository, "rev-parse", "annotated")
	if err != nil {
		t.Fatal(err)
	}
	tagOID, err := task.ParseGitOID(strings.TrimSpace(tagRaw))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(context.Background(), tagOID); err == nil {
		t.Fatal("annotated tag object accepted as an exact commit")
	}
}

func TestGitBaseVerifierUsesPromisorSafeEnvironment(t *testing.T) {
	directory := t.TempDir()
	observed := filepath.Join(directory, "observed")
	git := filepath.Join(directory, "git")
	script := "#!/bin/sh\n" +
		"printf '%s' \"$GIT_NO_LAZY_FETCH\" > " + strconv.Quote(observed) + "\n" +
		"case \"$*\" in\n" +
		"  *'rev-parse --is-inside-work-tree'*) printf 'true\\n' ;;\n" +
		"  *'cat-file -t'*) printf 'commit\\n' ;;\n" +
		"  *for-each-ref*) exit 0 ;;\n" +
		"  *'merge-base --is-ancestor'*) exit 0 ;;\n" +
		"  *) exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(git, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	verifier, err := NewGitBaseVerifier(directory, git, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(context.Background(), testBase); err != nil {
		t.Fatal(err)
	}
	value, err := os.ReadFile(observed)
	if err != nil || string(value) != "1" {
		t.Fatalf("GIT_NO_LAZY_FETCH=%q, error=%v", value, err)
	}
}

func execGit(args ...string) (string, error) {
	command := exec.Command("/usr/bin/git", args...)
	value, err := command.CombinedOutput()
	return string(value), err
}

func newAPIFixture(t *testing.T, available string) *apiFixture {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "tasks.db")
	store, err := taskstore.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	if err := store.CreateWorkspace(context.Background(), taskstore.Workspace{ID: testWorkspace, Name: "demo", State: taskstore.WorkspaceActive, RepositoryPath: "/srv/repo", GitHubAuthority: taskstore.GitHubAuthorityAppBroker, InstallationID: 1, RepositoryID: 99, RepositoryFullName: "owner/repository", ImageDigest: "sha256:image", OpenCodeProtocol: "0.0.0-next-17444", RuntimeDesiredState: "running", ReconciliationEpoch: 1, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	fixture := &apiFixture{store: store, actor: pluginActor("pc_owner"), verifier: &countingVerifier{}, path: path}
	t.Cleanup(func() { _ = store.Close() })
	fixture.handler = fixture.buildHandler(t, available)
	return fixture
}
func (f *apiFixture) buildHandler(t *testing.T, available string) *Handler {
	t.Helper()
	backgroundImage := ""
	if available == PluginOpenCodeProfile {
		backgroundImage = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	}
	handler, err := New(Config{WorkspaceID: testWorkspace, RepositoryID: 99, RepositoryRemote: "https://github.com/owner/repository", BackgroundImageIdentity: backgroundImage, AvailableProfile: available, Store: f.store, Generator: task.NewSecureGenerator(), ActorResolver: func(context.Context) (task.ActorSnapshot, error) { return f.actor, nil }, BaseVerifier: f.verifier, Now: func() time.Time { return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC) }, AttemptTimeout: time.Hour, Agent: "build", ModelProvider: "test", Model: "model", BudgetSnapshot: json.RawMessage(`{"turns":10}`)})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}
func (f *apiFixture) withActor(t *testing.T, actor task.ActorSnapshot) *apiFixture {
	clone := *f
	clone.actor = actor
	clone.handler = clone.buildHandler(t, PluginOpenCodeProfile)
	return &clone
}
func (f *apiFixture) withStore(t *testing.T, store *taskstore.Store) *apiFixture {
	clone := *f
	clone.store = store
	clone.handler = clone.buildHandler(t, PluginOpenCodeProfile)
	return &clone
}
func pluginActor(id string) task.ActorSnapshot {
	return task.ActorSnapshot{Type: task.ActorOpenCode, ID: id, DisplayName: "OpenCode plugin", CredentialID: id, Authentication: "fern_plugin_bearer", RequestID: "request"}
}
func validCreateBody(instruction string) string {
	value, _ := json.Marshal(createInput{Repository: "https://github.com/owner/repository", BaseOID: string(testBase), Branch: stringPointer("main"), Instruction: instruction, Profile: PluginOpenCodeProfile})
	return string(value)
}
func stringPointer(value string) *string { return &value }
func (f *apiFixture) request(method, path, body, key string) *httptest.ResponseRecorder {
	return f.requestWithContentType(method, path, body, key, "application/json")
}
func (f *apiFixture) requestWithContentType(method, path, body, key, contentType string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	credential := pluginauth.Credential{ID: f.actor.ID}
	request = request.WithContext(pluginauth.WithRequestAuthorization(request.Context(), credential))
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}
