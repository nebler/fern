package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"sigs.k8s.io/yaml"
)

// service is one entry in services.yaml. Commands are shell scripts, run
// with /bin/sh -c in the workspace.
type service struct {
	Run   string            `json:"run"`
	Stop  string            `json:"stop,omitempty"`  // graceful stop; else SIGTERM
	Ready string            `json:"ready,omitempty"` // exits 0 when the service is ready
	Port  int               `json:"port,omitempty"`  // passed as $PORT
	Env   map[string]string `json:"env,omitempty"`
}

type servicesFile struct {
	Services map[string]service `json:"services"`
}

// loadServices reads services.yaml at path. A missing file means no services.
func loadServices(path string) (map[string]service, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseServices(data)
}

func parseServices(data []byte) (map[string]service, error) {
	var f servicesFile
	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		return nil, fmt.Errorf("services.yaml: %w", err)
	}
	for name, svc := range f.Services {
		if svc.Run == "" {
			return nil, fmt.Errorf("services.yaml: service %q: run is required", name)
		}
	}
	return f.Services, nil
}

// environ returns base plus the service's own variables.
func (s service) environ(base []string) []string {
	env := append([]string(nil), base...)
	if s.Port != 0 {
		env = append(env, "PORT="+strconv.Itoa(s.Port))
	}
	for k, v := range s.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// supervise runs a service and restarts it with backoff whenever it exits,
// until ctx is done.
func supervise(ctx context.Context, name string, svc service, dir string, env []string, log *slog.Logger) {
	const minBackoff, maxBackoff = time.Second, 30 * time.Second
	backoff := minBackoff
	for {
		started := time.Now()
		err := runService(ctx, svc, dir, svc.environ(env))
		if ctx.Err() != nil {
			return
		}
		log.WarnContext(ctx, "service exited", slog.String("service", name), slog.Any("error", err))
		if time.Since(started) > time.Minute {
			backoff = minBackoff
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

func runService(ctx context.Context, svc service, dir string, env []string) error {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", svc.Run)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// The service gets its own process group, so stopping it stops its children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = stopTimeout
	return cmd.Run()
}

// ready runs every service's ready check and returns the first failure.
func ready(ctx context.Context, services map[string]service, dir string, env []string) error {
	for name, svc := range services {
		if svc.Ready == "" {
			continue
		}
		if err := shell(ctx, 2*time.Second, svc.Ready, dir, svc.environ(env)); err != nil {
			return fmt.Errorf("%s not ready: %w", name, err)
		}
	}
	return nil
}

// shell runs script with /bin/sh, bounded by timeout.
func shell(ctx context.Context, timeout time.Duration, script, dir string, env []string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Dir, cmd.Env = dir, env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}
