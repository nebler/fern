package runsc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

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
			config, err := json.Marshal(boxSpec(box, b, nil, boxVars(box, "w1", "")))
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
