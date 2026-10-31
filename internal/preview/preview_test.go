package preview_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"go.uber.org/goleak"

	"github.com/nebler/fern/internal/preview"
	"github.com/nebler/fern/internal/store"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fakeStore knows environment demo, at workspace.
type fakeStore struct{ workspace string }

func (f fakeStore) Get(_ context.Context, id string) (store.Env, error) {
	if id != "demo" {
		return store.Env{}, store.ErrNotFound
	}
	return store.Env{ID: id, Workspace: f.workspace}, nil
}

// fakeBoxes is demo's box, at 127.0.0.1, which counts the requests open to it.
type fakeBoxes struct {
	mu    sync.Mutex
	open  int
	total int
}

func (b *fakeBoxes) Preview(_ context.Context, id string, _ context.CancelFunc) (string, func(), error) {
	if id != "demo" {
		return "", nil, store.ErrNotFound
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.open++
	b.total++
	return "127.0.0.1:7070", func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.open--
	}, nil
}

func (b *fakeBoxes) counts() (open, total int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.open, b.total
}

// devServer is a dev server on 127.0.0.1 that answers with the host it was
// asked for, and echoes on a WebSocket at /ws.
func devServer(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			defer func() { _ = c.CloseNow() }()
			typ, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			_ = c.Write(r.Context(), typ, append([]byte("echo "), data...))
			return
		}
		_, _ = io.WriteString(w, "dev server at "+r.Host+r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	return n
}

// newPreview serves previews under localhost for demo, whose services.yaml
// is yaml, and returns the front door's URL and its boxes.
func newPreview(t *testing.T, yaml string) (string, *fakeBoxes) {
	t.Helper()
	ws := t.TempDir()
	if yaml != "" {
		if err := os.WriteFile(filepath.Join(ws, "services.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	boxes := &fakeBoxes{}
	srv := httptest.NewServer(preview.New("localhost", fakeStore{ws}, boxes, slog.New(slog.DiscardHandler)))
	t.Cleanup(srv.Close)
	return srv.URL, boxes
}

// get asks the front door at url for path as host, and returns the status
// and body.
func get(t *testing.T, url, host, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestPreview(t *testing.T) {
	port := devServer(t)
	url, boxes := newPreview(t, "services:\n  web:\n    run: vite\n    port: "+strconv.Itoa(port)+"\n")
	host := strconv.Itoa(port) + "-demo.localhost:30780"
	for _, tc := range []struct {
		name, host string
		want       int
		body       string
	}{
		{"a port", host, http.StatusOK, "dev server at " + host + "/app"},
		{"the default port", "demo.localhost", http.StatusOK, "dev server at demo.localhost/app"},
		{"names are case-insensitive", "DEMO.Localhost", http.StatusOK, "dev server at DEMO.Localhost/app"},
		{"no such environment", "5173-other.localhost", http.StatusNotFound, "no environment other"},
		{"another domain", "demo.example.com", http.StatusNotFound, "not a preview"},
		{"a subdomain", "a.demo.localhost", http.StatusNotFound, "not a preview"},
		{"fern-init's port", "7070-demo.localhost", http.StatusNotFound, "not a preview"},
		{"not a port", "99999-demo.localhost", http.StatusNotFound, "not a preview"},
		{"the bare domain", "localhost", http.StatusNotFound, "not a preview"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := get(t, url, tc.host, "/app")
			if code != tc.want || !strings.Contains(body, tc.body) {
				t.Errorf("GET %s: %d %q, want %d and %q in it", tc.host, code, body, tc.want, tc.body)
			}
		})
	}
	if open, total := boxes.counts(); open != 0 || total != 3 {
		t.Errorf("requests counted in the box: %d open, %d in all; want 0 and 3", open, total)
	}
}

func TestPreviewNoDefault(t *testing.T) {
	url, _ := newPreview(t, "services:\n  db:\n    run: postgres\n")
	code, body := get(t, url, "demo.localhost", "/")
	if code != http.StatusNotFound || !strings.Contains(body, "has no default preview") {
		t.Errorf("GET demo.localhost: %d %q, want 404 saying there is no default preview", code, body)
	}
}

// A WebSocket, such as a dev server's live reload, goes through, and counts
// as a request in the box while it is open.
func TestPreviewWebSocket(t *testing.T) {
	port := devServer(t)
	url, boxes := newPreview(t, "")
	host := strconv.Itoa(port) + "-demo.localhost"
	c, resp, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(url, "http")+"/ws", &websocket.DialOptions{Host: host})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	if err := c.Write(t.Context(), websocket.MessageText, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	_, data, err := c.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "echo hi" {
		t.Errorf("read %q through the preview, want %q", data, "echo hi")
	}
	if open, _ := boxes.counts(); open != 1 {
		t.Errorf("%d requests open in the box during a WebSocket, want 1", open)
	}
}
