package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/nebler/fern/internal/backgroundroute"
	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/observability"
	"github.com/nebler/fern/internal/pluginauth"
	"github.com/nebler/fern/internal/proxy"
	"golang.org/x/sync/errgroup"
)

func runUp(args []string, log *slog.Logger) (resultErr error) {
	opts, err := parseUpFlags(args)
	if err != nil {
		return err
	}
	cfg, err := loadUpConfig(opts)
	if err != nil {
		return err
	}
	remoteListener, operatorListener, err := listenProxySurfaces(cfg.Listen, cfg.OperatorListen)
	if err != nil {
		return err
	}
	defer func() { _ = remoteListener.Close() }()
	defer func() { _ = operatorListener.Close() }()
	backgroundListener, err := listenBackgroundRoute(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = backgroundListener.Close() }()

	rootCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	group, serviceCtx := errgroup.WithContext(rootCtx)
	lease, err := acquireHostLease(cfg.Workspace.Name)
	if err != nil {
		return err
	}
	defer lease.Release()
	runtime, err := assembleServices(serviceCtx, cfg, trustedProxyOrigins(cfg), remoteListener, operatorListener, backgroundListener, log)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, runtime.Close()) }()
	startTaskCoordinators(group, runtime.tasks, serviceCtx)
	startProxyServers(group, runtime, serviceCtx, log)

	fmt.Printf("repository: %s\nremote: %s\noperator: %s\nbackground: %s\nready in: %s\n",
		cfg.Workspace.Repo, runtime.origins.Remote, runtime.origins.Operator, cfg.Tasks.BackgroundRoute.Origin,
		time.Since(runtime.start).Round(time.Millisecond))
	log.Info("Background Run control plane ready", "remote", runtime.origins.Remote, "operator", runtime.origins.Operator,
		"background", cfg.Tasks.BackgroundRoute.Origin, "repository", cfg.Workspace.Name)
	err = group.Wait()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

type upOptions struct {
	configPath     string
	envPath        string
	configRequired bool
	overrides      config.BackgroundOverrides
}

func parseUpFlags(args []string) (upOptions, error) {
	fs := newFlagSet("up", "Run the Background Run control plane.")
	configPath := fs.String("config", "fern.yaml", "configuration file")
	envPath := fs.String("env-file", "", "protected environment file")
	name := fs.String("name", "", "repository binding name")
	repo := fs.String("repo", "", "host repository path")
	listenAddress := fs.String("listen", "", "remote/device control-plane listen address")
	operatorListenAddress := fs.String("operator-listen", "", "host/operator listen address")
	if err := parseFlags(fs, args); err != nil {
		return upOptions{}, err
	}
	return upOptions{configPath: *configPath, envPath: *envPath, configRequired: flagProvided(fs, "config"),
		overrides: config.BackgroundOverrides{Name: optionalFlag(fs, "name", name), Repo: optionalFlag(fs, "repo", repo),
			Listen: optionalFlag(fs, "listen", listenAddress), OperatorListen: optionalFlag(fs, "operator-listen", operatorListenAddress)}}, nil
}

func loadUpConfig(opts upOptions) (config.BackgroundConfig, error) {
	cfg, err := loadBackgroundCommandConfig(opts.configPath, opts.configRequired, opts.envPath, opts.overrides)
	if err != nil {
		return config.BackgroundConfig{}, err
	}
	if err := config.ValidateBackgroundBootstrap(cfg); err != nil {
		return config.BackgroundConfig{}, err
	}
	return cfg, nil
}

type upRuntime struct {
	tasks            *taskServices
	backgroundRoute  *backgroundroute.Manager
	remoteServer     *http.Server
	operatorServer   *http.Server
	remoteListener   net.Listener
	operatorListener net.Listener
	connections      *connectionTracker
	origins          proxy.TrustedOrigins
	status           *observability.Registry
	start            time.Time
}

func (runtime *upRuntime) Close() error {
	var taskErr, routeErr error
	if runtime.tasks != nil {
		taskErr = runtime.tasks.Close()
	}
	if runtime.backgroundRoute != nil {
		routeErr = runtime.backgroundRoute.Close()
	}
	return errors.Join(taskErr, routeErr)
}

func assembleServices(serviceCtx context.Context, cfg config.BackgroundConfig, origins proxy.TrustedOrigins,
	remoteListener, operatorListener, backgroundListener net.Listener, log *slog.Logger) (*upRuntime, error) {
	controlDir, err := statePath("control")
	if err != nil {
		return nil, err
	}
	controlStore, err := control.Open(controlDir, cfg.Workspace.Name)
	if err != nil {
		return nil, err
	}
	pluginAuthStore, err := openPluginAuthorizationStore(controlStore, cfg.Workspace.Name)
	if err != nil {
		return nil, err
	}
	route, err := backgroundroute.New(backgroundListener, cfg.Tasks.BackgroundRoute.Origin)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*upRuntime, error) {
		return nil, errors.Join(cause, route.Close())
	}
	status := observability.NewRegistry()
	updateLegacyPublicationReadiness(status, controlStore)
	onboarding, err := newGitHubOnboarding(cfg)
	if err != nil {
		return fail(err)
	}
	var tasks *taskServices
	if cfg.Workspace.GitHub.InstallationID == 0 {
		pending := errors.New("GitHub App installation ID is not configured")
		status.Blocked(observability.ComponentGitHubTaskDependency, pending)
		log.Warn("Background Runs await GitHub App installation binding and restart", "repository", cfg.Workspace.Name)
	} else {
		if err := config.ValidateBackground(cfg); err != nil {
			return fail(err)
		}
		tasks, err = newTaskServices(serviceCtx, cfg, route, status, log)
		if errors.Is(err, githubapp.ErrCredentialsNotFound) && onboarding != nil {
			log.Warn("Background Runs await GitHub App onboarding and restart", "repository", cfg.Workspace.Name)
			status.Blocked(observability.ComponentGitHubTaskDependency, err)
			tasks, err = nil, nil
		}
	}
	if err != nil {
		return fail(err)
	}
	unavailable := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Background Runs await GitHub App onboarding", http.StatusServiceUnavailable)
	}))
	runs, runClients, results := unavailable, unavailable, unavailable
	if tasks != nil {
		runs = tasks.runs
		runClients = tasks.runClients
		results = tasks.results
	}
	controls := proxy.Controls{Store: controlStore, Runs: runs, RunClients: runClients, Results: results, Onboarding: onboarding,
		ControlAuth: proxy.ControlAuth{Password: cfg.Control.Password}, PluginAuth: pluginAuthStore,
		Liveness: status.LivenessHandler(), Readiness: status.ReadinessHandler(), Status: status.StatusHandler(), Metrics: status.MetricsHandler()}
	handlers, err := proxy.NewHandlers(controls, origins)
	if err != nil {
		if tasks != nil {
			_ = tasks.Close()
		}
		return fail(err)
	}
	connections := newConnectionTracker()
	return &upRuntime{tasks: tasks, backgroundRoute: route,
		remoteServer:   &http.Server{Handler: handlers.Remote, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return serviceCtx }},
		operatorServer: &http.Server{Handler: handlers.Operator, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return serviceCtx }},
		remoteListener: remoteListener, operatorListener: operatorListener, connections: connections,
		origins: origins, status: status, start: time.Now()}, nil
}

func openPluginAuthorizationStore(store *control.Store, workspace string) (*pluginauth.Store, error) {
	return pluginauth.Open(store, workspace)
}

func updateLegacyPublicationReadiness(status *observability.Registry, store *control.Store) {
	if store.HasUnquarantinedLegacyPublications() {
		status.Failed(observability.ComponentLegacyPublication, errors.New("legacy control publications require offline quarantine"))
		return
	}
	status.Healthy(observability.ComponentLegacyPublication)
}

func startTaskCoordinators(group *errgroup.Group, tasks *taskServices, serviceCtx context.Context) {
	if tasks == nil {
		return
	}
	goComponent(group, serviceCtx, tasks.status, observability.ComponentBackgroundRunSerial, tasks.background.Run)
	if tasks.publication != nil {
		goComponent(group, serviceCtx, tasks.status, observability.ComponentTaskPublication, tasks.publication.Run)
	}
	if tasks.verification != nil {
		goComponent(group, serviceCtx, tasks.status, observability.ComponentTaskVerification, tasks.verification.Run)
	}
}

func goComponent(group *errgroup.Group, serviceCtx context.Context, status *observability.Registry,
	component observability.Component, service func(context.Context) error) {
	group.Go(func() error {
		err := service(serviceCtx)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		if err != nil {
			status.Failed(component, err)
		}
		return err
	})
}

func startProxyServers(group *errgroup.Group, runtime *upRuntime, serviceCtx context.Context, log *slog.Logger) {
	group.Go(func() error { return runtime.backgroundRoute.Run(serviceCtx) })
	for _, serving := range []struct {
		server   *http.Server
		listener net.Listener
	}{{runtime.remoteServer, runtime.remoteListener}, {runtime.operatorServer, runtime.operatorListener}} {
		group.Go(func() error {
			err := serving.server.Serve(runtime.connections.wrap(serving.listener))
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		})
	}
	group.Go(func() error {
		<-serviceCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := errors.Join(runtime.remoteServer.Shutdown(shutdownCtx), runtime.operatorServer.Shutdown(shutdownCtx))
		if err != nil {
			log.Warn("graceful HTTP shutdown timed out", "err", err)
			return errors.Join(err, runtime.remoteServer.Close(), runtime.operatorServer.Close())
		}
		runtime.connections.closeAll()
		return nil
	})
}

func listenBackgroundRoute(cfg config.BackgroundConfig) (net.Listener, error) {
	listener, err := net.Listen("tcp", cfg.Tasks.BackgroundRoute.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen on Background Run route %s: %w", cfg.Tasks.BackgroundRoute.Listen, err)
	}
	return listener, nil
}

func trustedProxyOrigins(cfg config.BackgroundConfig) proxy.TrustedOrigins {
	remote := cfg.RemoteOrigin
	if remote == "" {
		remote = "http://" + cfg.Listen
	}
	return proxy.TrustedOrigins{Remote: remote, Operator: "http://" + cfg.OperatorListen}
}

func listenProxySurfaces(remoteAddress, operatorAddress string) (net.Listener, net.Listener, error) {
	remote, err := net.Listen("tcp", remoteAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on %s: %w", remoteAddress, err)
	}
	operator, err := net.Listen("tcp", operatorAddress)
	if err != nil {
		_ = remote.Close()
		return nil, nil, fmt.Errorf("listen on %s: %w", operatorAddress, err)
	}
	return remote, operator, nil
}

type connectionTracker struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

type trackedListener struct {
	net.Listener
	tracker *connectionTracker
}

type trackedConnection struct {
	net.Conn
	tracker *connectionTracker
}

func newConnectionTracker() *connectionTracker {
	return &connectionTracker{conns: make(map[net.Conn]struct{})}
}
func (tracker *connectionTracker) wrap(listener net.Listener) net.Listener {
	return &trackedListener{Listener: listener, tracker: tracker}
}
func (listener *trackedListener) Accept() (net.Conn, error) {
	connection, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tracked := &trackedConnection{Conn: connection, tracker: listener.tracker}
	listener.tracker.mu.Lock()
	listener.tracker.conns[tracked] = struct{}{}
	listener.tracker.mu.Unlock()
	return tracked, nil
}
func (connection *trackedConnection) Close() error {
	err := connection.Conn.Close()
	connection.tracker.mu.Lock()
	delete(connection.tracker.conns, connection)
	connection.tracker.mu.Unlock()
	return err
}
func (tracker *connectionTracker) closeAll() {
	tracker.mu.Lock()
	connections := make([]net.Conn, 0, len(tracker.conns))
	for connection := range tracker.conns {
		connections = append(connections, connection)
		delete(tracker.conns, connection)
	}
	tracker.mu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
}
