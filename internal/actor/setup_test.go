package actor_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/setup"
	"github.com/nebler/fern/internal/store"
)

// A golden build runs the repository's setup script before its services'
// first start: in a box with the tools and no services, as a command, for
// as long as it may. A script that fails or runs past its limit leaves no
// golden, and the box asleep.
func TestGoldenSetupScript(t *testing.T) {
	tests := []struct {
		name   string
		files  []string // the scripts the repository has
		run    func(ctx context.Context) (int, error)
		script string // the one that runs
		built  bool   // a golden is made
	}{
		{
			name:   "a script that succeeds",
			files:  []string{".fern/setup"},
			run:    func(context.Context) (int, error) { return 0, nil },
			script: ".fern/setup", built: true,
		},
		{
			name:   ".fern's script replaces .agents'",
			files:  []string{".agents/setup", ".fern/setup"},
			run:    func(context.Context) (int, error) { return 0, nil },
			script: ".fern/setup", built: true,
		},
		{
			name:   "Amp's and Delta's place for it",
			files:  []string{".agents/setup"},
			run:    func(context.Context) (int, error) { return 0, nil },
			script: ".agents/setup", built: true,
		},
		{
			name:   "a script that fails",
			files:  []string{".fern/setup"},
			run:    func(context.Context) (int, error) { return 3, nil },
			script: ".fern/setup",
		},
		{
			name:  "a script that runs past its limit",
			files: []string{".fern/setup"},
			run: func(ctx context.Context) (int, error) {
				<-ctx.Done()
				return 0, ctx.Err()
			},
			script: ".fern/setup",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				repo := filepath.Join(t.TempDir(), "worktrees", "sample")
				for _, wt := range []string{"one", "two"} {
					for _, f := range append([]string{"services.yaml"}, tt.files...) {
						path := filepath.Join(repo, wt, f)
						if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, []byte("services: {pg: {run: ./pg.sh}}\n"), 0o755); err != nil {
							t.Fatal(err)
						}
					}
				}
				st, drv := newFakeStore(), runsc.NewFake("box")
				builder := store.BuilderID("sample")
				// What the script found when it ran.
				var mu sync.Mutex
				var ran []string
				var kind runsc.Kind
				var builtBefore []string
				cfg := actor.Config{
					Idle: idle, Image: "box", SetupTimeout: 30 * time.Minute,
					RunSetup: func(ctx context.Context, addr, token string, argv []string, _ io.Writer) (int, error) {
						mu.Lock()
						ran = append(ran, addr+" "+token+" "+argv[len(argv)-1])
						kind = drv.Woken(builder).Kind
						st.mu.Lock()
						builtBefore = slices.Clone(st.built)
						st.mu.Unlock()
						mu.Unlock()
						return tt.run(ctx)
					},
				}
				g := actor.New(st, drv, cfg, notrace, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() { _ = g.Run(ctx); close(done) }()
				defer func() { cancel(); <-done }()

				if err := g.Create(t.Context(), "one", store.CreateOptions{Workspace: filepath.Join(repo, "one")}); err != nil {
					t.Fatal(err)
				}
				// Past the script's limit: a build that waits for it is over.
				time.Sleep(31 * time.Minute)
				synctest.Wait()

				mu.Lock()
				if diff := cmp.Diff([]string{"box " + runsc.FakeToken + " " + tt.script}, ran); diff != "" {
					t.Errorf("the script's runs mismatch (-want +got):\n%s", diff)
				}
				if kind != runsc.Setup {
					t.Errorf("the script ran in a box of kind %d, want the one for setup scripts, %d", kind, runsc.Setup)
				}
				if diff := cmp.Diff([]string{"source " + builder + " from one/golden-source"}, builtBefore); diff != "" {
					t.Errorf("what the build had done when the script ran mismatch (-want +got):\n%s", diff)
				}
				mu.Unlock()
				if got := state(t, drv, builder); got != runsc.Asleep {
					t.Errorf("the builder's box after the build: %s, want %s", got, runsc.Asleep)
				}
				key, err := setup.Key(filepath.Join(repo, "one"), "box")
				if err != nil {
					t.Fatal(err)
				}
				st.mu.Lock()
				var want []string
				if tt.built {
					want = []string{key}
				}
				if diff := cmp.Diff(want, st.snaps[builder]); diff != "" {
					t.Errorf("the builder's snapshots mismatch (-want +got):\n%s", diff)
				}
				if got := st.snaps["one"]; len(got) != 0 {
					t.Errorf("one's snapshots after the build = %v, want the golden source gone", got)
				}
				st.mu.Unlock()
				if tt.built {
					// The services' first start came after the script, in the
					// builder's own kind of box.
					if got := drv.Woken(builder).Kind; got != runsc.Builder {
						t.Errorf("the builder's last box was of kind %d, want %d", got, runsc.Builder)
					}
				}

				if err := g.Create(t.Context(), "two", store.CreateOptions{Workspace: filepath.Join(repo, "two")}); err != nil {
					t.Fatal(err)
				}
				st.mu.Lock()
				defer st.mu.Unlock()
				var from *store.SnapshotRef
				if tt.built {
					from = &store.SnapshotRef{Env: builder, Snapshot: key}
				}
				if diff := cmp.Diff(from, st.from["two"]); diff != "" {
					t.Errorf("the next worktree's golden mismatch (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// A golden build that failed isn't made again from the same key for a
// quarter of an hour, however many new worktrees ask for it: a setup script
// that fails would run again for each, as long as an hour at a time.
func TestGoldenBuildNotRepeatedAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := filepath.Join(t.TempDir(), "worktrees", "sample")
		for _, wt := range []string{"one", "two", "three"} {
			for _, f := range []string{"services.yaml", ".fern/setup"} {
				path := filepath.Join(repo, wt, f)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("services: {pg: {run: ./pg.sh}}\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
		}
		st, drv := newFakeStore(), runsc.NewFake("box")
		var mu sync.Mutex
		runs := 0
		cfg := actor.Config{
			Idle: idle, Image: "box",
			RunSetup: func(context.Context, string, string, []string, io.Writer) (int, error) {
				mu.Lock()
				defer mu.Unlock()
				runs++
				return 1, nil
			},
		}
		g := actor.New(st, drv, cfg, notrace, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { _ = g.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()

		for _, step := range []struct {
			worktree string
			after    time.Duration // since the one before
			want     int           // times the script has run by then
		}{
			{"one", 0, 1},
			{"two", time.Minute, 1},
			{"three", 20 * time.Minute, 2},
		} {
			time.Sleep(step.after)
			if err := g.Create(t.Context(), step.worktree, store.CreateOptions{Workspace: filepath.Join(repo, step.worktree)}); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			mu.Lock()
			if runs != step.want {
				t.Errorf("after worktree %s the script has run %d times, want %d", step.worktree, runs, step.want)
			}
			mu.Unlock()
			st.mu.Lock()
			if got := st.snaps[step.worktree]; len(got) != 0 {
				t.Errorf("%s's snapshots = %v, want its golden source gone", step.worktree, got)
			}
			st.mu.Unlock()
			if got := state(t, drv, store.BuilderID("sample")); got != runsc.Asleep {
				t.Errorf("the builder's box after %s: %s, want %s", step.worktree, got, runsc.Asleep)
			}
		}
	})
}

// What has gone wrong on the node reaches the thread: the box with Claude
// is woken with a notice for a failed build of the checkout it has, and
// for a pool that takes no more snapshots. A failed build of other files,
// such as an older lockfile, is not this thread's.
func TestNoticesAtClaudesWake(t *testing.T) {
	tests := []struct {
		name   string
		change func(t *testing.T, worktree string, st *fakeStore) // after the build has failed
		want   []string                                           // a part of each notice, in order
	}{
		{
			name: "the build of this checkout failed",
			want: []string{"build of this repository's starting point failed"},
		},
		{
			name: "the checkout has changed since",
			change: func(t *testing.T, worktree string, _ *fakeStore) {
				if err := os.WriteFile(filepath.Join(worktree, "pnpm-lock.yaml"), []byte("newer\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "the pool is full too",
			change: func(_ *testing.T, _ string, st *fakeStore) {
				st.mu.Lock()
				defer st.mu.Unlock()
				st.space = store.Space{Pool: store.Volume{Total: 100 << 30, Free: 1 << 30}, Least: 5 << 30}
			},
			want: []string{"takes no new snapshots", "build of this repository's starting point failed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				worktree := filepath.Join(t.TempDir(), "worktrees", "sample", "one")
				for _, f := range []string{"services.yaml", ".fern/setup"} {
					path := filepath.Join(worktree, f)
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("services: {pg: {run: ./pg.sh}}\n"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				st, drv := newFakeStore(), runsc.NewFake("box")
				cfg := actor.Config{
					Idle: idle, Image: "box",
					RunSetup: func(_ context.Context, _, _ string, _ []string, out io.Writer) (int, error) {
						_, _ = io.WriteString(out, "pnpm: not found")
						return 127, nil
					},
				}
				g := actor.New(st, drv, cfg, notrace, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() { _ = g.Run(ctx); close(done) }()
				defer func() { cancel(); <-done }()

				if err := g.Create(t.Context(), "one", store.CreateOptions{Workspace: worktree}); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				failure, ok, _ := st.BuildFailure(t.Context(), store.BuilderID("sample"))
				if !ok || !strings.Contains(failure.Error, "pnpm: not found") {
					t.Fatalf("the builder's recorded failure = %+v, %t; want the script's output in it", failure, ok)
				}
				if tt.change != nil {
					tt.change(t, worktree, st)
				}
				if err := g.Wake(t.Context(), "one"); err != nil {
					t.Fatal(err)
				}
				got := noticesOf(t, drv.Woken("one"))
				if len(got) != len(tt.want) {
					t.Fatalf("notices = %q, want %d of them", got, len(tt.want))
				}
				for i, part := range tt.want {
					if !strings.Contains(got[i], part) {
						t.Errorf("notice %d = %q, want %q in it", i, got[i], part)
					}
				}
				if len(tt.want) > 0 && !strings.Contains(got[len(got)-1], "pnpm: not found") {
					t.Errorf("the build's notice = %q, want the script's output in it", got[len(got)-1])
				}
			})
		})
	}
}

// A box whose services.yaml can't be read isn't started, with the file's
// fault as the reason. One that runs already is kept: a daemon that
// restarts (an upgrade) adopts its boxes with their commands, whatever
// their repositories have become since.
func TestBrokenServicesFile(t *testing.T) {
	for _, running := range []bool{false, true} {
		name := "a box that isn't running"
		if running {
			name = "a box an earlier daemon left running"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				worktree := filepath.Join(t.TempDir(), "worktrees", "sample", "one")
				if err := os.MkdirAll(worktree, 0o755); err != nil {
					t.Fatal(err)
				}
				st, drv := newFakeStore(), runsc.NewFake("box")
				g := actor.New(st, drv, actor.Config{Idle: idle, Image: "box"}, notrace, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() { _ = g.Run(ctx); close(done) }()
				defer func() { cancel(); <-done }()
				if err := g.Create(t.Context(), "one", store.CreateOptions{Workspace: worktree}); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if running {
					if _, err := drv.Wake(t.Context(), runsc.Box{ID: "one"}); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(worktree, "services.yaml"), []byte("limits: [not, a, map]\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				err := g.Wake(t.Context(), "one")
				switch {
				case running && err != nil:
					t.Errorf("Wake() of a running box whose services.yaml broke error: %v, want it kept", err)
				case !running && (err == nil || !strings.Contains(err.Error(), "services.yaml")):
					t.Errorf("Wake() with a broken services.yaml error = %v, want the file's fault", err)
				}
			})
		})
	}
}
