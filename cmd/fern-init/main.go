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

	// Services outlive ctx: they are stopped, all at once, after it is done.
	supervisors := make([]*supervisor, 0, len(services))
	for name, svc := range services {
		supervisors = append(supervisors, startService(ctx, name, svc, dir, env, log))
	}

	mux := http.NewServeMux()
	mux.Handle("GET "+boxexec.Path, boxexec.NewServer(token, dir, env, log))
	mux.Handle("GET /healthz", newReadiness(func(ctx context.Context) error {
		return ready(ctx, services, dir, env)
	}))
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
	for _, s := range supervisors {
		stopping.Go(func() { s.stop(context.WithoutCancel(ctx)) })
	}
	stopping.Wait()
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
