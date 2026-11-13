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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nebler/fern/internal/background"
	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/mcprelay"
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
	// The box that runs the services puts the tools its workspace pins
	// first on its PATH. Claude's box does not: that folder is the
	// workspace's, which the other box writes, and a file named claude or
	// git there would run here, where Claude's login is (found 2026-11-13).
	// Nothing in Claude's box needs the repository's tools: its Bash runs
	// in the other one.
	env := os.Environ()
	if os.Getenv("FERN_SERVICES") != "off" {
		env = toolsEnv(dir, env)
	} else {
		env = guardGit(env)
	}

	services, guessed, err := loadServices(dir)
	// Claude's box starts with a services.yaml that can't be read: Claude
	// may be there to mend it, and its hint says what is wrong with it. The
	// box that runs the services doesn't.
	unreadable := ""
	if err != nil {
		if os.Getenv("FERN_SERVICES") != "off" {
			return err
		}
		unreadable = err.Error()
	}
	// What the environment's box runs, for Claude's hint: an agent box runs
	// none of it, and until 2026-11-13 its hint named none either.
	described := services
	// An agent box runs Claude next to the box that runs the services, which
	// installs the tools they share. A golden builder's first box installs
	// them for the repository's setup script, which comes before the
	// services: the daemon runs it there, then wakes the box again for them.
	// A golden builder's box, in either of its two kinds, has longer for it,
	// and doesn't start without the tools: a golden that lacks them would
	// give every thread a box that fails at its first command, with nothing
	// to say why.
	builder := os.Getenv("FERN_SERVICES") == "later" || os.Getenv("FERN_ON_DEMAND") == "off"
	switch {
	case os.Getenv("FERN_SERVICES") == "off":
		services = nil
	case builder:
		if os.Getenv("FERN_SERVICES") == "later" {
			services = nil
		}
		if err := installTools(ctx, dir, env, builderInstallTimeout, log); err != nil {
			return fmt.Errorf("installing the repository's tools for its golden snapshot: %w", err)
		}
	default:
		if err := installTools(ctx, dir, env, installTimeout, log); err != nil {
			log.WarnContext(ctx, "installing the workspace's tools failed", slog.Any("error", err))
		}
	}
	if claude := os.Getenv("CLAUDE_CONFIG_DIR"); claude != "" {
		// Claude works without the hint and the skills, only less well.
		if err := briefClaude(claude, dir, described, guessed, unreadable); err != nil {
			log.WarnContext(ctx, "writing Claude's hint and skills failed", slog.Any("error", err))
		}
	}

	// Services outlive ctx: they are stopped, all at once, after it is done.
	// One with a socket starts at its first connection, except in a golden
	// builder's box, whose services must make their first start.
	var stoppers []func(context.Context)
	lazy := make(map[string]*onDemand)
	always := make(map[string]*supervisor)
	for name, svc := range services {
		env := svc.environ(env)
		if svc.Listen != "" && os.Getenv("FERN_ON_DEMAND") != "off" {
			o, err := listenOnDemand(ctx, name, svc, dir, env, startTimeout, log)
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
		always[name] = startService(ctx, name, svc, dir, env, log)
		stoppers = append(stoppers, always[name].stop)
	}

	mux := http.NewServeMux()
	execs := boxexec.NewServer(token, dir, env, log)
	mux.Handle("GET "+boxexec.Path, execs)
	mux.HandleFunc("GET "+boxexec.ActivityPath, execs.ServeActivity)
	// Background commands, which outlive the daemon's connections to them.
	jobs := background.NewJobs(token, dir, env, log)
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
		return checkServices(ctx, check, dir, env, func(name string) string {
			sup := always[name]
			if o, ok := lazy[name]; ok {
				sup = o.supervisor()
			}
			return sup.status().why()
		})
	}))
	mux.Handle("POST "+boxexec.StopPath, shutdown(token, os.Getenv("FERN_WAKE_ID"), cancel))
	mux.Handle("POST "+boxexec.TouchPath, touch(token, dir))
	// An agent box's Claude reaches the daemon through this, which holds its
	// calls while the daemon restarts.
	var relayErr chan error
	var relay *http.Server
	if daemon := os.Getenv("FERN_MCP"); daemon != "" {
		// The daemon asks every agent box whether it has the relay, to tell
		// apart one running since before 2026-11-12, which doesn't answer
		// as this does: the route goes when the daemon stops asking (tools'
		// mcpURL).
		mux.Handle("GET "+mcprelay.Path, mcprelay.Check(token))
		relay = &http.Server{
			Addr:              "127.0.0.1:" + strconv.Itoa(mcprelay.Port),
			Handler:           mcprelay.New(daemon, token, boxexec.DefaultGrace),
			BaseContext:       func(net.Listener) context.Context { return ctx },
			ReadHeaderTimeout: 10 * time.Second,
		}
		relayErr = make(chan error, 1)
		go func() { relayErr <- relay.ListenAndServe() }()
	}
	// T3 names its own MCP server to Claude at an address on the node's
	// loopback: an agent box's fern-init answers there and passes the calls
	// to the daemon, which passes them to T3.
	var t3 *http.Server
	t3Done := make(chan struct{})
	if addr, daemon := os.Getenv("FERN_T3_ADDR"), os.Getenv("FERN_T3_MCP"); addr != "" && daemon != "" {
		h, err := t3Relay(daemon, token)
		if err != nil {
			return fmt.Errorf("FERN_T3_MCP: %w", err)
		}
		t3 = &http.Server{
			Addr: addr, Handler: h, BaseContext: func(net.Listener) context.Context { return ctx },
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			defer close(t3Done)
			// Claude works without T3's tools: the box goes on without them.
			if err := t3.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				log.WarnContext(ctx, "serving T3's MCP server in the box", slog.Any("error", err))
			}
		}()
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
	if t3 != nil {
		_ = t3.Close()
		<-t3Done
	}
	if err == nil {
		err = <-serveErr // the server has stopped: ErrServerClosed
	}
	if !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve exec: %w", err)
	}
	return nil
}

// gitGuard is where the image keeps git's own programs with fern-git as the
// git among them (cmd/fern-git, images/Dockerfile).
const gitGuard = "/usr/local/libexec/fern-git-core"

// guardGit returns env with fern-git as git, for Claude's box: first on the
// PATH, and as the git that git starts (GIT_EXEC_PATH). Claude Code runs git
// status in its folder by itself, and the folder is the workspace, which the
// other box writes: a repository made in a folder there has a config of that
// box's own, and once the index holds the folder as a submodule, git starts
// a git in it that runs what the config names, here, where Claude's login is
// (found 2026-11-13). fern-git opens the worktree's own repository and no
// other. Nor does git take a folder for a bare repository unless told.
func guardGit(env []string) []string {
	out := make([]string, 0, len(env)+6)
	path := "/usr/local/bin:/usr/bin:/bin"
	for _, v := range env {
		switch {
		case strings.HasPrefix(v, "PATH="):
			path = strings.TrimPrefix(v, "PATH=")
		case strings.HasPrefix(v, "GIT_EXEC_PATH="), strings.HasPrefix(v, "GIT_CONFIG_"):
		default:
			out = append(out, v)
		}
	}
	return append(out,
		"PATH="+gitGuard+":"+path,
		"GIT_EXEC_PATH="+gitGuard,
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.bareRepository", "GIT_CONFIG_VALUE_0=explicit",
	)
}
