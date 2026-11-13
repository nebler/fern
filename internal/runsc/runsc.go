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
	"io"
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
	"github.com/nebler/fern/internal/setup"
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
	// Setup is a golden builder's box before that, for the repository's
	// setup script: it installs the tools and runs no services, which the
	// script comes before.
	Setup
)

// Box is a box to wake.
type Box struct {
	ID   string    // its name: its environment's ID, or the environment's agent box's
	Kind Kind      // what it runs
	Env  store.Env // its folders and token; with Env.Claude "", it gets no Claude folder or login
	// Notices are what its Claude is told has gone wrong on the node, for
	// the person (actor's notices): fern-init puts them at the top of
	// Claude's hint. Only a box with Claude's folder has a use for them.
	Notices []string
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
	Login func(ctx context.Context) (string, error)
	// Skills returns the skill packs the node turns on for Claude, such as
	// "beginner design": a box with Claude's folder learns them as
	// FERN_SKILLS (fern-init). May be nil: none.
	Skills  func(ctx context.Context) (string, error)
	MCPPort int // the daemon's port agent boxes may reach
	// T3MCP is where T3 serves its own MCP server for its Claude, on the
	// node's loopback, such as 127.0.0.1:30773; "" for none. An agent box's
	// fern-init listens there, in the box, and passes the calls to the
	// daemon (FERN_T3_ADDR, FERN_T3_MCP), which passes them to T3.
	T3MCP string
	// Preview is where people open previews, such as localhost:30780
	// (internal/preview). A box learns its own as FERN_PREVIEW, for Claude
	// to hand out links; "" means there are none.
	Preview string
	// Limits are services' boxes' limits; the zero value means DefaultLimits.
	Limits Limits
	// MaxLimits is the most a repository's services.yaml may ask for its
	// box; the zero value means Limits, so that it can only ask for less.
	MaxLimits Limits
}

// The limits of a wake and a stop. fern-init needs at most 8 s for the
// services' stop commands, 3 s more to kill them and 1 s to finish.
const (
	wakeTimeout = 2 * time.Minute
	// A golden builder's box installs the tools its repository pins before
	// it is ready, for every thread after it: it has longer (fern-init's
	// builderInstallTimeout), within a change's ten minutes (actor).
	builderWakeTimeout = 8 * time.Minute
	stopGrace          = 15 * time.Second
	readyFirst         = 2 * time.Millisecond
	exitedEvery        = time.Second // how often a box that isn't ready is looked for
	readyInterval      = 50 * time.Millisecond
	readyWait          = 5 * time.Second
	// A box found running is kept if it is ready within adoptWait, which is
	// longer than fern-init's longest pause before it starts a service again
	// (30 s), and given up on once its fern-init has not answered at all for
	// adoptSilence: one that runs listens within a tenth of that.
	adoptWait    = 45 * time.Second
	adoptSilence = time.Second
)

// found is what there is on the node of a box that is to be woken.
type found int

const (
	// foundNothing: no directory. Its last stop finished, or it never ran:
	// the wake starts it.
	foundNothing found = iota
	// foundRunning: its container runs. It outlived a daemon, or a wake of it
	// was cut short after its start: the wake keeps it if it gets ready.
	foundRunning
	// foundLeftover: a directory and no running container, what a wake or a
	// stop that was cut short leaves: the wake clears it, then starts the box.
	foundLeftover
)

// find says what there is of b's box, given whether its directory exists and
// runsc's list.
func find(dir bool, cs []container, b layout) found {
	switch {
	case !dir:
		return foundNothing
	case slices.ContainsFunc(cs, func(c container) bool { return c.ID == b.cid && c.Status == "running" }):
		return foundRunning
	default:
		return foundLeftover
	}
}

// look says what there is of b's box on the node, and gives runsc's list if
// it had to ask for it. A box that was stopped has no directory, as its stop
// removes that last: there is none to find then, and runsc isn't asked, which
// keeps its 16 ms out of an ordinary wake.
func (d *Driver) look(ctx context.Context, b layout) (found, []container, error) {
	_, err := os.Lstat(b.dir)
	dir := !errors.Is(err, fs.ErrNotExist)
	var cs []container
	if dir {
		if cs, err = d.list(ctx); err != nil {
			return foundNothing, nil, err
		}
	}
	return find(dir, cs, b), cs, nil
}

// readiness is what a box's fern-init says of its services.
type readiness int

const (
	// noAnswer: nothing answers at its address. It hasn't started, hangs or
	// is gone.
	noAnswer readiness = iota
	// notReady: fern-init answers, and a service's ready check fails.
	notReady
	// ready: every service is.
	ready
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
// address of its exec server. A box that runs already, such as one that
// outlived a daemon restart, is kept if it gets ready (adopt), and with it
// the commands running in it; anything else left from before (a crash, a
// stop that didn't finish) is stopped first, so a wake starts clean.
func (d *Driver) Wake(ctx context.Context, box Box) (string, error) {
	limit := wakeTimeout
	if box.Kind == Builder || box.Kind == Setup {
		limit = builderWakeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	b := d.layout(box.ID)
	what, cs, err := d.look(ctx, b)
	if err != nil {
		return "", fmt.Errorf("wake %s: %w", box.ID, err)
	}
	if what == foundRunning {
		if _, guest, err := linkAddrs(b); err == nil && d.adopt(ctx, execAddr(guest)) {
			return execAddr(guest), nil
		}
		what = foundLeftover // it didn't get ready: it is started anew
	}
	if what == foundLeftover {
		if err := d.stopListed(ctx, box.ID, cs); err != nil {
			return "", fmt.Errorf("wake %s: %w", box.ID, err)
		}
	}
	start := time.Now()
	addr, err := d.netUp(ctx, b, box.Kind)
	if err != nil && what == foundNothing {
		// Its last stop left some of its network, which it can't say: a stop
		// now clears it.
		if err := d.stop(ctx, box.ID); err != nil {
			return "", fmt.Errorf("wake %s: %w", box.ID, err)
		}
		addr, err = d.netUp(ctx, b, box.Kind)
	}
	if err != nil {
		return "", fmt.Errorf("wake %s: %w", box.ID, err)
	}
	netDone := time.Now()
	if err := d.start(ctx, box, b); err != nil {
		return "", fmt.Errorf("wake %s: %w", box.ID, err)
	}
	started := time.Now()
	_, sp := d.tracer.Start(ctx, "ready")
	err = d.waitReady(ctx, b, addr)
	span.End(sp, err)
	if err != nil {
		return "", fmt.Errorf("wake %s: %w", box.ID, err)
	}
	d.steps.WithLabelValues("net").Observe(netDone.Sub(start).Seconds())
	d.steps.WithLabelValues("start").Observe(started.Sub(netDone).Seconds())
	d.steps.WithLabelValues("ready").Observe(time.Since(started).Seconds())
	return addr, nil
}

// adopt waits for the box that runs at addr to be ready, and says whether
// it is. A box whose services aren't ready yet is waited for, as a new one
// would be: stopping it would end the commands that outlived the daemon in
// it, for a service that is only starting again. It gives up after
// adoptWait, and on a fern-init that has not answered for adoptSilence.
func (d *Driver) adopt(ctx context.Context, addr string) bool {
	ctx, cancel := context.WithTimeout(ctx, adoptWait)
	defer cancel()
	silent := time.Now() // since when fern-init has not answered
	for {
		switch answer, _ := d.ask(ctx, addr); answer {
		case ready:
			return true
		case notReady:
			silent = time.Now()
		case noAnswer:
			if time.Since(silent) >= adoptSilence {
				return false
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(readyInterval):
		}
	}
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
	node, inside, err := netUp(b, kind, n)
	if err != nil {
		return "", err
	}
	for _, batch := range []struct {
		argv  []string
		stdin string
	}{
		{[]string{"ip", "-batch", "-"}, node},
		{[]string{"ip", "-n", b.nsName(), "-batch", "-"}, inside},
	} {
		ip := exec.CommandContext(ctx, batch.argv[0], batch.argv[1:]...)
		ip.Stdin = strings.NewReader(batch.stdin)
		if _, err := run(ip); err != nil {
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
	login, skills := "", ""
	if box.Env.Claude != "" && d.cfg.Login != nil {
		if login, err = d.cfg.Login(ctx); err != nil {
			return fmt.Errorf("claude login: %w", err)
		}
	}
	if box.Env.Claude != "" && d.cfg.Skills != nil {
		if skills, err = d.cfg.Skills(ctx); err != nil {
			return fmt.Errorf("claude's skills: %w", err)
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
	// The box's token is made here, for this wake alone, and kept with the
	// bundle on the node's tmpfs: it is in no record, snapshot or backup,
	// and no other box has the same one.
	token := rand.Text()
	if err := os.WriteFile(filepath.Join(b.dir, tokenFile), []byte(token), 0o600); err != nil {
		return err
	}
	vars := boxVars(box, hex.EncodeToString(wakeID), token, login, d.cfg.MCPPort)
	if skills != "" {
		vars = append(vars, "FERN_SKILLS="+skills)
	}
	if len(box.Notices) > 0 {
		notices, err := json.Marshal(box.Notices)
		if err != nil {
			return err
		}
		vars = append(vars, "FERN_NOTICES="+string(notices))
	}
	if box.Kind == Agent && d.cfg.T3MCP != "" {
		vars = append(vars, "FERN_T3_ADDR="+d.cfg.T3MCP,
			"FERN_T3_MCP=http://"+net.JoinHostPort(DaemonHost, strconv.Itoa(d.cfg.MCPPort))+"/t3/"+box.Env.ID)
	}
	if d.cfg.Preview != "" {
		// {port} is for the box to fill in.
		vars = append(vars, "FERN_PREVIEW=http://{port}-"+box.Env.ID+"."+d.cfg.Preview+"/")
	}
	asked, askedErr := setup.AskedLimits(box.Env.Workspace)
	if box.Kind == Agent {
		// Claude's box doesn't depend on the repository's services.yaml, which
		// Claude may be there to mend. It learns what the services' box gets,
		// to say so when the repository asks for more (fern-init's hint).
		if services, err := d.cfg.limitsOf(Services, asked); askedErr == nil && err == nil {
			vars = append(vars, boxLimitsVar(services))
		}
		asked = setup.Limits{}
	} else if askedErr != nil {
		return askedErr
	}
	lim, err := d.cfg.limitsOf(box.Kind, asked)
	if err != nil {
		return err
	}
	s := boxSpec(box, b, d.imageEnv, vars, lim)
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

// waitReady asks the fern-init of b's box, at addr, until it says its
// services are ready. Until fern-init listens, every answer is a refusal at
// once: it is asked again soon at first, so that a box up in tens of
// milliseconds isn't found up half an interval late, and less often after.
//
// A fern-init that has exited never answers: one that can't read its
// services.yaml, or bind a socket. So the box is looked for now and then,
// and the wake of one that is gone fails with the end of its log, not with
// wakeTimeout two minutes later.
func (d *Driver) waitReady(ctx context.Context, b layout, addr string) error {
	looked := time.Now()
	said := "its fern-init didn't answer"
	for wait := readyFirst; ; wait = min(2*wait, readyInterval) {
		answer, why := d.ask(ctx, addr)
		if answer == ready {
			return nil
		}
		if answer == notReady {
			said = why
		}
		if time.Since(looked) >= exitedEvery {
			looked = time.Now()
			cs, err := d.list(ctx)
			if err == nil && !slices.ContainsFunc(cs, func(c container) bool { return c.ID == b.cid && c.Status != "stopped" }) {
				return fmt.Errorf("the box exited as it started; its log ends: %s", logEnd(filepath.Join(b.data, "log")))
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not ready: %s: %w", said, ctx.Err())
		case <-time.After(wait):
		}
	}
}

// logEnd returns the last lines of the log at path, for an error.
func logEnd(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(no log: " + err.Error() + ")"
	}
	data = bytes.TrimSpace(data)
	if len(data) > 600 {
		data = data[len(data)-600:]
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	return string(data)
}

// ask asks the fern-init at addr whether its services are ready, and waits
// up to readyWait for them to be. With notReady it returns what fern-init
// said of them: which service, its check's output, and whether it keeps
// exiting.
func (d *Driver) ask(ctx context.Context, addr string) (readiness, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+boxexec.ReadyPath+"?wait="+readyWait.String(), nil)
	if err != nil {
		return noAnswer, ""
	}
	resp, err := d.ready.Do(req)
	if err != nil {
		return noAnswer, "" // not listening yet
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return ready, ""
	}
	why, _ := io.ReadAll(io.LimitReader(resp.Body, maxSaid))
	return notReady, string(bytes.TrimSpace(why))
}

// maxSaid is how much of fern-init's answer a failed wake's error quotes.
const maxSaid = 600

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
	cs, err := d.list(ctx)
	if err != nil {
		return err
	}
	return d.stopListed(ctx, id, cs)
}

// stopListed is stop for a caller that has runsc's list, cs, already.
func (d *Driver) stopListed(ctx context.Context, id string, cs []container) error {
	b := d.layout(id)
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
	if mounted, err := isMountPoint(b.rootfs); err != nil {
		return err
	} else if mounted {
		if err := unix.Unmount(b.rootfs, 0); err != nil {
			return fmt.Errorf("unmount %s: %w", b.rootfs, err)
		}
	}
	for _, dir := range []string{b.dir, b.data} {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return nil
}

// tokenFile is where a box's bundle keeps the token made at its start.
const tokenFile = "token"

// Token returns the token box id was started with, which authenticates the
// daemon to its fern-init and the box to the daemon. Each start makes a new
// one, so a box that isn't running has none: the error is fs.ErrNotExist.
// It wakes nothing.
func (d *Driver) Token(id string) (string, error) {
	b := d.layout(id)
	data, err := os.ReadFile(filepath.Join(b.dir, tokenFile))
	if errors.Is(err, fs.ErrNotExist) {
		// A box started before 2026-11-12 has it in its spec only.
		token, _, err := bundleVars(b)
		return token, err
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// bundleVars reads the token and the wake ID a box was started with from
// its bundle's spec.
func bundleVars(b layout) (token, wakeID string, err error) {
	data, err := os.ReadFile(filepath.Join(b.dir, "config.json"))
	if err != nil {
		return "", "", err
	}
	var s spec
	if err := json.Unmarshal(data, &s); err != nil {
		return "", "", err
	}
	for _, v := range s.Process.Env {
		if t, ok := strings.CutPrefix(v, "FERN_BOX_TOKEN="); ok {
			token = t
		}
		if w, ok := strings.CutPrefix(v, "FERN_WAKE_ID="); ok {
			wakeID = w
		}
	}
	return token, wakeID, nil
}

// askStop starts fern-init's graceful shutdown with its authenticated
// POST /stop, naming the wake it stops. A box that doesn't answer, or
// refuses (an older fern-init), gets runsc's SIGTERM instead.
func (d *Driver) askStop(ctx context.Context, b layout) {
	token, wakeID, err := bundleVars(b)
	if err != nil {
		return
	}
	_, guest, err := linkAddrs(b)
	if err != nil {
		return
	}
	// The request's own deadline: the SIGTERM below must not share it, or a
	// box that didn't answer in time would get none.
	reqCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, "http://"+execAddr(guest)+boxexec.StopPath, nil)
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

// isMountPoint reports whether something is mounted at path: the box's
// overlay is a filesystem of its own, so its device differs from its
// parent folder's. A path that isn't there has nothing mounted.
func isMountPoint(path string) (bool, error) {
	var st, parent unix.Stat_t
	if err := unix.Lstat(path, &st); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	if err := unix.Lstat(filepath.Dir(path), &parent); err != nil {
		return false, fmt.Errorf("stat %s: %w", filepath.Dir(path), err)
	}
	return st.Dev != parent.Dev, nil
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

// BoxOf returns the ID of the box process pid runs in, by its cgroup, or ""
// if it runs in none: runsc puts a box's sandbox and gofer in the box's
// cgroup (boxSpec).
func BoxOf(pid int) string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return "" // it has exited
	}
	return boxOfCgroup(string(data))
}

// boxOfCgroup returns the box whose cgroup /proc/<pid>/cgroup names, or "".
func boxOfCgroup(data string) string {
	for line := range strings.Lines(data) {
		cg, ok := strings.CutPrefix(strings.TrimSpace(line), "0::/"+cgroupParent+"/")
		if ok && cg != "" && !strings.Contains(cg, "/") {
			return cg
		}
	}
	return ""
}
