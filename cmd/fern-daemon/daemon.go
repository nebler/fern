// Command fern-daemon runs the node daemon as a systemd service on its node
// (deploy/systemd/fern-daemon.service).
package main

import (
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
	if err := serve(ctx, args, log); err != nil {
		log.ErrorContext(ctx, "fern daemon failed", slog.Any("error", err))
		return 1
	}
	return 0
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
	if err := flags.Parse(args); err != nil {
		return err
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
		DNS: strings.Split(*boxDNS, ","), Login: fileLogin(loginFile), MCPPort: mcpPort,
	}, tr, reg)
	// Golden keys name the image's content, not its tag.
	if cfg.Image, err = drv.Prepare(ctx); err != nil {
		return fmt.Errorf("prepare the node for boxes: %w", err)
	}
	if cfg.Agents {
		cfg.Watcher = watch.New(runsc.BoxOf, log)
	}
	envs := actor.New(st, drv, cfg, tr, reg, log)
	apiSrv := server.New(st, drv, envs, agentMCP, tr, log)

	g, ctx := errgroup.WithContext(ctx)
	servers := []struct {
		name string
		srv  *http.Server
		lis  net.Listener // nil: not served
	}{
		{"api", &http.Server{Handler: apiSrv.Handler()}, apiLis},
		{"metrics", &http.Server{Handler: metricsHandler(reg)}, metricsLis},
		{"mcp", &http.Server{Handler: apiSrv.MCPHandler()}, mcpLis},
	}
	log.InfoContext(ctx, "fern daemon started", slog.String("node", node))
	// Changes outlive their requests: Run returns once they have ended, and
	// the store stays open until then.
	g.Go(func() error { return envs.Run(ctx) })
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
			errs = append(errs, s.srv.Shutdown(shutdownCtx))
		}
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

// fileLogin returns a function that reads Claude's login from file, a line
// CLAUDE_CODE_OAUTH_TOKEN=…, at every wake, so a new login needs no
// restart: "" while there is none.
func fileLogin(file string) func(ctx context.Context) (string, error) {
	return func(context.Context) (string, error) {
		data, err := os.ReadFile(file)
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		for line := range strings.Lines(string(data)) {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "CLAUDE_CODE_OAUTH_TOKEN="); ok {
				return v, nil
			}
		}
		return "", nil
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
