package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/workspace"
)

type countingWaker struct {
	endpoint runtime.Endpoint
	wakes    atomic.Int64
}

type trackingBody struct {
	read atomic.Bool
}

func (body *trackingBody) Read([]byte) (int, error) {
	body.read.Store(true)
	return 0, io.EOF
}

func (*trackingBody) Close() error { return nil }

func (w *countingWaker) AcquireRequest(context.Context, workspace.RequestIntent) (workspace.RequestTarget, func(), error) {
	w.wakes.Add(1)
	return workspace.RequestTarget{Endpoint: w.endpoint, Generation: 1}, func() {}, nil
}

func (*countingWaker) InvalidateEndpoint(workspace.RequestTarget) {}

func TestProxyRejectsInvalidBasicAuthBeforeWake(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		username string
		password string
		header   string
	}{
		{name: "missing credentials"},
		{name: "missing username", password: "secret"},
		{name: "missing password", username: "opencode"},
		{name: "wrong username", username: "other", password: "secret"},
		{name: "wrong password", username: "opencode", password: "other"},
		{name: "malformed header", header: "Basic not-base64"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			waker := &countingWaker{}
			handler := New(waker, runtime.ServerAuth{Password: "secret"}, testLogger())
			request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
			if test.header != "" {
				request.Header.Set("Authorization", test.header)
			} else if test.username != "" || test.password != "" {
				request.SetBasicAuth(test.username, test.password)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
			}
			if got := response.Header().Get("WWW-Authenticate"); got != `Basic realm="opencode"` {
				t.Fatalf("WWW-Authenticate = %q", got)
			}
			if got := waker.wakes.Load(); got != 0 {
				t.Fatalf("wake count = %d, want 0", got)
			}
		})
	}
}

func TestProxyAuthenticatesCredentialsBeforeWake(t *testing.T) {
	t.Parallel()
	waker := &countingWaker{}
	handler := New(waker, runtime.ServerAuth{Password: "secret"}, testLogger())
	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	request.SetBasicAuth("opencode", "secret")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if waker.wakes.Load() != 1 {
		t.Fatal("valid credentials did not reach waker")
	}
}

func TestProxyUsesDefaultServerAuthUsername(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	waker := &countingWaker{endpoint: mustParseEndpoint(t, upstream.URL)}
	handler := New(waker, runtime.ServerAuth{Password: "secret"}, testLogger())

	for _, username := range []string{"", "agent", "opencode"} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.SetBasicAuth(username, "secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		want := http.StatusUnauthorized
		if username == "opencode" {
			want = http.StatusNoContent
		}
		if response.Code != want {
			t.Errorf("username %q: status = %d, want %d", username, response.Code, want)
		}
	}
	if got := waker.wakes.Load(); got != 1 {
		t.Fatalf("wake count = %d, want 1", got)
	}
}

func TestProxyForwardsValidAuthForAllRequestKinds(t *testing.T) {
	t.Parallel()
	type receivedRequest struct {
		path          string
		authorization string
		body          string
		upgrade       string
	}
	received := make(chan receivedRequest, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		received <- receivedRequest{
			path:          request.URL.Path,
			authorization: request.Header.Get("Authorization"),
			body:          string(body),
			upgrade:       request.Header.Get("Upgrade"),
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	waker := &countingWaker{endpoint: mustParseEndpoint(t, upstream.URL)}
	handler := New(waker, runtime.ServerAuth{Password: "secret"}, testLogger())

	tests := []struct {
		method  string
		path    string
		body    string
		upgrade bool
	}{
		{method: http.MethodGet, path: "/api/event"},
		{method: http.MethodGet, path: "/api/health"},
		{method: http.MethodPost, path: "/session", body: "request body"},
		{method: http.MethodGet, path: "/socket", upgrade: true},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.SetBasicAuth("opencode", "secret")
		if test.upgrade {
			request.Header.Set("Connection", "upgrade")
			request.Header.Set("Upgrade", "websocket")
		}
		wantAuthorization := request.Header.Get("Authorization")
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != http.StatusNoContent {
			t.Fatalf("%s %s: status = %d", test.method, test.path, response.Code)
		}
		got := <-received
		if got.path != test.path || got.authorization != wantAuthorization || got.body != test.body {
			t.Errorf("%s %s: upstream request = %+v", test.method, test.path, got)
		}
		if test.upgrade && got.upgrade != "websocket" {
			t.Errorf("%s: upstream Upgrade = %q", test.path, got.upgrade)
		}
	}
	if got := waker.wakes.Load(); got != int64(len(tests)) {
		t.Fatalf("wake count = %d, want %d", got, len(tests))
	}
}

func TestProxyWithoutPasswordRemainsUnauthenticated(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q", got)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	for _, auth := range []runtime.ServerAuth{{}, {Password: ""}} {
		waker := &countingWaker{endpoint: mustParseEndpoint(t, upstream.URL)}
		response := httptest.NewRecorder()
		New(waker, auth, testLogger()).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		if response.Code != http.StatusNoContent {
			t.Errorf("auth %+v: status = %d", auth, response.Code)
		}
		if got := waker.wakes.Load(); got != 1 {
			t.Errorf("auth %+v: wake count = %d, want 1", auth, got)
		}
	}
}

func TestProxyConcurrentUnauthorizedRequestsNeverWake(t *testing.T) {
	t.Parallel()
	const requestCount = 100
	waker := &countingWaker{}
	handler := New(waker, runtime.ServerAuth{Password: "secret"}, testLogger())
	var group sync.WaitGroup
	group.Add(requestCount)
	for i := 0; i < requestCount; i++ {
		go func(i int) {
			defer group.Done()
			request := httptest.NewRequest(http.MethodGet, "/api/event", nil)
			request.SetBasicAuth("opencode", fmt.Sprintf("wrong-%d", i))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", response.Code, http.StatusUnauthorized)
			}
		}(i)
	}
	group.Wait()
	if got := waker.wakes.Load(); got != 0 {
		t.Fatalf("wake count = %d, want 0", got)
	}
}

func TestProxyUnauthorizedRequestDoesNotReadBody(t *testing.T) {
	t.Parallel()
	waker := &countingWaker{}
	body := &trackingBody{}
	request := httptest.NewRequest(http.MethodPost, "/session", body)
	response := httptest.NewRecorder()

	New(waker, runtime.ServerAuth{Password: "secret"}, testLogger()).ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if body.read.Load() {
		t.Fatal("unauthorized request body was read")
	}
	if got := waker.wakes.Load(); got != 0 {
		t.Fatalf("wake count = %d, want 0", got)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
