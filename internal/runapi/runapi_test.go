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

	"github.com/nebler/fern/internal/backgroundroute"
	"github.com/nebler/fern/internal/pluginauth"
	rundomain "github.com/nebler/fern/internal/run"
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

type retentionVerifier struct {
	calls atomic.Int64
	err   error
}

func (v *retentionVerifier) Verify(context.Context, taskstore.BackgroundRunResultProjection) error {
	v.calls.Add(1)
	return v.err
}

type apiFixture struct {
	store    *taskstore.Store
	handler  *Handler
	actor    task.ActorSnapshot
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

func (route *fakeRoute) IssueAttachment(taskstore.BackgroundRun) (backgroundroute.Attachment, bool, error) {
	route.calls++
	return backgroundroute.Attachment{Origin: "https://fern.example:8443", Username: backgroundroute.AttachmentUsername,
		Password: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", ExpiresAt: time.Now().Add(time.Hour)}, route.active, nil
}

func (route *fakeRoute) ActiveOrigin(taskstore.BackgroundRun) (string, bool) {
	return "https://fern.example:8443", route.active
}

func (fixture *apiFixture) rebuildCommands(t *testing.T) {
	t.Helper()
	fixture.handler.commands = &service{config: fixture.handler.config}
}

type resultProjectionStore struct {
	*taskstore.Store
	run        taskstore.BackgroundRun
	projection taskstore.BackgroundRunResultProjection
}

func (store *resultProjectionStore) GetBackgroundRun(context.Context, task.WorkspaceID, task.RunID, task.ActorSnapshot) (taskstore.BackgroundRun, error) {
	return store.run, nil
}

func (store *resultProjectionStore) GetBackgroundRunResult(context.Context, task.WorkspaceID, task.RunID, task.ActorSnapshot) (taskstore.BackgroundRunResultProjection, error) {
	return store.projection, nil
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
	operator := fixture.withActor(t, task.ActorSnapshot{Type: task.ActorOperator, ID: "operator", DisplayName: "Operator",
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
	device := fixture.withActor(t, task.ActorSnapshot{Type: task.ActorDevice, ID: "dev_1", DisplayName: "Phone",
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
	fixture := newAPIFixture(t)
	tests := []struct {
		name, body, key string
		status          int
	}{
		{"remote", strings.Replace(validCreateBody("Work"), "owner/repository", "owner/other", 1), "remote", 400},
		{"base", strings.Replace(validCreateBody("Work"), string(testBase), "ABC", 1), "base", 400},
		{"profile", strings.Replace(validCreateBody("Work"), rundomain.SourceProfile, "opencode-latest", 1), "profile", 400},
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
	failedGenerator, err := task.NewGenerator(strings.NewReader(""), time.Now)
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
		runs, err := fixture.store.ListBackgroundRuns(context.Background(), testWorkspace, fixture.actor, 10)
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
		RunID task.RunID `json:"run_id"`
	}
	if created.Code != http.StatusAccepted || json.Unmarshal(created.Body.Bytes(), &response) != nil || wakes.Load() != 1 {
		t.Fatalf("create status=%d wakes=%d body=%s", created.Code, wakes.Load(), created.Body.String())
	}
	if replay := fixture.request(http.MethodPost, PathPrefix, validCreateBody("Work"), "create-wake"); replay.Code != http.StatusAccepted || wakes.Load() != 1 {
		t.Fatalf("create replay status=%d wakes=%d", replay.Code, wakes.Load())
	}
	fixture.handler.config.Wake = func() {
		wakes.Add(1)
		run, err := fixture.store.GetBackgroundRun(context.Background(), testWorkspace, response.RunID, fixture.actor)
		if err != nil || (run.State != rundomain.Canceling && run.State != rundomain.Failed) {
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
		RunID task.RunID `json:"run_id"`
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
	ids := task.NewSecureGenerator()
	runID, _ := ids.RunID()
	resultID, _ := ids.ResultID()
	changes := sha256.Sum256([]byte("changes"))
	manifest := sha256.Sum256([]byte("artifact manifest"))
	bundle := sha256.Sum256([]byte("bundle"))
	run := taskstore.BackgroundRun{RunID: runID, WorkspaceID: testWorkspace,
		RepositoryRemote: "https://github.com/owner/repository", State: rundomain.ResultReady,
		EffectPhase: rundomain.CleanupComplete, Seal: &taskstore.Seal{ResultID: resultID}}
	projection := taskstore.BackgroundRunResultProjection{Run: run,
		Result: taskstore.Result{ID: resultID, RunID: runID, State: taskstore.ResultSealed, Outcome: task.ResultChanged, BaseSHA: testBase,
			ResultCommit: task.GitOID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), TreeOID: task.GitOID("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
			ChangeCount: 2, ChangesSHA256: changes, ManifestSHA256: manifest, BundleSHA256: bundle, BundleBytes: 1234}}
	store := &resultProjectionStore{Store: fixture.store, run: run, projection: projection}
	fixture.handler.config.Store = store
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
	store.projection.Result.BundleSHA256 = sha256.Sum256([]byte("other bundle"))
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

	store.run.State = rundomain.Canceling
	store.run.EffectPhase = rundomain.Sealing
	store.run.LastError = "retained artifact export retry required"
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
	store, err := taskstore.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	if err := store.CreateWorkspace(context.Background(), taskstore.Workspace{ID: testWorkspace, Name: "demo", State: taskstore.WorkspaceActive, RepositoryPath: "/srv/repo", GitHubAuthority: taskstore.GitHubAuthorityAppBroker, InstallationID: 1, RepositoryID: 99, RepositoryFullName: "owner/repository", ImageDigest: "sha256:image", OpenCodeProtocol: "0.0.0-next-17444", RuntimeDesiredState: "running", ReconciliationEpoch: 1, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	fixture := &apiFixture{store: store, actor: pluginActor("pc_owner"), verifier: &countingVerifier{}, retained: &retentionVerifier{}, route: &fakeRoute{}, path: path, now: now}
	t.Cleanup(func() { _ = store.Close() })
	fixture.handler = fixture.buildHandler(t)
	return fixture
}
func (f *apiFixture) buildHandler(t *testing.T) *Handler {
	t.Helper()
	backgroundImage := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	handler, err := New(Config{WorkspaceID: testWorkspace, RepositoryID: 99, RepositoryRemote: "https://github.com/owner/repository", BackgroundImageIdentity: backgroundImage, BackgroundEnvironmentSHA256: sha256.Sum256([]byte("{}")), Store: f.store, Route: f.route, Generator: task.NewSecureGenerator(), ActorResolver: func(context.Context) (task.ActorSnapshot, error) { return f.actor, nil }, BaseVerifier: f.verifier, RetentionVerifier: f.retained, Now: func() time.Time { return f.now }, RunTimeout: time.Hour, Agent: "build", ModelProvider: "test", Model: "model", SealPolicyVersion: "fern.background-user-seal.v1"})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func (f *apiFixture) advanceToSession(t *testing.T, id task.RunID) taskstore.BackgroundRun {
	t.Helper()
	now := time.Date(2026, 8, 31, 12, 0, 1, 0, time.UTC)
	queued, err := f.store.NextBackgroundRun(context.Background(), testWorkspace, rundomain.SourceProfile)
	if err != nil || queued.RunID != id {
		t.Fatalf("next run=%+v error=%v", queued, err)
	}
	run, err := f.store.StartBackgroundRunProvisioning(context.Background(), openTestRef(queued, now))
	if err != nil {
		t.Fatalf("start run=%+v error=%v", run, err)
	}
	started := time.Date(2026, 8, 31, 12, 0, 2, 123456789, time.UTC)
	now = now.Add(time.Millisecond)
	run, err = f.store.RecordBackgroundRunRuntime(context.Background(), taskstore.RecordBackgroundRunRuntimeParams{
		BackgroundRunRef: openTestRef(run, now), ContainerID: strings.Repeat("a", 64), ContainerStartedAt: started.Format(time.RFC3339Nano),
		RuntimeEpoch: started.UnixNano(), HostPort: 49152, Evidence: `{"status":"exact"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Millisecond)
	run, err = f.store.RecordBackgroundRunPromptRequestAttempted(context.Background(), openTestRef(run, now))
	if err != nil {
		t.Fatal(err)
	}
	f.now = now.Add(time.Second)
	return run
}

func (f *apiFixture) advanceToPrompt(t *testing.T, id task.RunID) taskstore.BackgroundRun {
	run := f.advanceToSession(t, id)
	now := run.UpdatedAt.Add(time.Millisecond)
	var err error
	run, err = f.store.RecordBackgroundRunPromptAdmitted(context.Background(), taskstore.RecordBackgroundRunEvidenceParams{
		BackgroundRunRef: openTestRef(run, now), Evidence: `{"status":"exact"}`})
	if err != nil {
		t.Fatal(err)
	}
	f.now = now.Add(time.Second)
	return run
}

func openTestRef(run taskstore.BackgroundRun, now time.Time) taskstore.BackgroundRunRef {
	return taskstore.BackgroundRunRef{WorkspaceID: run.WorkspaceID, RunID: run.RunID,
		ExpectedRevision: run.Revision, ExpectedState: run.State, ExpectedPhase: run.EffectPhase, Now: now}
}
func (f *apiFixture) withActor(t *testing.T, actor task.ActorSnapshot) *apiFixture {
	clone := *f
	clone.actor = actor
	clone.handler = clone.buildHandler(t)
	return &clone
}
func (f *apiFixture) withStore(t *testing.T, store *taskstore.Store) *apiFixture {
	clone := *f
	clone.store = store
	clone.handler = clone.buildHandler(t)
	return &clone
}
func pluginActor(id string) task.ActorSnapshot {
	return task.ActorSnapshot{Type: task.ActorOpenCode, ID: id, DisplayName: "OpenCode plugin", CredentialID: id, Authentication: "fern_plugin_bearer", RequestID: "request"}
}
func validCreateBody(instruction string) string {
	value, _ := json.Marshal(createInput{Repository: "https://github.com/owner/repository", BaseOID: string(testBase), Branch: stringPointer("main"), Instruction: instruction, Profile: rundomain.SourceProfile})
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

func TestRunAPIAttachIssuesOnlyForReadyActiveRoute(t *testing.T) {
	fixture := newAPIFixture(t)
	created := fixture.request(http.MethodPost, PathPrefix, validCreateBody("attach to this"), "attach-create")
	var admission struct {
		RunID task.RunID `json:"run_id"`
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
	operator := fixture.withActor(t, task.ActorSnapshot{Type: task.ActorOperator, ID: "operator", DisplayName: "Operator",
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
