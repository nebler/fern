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

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/proxy"
	"github.com/nebler/fern/internal/publication"
	"github.com/nebler/fern/internal/registry"
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/taskpublicationcoord"
	"github.com/nebler/fern/internal/watch"
	"github.com/nebler/fern/internal/workspace"
	"golang.org/x/sync/errgroup"
)

func runUp(args []string, log *slog.Logger) error {
	fs := newFlagSet("up", "Run the workspace supervisor and authenticated proxy.")
	configPath := fs.String("config", "fern.yaml", "configuration file")
	envPath := fs.String("env-file", "", "protected environment file")
	name := fs.String("name", "", "workspace name")
	image := fs.String("image", "", "workspace image")
	repo := fs.String("repo", "", "host repository path")
	memory := fs.String("memory", "", "memory limit (for example 8Gi)")
	idle := fs.String("idle", "", "idle duration before stopping")
	listenAddress := fs.String("listen", "", "remote/device proxy listen address")
	operatorListenAddress := fs.String("operator-listen", "", "host/operator proxy listen address")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	values := map[string]string(nil)
	if *envPath != "" {
		values, err = readEnvFile(*envPath)
		if err != nil {
			return err
		}
	}
	cfg, err := config.LoadWithEnvironment(*configPath, cwd, flagSet(fs, "config"), config.Overrides{
		Name: optionalFlag(fs, "name", name), Image: optionalFlag(fs, "image", image),
		Repo: optionalFlag(fs, "repo", repo), Memory: optionalFlag(fs, "memory", memory),
		IdleAfter: optionalFlag(fs, "idle", idle), Listen: optionalFlag(fs, "listen", listenAddress),
		OperatorListen: optionalFlag(fs, "operator-listen", operatorListenAddress),
	}, values)
	if err != nil {
		return err
	}
	if values != nil {
		cfg.Workspace.Env = mergeWorkspaceEnvironment(cfg.Workspace.Env, values)
	}
	cfg.Workspace.Env = forwardedEnvironment(cfg.Workspace.Env)
	if err := config.Validate(cfg); err != nil {
		return err
	}
	memoryBytes, err := config.ParseMemoryBytes(cfg.Workspace.Memory)
	if err != nil {
		return err
	}
	spec := runtime.Spec{
		Name: cfg.Workspace.Name, Image: cfg.Workspace.Image, RepoPath: cfg.Workspace.Repo,
		MemoryBytes: memoryBytes, Env: cfg.Workspace.Env,
		WorkspaceGH: cfg.Workspace.GitHub != nil && cfg.Workspace.GitHub.Mode == config.GitHubModeWorkspaceGH,
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

	rootCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	group, serviceCtx := errgroup.WithContext(rootCtx)
	lockDir, err := statePath("locks")
	if err != nil {
		return err
	}
	lease, err := registry.Acquire(lockDir, spec.Name)
	if err != nil {
		return err
	}
	defer lease.Release()
	controlDir, err := statePath("control")
	if err != nil {
		return err
	}
	publisher, err := newWorkspacePublisher(cfg.Workspace)
	if err != nil {
		return err
	}
	docker, err := newDocker(log)
	if err != nil {
		return err
	}
	managerStarted, managerClosed := false, false
	defer func() {
		// A timed-out Manager.Close means manager-owned wake or rollback work may
		// still be using Docker. Leaking the client until process exit is safer
		// than racing those goroutines with client shutdown.
		if !managerStarted || managerClosed {
			docker.Close()
		}
	}()
	controlStore, err := control.Open(controlDir, spec.Name)
	if err != nil {
		return err
	}

	observations := make(chan watch.Observation, 64)
	streamController := watch.NewStreamController(serviceCtx, watch.StreamOptions{Auth: auth}, observations, log)
	manager := workspace.NewManager(
		serviceCtx,
		docker,
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
		func() {
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
		},
	)
	managerStarted = true
	closeManager := func(ctx context.Context) error {
		err := manager.Close(ctx)
		if err == nil {
			managerClosed = true
		}
		return err
	}
	start := time.Now()
	err = manager.ReconcileStartup(serviceCtx)
	if err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		return errors.Join(err, closeManager(closeCtx))
	}
	onboarding, err := newGitHubOnboarding(cfg)
	if err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		return errors.Join(err, closeManager(closeCtx))
	}
	tasks, err := newTaskServices(serviceCtx, cfg, docker, manager, auth, log)
	if errors.Is(err, githubapp.ErrCredentialsNotFound) && onboarding != nil {
		log.Warn("durable tasks await GitHub App onboarding and a Fern restart", "workspace", cfg.Workspace.Name)
		tasks, err = nil, nil
	}
	if err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		return errors.Join(err, closeManager(closeCtx))
	}
	if tasks != nil {
		defer tasks.store.Close()
	}

	supervisor := &watch.Supervisor{IdleAfter: cfg.IdleAfter, OnPause: manager.Pause, Log: log}
	connections := newConnectionTracker()
	controls := proxy.Controls{
		Store: controlStore, Onboarding: onboarding, ControlAuth: proxy.ControlAuth{Password: cfg.Control.Password},
	}
	if tasks != nil {
		controls.Tasks = tasks.handler
	}
	var publicationCoordinator *publication.Coordinator
	if publisher != nil {
		publicationCoordinator = publication.NewCoordinator(serviceCtx, controlStore, manager, publisher)
		controls.Publications = publicationCoordinator
	}
	closeStartup := func(startupErr error) error {
		var publicationErr error
		if publicationCoordinator != nil {
			publicationCtx, publicationCancel := context.WithTimeout(context.Background(), 5*time.Second)
			publicationErr = publicationCoordinator.Close(publicationCtx)
			publicationCancel()
		}
		managerCtx, managerCancel := context.WithTimeout(context.Background(), 10*time.Second)
		managerErr := closeManager(managerCtx)
		managerCancel()
		return errors.Join(startupErr, publicationErr, managerErr)
	}
	origins := trustedProxyOrigins(cfg)
	handlers, err := proxy.NewHandlers(manager, auth, controls, origins, log)
	if err != nil {
		return closeStartup(err)
	}
	remoteServer := &http.Server{
		Handler: handlers.Remote, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return serviceCtx },
	}
	operatorServer := &http.Server{
		Handler: handlers.Operator, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return serviceCtx },
	}
	if publicationCoordinator != nil {
		if err := publicationCoordinator.Reconcile(); err != nil {
			return closeStartup(err)
		}
	}
	if tasks != nil && tasks.publication != nil {
		for {
			err := tasks.publication.RunOnce(serviceCtx)
			if err == nil {
				continue
			}
			if errors.Is(err, taskpublicationcoord.ErrNoWork) || errors.Is(err, taskpublicationcoord.ErrReconciliationPending) {
				break
			}
			return closeStartup(err)
		}
	}
	group.Go(func() error {
		err := supervisor.Run(serviceCtx, observations)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	})
	if tasks != nil {
		group.Go(func() error {
			err := runTaskResultCoordinator(serviceCtx, tasks, log, cfg.Workspace.Name)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		})
		group.Go(func() error {
			err := tasks.coordinator.Run(serviceCtx)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		})
		group.Go(func() error {
			err := tasks.execution.Run(serviceCtx)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		})
		if tasks.publication != nil {
			group.Go(func() error {
				err := tasks.publication.Run(serviceCtx)
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			})
		}
		if tasks.verification != nil {
			group.Go(func() error {
				err := tasks.verification.Run(serviceCtx)
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			})
		}
	}
	for _, serving := range []struct {
		server   *http.Server
		listener net.Listener
	}{{remoteServer, remoteListener}, {operatorServer, operatorListener}} {
		serving := serving
		group.Go(func() error {
			err := serving.server.Serve(connections.wrap(serving.listener))
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
		shutdownErr := errors.Join(remoteServer.Shutdown(shutdownCtx), operatorServer.Shutdown(shutdownCtx))
		if shutdownErr != nil {
			log.Warn("graceful proxy shutdown timed out; forcing connections closed", "err", shutdownErr)
			connections.closeAll()
			return errors.Join(shutdownErr, remoteServer.Close(), operatorServer.Close())
		}
		connections.closeAll()
		return nil
	})

	fmt.Printf("workspace: %s\nremote: %s\noperator: %s\nready in: %s\nattach: fern attach\n", spec.Name, origins.Remote, origins.Operator, time.Since(start).Round(time.Millisecond))
	log.Info("proxy listeners ready", "remote_listener", remoteListener.Addr(), "remote_origin", origins.Remote, "operator", origins.Operator, "workspace", spec.Name)
	err = group.Wait()
	var publicationErr error
	if publicationCoordinator != nil {
		publicationCtx, publicationCancel := context.WithTimeout(context.Background(), 5*time.Second)
		publicationErr = publicationCoordinator.Close(publicationCtx)
		publicationCancel()
	}
	var prepareErr error
	if rootCtx.Err() != nil {
		prepareCtx, prepareCancel := context.WithTimeout(context.Background(), 5*time.Second)
		prepareErr = manager.PrepareShutdown(prepareCtx)
		prepareCancel()
	}
	// The manager owns wake and rollback goroutines. Do not close Docker until
	// that ownership has been handed back, even if shutdown takes longer.
	managerCtx, managerCancel := context.WithTimeout(context.Background(), 10*time.Second)
	managerErr := closeManager(managerCtx)
	managerCancel()
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	streamErr := streamController.Stop(stopCtx)
	cancel()
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return errors.Join(err, publicationErr, prepareErr, managerErr, streamErr)
}

func newWorkspacePublisher(workspace config.Workspace) (*publication.Publisher, error) {
	if workspace.GitHub == nil {
		return nil, nil
	}
	// Explicit GitHub authority modes supersede the legacy host-user gh lane.
	// Workspace gh runs inside the container; App credentials stay in the task
	// broker. Neither mode may accidentally consume the host user's credential.
	if workspace.GitHub.Mode == config.GitHubModeWorkspaceGH || workspace.GitHub.Mode == config.GitHubModeGitHubAppBroker {
		return nil, nil
	}
	return nil, fmt.Errorf("unsupported GitHub authority mode %q", workspace.GitHub.Mode)
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
