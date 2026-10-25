package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"

	"sigs.k8s.io/yaml"
)

// service is one entry in services.yaml. Commands are shell scripts, run
// with /bin/sh -c in the workspace.
type service struct {
	Run   string `json:"run"`
	Stop  string `json:"stop,omitempty"`  // graceful stop; else SIGTERM
	Ready string `json:"ready,omitempty"` // exits 0 when the service is ready
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

// parseServices parses services.yaml; every service needs a run command.
func parseServices(data []byte) (map[string]service, error) {
	var f struct {
		Services map[string]service `json:"services"`
		// Golden lists the files a golden build depends on; the daemon reads
		// it (internal/setup), fern-init doesn't.
		Golden struct {
			Inputs []string `json:"inputs,omitempty"`
		} `json:"golden"`
	}
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

// Stopping a box must fit in its pod's 15s grace period, after which
// Kubernetes kills everything.
const (
	stopTimeout = 8 * time.Second // bounds a service's stop command
	killDelay   = 3 * time.Second // from SIGTERM to SIGKILL
)

// The pause between restarts of a service that keeps exiting doubles from
// minBackoff to maxBackoff, and starts over once a run has lasted stableRun.
const (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
	stableRun  = time.Minute
)

// supervisor keeps one service running until it is stopped.
type supervisor struct {
	name string
	svc  service
	dir  string
	env  []string // the service's whole environment
	log  *slog.Logger

	quit      chan struct{}      // closed when the service must not be restarted
	terminate context.CancelFunc // sends SIGTERM to the running service
	done      chan struct{}      // closed when run has returned
}

// startService starts and supervises svc. It keeps running after ctx is
// done, until stop.
func startService(ctx context.Context, name string, svc service, dir string, env []string, log *slog.Logger) *supervisor {
	ctx, terminate := context.WithCancel(context.WithoutCancel(ctx))
	s := &supervisor{
		name: name, svc: svc, dir: dir, env: env, log: log,
		quit: make(chan struct{}), terminate: terminate, done: make(chan struct{}),
	}
	go s.run(ctx)
	return s
}

// run runs the service and restarts it with backoff whenever it exits.
func (s *supervisor) run(ctx context.Context) {
	defer close(s.done)
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
		if time.Since(started) > stableRun {
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
	s.terminate()
	<-s.done
}

// runService runs svc once, until it exits or ctx is done.
func runService(ctx context.Context, svc service, dir string, env []string) error {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", svc.Run)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// The service gets its own process group, so stopping it stops its children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Stopping sends SIGTERM to the group and, killDelay later, SIGKILL to
	// the group: cmd.WaitDelay would kill only the leader and leave children
	// that ignore SIGTERM running.
	//
	// A group must not be signalled once its leader has been reaped, as its
	// id may then be reused, so the timer is stopped when Run returns. Two
	// windows of microseconds remain, both because os/exec reaps the leader
	// before Run returns: the timer can fire in between, and os/exec can
	// still call Cancel if ctx is done at that moment. Harm would also need
	// the kernel to hand out the same pid again within that window.
	var (
		escalate *time.Timer
		kill     time.Time // when the group gets SIGKILL
	)
	cmd.Cancel = func() error {
		pgid := cmd.Process.Pid
		kill = time.Now().Add(killDelay)
		escalate = time.AfterFunc(killDelay, func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
		return syscall.Kill(-pgid, syscall.SIGTERM)
	}
	err := cmd.Run()
	// Run reports Cancel's error, so it returns only after Cancel has:
	// escalate and kill need no lock.
	if escalate != nil {
		escalate.Stop()
	} else {
		kill = time.Now().Add(killDelay)
	}
	// What the script started can outlive it, such as a database it put in
	// the background. Left running, it would hold the service's data while
	// a restart starts another copy, or outlive a stop.
	if cmd.Process != nil {
		endGroup(cmd.Process.Pid, kill)
	}
	return err
}

// endGroup ends what is left of process group pgid, whose leader has
// exited: SIGTERM, then SIGKILL if anything is still there at kill. A
// group's id isn't handed out again while a process is in it, so this
// signals only the service's processes, except in the moment the group
// empties.
func endGroup(pgid int, kill time.Time) {
	if syscall.Kill(-pgid, syscall.SIGTERM) != nil {
		return // the group is empty
	}
	for ; time.Now().Before(kill); time.Sleep(groupPoll) {
		if syscall.Kill(-pgid, 0) != nil {
			return
		}
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// groupPoll is how often endGroup looks whether a group has emptied.
const groupPoll = 50 * time.Millisecond

// shellWaitDelay bounds how long a script's output is waited for once it
// has been killed: something outside its group can still hold it.
const shellWaitDelay = 250 * time.Millisecond

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
	// A ready check runs every 100ms while a box wakes: a failing one that
	// prints a lot must not grow fern-init by what it prints.
	out := &headBuffer{max: maxShellOutput}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	// ErrWaitDelay means the script succeeded but left something behind
	// holding its output; the script's own result is what counts.
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// maxShellOutput is how much of a script's output an error quotes.
const maxShellOutput = 4 << 10

// headBuffer keeps the first max bytes written to it and counts the rest.
// It accepts every write, so the script never blocks on a full pipe. As a
// command's Stdout and Stderr both, os/exec writes to it from one goroutine
// at a time.
type headBuffer struct {
	max     int
	buf     []byte
	dropped int
}

func (b *headBuffer) Write(p []byte) (int, error) {
	n := min(len(p), b.max-len(b.buf))
	b.buf = append(b.buf, p[:n]...)
	b.dropped += len(p) - n
	return len(p), nil
}

func (b *headBuffer) String() string {
	if b.dropped == 0 {
		return string(b.buf)
	}
	return fmt.Sprintf("%s… (%d more bytes)", b.buf, b.dropped)
}
