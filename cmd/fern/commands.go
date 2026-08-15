package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/watch"
)

func runDown(args []string, log *slog.Logger) error {
	fs, nameFlag, configPath := workspaceFlags("down")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	name, err := workspaceName(fs, *nameFlag, *configPath)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext()
	defer cancel()
	lease, err := acquireWorkspaceLease(name)
	if err != nil {
		return err
	}
	defer lease.Release()
	docker, err := newDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
	return docker.Destroy(ctx, name)
}

func runResume(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	configPath := fs.String("config", "fern.yaml", "configuration file")
	name := fs.String("name", "", "workspace name")
	image := fs.String("image", "", "workspace image")
	repo := fs.String("repo", "", "host repository path")
	memory := fs.String("memory", "", "memory limit")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := config.LoadWorkspace(*configPath, cwd, flagSet(fs, "config"), config.Overrides{
		Name: optionalFlag(fs, "name", name), Image: optionalFlag(fs, "image", image),
		Repo: optionalFlag(fs, "repo", repo), Memory: optionalFlag(fs, "memory", memory),
	})
	if err != nil {
		return err
	}
	cfg.Workspace.Env = forwardedEnvironment(cfg.Workspace.Env)
	if err := config.ValidateWorkspace(cfg); err != nil {
		return err
	}
	memoryBytes, err := config.ParseMemoryBytes(cfg.Workspace.Memory)
	if err != nil {
		return err
	}
	spec := runtime.Spec{Name: cfg.Workspace.Name, Image: cfg.Workspace.Image, RepoPath: cfg.Workspace.Repo, MemoryBytes: memoryBytes, Env: cfg.Workspace.Env}
	ctx, cancel := commandContext()
	defer cancel()
	lease, err := acquireWorkspaceLease(spec.Name)
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
	fs, nameFlag, configPath := workspaceFlags("status")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	name, err := workspaceName(fs, *nameFlag, *configPath)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext()
	defer cancel()
	docker, err := newDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
	observation, err := docker.Status(ctx, name)
	if err != nil {
		return err
	}
	fmt.Printf("%s\t%s", name, observation.State)
	if observation.State != runtime.StateAbsent {
		fmt.Printf("\tdocker=%s exit=%d oom=%t", observation.DockerStatus, observation.ExitCode, observation.OOMKilled)
	}
	fmt.Println()
	return nil
}

func runEvents(args []string, log *slog.Logger) error {
	fs, nameFlag, configPath := workspaceFlags("debug events")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	client, err := config.LoadClient(*configPath, flagSet(fs, "config"))
	if err != nil {
		return err
	}
	if flagSet(fs, "name") {
		client.Name = *nameFlag
	}
	if err := config.ValidateWorkspaceName(client.Name); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	docker, err := newDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
	observation, err := docker.Status(ctx, client.Name)
	if err != nil {
		return err
	}
	if observation.State != runtime.StateRunning || !observation.HasEndpoint {
		return fmt.Errorf("workspace %q is %s; start it before reading events", client.Name, observation.State)
	}
	env := forwardedEnvironment(client.Env)
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
	fs, nameFlag, configPath := workspaceFlags("logs")
	follow := fs.Bool("follow", true, "follow log output")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	name, err := workspaceName(fs, *nameFlag, *configPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	docker, err := newDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
	return docker.StreamLogs(ctx, name, *follow, os.Stdout, os.Stderr)
}
