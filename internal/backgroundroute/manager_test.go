package backgroundroute

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestManagerAttachmentProxyHeadersPolicyAndSSE(t *testing.T) {
	observed := make(chan *http.Request, 8)
	releaseSSE := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		observed <- request.Clone(request.Context())
		if request.URL.Path == "/global/event" {
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(writer, `data: {"type":"server.connected"}`+"\n\n")
			writer.(http.Flusher).Flush()
			<-releaseSSE
			return
		}
		writer.Header().Add("Set-Cookie", "__Host-fern_device=stolen; Secure; Path=/")
		writer.Header().Add("Set-Cookie", "upstream=value; Path=/")
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	manager, origin, local := newTestManager(t)
	target, err := NewTarget(upstream.URL, roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		forward := request.Clone(request.Context())
		forward.Header = request.Header.Clone()
		forward.SetBasicAuth("opencode", "run-secret")
		return http.DefaultTransport.RoundTrip(forward)
	}))
	if err != nil {
		t.Fatal(err)
	}
	identity := testIdentity(1)
	if _, err := manager.Activate(identity, target); err != nil {
		t.Fatal(err)
	}
	attachment, active, err := manager.IssueAttachment(testRun(identity))
	if err != nil || !active {
		t.Fatalf("issue attachment active=%t error=%v", active, err)
	}
	token := attachment.Password

	unpaired, _ := http.NewRequest(http.MethodGet, local+"/", nil)
	if response, err := http.DefaultClient.Do(unpaired); err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unpaired response=%v error=%v", response, err)
	}

	request := pairedRequest(http.MethodPost, local+"/session/"+string(testRun(identity).OpenCodeSessionID)+"/message?x=1", token, origin, strings.NewReader("{}"))
	request.Header.Add("Cookie", "application=browser")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent || len(response.Cookies()) != 1 || response.Cookies()[0].Name != "upstream" {
		t.Fatalf("mutation status=%d cookies=%v", response.StatusCode, response.Cookies())
	}
	got := <-observed
	username, password, basic := got.BasicAuth()
	if !basic || username != "opencode" || password != "run-secret" || got.Header.Get("Cookie") != "" || got.URL.RequestURI() != "/session/"+string(testRun(identity).OpenCodeSessionID)+"/message?x=1" ||
		got.Host != strings.TrimPrefix(origin, "https://") || got.Header.Get("X-Forwarded-Host") != strings.TrimPrefix(origin, "https://") ||
		got.Header.Get("X-Forwarded-Proto") != "https" || got.Header.Get("X-Forwarded-Port") == "" {
		t.Fatalf("upstream request auth=%t user=%q password=%q host=%q headers=%v uri=%q", basic, username, password, got.Host, got.Header, got.URL.RequestURI())
	}

	fern := pairedRequest(http.MethodGet, local+"/fern/api/runs", token, origin, nil)
	if response, err := http.DefaultClient.Do(fern); err != nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("Fern route response=%v error=%v", response, err)
	}

	websocket := pairedRequest(http.MethodGet, local+"/socket", token, origin, nil)
	websocket.Header.Set("Connection", "Upgrade")
	websocket.Header.Set("Upgrade", "websocket")
	websocket.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	websocket.Header.Set("Sec-WebSocket-Version", "13")
	if response, err := http.DefaultClient.Do(websocket); err != nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("websocket shape response=%v error=%v", response, err)
	}
	crossSession := pairedRequest(http.MethodPost, local+"/session/ses_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/abort", token, origin, nil)
	if response, err := http.DefaultClient.Do(crossSession); err != nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-session response=%v error=%v", response, err)
	}

	events := pairedRequest(http.MethodGet, local+"/global/event", token, origin, nil)
	eventResponse, err := http.DefaultClient.Do(events)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(eventResponse.Body).ReadString('\n')
	if err != nil || line != `data: {"type":"server.connected"}`+"\n" {
		t.Fatalf("SSE first line=%q error=%v", line, err)
	}
	close(releaseSSE)
	_ = eventResponse.Body.Close()
}

func TestManagerRemovalFencesRebindAndRuntimeMismatch(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) }))
	defer upstream.Close()
	manager, _, local := newTestManager(t)
	target, err := NewTarget(upstream.URL, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	first, second := testIdentity(1), testIdentity(2)
	if _, err := manager.Activate(first, target); err != nil {
		t.Fatal(err)
	}
	attachment, active, err := manager.IssueAttachment(testRun(first))
	if err != nil || !active {
		t.Fatalf("issue attachment active=%t error=%v", active, err)
	}
	token := attachment.Password
	if _, err := manager.Activate(second, target); !errors.Is(err, ErrFenced) {
		t.Fatalf("replacement activation=%v", err)
	}
	if _, err := manager.Remove(context.Background(), second); !errors.Is(err, ErrMismatch) {
		t.Fatalf("wrong removal=%v", err)
	}
	if _, err := manager.Remove(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	request := pairedRequest(http.MethodGet, local+"/", token, "", nil)
	if response, err := http.DefaultClient.Do(request); err != nil || response.StatusCode != http.StatusNotFound || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("unbound response=%v error=%v", response, err)
	}
	if _, err := manager.Activate(second, target); !errors.Is(err, ErrFenced) {
		t.Fatalf("pre-commit rebind=%v", err)
	}
	if err := manager.ConfirmRemoval(first); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(second, target); err != nil {
		t.Fatal(err)
	}
	stale := pairedRequest(http.MethodGet, local+"/api/health", token, "", nil)
	if response, err := http.DefaultClient.Do(stale); err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stale capability response=%v error=%v", response, err)
	}
	run := testRun(second)
	if _, active := manager.ActiveOrigin(run); !active {
		t.Fatal("exact runtime was not active")
	}
	run.RuntimeEpoch++
	if _, active := manager.ActiveOrigin(run); active {
		t.Fatal("runtime epoch mismatch inherited route")
	}
}

func TestAttachmentCapabilityFencesSessionWriterAndExpiry(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) }))
	defer upstream.Close()
	manager, _, local := newTestManager(t)
	target, err := NewTarget(upstream.URL, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	identity := testIdentity(1)
	if _, err := manager.Activate(identity, target); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*taskstore.BackgroundRun){
		"writer":  func(run *taskstore.BackgroundRun) { run.WriterGeneration++ },
		"session": func(run *taskstore.BackgroundRun) { run.OpenCodeSessionID = "ses_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" },
	} {
		t.Run(name, func(t *testing.T) {
			run := testRun(identity)
			mutate(&run)
			if _, active, err := manager.IssueAttachment(run); err != nil || active {
				t.Fatalf("mismatched attachment active=%t error=%v", active, err)
			}
		})
	}
	attachment, active, err := manager.IssueAttachment(testRun(identity))
	if err != nil || !active {
		t.Fatalf("issue attachment active=%t error=%v", active, err)
	}
	digest := sha256.Sum256([]byte(attachment.Password))
	manager.mu.Lock()
	capability := manager.attachments[digest]
	capability.expiresAt = time.Now().Add(-time.Second)
	manager.attachments[digest] = capability
	manager.mu.Unlock()
	request := pairedRequest(http.MethodGet, local+"/api/health", attachment.Password, "", nil)
	if response, err := http.DefaultClient.Do(request); err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired capability response=%v error=%v", response, err)
	}
}

func TestAttachmentRequestPolicyAllowsTUIButRejectsManagementAndOtherSessions(t *testing.T) {
	session := "ses_0123456789abcdef0123456789abcdef"
	tests := []struct {
		method string
		path   string
		allow  bool
	}{
		{http.MethodGet, "/api/health", true},
		{http.MethodGet, "/api/session/" + session, true},
		{http.MethodGet, "/session?start=1&path=home%2Fuser%2Fworkspace", true},
		{http.MethodGet, "/api/session?parentID=" + session, true},
		{http.MethodGet, "/api/session?parentID=ses_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},
		{http.MethodGet, "/file/content?path=README.md", true},
		{http.MethodGet, "/file/content?path=../../etc/passwd", false},
		{http.MethodGet, "/api/provider?location%5Bdirectory%5D=%2Fhome%2Fuser%2Fworkspace", true},
		{http.MethodGet, "/api/provider?location%5Bdirectory%5D=%2Fetc", false},
		{http.MethodGet, "/api/provider?workspace=wsp_other", false},
		{http.MethodGet, "/api/fs/list?path=/etc", false},
		{http.MethodPost, "/api/session/" + session + "/prompt", true},
		{http.MethodPost, "/api/session/" + session + "/interrupt", true},
		{http.MethodPost, "/api/session/" + session + "/question/qst_1/reply", true},
		{http.MethodPost, "/session/" + session + "/message", true},
		{http.MethodPost, "/api/session", false},
		{http.MethodDelete, "/session/" + session, false},
		{http.MethodPost, "/api/workspace", false},
		{http.MethodPatch, "/api/credential/cred_1", false},
		{http.MethodPost, "/api/session/ses_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/prompt", false},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, "https://fern.example"+test.path, nil)
		if got := attachmentRequestAllowed(request, session); got != test.allow {
			t.Errorf("%s %s allowed=%t, want %t", test.method, test.path, got, test.allow)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "https://fern.example/api/provider", nil)
	request.Header.Set("X-OpenCode-Directory", "/etc")
	if attachmentRequestAllowed(request, session) {
		t.Fatal("attachment accepted a different OpenCode directory")
	}
}

func TestAttachmentResponseFiltersSessionListsStatusesAndEvents(t *testing.T) {
	session := "ses_0123456789abcdef0123456789abcdef"
	other := "ses_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	filter := func(path, body string) string {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "https://fern.example"+path, nil)
		response := &http.Response{Request: request, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
		if err := filterAttachmentResponse(response, session); err != nil {
			t.Fatal(err)
		}
		filtered, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(filtered)
	}
	list := filter("/session", `[{"id":"`+other+`"},{"id":"`+session+`"}]`)
	if strings.Contains(list, other) || !strings.Contains(list, session) {
		t.Fatalf("session list=%s", list)
	}
	status := filter("/session/status", `{"`+other+`":{"type":"busy"},"`+session+`":{"type":"idle"}}`)
	if strings.Contains(status, other) || !strings.Contains(status, session) {
		t.Fatalf("session status=%s", status)
	}
	events := "data: {\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"" + other + "\"}}\n\n" +
		"data: {\"type\":\"message.updated\",\"properties\":{}}\n\n" +
		"data: {\"type\":\"server.connected\"}\n\n" +
		"data: {\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"" + session + "\"}}\n\n"
	filteredEvents := filter("/global/event", events)
	if strings.Contains(filteredEvents, other) || !strings.Contains(filteredEvents, "server.connected") || !strings.Contains(filteredEvents, session) {
		t.Fatalf("events=%s", filteredEvents)
	}
}

func newTestManager(t *testing.T) (*Manager, string, string) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strings.TrimPrefix(listener.Addr().String(), "127.0.0.1:")
	origin := "https://fern.example.ts.net:" + port
	manager, err := New(listener, origin)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("route shutdown: %v", err)
			}
		case <-time.After(6 * time.Second):
			t.Error("route shutdown timed out")
		}
		_ = manager.Close()
	})
	return manager, origin, "http://" + listener.Addr().String()
}

func pairedRequest(method, target, token, origin string, body io.Reader) *http.Request {
	request, _ := http.NewRequest(method, target, body)
	request.SetBasicAuth(AttachmentUsername, token)
	if origin != "" {
		request.Header.Set("Origin", origin)
		request.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	return request
}

func testRun(identity Identity) taskstore.BackgroundRun {
	return taskstore.BackgroundRun{WorkspaceID: task.WorkspaceID(identity.WorkspaceID), TaskID: task.TaskID(identity.TaskID),
		AttemptID: task.AttemptID(identity.AttemptID), Generation: identity.Generation, WriterGeneration: identity.WriterGeneration, RuntimeEpoch: identity.RuntimeEpoch,
		ObservedContainerID: identity.ContainerID, ObservedContainerStartedAt: identity.StartedAt,
		OpenCodeSessionID: task.OpenCodeSessionID("ses_0123456789abcdef0123456789abcdef")}
}

func testIdentity(generation int64) Identity {
	started := time.Date(2026, 8, 31, 12, 0, int(generation), 0, time.UTC)
	container := strings.Repeat(string(rune('a'+generation)), 64)
	digest := runtimeDigest(container, started.Format(time.RFC3339Nano))
	return Identity{WorkspaceID: "wsp_0198d34d-6a50-75fb-b1f2-000000000001", TaskID: "tsk_0198d34d-6a50-75fb-b1f2-000000000201",
		AttemptID: "att_0198d34d-6a50-75fb-b1f2-000000000301", Generation: generation, WriterGeneration: 1,
		SessionID: "ses_0123456789abcdef0123456789abcdef", RuntimeEpoch: started.UnixNano(),
		ContainerID: container, StartedAt: started.Format(time.RFC3339Nano), RuntimeToken: digest}
}

func runtimeDigest(container, started string) string {
	digest := sha256.Sum256([]byte(container + "\x00" + started))
	return hex.EncodeToString(digest[:])
}
