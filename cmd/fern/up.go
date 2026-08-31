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
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/watch"
	"github.com/nebler/fern/internal/workspace"
	"golang.org/x/sync/errgroup"
)

// observationBufferSize deep-queues workspace activity observations so the
// manager can request immediate attention without blocking on the supervisor.
const observationBufferSize = 64

// managerCloseTimeout bounds each manager close attempt; manager-owned wake and
// rollback goroutines get this long to hand back before resources are leaked.
const managerCloseTimeout = 10 * time.Second

func runUp(args []string, log *slog.Logger) (resultErr error) {
	opts, err := parseUpFlags(args)
	if err != nil {
		return err
	}
	cfg, spec, err := loadUpSpec(opts)
	if err != nil {
		return err
	}
	auth := spec.ServerAuth()

	// Bind before any Docker side effect so an invalid or occupied address is
	// a side-effect-free startup failure.
	remoteListener, operatorListener, err := listenProxySurfaces(cfg.Listen, cfg.OperatorListen)
	if err != nil {
		return err
	}
	defer remoteListener.Close()
	defer operatorListener.Close()
	backgroundListener, err := listenBackgroundRoute(cfg)
	if err != nil {
		return err
	}
	if backgroundListener != nil {
		defer backgroundListener.Close()
	}

	rootCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	group, serviceCtx := errgroup.WithContext(rootCtx)
	lease, err := acquireWorkspaceLease(spec.Name)
	if err != nil {
		return err
	}
	defer lease.Release()

	rt, err := assembleServices(serviceCtx, cfg, spec, auth, trustedProxyOrigins(cfg), remoteListener, operatorListener, backgroundListener, log)
	if err != nil {
		return err
	}
	// Deferred releases execute in the historical teardown order: task store,
	// guarded Docker close, lease, listeners, signal restoration.
	defer rt.lifecycle.release()
	if rt.tasks != nil {
		defer func() { resultErr = errors.Join(resultErr, rt.tasks.Close()) }()
	}
	if rt.backgroundRoute != nil {
		defer func() { resultErr = errors.Join(resultErr, rt.backgroundRoute.Close()) }()
	}

	startSupervisor(group, rt, serviceCtx)
	if rt.tasks != nil {
		startTaskCoordinators(group, rt.tasks, serviceCtx, log, spec.Name)
	}
	startProxyServers(group, rt, serviceCtx, log)

	fmt.Printf("workspace: %s\nremote: %s\noperator: %s\nidle: %s after %s\nready in: %s\nattach: fern attach\n",
		spec.Name, rt.origins.Remote, rt.origins.Operator, cfg.IdleMode, cfg.IdleAfter, time.Since(rt.start).Round(time.Millisecond))
	log.Info("proxy listeners ready", "remote_listener", remoteListener.Addr(), "remote_origin", rt.origins.Remote,
		"operator", rt.origins.Operator, "workspace", spec.Name, "idle_mode", cfg.IdleMode)
	return awaitShutdown(group, rt, rootCtx)
}

type upOptions struct {
	configPath     string
	envPath        string
	configRequired bool
	overrides      config.Overrides
}

func parseUpFlags(args []string) (upOptions, error) {
	fs := newFlagSet("up", "Run the workspace supervisor and authenticated proxy.")
	configPath := fs.String("config", "fern.yaml", "configuration file")
	envPath := fs.String("env-file", "", "protected environment file")
	name := fs.String("name", "", "workspace name")
	image := fs.String("image", "", "workspace image")
	repo := fs.String("repo", "", "host repository path")
	memory := fs.String("memory", "", "memory limit (for example 8Gi)")
	idle := fs.String("idle", "", "idle duration before stopping")
	idleMode := fs.String("idle-mode", "", "idle suspension mechanism: stop or freeze")
	listenAddress := fs.String("listen", "", "remote/device proxy listen address")
	operatorListenAddress := fs.String("operator-listen", "", "host/operator proxy listen address")
	if err := parseFlags(fs, args); err != nil {
		return upOptions{}, err
	}
	return upOptions{
		configPath:     *configPath,
		envPath:        *envPath,
		configRequired: flagProvided(fs, "config"),
		overrides: config.Overrides{
			Name: optionalFlag(fs, "name", name), Image: optionalFlag(fs, "image", image),
			Repo: optionalFlag(fs, "repo", repo), Memory: optionalFlag(fs, "memory", memory),
			IdleAfter: optionalFlag(fs, "idle", idle), IdleMode: optionalFlag(fs, "idle-mode", idleMode),
			Listen:         optionalFlag(fs, "listen", listenAddress),
			OperatorListen: optionalFlag(fs, "operator-listen", operatorListenAddress),
		},
	}, nil
}

// loadUpSpec loads configuration through the shared command preamble, validates
// it, and derives the Docker runtime specification from the workspace section.
func loadUpSpec(opts upOptions) (config.Config, runtime.Spec, error) {
	cfg, _, err := loadCommandConfig(opts.configPath, opts.configRequired, opts.envPath, opts.overrides)
	if err != nil {
		return config.Config{}, runtime.Spec{}, err
	}
	if err := config.Validate(cfg); err != nil {
		return config.Config{}, runtime.Spec{}, err
	}
	memoryBytes, err := config.ParseMemoryBytes(cfg.Workspace.Memory)
	if err != nil {
		return config.Config{}, runtime.Spec{}, err
	}
	spec := runtime.Spec{
		Name: cfg.Workspace.Name, Image: cfg.Workspace.Image, RepoPath: cfg.Workspace.Repo,
		MemoryBytes: memoryBytes, Env: cfg.Workspace.Env,
		WorkspaceGH: cfg.Workspace.GitHub != nil && cfg.Workspace.GitHub.Mode == config.GitHubModeWorkspaceGH,
	}
	return cfg, spec, nil
}

// dockerLifecycle pairs the Docker client with manager shutdown bookkeeping.
type dockerLifecycle struct {
	docker         *runtime.Docker
	manager        managerLifecycle
	managerStarted bool
	managerClosed  bool
}

type managerLifecycle interface {
	Close(context.Context) error
	PrepareShutdown(context.Context) error
}

type streamStopper interface {
	Stop(context.Context) error
}

func newDockerLifecycle(log *slog.Logger, suspend runtime.SuspendKind) (*dockerLifecycle, error) {
	docker, err := newDockerWithSuspend(log, suspend)
	if err != nil {
		return nil, err
	}
	return &dockerLifecycle{docker: docker}, nil
}

func (l *dockerLifecycle) closeManager(ctx context.Context) error {
	err := l.manager.Close(ctx)
	if err == nil {
		l.managerClosed = true
	}
	return err
}

// release closes the Docker client unless a timed-out manager close means
// manager-owned wake or rollback work may still be using it. Leaking the client
// until process exit is safer than racing those goroutines with shutdown.
func (l *dockerLifecycle) release() {
	if !l.managerStarted || l.managerClosed {
		l.docker.Close()
	}
}

// failStartup joins a pre-serving startup error with the bounded manager close
// and then applies the release rule, mirroring the deferred teardown that the
// inline implementation performed for every failure after manager creation.
func (l *dockerLifecycle) failStartup(startupErr error) error {
	managerErr := runWithTimeout(managerCloseTimeout, l.closeManager)
	l.release()
	return errors.Join(startupErr, managerErr)
}

// runWithTimeout executes one shutdown step against a fresh background deadline
// so steps remain bounded even though the signal context is already canceled.
func runWithTimeout(timeout time.Duration, operation func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return operation(ctx)
}

// upRuntime bundles what 'fern up' keeps alive until shutdown. Assembly cleans
// up after its own failures; runUp defers releases for a successful return.
type upRuntime struct {
	lifecycle        *dockerLifecycle
	streamController streamStopper
	supervisor       *watch.Supervisor
	observations     chan watch.Observation
	connections      *connectionTracker
	remoteServer     *http.Server
	operatorServer   *http.Server
	remoteListener   net.Listener
	operatorListener net.Listener
	backgroundRoute  *backgroundroute.Manager
	origins          proxy.TrustedOrigins
	tasks            *taskServices
	status           *observability.Registry
	start            time.Time
}

// assembleServices builds every coordinator, handler, and HTTP server the
// supervisor needs before it starts serving, performing the same partial
// teardown the inline version performed on failure.
func assembleServices(serviceCtx context.Context, cfg config.Config, spec runtime.Spec, auth runtime.ServerAuth, origins proxy.TrustedOrigins, remoteListener, operatorListener, backgroundListener net.Listener, log *slog.Logger) (*upRuntime, error) {
	controlDir, err := statePath("control")
	if err != nil {
		return nil, err
	}
	lifecycle, err := newDockerLifecycle(log, runtime.SuspendKind(cfg.IdleMode))
	if err != nil {
		return nil, err
	}
	controlStore, err := control.Open(controlDir, spec.Name)
	if err != nil {
		lifecycle.release()
		return nil, err
	}
	var backgroundRoute *backgroundroute.Manager
	if cfg.Tasks != nil && cfg.Tasks.BackgroundRoute != nil {
		backgroundRoute, err = backgroundroute.New(backgroundListener, cfg.Tasks.BackgroundRoute.Origin, controlStore)
		if err != nil {
			lifecycle.release()
			return nil, err
		}
	}
	failStartup := func(startupErr error) error {
		if backgroundRoute != nil {
			startupErr = errors.Join(startupErr, backgroundRoute.Close())
		}
		return lifecycle.failStartup(startupErr)
	}
	pluginAuthStore, err := openPluginAuthorizationStore(controlStore, spec.Name)
	if err != nil {
		if backgroundRoute != nil {
			err = errors.Join(err, backgroundRoute.Close())
		}
		lifecycle.release()
		return nil, err
	}
	observations := make(chan watch.Observation, observationBufferSize)
	status := observability.NewRegistry()
	updateLegacyPublicationReadiness(status, controlStore)
	streamController := watch.NewStreamController(serviceCtx, watch.StreamOptions{Auth: auth}, observations, log)
	manager := workspace.NewManager(
		serviceCtx,
		lifecycle.docker,
		spec,
		func(ctx context.Context, ep runtime.Endpoint, force bool) error {
			if force {
				return streamController.ReconnectEndpoint(ctx, ep)
			}
			return streamController.ConnectEndpoint(ctx, ep)
		},
		func(ctx context.Context, ep runtime.Endpoint) (bool, error) {
			return watch.AllSessionsIdle(ctx, ep, auth)
		},
		requestObservation(serviceCtx, observations),
	)
	lifecycle.manager, lifecycle.managerStarted = manager, true
	start := time.Now()
	if err := manager.ReconcileStartup(serviceCtx); err != nil {
		return nil, failStartup(err)
	}
	status.Healthy(observability.ComponentRuntime)
	status.Healthy(observability.ComponentSupervisor)
	observedManager := &statusWaker{Waker: manager, status: status}
	onboarding, err := newGitHubOnboarding(cfg)
	if err != nil {
		return nil, failStartup(err)
	}
	tasks, err := newTaskServices(serviceCtx, cfg, lifecycle.docker, manager, backgroundRoute, auth, status, log)
	if errors.Is(err, githubapp.ErrCredentialsNotFound) && onboarding != nil {
		log.Warn("durable tasks await GitHub App onboarding and a Fern restart", "workspace", cfg.Workspace.Name)
		tasks, err = nil, nil
	}
	if err != nil {
		return nil, failStartup(err)
	}
	// abortAfterTasks mirrors the historical post-task failure unwind, which
	// closed the bounded manager first, then the task store, then applied the
	// Docker release rule.
	abortAfterTasks := func(startupErr error) error {
		managerErr := runWithTimeout(managerCloseTimeout, lifecycle.closeManager)
		if tasks != nil {
			tasks.Close()
		}
		if backgroundRoute != nil {
			startupErr = errors.Join(startupErr, backgroundRoute.Close())
		}
		lifecycle.release()
		return errors.Join(startupErr, managerErr)
	}

	connections := newConnectionTracker()
	controls := proxy.Controls{
		Store: controlStore, Onboarding: onboarding, ControlAuth: proxy.ControlAuth{Password: cfg.Control.Password},
		PluginAuth: pluginAuthStore,
		WakeTrace:  proxy.NewWakeTraceHandler(manager, manager.LastWakeTrace, log),
		Liveness:   status.LivenessHandler(), Readiness: status.ReadinessHandler(),
		Status: status.StatusHandler(), Metrics: status.MetricsHandler(),
	}
	if tasks != nil {
		controls.Tasks = tasks.handler
		controls.Runs = tasks.runs
	}
	handlers, err := proxy.NewHandlers(observedManager, auth, controls, origins, log)
	if err != nil {
		return nil, abortAfterTasks(err)
	}
	remoteServer := &http.Server{
		Handler: handlers.Remote, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return serviceCtx },
	}
	operatorServer := &http.Server{
		Handler: handlers.Operator, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return serviceCtx },
	}
	return &upRuntime{
		lifecycle: lifecycle, streamController: streamController,
		supervisor: &watch.Supervisor{IdleAfter: cfg.IdleAfter, OnPause: func(ctx context.Context) error {
			err := manager.Pause(ctx)
			if err != nil {
				status.Degraded(observability.ComponentRuntime, err)
			} else {
				status.Healthy(observability.ComponentRuntime)
			}
			return err
		}, Log: log},
		observations: observations, connections: connections,
		remoteServer: remoteServer, operatorServer: operatorServer,
		remoteListener: remoteListener, operatorListener: operatorListener,
		backgroundRoute: backgroundRoute, origins: origins, tasks: tasks, status: status, start: start,
	}, nil
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

// statusWaker observes lifecycle outcomes already required by admitted
// requests. It never performs an extra runtime query, so sleeping remains a
// healthy state and health reporting cannot wake compute.
type statusWaker struct {
	proxy.Waker
	status *observability.Registry
}

func (waker *statusWaker) AcquireRequest(ctx context.Context, intent workspace.RequestIntent) (workspace.RequestTarget, func(), error) {
	target, release, err := waker.Waker.AcquireRequest(ctx, intent)
	if err == nil {
		waker.status.Healthy(observability.ComponentRuntime)
	} else if ctx.Err() == nil && !errors.Is(err, workspace.ErrManagerClosed) {
		waker.status.Degraded(observability.ComponentRuntime, err)
	}
	return target, release, err
}

// requestObservation asks the activity stream for an immediate observation pass
// and waits until the observation loop has handled the request or shutdown won.
func requestObservation(serviceCtx context.Context, observations chan<- watch.Observation) func() {
	return func() {
		handled := make(chan struct{})
		select {
		case observations <- watch.Observation{Kind: watch.ObservationRequest, Handled: handled}:
		case <-serviceCtx.Done():
			return
		}
		select {
		case <-handled:
		case <-serviceCtx.Done():
		}
	}
}

// goUntilCanceled runs service on the errgroup, treating context cancellation
// as a clean stop so shutdown does not surface spurious errors.
func goUntilCanceled(group *errgroup.Group, serviceCtx context.Context, service func(context.Context) error) {
	group.Go(func() error {
		err := service(serviceCtx)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	})
}

func startSupervisor(group *errgroup.Group, rt *upRuntime, serviceCtx context.Context) {
	goComponent(group, serviceCtx, rt.status, observability.ComponentSupervisor, func(ctx context.Context) error {
		return rt.supervisor.Run(ctx, rt.observations)
	})
}

func startTaskCoordinators(group *errgroup.Group, tasks *taskServices, serviceCtx context.Context, log *slog.Logger, workspaceName string) {
	goComponent(group, serviceCtx, tasks.status, observability.ComponentTaskResult, func(ctx context.Context) error {
		return runTaskResultCoordinator(ctx, tasks, log, workspaceName)
	})
	goComponent(group, serviceCtx, tasks.status, observability.ComponentTaskDelivery, tasks.coordinator.Run)
	goComponent(group, serviceCtx, tasks.status, observability.ComponentTaskExecution, tasks.execution.Run)
	if tasks.publication != nil {
		goComponent(group, serviceCtx, tasks.status, observability.ComponentTaskPublication, tasks.publication.Run)
	}
	if tasks.verification != nil {
		goComponent(group, serviceCtx, tasks.status, observability.ComponentTaskVerification, tasks.verification.Run)
	}
	if tasks.background != nil {
		goComponent(group, serviceCtx, tasks.status, observability.ComponentBackgroundRunSerial, tasks.background.Run)
	}
}

func goComponent(group *errgroup.Group, serviceCtx context.Context, status *observability.Registry, component observability.Component, service func(context.Context) error) {
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

// startProxyServers serves both proxies until serviceCtx is canceled, then
// attempts graceful shutdown with a forced-close fallback so hung clients
// cannot stall exit past the shutdown deadline.
func startProxyServers(group *errgroup.Group, rt *upRuntime, serviceCtx context.Context, log *slog.Logger) {
	if rt.backgroundRoute != nil {
		group.Go(func() error { return rt.backgroundRoute.Run(serviceCtx) })
	}
	for _, serving := range []struct {
		server   *http.Server
		listener net.Listener
	}{{rt.remoteServer, rt.remoteListener}, {rt.operatorServer, rt.operatorListener}} {
		group.Go(func() error {
			err := serving.server.Serve(rt.connections.wrap(serving.listener))
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
		shutdownErr := errors.Join(rt.remoteServer.Shutdown(shutdownCtx), rt.operatorServer.Shutdown(shutdownCtx))
		if shutdownErr != nil {
			log.Warn("graceful proxy shutdown timed out; forcing connections closed", "err", shutdownErr)
			rt.connections.closeAll()
			return errors.Join(shutdownErr, rt.remoteServer.Close(), rt.operatorServer.Close())
		}
		rt.connections.closeAll()
		return nil
	})
}

func listenBackgroundRoute(cfg config.Config) (net.Listener, error) {
	if cfg.Tasks == nil || cfg.Tasks.BackgroundRoute == nil {
		return nil, nil
	}
	listener, err := net.Listen("tcp", cfg.Tasks.BackgroundRoute.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen on background route %s: %w", cfg.Tasks.BackgroundRoute.Listen, err)
	}
	return listener, nil
}

// awaitShutdown waits for every service goroutine and then unwinds in the
// historical order: wake preparation when a signal ended the run, the manager
// close that hands wake and rollback ownership back, and the activity stream.
func awaitShutdown(group *errgroup.Group, rt *upRuntime, rootCtx context.Context) error {
	err := group.Wait()
	var prepareErr error
	if rootCtx.Err() != nil {
		prepareErr = runWithTimeout(5*time.Second, rt.lifecycle.manager.PrepareShutdown)
	}
	// The manager owns wake and rollback goroutines. Do not close Docker until
	// that ownership has been handed back, even if shutdown takes longer.
	managerErr := runWithTimeout(managerCloseTimeout, rt.lifecycle.closeManager)
	stopErr := runWithTimeout(5*time.Second, rt.streamController.Stop)
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return errors.Join(err, prepareErr, managerErr, stopErr)
}

func trustedProxyOrigins(cfg config.Config) proxy.TrustedOrigins {
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

func (t *connectionTracker) wrap(listener net.Listener) net.Listener {
	return &trackedListener{Listener: listener, tracker: t}
}

func (l *trackedListener) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tracked := &trackedConnection{Conn: connection, tracker: l.tracker}
	l.tracker.add(tracked)
	return tracked, nil
}

func (t *connectionTracker) add(connection net.Conn) {
	t.mu.Lock()
	t.conns[connection] = struct{}{}
	t.mu.Unlock()
}

func (t *connectionTracker) remove(connection net.Conn) {
	t.mu.Lock()
	delete(t.conns, connection)
	t.mu.Unlock()
}

func (c *trackedConnection) Close() error {
	err := c.Conn.Close()
	c.tracker.remove(c)
	return err
}

func (t *connectionTracker) closeAll() {
	t.mu.Lock()
	connections := make([]net.Conn, 0, len(t.conns))
	for connection := range t.conns {
		connections = append(connections, connection)
		delete(t.conns, connection)
	}
	t.mu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
}
