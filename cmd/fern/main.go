package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/proxy"
	"github.com/nebler/fern/internal/registry"
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/watch"
	"github.com/nebler/fern/internal/workspace"
	"golang.org/x/sync/errgroup"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if err := run(os.Args[1:], log); err != nil {
		log.Error("command failed", "err", err)
		os.Exit(1)
	}
}

func run(args []string, log *slog.Logger) error {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "up":
		return runUp(args[1:], log)
	case "down":
		return runDown(args[1:], log)
	case "resume":
		return runResume(args[1:], log)
	case "status":
		return runStatus(args[1:], log)
	case "logs":
		return runLogs(args[1:], log)
	case "debug":
		if len(args) > 1 && args[1] == "events" {
			return runEvents(args[2:], log)
		}
	}
	return usage()
}

func runUp(args []string, log *slog.Logger) error {
	cfg, fs, err := commandConfig("up", args)
	if err != nil {
		return err
	}
	name := fs.String("name", cfg.Workspace.Name, "workspace name")
	image := fs.String("image", cfg.Workspace.Image, "workspace image")
	repo := fs.String("repo", cfg.Workspace.Repo, "host repository path")
	memory := fs.String("memory", cfg.Workspace.Memory, "memory limit (for example 8Gi)")
	idle := fs.Duration("idle", cfg.IdleAfter, "idle duration before stopping")
	listenAddress := fs.String("listen", cfg.Listen, "proxy listen address")
	selection, err := configSelection(args)
	if err != nil {
		return err
	}
	_ = fs.String("config", selection.path, "configuration file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	resolvedRepo, err := filepath.Abs(*repo)
	if err != nil {
		return fmt.Errorf("resolve repository path: %w", err)
	}
	cfg.Workspace.Name = *name
	cfg.Workspace.Image = *image
	cfg.Workspace.Repo = resolvedRepo
	cfg.Workspace.Memory = *memory
	cfg.IdleAfter = *idle
	cfg.Listen = *listenAddress
	cfg.Workspace.Env = forwardedEnvironment(cfg.Workspace.Env)
	if err := config.Validate(cfg); err != nil {
		return err
	}
	memoryBytes, _ := config.ParseMemoryBytes(cfg.Workspace.Memory)
	auth := runtime.ServerAuth{
		Username: cfg.Workspace.Env["OPENCODE_SERVER_USERNAME"],
		Password: cfg.Workspace.Env["OPENCODE_SERVER_PASSWORD"],
	}
	spec := runtime.Spec{
		Name:        cfg.Workspace.Name,
		Image:       cfg.Workspace.Image,
		RepoPath:    cfg.Workspace.Repo,
		MemoryBytes: memoryBytes,
		Env:         cfg.Workspace.Env,
	}

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
	docker, err := newDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()

	observations := make(chan watch.Observation, 64)
	streamController := watch.NewStreamController(serviceCtx, watch.StreamOptions{Auth: auth}, observations, log)
	manager := workspace.NewManager(
		serviceCtx,
		docker,
		spec,
		func(ctx context.Context, ep runtime.Endpoint, force bool) error {
			if force {
				return streamController.Reconnect(ctx, ep.URL())
			}
			return streamController.Connect(ctx, ep.URL())
		},
		func(ctx context.Context, ep runtime.Endpoint) (bool, error) {
			return watch.AllSessionsIdle(ctx, ep, auth)
		},
		func() {
			select {
			case observations <- watch.Observation{Kind: watch.ObservationRequest}:
			case <-serviceCtx.Done():
			}
		},
		log,
	)
	start := time.Now()
	ep, err := manager.EnsureRunning(serviceCtx)
	if err != nil {
		return err
	}

	supervisor := &watch.Supervisor{IdleAfter: cfg.IdleAfter, OnPause: manager.Pause, Log: log}
	connections := newConnectionTracker()
	server := &http.Server{
		Handler:           proxy.New(manager, log),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return serviceCtx },
		ConnState:         connections.track,
	}
	group.Go(func() error {
		err := supervisor.Run(serviceCtx, observations)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	})
	group.Go(func() error {
		err := server.Serve(listener)
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

	fmt.Printf("workspace: %s\ndirect: %s\nproxy: http://%s\nready in: %s\n", spec.Name, ep.URL(), listener.Addr(), time.Since(start).Round(time.Millisecond))
	log.Info("proxy listening", "address", listener.Addr(), "workspace", spec.Name)
	err = group.Wait()
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	streamErr := streamController.Stop(stopCtx)
	waitErr := manager.Close(stopCtx)
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return errors.Join(err, streamErr, waitErr)
}

func runDown(args []string, log *slog.Logger) error {
	name, fs, err := commandWorkspaceName("down", args)
	if err != nil {
		return err
	}
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	ctx, cancel := commandContext()
	defer cancel()
	lease, err := acquireWorkspaceLease(*name)
	if err != nil {
		return err
	}
	defer lease.Release()
	docker, err := newDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
	return docker.Destroy(ctx, *name)
}

func runResume(args []string, log *slog.Logger) error {
	cfg, fs, err := commandConfig("resume", args)
	if err != nil {
		return err
	}
	name := fs.String("name", cfg.Workspace.Name, "workspace name")
	image := fs.String("image", cfg.Workspace.Image, "workspace image")
	repo := fs.String("repo", cfg.Workspace.Repo, "host repository path")
	memory := fs.String("memory", cfg.Workspace.Memory, "memory limit")
	selection, err := configSelection(args)
	if err != nil {
		return err
	}
	_ = fs.String("config", selection.path, "configuration file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	resolvedRepo, err := filepath.Abs(*repo)
	if err != nil {
		return err
	}
	cfg.Workspace.Name, cfg.Workspace.Image, cfg.Workspace.Repo, cfg.Workspace.Memory = *name, *image, resolvedRepo, *memory
	cfg.Workspace.Env = forwardedEnvironment(cfg.Workspace.Env)
	if err := config.ValidateWorkspace(cfg); err != nil {
		return err
	}
	memoryBytes, _ := config.ParseMemoryBytes(cfg.Workspace.Memory)
	spec := runtime.Spec{Name: *name, Image: *image, RepoPath: resolvedRepo, MemoryBytes: memoryBytes, Env: cfg.Workspace.Env}
	ctx, cancel := commandContext()
	defer cancel()
	lease, err := acquireWorkspaceLease(*name)
	if err != nil {
		return err
	}
	defer lease.Release()
	docker, err := newDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
	ep, err := docker.Resume(ctx, spec)
	if err != nil {
		return err
	}
	fmt.Println(ep.URL())
	return nil
}

func runStatus(args []string, log *slog.Logger) error {
	name, fs, err := commandWorkspaceName("status", args)
	if err != nil {
		return err
	}
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	ctx, cancel := commandContext()
	defer cancel()
	docker, err := newDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
	observation, err := docker.Status(ctx, *name)
	if err != nil {
		return err
	}
	fmt.Printf("%s\t%s", *name, observation.State)
	if observation.State != runtime.StateAbsent {
		fmt.Printf("\tdocker=%s exit=%d oom=%t", observation.DockerStatus, observation.ExitCode, observation.OOMKilled)
	}
	fmt.Println()
	return nil
}

func runEvents(args []string, log *slog.Logger) error {
	cfg, fs, err := commandConfig("debug events", args)
	if err != nil {
		return err
	}
	name := fs.String("name", cfg.Workspace.Name, "workspace name")
	selection, err := configSelection(args)
	if err != nil {
		return err
	}
	_ = fs.String("config", selection.path, "configuration file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	docker, err := newDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
	observation, err := docker.Status(ctx, *name)
	if err != nil {
		return err
	}
	if observation.State != runtime.StateRunning || !observation.HasEndpoint {
		return fmt.Errorf("workspace %q is %s; start it before reading events", *name, observation.State)
	}
	env := forwardedEnvironment(cfg.Workspace.Env)
	auth := runtime.ServerAuth{Username: env["OPENCODE_SERVER_USERNAME"], Password: env["OPENCODE_SERVER_PASSWORD"]}
	events := make(chan watch.Event, 128)
	go watch.StreamForever(ctx, watch.StreamOptions{BaseURL: observation.Endpoint.URL(), Auth: auth}, events, log)
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-events:
			properties := string(event.Properties)
			if len(properties) > 160 {
				properties = properties[:160] + "..."
			}
			fmt.Printf("[%s] +%-8s %-28s %s\n", time.Now().Format("15:04:05.000"), time.Since(last).Round(time.Millisecond), event.Type, properties)
			last = time.Now()
		}
	}
}

func runLogs(args []string, log *slog.Logger) error {
	name, fs, err := commandWorkspaceName("logs", args)
	if err != nil {
		return err
	}
	follow := fs.Bool("follow", true, "follow log output")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	docker, err := newDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
	return docker.StreamLogs(ctx, *name, *follow, os.Stdout, os.Stderr)
}

type selectedConfig struct {
	path     string
	required bool
}

type connectionTracker struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func newConnectionTracker() *connectionTracker {
	return &connectionTracker{conns: make(map[net.Conn]struct{})}
}

func (t *connectionTracker) track(connection net.Conn, state http.ConnState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch state {
	case http.StateNew:
		t.conns[connection] = struct{}{}
	case http.StateClosed:
		delete(t.conns, connection)
	}
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

func commandConfig(command string, args []string) (config.Config, *flag.FlagSet, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return config.Config{}, nil, err
	}
	selection, err := configSelection(args)
	if err != nil {
		return config.Config{}, nil, err
	}
	cfg, err := config.Load(selection.path, cwd, selection.required)
	if err != nil {
		return config.Config{}, nil, err
	}
	return cfg, flag.NewFlagSet(command, flag.ContinueOnError), nil
}

func commandWorkspaceName(command string, args []string) (*string, *flag.FlagSet, error) {
	selection, err := configSelection(args)
	if err != nil {
		return nil, nil, err
	}
	name, explicit, err := selectedFlag(args, "name")
	if err != nil {
		return nil, nil, err
	}
	if !explicit {
		name, err = config.LoadWorkspaceName(selection.path, selection.required)
		if err != nil {
			return nil, nil, err
		}
	}
	if err := config.ValidateWorkspaceName(name); err != nil {
		return nil, nil, err
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	nameFlag := flags.String("name", name, "workspace name")
	_ = flags.String("config", selection.path, "configuration file")
	return nameFlag, flags, nil
}

func selectedFlag(args []string, name string) (string, bool, error) {
	var value string
	found := false
	for index, arg := range args {
		if arg == "--" {
			break
		}
		for _, prefix := range []string{"-" + name + "=", "--" + name + "="} {
			if strings.HasPrefix(arg, prefix) {
				if found {
					return "", false, fmt.Errorf("-%s may be specified only once", name)
				}
				value, found = strings.TrimPrefix(arg, prefix), true
			}
		}
		if (arg == "-"+name || arg == "--"+name) && index+1 < len(args) {
			if found {
				return "", false, fmt.Errorf("-%s may be specified only once", name)
			}
			value, found = args[index+1], true
		} else if arg == "-"+name || arg == "--"+name {
			return "", false, fmt.Errorf("-%s requires a value", name)
		}
	}
	return value, found, nil
}

func configSelection(args []string) (selectedConfig, error) {
	path, found, err := selectedFlag(args, "config")
	if err != nil {
		return selectedConfig{}, err
	}
	if found {
		if path == "" {
			return selectedConfig{}, errors.New("-config requires a path")
		}
		return selectedConfig{path: path, required: true}, nil
	}
	return selectedConfig{path: "fern.yaml"}, nil
}

func parseFlags(flags *flag.FlagSet, args []string) error {
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	return nil
}

func forwardedEnvironment(configured map[string]string) map[string]string {
	env := make(map[string]string, len(configured)+4)
	for key, value := range configured {
		env[key] = value
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENCODE_SERVER_USERNAME", "OPENCODE_SERVER_PASSWORD"} {
		if _, configured := env[key]; !configured {
			if value := os.Getenv(key); value != "" {
				env[key] = value
			}
		}
	}
	return env
}

func acquireWorkspaceLease(name string) (*registry.Lease, error) {
	lockDir, err := statePath("locks")
	if err != nil {
		return nil, err
	}
	return registry.Acquire(lockDir, name)
}

func newDocker(log *slog.Logger) (*runtime.Docker, error) {
	stateDirectory, err := statePath("state")
	if err != nil {
		return nil, err
	}
	return runtime.NewDocker(log, registry.NewIntentStore(stateDirectory))
}

func commandContext() (context.Context, context.CancelFunc) {
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(signalCtx, 70*time.Second)
	return ctx, func() {
		cancel()
		stop()
	}
}

func statePath(child string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".fern", child), nil
}

func usage() error {
	fmt.Fprintln(os.Stderr, "usage: fern <up|down|resume|status|logs|debug events> [flags]")
	return errors.New("invalid command")
}
