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

type retentionVerifier struct {
	calls atomic.Int64
	err   error
}

func (v *retentionVerifier) Verify(context.Context, taskstore.Result) error {
	v.calls.Add(1)
	return v.err
}

type apiFixture struct {
	store    *taskstore.Store
	handler  *Handler
	actor    task.ActorSnapshot
	verifier *countingVerifier
	retained *retentionVerifier
	path     string
	now      time.Time
}

type resultProjectionStore struct {
	*taskstore.Store
	run        taskstore.BackgroundRun
	projection taskstore.BackgroundRunResultProjection
	export     taskstore.BackgroundRunExport
}

func (store *resultProjectionStore) GetBackgroundRun(context.Context, task.WorkspaceID, task.TaskID, task.ActorSnapshot) (taskstore.BackgroundRun, error) {
	return store.run, nil
}

func (store *resultProjectionStore) GetBackgroundRunResult(context.Context, task.WorkspaceID, task.TaskID, task.ActorSnapshot) (taskstore.BackgroundRunResultProjection, error) {
	return store.projection, nil
}

func (store *resultProjectionStore) GetBackgroundRunExport(context.Context, task.ArtifactExportID) (taskstore.BackgroundRunExport, error) {
	return store.export, nil
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
	operator := fixture.withActor(t, task.ActorSnapshot{Type: task.ActorOperator, ID: "operator", DisplayName: "Operator",
		CredentialID: "operator", Authentication: "basic", RequestID: "request"})
	if got := operator.request(http.MethodGet, PathPrefix, "", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("operator reached plugin-only run API = %d %s", got.Code, got.Body.String())
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

func TestRunAPIWakeFollowsDurableCreateAndStopCommitOnly(t *testing.T) {
	fixture := newAPIFixture(t, PluginOpenCodeProfile)
	var wakes atomic.Int64
	fixture.handler.config.Wake = func() {
		wakes.Add(1)
		runs, err := fixture.store.ListBackgroundRuns(context.Background(), testWorkspace, fixture.actor, 10)
		if err != nil || len(runs) != 1 {
			t.Errorf("wake could not observe committed run: runs=%d error=%v", len(runs), err)
		}
	}
	if got := fixture.request(http.MethodPost, PathPrefix, strings.Replace(validCreateBody("Work"), "owner/repository", "owner/other", 1), "invalid"); got.Code != http.StatusBadRequest || wakes.Load() != 0 {
		t.Fatalf("invalid create status=%d wakes=%d", got.Code, wakes.Load())
	}
	created := fixture.request(http.MethodPost, PathPrefix, validCreateBody("Work"), "create-wake")
	var response struct {
		RunID task.TaskID `json:"run_id"`
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
		if err != nil || (run.State != taskstore.BackgroundRunCanceling && run.State != taskstore.BackgroundRunFailed) {
			t.Errorf("stop wake could not observe committed state: run=%+v error=%v", run, err)
		}
	}
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
	fixture := newAPIFixture(t, PluginOpenCodeProfile)
	created := fixture.request(http.MethodPost, PathPrefix, validCreateBody("seal this run"), "seal-create")
	if created.Code != http.StatusAccepted {
		t.Fatalf("create = %d: %s", created.Code, created.Body.String())
	}
	var admission struct {
		RunID task.TaskID `json:"run_id"`
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
		first.ResultPhase != "seal_requested" || first.SealRequestID == "" || !first.Committed {
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
	fixture := newAPIFixture(t, PluginOpenCodeProfile)
	ids := task.NewSecureGenerator()
	runID, _ := ids.TaskID()
	attemptID, _ := ids.AttemptID()
	resultID, _ := ids.ResultID()
	artifactID, _ := ids.RetainedArtifactID()
	materializationID, _ := ids.MaterializationID()
	exportID, _ := ids.ArtifactExportID()
	changes := sha256.Sum256([]byte("changes"))
	manifest := sha256.Sum256([]byte("artifact manifest"))
	bundle := sha256.Sum256([]byte("bundle"))
	run := taskstore.BackgroundRun{TaskID: runID, AttemptID: attemptID, WorkspaceID: testWorkspace,
		RepositoryRemote: "https://github.com/owner/repository", State: taskstore.BackgroundRunResultReady,
		EffectPhase: taskstore.BackgroundRunEffectCleanupComplete, RetainedResultID: resultID,
		RetainedArtifactID: artifactID, MaterializationID: materializationID, ArtifactExportID: exportID}
	projection := taskstore.BackgroundRunResultProjection{Run: run,
		Result: taskstore.Result{ID: resultID, Outcome: task.ResultChanged, BaseSHA: testBase,
			ResultCommit: task.GitOID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), TreeOID: task.GitOID("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
			ManifestEntries: 2, ManifestSHA256: changes},
		Artifact:        taskstore.RetainedArtifact{ID: artifactID, ManifestSHA256: manifest, BundleSHA256: bundle, BundleBytes: 1234},
		Materialization: taskstore.ArtifactMaterialization{ID: materializationID, State: taskstore.ArtifactMaterializationReady}}
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
	fixture.retained.err = errors.New("artifact missing")
	missing := fixture.request(http.MethodGet, PathPrefix+"/"+string(runID)+"/result", "", "")
	if missing.Code != http.StatusOK {
		t.Fatalf("missing artifact result=%d %s", missing.Code, missing.Body.String())
	}
	var missingBody map[string]any
	if json.Unmarshal(missing.Body.Bytes(), &missingBody) != nil {
		t.Fatal("decode missing artifact result")
	}
	if retention := missingBody["retention"].(map[string]any); retention["verified"] != false || retention["reconstructable"] != false || fixture.retained.calls.Load() != 2 {
		t.Fatalf("missing retention projection=%v calls=%d", retention, fixture.retained.calls.Load())
	}

	store.run.State = taskstore.BackgroundRunCleanupRequired
	store.run.EffectPhase = taskstore.BackgroundRunEffectExporting
	store.export = taskstore.BackgroundRunExport{State: taskstore.BackgroundRunExportRecoveryRequired}
	recovery := fixture.request(http.MethodGet, PathPrefix+"/"+string(runID)+"/result", "", "")
	if recovery.Code != http.StatusServiceUnavailable || !strings.Contains(recovery.Body.String(), "recovery_required") {
		t.Fatalf("recovery result=%d %s", recovery.Code, recovery.Body.String())
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
	fixture := &apiFixture{store: store, actor: pluginActor("pc_owner"), verifier: &countingVerifier{}, retained: &retentionVerifier{}, path: path, now: now}
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
	handler, err := New(Config{WorkspaceID: testWorkspace, RepositoryID: 99, RepositoryRemote: "https://github.com/owner/repository", BackgroundImageIdentity: backgroundImage, BackgroundEnvironmentSHA256: sha256.Sum256([]byte("{}")), AvailableProfile: available, Store: f.store, Generator: task.NewSecureGenerator(), ActorResolver: func(context.Context) (task.ActorSnapshot, error) { return f.actor, nil }, BaseVerifier: f.verifier, RetentionVerifier: f.retained, Now: func() time.Time { return f.now }, AttemptTimeout: time.Hour, Agent: "build", ModelProvider: "test", Model: "model", BudgetSnapshot: json.RawMessage(`{"turns":10}`), SealPolicyVersion: "fern.background-user-seal.v1"})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func (f *apiFixture) advanceToSession(t *testing.T, id task.TaskID) taskstore.BackgroundRun {
	t.Helper()
	now := time.Date(2026, 8, 31, 12, 0, 1, 0, time.UTC)
	work, err := f.store.ClaimNextBackgroundRunWork(context.Background(), taskstore.ClaimNextBackgroundRunParams{WorkspaceID: testWorkspace,
		ClaimOwner: "test-worker", Now: now, LeaseDuration: time.Minute, Profile: PluginOpenCodeProfile,
		ImageIdentity: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"})
	if err != nil || work.Run.TaskID != id {
		t.Fatalf("claim run=%+v error=%v", work.Run, err)
	}
	run := work.Run
	next := func(transition func(context.Context, taskstore.RecordBackgroundRunEvidenceParams) (taskstore.BackgroundRun, error)) {
		now = now.Add(time.Millisecond)
		run, err = transition(context.Background(), taskstore.RecordBackgroundRunEvidenceParams{BackgroundRunClaim: openTestClaim(run, now), Evidence: `{"status":"exact"}`})
		if err != nil {
			t.Fatal(err)
		}
	}
	next(f.store.RecordBackgroundRunCloneObserved)
	next(f.store.RecordBackgroundRunVolumeObserved)
	started := time.Date(2026, 8, 31, 12, 0, 2, 123456789, time.UTC)
	now = now.Add(time.Millisecond)
	run, err = f.store.RecordBackgroundRunContainerObserved(context.Background(), taskstore.RecordBackgroundRunContainerObservedParams{
		BackgroundRunClaim: openTestClaim(run, now), ContainerID: strings.Repeat("a", 64), ContainerStartedAt: started.Format(time.RFC3339Nano),
		RuntimeEpoch: started.UnixNano(), HostPort: 49152, Evidence: `{"status":"exact"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	next(f.store.RecordBackgroundRunHealthObserved)
	next(f.store.RecordBackgroundRunReady)
	next(f.store.RecordBackgroundRunSessionObserved)
	f.now = now.Add(time.Second)
	return run
}

func (f *apiFixture) advanceToPrompt(t *testing.T, id task.TaskID) taskstore.BackgroundRun {
	run := f.advanceToSession(t, id)
	now := run.UpdatedAt.Add(time.Millisecond)
	var err error
	run, err = f.store.RecordBackgroundRunPromptIntent(context.Background(), taskstore.RecordBackgroundRunEvidenceParams{
		BackgroundRunClaim: openTestClaim(run, now), Evidence: `{"status":"committed"}`})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Millisecond)
	run, err = f.store.RecordBackgroundRunPromptRequestAttempted(context.Background(), openTestClaim(run, now))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Millisecond)
	run, err = f.store.RecordBackgroundRunPromptAdmitted(context.Background(), taskstore.RecordBackgroundRunEvidenceParams{
		BackgroundRunClaim: openTestClaim(run, now), Evidence: `{"status":"exact"}`})
	if err != nil {
		t.Fatal(err)
	}
	f.now = now.Add(time.Second)
	return run
}

func openTestClaim(run taskstore.BackgroundRun, now time.Time) taskstore.BackgroundRunClaim {
	return taskstore.BackgroundRunClaim{WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID, Generation: run.Generation,
		ClaimOwner: run.ClaimOwner, ClaimGeneration: run.ClaimGeneration, ExpectedRevision: run.Revision, ExpectedState: run.State,
		ExpectedPhase: run.EffectPhase, CancelEpoch: run.CancelEpoch, Now: now}
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
