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
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

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
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		envs: make(map[string]store.Env), snaps: make(map[string][]store.Snapshot), changing: make(map[string]bool),
	}
}

var validID = regexp.MustCompile(`^[a-z0-9-]+$`)

func (f *fakeStore) Create(_ context.Context, id string, opts store.CreateOptions) (store.Env, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !validID.MatchString(id) {
		return store.Env{}, store.ErrInvalidID
	}
	if env, ok := f.envs[id]; ok {
		return env, nil
	}
	path := opts.Workspace
	if path == "" {
		path = "/pool/envs/" + id + "/live"
	}
	env := store.Env{ID: id, Workspace: path, Created: created, BoxToken: boxToken}
	f.envs[id] = env
	return env, nil
}

func (f *fakeStore) Get(_ context.Context, id string) (store.Env, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getLocked(id)
}

func (f *fakeStore) getLocked(id string) (store.Env, error) {
	if !validID.MatchString(id) {
		return store.Env{}, store.ErrInvalidID
	}
	env, ok := f.envs[id]
	if !ok {
		return store.Env{}, fmt.Errorf("environment %s: %w", id, store.ErrNotFound)
	}
	return env, nil
}

func (f *fakeStore) FindByPath(_ context.Context, path string) (store.Env, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, env := range f.envs {
		if path == env.Workspace || strings.HasPrefix(path, env.Workspace+"/") {
			return env, nil
		}
	}
	return store.Env{}, fmt.Errorf("environment at %s: %w", path, store.ErrNotFound)
}

func (f *fakeStore) DeleteSnapshot(_ context.Context, env, snap string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.getLocked(env); err != nil {
		return err
	}
	f.snaps[env] = slices.DeleteFunc(f.snaps[env], func(s store.Snapshot) bool { return s.ID == snap })
	return nil
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

func (f *fakeStore) Snapshot(_ context.Context, env, snap string) error {
	f.mu.Lock()
	if f.changing[env] {
		f.mu.Unlock()
		return fmt.Errorf("snapshot %s/%s overlaps another change", env, snap)
	}
	f.changing[env] = true
	f.mu.Unlock()
	// Give an overlapping call the chance to run.
	runtime.Gosched()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changing[env] = false
	if _, err := f.getLocked(env); err != nil {
		return err
	}
	if !slices.ContainsFunc(f.snaps[env], func(s store.Snapshot) bool { return s.ID == snap }) {
		f.snaps[env] = append(f.snaps[env], store.Snapshot{ID: snap, Created: created})
	}
	return nil
}

func (f *fakeStore) Snapshots(_ context.Context, env string) ([]store.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.getLocked(env); err != nil {
		return nil, err
	}
	return slices.Clone(f.snaps[env]), nil
}

func (f *fakeStore) Restore(_ context.Context, env, snap string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.getLocked(env); err != nil {
		return err
	}
	if slices.ContainsFunc(f.snaps[env], func(s store.Snapshot) bool { return s.ID == snap }) {
		return nil
	}
	return fmt.Errorf("snapshot %s/%s: %w", env, snap, store.ErrNotFound)
}

// newAPI serves the API over a unix socket, with a fake store and a fake
// driver whose boxes all run commands with a real boxexec server.
func newAPI(t *testing.T) (*api.Client, *driver.Fake) {
	t.Helper()
	return newAPIWith(t, nil)
}

// newAPIWith is newAPI with the fake driver wrapped by wrap, if it isn't nil.
func newAPIWith(t *testing.T, wrap func(*driver.Fake) api.Driver) (*api.Client, *driver.Fake) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	mux := http.NewServeMux()
	mux.Handle(boxexec.Path, boxexec.NewServer(boxToken, t.TempDir(), nil, log))
	box := httptest.NewServer(mux)
	t.Cleanup(box.Close)

	drv := driver.NewFake(box.Listener.Addr().String())
	var d api.Driver = drv
	if wrap != nil {
		d = wrap(drv)
	}
	srv := httptest.NewUnstartedServer(api.NewServer(newFakeStore(), d, log).Handler())
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
	if _, err := c.Create(t.Context(), id, api.CreateOptions{}); err != nil {
		t.Fatalf("Create(%s) error: %v", id, err)
	}
}

// errCode returns the code of an *api.Error in err's chain, or "".
func errCode(err error) string {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return ""
	}
	return apiErr.Code
}

func TestCreateGetList(t *testing.T) {
	c, _ := newAPI(t)
	ctx := t.Context()
	want := api.Env{ID: "demo", Workspace: "/pool/envs/demo/live", Created: created, State: driver.Asleep}

	for range 2 { // creating again returns the same environment
		got, err := c.Create(ctx, "demo", api.CreateOptions{})
		if err != nil {
			t.Fatalf("Create(demo) error: %v", err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Create(demo) mismatch (-want +got):\n%s", diff)
		}
	}
	got, err := c.Get(ctx, "demo")
	if err != nil {
		t.Fatalf("Get(demo) error: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Get(demo) mismatch (-want +got):\n%s", diff)
	}
	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if diff := cmp.Diff([]api.Env{want}, list); diff != "" {
		t.Errorf("List() mismatch (-want +got):\n%s", diff)
	}
}

func TestErrors(t *testing.T) {
	c, _ := newAPI(t)
	mustCreate(t, c, "demo")
	tests := []struct {
		name     string
		call     func(ctx context.Context) error
		wantCode string
	}{
		{"get unknown", func(ctx context.Context) error { _, err := c.Get(ctx, "nope"); return err }, api.CodeNotFound},
		{"get invalid", func(ctx context.Context) error { _, err := c.Get(ctx, "Bad_ID"); return err }, api.CodeInvalid},
		{"create invalid", func(ctx context.Context) error {
			_, err := c.Create(ctx, "Bad_ID", api.CreateOptions{})
			return err
		}, api.CodeInvalid},
		{"delete unknown", func(ctx context.Context) error { return c.Delete(ctx, "nope") }, api.CodeNotFound},
		{"snapshot unknown", func(ctx context.Context) error {
			_, err := c.Snapshot(ctx, "nope", "s1")
			return err
		}, api.CodeNotFound},
		{"restore unknown snapshot", func(ctx context.Context) error { return c.Restore(ctx, "demo", "nope") }, api.CodeNotFound},
		{"wake unknown", func(ctx context.Context) error { return c.Wake(ctx, "nope") }, api.CodeNotFound},
		{"exec unknown", func(ctx context.Context) error {
			_, err := c.Exec(ctx, "nope", boxexec.Request{Argv: []string{"true"}}, boxexec.Streams{Stdout: io.Discard, Stderr: io.Discard})
			return err
		}, api.CodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(t.Context()); errCode(err) != tt.wantCode {
				t.Errorf("error = %v, want code %q", err, tt.wantCode)
			}
		})
	}
}

func TestSnapshotRestore(t *testing.T) {
	c, drv := newAPI(t)
	ctx := t.Context()
	mustCreate(t, c, "demo")

	snap, err := c.Snapshot(ctx, "demo", "s1")
	if err != nil {
		t.Fatalf("Snapshot(demo, s1) error: %v", err)
	}
	gen, err := c.Snapshot(ctx, "demo", "")
	if err != nil {
		t.Fatalf("Snapshot(demo, \"\") error: %v", err)
	}
	if gen.ID == "" {
		t.Errorf("Snapshot(demo, \"\") ID is empty, want a generated one")
	}
	snaps, err := c.Snapshots(ctx, "demo")
	if err != nil {
		t.Fatalf("Snapshots(demo) error: %v", err)
	}
	var ids []string
	for _, s := range snaps {
		ids = append(ids, s.ID)
	}
	if diff := cmp.Diff([]string{snap.ID, gen.ID}, ids); diff != "" {
		t.Errorf("Snapshots(demo) mismatch (-want +got):\n%s", diff)
	}

	if err := c.Wake(ctx, "demo"); err != nil {
		t.Fatalf("Wake(demo) error: %v", err)
	}
	if env, err := c.Get(ctx, "demo"); err != nil || env.State != driver.Running {
		t.Fatalf("after Wake: Get(demo) = %v, %v, want state %s", env.State, err, driver.Running)
	}
	if err := c.Restore(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Restore(demo, s1) error: %v", err)
	}
	if env, err := c.Get(ctx, "demo"); err != nil || env.State != driver.Asleep {
		t.Errorf("after Restore: Get(demo) = %v, %v, want state %s", env.State, err, driver.Asleep)
	}
	// The next exec must wake the box again, not reuse the old address.
	if _, err := c.Exec(ctx, "demo", boxexec.Request{Argv: []string{"true"}}, boxexec.Streams{Stdout: io.Discard, Stderr: io.Discard}); err != nil {
		t.Fatalf("Exec(demo) after Restore error: %v", err)
	}
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
	if env, err := c.Get(t.Context(), "demo"); err != nil || env.State != driver.Running {
		t.Errorf("Get(demo) = %v, %v, want state %s", env.State, err, driver.Running)
	}
}

// A large prompt on stdin reaches the box in frames of up to 32 KiB, more than
// a WebSocket reads by default.
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
	if _, err := c.Get(ctx, "demo"); errCode(err) != api.CodeNotFound {
		t.Errorf("Get(demo) after Delete error = %v, want code %s", err, api.CodeNotFound)
	}
	if list, err := c.List(ctx); err != nil || len(list) != 0 {
		t.Errorf("List() after Delete = %v, %v, want none", list, err)
	}
}

// Every lifecycle call is safe to repeat: a caller that retries after a lost
// answer must not get an error, a second box or an extra wake.
func TestLifecycleRepeats(t *testing.T) {
	c, drv := newAPI(t)
	ctx := t.Context()
	create := func() error { _, err := c.Create(ctx, "demo", api.CreateOptions{}); return err }
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
		env, err := c.Get(ctx, "demo")
		if err != nil {
			t.Fatalf("%s: Get(demo) error: %v", step.name, err)
		}
		if env.State != step.wantState || drv.Wakes() != step.wantWakes {
			t.Errorf("%s: state %s, %d wakes; want %s, %d", step.name, env.State, drv.Wakes(), step.wantState, step.wantWakes)
		}
	}
	if err := c.Delete(ctx, "demo"); err != nil {
		t.Fatalf("Delete(demo) error: %v", err)
	}
	if _, err := c.Get(ctx, "demo"); errCode(err) != api.CodeNotFound {
		t.Errorf("Get(demo) after Delete error = %v, want code %s", err, api.CodeNotFound)
	}
}

// Concurrent mutations of one environment are serialized by the server and
// must not race (run with -race).
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
			if _, err := c.Snapshot(ctx, env, snap); err != nil {
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
	if err := c.Restore(ctx, "demo", "typo"); errCode(err) != api.CodeNotFound {
		t.Fatalf("Restore(demo, typo) error = %v, want code %s", err, api.CodeNotFound)
	}
	if env, err := c.Get(ctx, "demo"); err != nil || env.State != driver.Running {
		t.Errorf("after a failed Restore: Get(demo) = %v, %v, want state %s", env.State, err, driver.Running)
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

func (d *blockingDelete) Delete(ctx context.Context, env string) error {
	close(d.started)
	<-d.release
	return d.Fake.Delete(ctx, env)
}

// A client that goes away must not leave a delete half done: the box gone
// but the environment still listed.
func TestDeleteOutlivesClient(t *testing.T) {
	d := &blockingDelete{started: make(chan struct{}), release: make(chan struct{})}
	c, _ := newAPIWith(t, func(f *driver.Fake) api.Driver { d.Fake = f; return d })
	mustCreate(t, c, "demo")

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- c.Delete(ctx, "demo") }()
	<-d.started
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete(demo) with a cancelled client error = %v, want %v", err, context.Canceled)
	}
	close(d.release)
	// The next change waits for the delete, which must have finished.
	if _, err := c.Snapshot(t.Context(), "demo", "s1"); errCode(err) != api.CodeNotFound {
		t.Errorf("Snapshot(demo) after the delete error = %v, want code %s", err, api.CodeNotFound)
	}
}

// staleWake is a driver whose first wake reports an address nothing
// listens on, like a box whose pod has just gone.
type staleWake struct {
	*driver.Fake
	dead  string
	waked bool
}

func (d *staleWake) Wake(ctx context.Context, env string) (string, error) {
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
	c, _ := newAPIWith(t, func(f *driver.Fake) api.Driver { d.Fake = f; return d })
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
	c, _ := newAPIWith(t, func(f *driver.Fake) api.Driver {
		drv = &countStates{Fake: f}
		return drv
	})
	ctx := t.Context()
	const path = "/pool/worktrees/fern/main"
	if _, err := c.Create(ctx, "wt", api.CreateOptions{Path: path}); err != nil {
		t.Fatalf("Create(wt, %s) error: %v", path, err)
	}
	for _, p := range []string{path, path + "/src/pkg"} {
		env, err := c.FindByPath(ctx, p)
		if err != nil || env.ID != "wt" || env.Workspace != path {
			t.Errorf("FindByPath(%s) = %+v, %v, want wt at %s", p, env, err, path)
		}
	}
	if _, err := c.FindByPath(ctx, "/pool/worktrees/fern"); errCode(err) != api.CodeNotFound {
		t.Errorf("FindByPath(parent) error = %v, want code %s", err, api.CodeNotFound)
	}
	// fern-claude looks its environment up before every Claude start.
	if n := drv.n.Load(); n != 0 {
		t.Errorf("lookups by path asked Kubernetes for box states %d times, want 0", n)
	}
}

// T3 deletes checkpoints after a revert and reuses their names.
func TestSnapshotDeleteAndReplace(t *testing.T) {
	c, _ := newAPI(t)
	ctx := t.Context()
	mustCreate(t, c, "demo")
	for _, id := range []string{"s1", "s2"} {
		if _, err := c.Snapshot(ctx, "demo", id); err != nil {
			t.Fatalf("Snapshot(demo, %s) error: %v", id, err)
		}
	}
	for range 2 { // deleting again is fine
		if err := c.DeleteSnapshot(ctx, "demo", "s2"); err != nil {
			t.Fatalf("DeleteSnapshot(demo, s2) error: %v", err)
		}
	}
	if _, err := c.ReplaceSnapshot(ctx, "demo", "s1"); err != nil {
		t.Fatalf("ReplaceSnapshot(demo, s1) error: %v", err)
	}
	snaps, err := c.Snapshots(ctx, "demo")
	if err != nil {
		t.Fatalf("Snapshots(demo) error: %v", err)
	}
	var ids []string
	for _, s := range snaps {
		ids = append(ids, s.ID)
	}
	if diff := cmp.Diff([]string{"s1"}, ids); diff != "" {
		t.Errorf("Snapshots(demo) mismatch (-want +got):\n%s", diff)
	}
	if err := c.DeleteSnapshot(ctx, "nope", "s1"); errCode(err) != api.CodeNotFound {
		t.Errorf("DeleteSnapshot(nope, s1) error = %v, want code %s", err, api.CodeNotFound)
	}
}
