// Command fern-daemon runs the node daemon and CSI plugin. The fern CLI
// also starts it with "fern daemon".
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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	sandboxv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/api/server"
	"github.com/nebler/fern/internal/csi"
	"github.com/nebler/fern/internal/driver"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
)

// Where the daemon finds the pool and serves the CSI plugin; deploy/fern.yaml
// mounts both.
const (
	socket    = "/run/fern/fern.sock"
	pool      = "/var/lib/fern"
	csiSocket = "/csi/csi.sock"
	// boxImage is what lab/build.sh builds and imports into every node.
	boxImage = "fern-box:dev"
	// runscState is runsc's state directory on the node, with --boxes=runsc,
	// and runscBundles the boxes' bundles: both on the node's tmpfs.
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

// runDaemon serves the API and the CSI plugin until ctx is cancelled, and
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
	var idle actor.Idle
	flags.DurationVar(&idle.Agent, "idle-agent", 5*time.Minute, "sleep this long after a box's last command ended (0: never)")
	flags.DurationVar(&idle.Human, "idle-human", 20*time.Minute, "… and this long after it started (0: never)")
	flags.DurationVar(&idle.Prewake, "idle-prewake", 2*time.Minute, "sleep a box woken ahead this long after its wake, unless a command comes (0: no pre-wakes)")
	metricsAddr := flags.String("metrics-addr", ":9090", "serve Prometheus metrics at /metrics, and Go's profiles at /debug/pprof/, on this address")
	traceEndpoint := flags.String("trace-endpoint", "", "send traces by OTLP/HTTP to this URL, such as http://10.0.0.1:4318 (\"\": none)")
	runtimeClass := flags.String("runtime-class", "", "run boxes under this RuntimeClass, such as gvisor (\"\": the node's default)")
	boxes := flags.String("boxes", "kubernetes", "who runs boxes: kubernetes (a Sandbox each) or runsc (the daemon itself; needs the node's PID and network namespaces)")
	boxDNS := flags.String("box-dns", "1.1.1.1,8.8.8.8", "with --boxes=runsc, the resolvers boxes use")
	agentBoxes := flags.Bool("agent-boxes", false, "with --boxes=runsc, run Claude in a box of its own per environment, whose Bash runs in the environment's box (docs/briefs/claude-outside-the-box.md)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	node := os.Getenv("NODE_NAME")
	if node == "" {
		return errors.New("NODE_NAME is not set")
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
	kube, err := kubeClient()
	if err != nil {
		return err
	}
	// T3's server runs as the box user, and uses the API too.
	apiLis, err := listenUnix(ctx, socket, store.BoxGID)
	if err != nil {
		return err
	}
	csiLis, err := listenUnix(ctx, csiSocket, -1)
	if err != nil {
		return err
	}
	metricsLis, err := new(net.ListenConfig).Listen(ctx, "tcp", *metricsAddr)
	if err != nil {
		return err
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

	g, ctx := errgroup.WithContext(ctx)
	var drv boxDriver
	image := boxImage
	switch *boxes {
	case "kubernetes":
		drv = driver.New(kube, node, boxImage, *runtimeClass, tr)
	case "runsc":
		r := runsc.New(runsc.NodeHost, runsc.Config{
			Pool: pool, Bundles: runscBundles, State: runscState, Image: "docker.io/library/" + boxImage,
			DNS: strings.Split(*boxDNS, ","), Login: claudeLogin(kube), Agents: *agentBoxes, MCPPort: mcpPort,
		}, tr)
		// Golden keys name the image's content, not its tag.
		if image, err = r.Prepare(ctx); err != nil {
			return fmt.Errorf("prepare the node for boxes: %w", err)
		}
		drv = r
	default:
		return fmt.Errorf("--boxes=%s: want kubernetes or runsc", *boxes)
	}
	agentMCP := ""
	var mcpLis net.Listener
	if *agentBoxes {
		if *boxes != "runsc" {
			return errors.New("--agent-boxes needs --boxes=runsc")
		}
		// On every address: the boxes' firewall lets only agent boxes in.
		if mcpLis, err = new(net.ListenConfig).Listen(ctx, "tcp", ":"+strconv.Itoa(mcpPort)); err != nil {
			return err
		}
		agentMCP = "http://" + runsc.DaemonHost + ":" + strconv.Itoa(mcpPort)
	}
	envs := actor.New(st, drv, idle, image, tr, log)
	apiSrv := server.New(st, drv, envs, agentMCP, tr, log)
	httpSrv := &http.Server{
		Handler:           apiSrv.Handler(),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 10 * time.Second,
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(envs, drv, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(prometheus.Gatherers{reg, restMetrics{}}, promhttp.HandlerOpts{}))
	// Profiles cost nothing until asked for. The port is the pod's, which
	// boxes can't reach: their network policy blocks private addresses.
	metricsMux.HandleFunc("/debug/pprof/", pprof.Index)
	metricsMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	metricsMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	metricsMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	metricsMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	metricsSrv := &http.Server{
		Handler:           metricsMux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	grpcSrv := grpc.NewServer()
	csi.New(node, st.Get, log).Register(grpcSrv)
	log.InfoContext(ctx, "fern daemon started", slog.String("node", node))
	// Changes outlive their requests: Run returns once they have ended, and
	// the store stays open until then.
	g.Go(func() error { return envs.Run(ctx) })
	g.Go(func() error {
		if err := httpSrv.Serve(apiLis); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("api: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		if err := metricsSrv.Serve(metricsLis); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("metrics: %w", err)
		}
		return nil
	})
	mcpSrv := &http.Server{Handler: apiSrv.MCPHandler(), BaseContext: func(net.Listener) context.Context { return ctx }, ReadHeaderTimeout: 10 * time.Second}
	if mcpLis != nil {
		g.Go(func() error {
			if err := mcpSrv.Serve(mcpLis); !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("mcp: %w", err)
			}
			return nil
		})
	}
	g.Go(func() error {
		if err := grpcSrv.Serve(csiLis); err != nil {
			return fmt.Errorf("csi: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		grpcSrv.GracefulStop()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutdownCtx)
		_ = mcpSrv.Shutdown(shutdownCtx)
		return httpSrv.Shutdown(shutdownCtx)
	})
	return g.Wait()
}

// boxDriver runs boxes, and reports its steps as metrics.
type boxDriver interface {
	actor.Driver
	prometheus.Collector
}

// claudeLogin returns a function that reads Claude's login from its Secret
// at every wake, as kubelet does for a pod: "" while there is none.
func claudeLogin(kube client.Client) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		var s corev1.Secret
		err := kube.Get(ctx, client.ObjectKey{Namespace: csi.BoxNamespace, Name: "claude-login"}, &s)
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return string(s.Data["token"]), nil
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

// restMetrics gathers client-go's counts and times of Kubernetes API
// requests (rest_client_*), which controller-runtime keeps in a registry of
// its own: a change's requests are a cost no busier machine changes.
type restMetrics struct{}

func (restMetrics) Gather() ([]*dto.MetricFamily, error) {
	families, err := crmetrics.Registry.Gather()
	return slices.DeleteFunc(families, func(f *dto.MetricFamily) bool {
		return !strings.HasPrefix(f.GetName(), "rest_client_")
	}), err
}

// tracing is a TracerProvider whose Shutdown sends the spans it holds.
type tracing interface {
	trace.TracerProvider
	Shutdown(ctx context.Context) error
}

// noopProvider is a provider that records nothing, with nothing to send.
type noopProvider struct{ noop.TracerProvider }

func (noopProvider) Shutdown(context.Context) error { return nil }

func kubeClient() (client.WithWatch, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes config: %w", err)
	}
	// client-go's default of 5 requests a second made waking 10 boxes at
	// once take 6.5s: every wake polls its Sandbox and pod.
	cfg.QPS, cfg.Burst = 50, 100
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := sandboxv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return c, nil
}

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
