package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/boxexec"
)

// A call to T3's MCP server in the box reaches the daemon's route with the
// box's token and T3's own, and no other path of T3's is reachable.
func TestT3Relay(t *testing.T) {
	type seen struct{ path, auth, box, body string }
	calls := make(chan seen, 1)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls <- seen{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get(boxexec.BoxTokenHeader), string(body)}
		_, _ = io.WriteString(w, "from t3")
	}))
	defer daemon.Close()
	h, err := t3Relay(daemon.URL+"/t3/demo", "box-token")
	if err != nil {
		t.Fatal(err)
	}
	box := httptest.NewServer(h)
	defer box.Close()
	call := func(path string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, box.URL+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer t3-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, body := call("/mcp"); code != http.StatusOK || body != "from t3" {
		t.Errorf("POST /mcp = %d %q, want T3's answer", code, body)
	}
	if got, want := <-calls, (seen{"/t3/demo", "Bearer t3-token", "box-token", "{}"}); got != want {
		t.Errorf("the daemon saw %+v, want %+v", got, want)
	}
	if code, _ := call("/api/projects"); code != http.StatusNotFound {
		t.Errorf("POST /api/projects = %d, want 404", code)
	}
	select {
	case got := <-calls:
		t.Errorf("another path reached the daemon: %+v", got)
	default:
	}
}
