// Command fern-init is the main process of every box (under tini, which is
// PID 1 and reaps orphans). It starts and supervises the
// services in the workspace's services.yaml, serves exec and readiness on
// boxexec.Port, and stops the services before exiting on SIGTERM or an
// authenticated stop request.
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
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/nebler/fern/internal/boxexec"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	err := run(ctx, log)
	stop()
	if err != nil {
		log.Error("fern-init failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	dir := os.Getenv("FERN_WORKSPACE")
	if dir == "" {
		return errors.New("FERN_WORKSPACE is not set")
	}
	// The token is for fern-init alone: commands and services don't inherit
	// it. A process of the box's user can still read it in /proc, which is
	// why only the daemon may connect to fern-init.
	token := os.Getenv("FERN_BOX_TOKEN")
	if token == "" {
		return errors.New("FERN_BOX_TOKEN is not set")
	}
	if err := os.Unsetenv("FERN_BOX_TOKEN"); err != nil {
		return err
	}
	env := toolsEnv(dir, os.Environ())

	services, guessed, err := loadServices(dir)
	if err != nil {
		return err
	}
	// An agent box runs Claude next to the box that runs the services, which
	// installs the tools they share.
	if os.Getenv("FERN_SERVICES") == "off" {
		services = nil
	} else {
		installTools(ctx, dir, env, log)
	}
	if claude := os.Getenv("CLAUDE_CONFIG_DIR"); claude != "" {
		// Claude works without the hint and the skills, only less well.
		if err := briefClaude(claude, dir, services, guessed); err != nil {
			log.WarnContext(ctx, "writing Claude's hint and skills failed", slog.Any("error", err))
		}
	}

	// Services outlive ctx: they are stopped, all at once, after it is done.
	// One with a socket starts at its first connection, except in a golden
	// builder's box, whose services must make their first start.
	var stoppers []func(context.Context)
	lazy := make(map[string]*onDemand)
	for name, svc := range services {
		env := svc.environ(env)
		if svc.Listen != "" && os.Getenv("FERN_ON_DEMAND") != "off" {
			o, err := listenOnDemand(ctx, name, svc, dir, env, log)
			if err != nil {
				for _, stop := range stoppers {
					stop(ctx)
				}
				return err
			}
			lazy[name] = o
			stoppers = append(stoppers, o.stop)
			continue
		}
		stoppers = append(stoppers, startService(ctx, name, svc, dir, env, log).stop)
	}

	mux := http.NewServeMux()
	execs := boxexec.NewServer(token, dir, env, log)
	mux.Handle("GET "+boxexec.Path, execs)
	mux.HandleFunc("GET "+boxexec.ActivityPath, execs.ServeActivity)
	// Background commands, which outlive the daemon's connections to them.
	jobs := boxexec.NewJobs(token, dir, env, log)
	jobs.OnStart = execs.Opened
	jobs.Register(mux)
	mux.Handle("GET "+boxexec.ReadyPath, readiness(func(ctx context.Context) error {
		// A service that hasn't started is ready: its socket is.
		check := make(map[string]service, len(services))
		for name, svc := range services {
			if o, ok := lazy[name]; !ok || o.isStarted() {
				check[name] = svc
			}
		}
		return checkServices(ctx, check, dir, env)
	}))
	mux.Handle("POST "+boxexec.StopPath, shutdown(token, os.Getenv("FERN_WAKE_ID"), cancel))
	mux.Handle("POST "+boxexec.TouchPath, touch(token, dir))
	// An agent box's Claude reaches the daemon through this, which holds its
	// calls while the daemon restarts.
	var relayErr chan error
	var relay *http.Server
	if daemon := os.Getenv("FERN_MCP"); daemon != "" {
		mux.Handle("GET "+boxexec.MCPRelayPath, boxexec.MCPRelayCheck(token))
		relay = &http.Server{
			Addr:              "127.0.0.1:" + strconv.Itoa(boxexec.MCPRelayPort),
			Handler:           boxexec.NewMCPRelay(daemon, boxexec.DefaultGrace),
			BaseContext:       func(net.Listener) context.Context { return ctx },
			ReadHeaderTimeout: 10 * time.Second,
		}
		relayErr = make(chan error, 1)
		go func() { relayErr <- relay.ListenAndServe() }()
	}
	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(boxexec.Port),
		Handler:           mux,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	log.InfoContext(ctx, "fern-init started", slog.Int("services", len(services)))

	select {
	case <-ctx.Done():
	case err = <-serveErr:
	case err = <-relayErr:
		if err != nil {
			err = fmt.Errorf("serve the MCP relay: %w", err)
		}
		cancel()
	}
	log.InfoContext(ctx, "stopping")
	jobs.Close()
	var stopping sync.WaitGroup
	for _, stop := range stoppers {
		stopping.Go(func() { stop(context.WithoutCancel(ctx)) })
	}
	stopping.Wait()
	// Execs have ended with ctx; what is left is a readiness answer no one
	// needs any more.
	_ = srv.Close()
	if relay != nil {
		_ = relay.Close()
	}
	if err == nil {
		err = <-serveErr // the server has stopped: ErrServerClosed
	}
	if !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve exec: %w", err)
	}
	return nil
}
