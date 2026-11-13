// Command fern-daemon runs the node daemon as a systemd service on its node
// (deploy/systemd/fern-daemon.service).
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"golang.org/x/sync/errgroup"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/api/server"
	"github.com/nebler/fern/internal/backup"
	"github.com/nebler/fern/internal/commit"
	"github.com/nebler/fern/internal/disk"
	"github.com/nebler/fern/internal/envfile"
	"github.com/nebler/fern/internal/github"
	"github.com/nebler/fern/internal/health"
	"github.com/nebler/fern/internal/preview"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
	"github.com/nebler/fern/internal/tools"
	"github.com/nebler/fern/internal/watch"
)

// Where the daemon finds the pool, serves its API and keeps its boxes.
const (
	socket = "/run/fern/fern.sock"
	pool   = "/var/lib/fern"
	// boxImage is what lab/build.sh builds and loads into every node (fern
	// image load).
	boxImage = "fern-box:dev"
	// loginFile holds Claude's login as CLAUDE_CODE_OAUTH_TOKEN=…, readable
	// by root only. T3's server reads it as its environment.
	loginFile = "/etc/fern/claude.env"
	// runscState is runsc's state directory, and runscBundles the boxes'
	// bundles: both on the node's tmpfs.
	runscState   = "/run/fern/runsc"
	runscBundles = "/run/fern/boxes"
	// mcpPort is where agent boxes reach the daemon's MCP endpoint.
	mcpPort = 7071
	// diskEvery is how often the daemon looks at the free space of the pool
	// and the disk under it (internal/disk).
	diskEvery = time.Minute
	// cloudflareFile holds the node's Cloudflare API token as
	// CLOUDFLARE_API_TOKEN=…, readable by root only, if someone opted in
	// (install.sh cloudflare): agents in Cloudflare repositories' agent
	// boxes get Cloudflare's MCP server with it.
	cloudflareFile = "/etc/fern/cloudflare.env"
	// skillsFile lists the skill packs the node turns on for Claude
	// (install.sh skills): names, separated by spaces or lines.
	skillsFile = "/etc/fern/skills"
	// defaultSkills are the packs without skillsFile.
	defaultSkills = "safety"
	// backupConfig says where backups go (internal/backup), root only.
	backupConfig = "/etc/fern/backup.env"
	// notifyEvery is how often the daemon looks whether the node's problems
	// have changed, to tell --notify-url (health.Notifier).
	notifyEvery = time.Minute
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runDaemon(ctx, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}

// runDaemon serves the daemon's API until ctx is cancelled, and
// returns the process exit code.
func runDaemon(ctx context.Context, args []string, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, nil))
	// fern-daemon restore NODE [POOL], on a new node with the daemon
	// stopped; POOL, another pool than the node's, is for tests.
	if len(args) >= 2 && len(args) <= 3 && args[0] == "restore" {
		into := pool
		if len(args) == 3 {
			into = args[2]
		}
		if err := restoreBackup(ctx, args[1], into, log); err != nil {
			log.ErrorContext(ctx, "restore failed", slog.Any("error", err))
			return 1
		}
		return 0
	}
	if err := serve(ctx, args, log); err != nil {
		log.ErrorContext(ctx, "fern daemon failed", slog.Any("error", err))
		return 1
	}
	return 0
}

// limitFlags adds the flags for boxes' limits, and returns what reads them
// once the flags are parsed: a services' box's limits, and the most a
// repository may ask for its own in its services.yaml, which its agent can
// write. Agent boxes keep the defaults: they run the agent, not the
// repository.
func limitFlags(flags *flag.FlagSet) func() (limits, most runsc.Limits, err error) {
	def := runsc.DefaultLimits
	cpus := flags.Float64("box-cpus", def.CPUs, "the CPUs a box may use, from its next wake on")
	memory := flags.Int64("box-memory", def.Memory>>20, "… and its memory, in MiB")
	pids := flags.Int64("box-pids", def.Pids, "… and its processes and threads")
	maxCPUs := flags.Float64("box-max-cpus", 0, "the most CPUs a repository may ask for its box (0: --box-cpus, so only less)")
	maxMemory := flags.Int64("box-max-memory", 0, "… and the most memory, in MiB (0: --box-memory)")
	maxPids := flags.Int64("box-max-pids", 0, "… and the most processes and threads (0: --box-pids)")
	return func() (limits, most runsc.Limits, err error) {
		limits = runsc.Limits{CPUs: *cpus, Memory: *memory << 20, Pids: *pids}
		if err := limits.Check(); err != nil {
			return limits, most, err
		}
		most = runsc.Limits{
			CPUs: cmp.Or(*maxCPUs, limits.CPUs), Memory: cmp.Or(*maxMemory<<20, limits.Memory), Pids: cmp.Or(*maxPids, limits.Pids),
		}
		if most.CPUs < limits.CPUs || most.Memory < limits.Memory || most.Pids < limits.Pids {
			return limits, most, errors.New("a --box-max-… is under its --box-…")
		}
		return limits, most, nil
	}
}

func serve(ctx context.Context, args []string, log *slog.Logger) error {
	flags := flag.NewFlagSet("daemon", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var cfg actor.Config
	flags.DurationVar(&cfg.Idle.Agent, "idle-agent", 5*time.Minute, "sleep this long after a box's last command ended (0: never)")
	flags.DurationVar(&cfg.Idle.Human, "idle-human", 20*time.Minute, "… and this long after it started (0: never)")
	flags.DurationVar(&cfg.Idle.Prewake, "idle-prewake", 2*time.Minute, "sleep a box woken ahead this long after its wake, unless a command comes (0: no pre-wakes)")
	flags.DurationVar(&cfg.SetupTimeout, "setup-timeout", actor.DefaultSetupTimeout, "stop a repository's setup script that runs longer than this in a golden build")
	flags.BoolVar(&cfg.Agents, "agent-boxes", true, "run Claude in a box of its own per environment, whose Bash runs in the environment's box (docs/briefs/claude-outside-the-box.md)")
	// Localhost only: Go's profiles cost CPU on demand, and nothing but the
	// node itself needs them. A scraper elsewhere sets an address.
	metricsAddr := flags.String("metrics-addr", "127.0.0.1:9090", "serve Prometheus metrics at /metrics, and Go's profiles at /debug/pprof/, on this address")
	traceEndpoint := flags.String("trace-endpoint", "", "send traces by OTLP/HTTP to this URL, such as http://10.0.0.1:4318 (\"\": none)")
	boxDNS := flags.String("box-dns", "1.1.1.1,8.8.8.8", "the resolvers boxes use")
	boxLimits := limitFlags(flags)
	// On every address, as T3's port: the node's firewall keeps it private.
	previewAddr := flags.String("preview-addr", ":30780", "serve previews of boxes' ports on this address (\"\": none)")
	previewHost := flags.String("preview-host", "localhost", "previews are http://PORT-ENV.HOST/ and http://ENV.HOST/")
	t3 := flags.String("t3", "http://127.0.0.1:30773", "T3's server on this node: Claude in an agent box reaches T3's MCP server there through the daemon (\"\": not)")
	githubApp := flags.String("github-app", "/etc/fern/github-app", "the GitHub App T3's git and gh get tokens from: its id and key.pem (missing: none)")
	githubAPI := flags.String("github-api", github.API, "GitHub's API (another for tests)")
	backupEvery := flags.Duration("backup-every", time.Hour, "back up the threads this often, if "+backupConfig+" says where")
	notifyURL := flags.String("notify-url", "", "POST a short text to this URL when something goes wrong on the node, and when all is well again, such as https://ntfy.sh/TOPIC (\"\": don't)")
	heartbeatURL := flags.String("heartbeat-url", "", "GET this URL every --heartbeat-every while the daemon runs and its store answers, for a watcher that tells when the GETs stop, such as https://hc-ping.com/UUID (\"\": don't)")
	heartbeatEvery := flags.Duration("heartbeat-every", 5*time.Minute, "how often to GET --heartbeat-url")
	if err := flags.Parse(args); err != nil {
		return err
	}
	limits, most, err := boxLimits()
	if err != nil {
		return fmt.Errorf("the box limits: %w", err)
	}
	if *notifyURL != "" {
		if err := health.CheckURL(*notifyURL); err != nil {
			return fmt.Errorf("--notify-url: %w", err)
		}
	}
	if *heartbeatURL != "" {
		if err := health.CheckURL(*heartbeatURL); err != nil {
			return fmt.Errorf("--heartbeat-url: %w", err)
		}
		if *heartbeatEvery <= 0 {
			return errors.New("--heartbeat-every: want more than 0")
		}
	}
	node, err := os.Hostname()
	if err != nil {
		return err
	}

	st, err := store.Open(ctx, pool)
	if err != nil {
		return err
	}
	// Finish what a crash interrupted before anyone can see the half-done state.
	lost, err := st.Reconcile(ctx)
	if err != nil {
		return err
	}
	for _, dir := range lost {
		log.WarnContext(ctx, "environment directory without a record it can use; left for a person to look at", slog.String("dir", dir))
	}
	// T3's server runs as the box user, and uses the API too.
	apiLis, err := listenUnix(ctx, socket, store.BoxGID)
	if err != nil {
		return err
	}
	metricsLis, err := new(net.ListenConfig).Listen(ctx, "tcp", *metricsAddr)
	if err != nil {
		return err
	}
	var previewLis net.Listener
	previewAt := "" // where boxes tell people to open their previews
	if *previewAddr != "" {
		if previewLis, err = new(net.ListenConfig).Listen(ctx, "tcp", *previewAddr); err != nil {
			return err
		}
		previewAt = *previewHost
		if _, port, err := net.SplitHostPort(*previewAddr); err == nil && port != "80" {
			previewAt += ":" + port
		}
	}
	var mcpLis net.Listener
	agentMCP := ""
	if cfg.Agents {
		// On every address: the boxes' firewall lets only agent boxes in.
		if mcpLis, err = new(net.ListenConfig).Listen(ctx, "tcp", ":"+strconv.Itoa(mcpPort)); err != nil {
			return err
		}
		agentMCP = "http://" + runsc.DaemonHost + ":" + strconv.Itoa(mcpPort)
	}

	tp, err := tracerProvider(ctx, *traceEndpoint, node)
	if err != nil {
		return err
	}
	defer func() {
		// Send the spans still buffered; a collector that is gone costs at most this long.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(shutdownCtx); err != nil {
			log.WarnContext(ctx, "sending the last traces failed", slog.Any("error", err))
		}
	}()
	tr := tp.Tracer("github.com/nebler/fern")
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	drv := runsc.New(runsc.Config{
		Pool: pool, Bundles: runscBundles, State: runscState, Images: filepath.Join(pool, "images"), Image: boxImage,
		DNS: strings.Split(*boxDNS, ","), MCPPort: mcpPort,
	}, tr, reg)
	// What boxes get is the actors' to decide (actor's boxOf).
	cfg.Login, cfg.Skills = fileVar(loginFile, "CLAUDE_CODE_OAUTH_TOKEN"), fileSkills(skillsFile, defaultSkills)
	cfg.MCPPort, cfg.T3MCP, cfg.Preview = mcpPort, t3MCP(*t3), previewAt
	cfg.Limits, cfg.MaxLimits = limits, most
	// Golden keys name the image's content, not its tag.
	if cfg.Image, err = drv.Prepare(ctx); err != nil {
		return fmt.Errorf("prepare the node for boxes: %w", err)
	}
	if cfg.Agents {
		cfg.Watcher = watch.New(runsc.BoxOf, log)
	}
	// Woken boxes' default previews are rendered ahead.
	woken := make(chan string, 16)
	if previewLis != nil {
		cfg.Woken = woken
	}
	// A turn's snapshot that fails is kept with its environment, for fern
	// doctor and the thread's Claude.
	cfg.SnapshotFailures = st
	envs := actor.New(st, drv, cfg, tr, reg, log)
	apiSrv := server.New(st, drv, envs, cfg.Agents, tr, log)
	// What Claude in an agent box gets in place of its own shell.
	toolSrv := tools.New(st, drv, envs, agentMCP, log)
	apiSrv.StartAgent = toolSrv.StartAgent
	toolSrv.Cloudflare = fileVar(cloudflareFile, "CLOUDFLARE_API_TOKEN")
	toolSrv.T3 = *t3
	// Claude's commits: the node's git as T3's user, with T3's home, whose
	// .gitconfig names the author.
	toolSrv.Git = &commit.Committer{
		Git: "git", User: &syscall.Credential{Uid: store.BoxUID, Gid: store.BoxGID}, Home: filepath.Join(pool, "projects"),
	}
	app, err := github.Load(*githubApp, *githubAPI)
	if err != nil {
		return fmt.Errorf("the GitHub App: %w", err)
	}
	if app != nil {
		toolSrv.GitHub = app
	}
	// T3's git and gh get their tokens on the API's socket, which boxes
	// can't reach.
	apiSrv.GitHubToken = github.Handler(app)
	backups, err := backup.LoadConfig(backupConfig)
	if err != nil {
		return err
	}
	// fern backup: one now, as the hourly ones make it.
	var backer *backup.Backer
	apiSrv.BackupConfig = backupConfig
	if backups != nil {
		backer = backup.New(backups, node, pool, st, envs, log)
		apiSrv.Backups = backer
	}

	// fern df and fern gc: what uses the node's disk, and what nothing needs
	// removed from it.
	keeper := disk.New(st, envs, disk.Config{Root: pool, Proc: "/proc", Podman: disk.Podman, Sizes: store.PathUsage}, log)
	apiSrv.Disk = keeper

	// fern doctor: what is wrong now that would otherwise be a line in this
	// daemon's log, or the error of a box's next command. The same list is a
	// metric, with the pool's room.
	// btrfs's room for its own records is read by a program, at most every
	// ten minutes, by these looks only: never on a wake's or a snapshot's way.
	checker := health.Checker{Store: st, Pool: pool, Boxes: envs, Records: &health.Records{Btrfs: "btrfs", Pool: pool}}
	if backer != nil {
		checker.BackupEvery = *backupEvery
	}
	apiSrv.Health = checker
	registerGauges(ctx, reg, checker, st, drv)

	previews := preview.New(*previewHost, st, envs, log)
	g, ctx := errgroup.WithContext(ctx)
	apiSrv.Stopping, toolSrv.Stopping = ctx.Done(), ctx.Done()
	servers := []struct {
		name string
		srv  *http.Server
		lis  net.Listener // nil: not served
	}{
		{"api", &http.Server{Handler: apiSrv.Handler()}, apiLis},
		{"metrics", &http.Server{Handler: metricsHandler(reg)}, metricsLis},
		{"mcp", &http.Server{Handler: toolSrv.MCPHandler()}, mcpLis},
		{"preview", &http.Server{Handler: previews}, previewLis},
	}
	log.InfoContext(ctx, "fern daemon started", slog.String("node", node))
	// Changes outlive their requests: Run returns once they have ended, and
	// the store stays open until then.
	g.Go(func() error { return envs.Run(ctx) })
	if previewLis != nil {
		g.Go(func() error { previews.Warm(ctx, woken); return nil })
	}
	// Background commands an earlier daemon watched run on in their boxes.
	g.Go(func() error { toolSrv.AdoptJobs(ctx); return nil })
	if backer != nil {
		g.Go(func() error { backer.Run(ctx, *backupEvery); return nil })
	}
	// A disk that fills up fails every thread: free what nothing needs first.
	g.Go(func() error { keeper.Run(ctx, diskEvery); return nil })
	// What doctor would list also reaches a person who isn't on the server.
	if *notifyURL != "" {
		notifier := health.Notifier{Checker: checker, URL: *notifyURL, Node: node, File: filepath.Join(pool, health.NotifiedFile), Log: log}
		g.Go(func() error { notifier.Run(ctx, notifyEvery); return nil })
	}
	// And a watcher outside the server hears when the daemon, or the server,
	// has stopped: the beats stop.
	if *heartbeatURL != "" {
		heartbeat := health.Heartbeat{Store: st, URL: *heartbeatURL, Log: log}
		g.Go(func() error { heartbeat.Run(ctx, *heartbeatEvery); return nil })
	}
	for _, s := range servers {
		if s.lis == nil {
			continue
		}
		s.srv.BaseContext = func(net.Listener) context.Context { return ctx }
		s.srv.ReadHeaderTimeout = 10 * time.Second
		g.Go(func() error {
			if err := s.srv.Serve(s.lis); !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("%s: %w", s.name, err)
			}
			return nil
		})
	}
	g.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		var errs []error
		for _, s := range servers {
			// A Bash call can run for minutes: its connection just breaks, and
			// the agent box's relay sends it again to the next daemon.
			if s.name == "mcp" {
				errs = append(errs, s.srv.Close())
				continue
			}
			errs = append(errs, s.srv.Shutdown(shutdownCtx))
		}
		// Claude's background commands, which no request holds.
		toolSrv.Close()
		return errors.Join(errs...)
	})
	return g.Wait()
}

// registerGauges adds to reg what is read when the metrics are, not counted
// as it happens: the node's problems, its environments, the boxes that are
// up and the room the pool and the disk under it have. A value that can't
// be read is -1. Listing the boxes runs runsc, once per read.
func registerGauges(ctx context.Context, reg prometheus.Registerer, checker health.Checker, st *store.Store, drv *runsc.Driver) {
	gauge := func(name, help string, read func() (float64, error)) {
		promauto.With(reg).NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, func() float64 {
			v, err := read()
			if err != nil {
				return -1
			}
			return v
		})
	}
	gauge("fern_problems", "What is wrong on the node now (fern doctor lists them): failed golden builds, failing backups, a full pool, boxes that keep crashing.", func() (float64, error) {
		p, err := checker.Problems(ctx)
		return float64(len(p)), err
	})
	gauge("fern_environments", "Environments in the pool, golden builders included.", func() (float64, error) {
		envs, err := st.List(ctx)
		return float64(len(envs)), err
	})
	gauge("fern_boxes_up", "Boxes that are running or starting, agent boxes included.", func() (float64, error) {
		states, err := drv.States(ctx)
		return float64(len(states)), err
	})
	space := func(pick func(store.Space) float64) func() (float64, error) {
		return func() (float64, error) {
			sp, err := st.Space()
			return pick(sp), err
		}
	}
	gauge("fern_pool_bytes", "The pool's size.", space(func(sp store.Space) float64 { return float64(sp.Pool.Total) }))
	gauge("fern_pool_free_bytes", "The pool's free space.", space(func(sp store.Space) float64 { return float64(sp.Pool.Free) }))
	gauge("fern_pool_least_free_bytes", "The free space below which new snapshots and environments are refused.", space(func(sp store.Space) float64 { return float64(sp.Least) }))
	gauge("fern_disk_free_bytes", "The free space of the disk a pool that is a file is on; -1 for a pool on a partition of its own.", space(func(sp store.Space) float64 {
		if sp.Under == nil {
			return -1
		}
		return float64(sp.Under.Free)
	}))
}

// metricsHandler serves reg's metrics at /metrics and Go's profiles at
// /debug/pprof/. Profiles cost nothing until asked for; boxes can't reach
// the port, as their firewall blocks the node, and by default it listens on
// localhost only.
func metricsHandler(reg *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// fileVar returns a function that reads variable name from file, a line
// NAME=…, at every call (Claude's login at every wake), so a new value needs
// no restart: "" while there is none.
func fileVar(file, name string) func(ctx context.Context) (string, error) {
	return func(context.Context) (string, error) {
		vars, err := envfile.Read(file)
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return vars[name], err
	}
}

// t3MCP returns the address T3's server at URL t3 names its MCP server at to
// its Claude, for an agent box's fern-init to answer at: T3 on the node's
// loopback, which is where a box can stand in for it. "" otherwise.
func t3MCP(t3 string) string {
	u, err := url.Parse(t3)
	if err != nil || u.Port() == "" {
		return ""
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err != nil || !ip.IsLoopback() {
		return ""
	}
	return u.Host
}

// fileSkills returns a function that reads the skill packs in file at every
// wake, so a change needs no restart: def without the file. Lines starting
// with # are comments.
func fileSkills(file, def string) func(ctx context.Context) (string, error) {
	return func(context.Context) (string, error) {
		data, err := os.ReadFile(file)
		if errors.Is(err, fs.ErrNotExist) {
			return def, nil
		}
		if err != nil {
			return "", err
		}
		var packs []string
		for line := range strings.Lines(string(data)) {
			if line, _, _ = strings.Cut(line, "#"); strings.TrimSpace(line) != "" {
				packs = append(packs, strings.Fields(line)...)
			}
		}
		return strings.Join(packs, " "), nil
	}
}

// tracerProvider returns a provider whose spans go to endpoint by OTLP/HTTP,
// as the service fern-daemon on node, or one that records nothing if
// endpoint is "". Its Shutdown sends what is buffered.
func tracerProvider(ctx context.Context, endpoint, node string) (tracing, error) {
	if endpoint == "" {
		return noopProvider{noop.NewTracerProvider()}, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("trace endpoint: %w", err)
	}
	// The exporter posts to the URL's path as it is, and a collector's
	// traces are at /v1/traces.
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/traces"
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(u.String()))
	if err != nil {
		return nil, fmt.Errorf("trace exporter: %w", err)
	}
	res := resource.NewSchemaless(attribute.String("service.name", "fern-daemon"), attribute.String("host.name", node))
	return sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res)), nil
}

// tracing is a TracerProvider whose Shutdown sends the spans it holds.
type tracing interface {
	trace.TracerProvider
	Shutdown(ctx context.Context) error
}

// noopProvider is a provider that records nothing, with nothing to send.
type noopProvider struct{ noop.TracerProvider }

func (noopProvider) Shutdown(context.Context) error { return nil }

// listenUnix listens on a unix socket at path, replacing a stale one, and
// lets only the owner (root) connect, and group gid if it isn't -1.
func listenUnix(ctx context.Context, path string, gid int) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var lc net.ListenConfig
	lis, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	mode := os.FileMode(0o600)
	if gid != -1 {
		mode = 0o660
		if err := os.Chown(path, -1, gid); err != nil {
			_ = lis.Close()
			return nil, err
		}
	}
	if err := os.Chmod(path, mode); err != nil {
		_ = lis.Close()
		return nil, err
	}
	return lis, nil
}

// restoreBackup brings node's latest backup into the pool: fern-daemon
// restore NODE, on a new node, with the daemon and T3 stopped.
func restoreBackup(ctx context.Context, node, into string, log *slog.Logger) error {
	cfg, err := backup.LoadConfig(backupConfig)
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("%s is missing: it says where the backups are", backupConfig)
	}
	return backup.Restore(ctx, cfg, node, into, log)
}
