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
	"syscall"
	"time"

	"github.com/nebler/fern/internal/backgroundroute"
	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/observability"
	"github.com/nebler/fern/internal/pluginauth"
	"github.com/nebler/fern/internal/proxy"
	"github.com/nebler/fern/internal/taskstore"
	"golang.org/x/sync/errgroup"
)

func runUp(args []string, log *slog.Logger) (resultErr error) {
	fs := newFlagSet("up", "Run the Background Run control plane.")
	configPath, envPath := addConfigFlags(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadCommandConfig(*configPath, *envPath)
	if err != nil {
		return err
	}
	if err := config.ValidateBootstrap(cfg); err != nil {
		return err
	}
	var listeners [3]net.Listener
	for index, address := range []string{cfg.Proxy.Listen, cfg.Proxy.OperatorListen, cfg.Runs.BackgroundRoute.Listen} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", address, err)
		}
		defer func() { _ = listener.Close() }()
		listeners[index] = listener
	}
	remoteListener, operatorListener, backgroundListener := listeners[0], listeners[1], listeners[2]

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
		cfg.Workspace.Repo, runtime.origins.Remote, runtime.origins.Operator, cfg.Runs.BackgroundRoute.Origin,
		time.Since(runtime.start).Round(time.Millisecond))
	log.Info("Background Run control plane ready", "remote", runtime.origins.Remote, "operator", runtime.origins.Operator,
		"background", cfg.Runs.BackgroundRoute.Origin, "repository", cfg.Workspace.Name)
	err = group.Wait()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

type upRuntime struct {
	state            *taskstore.Store
	tasks            *taskServices
	backgroundRoute  *backgroundroute.Manager
	remoteServer     *http.Server
	operatorServer   *http.Server
	remoteListener   net.Listener
	operatorListener net.Listener
	origins          proxy.TrustedOrigins
	status           *observability.Registry
	start            time.Time
}

func (runtime *upRuntime) Close() error {
	var taskErr, routeErr, stateErr error
	// Fence attachment admission and close its connections before releasing
	// the provider and the durable store that back the runtime.
	if runtime.backgroundRoute != nil {
		routeErr = runtime.backgroundRoute.Close()
	}
	if runtime.tasks != nil {
		taskErr = runtime.tasks.Close()
	}
	if runtime.state != nil {
		stateErr = runtime.state.Close()
	}
	return errors.Join(taskErr, routeErr, stateErr)
}

func assembleServices(serviceCtx context.Context, cfg config.Config, origins proxy.TrustedOrigins,
	remoteListener, operatorListener, backgroundListener net.Listener, log *slog.Logger) (*upRuntime, error) {
	state, err := openStateStore(serviceCtx, cfg)
	if err != nil {
		return nil, err
	}
	route, err := backgroundroute.New(backgroundListener, cfg.Runs.BackgroundRoute.Origin)
	if err != nil {
		return nil, errors.Join(err, state.Close())
	}
	fail := func(cause error) (*upRuntime, error) {
		return nil, errors.Join(cause, route.Close(), state.Close())
	}
	status := observability.NewRegistry()
	var tasks *taskServices
	if cfg.Workspace.GitHub.InstallationID == 0 {
		pending := errors.New("GitHub App installation ID is not configured")
		status.Blocked(observability.ComponentGitHubTaskDependency, pending)
		log.Warn("Background Runs await GitHub App installation binding and restart", "repository", cfg.Workspace.Name)
	} else {
		if err := config.Validate(cfg); err != nil {
			return fail(err)
		}
		tasks, err = newTaskServices(serviceCtx, cfg, state, route, status, log)
		if errors.Is(err, githubapp.ErrCredentialsNotFound) {
			log.Warn("Background Runs await GitHub App credentials ('fern credentials set') and restart", "repository", cfg.Workspace.Name)
			status.Blocked(observability.ComponentGitHubTaskDependency, err)
			tasks, err = nil, nil
		}
	}
	if err != nil {
		return fail(err)
	}
	unavailable := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Background Runs await GitHub App setup", http.StatusServiceUnavailable)
	}))
	runs := unavailable
	if tasks != nil {
		runs = tasks.runs
	}
	controls := proxy.Controls{Store: control.New(state.DB()), Runs: runs,
		ControlAuth: proxy.ControlAuth{Password: cfg.ControlPassword}, PluginAuth: pluginauth.New(state.DB()),
		Liveness: status.LivenessHandler(), Readiness: status.ReadinessHandler()}
	handlers, err := proxy.NewHandlers(controls, origins)
	if err != nil {
		if tasks != nil {
			_ = tasks.Close()
		}
		return fail(err)
	}
	return &upRuntime{state: state, tasks: tasks, backgroundRoute: route,
		remoteServer:   &http.Server{Handler: handlers.Remote, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return serviceCtx }},
		operatorServer: &http.Server{Handler: handlers.Operator, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return serviceCtx }},
		remoteListener: remoteListener, operatorListener: operatorListener,
		origins: origins, status: status, start: time.Now()}, nil
}

// startTaskCoordinators runs the serial coordinator; a fatal failure marks it
// unready and, through the group, stops the other services.
func startTaskCoordinators(group *errgroup.Group, tasks *taskServices, serviceCtx context.Context) {
	if tasks == nil {
		return
	}
	group.Go(func() error {
		err := tasks.background.Run(serviceCtx)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		if err != nil {
			tasks.status.Failed(observability.ComponentBackgroundRunSerial, err)
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
			err := serving.server.Serve(serving.listener)
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		})
	}
	// Neither server hijacks connections (live attachment WebSockets are served
	// by the background route, which closes its own), so Shutdown and Close
	// account for every connection.
	group.Go(func() error {
		<-serviceCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := errors.Join(runtime.remoteServer.Shutdown(shutdownCtx), runtime.operatorServer.Shutdown(shutdownCtx))
		if err != nil {
			log.Warn("graceful HTTP shutdown timed out", "err", err)
			return errors.Join(err, runtime.remoteServer.Close(), runtime.operatorServer.Close())
		}
		return nil
	})
}

func trustedProxyOrigins(cfg config.Config) proxy.TrustedOrigins {
	remote := cfg.Proxy.RemoteOrigin
	if remote == "" {
		remote = "http://" + cfg.Proxy.Listen
	}
	return proxy.TrustedOrigins{Remote: remote, Operator: "http://" + cfg.Proxy.OperatorListen}
}
