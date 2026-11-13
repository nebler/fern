package actor_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
)

// node is a node that gives its boxes everything it can: a login, skill
// packs, T3's MCP server, previews and room to ask for more.
func node(agents bool) actor.Config {
	read := func(s string) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return s, nil }
	}
	return actor.Config{
		Image: "box", Agents: agents,
		Login: read("login"), Skills: read("safety design"), MCPPort: 7071, T3MCP: "127.0.0.1:30773", Preview: "localhost:30780",
		MaxLimits: runsc.Limits{CPUs: 4, Memory: 8 << 30, Pids: 9000},
	}
}

// workspace returns a new folder with yaml as its services.yaml, or with
// none if yaml is "".
func workspace(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	if yaml != "" {
		if err := os.WriteFile(filepath.Join(dir, "services.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// woken wakes box, of a new environment whose workspace is dir, on a node
// set up as cfg, and returns it as the driver got it. Call it inside a
// synctest bubble.
func woken(t *testing.T, cfg actor.Config, st *fakeStore, box, dir string) (runsc.Box, error) {
	t.Helper()
	drv := runsc.NewFake("box")
	g := actor.New(st, drv, cfg, notrace, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = g.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	env := store.EnvOfBox(box)
	if err := g.Create(ctx, env, store.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Not through Create, which would start a golden build of it.
	st.mu.Lock()
	st.paths[env] = dir
	st.mu.Unlock()
	err := g.Wake(ctx, box)
	return drv.Woken(box), err
}

// noticesOf returns what box's Claude is told at its start has gone wrong
// on the node.
func noticesOf(t *testing.T, box runsc.Box) []string {
	t.Helper()
	var notices []string
	for _, v := range box.Vars {
		if data, ok := strings.CutPrefix(v, "FERN_NOTICES="); ok {
			if err := json.Unmarshal([]byte(data), &notices); err != nil {
				t.Fatalf("%s: %v", v, err)
			}
		}
	}
	return notices
}

// What each kind of box is started with: its whole list of variables, in
// their order, and its limits. The driver adds only the token and the wake
// ID of each start (runsc's TestBoxSpec).
func TestBoxGets(t *testing.T) {
	const limits = "limits: {cpus: 3, memory: 4096, pids: 6000}\n"
	asks := workspace(t, limits) // its services.yaml asks for limits
	broken := workspace(t, "limits: [\n")
	plain := workspace(t, "")
	asked := runsc.Limits{CPUs: 3, Memory: 4 << 30, Pids: 6000}
	builder := store.BuilderID("r")
	full := store.Space{Pool: store.Volume{Total: 100 << 30, Free: 1 << 30}, Least: 5 << 30}
	notices, err := json.Marshal([]string{
		"Fern takes no new snapshots, so a revert can't bring back what this turn changes: " + full.Full().Error() + ".",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		cfg     actor.Config
		space   store.Space // the pool's room: when full, Claude gets a notice
		box     string
		dir     string // its workspace
		want    []string
		limits  runsc.Limits
		wantErr bool
	}{
		{
			name: "services",
			cfg:  node(true),
			box:  "demo", dir: asks,
			want: []string{
				"FERN_WORKSPACE=" + asks,
				"FERN_PREVIEW=http://{port}-demo.localhost:30780/",
			},
			limits: asked,
		},
		{
			name:  "services with Claude, as without agent boxes",
			cfg:   node(false),
			space: full,
			box:   "demo", dir: asks,
			want: []string{
				"FERN_WORKSPACE=" + asks,
				"CLAUDE_CONFIG_DIR=claude/demo",
				"CLAUDE_CODE_OAUTH_TOKEN=login",
				"FERN_SKILLS=safety design",
				"FERN_NOTICES=" + string(notices),
				"FERN_PREVIEW=http://{port}-demo.localhost:30780/",
			},
			limits: asked,
		},
		{
			name:  "agent",
			cfg:   node(true),
			space: full,
			box:   "demo.agent", dir: asks,
			want: []string{
				"FERN_WORKSPACE=" + asks,
				"FERN_SERVICES=off",
				"FERN_MCP=http://fern-daemon:7071",
				"CLAUDE_CONFIG_DIR=claude/demo",
				"CLAUDE_CODE_OAUTH_TOKEN=login",
				"FERN_SKILLS=safety design",
				"FERN_NOTICES=" + string(notices),
				"FERN_T3_ADDR=127.0.0.1:30773",
				"FERN_T3_MCP=http://fern-daemon:7071/t3/demo",
				"FERN_PREVIEW=http://{port}-demo.localhost:30780/",
				"FERN_BOX_LIMITS=3:4096:6000",
			},
			limits: runsc.DefaultLimits(),
		},
		{
			name: "agent, whose repository's services.yaml is broken",
			cfg:  node(true),
			box:  "demo.agent", dir: broken,
			want: []string{
				"FERN_WORKSPACE=" + broken,
				"FERN_SERVICES=off",
				"FERN_MCP=http://fern-daemon:7071",
				"CLAUDE_CONFIG_DIR=claude/demo",
				"CLAUDE_CODE_OAUTH_TOKEN=login",
				"FERN_SKILLS=safety design",
				"FERN_T3_ADDR=127.0.0.1:30773",
				"FERN_T3_MCP=http://fern-daemon:7071/t3/demo",
				"FERN_PREVIEW=http://{port}-demo.localhost:30780/",
			},
			limits: runsc.DefaultLimits(),
		},
		{
			name: "agent, on a node with no login, skills, T3 or previews",
			cfg:  actor.Config{Image: "box", Agents: true, MCPPort: 7071},
			box:  "demo.agent", dir: plain,
			want: []string{
				"FERN_WORKSPACE=" + plain,
				"FERN_SERVICES=off",
				"FERN_MCP=http://fern-daemon:7071",
				"CLAUDE_CONFIG_DIR=claude/demo",
				"FERN_BOX_LIMITS=2:2048:4096",
			},
			limits: runsc.DefaultLimits(),
		},
		{
			name: "builder",
			cfg:  node(true),
			box:  builder, dir: asks,
			want: []string{
				"FERN_WORKSPACE=" + asks,
				"FERN_ON_DEMAND=off",
				"FERN_PREVIEW=http://{port}-" + builder + ".localhost:30780/",
			},
			limits: asked,
		},
		{
			name: "services, whose services.yaml is broken",
			cfg:  node(true),
			box:  "demo", dir: broken,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st := newFakeStore()
				st.space = tc.space
				got, err := woken(t, tc.cfg, st, tc.box, tc.dir)
				if (err != nil) != tc.wantErr {
					t.Fatalf("Wake() error = %v, want error %t", err, tc.wantErr)
				}
				if diff := cmp.Diff(tc.want, got.Vars); diff != "" {
					t.Errorf("variables (-want +got):\n%s", diff)
				}
				if got.Limits != tc.limits {
					t.Errorf("limits = %+v, want %+v", got.Limits, tc.limits)
				}
			})
		})
	}
}

// A golden builder's box for the repository's setup script is started with
// the builder's limits and no services yet; the one after it, for the
// services' first start, as any builder's.
func TestSetupBoxGets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		worktree := filepath.Join(t.TempDir(), "worktrees", "sample", "one")
		for f, data := range map[string]string{
			"services.yaml": "services: {pg: {run: ./pg.sh}}\nlimits: {cpus: 3, memory: 4096, pids: 6000}\n",
			".fern/setup":   "#!/bin/sh\n",
		} {
			path := filepath.Join(worktree, f)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(data), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		st, drv := newFakeStore(), runsc.NewFake("box")
		builder := store.BuilderID("sample")
		var mu sync.Mutex
		var forSetup runsc.Box // the box the script ran in
		cfg := node(true)
		cfg.RunSetup = func(context.Context, string, string, []string, io.Writer) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			forSetup = drv.Woken(builder)
			return 0, nil
		}
		g := actor.New(st, drv, cfg, notrace, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { _ = g.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		if err := g.Create(t.Context(), "one", store.CreateOptions{Workspace: worktree}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()

		asked := runsc.Limits{CPUs: 3, Memory: 4 << 30, Pids: 6000}
		preview := "FERN_PREVIEW=http://{port}-" + builder + ".localhost:30780/"
		mu.Lock()
		defer mu.Unlock()
		for _, tc := range []struct {
			name string
			got  runsc.Box
			kind runsc.Kind
			want []string
		}{
			{"for the setup script", forSetup, runsc.Setup, []string{"FERN_WORKSPACE=" + worktree, "FERN_SERVICES=later", preview}},
			{"for the services' first start", drv.Woken(builder), runsc.Builder, []string{"FERN_WORKSPACE=" + worktree, "FERN_ON_DEMAND=off", preview}},
		} {
			if tc.got.Kind != tc.kind {
				t.Errorf("the box %s is of kind %d, want %d", tc.name, tc.got.Kind, tc.kind)
			}
			if diff := cmp.Diff(tc.want, tc.got.Vars); diff != "" {
				t.Errorf("the variables of the box %s (-want +got):\n%s", tc.name, diff)
			}
			if tc.got.Limits != asked {
				t.Errorf("the limits of the box %s = %+v, want %+v", tc.name, tc.got.Limits, asked)
			}
		}
	})
}

// A box that runs a repository's services gets the node's limits, or what
// the repository asks for up to the most it may, and so does a golden
// builder's; an agent box gets the defaults.
func TestBoxLimits(t *testing.T) {
	big := runsc.Limits{CPUs: 4, Memory: 8 << 30, Pids: 9000}
	const all = "limits: {cpus: 3, memory: 4096, pids: 6000}\n"
	for _, tc := range []struct {
		name    string
		cfg     actor.Config
		box     string
		yaml    string // the repository's services.yaml
		want    runsc.Limits
		wantErr bool
	}{
		{name: "unset", box: "demo", want: runsc.DefaultLimits()},
		{name: "set", cfg: actor.Config{Limits: big}, box: "demo", want: big},
		{name: "an agent box", cfg: actor.Config{Limits: big, MaxLimits: big}, box: "demo.agent", yaml: all, want: runsc.DefaultLimits()},
		{name: "a builder", cfg: actor.Config{MaxLimits: big}, box: store.BuilderID("r"), yaml: all, want: runsc.Limits{CPUs: 3, Memory: 4 << 30, Pids: 6000}},
		{name: "asked for less", box: "demo", yaml: "limits: {memory: 512}\n", want: runsc.Limits{CPUs: 2, Memory: 512 << 20, Pids: 4096}},
		{name: "asked for more than the node gives", box: "demo", yaml: all, want: runsc.DefaultLimits()},
		{name: "asked for more, under the most", cfg: actor.Config{MaxLimits: big}, box: "demo", yaml: all, want: runsc.Limits{CPUs: 3, Memory: 4 << 30, Pids: 6000}},
		{name: "asked for more than the most", cfg: actor.Config{MaxLimits: big}, box: "demo", yaml: "limits: {cpus: 64, memory: 1048576, pids: 1048576}\n", want: big},
		{name: "asked for too little", box: "demo", yaml: "limits: {memory: 1}\n", wantErr: true},
		{name: "asked for less than nothing", cfg: actor.Config{MaxLimits: big}, box: "demo", yaml: "limits: {cpus: -1}\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tc.cfg.Image, tc.cfg.Agents = "box", true
				got, err := woken(t, tc.cfg, newFakeStore(), tc.box, workspace(t, tc.yaml))
				if (err != nil) != tc.wantErr || got.Limits != tc.want {
					t.Errorf("woke %s with limits %+v, error %v; want %+v, error %t", tc.box, got.Limits, err, tc.want, tc.wantErr)
				}
				if tc.wantErr && !strings.Contains(err.Error(), "services.yaml: limits") {
					t.Errorf("Wake() error = %v, want it to name services.yaml's limits", err)
				}
			})
		})
	}
}
