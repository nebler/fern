package runsc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace/noop"
)

// fakeNode puts fake runsc and ip programs first on PATH, which record every
// call in log. The fake runsc lists the containers in list (JSON) and exits
// from wait with waitExit.
type fakeNode struct {
	dir, log, list string
}

func newFakeNode(t *testing.T, waitExit int) *fakeNode {
	t.Helper()
	dir := t.TempDir()
	n := &fakeNode{dir: dir, log: filepath.Join(dir, "log"), list: filepath.Join(dir, "list.json")}
	runsc := fmt.Sprintf(`#!/bin/sh
echo "runsc $*" >>%q
shift 2 # --root DIR
case "$1" in
list) cat %q ;;
wait) exit %d ;;
esac
`, n.log, n.list, waitExit)
	ip := fmt.Sprintf(`#!/bin/sh
echo "ip $*" >>%q
cat >>%q
`, n.log, n.log)
	for name, script := range map[string]string{"runsc": runsc, "ip": ip} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	n.setList(t, nil)
	return n
}

func (n *fakeNode) setList(t *testing.T, cs []container) {
	t.Helper()
	data, err := json.Marshal(cs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.list, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// calls returns the lines the fake programs logged.
func (n *fakeNode) calls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(n.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func newTestDriver(t *testing.T) *Driver {
	t.Helper()
	root := t.TempDir()
	cfg := Config{Pool: filepath.Join(root, "pool"), Bundles: filepath.Join(root, "bundles"), State: filepath.Join(root, "state")}
	for _, dir := range []string{cfg.Bundles, filepath.Join(cfg.Pool, "boxes")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return New(cfg, noop.NewTracerProvider().Tracer(""), prometheus.NewRegistry())
}

// The states runsc lists become box states; boxes of another pool's bundles
// aren't this driver's, and an empty list (runsc prints null) is no boxes.
func TestStates(t *testing.T) {
	node := newFakeNode(t, 0)
	d := newTestDriver(t)
	if got, err := d.States(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("States() with none = %v, %v; want none", got, err)
	}
	node.setList(t, []container{
		{ID: "fern-a", Status: "running", Bundle: filepath.Join(d.cfg.Bundles, "up")},
		{ID: "fern-b", Status: "created", Bundle: filepath.Join(d.cfg.Bundles, "waking")},
		{ID: "fern-c", Status: "stopped", Bundle: filepath.Join(d.cfg.Bundles, "stopping")},
		{ID: "fern-d", Status: "running", Bundle: "/elsewhere/bundles/other"},
	})
	got, err := d.States(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]State{"up": Running, "waking": Waking, "stopping": Stopping}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("States() mismatch (-want +got):\n%s", diff)
	}
}

// Sleep stops a box in order: it asks fern-init to stop (here no one
// answers, so runsc sends SIGTERM), waits for the box, kills it if it
// doesn't exit, deletes it, then removes both possible veths and the
// network namespace in one ip batch, and the box's directories.
func TestSleep(t *testing.T) {
	for _, tc := range []struct {
		name     string
		waitExit int
		killed   bool
	}{
		{"exits", 0, false},
		{"killed after the grace", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := newFakeNode(t, tc.waitExit)
			d := newTestDriver(t)
			b := d.layout("demo")
			for _, dir := range []string{b.dir, b.tmp} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			box := Box{ID: "demo", Env: env}
			config, err := json.Marshal(boxSpec(box, b, nil, boxVars(box, "w1", "", 7071), DefaultLimits))
			if err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string][]byte{"config.json": config, "net": []byte("0")} {
				if err := os.WriteFile(filepath.Join(b.dir, name), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			node.setList(t, []container{{ID: b.cid, Status: "running", Bundle: b.dir}})

			if err := d.Sleep(t.Context(), "demo"); err != nil {
				t.Fatalf("Sleep(demo) error: %v", err)
			}
			want := []string{
				"runsc --root " + d.cfg.State + " list --format=json",
				"runsc --root " + d.cfg.State + " kill " + b.cid + " TERM",
				"runsc --root " + d.cfg.State + " wait " + b.cid,
			}
			if tc.killed {
				want = append(want, "runsc --root "+d.cfg.State+" kill --all "+b.cid+" KILL")
			}
			want = append(want,
				"runsc --root "+d.cfg.State+" delete --force "+b.cid,
				"ip -force -batch -",
				"link del "+b.veth(Services),
				"link del "+b.veth(Agent),
				"netns del "+b.nsName(),
			)
			if diff := cmp.Diff(want, node.calls(t)); diff != "" {
				t.Errorf("calls mismatch (-want +got):\n%s", diff)
			}
			for _, dir := range []string{b.dir, b.data} {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Errorf("%s is still there after the stop", dir)
				}
			}
		})
	}
}

// A box that isn't running is asleep already: Sleep only clears what a stop
// a crash cut short may have left of its network.
func TestSleepAsleep(t *testing.T) {
	node := newFakeNode(t, 0)
	d := newTestDriver(t)
	if err := d.Sleep(t.Context(), "demo"); err != nil {
		t.Fatalf("Sleep(demo) error: %v", err)
	}
	for _, call := range node.calls(t) {
		if strings.Contains(call, " kill ") || strings.Contains(call, " delete ") {
			t.Errorf("Sleep of an asleep box ran %q", call)
		}
	}
}

// A box whose fern-init exited as it started is never ready: its wake fails
// with the end of its log, long before the wake's time is up.
func TestWaitReadyExited(t *testing.T) {
	node := newFakeNode(t, 0)
	d := newTestDriver(t)
	b := d.layout("demo")
	if err := os.MkdirAll(b.data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.data, "log"), []byte("fern-init: services.yaml: unknown field \"env\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	node.setList(t, []container{{ID: b.cid, Status: "stopped", Bundle: b.dir}})
	// Nothing listens there.
	err := d.waitReady(t.Context(), b, "127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), `unknown field "env"`) {
		t.Errorf("waitReady() of a box that exited: %v, want its log's end", err)
	}
}

// A wake that runs out of time says what fern-init last said of its
// services, which names the one that isn't ready and what it is doing.
func TestWaitReadySaysWhy(t *testing.T) {
	newFakeNode(t, 0)
	d := newTestDriver(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "db not ready (it has exited 3 times in a row, last with exit status 1, and starts again in 4s): exit status 2", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err := d.waitReady(ctx, d.layout("demo"), strings.TrimPrefix(srv.URL, "http://"))
	if err == nil || !strings.Contains(err.Error(), "db not ready (it has exited 3 times") {
		t.Errorf("waitReady() of a box whose service keeps exiting: %v, want fern-init's words", err)
	}
}

// A box's network is made by two ip batches: one on the node, one in the
// box's namespace.
func TestNetUp(t *testing.T) {
	node := newFakeNode(t, 0)
	d := newTestDriver(t)
	b := d.layout("demo")
	addr, err := d.netUp(t.Context(), b, Agent)
	if err != nil {
		t.Fatalf("netUp() error: %v", err)
	}
	host, guest, err := addrs(0)
	if err != nil {
		t.Fatal(err)
	}
	if want := execAddr(guest); addr != want {
		t.Errorf("netUp() = %s, want the first free address, %s", addr, want)
	}
	ns, veth := b.nsName(), b.veth(Agent)
	want := []string{
		"ip -batch -",
		"netns add " + ns,
		"link add " + veth + " type veth peer name eth0 netns " + ns,
		"addr add " + host.String() + "/30 dev " + veth,
		"link set " + veth + " up",
		"ip -n " + ns + " -batch -",
		"addr add " + guest.String() + "/30 dev eth0",
		"link set eth0 up",
		"link set lo up",
		"route add default via " + host.String(),
	}
	if diff := cmp.Diff(want, node.calls(t)); diff != "" {
		t.Errorf("calls mismatch (-want +got):\n%s", diff)
	}
}

// Boxes waking at once get different addresses, the lowest free ones; an
// address is free again once its box's directory is gone.
func TestAllocate(t *testing.T) {
	d := newTestDriver(t)
	const n = 20
	got := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			b := d.layout("box" + strconv.Itoa(i))
			if err := os.MkdirAll(b.dir, 0o700); err != nil {
				t.Error(err)
				return
			}
			a, err := d.allocate(b)
			if err != nil {
				t.Error(err)
			}
			got[i] = a
		})
	}
	wg.Wait()
	seen := make(map[int]bool)
	for _, a := range got {
		if seen[a] || a < 0 || a >= n {
			t.Fatalf("allocate gave %v; want each of 0..%d once", got, n-1)
		}
		seen[a] = true
	}
	if err := os.RemoveAll(d.layout("box7").dir); err != nil {
		t.Fatal(err)
	}
	b := d.layout("new")
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if a, err := d.allocate(b); err != nil || a != got[7] {
		t.Errorf("allocate after box7 went = %d, %v; want its address %d", a, err, got[7])
	}
}

// What a wake does with a box depends on what it finds of it.
func TestFind(t *testing.T) {
	d := newTestDriver(t)
	b := d.layout("demo")
	tests := []struct {
		name string
		dir  bool
		cs   []container
		want found
	}{
		{name: "no directory", want: foundNothing},
		{name: "no directory, whatever runsc lists", cs: []container{{ID: b.cid, Status: "running", Bundle: b.dir}}, want: foundNothing},
		{name: "a directory only", dir: true, want: foundLeftover},
		{name: "created, not started", dir: true, cs: []container{{ID: b.cid, Status: "created", Bundle: b.dir}}, want: foundLeftover},
		{name: "stopped, not deleted", dir: true, cs: []container{{ID: b.cid, Status: "stopped", Bundle: b.dir}}, want: foundLeftover},
		{name: "another box runs", dir: true, cs: []container{{ID: "other", Status: "running", Bundle: "/elsewhere"}}, want: foundLeftover},
		{name: "running", dir: true, cs: []container{{ID: b.cid, Status: "running", Bundle: b.dir}}, want: foundRunning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := find(tt.dir, tt.cs, b); got != tt.want {
				t.Errorf("find() = %d, want %d", got, tt.want)
			}
		})
	}
}

// A box found running is kept once its services are ready, however often
// its fern-init says they aren't yet; one whose fern-init is silent is not.
func TestAdopt(t *testing.T) {
	d := newTestDriver(t)
	t.Run("not ready, then ready", func(t *testing.T) {
		var asked atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if asked.Add(1) <= 3 {
				http.Error(w, "db not ready", http.StatusServiceUnavailable)
			}
		}))
		defer srv.Close()
		if !d.adopt(t.Context(), strings.TrimPrefix(srv.URL, "http://")) {
			t.Error("adopt() gave up on a box that got ready")
		}
		if got := asked.Load(); got != 4 {
			t.Errorf("asked %d times, want 4", got)
		}
	})
	t.Run("silent", func(t *testing.T) {
		start := time.Now()
		// Nothing listens there.
		if d.adopt(t.Context(), "127.0.0.1:1") {
			t.Error("adopt() kept a box that doesn't answer")
		}
		if took := time.Since(start); took < adoptSilence || took > adoptSilence+5*time.Second {
			t.Errorf("adopt() gave up after %s, want about %s", took, adoptSilence)
		}
	})
}
