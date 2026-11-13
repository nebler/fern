package mcprelay_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/nebler/fern/internal/mcprelay"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// The relay sends a call again for its limit from when the daemon first
// fails to answer, however long the call had run by then.
func TestMCPRelayRetriesALongCall(t *testing.T) {
	const limit = 2 * time.Second
	var calls atomic.Int32
	keys := make(chan string, 2)
	auths := make(chan string, 2)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys <- r.Header.Get(mcprelay.CallKeyHeader)
		auths <- r.Header.Get("Authorization")
		if calls.Add(1) == 1 {
			// A tool that runs past the limit, in a daemon that then stops.
			time.Sleep(limit + 500*time.Millisecond)
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = io.Copy(w, r.Body)
	}))
	t.Cleanup(daemon.Close)
	relay := httptest.NewServer(mcprelay.New(daemon.URL, "the-box-token", limit))
	t.Cleanup(relay.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, relay.URL+"/mcp/demo", strings.NewReader("the call"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := relay.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "the call" {
		t.Fatalf("the relay answered %d %q, want the second daemon's answer", resp.StatusCode, body)
	}
	if first, second := <-keys, <-keys; first == "" || first != second {
		t.Errorf("the call's keys = %q, %q, want the same one twice", first, second)
	}
	// The caller sent no token: the relay has the box's, so Claude needs none.
	for range 2 {
		if got := <-auths; got != "Bearer the-box-token" {
			t.Errorf("the daemon was sent Authorization %q, want the box's token from the relay", got)
		}
	}
}

// A tool call is answered at once, with a stream, and gets the daemon's
// answer as the stream's one message when the tool is done: Claude Code gives
// the answer to a POST 60 s to begin. What the daemon can't answer is the
// call's error, under its ID.
func TestMCPRelayStreamsAToolCall(t *testing.T) {
	const call = `{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{"name":"Bash"}}`
	tests := []struct {
		name   string
		daemon http.HandlerFunc
		want   string
	}{
		{
			name: "the tool's answer",
			daemon: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, "{\"jsonrpc\": \"2.0\",\n \"id\": 41, \"result\": {\"content\": []}}\n")
			},
			want: `{"jsonrpc":"2.0","id":41,"result":{"content":[]}}`,
		},
		{
			name:   "a daemon that refuses the call",
			daemon: func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unauthorized", http.StatusUnauthorized) },
			want:   `{"error":{"code":-32603,"message":"the daemon answered 401 Unauthorized: unauthorized"},"id":41,"jsonrpc":"2.0"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release := make(chan struct{})
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-release: // the tool is done
				case <-r.Context().Done():
					return
				}
				tt.daemon(w, r)
			}))
			t.Cleanup(daemon.Close)
			relay := httptest.NewServer(mcprelay.New(daemon.URL, "the-box-token", time.Second))
			t.Cleanup(relay.Close)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, relay.URL+"/mcp/demo", strings.NewReader(call))
			if err != nil {
				t.Fatal(err)
			}
			// The answer begins while the tool still runs.
			resp, err := relay.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if got := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || got != "text/event-stream" {
				t.Fatalf("the relay answered %d %q, want a stream", resp.StatusCode, got)
			}
			close(release)
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if want := "event: message\ndata: " + tt.want + "\n\n"; string(body) != want {
				t.Errorf("the stream held %q, want %q", body, want)
			}
		})
	}
}
