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

func (f *fakeStore) Create(_ context.Context, id, repo string, _ io.Reader) (store.Env, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !validID.MatchString(id) {
		return store.Env{}, store.ErrInvalidID
	}
	if env, ok := f.envs[id]; ok {
		return env, nil
	}
	env := store.Env{ID: id, Repo: repo, Created: created, Token: boxToken}
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

func (f *fakeStore) Snapshot(_ context.Context, env, snap string) (store.Snapshot, error) {
	f.mu.Lock()
	if f.changing[env] {
		f.mu.Unlock()
		return store.Snapshot{}, fmt.Errorf("snapshot %s/%s overlaps another change", env, snap)
	}
	f.changing[env] = true
	f.mu.Unlock()
	// Give an overlapping call the chance to run.
	runtime.Gosched()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changing[env] = false
	if _, err := f.getLocked(env); err != nil {
		return store.Snapshot{}, err
	}
	if i := slices.IndexFunc(f.snaps[env], func(s store.Snapshot) bool { return s.ID == snap }); i >= 0 {
		return f.snaps[env][i], nil
	}
	s := store.Snapshot{ID: snap, Env: env, Created: created}
	f.snaps[env] = append(f.snaps[env], s)
	return s, nil
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
	if _, err := c.Create(t.Context(), id, "", nil); err != nil {
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
	want := api.Env{ID: "demo", Repo: "/src/demo", Created: created, State: driver.Asleep}

	for range 2 { // creating again returns the same environment
		got, err := c.Create(ctx, "demo", "/src/demo", nil)
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
			_, err := c.Create(ctx, "Bad_ID", "", nil)
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
			_, err := c.Exec(ctx, "nope", boxexec.Request{Argv: []string{"true"}}, nil, io.Discard, io.Discard, nil)
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
	if diff := cmp.Diff([]api.Snapshot{snap, gen}, snaps); diff != "" {
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
	if _, err := c.Exec(ctx, "demo", boxexec.Request{Argv: []string{"true"}}, nil, io.Discard, io.Discard, nil); err != nil {
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
		code, err := c.Exec(t.Context(), "demo", req, bytes.NewBufferString("hi"), &stdout, &stderr, nil)
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
	code, err := c.Exec(t.Context(), "demo", boxexec.Request{Argv: []string{"cat"}}, strings.NewReader(in), &stdout, io.Discard, nil)
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
	code, err := c.Exec(t.Context(), "demo", boxexec.Request{Argv: []string{"echo", "hi"}}, nil, &stdout, io.Discard, nil)
	if err != nil || code != 0 || stdout.String() != "hi\n" {
		t.Errorf("Exec(echo hi) = %d, %q, %v, want 0, %q, nil", code, stdout.String(), err, "hi\n")
	}
}
