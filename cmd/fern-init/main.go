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
	"path/filepath"
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
	env := os.Environ()

	services, err := loadServices(filepath.Join(dir, "services.yaml"))
	if err != nil {
		return err
	}
	// An agent box runs Claude next to the box that runs the services.
	if os.Getenv("FERN_SERVICES") == "off" {
		services = nil
	}
	if claude := os.Getenv("CLAUDE_CONFIG_DIR"); claude != "" {
		// Claude works without the hint, only less well.
		if err := writeHint(claude, services); err != nil {
			log.WarnContext(ctx, "writing Claude's hint failed", slog.Any("error", err))
		}
	}

	// Services outlive ctx: they are stopped, all at once, after it is done.
	// One with a socket starts at its first connection, except in a golden
	// builder's box, whose services must make their first start.
	var stoppers []func(context.Context)
	lazy := make(map[string]*onDemand)
	for name, svc := range services {
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
	mux.Handle("GET "+boxexec.Path, boxexec.NewServer(token, dir, env, log))
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
	}
	log.InfoContext(ctx, "stopping")
	var stopping sync.WaitGroup
	for _, stop := range stoppers {
		stopping.Go(func() { stop(context.WithoutCancel(ctx)) })
	}
	stopping.Wait()
	// Execs have ended with ctx; what is left is a readiness answer no one
	// needs any more.
	_ = srv.Close()
	if err == nil {
		err = <-serveErr // the server has stopped: ErrServerClosed
	}
	if !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve exec: %w", err)
	}
	return nil
}
