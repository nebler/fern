package tools_test

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/goleak"

	"github.com/nebler/fern/internal/api"
	"github.com/nebler/fern/internal/api/server"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
	"github.com/nebler/fern/internal/tools"
)

// notrace records nothing.
var notrace = noop.NewTracerProvider().Tracer("")

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// boxToken is the token of every box the fake driver runs; the test box
// accepts it.
const boxToken = runsc.FakeToken

// fakeStore keeps environments' records in memory, and no snapshots: the
// tools take none, and a fork is a new record.
type fakeStore struct {
	mu   sync.Mutex
	envs map[string]store.Env
}

func newFakeStore() *fakeStore { return &fakeStore{envs: make(map[string]store.Env)} }

func (f *fakeStore) Create(_ context.Context, id string, opts store.CreateOptions) (store.Env, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if env, ok := f.envs[id]; ok {
		return env, nil
	}
	workspace := opts.Workspace
	if workspace == "" {
		workspace = "/pool/envs/" + id + "/live"
	}
	env := store.Env{ID: id, Workspace: workspace}
	f.envs[id] = env
	return env, nil
}

func (f *fakeStore) Get(_ context.Context, id string) (store.Env, error) {
	if err := store.CheckEnvID(id); err != nil {
		return store.Env{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	env, ok := f.envs[id]
	if !ok {
		return store.Env{}, fmt.Errorf("environment %s: %w", id, store.ErrNotFound)
	}
	return env, nil
}

func (f *fakeStore) List(context.Context) ([]store.Env, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.SortedFunc(maps.Values(f.envs), func(a, b store.Env) int { return strings.Compare(a.ID, b.ID) }), nil
}

func (f *fakeStore) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.envs, id)
	return nil
}

// FindByPath is for the API server that starts the agents (demoBox.agents),
// which looks no environment up by path here.
func (f *fakeStore) FindByPath(_ context.Context, path string) (store.Env, error) {
	return store.Env{}, fmt.Errorf("environment at %s: %w", path, store.ErrNotFound)
}

func (f *fakeStore) Snapshot(context.Context, string, string) error { return nil }

func (f *fakeStore) DeleteSnapshot(context.Context, string, string) error { return nil }

func (f *fakeStore) Snapshots(context.Context, string) ([]store.Snapshot, error) { return nil, nil }

func (f *fakeStore) HasSnapshot(context.Context, store.SnapshotRef) (bool, error) { return false, nil }

func (f *fakeStore) Restore(context.Context, string, string) error { return nil }

func (f *fakeStore) Prune(context.Context, string) error { return nil }

func (f *fakeStore) PruneGoldens(context.Context, string) error { return nil }

func (f *fakeStore) Orphans(context.Context) ([]string, error) { return nil, nil }

func (f *fakeStore) BuildSource(context.Context, string, store.SnapshotRef) error { return nil }

func (f *fakeStore) Sources(context.Context, string) ([]store.SnapshotRef, error) { return nil, nil }

func (f *fakeStore) StripSource(context.Context, string) error { return nil }

func (f *fakeStore) SetBuildFailure(context.Context, string, *store.BuildFailure) error { return nil }

func (f *fakeStore) BuildFailure(context.Context, string) (store.BuildFailure, bool, error) {
	return store.BuildFailure{}, false, nil
}

func (f *fakeStore) Space() (store.Space, error) { return store.Space{}, nil }

// serveSocket serves h on a unix socket and returns a client of it.
func serveSocket(t testing.TB, h http.Handler) *api.Client {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	// t.TempDir is too long for a socket path when test names are long.
	dir, err := os.MkdirTemp("", "fern")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	ln, err := new(net.ListenConfig).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return api.NewClient(socket)
}

// agents serves the daemon's API for the box's environments, whose commands
// in agent boxes get their agents' tools from tl, and returns a client of
// it: an agent starts through the API's exec.
func (b demoBox) agents(t *testing.T, tl *tools.Server) *api.Client {
	t.Helper()
	srv := server.New(b.st, b.drv, b.envs, true, notrace, b.log)
	srv.StartAgent = tl.StartAgent
	return serveSocket(t, srv.Handler())
}
