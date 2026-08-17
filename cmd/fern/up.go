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
	"github.com/nebler/fern/internal/proxy"
	"github.com/nebler/fern/internal/publication"
	"github.com/nebler/fern/internal/registry"
	"github.com/nebler/fern/internal/runtime"
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
	listenAddress := fs.String("listen", "", "proxy listen address")
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
	}, values)
	if err != nil {
		return err
	}
	if values != nil {
		cfg.Workspace.Env = mergeEnvironment(cfg.Workspace.Env, values)
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
	}
	auth := spec.ServerAuth()

	// Bind before any Docker side effect so an invalid or occupied address is
	// a side-effect-free startup failure.
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	defer listener.Close()

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
	publisher, err := publication.New(spec.Name, spec.RepoPath)
	if err != nil {
		return err
	}
	docker, err := newDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
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
	start := time.Now()
	_, err = manager.EnsureRunning(serviceCtx)
	if err != nil {
		return errors.Join(err, manager.Close(context.Background()))
	}

	supervisor := &watch.Supervisor{IdleAfter: cfg.IdleAfter, OnPause: manager.Pause, Log: log}
	connections := newConnectionTracker()
	trackedListener := connections.wrap(listener)
	controls := proxy.Controls{
		Store: controlStore, Fencer: manager, Publisher: publisher, ServiceContext: serviceCtx,
	}
	server := &http.Server{
		Handler: proxy.NewWithControls(manager, auth, controls, log), ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return serviceCtx },
	}
	proxy.ReconcilePublications(serviceCtx, controls, log)
	group.Go(func() error {
		err := supervisor.Run(serviceCtx, observations)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	})
	group.Go(func() error {
		err := server.Serve(trackedListener)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	})
	group.Go(func() error {
		<-serviceCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Warn("graceful proxy shutdown timed out; forcing connections closed", "err", err)
			connections.closeAll()
			return server.Close()
		}
		connections.closeAll()
		return nil
	})

	fmt.Printf("workspace: %s\nproxy: http://%s\nready in: %s\nattach: fern attach\n", spec.Name, listener.Addr(), time.Since(start).Round(time.Millisecond))
	log.Info("proxy listening", "address", listener.Addr(), "workspace", spec.Name)
	err = group.Wait()
	var prepareErr error
	if rootCtx.Err() != nil {
		prepareErr = manager.PrepareShutdown(context.Background())
	}
	// The manager owns wake and rollback goroutines. Do not close Docker until
	// that ownership has been handed back, even if shutdown takes longer.
	managerErr := manager.Close(context.Background())
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	streamErr := streamController.Stop(stopCtx)
	cancel()
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return errors.Join(err, prepareErr, managerErr, streamErr)
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
