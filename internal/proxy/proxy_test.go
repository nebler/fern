package proxy

import (
	"bufio"
	"context"
	"io"
	"log/slog"
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
}

func TestRequestIntentDefaultsUnknownReadsToWorkStarting(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/future/starts-work", nil)
	intent := requestIntent(request)
	if !intent.Hold || !intent.MayStartWork {
		t.Fatalf("unknown GET intent = %+v", intent)
	}
	request = httptest.NewRequest(http.MethodGet, "/event/", nil)
	if intent := requestIntent(request); intent.Hold || intent.MayStartWork {
		t.Fatalf("normalized event intent = %+v", intent)
	}
}

func (w staticWaker) AcquireRequest(_ context.Context, intent workspace.RequestIntent) (runtime.Endpoint, func(), error) {
	if w.active != nil {
		w.active <- intent.Hold && intent.MayStartWork
	}
	return w.endpoint, func() {
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
	server := httptest.NewServer(New(staticWaker{endpoint: upstreamURL}, logger))
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
	server := httptest.NewServer(New(waker, slog.New(slog.NewTextHandler(io.Discard, nil))))
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
