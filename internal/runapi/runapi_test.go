package runapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nebler/fern/internal/auth"
	"github.com/nebler/fern/internal/domain"
	"github.com/nebler/fern/internal/opencode"
	"github.com/nebler/fern/internal/store"
)

const (
	testWorkspace = domain.WorkspaceID("wsp_0198d34d-6a50-75fb-b1f2-000000000001")
	testBase      = domain.GitOID("0123456789abcdef0123456789abcdef01234567")
)

type countingVerifier struct {
	calls atomic.Int64
	err   error
}

func (v *countingVerifier) Verify(context.Context, domain.GitOID) error { v.calls.Add(1); return v.err }

type retentionVerifier struct {
	calls atomic.Int64
	err   error
}

func (v *retentionVerifier) Verify(context.Context, store.RunResult) error {
	v.calls.Add(1)
	return v.err
}

type apiFixture struct {
	store    *store.Store
	handler  *Handler
	actor    domain.ActorSnapshot
	verifier *countingVerifier
	retained *retentionVerifier
	route    *fakeRoute
	path     string
	now      time.Time
}

type fakeRoute struct {
	active bool
	calls  int
}

func (route *fakeRoute) IssueAttachment(store.Run) (opencode.Attachment, bool, error) {
	route.calls++
	return opencode.Attachment{Origin: "https://fern.example:8443", Username: opencode.AttachmentUsername,
		Password: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", ExpiresAt: time.Now().Add(time.Hour)}, route.active, nil
}

func (route *fakeRoute) ActiveOrigin(store.Run) (string, bool) {
	return "https://fern.example:8443", route.active
}

func (fixture *apiFixture) rebuildCommands(t *testing.T) {
	t.Helper()
	fixture.handler.commands = &service{config: fixture.handler.config}
}

type resultProjectionStore struct {
	*store.Store
	run        store.Run
	projection store.RunResult
}

func (s *resultProjectionStore) GetRun(context.Context, domain.WorkspaceID, domain.RunID, domain.ActorSnapshot) (store.Run, error) {
	return s.run, nil
}

func (s *resultProjectionStore) GetRunResult(context.Context, domain.WorkspaceID, domain.RunID, domain.ActorSnapshot) (store.RunResult, error) {
	return s.projection, nil
}

func TestCreateMalformedJSONWritesOneError(t *testing.T) {
	fixture := newAPIFixture(t)
	for _, body := range []string{`{`, `{"unknown":true}`, `{"repository":"a","repository":"b"}`, strings.Repeat(" ", maxCreateBodyBytes+1)} {
		response := fixture.request(http.MethodPost, PathPrefix, body, "invalid-json-key")
		var payload struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		// Unmarshal rejects concatenated JSON responses, unlike a single Decode.
		if response.Code != http.StatusBadRequest || json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload.Error.Code != "invalid_json" {
			t.Fatalf("malformed body response = %d %s", response.Code, response.Body.String())
		}
	}
	if fixture.verifier.calls.Load() != 0 {
		t.Fatal("malformed request reached verifier")
	}
}

func TestRunAPIAdmissionReplayOwnershipStopAndRestart(t *testing.T) {
	fixture := newAPIFixture(t)
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
	operator := fixture.withActor(t, domain.ActorSnapshot{Type: domain.ActorOperator, ID: "operator", DisplayName: "Operator",
		CredentialID: "operator", Authentication: "basic", RequestID: "request"})
	if got := operator.request(http.MethodGet, PathPrefix, "", ""); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), response.RunID) ||
		!strings.Contains(got.Body.String(), `"attachable":false`) {
		t.Fatalf("operator workspace list = %d %s", got.Code, got.Body.String())
	}
	if got := operator.request(http.MethodGet, PathPrefix+"/"+response.RunID, "", ""); got.Code != http.StatusOK {
		t.Fatalf("operator get = %d %s", got.Code, got.Body.String())
	}
	for _, mutation := range []struct{ method, suffix, body string }{{http.MethodPost, "", validCreateBody("x")},
		{http.MethodPost, "/" + response.RunID + "/stop", "{}"}, {http.MethodPost, "/" + response.RunID + "/seal", "{}"},
		{http.MethodGet, "/" + response.RunID + "/result", ""}} {
		if got := operator.request(mutation.method, PathPrefix+mutation.suffix, mutation.body, "operator-key"); got.Code != http.StatusForbidden {
			t.Fatalf("operator %s %s = %d %s", mutation.method, mutation.suffix, got.Code, got.Body.String())
		}
	}
	device := fixture.withActor(t, domain.ActorSnapshot{Type: domain.ActorDevice, ID: "dev_1", DisplayName: "Phone",
		CredentialID: "dev_1", Authentication: "device_cookie", RequestID: "request"})
	if got := device.request(http.MethodGet, PathPrefix, "", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("device reached run API = %d %s", got.Code, got.Body.String())
	}
	for _, route := range []struct{ method, suffix string }{{http.MethodGet, "/result"}} {
		result := fixture.request(route.method, PathPrefix+"/"+response.RunID+route.suffix, "", "")
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
	runStore, err := store.Open(context.Background(), fixture.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runStore.Close() })
	restarted := fixture.withStore(t, runStore)
	got := restarted.request(http.MethodGet, PathPrefix+"/"+response.RunID, "", "")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"state":"failed"`) {
		t.Fatalf("restart get = %d %s", got.Code, got.Body.String())
	}
}

func TestRunAPIRejectsRepositoryBaseProfileScopeAndMalformedHTTP(t *testing.T) {
	fixture := newAPIFixture(t)
	work := validCreateBody("Work")
	tests := []struct {
		name, method, path, body, key string
		status                        int
	}{
		{"remote", http.MethodPost, PathPrefix, strings.Replace(work, "owner/repository", "owner/other", 1), "remote", 400},
		{"base", http.MethodPost, PathPrefix, strings.Replace(work, string(testBase), "ABC", 1), "base", 400},
		{"profile", http.MethodPost, PathPrefix, strings.Replace(work, domain.SourceProfile, "opencode-latest", 1), "profile", 400},
		{"unknown field", http.MethodPost, PathPrefix, strings.TrimSuffix(work, "}") + `,"extra":true}`, "unknown", 400},
		{"duplicate", http.MethodPost, PathPrefix, strings.Replace(work, `"profile":`, `"profile":"x","profile":`, 1), "duplicate", 400},
		{"query", http.MethodGet, PathPrefix + "?limit=1", "", "", 400},
		{"missing idempotency key", http.MethodPost, PathPrefix, work, "", 400},
		{"multiline instruction", http.MethodPost, PathPrefix, validCreateBody("first line\n\tsecond line"), "multiline", 202},
		{"control instruction", http.MethodPost, PathPrefix, validCreateBody("unsafe\rcontrol"), "control", 400},
		{"Unicode control instruction", http.MethodPost, PathPrefix, validCreateBody("unsafe\u0085control"), "unicode-control", 400},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := fixture.request(test.method, test.path, test.body, test.key); got.Code != test.status {
				t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
			}
		})
	}
	if got := fixture.requestWithContentType(http.MethodPost, PathPrefix, work, "content", "application/json; charset=utf-8"); got.Code != http.StatusBadRequest {
		t.Fatalf("content type=%d", got.Code)
	}
	unauthenticated := httptest.NewRecorder()
	fixture.handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, PathPrefix, nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("missing scope context=%d", unauthenticated.Code)
	}
	fixture.verifier.err = errors.New("unreachable")
	if got := fixture.request(http.MethodPost, PathPrefix, work, "unreachable"); got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unreachable=%d %s", got.Code, got.Body.String())
	}
}

func TestRunAPIMutationsRequireBoundedEmptyObject(t *testing.T) {
	fixture := newAPIFixture(t)
	created := fixture.request(http.MethodPost, PathPrefix, validCreateBody("Work"), "create")
	var response struct {
		RunID string `json:"run_id"`
	}
	if created.Code != http.StatusAccepted || json.Unmarshal(created.Body.Bytes(), &response) != nil {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	for _, suffix := range []string{"/stop"} {
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
	fixture := newAPIFixture(t)
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
	failedGenerator, err := domain.NewGenerator(strings.NewReader(""), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	fixture.handler.config.Generator = failedGenerator
	fixture.handler.config.Now = func() time.Time { panic("stop replay read time") }
	fixture.rebuildCommands(t)
	if got := fixture.request(http.MethodPost, PathPrefix+"/"+response.RunID+"/stop", "{}", "stop"); got.Code != http.StatusAccepted || got.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay=%d %s", got.Code, got.Body.String())
	}
}

func TestRunAPIWakeFollowsDurableCreateAndStopCommitOnly(t *testing.T) {
	fixture := newAPIFixture(t)
	var wakes atomic.Int64
	fixture.handler.config.Wake = func() {
		wakes.Add(1)
		runs, err := fixture.store.ListRuns(context.Background(), testWorkspace, fixture.actor, 10)
		if err != nil || len(runs) != 1 {
			t.Errorf("wake could not observe committed run: runs=%d error=%v", len(runs), err)
		}
	}
	fixture.rebuildCommands(t)
	if got := fixture.request(http.MethodPost, PathPrefix, strings.Replace(validCreateBody("Work"), "owner/repository", "owner/other", 1), "invalid"); got.Code != http.StatusBadRequest || wakes.Load() != 0 {
		t.Fatalf("invalid create status=%d wakes=%d", got.Code, wakes.Load())
	}
	created := fixture.request(http.MethodPost, PathPrefix, validCreateBody("Work"), "create-wake")
	var response struct {
		RunID domain.RunID `json:"run_id"`
	}
	if created.Code != http.StatusAccepted || json.Unmarshal(created.Body.Bytes(), &response) != nil || wakes.Load() != 1 {
		t.Fatalf("create status=%d wakes=%d body=%s", created.Code, wakes.Load(), created.Body.String())
	}
	if replay := fixture.request(http.MethodPost, PathPrefix, validCreateBody("Work"), "create-wake"); replay.Code != http.StatusAccepted || wakes.Load() != 1 {
		t.Fatalf("create replay status=%d wakes=%d", replay.Code, wakes.Load())
	}
	fixture.handler.config.Wake = func() {
		wakes.Add(1)
		run, err := fixture.store.GetRun(context.Background(), testWorkspace, response.RunID, fixture.actor)
		if err != nil || (run.State != domain.Canceling && run.State != domain.Failed) {
			t.Errorf("stop wake could not observe committed state: run=%+v error=%v", run, err)
		}
	}
	fixture.rebuildCommands(t)
	stopPath := PathPrefix + "/" + string(response.RunID) + "/stop"
	if got := fixture.request(http.MethodPost, stopPath, `{"not":"empty"}`, "bad-stop"); got.Code != http.StatusBadRequest || wakes.Load() != 1 {
		t.Fatalf("invalid stop status=%d wakes=%d", got.Code, wakes.Load())
	}
	if got := fixture.request(http.MethodPost, stopPath, "{}", "stop-wake"); got.Code != http.StatusAccepted || wakes.Load() != 2 {
		t.Fatalf("stop status=%d wakes=%d body=%s", got.Code, wakes.Load(), got.Body.String())
	}
	if replay := fixture.request(http.MethodPost, stopPath, "{}", "stop-wake"); replay.Code != http.StatusAccepted || wakes.Load() != 2 {
		t.Fatalf("stop replay status=%d wakes=%d", replay.Code, wakes.Load())
	}
}

func TestRunAPISealIsStrictOwnedAndExactlyReplayable(t *testing.T) {
	fixture := newAPIFixture(t)
	created := fixture.request(http.MethodPost, PathPrefix, validCreateBody("seal this run"), "seal-create")
	if created.Code != http.StatusAccepted {
		t.Fatalf("create = %d: %s", created.Code, created.Body.String())
	}
	var admission struct {
		RunID domain.RunID `json:"run_id"`
	}
	if json.Unmarshal(created.Body.Bytes(), &admission) != nil {
		t.Fatal("decode create")
	}
	fixture.advanceToPrompt(t, admission.RunID)
	path := PathPrefix + "/" + string(admission.RunID) + "/seal"
	if read := fixture.request(http.MethodGet, PathPrefix+"/"+string(admission.RunID), "", ""); read.Code != http.StatusOK {
		t.Fatalf("run before seal = %d: %s", read.Code, read.Body.String())
	}
	bad := fixture.request(http.MethodPost, path, `{"unexpected":true}`, "seal-key-bad")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("nonempty seal body = %d", bad.Code)
	}
	sealed := fixture.request(http.MethodPost, path, `{}`, "seal-key")
	if sealed.Code != http.StatusAccepted {
		t.Fatalf("seal = %d: %s", sealed.Code, sealed.Body.String())
	}
	var first sealProjection
	if json.Unmarshal(sealed.Body.Bytes(), &first) != nil || first.RunID != admission.RunID || first.State != "canceling" ||
		first.ResultPhase != "seal_requested" || first.ResultID == "" || !first.Committed {
		t.Fatalf("seal projection = %+v", first)
	}
	replay := fixture.request(http.MethodPost, path, `{}`, "seal-key")
	var second sealProjection
	if replay.Code != http.StatusAccepted || replay.Header().Get("Idempotency-Replayed") != "true" ||
		json.Unmarshal(replay.Body.Bytes(), &second) != nil || second != first {
		t.Fatalf("seal replay = %d %+v headers=%v", replay.Code, second, replay.Header())
	}
	other := fixture.withActor(t, pluginActor("pc_other"))
	hidden := other.request(http.MethodPost, path, `{}`, "other-seal-key")
	if hidden.Code != http.StatusNotFound {
		t.Fatalf("foreign seal = %d", hidden.Code)
	}
}

func TestRunAPIResultSeparatesImmutableAuthoritiesAndHidesStorage(t *testing.T) {
	fixture := newAPIFixture(t)
	ids := domain.NewSecureGenerator()
	runID, _ := ids.RunID()
	resultID, _ := ids.ResultID()
	changes := sha256.Sum256([]byte("changes"))
	manifest := sha256.Sum256([]byte("artifact manifest"))
	bundle := sha256.Sum256([]byte("bundle"))
	run := store.Run{RunID: runID, WorkspaceID: testWorkspace,
		RepositoryRemote: "https://github.com/owner/repository", State: domain.ResultReady,
		EffectPhase: domain.CleanupComplete, Seal: &store.Seal{ResultID: resultID}}
	projection := store.RunResult{Run: run,
		Result: store.Result{ID: resultID, RunID: runID, State: store.ResultSealed, Outcome: domain.ResultChanged, BaseSHA: testBase,
			ResultCommit: domain.GitOID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), TreeOID: domain.GitOID("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
			ChangeCount: 2, ChangesSHA256: changes, ManifestSHA256: manifest, BundleSHA256: bundle, BundleBytes: 1234}}
	runStore := &resultProjectionStore{Store: fixture.store, run: run, projection: projection}
	fixture.handler.config.Store = runStore
	response := fixture.request(http.MethodGet, PathPrefix+"/"+string(runID)+"/result", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("result=%d %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	result := body["result"].(map[string]any)
	artifact := body["artifact"].(map[string]any)
	if result["manifest_sha256"] != fmt.Sprintf("%x", changes) || artifact["manifest_sha256"] != fmt.Sprintf("%x", manifest) ||
		artifact["sha256"] != fmt.Sprintf("%x", manifest) || artifact["bundle_sha256"] != fmt.Sprintf("%x", bundle) ||
		result["manifest_sha256"] == artifact["manifest_sha256"] {
		t.Fatalf("digest authorities were conflated: %s", response.Body.String())
	}
	for _, forbidden := range []string{"locator", "cas_locator", "path", "storage_key", "container", "credential", "url"} {
		if strings.Contains(strings.ToLower(response.Body.String()), forbidden) {
			t.Fatalf("result exposed forbidden field %q: %s", forbidden, response.Body.String())
		}
	}
	if cleanup := body["cleanup"].(map[string]any); cleanup["complete"] != true {
		t.Fatalf("cleanup projection=%v", cleanup)
	}
	if retention := body["retention"].(map[string]any); retention["verified"] != true || retention["reconstructable"] != true || fixture.retained.calls.Load() != 1 {
		t.Fatalf("retention projection=%v calls=%d", retention, fixture.retained.calls.Load())
	}
	retention := func() map[string]any {
		t.Helper()
		response := fixture.request(http.MethodGet, PathPrefix+"/"+string(runID)+"/result", "", "")
		if response.Code != http.StatusOK {
			t.Fatalf("result=%d %s", response.Code, response.Body.String())
		}
		var decoded map[string]any
		if json.Unmarshal(response.Body.Bytes(), &decoded) != nil {
			t.Fatal("decode result")
		}
		return decoded["retention"].(map[string]any)
	}
	// The immutable retained tuple is verified once per process.
	if got := retention(); got["verified"] != true || fixture.retained.calls.Load() != 1 {
		t.Fatalf("cached retention projection=%v calls=%d", got, fixture.retained.calls.Load())
	}
	// A different bundle digest is a different tuple; failures are not cached.
	runStore.projection.Result.BundleSHA256 = sha256.Sum256([]byte("other bundle"))
	fixture.retained.err = errors.New("artifact missing")
	for want := int64(2); want <= 3; want++ {
		if got := retention(); got["verified"] != false || got["reconstructable"] != false || fixture.retained.calls.Load() != want {
			t.Fatalf("missing retention projection=%v calls=%d", got, fixture.retained.calls.Load())
		}
	}
	fixture.retained.err = nil
	if got := retention(); got["verified"] != true || fixture.retained.calls.Load() != 4 {
		t.Fatalf("recovered retention projection=%v calls=%d", got, fixture.retained.calls.Load())
	}

	runStore.run.State = domain.Canceling
	runStore.run.EffectPhase = domain.Sealing
	runStore.run.LastError = "retained artifact export retry required"
	recovery := fixture.request(http.MethodGet, PathPrefix+"/"+string(runID)+"/result", "", "")
	if recovery.Code != http.StatusServiceUnavailable || !strings.Contains(recovery.Body.String(), "recovery_required") {
		t.Fatalf("recovery result=%d %s", recovery.Code, recovery.Body.String())
	}
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "tasks.db")
	runStore, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	if err := runStore.CreateWorkspace(context.Background(), store.Workspace{ID: testWorkspace, Name: "demo", State: store.WorkspaceActive, RepositoryPath: "/srv/repo", GitHubAuthority: store.GitHubAuthorityAppBroker, InstallationID: 1, RepositoryID: 99, RepositoryFullName: "owner/repository", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	fixture := &apiFixture{store: runStore, actor: pluginActor("pc_owner"), verifier: &countingVerifier{}, retained: &retentionVerifier{}, route: &fakeRoute{}, path: path, now: now}
	t.Cleanup(func() { _ = runStore.Close() })
	fixture.handler = fixture.buildHandler(t)
	return fixture
}
func (f *apiFixture) buildHandler(t *testing.T) *Handler {
	t.Helper()
	backgroundImage := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	handler, err := New(Config{WorkspaceID: testWorkspace, RepositoryID: 99, RepositoryRemote: "https://github.com/owner/repository", BackgroundImageIdentity: backgroundImage, BackgroundEnvironmentSHA256: sha256.Sum256([]byte("{}")), Store: f.store, Route: f.route, Generator: domain.NewSecureGenerator(), ActorResolver: func(context.Context) (domain.ActorSnapshot, error) { return f.actor, nil }, BaseVerifier: f.verifier, RetentionVerifier: f.retained, Now: func() time.Time { return f.now }, RunTimeout: time.Hour, Agent: "build", ModelProvider: "test", Model: "model", SealPolicyVersion: "fern.background-user-seal.v1"})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func (f *apiFixture) advanceToSession(t *testing.T, id domain.RunID) store.Run {
	t.Helper()
	now := time.Date(2026, 8, 31, 12, 0, 1, 0, time.UTC)
	queued, err := f.store.NextRun(context.Background(), testWorkspace, domain.SourceProfile)
	if err != nil || queued.RunID != id {
		t.Fatalf("next run=%+v error=%v", queued, err)
	}
	run, err := f.store.StartRunProvisioning(context.Background(), openTestRef(queued, now))
	if err != nil {
		t.Fatalf("start run=%+v error=%v", run, err)
	}
	started := time.Date(2026, 8, 31, 12, 0, 2, 123456789, time.UTC)
	now = now.Add(time.Millisecond)
	run, err = f.store.RecordRunRuntime(context.Background(), store.RecordRunRuntimeParams{
		RunRef: openTestRef(run, now), ContainerID: strings.Repeat("a", 64), ContainerStartedAt: started.Format(time.RFC3339Nano),
		RuntimeEpoch: started.UnixNano(), HostPort: 49152, Evidence: `{"status":"exact"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Millisecond)
	run, err = f.store.RecordRunPromptRequestAttempted(context.Background(), openTestRef(run, now))
	if err != nil {
		t.Fatal(err)
	}
	f.now = now.Add(time.Second)
	return run
}

func (f *apiFixture) advanceToPrompt(t *testing.T, id domain.RunID) store.Run {
	run := f.advanceToSession(t, id)
	now := run.UpdatedAt.Add(time.Millisecond)
	var err error
	run, err = f.store.RecordRunPromptAdmitted(context.Background(), store.RecordRunEvidenceParams{
		RunRef: openTestRef(run, now), Evidence: `{"status":"exact"}`})
	if err != nil {
		t.Fatal(err)
	}
	f.now = now.Add(time.Second)
	return run
}

func openTestRef(run store.Run, now time.Time) store.RunRef {
	return store.RunRef{WorkspaceID: run.WorkspaceID, RunID: run.RunID,
		ExpectedRevision: run.Revision, ExpectedState: run.State, ExpectedPhase: run.EffectPhase, Now: now}
}
func (f *apiFixture) withActor(t *testing.T, actor domain.ActorSnapshot) *apiFixture {
	clone := *f
	clone.actor = actor
	clone.handler = clone.buildHandler(t)
	return &clone
}
func (f *apiFixture) withStore(t *testing.T, runStore *store.Store) *apiFixture {
	clone := *f
	clone.store = runStore
	clone.handler = clone.buildHandler(t)
	return &clone
}
func pluginActor(id string) domain.ActorSnapshot {
	return domain.ActorSnapshot{Type: domain.ActorOpenCode, ID: id, DisplayName: "OpenCode plugin", CredentialID: id, Authentication: "fern_plugin_bearer", RequestID: "request"}
}
func validCreateBody(instruction string) string {
	value, _ := json.Marshal(createInput{Repository: "https://github.com/owner/repository", BaseOID: string(testBase), Branch: stringPointer("main"), Instruction: instruction, Profile: domain.SourceProfile})
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
	credential := auth.Credential{ID: f.actor.ID}
	request = request.WithContext(auth.WithRequestAuthorization(request.Context(), credential))
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

func TestRunAPIAttachIssuesOnlyForReadyActiveRoute(t *testing.T) {
	fixture := newAPIFixture(t)
	created := fixture.request(http.MethodPost, PathPrefix, validCreateBody("attach to this"), "attach-create")
	var admission struct {
		RunID domain.RunID `json:"run_id"`
	}
	if created.Code != http.StatusAccepted || json.Unmarshal(created.Body.Bytes(), &admission) != nil {
		t.Fatalf("create = %d: %s", created.Code, created.Body.String())
	}
	path := PathPrefix + "/" + string(admission.RunID) + "/attach"
	if got := fixture.request(http.MethodGet, path, "", ""); got.Code != http.StatusConflict || fixture.route.calls != 0 {
		t.Fatalf("queued attach = %d calls=%d %s", got.Code, fixture.route.calls, got.Body.String())
	}
	fixture.advanceToSession(t, admission.RunID)
	if got := fixture.request(http.MethodGet, path, "", ""); got.Code != http.StatusConflict || fixture.route.calls != 1 {
		t.Fatalf("inactive route attach = %d calls=%d %s", got.Code, fixture.route.calls, got.Body.String())
	}
	fixture.route.active = true
	if got := fixture.request(http.MethodPost, path, "{}", "attach-post"); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("attach POST = %d", got.Code)
	}
	if got := fixture.withActor(t, pluginActor("pc_other")).request(http.MethodGet, path, "", ""); got.Code != http.StatusNotFound {
		t.Fatalf("cross-credential attach = %d %s", got.Code, got.Body.String())
	}
	operator := fixture.withActor(t, domain.ActorSnapshot{Type: domain.ActorOperator, ID: "operator", DisplayName: "Operator",
		CredentialID: "operator", Authentication: "basic", RequestID: "request"})
	if got := operator.request(http.MethodGet, PathPrefix, "", ""); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"attachable":true`) {
		t.Fatalf("operator list = %d %s", got.Code, got.Body.String())
	}
	for _, client := range []*apiFixture{fixture, operator} {
		got := client.request(http.MethodGet, path, "", "")
		if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"session_id":"ses_`) ||
			!strings.Contains(got.Body.String(), `"password":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`) || got.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s attach = %d %s", client.actor.Type, got.Code, got.Body.String())
		}
	}
}
