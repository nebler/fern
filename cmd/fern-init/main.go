// Command fern-init is the main process of every box (the pod's pause
// container is PID 1 and reaps orphans). It starts and supervises the
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
	if claude := os.Getenv("CLAUDE_CONFIG_DIR"); claude != "" {
		// Claude works without the hint, only less well.
		if err := writeHint(claude, services); err != nil {
			log.WarnContext(ctx, "writing Claude's hint failed", slog.Any("error", err))
		}
	}

	// Services outlive ctx: they are stopped, all at once, after it is done.
	supervisors := make([]*supervisor, 0, len(services))
	for name, svc := range services {
		supervisors = append(supervisors, startService(ctx, name, svc, dir, env, log))
	}

	mux := http.NewServeMux()
	mux.Handle("GET "+boxexec.Path, boxexec.NewServer(token, dir, env, log))
	mux.Handle("GET "+boxexec.ReadyPath, readiness(func(ctx context.Context) error {
		return checkServices(ctx, services, dir, env)
	}))
	mux.Handle("POST "+boxexec.StopPath, shutdown(token, os.Getenv("FERN_POD_UID"), cancel))
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
