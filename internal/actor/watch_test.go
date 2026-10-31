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

// TestWatch checks that the environment's box, while it is awake, watches
// for the files the agent box writes and is touched with them.
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

	// The agent box alone doesn't watch: no dev server would hear.
	agent, err := g.Exec(t.Context(), store.AgentBox("demo"), "", func() {})
	if err != nil {
		t.Fatal(err)
	}
	agent.Close()
	select {
	case w := <-fw.started:
		t.Fatalf("the agent box's wake started a watch of %v", w)
	default:
	}

	// The environment's box watches for the agent box's writes, and passes
	// them on to itself.
	main, err := g.Exec(t.Context(), "demo", "", func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	if diff := cmp.Diff([2]string{ws, store.AgentBox("demo")}, <-fw.started); diff != "" {
		t.Errorf("Watch (-want +got):\n%s", diff)
	}
	fire("src/a.ts", "src/b.ts")
	select {
	case paths := <-touched:
		if diff := cmp.Diff([]string{"src/a.ts", "src/b.ts"}, paths); diff != "" {
			t.Errorf("touched (-want +got):\n%s", diff)
		}
	default:
		t.Error("the box wasn't touched")
	}

	// The watch ends with the box.
	if err := g.Sleep(t.Context(), "demo"); err != nil {
		t.Fatal(err)
	}
	<-fw.ended
}
