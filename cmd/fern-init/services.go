package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"sync"
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

// Stopping a box must fit in its pod's 15s grace period, after which
// Kubernetes kills everything.
const (
	stopTimeout = 8 * time.Second // bounds a service's stop command
	killDelay   = 3 * time.Second // from SIGTERM to SIGKILL
)

// supervisor keeps one service running until it is stopped.
type supervisor struct {
	name string
	svc  service
	dir  string
	env  []string // the service's whole environment
	log  *slog.Logger

	quit chan struct{}      // closed when the service must not be restarted
	kill context.CancelFunc // sends SIGTERM to the running service
	done chan struct{}      // closed when run has returned
}

// startService starts and supervises svc. It keeps running after ctx is
// done, until stop.
func startService(ctx context.Context, name string, svc service, dir string, env []string, log *slog.Logger) *supervisor {
	ctx, kill := context.WithCancel(context.WithoutCancel(ctx))
	s := &supervisor{
		name: name, svc: svc, dir: dir, env: svc.environ(env), log: log,
		quit: make(chan struct{}), kill: kill, done: make(chan struct{}),
	}
	go s.run(ctx)
	return s
}

// run runs the service and restarts it with backoff whenever it exits.
func (s *supervisor) run(ctx context.Context) {
	defer close(s.done)
	const minBackoff, maxBackoff = time.Second, 30 * time.Second
	backoff := minBackoff
	for {
		started := time.Now()
		err := runService(ctx, s.svc, s.dir, s.env)
		select {
		case <-s.quit:
			return
		default:
		}
		s.log.WarnContext(ctx, "service exited", slog.String("service", s.name), slog.Any("error", err))
		if time.Since(started) > time.Minute {
			backoff = minBackoff
		}
		select {
		case <-s.quit:
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

// stop stops the service for good: its stop command, if it has one, then
// SIGTERM, then SIGKILL. It returns once the service has exited.
func (s *supervisor) stop(ctx context.Context) {
	close(s.quit)
	if s.svc.Stop != "" {
		if err := shell(ctx, stopTimeout, s.svc.Stop, s.dir, s.env); err != nil {
			s.log.WarnContext(ctx, "stop command failed", slog.String("service", s.name), slog.Any("error", err))
		}
	}
	s.kill()
	<-s.done
}

func runService(ctx context.Context, svc service, dir string, env []string) error {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", svc.Run)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// The service gets its own process group, so stopping it stops its children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = killDelay
	return cmd.Run()
}

// The ready checks share readyTimeout. A timed-out script is killed and
// gets shellWaitDelay to let go of its output, so /healthz answers within
// 2.25s, under the 2.5s it promises and the probe's 3s timeout.
const (
	readyTimeout   = 2 * time.Second
	shellWaitDelay = 250 * time.Millisecond
)

// ready runs every service's ready check at once and returns the failures.
func ready(ctx context.Context, services map[string]service, dir string, env []string) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	names := slices.Sorted(maps.Keys(services)) // a stable order for the errors
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		svc := services[name]
		if svc.Ready == "" {
			continue
		}
		wg.Go(func() {
			if err := shell(ctx, readyTimeout, svc.Ready, dir, svc.environ(env)); err != nil {
				errs[i] = fmt.Errorf("%s not ready: %w", name, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// shell runs script with /bin/sh in its own process group, bounded by
// timeout. When time is up the whole group is killed.
func shell(ctx context.Context, timeout time.Duration, script, dir string, env []string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Dir, cmd.Env = dir, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Killing only the shell would leave its children holding the output
	// pipe, and Wait would wait for them. Like runService's Cancel, this can
	// run in the microseconds after the shell has been reaped.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// Bounds the wait for output held open by processes outside the group.
	cmd.WaitDelay = shellWaitDelay
	out, err := cmd.CombinedOutput()
	// ErrWaitDelay means the script succeeded but left something behind
	// holding its output; the script's own result is what counts.
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}
