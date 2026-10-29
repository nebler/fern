// Package runsc runs each environment's box as a gVisor sandbox that the
// daemon starts itself with runsc (docs/briefs/boxes-without-kubernetes.md).
//
// The daemon runs on the node, as a systemd service, and a box is the
// node's process: it outlives a daemon restart. While a box may run,
// <bundles>/<id> holds its bundle and <pool>/boxes/<id> its /tmp and log.
package runsc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sys/unix"

	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/image"
	"github.com/nebler/fern/internal/span"
	"github.com/nebler/fern/internal/store"
)

// State is whether a box is up.
type State string

// The states of a box. A box that runsc doesn't list is Asleep.
const (
	Asleep   State = "asleep"
	Waking   State = "waking"   // created, not started yet
	Running  State = "running"  // its fern-init runs
	Stopping State = "stopping" // stopped, not deleted yet
	Unknown  State = "unknown"  // runsc didn't say
)

// Kind is what a box runs.
type Kind int

// The kinds of box.
const (
	// Services runs the environment's services, each started by its first
	// connection.
	Services Kind = iota
	// Agent runs Claude for an environment whose services run in its own
	// box: it runs no services, and may reach the daemon's MCP port, whose
	// Bash runs commands in the services' box.
	Agent
	// Builder makes a golden snapshot: its services start with it, since
	// their first start is what it builds.
	Builder
)

// Box is a box to wake.
type Box struct {
	ID   string    // its name: its environment's ID, or the environment's agent box's
	Kind Kind      // what it runs
	Env  store.Env // its folders and token; with Env.Claude "", it gets no Claude folder or login
}

// Config is where a Driver keeps its boxes and what it gives them.
type Config struct {
	Pool    string   // the pool, /var/lib/fern
	Bundles string   // the boxes' bundles, on the node's tmpfs: /run/fern/boxes
	State   string   // runsc's state directory, on the node's tmpfs
	Images  string   // where images are unpacked (fern image load): <pool>/images
	Image   string   // the box image's tag, such as fern-box:dev
	DNS     []string // the resolvers boxes use
	// Login returns Claude's login, or "" if there is none yet.
	Login   func(ctx context.Context) (string, error)
	MCPPort int // the daemon's port agent boxes may reach
}

// The limits of a wake and a stop. fern-init needs at most 8 s for the
// services' stop commands, 3 s more to kill them and 1 s to finish.
const (
	wakeTimeout   = 2 * time.Minute
	stopGrace     = 15 * time.Second
	readyInterval = 50 * time.Millisecond
	readyWait     = 5 * time.Second
)

// Driver runs boxes with runsc on the node.
type Driver struct {
	cfg    Config
	tracer trace.Tracer
	ready  *http.Client // asks waking boxes' fern-init whether they are ready
	steps  *prometheus.HistogramVec

	rootfs   string   // the image's root, which boxes' overlays share; set by Prepare
	imageEnv []string // the image's environment

	mu sync.Mutex // allocations of box addresses
}

// New returns a Driver that runs boxes as cfg says, records its steps with
// tr and its metrics in reg. Call Prepare before anything else.
func New(cfg Config, tr trace.Tracer, reg prometheus.Registerer) *Driver {
	return &Driver{
		cfg: cfg, tracer: tr,
		ready: &http.Client{
			Timeout:   readyWait + 3*time.Second,
			Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 250 * time.Millisecond}).DialContext},
		},
		steps: promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
			Name:    "fern_wake_step_seconds",
			Help:    "Time a wake spent in each step: net (the box's network), start (runsc create and start) and ready (until its services are).",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 15), // 5ms to 82s
		}, []string{"step"}),
	}
}

// Prepare gets the node ready for boxes: the box image's root, the
// firewall that keeps boxes to the internet, the cgroup that holds them,
// and their resolv.conf. It returns the image's ID, which golden keys
// include.
func (d *Driver) Prepare(ctx context.Context) (string, error) {
	img, err := image.Open(d.cfg.Images, d.cfg.Image)
	if err != nil {
		return "", fmt.Errorf("box image (fern image load): %w", err)
	}
	d.rootfs, d.imageEnv = img.Root, img.Env
	nft := exec.CommandContext(ctx, "nft", "-f", "-")
	nft.Stdin = strings.NewReader(firewall(d.cfg.MCPPort))
	if _, err := run(nft); err != nil {
		return "", fmt.Errorf("box firewall: %w", err)
	}
	// The controllers boxes' limits need, from the root down: systemd
	// doesn't enable cpu at the root unless a unit asks for it.
	cg := "/sys/fs/cgroup/" + cgroupParent
	if err := os.MkdirAll(cg, 0o755); err != nil {
		return "", err
	}
	for _, dir := range []string{"/sys/fs/cgroup", cg} {
		if err := os.WriteFile(dir+"/cgroup.subtree_control", []byte("+cpu +memory +pids"), 0o644); err != nil {
			return "", fmt.Errorf("box cgroup: %w", err)
		}
	}
	for _, dir := range []string{d.cfg.Bundles, filepath.Join(d.cfg.Pool, "boxes")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
	}
	var resolv strings.Builder
	for _, ns := range d.cfg.DNS {
		fmt.Fprintf(&resolv, "nameserver %s\n", ns)
	}
	if err := os.WriteFile(d.layout("").resolv, []byte(resolv.String()), 0o644); err != nil {
		return "", err
	}
	if err := d.reconcile(ctx); err != nil {
		return "", err
	}
	return img.ID, nil
}

// reconcile finishes what a crash cut short: a box directory with no box
// running is a wake or a stop that didn't end (or a node that restarted),
// and is stopped, which frees its network, overlay, address and /tmp.
// Running boxes are left alone; the actors adopt them.
func (d *Driver) reconcile(ctx context.Context) error {
	states, err := d.States(ctx)
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	for _, dir := range []string{d.cfg.Bundles, filepath.Join(d.cfg.Pool, "boxes")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() {
				ids[e.Name()] = true
			}
		}
	}
	for id := range ids {
		if states[id] == Running {
			continue
		}
		if err := d.stop(ctx, id); err != nil {
			return fmt.Errorf("clear box %s: %w", id, err)
		}
	}
	return nil
}

// layout names box id's resources on the node.
func (d *Driver) layout(id string) layout { return newLayout(d.cfg.Bundles, d.cfg.Pool, id) }

// container is a box as runsc lists it.
type container struct {
	ID     string `json:"id"`
	Status string `json:"status"` // created, running or stopped
	Bundle string `json:"bundle"` // <bundles>/<box>
}

func (d *Driver) runsc(ctx context.Context, args ...string) ([]byte, error) {
	return run(exec.CommandContext(ctx, "runsc", append([]string{"--root", d.cfg.State}, args...)...))
}

func (d *Driver) list(ctx context.Context) ([]container, error) {
	out, err := d.runsc(ctx, "list", "--format=json")
	if err != nil {
		return nil, err
	}
	var cs []container
	if out = bytes.TrimSpace(out); len(out) > 0 && !bytes.Equal(out, []byte("null")) {
		if err := json.Unmarshal(out, &cs); err != nil {
			return nil, fmt.Errorf("runsc list: %w", err)
		}
	}
	return cs, nil
}

// States reports the state of every box runsc runs, by box ID. A box that
// isn't there is asleep.
func (d *Driver) States(ctx context.Context) (map[string]State, error) {
	cs, err := d.list(ctx)
	if err != nil {
		return nil, err
	}
	states := make(map[string]State, len(cs))
	for _, c := range cs {
		if filepath.Dir(c.Bundle) != d.cfg.Bundles {
			continue // not a box of this pool's
		}
		id := filepath.Base(c.Bundle)
		switch c.Status {
		case "running":
			states[id] = Running
		case "created":
			states[id] = Waking
		default:
			states[id] = Stopping
		}
	}
	return states, nil
}

// Wake starts box, waits until its services are ready and returns the
// address of its exec server. A box that runs and is ready, such as one
// that outlived a daemon restart, is kept; any other left from before (a
// crash, a stop that didn't finish) is stopped first, so a wake starts clean.
func (d *Driver) Wake(ctx context.Context, box Box) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, wakeTimeout)
	defer cancel()
	b := d.layout(box.ID)
	if addr, ok := d.running(ctx, b); ok {
		return addr, nil
	}
	if err := d.stop(ctx, box.ID); err != nil {
		return "", fmt.Errorf("wake %s: %w", box.ID, err)
	}
	start := time.Now()
	addr, err := d.netUp(ctx, b, box.Kind)
	if err != nil {
		return "", fmt.Errorf("wake %s: %w", box.ID, err)
	}
	netDone := time.Now()
	if err := d.start(ctx, box, b); err != nil {
		return "", fmt.Errorf("wake %s: %w", box.ID, err)
	}
	started := time.Now()
	_, sp := d.tracer.Start(ctx, "ready")
	err = d.waitReady(ctx, addr)
	span.End(sp, err)
	if err != nil {
		return "", fmt.Errorf("wake %s: %w", box.ID, err)
	}
	d.steps.WithLabelValues("net").Observe(netDone.Sub(start).Seconds())
	d.steps.WithLabelValues("start").Observe(started.Sub(netDone).Seconds())
	d.steps.WithLabelValues("ready").Observe(time.Since(started).Seconds())
	return addr, nil
}

// running returns the exec address of b's box if it runs and fern-init says
// it is ready.
func (d *Driver) running(ctx context.Context, b layout) (string, bool) {
	cs, err := d.list(ctx)
	if err != nil || !slices.ContainsFunc(cs, func(c container) bool { return c.ID == b.cid && c.Status == "running" }) {
		return "", false
	}
	_, guest, err := linkAddrs(b)
	if err != nil {
		return "", false
	}
	addr := execAddr(guest)
	ctx, cancel := context.WithTimeout(ctx, readyWait)
	defer cancel()
	return addr, d.isReady(ctx, addr)
}

// execAddr is the address of fern-init's exec server in the box at guest.
func execAddr(guest netip.Addr) string {
	return net.JoinHostPort(guest.String(), strconv.Itoa(boxexec.Port))
}

// linkAddrs returns the node's and the box's address on b's link, from the
// address number in its directory.
func linkAddrs(b layout) (host, guest netip.Addr, err error) {
	data, err := os.ReadFile(filepath.Join(b.dir, "net"))
	if err != nil {
		return host, guest, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return host, guest, err
	}
	return addrs(n)
}

// netUp makes b's network with a free address and returns the box's exec
// address. The address's number is in b's directory, which shows it taken.
func (d *Driver) netUp(ctx context.Context, b layout, kind Kind) (_ string, err error) {
	_, sp := d.tracer.Start(ctx, "net")
	defer func() { span.End(sp, err) }()
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		return "", err
	}
	n, err := d.allocate(b)
	if err != nil {
		return "", err
	}
	cmds, err := netUp(b, kind, n)
	if err != nil {
		return "", err
	}
	for _, c := range cmds {
		if _, err := run(exec.CommandContext(ctx, c[0], c[1:]...)); err != nil {
			return "", err
		}
	}
	_, guest, _ := addrs(n)
	return execAddr(guest), nil
}

// allocate gives b the lowest address number no other box holds.
func (d *Driver) allocate(b layout) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	taken := make(map[int]bool)
	files, err := filepath.Glob(filepath.Join(d.cfg.Bundles, "*", "net"))
	if err != nil {
		return 0, err
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue // a box stopping meanwhile
		}
		if n, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			taken[n] = true
		}
	}
	for n := range maxBoxes {
		if !taken[n] {
			return n, os.WriteFile(filepath.Join(b.dir, "net"), []byte(strconv.Itoa(n)), 0o600)
		}
	}
	return 0, fmt.Errorf("no address left for box %s", b.id)
}

// start writes box's bundle to b and starts it.
func (d *Driver) start(ctx context.Context, box Box, b layout) (err error) {
	_, sp := d.tracer.Start(ctx, "start")
	defer func() { span.End(sp, err) }()
	login := ""
	if box.Env.Claude != "" && d.cfg.Login != nil {
		if login, err = d.cfg.Login(ctx); err != nil {
			return fmt.Errorf("claude login: %w", err)
		}
	}
	// A fresh /tmp at every wake.
	if err := os.RemoveAll(b.tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(b.data, 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(b.tmp, 0o700); err != nil {
		return err
	}
	if err := os.Chown(b.tmp, store.BoxUID, store.BoxGID); err != nil {
		return err
	}
	// runsc's gofer makes the mount points in the root, so each box has its
	// own: an overlay on the image, whose upper layer holds only them. The
	// box sees its root read-only. The overlay's root takes the upper
	// layer's mode: the box's user must be able to walk it.
	upper, work := filepath.Join(b.dir, "upper"), filepath.Join(b.dir, "work")
	for _, dir := range []string{b.rootfs, upper, work} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	opts := "lowerdir=" + d.rootfs + ",upperdir=" + upper + ",workdir=" + work
	if err := unix.Mount("overlay", b.rootfs, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount %s: %w", b.rootfs, err)
	}
	wakeID := make([]byte, 8)
	_, _ = rand.Read(wakeID)
	s := boxSpec(box, b, d.imageEnv, boxVars(box, hex.EncodeToString(wakeID), login))
	data, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(b.dir, "config.json"), data, 0o600); err != nil {
		return err
	}
	host, _, err := linkAddrs(b)
	if err != nil {
		return err
	}
	if err := os.WriteFile(b.hosts(), []byte(hostsFile(box, host)), 0o644); err != nil {
		return err
	}
	// The box's stdio is create's: a file, as a pipe would stay open as
	// long as the box runs.
	log, err := os.OpenFile(filepath.Join(b.data, "log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	create := exec.CommandContext(ctx, "runsc", "--root", d.cfg.State, "create", "--bundle", b.dir, b.cid)
	create.Stdout, create.Stderr = log, log
	if err := create.Run(); err != nil {
		return fmt.Errorf("runsc create: %w (see %s)", err, log.Name())
	}
	_, err = d.runsc(ctx, "start", b.cid)
	return err
}

// waitReady asks fern-init at addr until it says its services are ready.
func (d *Driver) waitReady(ctx context.Context, addr string) error {
	for {
		if d.isReady(ctx, addr) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not ready: %w", ctx.Err())
		case <-time.After(readyInterval):
		}
	}
}

func (d *Driver) isReady(ctx context.Context, addr string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+boxexec.ReadyPath+"?wait="+readyWait.String(), nil)
	if err != nil {
		return false
	}
	resp, err := d.ready.Do(req)
	if err != nil {
		return false // not listening yet
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Sleep stops box id and waits until it is gone, with its network and
// cgroup. A box that isn't there is asleep already. A box has nothing that
// outlives it, so this is also how one is deleted.
func (d *Driver) Sleep(ctx context.Context, id string) error {
	ctx, sp := d.tracer.Start(ctx, "stop")
	err := d.stop(ctx, id)
	span.End(sp, err)
	if err != nil {
		return fmt.Errorf("sleep %s: %w", id, err)
	}
	return nil
}

// stop asks fern-init to shut down, waits for the box to exit (killing it
// after stopGrace), deletes it, and removes its network, cgroup and
// directory. Every step is safe to repeat, so a stop that a crash cut short
// is finished by the next.
func (d *Driver) stop(ctx context.Context, id string) error {
	b := d.layout(id)
	cs, err := d.list(ctx)
	if err != nil {
		return err
	}
	// By its bundle, not its ID: a box left by any earlier daemon is found.
	for _, c := range cs {
		if c.Bundle != b.dir {
			continue
		}
		if c.ID == b.cid {
			d.askStop(ctx, b)
		}
		waitCtx, cancel := context.WithTimeout(ctx, stopGrace)
		_, err := d.runsc(waitCtx, "wait", c.ID)
		cancel()
		if err != nil {
			_, _ = d.runsc(ctx, "kill", "--all", c.ID, "KILL")
		}
		if _, err := d.runsc(ctx, "delete", "--force", c.ID); err != nil {
			return err
		}
	}
	// One process for the three commands: a stop is on every sleep's path.
	ip := exec.CommandContext(ctx, "ip", "-force", "-batch", "-")
	ip.Stdin = strings.NewReader(netDown(b))
	_, _ = run(ip) // fails for what is gone already
	// runsc removes the box's cgroup only on a clean exit.
	_ = os.Remove("/sys/fs/cgroup/" + cgroupParent + "/" + id)
	if err := unix.Unmount(b.rootfs, 0); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("unmount %s: %w", b.rootfs, err) // EINVAL: not mounted
	}
	for _, dir := range []string{b.dir, b.data} {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return nil
}

// askStop starts fern-init's graceful shutdown with its authenticated
// POST /stop, naming the wake it stops. A box that doesn't answer, or
// refuses (an older fern-init), gets runsc's SIGTERM instead.
func (d *Driver) askStop(ctx context.Context, b layout) {
	data, err := os.ReadFile(filepath.Join(b.dir, "config.json"))
	if err != nil {
		return
	}
	var s spec
	if json.Unmarshal(data, &s) != nil {
		return
	}
	var token, wakeID string
	for _, v := range s.Process.Env {
		if t, ok := strings.CutPrefix(v, "FERN_BOX_TOKEN="); ok {
			token = t
		}
		if w, ok := strings.CutPrefix(v, "FERN_WAKE_ID="); ok {
			wakeID = w
		}
	}
	_, guest, err := linkAddrs(b)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+execAddr(guest)+boxexec.StopPath, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("If-Match", strconv.Quote(wakeID))
	resp, err := d.ready.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	if err != nil || resp.StatusCode != http.StatusNoContent {
		_, _ = d.runsc(ctx, "kill", b.cid, "TERM")
	}
}

// run runs cmd and returns its stdout, or an error with its stderr.
func run(cmd *exec.Cmd) ([]byte, error) {
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s: %w: %s", strings.Join(cmd.Args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}
