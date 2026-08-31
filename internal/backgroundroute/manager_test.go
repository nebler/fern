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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestManagerPairedProxyHeadersMutationsSSEAndWebSocket(t *testing.T) {
	observed := make(chan *http.Request, 8)
	releaseSSE := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		observed <- request.Clone(request.Context())
		if request.URL.Path == "/events" {
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(writer, "data: ready\n\n")
			writer.(http.Flusher).Flush()
			<-releaseSSE
			return
		}
		writer.Header().Add("Set-Cookie", "__Host-fern_device=stolen; Secure; Path=/")
		writer.Header().Add("Set-Cookie", "upstream=value; Path=/")
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	manager, origin, local, token := newTestManager(t)
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

	unpaired, _ := http.NewRequest(http.MethodGet, local+"/", nil)
	if response, err := http.DefaultClient.Do(unpaired); err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unpaired response=%v error=%v", response, err)
	}

	request := pairedRequest(http.MethodPost, local+"/api/session?x=1", token, origin, strings.NewReader("{}"))
	request.Header.Set("Authorization", "Bearer browser-secret")
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
	if !basic || username != "opencode" || password != "run-secret" || got.Header.Get("Cookie") != "" || got.URL.RequestURI() != "/api/session?x=1" ||
		got.Host != strings.TrimPrefix(origin, "https://") || got.Header.Get("X-Forwarded-Host") != strings.TrimPrefix(origin, "https://") ||
		got.Header.Get("X-Forwarded-Proto") != "https" || got.Header.Get("X-Forwarded-Port") == "" {
		t.Fatalf("upstream request auth=%t user=%q password=%q host=%q headers=%v uri=%q", basic, username, password, got.Host, got.Header, got.URL.RequestURI())
	}

	cross := pairedRequest(http.MethodPost, local+"/api/session", token, "https://other.example.ts.net:8443", strings.NewReader("{}"))
	if response, err := http.DefaultClient.Do(cross); err != nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin response=%v error=%v", response, err)
	}
	fern := pairedRequest(http.MethodGet, local+"/fern/api/runs", token, origin, nil)
	if response, err := http.DefaultClient.Do(fern); err != nil || response.StatusCode != http.StatusNotFound {
		t.Fatalf("Fern route response=%v error=%v", response, err)
	}

	websocket := pairedRequest(http.MethodGet, local+"/socket", token, origin, nil)
	websocket.Header.Set("Connection", "Upgrade")
	websocket.Header.Set("Upgrade", "websocket")
	websocket.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	websocket.Header.Set("Sec-WebSocket-Version", "13")
	if response, err := http.DefaultClient.Do(websocket); err != nil || response.StatusCode != http.StatusNoContent {
		t.Fatalf("websocket shape response=%v error=%v", response, err)
	}
	got = <-observed
	if !strings.EqualFold(got.Header.Get("Upgrade"), "websocket") || !strings.Contains(strings.ToLower(got.Header.Get("Connection")), "upgrade") {
		t.Fatalf("websocket headers=%v", got.Header)
	}

	events := pairedRequest(http.MethodGet, local+"/events", token, origin, nil)
	eventResponse, err := http.DefaultClient.Do(events)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(eventResponse.Body).ReadString('\n')
	if err != nil || line != "data: ready\n" {
		t.Fatalf("SSE first line=%q error=%v", line, err)
	}
	close(releaseSSE)
	_ = eventResponse.Body.Close()
}

func TestManagerRemovalFencesRebindAndRuntimeMismatch(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) }))
	defer upstream.Close()
	manager, _, local, token := newTestManager(t)
	target, err := NewTarget(upstream.URL, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	first, second := testIdentity(1), testIdentity(2)
	if _, err := manager.Activate(first, target); err != nil {
		t.Fatal(err)
	}
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
	run := taskstore.BackgroundRun{WorkspaceID: task.WorkspaceID(second.WorkspaceID), TaskID: task.TaskID(second.TaskID), AttemptID: task.AttemptID(second.AttemptID),
		Generation: second.Generation, RuntimeEpoch: second.RuntimeEpoch, ObservedContainerID: second.ContainerID, ObservedContainerStartedAt: second.StartedAt}
	if _, active := manager.ActiveOrigin(run); !active {
		t.Fatal("exact runtime was not active")
	}
	run.RuntimeEpoch++
	if _, active := manager.ActiveOrigin(run); active {
		t.Fatal("runtime epoch mismatch inherited route")
	}
}

func newTestManager(t *testing.T) (*Manager, string, string, string) {
	t.Helper()
	directory := t.TempDir()
	if err := controlDirectoryMode(directory); err != nil {
		t.Fatal(err)
	}
	store, err := control.Open(directory, "route-test")
	if err != nil {
		t.Fatal(err)
	}
	token := "paired-device-token"
	now := time.Now()
	if _, err := store.AddDevice(token, "Test phone", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strings.TrimPrefix(listener.Addr().String(), "127.0.0.1:")
	origin := "https://fern.example.ts.net:" + port
	manager, err := New(listener, origin, store)
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
	return manager, origin, "http://" + listener.Addr().String(), token
}

func controlDirectoryMode(path string) error { return os.Chmod(path, 0o700) }

func pairedRequest(method, target, token, origin string, body io.Reader) *http.Request {
	request, _ := http.NewRequest(method, target, body)
	request.AddCookie(&http.Cookie{Name: "__Host-fern_device", Value: token})
	if origin != "" {
		request.Header.Set("Origin", origin)
		request.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	return request
}

func testIdentity(generation int64) Identity {
	started := time.Date(2026, 8, 31, 12, 0, int(generation), 0, time.UTC)
	container := strings.Repeat(string(rune('a'+generation)), 64)
	digest := runtimeDigest(container, started.Format(time.RFC3339Nano))
	return Identity{WorkspaceID: "wsp_0198d34d-6a50-75fb-b1f2-000000000001", TaskID: "tsk_0198d34d-6a50-75fb-b1f2-000000000201",
		AttemptID: "att_0198d34d-6a50-75fb-b1f2-000000000301", Generation: generation, RuntimeEpoch: started.UnixNano(),
		ContainerID: container, StartedAt: started.Format(time.RFC3339Nano), RuntimeToken: digest}
}

func runtimeDigest(container, started string) string {
	digest := sha256.Sum256([]byte(container + "\x00" + started))
	return hex.EncodeToString(digest[:])
}
