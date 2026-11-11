package boxexec_test

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/boxexec"
)

// newJobs serves a box's background commands and returns a client of them.
func newJobs(t *testing.T) boxexec.JobClient {
	t.Helper()
	jobs := boxexec.NewJobs(token, t.TempDir(), nil, slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	jobs.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Cleanup(jobs.Close)
	return boxexec.JobClient{HC: srv.Client(), Addr: srv.Listener.Addr().String(), Token: token}
}

func sh(id, script string) boxexec.JobRequest {
	return boxexec.JobRequest{ID: id, Argv: []string{"/bin/sh", "-c", script}}
}

func TestJobs(t *testing.T) {
	c := newJobs(t)
	ctx := t.Context()
	if err := c.Start(ctx, sh("one", "echo out; echo err >&2; exit 3")); err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(ctx, "one"); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	got, err := c.Read(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(boxexec.JobOutput{Output: "out\nerr\n", Code: 3}, got); diff != "" {
		t.Errorf("Read() of a job that exited (-want +got):\n%s", diff)
	}
	// What was read is not read again.
	if got, err := c.Read(ctx, "one"); err != nil || got.Output != "" || got.Code != 3 {
		t.Errorf("second Read() = %+v, %v, want no output and code 3", got, err)
	}
	// A call sent again finds the job, and doesn't run it twice.
	if err := c.Start(ctx, sh("one", "echo again")); !errors.Is(err, boxexec.ErrJobExists) {
		t.Errorf("Start() of the same ID error = %v, want %v", err, boxexec.ErrJobExists)
	}

	if err := c.Start(ctx, sh("two", "echo up; sleep 100; echo never")); err != nil {
		t.Fatal(err)
	}
	infos, err := c.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]boxexec.JobInfo{{ID: "one"}, {ID: "two", Running: true}}, infos); diff != "" {
		t.Errorf("List() (-want +got):\n%s", diff)
	}
	killed, err := c.Kill(ctx, "two")
	if err != nil {
		t.Fatal(err)
	}
	if !killed.Killed || killed.Running || strings.Contains(killed.Output, "never") {
		t.Errorf("Kill() = %+v, want it killed before it printed more", killed)
	}

	for name, call := range map[string]func() error{
		"Read": func() error { _, err := c.Read(ctx, "nope"); return err },
		"Kill": func() error { _, err := c.Kill(ctx, "nope"); return err },
		"Wait": func() error { return c.Wait(ctx, "nope") },
	} {
		if err := call(); !errors.Is(err, boxexec.ErrNoJob) {
			t.Errorf("%s() of an unknown job error = %v, want %v", name, err, boxexec.ErrNoJob)
		}
	}
	if err := c.Start(ctx, boxexec.JobRequest{ID: "bad"}); err == nil {
		t.Error("Start() without a command succeeded")
	}
	other := c
	other.Token = "wrong"
	if _, err := other.List(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("List() with the wrong token error = %v, want a 401", err)
	}
}

// A box keeps MaxJobs jobs. A new one takes the place of the oldest that
// has finished; with none finished it is refused.
func TestJobsMakeRoom(t *testing.T) {
	c := newJobs(t)
	ctx := t.Context()
	for i := range boxexec.MaxJobs {
		if err := c.Start(ctx, sh(fmt.Sprint("run", i), "sleep 100")); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Start(ctx, sh("extra", "true")); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("Start() with %d running error = %v, want a 429", boxexec.MaxJobs, err)
	}
	for _, id := range []string{"run3", "run5"} {
		if _, err := c.Kill(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Start(ctx, sh("extra", "true")); err != nil {
		t.Fatalf("Start() with two finished error = %v", err)
	}
	if _, err := c.Read(ctx, "run3"); !errors.Is(err, boxexec.ErrNoJob) {
		t.Errorf("Read() of the oldest finished job error = %v, want it forgotten", err)
	}
	if _, err := c.Read(ctx, "run5"); err != nil {
		t.Errorf("Read() of the next finished job error = %v, want it kept", err)
	}
}

// A job keeps its newest megabyte of unread output and counts the rest.
func TestJobOutputBounded(t *testing.T) {
	c := newJobs(t)
	ctx := t.Context()
	const kept, printed = 1 << 20, 3<<20 + 5
	// Zeros, then a mark at the very end.
	script := fmt.Sprintf("head -c %d /dev/zero | tr '\\0' x; printf END", printed-3)
	if err := c.Start(ctx, sh("loud", script)); err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(ctx, "loud"); err != nil {
		t.Fatal(err)
	}
	got, err := c.Read(ctx, "loud")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Output) != kept || got.Dropped != printed-kept || !strings.HasSuffix(got.Output, "xEND") {
		t.Errorf("Read() = %d bytes ending %q, %d dropped; want %d ending \"xEND\", %d dropped",
			len(got.Output), got.Output[max(0, len(got.Output)-4):], got.Dropped, kept, printed-kept)
	}
	if got, err := c.Read(ctx, "loud"); err != nil || got.Output != "" || got.Dropped != 0 {
		t.Errorf("second Read() = %d bytes, %d dropped, %v; want none", len(got.Output), got.Dropped, err)
	}
}

// The relay sends a call again for its limit from when the daemon first
// fails to answer, however long the call had run by then.
func TestMCPRelayRetriesALongCall(t *testing.T) {
	const limit = 2 * time.Second
	var calls atomic.Int32
	keys := make(chan string, 2)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys <- r.Header.Get(boxexec.CallKeyHeader)
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
	relay := httptest.NewServer(boxexec.NewMCPRelay(daemon.URL, limit))
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
}
