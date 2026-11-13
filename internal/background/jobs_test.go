package background_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/nebler/fern/internal/background"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// token is the box's, which its jobs take.
const token = "secret"

// newJobs serves a box's background commands and returns a client of them.
func newJobs(t *testing.T) background.JobClient {
	t.Helper()
	jobs := background.NewJobs(token, t.TempDir(), nil, slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	jobs.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Cleanup(jobs.Close)
	return background.JobClient{HC: srv.Client(), Addr: srv.Listener.Addr().String(), Token: token}
}

func sh(id, script string) background.JobRequest {
	return background.JobRequest{ID: id, Argv: []string{"/bin/sh", "-c", script}}
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
	if diff := cmp.Diff(background.JobOutput{Output: "out\nerr\n", Code: 3}, got); diff != "" {
		t.Errorf("Read() of a job that exited (-want +got):\n%s", diff)
	}
	// What was read is not read again.
	if got, err := c.Read(ctx, "one"); err != nil || got.Output != "" || got.Code != 3 {
		t.Errorf("second Read() = %+v, %v, want no output and code 3", got, err)
	}
	// A call sent again finds the job, and doesn't run it twice.
	if err := c.Start(ctx, sh("one", "echo again")); !errors.Is(err, background.ErrJobExists) {
		t.Errorf("Start() of the same ID error = %v, want %v", err, background.ErrJobExists)
	}

	if err := c.Start(ctx, sh("two", "echo up; sleep 100; echo never")); err != nil {
		t.Fatal(err)
	}
	infos, err := c.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]background.JobInfo{{ID: "one"}, {ID: "two", Running: true}}, infos); diff != "" {
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
		if err := call(); !errors.Is(err, background.ErrNoJob) {
			t.Errorf("%s() of an unknown job error = %v, want %v", name, err, background.ErrNoJob)
		}
	}
	if err := c.Start(ctx, background.JobRequest{ID: "bad"}); err == nil {
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
	for i := range background.MaxJobs {
		if err := c.Start(ctx, sh(fmt.Sprint("run", i), "sleep 100")); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Start(ctx, sh("extra", "true")); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("Start() with %d running error = %v, want a 429", background.MaxJobs, err)
	}
	for _, id := range []string{"run3", "run5"} {
		if _, err := c.Kill(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Start(ctx, sh("extra", "true")); err != nil {
		t.Fatalf("Start() with two finished error = %v", err)
	}
	if _, err := c.Read(ctx, "run3"); !errors.Is(err, background.ErrNoJob) {
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

// A wait that ends because the box stops, with its job still running, is an
// error, not the job's end: the daemon read the empty answer it used to get
// as "ended", asked the box that was killing the job how it had ended, and
// told Claude it was running, or had exited with code 137, for good.
func TestJobWaitEndsWithTheBox(t *testing.T) {
	jobs := background.NewJobs(token, t.TempDir(), nil, slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	jobs.Register(mux)
	// As fern-init's server: its requests end when the box is told to stop.
	stop, cancel := context.WithCancel(t.Context())
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.BaseContext = func(net.Listener) context.Context { return stop }
	srv.Start()
	t.Cleanup(srv.Close)
	t.Cleanup(jobs.Close)
	c := background.JobClient{HC: srv.Client(), Addr: srv.Listener.Addr().String(), Token: token}

	if err := c.Start(t.Context(), sh("server", "sleep 300")); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- c.Wait(t.Context(), "server") }()
	// The wait is under way once the job is listed as running and a moment
	// has passed for the request to arrive; then the box stops.
	if infos, err := c.List(t.Context()); err != nil || len(infos) != 1 || !infos[0].Running {
		t.Fatalf("List() = %+v, %v; want the job running", infos, err)
	}
	cancel()
	select {
	case err := <-waited:
		if err == nil {
			t.Error("Wait() returned no error for a job that was still running when its box stopped")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Wait() didn't return when the box stopped")
	}
}
