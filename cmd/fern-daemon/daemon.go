// Command fern-daemon runs the node daemon as a systemd service on its node
// (deploy/systemd/fern-daemon.service).
package main

import (
	"cmp"
	"context"
	"encoding/json"
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
	"github.com/nebler/fern/internal/github"
	"github.com/nebler/fern/internal/preview"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
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
	if err := flags.Parse(args); err != nil {
		return err
	}
	limits, most, err := boxLimits()
	if err != nil {
		return fmt.Errorf("the box limits: %w", err)
	}
	node, err := os.Hostname()
	if err != nil {
		return err
	}

	st, err := store.Open(ctx, pool)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	// Finish what a crash interrupted before anyone can see the half-done state.
	lost, err := st.Reconcile(ctx)
	if err != nil {
		return err
	}
	for _, dir := range lost {
		log.WarnContext(ctx, "environment directory not in the index; left for a person to remove", slog.String("dir", dir))
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
		DNS: strings.Split(*boxDNS, ","), Login: fileVar(loginFile, "CLAUDE_CODE_OAUTH_TOKEN"), Skills: fileSkills(skillsFile, defaultSkills), MCPPort: mcpPort, T3MCP: t3MCP(*t3), Preview: previewAt,
		Limits: limits, MaxLimits: most,
	}, tr, reg)
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
	envs := actor.New(st, drv, cfg, tr, reg, log)
	apiSrv := server.New(st, drv, envs, agentMCP, tr, log)
	apiSrv.Cloudflare = fileVar(cloudflareFile, "CLOUDFLARE_API_TOKEN")
	apiSrv.T3 = *t3
	app, err := github.Load(*githubApp, *githubAPI)
	if err != nil {
		return fmt.Errorf("the GitHub App: %w", err)
	}
	if app != nil {
		apiSrv.GitHub = app
	}
	// T3's git and gh get their tokens on the API's socket, which boxes
	// can't reach.
	api := http.NewServeMux()
	api.Handle("/", apiSrv.Handler())
	api.Handle("POST /github/token", github.Handler(app))
	backups, err := backup.LoadConfig(backupConfig)
	if err != nil {
		return err
	}
	var backer *backup.Backer
	if backups != nil {
		backer = backup.New(backups, node, pool, st, envs, log)
	}
	// fern backup: one now, as the hourly ones make it.
	api.HandleFunc("POST /backup", func(w http.ResponseWriter, r *http.Request) {
		fail := func(code int, msg string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
		}
		if backer == nil {
			fail(http.StatusNotFound, "no backups are set up: "+backupConfig+" is missing (README: backups)")
			return
		}
		if err := backer.Once(r.Context()); errors.Is(err, backup.ErrBusy) {
			fail(http.StatusConflict, err.Error())
			return
		} else if err != nil {
			fail(http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	previews := preview.New(*previewHost, st, envs, log)
	g, ctx := errgroup.WithContext(ctx)
	apiSrv.Stopping = ctx.Done()
	servers := []struct {
		name string
		srv  *http.Server
		lis  net.Listener // nil: not served
	}{
		{"api", &http.Server{Handler: api}, apiLis},
		{"metrics", &http.Server{Handler: metricsHandler(reg)}, metricsLis},
		{"mcp", &http.Server{Handler: apiSrv.MCPHandler()}, mcpLis},
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
	g.Go(func() error { apiSrv.AdoptJobs(ctx); return nil })
	if backer != nil {
		g.Go(func() error { backer.Run(ctx, *backupEvery); return nil })
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
		apiSrv.Close()
		return errors.Join(errs...)
	})
	return g.Wait()
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
		data, err := os.ReadFile(file)
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		for line := range strings.Lines(string(data)) {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), name+"="); ok {
				return v, nil
			}
		}
		return "", nil
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
