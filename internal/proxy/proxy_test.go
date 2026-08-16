package proxy

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/workspace"
)

type staticWaker struct {
	endpoint runtime.Endpoint
	active   chan bool
	released chan struct{}
	invalid  chan workspace.RequestTarget
}

func (w staticWaker) InvalidateEndpoint(target workspace.RequestTarget) {
	if w.invalid != nil {
		w.invalid <- target
	}
}

func TestRequestIntentDefaultsUnknownReadsToWorkStarting(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/future/starts-work", nil)
	intent := requestIntent(request)
	if intent != workspace.RequestWork {
		t.Fatalf("unknown GET intent = %+v", intent)
	}
	request = httptest.NewRequest(http.MethodGet, "/event/", nil)
	if intent := requestIntent(request); intent != workspace.RequestWork {
		t.Fatalf("noncanonical event intent = %+v", intent)
	}
	request = httptest.NewRequest(http.MethodPost, "/event", nil)
	if intent := requestIntent(request); intent != workspace.RequestWork {
		t.Fatalf("mutating event-path intent = %+v", intent)
	}
	request = httptest.NewRequest(http.MethodGet, "/event", nil)
	request.Header.Set("Upgrade", "websocket")
	if intent := requestIntent(request); intent != workspace.RequestWork {
		t.Fatalf("upgraded event-path intent = %+v", intent)
	}
}

func TestRequestIntentUsesSelectedOpenCodeProtocol(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		protocol runtime.Protocol
		method   string
		path     string
		want     workspace.RequestIntent
	}{
		{name: "V1 event", protocol: runtime.ProtocolV1, method: http.MethodGet, path: "/event", want: workspace.RequestObserve},
		{name: "V1 health", protocol: runtime.ProtocolV1, method: http.MethodGet, path: "/global/health", want: workspace.RequestRead},
		{name: "V1 treats V2 event as work", protocol: runtime.ProtocolV1, method: http.MethodGet, path: "/api/event", want: workspace.RequestWork},
		{name: "V2 event", protocol: runtime.ProtocolV2, method: http.MethodGet, path: "/api/event", want: workspace.RequestObserve},
		{name: "V2 health", protocol: runtime.ProtocolV2, method: http.MethodHead, path: "/api/health", want: workspace.RequestRead},
		{name: "V2 active", protocol: runtime.ProtocolV2, method: http.MethodGet, path: "/api/session/active", want: workspace.RequestRead},
		{name: "V2 treats V1 event as work", protocol: runtime.ProtocolV2, method: http.MethodGet, path: "/event", want: workspace.RequestWork},
		{name: "auto accepts V1", protocol: runtime.ProtocolAuto, method: http.MethodGet, path: "/event", want: workspace.RequestObserve},
		{name: "auto accepts V2", protocol: runtime.ProtocolAuto, method: http.MethodGet, path: "/api/event", want: workspace.RequestObserve},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(test.method, test.path, nil)
			if got := requestIntentFor(test.protocol, request); got != test.want {
				t.Fatalf("requestIntentFor(%s, %s %s) = %v, want %v", test.protocol, test.method, test.path, got, test.want)
			}
		})
	}
}

func (w staticWaker) AcquireRequest(_ context.Context, intent workspace.RequestIntent) (workspace.RequestTarget, func(), error) {
	if w.active != nil {
		w.active <- intent == workspace.RequestWork
	}
	return workspace.RequestTarget{Endpoint: w.endpoint, Generation: 1}, func() {
		if w.released != nil {
			close(w.released)
		}
	}, nil
}

func TestProxyDoesNotBufferSSE(t *testing.T) {
	t.Parallel()
	writeSecond := make(chan struct{})
	defer func() {
		select {
		case <-writeSecond:
		default:
			close(writeSecond)
		}
	}()
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: first\n\n"))
		writer.(http.Flusher).Flush()
		<-writeSecond
		_, _ = writer.Write([]byte("data: second\n\n"))
	}))
	defer upstream.Close()
	upstreamURL := mustParseEndpoint(t, upstream.URL)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewServer(New(staticWaker{endpoint: upstreamURL}, runtime.ServerAuth{}, logger))
	defer server.Close()

	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(server.URL + "/event")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	firstLine := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(response.Body).ReadString('\n')
		firstLine <- line
	}()
	select {
	case line := <-firstLine:
		if line != "data: first\n" {
			t.Fatalf("first line = %q", line)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("first SSE event was buffered")
	}
	close(writeSecond)
}

func TestProxyHoldsMutatingRequestLeaseUntilResponseEnds(t *testing.T) {
	t.Parallel()
	finish := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-finish
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	active := make(chan bool, 1)
	released := make(chan struct{})
	waker := staticWaker{endpoint: mustParseEndpoint(t, upstream.URL), active: active, released: released}
	server := httptest.NewServer(New(waker, runtime.ServerAuth{}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/session", nil)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			response.Body.Close()
		}
		done <- err
	}()
	if !<-active {
		t.Fatal("POST request was not marked active")
	}
	select {
	case <-released:
		t.Fatal("request lease released before upstream response ended")
	default:
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("request lease was not released")
	}
}

func TestProxyInvalidatesFailedEndpoint(t *testing.T) {
	t.Parallel()
	invalid := make(chan workspace.RequestTarget, 1)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := runtime.Endpoint{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(staticWaker{endpoint: endpoint, invalid: invalid}, runtime.ServerAuth{}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer server.Close()
	response, err := http.Get(server.URL + "/global/health")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadGateway)
	}
	select {
	case got := <-invalid:
		if got.Endpoint != endpoint || got.Generation != 1 {
			t.Fatalf("invalidated target = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("failed endpoint was not invalidated")
	}
}

func TestProxyClientCancellationDoesNotInvalidateEndpoint(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
	}))
	defer upstream.Close()
	invalid := make(chan workspace.RequestTarget, 1)
	server := httptest.NewServer(New(staticWaker{
		endpoint: mustParseEndpoint(t, upstream.URL),
		invalid:  invalid,
	}, runtime.ServerAuth{}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/global/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; err == nil {
		t.Fatal("canceled request unexpectedly succeeded")
	}
	select {
	case target := <-invalid:
		t.Fatalf("client cancellation invalidated healthy target %+v", target)
	case <-time.After(100 * time.Millisecond):
	}
}

func mustParseEndpoint(t *testing.T, rawURL string) runtime.Endpoint {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	port := request.URL.Port()
	var parsed int
	for _, char := range port {
		parsed = parsed*10 + int(char-'0')
	}
	return runtime.Endpoint{Host: request.URL.Hostname(), Port: parsed}
}
