package api_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/api"
	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/driver"
	"github.com/nebler/fern/internal/store"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// boxToken is the token of every fake environment; the test box accepts it.
const boxToken = "box-token"

var created = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// fakeStore keeps environments in memory. It is safe for concurrent use,
// but fails a snapshot that overlaps another of the same environment: the
// server must run changes of one environment one at a time.
type fakeStore struct {
	mu       sync.Mutex
	envs     map[string]store.Env
	snaps    map[string][]store.Snapshot
	changing map[string]bool
	taken    int // snapshots taken, so each gets its own time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		envs: make(map[string]store.Env), snaps: make(map[string][]store.Snapshot), changing: make(map[string]bool),
	}
}

func (f *fakeStore) Prune(context.Context, string) error { return nil }

func (f *fakeStore) Orphans(context.Context) ([]string, error) { return nil, nil }

func (f *fakeStore) BuildSource(context.Context, string, store.SnapshotRef) error { return nil }

func (f *fakeStore) StripSource(context.Context, string) error { return nil }

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
	env := store.Env{ID: id, Workspace: workspace, Created: created, BoxToken: boxToken}
	f.envs[id] = env
	return env, nil
}

func (f *fakeStore) Get(_ context.Context, id string) (store.Env, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getLocked(id)
}

func (f *fakeStore) getLocked(id string) (store.Env, error) {
	if err := store.CheckEnvID(id); err != nil {
		return store.Env{}, err
	}
	env, ok := f.envs[id]
	if !ok {
		return store.Env{}, fmt.Errorf("environment %s: %w", id, store.ErrNotFound)
	}
	return env, nil
}

// FindByPath matches the workspace only: the store's own tests cover paths
// inside it.
func (f *fakeStore) FindByPath(_ context.Context, path string) (store.Env, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, env := range f.envs {
		if env.Workspace == path {
			return env, nil
		}
	}
	return store.Env{}, fmt.Errorf("environment at %s: %w", path, store.ErrNotFound)
}

func (f *fakeStore) List(context.Context) ([]store.Env, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.SortedFunc(maps.Values(f.envs), func(a, b store.Env) int { return strings.Compare(a.ID, b.ID) }), nil
}

func (f *fakeStore) Delete(ctx context.Context, id string) error {
	// Like the real store, which runs btrfs with ctx.
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.envs, id)
	delete(f.snaps, id)
	return nil
}

func (f *fakeStore) Snapshot(_ context.Context, id, snapID string) error {
	f.mu.Lock()
	if f.changing[id] {
		f.mu.Unlock()
		return fmt.Errorf("snapshot %s/%s overlaps another change", id, snapID)
	}
	f.changing[id] = true
	f.mu.Unlock()
	// Give an overlapping call the chance to run.
	runtime.Gosched()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changing[id] = false
	if _, err := f.getLocked(id); err != nil {
		return err
	}
	if snapID == "" {
		return fmt.Errorf("snapshot %q: %w", snapID, store.ErrInvalidID)
	}
	if !slices.ContainsFunc(f.snaps[id], func(s store.Snapshot) bool { return s.ID == snapID }) {
		f.taken++
		f.snaps[id] = append(f.snaps[id], store.Snapshot{ID: snapID, Created: created.Add(time.Duration(f.taken) * time.Second)})
	}
	return nil
}

func (f *fakeStore) DeleteSnapshot(_ context.Context, id, snapID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.getLocked(id); err != nil {
		return err
	}
	f.snaps[id] = slices.DeleteFunc(f.snaps[id], func(s store.Snapshot) bool { return s.ID == snapID })
	return nil
}

func (f *fakeStore) Snapshots(_ context.Context, id string) ([]store.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.getLocked(id); err != nil {
		return nil, err
	}
	return slices.Clone(f.snaps[id]), nil
}

func (f *fakeStore) Restore(_ context.Context, id, snapID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.getLocked(id); err != nil {
		return err
	}
	if slices.ContainsFunc(f.snaps[id], func(s store.Snapshot) bool { return s.ID == snapID }) {
		return nil
	}
	return fmt.Errorf("snapshot %s/%s: %w", id, snapID, store.ErrNotFound)
}

// newAPI serves the API over a unix socket, with a fake store and a fake
// driver whose boxes all run commands with a real boxexec server.
func newAPI(t *testing.T) (*api.Client, *driver.Fake) {
	t.Helper()
	return newAPIWith(t, nil)
}

// driverWith is what a test's wrapped fake driver must do.
type driverWith interface {
	actor.Driver
	api.Driver
}

// newServer returns the API's handler for st and drv, with their actors
// running until the test ends.
func newServer(t testing.TB, st *fakeStore, drv actor.Driver) http.Handler {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	envs := actor.New(st, drv, actor.Idle{}, "box", log)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		if err := envs.Run(ctx); err != nil {
			t.Errorf("Run() error: %v", err)
		}
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return api.NewServer(st, drv, envs, log).Handler()
}

// newAPIWith is newAPI with the fake driver wrapped by wrap, if it isn't nil.
func newAPIWith(t testing.TB, wrap func(*driver.Fake) driverWith) (*api.Client, *driver.Fake) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	mux := http.NewServeMux()
	mux.Handle(boxexec.Path, boxexec.NewServer(boxToken, t.TempDir(), nil, log))
	box := httptest.NewServer(mux)
	t.Cleanup(box.Close)

	drv := driver.NewFake(box.Listener.Addr().String())
	var d driverWith = drv
	if wrap != nil {
		d = wrap(drv)
	}
	srv := httptest.NewUnstartedServer(newServer(t, newFakeStore(), d))
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
	return api.NewClient(socket), drv
}

func mustCreate(t *testing.T, c *api.Client, id string) {
	t.Helper()
	if err := c.Create(t.Context(), id, "", nil); err != nil {
		t.Fatalf("Create(%s) error: %v", id, err)
	}
}

// status returns the HTTP status of an *api.Error in err's chain, or 0.
func status(err error) int {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return 0
	}
	return apiErr.Status
}

// stateOf returns the state List reports for environment id.
func stateOf(t *testing.T, c *api.Client, id string) driver.State {
	t.Helper()
	envs, err := c.List(t.Context())
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	i := slices.IndexFunc(envs, func(e api.Env) bool { return e.ID == id })
	if i < 0 {
		t.Fatalf("List() has no %s", id)
	}
	return envs[i].State
}

func run(t *testing.T, c *api.Client, id string, argv ...string) {
	t.Helper()
	if _, err := c.Exec(t.Context(), id, boxexec.Request{Argv: argv}, boxexec.Streams{Stdout: io.Discard, Stderr: io.Discard}); err != nil {
		t.Fatalf("Exec(%s, %q) error: %v", id, argv, err)
	}
}

func TestCreateList(t *testing.T) {
	c, _ := newAPI(t)
	mustCreate(t, c, "demo")
	mustCreate(t, c, "demo") // creating again changes nothing
	list, err := c.List(t.Context())
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	want := []api.Env{{ID: "demo", Workspace: "/pool/envs/demo/live", Created: created, State: driver.Asleep}}
	if diff := cmp.Diff(want, list); diff != "" {
		t.Errorf("List() mismatch (-want +got):\n%s", diff)
	}
}

func TestErrors(t *testing.T) {
	c, _ := newAPI(t)
	mustCreate(t, c, "demo")
	tests := []struct {
		name       string
		call       func(ctx context.Context) error
		wantStatus int
	}{
		{"create invalid", func(ctx context.Context) error { return c.Create(ctx, "Bad_ID", "", nil) }, http.StatusBadRequest},
		{"delete unknown", func(ctx context.Context) error { return c.Delete(ctx, "nope") }, http.StatusNotFound},
		{"snapshot unknown", func(ctx context.Context) error { return c.Snapshot(ctx, "nope", "s1", false) }, http.StatusNotFound},
		{"snapshot without a name", func(ctx context.Context) error { return c.Snapshot(ctx, "demo", "", false) }, http.StatusBadRequest},
		{"restore unknown snapshot", func(ctx context.Context) error { return c.Restore(ctx, "demo", "nope") }, http.StatusNotFound},
		{"wake unknown", func(ctx context.Context) error { return c.Wake(ctx, "nope") }, http.StatusNotFound},
		{"exec unknown", func(ctx context.Context) error {
			_, err := c.Exec(ctx, "nope", boxexec.Request{Argv: []string{"true"}}, boxexec.Streams{Stdout: io.Discard, Stderr: io.Discard})
			return err
		}, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(t.Context()); status(err) != tt.wantStatus {
				t.Errorf("error = %v, want status %d", err, tt.wantStatus)
			}
		})
	}
}

func TestSnapshotRestore(t *testing.T) {
	c, drv := newAPI(t)
	ctx := t.Context()
	mustCreate(t, c, "demo")
	if err := c.Snapshot(ctx, "demo", "s1", false); err != nil {
		t.Fatalf("Snapshot(demo, s1) error: %v", err)
	}
	if err := c.Wake(ctx, "demo"); err != nil {
		t.Fatalf("Wake(demo) error: %v", err)
	}
	if got := stateOf(t, c, "demo"); got != driver.Running {
		t.Fatalf("after Wake: state %s, want %s", got, driver.Running)
	}
	if err := c.Restore(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Restore(demo, s1) error: %v", err)
	}
	if got := stateOf(t, c, "demo"); got != driver.Asleep {
		t.Errorf("after Restore: state %s, want %s", got, driver.Asleep)
	}
	// The next exec must wake the box again, not reuse the old address.
	run(t, c, "demo", "true")
	if got := drv.Wakes(); got != 2 {
		t.Errorf("Wakes() = %d, want 2", got)
	}
}

func TestExecWakesOnce(t *testing.T) {
	c, drv := newAPI(t)
	mustCreate(t, c, "demo")
	req := boxexec.Request{Argv: []string{"sh", "-c", "cat; echo err >&2; exit 4"}}
	for i := range 2 {
		var stdout, stderr bytes.Buffer
		code, err := c.Exec(t.Context(), "demo", req, boxexec.Streams{Stdin: bytes.NewBufferString("hi"), Stdout: &stdout, Stderr: &stderr})
		if err != nil {
			t.Fatalf("Exec #%d error: %v", i+1, err)
		}
		if code != 4 || stdout.String() != "hi" || stderr.String() != "err\n" {
			t.Errorf("Exec #%d = %d, stdout %q, stderr %q; want 4, %q, %q",
				i+1, code, stdout.String(), stderr.String(), "hi", "err\n")
		}
		if got := drv.Wakes(); got != 1 {
			t.Errorf("after exec #%d: Wakes() = %d, want 1", i+1, got)
		}
	}
	if got := stateOf(t, c, "demo"); got != driver.Running {
		t.Errorf("state %s, want %s", got, driver.Running)
	}
}

// A large prompt on stdin reaches the box in frames of up to 64 KiB, more
// than a WebSocket reads by default.
func TestExecLargeStdin(t *testing.T) {
	c, _ := newAPI(t)
	mustCreate(t, c, "demo")
	in := strings.Repeat("0123456789", 10_000)
	var stdout bytes.Buffer
	code, err := c.Exec(t.Context(), "demo", boxexec.Request{Argv: []string{"cat"}}, boxexec.Streams{Stdin: strings.NewReader(in), Stdout: &stdout, Stderr: io.Discard})
	if err != nil || code != 0 {
		t.Fatalf("Exec(cat) = %d, %v, want 0, nil", code, err)
	}
	if stdout.String() != in {
		t.Errorf("Exec(cat) echoed %d bytes, want the %d sent", stdout.Len(), len(in))
	}
}

func TestDelete(t *testing.T) {
	c, _ := newAPI(t)
	ctx := t.Context()
	mustCreate(t, c, "demo")
	if err := c.Delete(ctx, "demo"); err != nil {
		t.Fatalf("Delete(demo) error: %v", err)
	}
	if list, err := c.List(ctx); err != nil || len(list) != 0 {
		t.Errorf("List() after Delete = %v, %v, want none", list, err)
	}
	if err := c.Delete(ctx, "demo"); status(err) != http.StatusNotFound {
		t.Errorf("Delete(demo) again error = %v, want status %d", err, http.StatusNotFound)
	}
}

// Every lifecycle call is safe to repeat: a caller that retries after a lost
// answer must not get an error, a second box or an extra wake.
func TestLifecycleRepeats(t *testing.T) {
	c, drv := newAPI(t)
	ctx := t.Context()
	create := func() error { return c.Create(ctx, "demo", "", nil) }
	wake := func() error { return c.Wake(ctx, "demo") }
	sleep := func() error { return c.Sleep(ctx, "demo") }
	steps := []struct {
		name      string
		do        func() error
		wantState driver.State
		wantWakes int
	}{
		{"create", create, driver.Asleep, 0},
		{"create again", create, driver.Asleep, 0},
		{"wake", wake, driver.Running, 1},
		{"wake again", wake, driver.Running, 1},
		{"create while running", create, driver.Running, 1},
		{"sleep", sleep, driver.Asleep, 1},
		{"sleep again", sleep, driver.Asleep, 1},
		{"wake after sleep", wake, driver.Running, 2},
	}
	for _, step := range steps {
		if err := step.do(); err != nil {
			t.Fatalf("%s: error %v", step.name, err)
		}
		if got := stateOf(t, c, "demo"); got != step.wantState || drv.Wakes() != step.wantWakes {
			t.Errorf("%s: state %s, %d wakes; want %s, %d", step.name, got, drv.Wakes(), step.wantState, step.wantWakes)
		}
	}
}

// Concurrent changes of one environment are run one at a time: the fake
// store fails a snapshot that overlaps another.
func TestConcurrentSnapshots(t *testing.T) {
	c, _ := newAPI(t)
	ctx := t.Context()
	envs := []string{"a", "b"}
	for _, env := range envs {
		mustCreate(t, c, env)
	}
	const n = 20
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			env, snap := envs[i%len(envs)], fmt.Sprintf("s%02d", i)
			if err := c.Snapshot(ctx, env, snap, false); err != nil {
				t.Errorf("Snapshot(%s, %s) error: %v", env, snap, err)
			}
		})
	}
	wg.Wait()
	var got []string
	for _, env := range envs {
		snaps, err := c.Snapshots(ctx, env)
		if err != nil {
			t.Fatalf("Snapshots(%s) error: %v", env, err)
		}
		for _, s := range snaps {
			got = append(got, s.ID)
		}
	}
	slices.Sort(got)
	var want []string
	for i := range n {
		want = append(want, fmt.Sprintf("s%02d", i))
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("snapshots mismatch (-want +got):\n%s", diff)
	}
}

// A restore of a snapshot that doesn't exist must not stop the box.
func TestRestoreUnknownKeepsBox(t *testing.T) {
	c, drv := newAPI(t)
	ctx := t.Context()
	mustCreate(t, c, "demo")
	if err := c.Wake(ctx, "demo"); err != nil {
		t.Fatalf("Wake(demo) error: %v", err)
	}
	if err := c.Restore(ctx, "demo", "typo"); status(err) != http.StatusNotFound {
		t.Fatalf("Restore(demo, typo) error = %v, want status %d", err, http.StatusNotFound)
	}
	if got := stateOf(t, c, "demo"); got != driver.Running {
		t.Errorf("after a failed Restore: state %s, want %s", got, driver.Running)
	}
	if got := drv.Wakes(); got != 1 {
		t.Errorf("Wakes() = %d, want 1", got)
	}
}

// blockingDelete is a driver whose Delete waits until release is closed.
type blockingDelete struct {
	*driver.Fake
	started chan struct{}
	release chan struct{}
}

func (d *blockingDelete) Delete(ctx context.Context, id string) error {
	close(d.started)
	<-d.release
	return d.Fake.Delete(ctx, id)
}

// A client that goes away must not leave a delete half done: the box gone
// but the environment still listed. The client's cancel always comes before
// the delete goes on; a second delete, which the actor runs after it, waits
// for it to end.
func TestDeleteOutlivesClient(t *testing.T) {
	st := newFakeStore()
	if _, err := st.Create(t.Context(), "demo", store.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	d := &blockingDelete{Fake: driver.NewFake(""), started: make(chan struct{}), release: make(chan struct{})}
	h := newServer(t, st, d)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, http.MethodDelete, "/envs/demo", nil))
	}()
	<-d.started
	cancel()
	close(d.release)
	<-done
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/envs/demo", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("second Delete(demo) status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if _, err := st.Get(t.Context(), "demo"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get(demo) after a delete whose client went away error = %v, want %v", err, store.ErrNotFound)
	}
}

// staleWake is a driver whose first wake reports an address nothing
// listens on, like a box whose pod has just gone.
type staleWake struct {
	*driver.Fake
	dead  string
	waked bool
}

func (d *staleWake) Wake(ctx context.Context, env store.Env) (string, error) {
	addr, err := d.Fake.Wake(ctx, env)
	if err == nil && !d.waked {
		d.waked = true
		return d.dead, nil
	}
	return addr, err
}

func TestExecRedialsStaleBox(t *testing.T) {
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	_ = ln.Close()
	d := &staleWake{dead: dead}
	c, _ := newAPIWith(t, func(f *driver.Fake) driverWith { d.Fake = f; return d })
	mustCreate(t, c, "demo")
	var stdout bytes.Buffer
	code, err := c.Exec(t.Context(), "demo", boxexec.Request{Argv: []string{"echo", "hi"}}, boxexec.Streams{Stdout: &stdout, Stderr: io.Discard})
	if err != nil || code != 0 || stdout.String() != "hi\n" {
		t.Errorf("Exec(echo hi) = %d, %q, %v, want 0, %q, nil", code, stdout.String(), err, "hi\n")
	}
}

// countStates counts how often the server asks for every box's state, which
// costs a Kubernetes call.
type countStates struct {
	*driver.Fake
	n atomic.Int32
}

func (d *countStates) States(ctx context.Context) (map[string]driver.State, error) {
	d.n.Add(1)
	return d.Fake.States(ctx)
}

func TestFindByPath(t *testing.T) {
	var drv *countStates
	c, _ := newAPIWith(t, func(f *driver.Fake) driverWith {
		drv = &countStates{Fake: f}
		return drv
	})
	ctx := t.Context()
	const path = "/pool/worktrees/fern/main"
	if err := c.Create(ctx, "wt", path, nil); err != nil {
		t.Fatalf("Create(wt, %s) error: %v", path, err)
	}
	if env, err := c.FindByPath(ctx, path); err != nil || env.ID != "wt" || env.Workspace != path {
		t.Errorf("FindByPath(%s) = %+v, %v, want wt at %s", path, env, err, path)
	}
	if _, err := c.FindByPath(ctx, "/pool/worktrees/fern"); status(err) != http.StatusNotFound {
		t.Errorf("FindByPath(parent) error = %v, want status %d", err, http.StatusNotFound)
	}
	// fern-claude looks its environment up before every Claude start. The
	// actors ask once, when they start.
	if n := drv.n.Load(); n != 1 {
		t.Errorf("asked Kubernetes for box states %d times, want 1", n)
	}
}

// T3 deletes checkpoints after a revert and reuses their names: --replace
// must take the snapshot again.
func TestSnapshotDeleteAndReplace(t *testing.T) {
	c, _ := newAPI(t)
	ctx := t.Context()
	mustCreate(t, c, "demo")
	for _, id := range []string{"s1", "s2"} {
		if err := c.Snapshot(ctx, "demo", id, false); err != nil {
			t.Fatalf("Snapshot(demo, %s) error: %v", id, err)
		}
	}
	if err := c.DeleteSnapshot(ctx, "demo", "s2"); err != nil {
		t.Fatalf("DeleteSnapshot(demo, s2) error: %v", err)
	}
	old, err := c.Snapshots(ctx, "demo")
	if err != nil {
		t.Fatalf("Snapshots(demo) error: %v", err)
	}
	if err := c.Snapshot(ctx, "demo", "s1", true); err != nil {
		t.Fatalf("Snapshot(demo, s1, replace) error: %v", err)
	}
	snaps, err := c.Snapshots(ctx, "demo")
	if err != nil {
		t.Fatalf("Snapshots(demo) error: %v", err)
	}
	if len(old) != 1 || len(snaps) != 1 || snaps[0].ID != "s1" || snaps[0].Created.Equal(old[0].Created) {
		t.Errorf("Snapshots(demo) = %v after replacing %v, want s1 taken again", snaps, old)
	}
	if err := c.DeleteSnapshot(ctx, "nope", "s1"); status(err) != http.StatusNotFound {
		t.Errorf("DeleteSnapshot(nope, s1) error = %v, want status %d", err, http.StatusNotFound)
	}
}
