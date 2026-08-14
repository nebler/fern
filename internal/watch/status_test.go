package watch

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/nebler/fern/internal/runtime"
)

func TestAllSessionsIdleUsesAuthAndRejectsBusy(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "agent" || password != "secret" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"session-one":{"type":"busy"}}`))
	}))
	defer server.Close()
	_, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	idle, err := AllSessionsIdle(context.Background(), runtime.Endpoint{Host: "127.0.0.1", Port: port}, runtime.ServerAuth{Username: "agent", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if idle {
		t.Fatal("busy session was reported idle")
	}
}

func TestAllSessionsIdleRejectsTrailingJSON(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{} {}`))
	}))
	defer server.Close()
	_, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	if idle, err := AllSessionsIdle(context.Background(), runtime.Endpoint{Host: "127.0.0.1", Port: port}, runtime.ServerAuth{}); err == nil || idle {
		t.Fatalf("trailing JSON result idle=%t err=%v", idle, err)
	}
}

func TestAllSessionsIdleAcceptsEmptyStatusMap(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()
	_, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	idle, err := AllSessionsIdle(context.Background(), runtime.Endpoint{Host: "127.0.0.1", Port: port}, runtime.ServerAuth{})
	if err != nil {
		t.Fatal(err)
	}
	if !idle {
		t.Fatal("empty active-status map was not idle")
	}
}

func TestAllSessionsIdleRejectsNull(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`null`))
	}))
	defer server.Close()
	_, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	if idle, err := AllSessionsIdle(context.Background(), runtime.Endpoint{Host: "127.0.0.1", Port: port}, runtime.ServerAuth{}); err == nil || idle {
		t.Fatalf("null status result idle=%t err=%v", idle, err)
	}
}
