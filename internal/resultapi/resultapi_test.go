package resultapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	testWorkspace      = task.WorkspaceID("wsp_0198d34d-5e40-7c5a-8e3f-6bfad471ae12")
	testOtherWorkspace = task.WorkspaceID("wsp_0198d34d-5e40-7c5a-8e3f-6bfad471ae13")
	testTask           = task.TaskID("tsk_0198d34d-6a50-75fb-b1f2-b4a14d70ec55")
	testAttempt        = task.AttemptID("att_0198d34d-6a50-75fb-b1f2-b4a14d70ec56")
	testResult         = task.ResultID("res_0198d34d-6a50-75fb-b1f2-b4a14d70ec59")
	testVerification   = task.VerificationID("ver_0198d34d-6a50-75fb-b1f2-b4a14d70ec60")
	testSHA            = task.GitOID("0123456789abcdef0123456789abcdef01234567")
)

var testNow = time.Date(2026, 8, 22, 18, 57, 11, 565000000, time.UTC)

type fakeStore struct {
	owners    func(context.Context, task.ResultID) (taskstore.Result, taskstore.Task, taskstore.Attempt, error)
	authority func(context.Context, task.ResultID) (bool, error)
	admit     func(context.Context, taskstore.AdmitPublicationParams) (taskstore.PublicationAdmission, error)
}

func (s *fakeStore) GetResultOwners(ctx context.Context, id task.ResultID) (taskstore.Result, taskstore.Task, taskstore.Attempt, error) {
	if s.owners == nil {
		return taskstore.Result{}, taskstore.Task{}, taskstore.Attempt{}, errors.New("unexpected GetResultOwners")
	}
	return s.owners(ctx, id)
}

func (s *fakeStore) AdmitPublication(ctx context.Context, p taskstore.AdmitPublicationParams) (taskstore.PublicationAdmission, error) {
	if s.admit == nil {
		return taskstore.PublicationAdmission{}, errors.New("unexpected AdmitPublication")
	}
	return s.admit(ctx, p)
}

func (s *fakeStore) HasRetainedResultAuthority(ctx context.Context, id task.ResultID) (bool, error) {
	if s.authority == nil {
		return true, nil
	}
	return s.authority(ctx, id)
}

func actor(kind task.ActorType) task.ActorSnapshot {
	return task.ActorSnapshot{Type: kind, ID: "principal", DisplayName: "Principal", CredentialID: "credential-v1", Authentication: "test", RequestID: "request-1"}
}

func owners(workspace task.WorkspaceID) (taskstore.Result, taskstore.Task, taskstore.Attempt) {
	result := taskstore.Result{ID: testResult, TaskID: testTask, AttemptID: testAttempt, WorkspaceID: workspace,
		State: task.ResultSealed, Outcome: task.ResultChanged, BaseSHA: testSHA, ResultCommit: testSHA,
		ManifestEntries: 2, ManifestSHA256: sha256.Sum256([]byte("manifest")), SealedAt: testNow}
	owner := taskstore.Task{ID: testTask, WorkspaceID: workspace, CurrentAttemptID: testAttempt, SealedResultID: testResult}
	attempt := taskstore.Attempt{ID: testAttempt, TaskID: testTask, WorkspaceID: workspace, SealedResultID: testResult}
	return result, owner, attempt
}

func testConfig(t *testing.T, store Store, kind task.ActorType) Config {
	t.Helper()
	generator, err := task.NewGenerator(bytes.NewReader(bytes.Repeat([]byte{0xa5}, 4096)), func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	return Config{WorkspaceID: testWorkspace, Store: store, Generator: generator,
		ActorResolver: func(context.Context) (task.ActorSnapshot, error) { return actor(kind), nil }, Wake: func() {},
		Now: func() time.Time { return testNow }, PublicationPolicyVersion: "publication.v1",
		PublicationPolicySHA256: sha256.Sum256([]byte("publication policy")), APIContractVersion: "fern.result.v1"}
}

func handler(t *testing.T, config Config) *Handler {
	t.Helper()
	h, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func request(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestGetResultUsesOwnedSafeProjection(t *testing.T) {
	result, owner, attempt := owners(testWorkspace)
	store := &fakeStore{owners: func(_ context.Context, id task.ResultID) (taskstore.Result, taskstore.Task, taskstore.Attempt, error) {
		if id != testResult {
			t.Fatalf("result ID = %s", id)
		}
		return result, owner, attempt, nil
	}}
	response := request(handler(t, testConfig(t, store, task.ActorDevice)), http.MethodGet, PathPrefix+string(testResult), "", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"`+string(testResult)+`"`) ||
		!strings.Contains(response.Body.String(), `"manifestSha256":"sha256:`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"principal", "credential-v1", string(testWorkspace), string(testTask), string(testAttempt)} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("response leaked %q: %s", secret, response.Body.String())
		}
	}
	for _, tc := range []struct {
		method, path, body string
		status             int
		allow              string
	}{
		{http.MethodPost, PathPrefix + string(testResult), "", 405, "GET"},
		{http.MethodGet, PathPrefix + string(testResult) + "?x=1", "", 400, ""},
		{http.MethodGet, PathPrefix + string(testResult), "{}", 400, ""},
		{http.MethodGet, PathPrefix + "bad", "", 404, ""},
	} {
		got := request(handler(t, testConfig(t, store, task.ActorOperator)), tc.method, tc.path, tc.body, nil)
		if got.Code != tc.status || got.Header().Get("Allow") != tc.allow {
			t.Errorf("%s %s = %d allow %q", tc.method, tc.path, got.Code, got.Header().Get("Allow"))
		}
	}
}

func TestOwnershipAndAuthenticationAreHidden(t *testing.T) {
	result, owner, attempt := owners(testOtherWorkspace)
	var admissions atomic.Int64
	store := &fakeStore{
		owners: func(context.Context, task.ResultID) (taskstore.Result, taskstore.Task, taskstore.Attempt, error) {
			return result, owner, attempt, nil
		},
		admit: func(context.Context, taskstore.AdmitPublicationParams) (taskstore.PublicationAdmission, error) {
			admissions.Add(1)
			return taskstore.PublicationAdmission{}, nil
		},
	}
	body := `{"expectedVerificationId":"` + string(testVerification) + `"}`
	headers := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "publish-1"}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		path := PathPrefix + string(testResult)
		if method == http.MethodPost {
			path += "/publications"
		}
		response := request(handler(t, testConfig(t, store, task.ActorOperator)), method, path, bodyFor(method, body), headers)
		if response.Code != http.StatusNotFound {
			t.Fatalf("cross-workspace %s = %d %s", method, response.Code, response.Body.String())
		}
	}
	if admissions.Load() != 0 {
		t.Fatalf("cross-workspace admissions = %d", admissions.Load())
	}

	for _, kind := range []task.ActorType{task.ActorOpenCode, task.ActorSystem, task.ActorGitHubApp} {
		response := request(handler(t, testConfig(t, store, kind)), http.MethodGet, PathPrefix+string(testResult), "", nil)
		if response.Code != http.StatusForbidden {
			t.Fatalf("actor %s = %d", kind, response.Code)
		}
	}
	config := testConfig(t, store, task.ActorDevice)
	config.ActorResolver = task.ContextActor
	if response := request(handler(t, config), http.MethodGet, PathPrefix+string(testResult), "", nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("missing context actor = %d", response.Code)
	}
	deviceRequest := httptest.NewRequest(http.MethodGet, PathPrefix+string(testResult), nil)
	deviceRequest = deviceRequest.WithContext(task.WithActor(deviceRequest.Context(), actor(task.ActorDevice)))
	recorder := httptest.NewRecorder()
	handler(t, config).ServeHTTP(recorder, deviceRequest)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("context device result = %d", recorder.Code)
	}
}

func TestUnavailableResultStateIsHidden(t *testing.T) {
	store := &fakeStore{owners: func(context.Context, task.ResultID) (taskstore.Result, taskstore.Task, taskstore.Attempt, error) {
		return taskstore.Result{}, taskstore.Task{}, taskstore.Attempt{}, taskstore.ErrInvalidState
	}}
	response := request(handler(t, testConfig(t, store, task.ActorOperator)), http.MethodGet, PathPrefix+string(testResult), "", nil)
	if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "state") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func bodyFor(method, body string) string {
	if method == http.MethodPost {
		return body
	}
	return ""
}

func TestPublicationIsStrictStableAndWakesOnlyFresh(t *testing.T) {
	result, owner, attempt := owners(testWorkspace)
	var calls, wakes atomic.Int64
	var first taskstore.PublicationAdmission
	store := &fakeStore{
		owners: func(context.Context, task.ResultID) (taskstore.Result, taskstore.Task, taskstore.Attempt, error) {
			return result, owner, attempt, nil
		},
		admit: func(_ context.Context, p taskstore.AdmitPublicationParams) (taskstore.PublicationAdmission, error) {
			if p.ResultID != testResult || p.VerificationID != testVerification || p.Claim.Actor.Type != task.ActorOperator ||
				p.Claim.Scope.WorkspaceID != testWorkspace || p.Claim.Scope.CommandKind != taskstore.PublishResultCommand ||
				p.BrokerPolicyVersion != "publication.v1" || p.APIContractVersion != "fern.result.v1" || p.AcceptedAt != testNow {
				t.Fatalf("params = %+v", p)
			}
			if calls.Add(1) == 1 {
				projection, _ := json.Marshal(struct {
					PublicationID  task.PublicationID  `json:"publicationId"`
					ResultID       task.ResultID       `json:"resultId"`
					VerificationID task.VerificationID `json:"verificationId"`
				}{p.PublicationID, p.ResultID, p.VerificationID})
				first = taskstore.PublicationAdmission{
					Publication: taskstore.Publication{ID: p.PublicationID, ResultID: p.ResultID, VerificationID: p.VerificationID,
						TaskID: testTask, AttemptID: testAttempt, WorkspaceID: testWorkspace, State: taskstore.PublicationPrepared},
					Receipt: taskstore.Receipt{ID: p.ReceiptID, WorkspaceID: testWorkspace, CommandKind: taskstore.PublishResultCommand,
						State: taskstore.ReceiptAccepted, AcceptedAt: testNow, TargetID: testTask, ResponseProjection: projection},
				}
				return first, nil
			}
			replay := first
			replay.Replayed = true
			replay.Publication.State = taskstore.PublicationPublished
			return replay, nil
		},
	}
	config := testConfig(t, store, task.ActorOperator)
	config.Wake = func() { wakes.Add(1) }
	h := handler(t, config)
	path := PathPrefix + string(testResult) + "/publications"
	body := `{"expectedVerificationId":"` + string(testVerification) + `"}`
	headers := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "publish-1"}
	fresh := request(h, http.MethodPost, path, body, headers)
	replay := request(h, http.MethodPost, path, body, headers)
	if fresh.Code != 202 || replay.Code != 202 || fresh.Body.String() != replay.Body.String() ||
		replay.Header().Get("Idempotency-Replayed") != "true" || wakes.Load() != 1 {
		t.Fatalf("fresh=%d %s replay=%d %s wakes=%d", fresh.Code, fresh.Body.String(), replay.Code, replay.Body.String(), wakes.Load())
	}
	for _, body := range []string{
		`{}`, `null`, `{"ExpectedVerificationId":"` + string(testVerification) + `"}`,
		`{"expectedVerificationId":"` + string(testVerification) + `","branch":"attacker"}`,
		`{"expectedVerificationId":"` + string(testVerification) + `","expectedVerificationId":"` + string(testVerification) + `"}`,
		`{"expectedVerificationId":"bad"}`, `{"expectedVerificationId":"\ud800"}`,
	} {
		got := request(h, http.MethodPost, path, body, map[string]string{"Content-Type": "application/json", "Idempotency-Key": "invalid"})
		if got.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d %s", body, got.Code, got.Body.String())
		}
	}
	for _, headers := range []map[string]string{
		{"Content-Type": "application/json"},
		{"Content-Type": "application/json; charset=utf-8", "Idempotency-Key": "x"},
		{"Content-Type": "application/json", "Idempotency-Key": " bad"},
	} {
		if got := request(h, http.MethodPost, path, body, headers); got.Code != http.StatusBadRequest {
			t.Errorf("headers %+v = %d", headers, got.Code)
		}
	}
	if got := request(h, http.MethodPost, path+"?branch=x", body, headers); got.Code != http.StatusBadRequest {
		t.Fatalf("query = %d", got.Code)
	}
	if got := request(h, http.MethodGet, path, "", nil); got.Code != http.StatusMethodNotAllowed || got.Header().Get("Allow") != "POST" {
		t.Fatalf("method = %d allow %q", got.Code, got.Header().Get("Allow"))
	}
}

func TestPublicationRejectsResultWithoutRetainedAuthority(t *testing.T) {
	result, owner, attempt := owners(testWorkspace)
	store := &fakeStore{
		owners: func(context.Context, task.ResultID) (taskstore.Result, taskstore.Task, taskstore.Attempt, error) {
			return result, owner, attempt, nil
		},
		authority: func(context.Context, task.ResultID) (bool, error) { return false, nil },
	}
	response := request(handler(t, testConfig(t, store, task.ActorDevice)), http.MethodPost,
		PathPrefix+string(testResult)+"/publications", `{"expectedVerificationId":"`+string(testVerification)+`"}`,
		map[string]string{"Content-Type": "application/json", "Idempotency-Key": "publish-1"})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "retained artifact authority") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestStoreFailuresDoNotWake(t *testing.T) {
	result, owner, attempt := owners(testWorkspace)
	var wakes atomic.Int64
	store := &fakeStore{
		owners: func(context.Context, task.ResultID) (taskstore.Result, taskstore.Task, taskstore.Attempt, error) {
			return result, owner, attempt, nil
		},
		admit: func(context.Context, taskstore.AdmitPublicationParams) (taskstore.PublicationAdmission, error) {
			return taskstore.PublicationAdmission{}, errors.New("database secret")
		},
	}
	config := testConfig(t, store, task.ActorDevice)
	config.Wake = func() { wakes.Add(1) }
	response := request(handler(t, config), http.MethodPost, PathPrefix+string(testResult)+"/publications",
		`{"expectedVerificationId":"`+string(testVerification)+`"}`,
		map[string]string{"Content-Type": "application/json", "Idempotency-Key": "publish-1"})
	if response.Code != 500 || wakes.Load() != 0 || strings.Contains(response.Body.String(), "database secret") {
		t.Fatalf("response=%d %s wakes=%d", response.Code, response.Body.String(), wakes.Load())
	}
}
