// Command fern-init is PID 1 in every box. It starts and supervises the
// services in the workspace's services.yaml, serves exec on boxexec.Port,
// and on SIGTERM stops the services before exiting.
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

// stopTimeout bounds each service's stop command.
const stopTimeout = 10 * time.Second

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
	dir := os.Getenv("FERN_WORKSPACE")
	if dir == "" {
		dir = "/workspace"
	}
	// The token is for fern-init alone; commands and services don't inherit it.
	token := os.Getenv("FERN_TOKEN")
	if token == "" {
		return errors.New("FERN_TOKEN is not set")
	}
	if err := os.Unsetenv("FERN_TOKEN"); err != nil {
		return err
	}
	env := os.Environ()

	services, err := loadServices(filepath.Join(dir, "services.yaml"))
	if err != nil {
		return err
	}

	// Services outlive ctx: they are stopped in order once ctx is done.
	svcCtx, stopServices := context.WithCancel(context.WithoutCancel(ctx))
	defer stopServices()
	var running sync.WaitGroup
	for name, svc := range services {
		running.Go(func() { supervise(svcCtx, name, svc, dir, env, log) })
	}

	mux := http.NewServeMux()
	mux.Handle("GET /exec", boxexec.NewServer(token, dir, env, log))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := ready(r.Context(), services, dir, env); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintln(w, "ok")
	})
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
	for name, svc := range services {
		if svc.Stop == "" {
			continue
		}
		if err := shell(context.WithoutCancel(ctx), stopTimeout, svc.Stop, dir, svc.environ(env)); err != nil {
			log.WarnContext(ctx, "stop command failed", slog.String("service", name), slog.Any("error", err))
		}
	}
	stopServices()
	running.Wait()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if err == nil {
		err = <-serveErr // the server has stopped: ErrServerClosed
	}
	if !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve exec: %w", err)
	}
	return nil
}
