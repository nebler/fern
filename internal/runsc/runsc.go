// Package runsc runs each environment's box as a gVisor sandbox that the
// daemon starts itself with runsc, instead of as a Kubernetes pod
// (docs/briefs/boxes-without-kubernetes.md). It skips the API server, the
// controller, kubelet, the CSI calls and the CNI, which cost a pod's wake
// about half its time and more than half its CPU.
//
// Everything runs in the node's mount and cgroup namespaces (Host): a box is
// the node's process, outside the daemon's container, so it outlives a
// daemon restart. While a box may run, <pool>/boxes/<id> holds its bundle.
package runsc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/driver"
	"github.com/nebler/fern/internal/setup"
	"github.com/nebler/fern/internal/store"
)

// Command is a program to run on the node.
type Command struct {
	Name  string
	Args  []string
	Stdin []byte // nil: none
	// Out, if set, gets the program's stdout and stderr, and nothing is
	// returned: what it leaves running (a box) may hold them open.
	Out *os.File
}

// Host runs a program in the node's namespaces and returns what it printed.
type Host func(ctx context.Context, c Command) ([]byte, error)

// NodeHost runs programs in the mount and cgroup namespaces of the node's
// PID 1: the daemon shares the node's PID namespace, so that is the node's
// init.
func NodeHost(ctx context.Context, c Command) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "nsenter", append([]string{"--target", "1", "--mount", "--cgroup", "--", c.Name}, c.Args...)...)
	// The node's programs, wherever it keeps them: k3s's image has its
	// iptables in /bin/aux.
	cmd.Env = []string{"PATH=" + nodePath}
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if c.Out != nil {
		cmd.Stdout, cmd.Stderr = c.Out, c.Out
	}
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s", c.Name, strings.Join(c.Args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

// nodePath is where NodeHost looks for programs on the node.
const nodePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/bin/aux"

// run runs name with args on the node.
func (d *Driver) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return d.host(ctx, Command{Name: name, Args: args})
}

// Config is where a Driver keeps its boxes and what it gives them.
type Config struct {
	Pool  string   // the pool, /var/lib/fern
	State string   // runsc's state directory, on the node
	Image string   // the box image, as the node's containerd names it
	DNS   []string // the resolvers boxes use: the node's own is the node
	// Login returns Claude's login, or "" if there is none yet.
	Login func(ctx context.Context) (string, error)
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
	host   Host
	cfg    Config
	tracer trace.Tracer
	ready  *http.Client // asks waking boxes' fern-init whether they are ready
	steps  *prometheus.HistogramVec

	rootfs   string   // the image's root, mounted read-only; set by Prepare
	imageEnv []string // the image's environment

	mu sync.Mutex // allocations of box addresses
}

// New returns a Driver that runs boxes as cfg says, through host, and
// records its steps with tr. Call Prepare before anything else.
func New(host Host, cfg Config, tr trace.Tracer) *Driver {
	return &Driver{
		host: host, cfg: cfg, tracer: tr,
		ready: &http.Client{
			Timeout:   readyWait + 3*time.Second,
			Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 250 * time.Millisecond}).DialContext},
		},
		steps: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "fern_wake_step_seconds",
			Help:    "Time a wake spent in each step: net (the box's network), start (runsc create and start) and ready (until its services are).",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 15), // 5ms to 82s
		}, []string{"step"}),
	}
}

// Describe sends the descriptions of the Driver's metrics.
func (d *Driver) Describe(ch chan<- *prometheus.Desc) { d.steps.Describe(ch) }

// Collect sends the Driver's metrics.
func (d *Driver) Collect(ch chan<- prometheus.Metric) { d.steps.Collect(ch) }

// imageInfo is what Prepare reads from crictl inspecti.
type imageInfo struct {
	Status struct {
		ID string `json:"id"`
	} `json:"status"`
	Info struct {
		ImageSpec struct {
			Config struct {
				Env []string `json:"Env"`
			} `json:"config"`
		} `json:"imageSpec"`
	} `json:"info"`
}

// Prepare gets the node ready for boxes: the box image's root, mounted
// read-only from containerd's store under <pool>/images/<id>, the firewall
// that keeps boxes to the internet, the cgroup that holds them, and their
// resolv.conf. It returns the image's ID, which
// golden keys include.
func (d *Driver) Prepare(ctx context.Context) (string, error) {
	out, err := d.run(ctx, "crictl", "inspecti", "-o", "json", d.cfg.Image)
	if err != nil {
		return "", fmt.Errorf("box image %s: %w", d.cfg.Image, err)
	}
	var info imageInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("box image %s: %w", d.cfg.Image, err)
	}
	id := strings.TrimPrefix(info.Status.ID, "sha256:")
	if id == "" {
		return "", fmt.Errorf("box image %s has no ID", d.cfg.Image)
	}
	d.imageEnv = info.Info.ImageSpec.Config.Env
	d.rootfs = filepath.Join(d.cfg.Pool, "images", id)
	if _, err := d.run(ctx, "mountpoint", "-q", d.rootfs); err != nil {
		if err := os.MkdirAll(d.rootfs, 0o755); err != nil {
			return "", err
		}
		// A lease named after the target keeps containerd from collecting
		// the image's layers while it is mounted.
		if _, err := d.run(ctx, "ctr", "-n", "k8s.io", "images", "mount", d.cfg.Image, d.rootfs); err != nil {
			return "", fmt.Errorf("mount box image: %w", err)
		}
	}
	for _, r := range firewall() {
		if r.unless != nil {
			if _, err := d.run(ctx, "iptables", r.unless...); err == nil {
				continue
			}
		}
		if _, err := d.run(ctx, "iptables", r.args...); err != nil && !r.mayFail {
			return "", fmt.Errorf("box firewall: %w", err)
		}
	}
	cg := "/sys/fs/cgroup/" + cgroupParent
	if _, err := d.run(ctx, "mkdir", "-p", cg); err != nil {
		return "", err
	}
	if _, err := d.host(ctx, Command{Name: "tee", Args: []string{cg + "/cgroup.subtree_control"}, Stdin: []byte("+cpu +memory +pids")}); err != nil {
		return "", fmt.Errorf("box cgroup: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(d.cfg.Pool, "boxes"), 0o700); err != nil {
		return "", err
	}
	var resolv strings.Builder
	for _, ns := range d.cfg.DNS {
		fmt.Fprintf(&resolv, "nameserver %s\n", ns)
	}
	if err := os.WriteFile(newBox(d.cfg.Pool, "").resolv, []byte(resolv.String()), 0o644); err != nil {
		return "", err
	}
	return id, nil
}

// container is a box as runsc lists it.
type container struct {
	ID     string `json:"id"`
	Status string `json:"status"` // created, running or stopped
	Bundle string `json:"bundle"` // <pool>/boxes/<environment>
}

func (d *Driver) runsc(ctx context.Context, args ...string) ([]byte, error) {
	return d.run(ctx, "runsc", append([]string{"--root", d.cfg.State}, args...)...)
}

func (d *Driver) list(ctx context.Context) ([]container, error) {
	out, err := d.runsc(ctx, "list", "--format=json")
	if err != nil {
		return nil, err
	}
	var cs []container
	if len(bytes.TrimSpace(out)) > 0 && !bytes.Equal(bytes.TrimSpace(out), []byte("null")) {
		if err := json.Unmarshal(out, &cs); err != nil {
			return nil, fmt.Errorf("runsc list: %w", err)
		}
	}
	return cs, nil
}

// States reports the state of every box runsc runs, by environment ID. An
// environment that has none is asleep.
func (d *Driver) States(ctx context.Context) (map[string]driver.State, error) {
	cs, err := d.list(ctx)
	if err != nil {
		return nil, err
	}
	states := make(map[string]driver.State, len(cs))
	boxes := filepath.Join(d.cfg.Pool, "boxes")
	for _, c := range cs {
		if filepath.Dir(c.Bundle) != boxes {
			continue // not a box of this pool's
		}
		id := filepath.Base(c.Bundle)
		switch c.Status {
		case "running":
			states[id] = driver.Running
		case "created":
			states[id] = driver.Waking
		default:
			states[id] = driver.Stopping
		}
	}
	return states, nil
}

// Wake starts env's box, waits until its services are ready and returns
// the address of its exec server. A box left from before (a crash, a stop
// that didn't finish) is stopped first, so every wake starts clean.
func (d *Driver) Wake(ctx context.Context, env store.Env) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, wakeTimeout)
	defer cancel()
	if err := d.stop(ctx, env.ID); err != nil {
		return "", fmt.Errorf("wake %s: %w", env.ID, err)
	}
	b := newBox(d.cfg.Pool, env.ID)
	start := time.Now()
	addr, err := d.netUp(ctx, b)
	if err != nil {
		return "", fmt.Errorf("wake %s: %w", env.ID, err)
	}
	netDone := time.Now()
	if err := d.start(ctx, env, b); err != nil {
		return "", fmt.Errorf("wake %s: %w", env.ID, err)
	}
	started := time.Now()
	_, span := d.tracer.Start(ctx, "ready")
	err = d.waitReady(ctx, addr)
	end(span, err)
	if err != nil {
		return "", fmt.Errorf("wake %s: %w", env.ID, err)
	}
	d.steps.WithLabelValues("net").Observe(netDone.Sub(start).Seconds())
	d.steps.WithLabelValues("start").Observe(started.Sub(netDone).Seconds())
	d.steps.WithLabelValues("ready").Observe(time.Since(started).Seconds())
	return addr, nil
}

// netUp makes b's network with a free address and returns the box's exec
// address. The address's number is in b's directory, which shows it taken.
func (d *Driver) netUp(ctx context.Context, b box) (string, error) {
	_, span := d.tracer.Start(ctx, "net")
	var err error
	defer func() { end(span, err) }()
	if err = os.MkdirAll(b.dir, 0o700); err != nil {
		return "", err
	}
	n, err := d.allocate(b)
	if err != nil {
		return "", err
	}
	cmds, err := netUp(b, n)
	if err != nil {
		return "", err
	}
	for _, c := range cmds {
		if _, err = d.run(ctx, c[0], c[1:]...); err != nil {
			return "", err
		}
	}
	_, guest, _ := addrs(n)
	return net.JoinHostPort(guest.String(), strconv.Itoa(boxexec.Port)), nil
}

// allocate gives b the lowest address number no other box holds.
func (d *Driver) allocate(b box) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	taken := make(map[int]bool)
	files, err := filepath.Glob(filepath.Join(d.cfg.Pool, "boxes", "*", "net"))
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

// start writes b's bundle and starts it.
func (d *Driver) start(ctx context.Context, env store.Env, b box) error {
	_, span := d.tracer.Start(ctx, "start")
	var err error
	defer func() { end(span, err) }()
	builder := setup.IsBuilder(env.ID)
	login := ""
	if builder {
		// A golden builder's box runs no agent and gets no login: what its
		// services write goes into every thread of the repository.
		env.Claude = ""
	} else if d.cfg.Login != nil {
		if login, err = d.cfg.Login(ctx); err != nil {
			return fmt.Errorf("claude login: %w", err)
		}
	}
	// A fresh /tmp, as a pod gets a new emptyDir.
	if err = os.RemoveAll(b.tmp); err != nil {
		return err
	}
	if err = os.Mkdir(b.tmp, 0o700); err != nil {
		return err
	}
	if err = os.Chown(b.tmp, store.BoxUID, store.BoxGID); err != nil {
		return err
	}
	// runsc's gofer makes the mount points in the root, so each box has its
	// own: an overlay on the image, whose upper layer holds only them. The
	// box sees its root read-only.
	// The overlay's root takes the upper layer's mode: the box's user must
	// be able to walk it.
	for _, dir := range []string{b.rootfs, filepath.Join(b.dir, "upper"), filepath.Join(b.dir, "work")} {
		if err = os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	opts := "lowerdir=" + d.rootfs + ",upperdir=" + filepath.Join(b.dir, "upper") + ",workdir=" + filepath.Join(b.dir, "work")
	if _, err = d.run(ctx, "mount", "-t", "overlay", "overlay", "-o", opts, b.rootfs); err != nil {
		return err
	}
	wakeID := make([]byte, 8)
	_, _ = rand.Read(wakeID)
	s := boxSpec(env, b, d.imageEnv, boxVars(env, hex.EncodeToString(wakeID), login, builder))
	data, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(b.dir, "config.json"), data, 0o600); err != nil {
		return err
	}
	// The box's stdio is create's: a file, as a pipe would stay open as
	// long as the box runs.
	log, err := os.OpenFile(filepath.Join(b.dir, "log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	if _, err = d.host(ctx, Command{Name: "runsc", Args: []string{"--root", d.cfg.State, "create", "--bundle", b.dir, b.cid}, Out: log}); err != nil {
		return err
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

// Sleep stops environment id's box and waits until it is gone, with its
// network and cgroup. A box that isn't there is asleep already.
func (d *Driver) Sleep(ctx context.Context, id string) error {
	ctx, span := d.tracer.Start(ctx, "stop")
	err := d.stop(ctx, id)
	end(span, err)
	if err != nil {
		return fmt.Errorf("sleep %s: %w", id, err)
	}
	return nil
}

// Delete is Sleep: a box has nothing that outlives it.
func (d *Driver) Delete(ctx context.Context, id string) error { return d.Sleep(ctx, id) }

// stop asks fern-init to shut down, waits for the box to exit (killing it
// after stopGrace), deletes it, and removes its network, cgroup and
// directory. Every step is safe to repeat, so a stop that a crash cut short
// is finished by the next.
func (d *Driver) stop(ctx context.Context, id string) error {
	b := newBox(d.cfg.Pool, id)
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
	for _, c := range netDown(b) {
		_, _ = d.run(ctx, c[0], c[1:]...) // gone already, after a crash
	}
	// runsc removes the box's cgroup only on a clean exit.
	_, _ = d.run(ctx, "rmdir", "/sys/fs/cgroup/"+cgroupParent+"/"+id)
	if _, err := d.run(ctx, "mountpoint", "-q", b.rootfs); err == nil {
		if _, err := d.run(ctx, "umount", b.rootfs); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(b.dir); err != nil {
		return err
	}
	return nil
}

// askStop starts fern-init's graceful shutdown with its authenticated
// POST /stop, naming the wake it stops. An older or unreachable box gets
// runsc's SIGTERM instead.
func (d *Driver) askStop(ctx context.Context, b box) {
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
		if w, ok := strings.CutPrefix(v, "FERN_POD_UID="); ok {
			wakeID = w
		}
	}
	n, err := os.ReadFile(filepath.Join(b.dir, "net"))
	if err != nil {
		return
	}
	num, err := strconv.Atoi(strings.TrimSpace(string(n)))
	if err != nil {
		return
	}
	_, guest, err := addrs(num)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	url := "http://" + net.JoinHostPort(guest.String(), strconv.Itoa(boxexec.Port)) + boxexec.StopPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("If-Match", strconv.Quote(wakeID))
	resp, err := d.ready.Do(req)
	if err != nil {
		_, _ = d.runsc(ctx, "kill", b.cid, "TERM")
		return
	}
	_ = resp.Body.Close()
}

// end ends span, marking it failed if err isn't nil.
func end(span trace.Span, err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}
