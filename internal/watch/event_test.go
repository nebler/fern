package watch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nebler/fern/internal/runtime"
)

func TestStreamParsesSSEAndUsesBasicAuth(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "opencode" || password != "secret" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte(": heartbeat\n\ndata: {\"type\":\"session.status\",\ndata: \"properties\":{\"sessionID\":\"one\",\"status\":{\"type\":\"busy\"}}}\n\n"))
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	events := make(chan Event, 1)
	err := Stream(ctx, StreamOptions{BaseURL: server.URL, Auth: runtime.ServerAuth{Password: "secret"}}, events)
	if err == nil {
		t.Fatal("Stream returned nil after the server closed")
	}
	select {
	case event := <-events:
		if event.Type != "session.status" {
			t.Fatalf("event type = %q, want session.status", event.Type)
		}
	default:
		t.Fatal("Stream did not emit the SSE event")
	}
}

func TestStreamDiscardsUnterminatedSSEFrame(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("data: {\"type\":\"session.status\",\"properties\":{}}\n"))
	}))
	defer server.Close()
	events := make(chan Event, 1)
	if err := Stream(context.Background(), StreamOptions{BaseURL: server.URL}, events); err == nil {
		t.Fatal("Stream returned nil after EOF")
	}
	select {
	case <-events:
		t.Fatal("Stream emitted an unterminated frame")
	default:
	}
}

func TestStreamUsesV2PathEnvelopeAndAuth(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/event" {
			http.NotFound(writer, request)
			return
		}
		username, password, ok := request.BasicAuth()
		if !ok || username != "opencode" || password != "v2-secret" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"id\":\"evt_one\",\"type\":\"session.status\",\"data\":{\"sessionID\":\"ses_one\",\"status\":{\"type\":\"busy\"}}}\n\n"))
	}))
	defer server.Close()
	events := make(chan Event, 1)
	err := Stream(context.Background(), StreamOptions{
		BaseURL: server.URL, Auth: runtime.ServerAuth{Password: "v2-secret"},
	}, events)
	if err == nil {
		t.Fatal("Stream returned nil after the server closed")
	}
	event := <-events
	if event.ID != "evt_one" || string(event.Data) == "" {
		t.Fatalf("event = %+v", event)
	}
	sessionID, status, ok := parseStatus(event)
	if !ok || sessionID != "ses_one" || status != "busy" {
		t.Fatalf("parseStatus = %q, %q, %t", sessionID, status, ok)
	}
}

func TestDefaultStreamClientBoundsResponseHeaders(t *testing.T) {
	t.Parallel()
	transport, ok := defaultStreamClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default stream transport = %T", defaultStreamClient.Transport)
	}
	if transport.ResponseHeaderTimeout <= 0 {
		t.Fatal("default stream client has no response-header timeout")
	}
}
