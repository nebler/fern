package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestFixtureStateVocabulary(t *testing.T) {
	wantTaskStates := []string{"cancel_requested", "canceled", "completed", "failed", "input_required", "queued", "recovery_required", "running", "uncertain"}
	wantPublicationStates := []string{"cancel_requested", "canceled", "conflict", "failed", "opening_pr", "preparing", "published", "pushing", "ready", "reconciling", "recovery_required", "requested", "uncertain"}
	if got := sortedKeys(taskStates); !reflect.DeepEqual(got, wantTaskStates) {
		t.Fatalf("task state vocabulary = %v, want %v", got, wantTaskStates)
	}
	if got := sortedKeys(publicationStates); !reflect.DeepEqual(got, wantPublicationStates) {
		t.Fatalf("publication state vocabulary = %v, want %v", got, wantPublicationStates)
	}

	fixtures, err := loadFixtures(fixtureFiles, "fixtures/tasks.json")
	if err != nil {
		t.Fatal(err)
	}
	requiredTasks := map[string]bool{
		"queued": false, "running": false, "input_required": false,
		"cancel_requested": false, "uncertain": false,
		"recovery_required": false, "completed": false,
	}
	requiredPublications := map[string]bool{
		"requested": false, "pushing": false, "reconciling": false,
		"published": false, "conflict": false,
	}
	for _, task := range fixtures.Tasks {
		if _, ok := requiredTasks[task.State]; ok {
			requiredTasks[task.State] = true
		}
		if task.Publication != nil {
			if _, ok := requiredPublications[task.Publication.State]; ok {
				requiredPublications[task.Publication.State] = true
			}
		}
	}
	for state, found := range requiredTasks {
		if !found {
			t.Errorf("fixtures do not cover task state %q", state)
		}
	}
	for state, found := range requiredPublications {
		if !found {
			t.Errorf("fixtures do not cover publication state %q", state)
		}
	}
}

func TestRoutesRenderInboxDetailsAndExactOpenCodeLinks(t *testing.T) {
	handler, err := newHandler()
	if err != nil {
		t.Fatal(err)
	}
	fixtures, err := loadFixtures(fixtureFiles, "fixtures/tasks.json")
	if err != nil {
		t.Fatal(err)
	}

	inbox := request(t, handler, http.MethodGet, "/")
	if inbox.Code != http.StatusOK {
		t.Fatalf("inbox status = %d", inbox.Code)
	}
	if got := inbox.Header().Get("Content-Security-Policy"); !strings.Contains(got, "form-action 'none'") {
		t.Errorf("missing read-only CSP: %q", got)
	}
	for _, task := range fixtures.Tasks {
		if !strings.Contains(inbox.Body.String(), "/tasks/"+task.ID) {
			t.Errorf("inbox missing detail link for %s", task.ID)
		}
		detail := request(t, handler, http.MethodGet, "/tasks/"+task.ID)
		if detail.Code != http.StatusOK {
			t.Errorf("detail %s status = %d", task.ID, detail.Code)
			continue
		}
		body := detail.Body.String()
		if !strings.Contains(body, `href="`+task.Links.OpenCode+`"`) {
			t.Errorf("detail %s missing exact OpenCode link %q", task.ID, task.Links.OpenCode)
		}
		if strings.Contains(body, "transcript-pane") || strings.Contains(body, "diff-viewer") {
			t.Errorf("detail %s rendered an OpenCode-owned surface", task.ID)
		}
		if task.Result != nil {
			for _, file := range task.Result.ChangedFiles {
				if strings.Contains(body, file.Path) {
					t.Errorf("detail %s reproduced OpenCode-owned file path %q", task.ID, file.Path)
				}
			}
		}
	}
}

func TestPreviewRejectsMutationMethods(t *testing.T) {
	handler, err := newHandler()
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, handler, http.MethodPost, "/tasks/anything/cancel")
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
	if got := response.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q", got)
	}
}

func TestTemplateEscapesUntrustedDisplayText(t *testing.T) {
	fixtures, err := loadFixtures(fixtureFiles, "fixtures/tasks.json")
	if err != nil {
		t.Fatal(err)
	}
	a, err := buildApp(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	task := a.fixtures.Tasks[0]
	task.Title = `<script>alert("fixture")</script>`
	data := pageData{Workspace: a.fixtures.Workspace, Tasks: []Task{task}, Task: &task}
	var rendered bytes.Buffer
	if err := a.template.ExecuteTemplate(&rendered, "detail", data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered.String(), "<script>") {
		t.Fatal("untrusted title was rendered as executable markup")
	}
	if !strings.Contains(rendered.String(), "&lt;script&gt;") {
		t.Fatal("escaped title is missing")
	}
}

func TestFixtureValidationRejectsUnsafeOpenCodeLink(t *testing.T) {
	set, err := loadFixtures(fixtureFiles, "fixtures/tasks.json")
	if err != nil {
		t.Fatal(err)
	}
	set.Tasks[0].Links.OpenCode = "javascript:alert(1)"
	if err := set.validate(); err == nil {
		t.Fatal("unsafe OpenCode URL passed validation")
	}
}

func TestFixtureValidationRejectsUnsafePullRequestLink(t *testing.T) {
	fixtures, err := loadFixtures(fixtureFiles, "fixtures/tasks.json")
	if err != nil {
		t.Fatal(err)
	}
	for index := range fixtures.Tasks {
		if fixtures.Tasks[index].Publication != nil && fixtures.Tasks[index].Publication.PullRequest != nil {
			fixtures.Tasks[index].Publication.PullRequest.URL = "javascript:alert(1)"
			if err := fixtures.validate(); err == nil {
				t.Fatal("fixture validation accepted an unsafe pull request link")
			}
			return
		}
	}
	t.Fatal("fixtures contain no published pull request")
}

func TestStaticAssetIsServedWithNosniff(t *testing.T) {
	handler, err := newHandler()
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, handler, http.MethodGet, "/assets/style.css")
	if response.Code != http.StatusOK {
		t.Fatalf("asset status = %d", response.Code)
	}
	if response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("asset response is missing nosniff")
	}
}

func TestFaviconIsIntentionallyEmpty(t *testing.T) {
	handler, err := newHandler()
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func request(t *testing.T, handler http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, target, nil))
	return response
}
