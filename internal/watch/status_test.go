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

func TestAllSessionsIdleUsesV2ActiveSessions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		body     string
		wantIdle bool
		wantErr  bool
	}{
		{name: "empty", body: `{"data":{}}`, wantIdle: true},
		{name: "running", body: `{"data":{"ses_one":{"type":"running"}}}`},
		{name: "missing data", body: `{}`, wantErr: true},
		{name: "null data", body: `{"data":null}`, wantErr: true},
		{name: "unknown active state", body: `{"data":{"ses_one":{"type":"waiting"}}}`, wantErr: true},
		{name: "trailing data", body: `{"data":{}} {}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				username, password, ok := request.BasicAuth()
				if !ok || username != "opencode" || password != "v2-secret" {
					http.Error(writer, "unauthorized", http.StatusUnauthorized)
					return
				}
				switch request.URL.Path {
				case "/api/session/active":
					_, _ = writer.Write([]byte(test.body))
				case "/api/shell", "/api/pty", "/api/permission/request", "/api/form/request":
					_, _ = writer.Write([]byte(`{"data":[]}`))
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()
			_, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
			port, _ := strconv.Atoi(portText)
			idle, err := AllSessionsIdle(
				context.Background(),
				runtime.Endpoint{Host: "127.0.0.1", Port: port},
				runtime.ServerAuth{Password: "v2-secret"},
			)
			if idle != test.wantIdle || (err != nil) != test.wantErr {
				t.Fatalf("idle=%t err=%v, want idle=%t wantErr=%t", idle, err, test.wantIdle, test.wantErr)
			}
		})
	}
}

func TestAllSessionsIdleV2BlocksEveryVolatileWorkClass(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		body string
	}{
		{name: "foreground execution", path: "/api/session/active", body: `{"data":{"ses_one":{"type":"running"}}}`},
		{name: "shell", path: "/api/shell", body: `{"data":[{"status":"running"}]}`},
		{name: "PTY", path: "/api/pty", body: `{"data":[{"status":"running"}]}`},
		{name: "permission", path: "/api/permission/request", body: `{"data":[{}]}`},
		{name: "form", path: "/api/form/request", body: `{"data":[{}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == test.path {
					_, _ = writer.Write([]byte(test.body))
					return
				}
				if request.URL.Path == "/api/session/active" {
					_, _ = writer.Write([]byte(`{"data":{}}`))
					return
				}
				_, _ = writer.Write([]byte(`{"data":[]}`))
			}))
			defer server.Close()
			_, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
			port, _ := strconv.Atoi(portText)
			idle, err := AllSessionsIdle(context.Background(), runtime.Endpoint{
				Host: "127.0.0.1", Port: port,
			}, runtime.ServerAuth{})
			if err != nil {
				t.Fatal(err)
			}
			if idle {
				t.Fatalf("%s activity was reported idle", test.name)
			}
		})
	}
}
