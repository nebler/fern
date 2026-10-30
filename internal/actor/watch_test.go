package actor_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
)

// fakeWatcher reports the changes a test sends on fire from Watch's
// goroutine, as package watch does its own.
type fakeWatcher struct {
	started chan [2]string // dir and box of each Watch
	fire    chan []string
	fired   chan struct{} // after each changed call
	ended   chan struct{} // after each Watch
}

func (w *fakeWatcher) Watch(ctx context.Context, dir, box string, changed func(context.Context, []string)) error {
	w.started <- [2]string{dir, box}
	defer func() { w.ended <- struct{}{} }()
	for {
		select {
		case <-ctx.Done():
			return nil
		case paths := <-w.fire:
			changed(ctx, paths)
			w.fired <- struct{}{}
		}
	}
}

// TestWatch checks that the files the agent box writes reach the
// environment's box while it is awake, and that the watch ends with the agent
// box.
func TestWatch(t *testing.T) {
	touched := make(chan []string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req boxexec.TouchRequest
		if r.URL.Path != boxexec.TouchPath || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad touch", http.StatusBadRequest)
			return
		}
		touched <- req.Paths
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	fw := &fakeWatcher{started: make(chan [2]string, 1), fire: make(chan []string), fired: make(chan struct{}), ended: make(chan struct{}, 1)}
	st, drv := newFakeStore(), runsc.NewFake(srv.Listener.Addr().String())
	g := actor.New(st, drv, actor.Config{Image: "box", Agents: true, Watcher: fw}, notrace, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		if err := g.Run(ctx); err != nil {
			t.Errorf("Run() error: %v", err)
		}
		close(done)
	}()
	defer func() { cancel(); <-done }()
	ws := t.TempDir()
	if err := g.Create(t.Context(), "demo", store.CreateOptions{Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	fire := func(paths ...string) {
		fw.fire <- paths
		<-fw.fired
	}

	// The environment's own box doesn't watch.
	main, err := g.Exec(t.Context(), "demo", "", func() {})
	if err != nil {
		t.Fatal(err)
	}
	main.Close()
	if err := g.Sleep(t.Context(), "demo"); err != nil {
		t.Fatal(err)
	}
	agent, err := g.Exec(t.Context(), store.AgentBox("demo"), "", func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	if diff := cmp.Diff([2]string{ws, store.AgentBox("demo")}, <-fw.started); diff != "" {
		t.Errorf("Watch (-want +got):\n%s", diff)
	}

	// Asleep, the environment's box is neither woken nor touched.
	fire("asleep.txt")
	if state(t, drv, "demo") == runsc.Running {
		t.Error("a change woke the environment's box")
	}
	select {
	case paths := <-touched:
		t.Errorf("touched %v in an asleep box", paths)
	default:
	}

	main, err = g.Exec(t.Context(), "demo", "", func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	fire("src/a.ts", "src/b.ts")
	select {
	case paths := <-touched:
		if diff := cmp.Diff([]string{"src/a.ts", "src/b.ts"}, paths); diff != "" {
			t.Errorf("touched (-want +got):\n%s", diff)
		}
	default:
		t.Error("an awake box wasn't touched")
	}

	// Sleeping the environment stops its agent box, and the watch with it.
	if err := g.Sleep(t.Context(), "demo"); err != nil {
		t.Fatal(err)
	}
	<-fw.ended
}
