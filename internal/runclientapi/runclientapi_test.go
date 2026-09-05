package runclientapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/backgroundroute"
	"github.com/nebler/fern/internal/pluginauth"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	testWorkspace = task.WorkspaceID("wsp_0198d34d-6a50-75fb-b1f2-000000000001")
	testRun       = task.TaskID("tsk_0198d34d-6a50-75fb-b1f2-000000000201")
)

type fakeStore struct{ run taskstore.BackgroundRun }

func (store *fakeStore) GetBackgroundRun(_ context.Context, workspace task.WorkspaceID, run task.TaskID, _ task.ActorSnapshot) (taskstore.BackgroundRun, error) {
	if workspace != testWorkspace || run != testRun {
		return taskstore.BackgroundRun{}, taskstore.ErrNotFound
	}
	return store.run, nil
}

func (store *fakeStore) ListBackgroundRuns(_ context.Context, workspace task.WorkspaceID, _ task.ActorSnapshot, _ int) ([]taskstore.BackgroundRun, error) {
	if workspace != testWorkspace {
		return nil, taskstore.ErrNotFound
	}
	return []taskstore.BackgroundRun{store.run}, nil
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

func TestOperatorListsAndAttachesThroughSeparateClientSurface(t *testing.T) {
	now := time.Now().UTC()
	run := taskstore.BackgroundRun{WorkspaceID: testWorkspace, TaskID: testRun, State: taskstore.BackgroundRunWorking,
		EffectPhase: taskstore.BackgroundRunEffectPromptAdmitted, RepositoryRemote: "https://github.com/owner/repository",
		BaseOID: task.GitOID(strings.Repeat("a", 40)), OpenCodeSessionID: task.OpenCodeSessionID("ses_0123456789abcdef0123456789abcdef"), SessionObservedAt: &now}
	route := &fakeRoute{active: true}
	handler, err := New(Config{WorkspaceID: testWorkspace, Store: &fakeStore{run: run}, Route: route})
	if err != nil {
		t.Fatal(err)
	}
	actor := task.ActorSnapshot{Type: task.ActorOperator, ID: "operator", DisplayName: "Operator",
		CredentialID: "operator", Authentication: "basic", RequestID: "request"}
	request := func(path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		value := httptest.NewRequest(http.MethodGet, path, nil)
		value = value.WithContext(task.WithActor(value.Context(), actor))
		handler.ServeHTTP(recorder, value)
		return recorder
	}
	if response := request(PathPrefix); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), string(testRun)) ||
		!strings.Contains(response.Body.String(), `"attachable":true`) {
		t.Fatalf("list=%d %s", response.Code, response.Body.String())
	}
	response := request(PathPrefix + "/" + string(testRun) + "/attach")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"session_id":"ses_0123456789abcdef0123456789abcdef"`) ||
		!strings.Contains(response.Body.String(), `"password":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`) || route.calls != 1 {
		t.Fatalf("attach=%d calls=%d %s", response.Code, route.calls, response.Body.String())
	}
}

func TestPluginClientRequiresIngressIdentityAndFixedScopes(t *testing.T) {
	store := &fakeStore{run: taskstore.BackgroundRun{WorkspaceID: testWorkspace, TaskID: testRun}}
	handler, err := New(Config{WorkspaceID: testWorkspace, Store: store, Route: &fakeRoute{}})
	if err != nil {
		t.Fatal(err)
	}
	credential := pluginauth.Credential{ID: "pc_client", State: pluginauth.Active, ExpiresAt: time.Now().Add(time.Hour)}
	actor := task.ActorSnapshot{Type: task.ActorOpenCode, ID: credential.ID, DisplayName: "OpenCode plugin",
		CredentialID: credential.ID, Authentication: "fern_plugin_bearer", RequestID: "request"}
	value := httptest.NewRequest(http.MethodGet, PathPrefix, nil)
	ctx := task.WithActor(value.Context(), actor)
	ctx = pluginauth.WithRequestAuthorization(ctx, credential)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, value.WithContext(ctx))
	if recorder.Code != http.StatusOK {
		t.Fatalf("plugin list=%d %s", recorder.Code, recorder.Body.String())
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, value.WithContext(task.WithActor(value.Context(), actor)))
	if missing.Code != http.StatusForbidden {
		t.Fatalf("plugin without authorization=%d %s", missing.Code, missing.Body.String())
	}
}
