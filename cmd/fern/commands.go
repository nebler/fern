package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

func runStatus(args []string, log *slog.Logger) error {
	fs, nameFlag, configPath := workspaceFlags("status")
	jsonOutput := fs.Bool("json", false, "output a stable JSON object")
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
	if *jsonOutput {
		return writeStatusJSON(os.Stdout, name, observation)
	}
	fmt.Printf("%s\t%s", name, observation.State)
	if observation.State != runtime.StateAbsent {
		fmt.Printf("\tdocker=%s exit=%d oom=%t", observation.DockerStatus, observation.ExitCode, observation.OOMKilled)
	}
	fmt.Println()
	return nil
}

type statusJSON struct {
	Workspace    string        `json:"workspace"`
	State        runtime.State `json:"state"`
	DockerStatus string        `json:"dockerStatus"`
	ExitCode     int           `json:"exitCode"`
	OOMKilled    bool          `json:"oomKilled"`
}

func writeStatusJSON(output io.Writer, workspace string, observation runtime.Observation) error {
	return json.NewEncoder(output).Encode(statusJSON{
		Workspace: workspace, State: observation.State, DockerStatus: observation.DockerStatus,
		ExitCode: observation.ExitCode, OOMKilled: observation.OOMKilled,
	})
}

func runEvents(args []string, log *slog.Logger) error {
	fs, nameFlag, configPath := workspaceFlags("debug events")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	client, err := config.LoadEvents(*configPath, flagSet(fs, "config"), optionalFlag(fs, "name", nameFlag))
	if err != nil {
		return err
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
	auth := runtime.ServerAuth{Password: env["OPENCODE_PASSWORD"]}
	if err := runtime.WaitHealthy(ctx, observation.Endpoint, auth, 60*time.Second); err != nil {
		return fmt.Errorf("wait for OpenCode health: %w", err)
	}
	events := make(chan watch.Event, 128)
	go watch.StreamForever(ctx, watch.StreamOptions{
		BaseURL: observation.Endpoint.URL(), Auth: auth,
	}, events, log)
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-events:
			payload := event.Properties
			if len(payload) == 0 {
				payload = event.Data
			}
			properties := string(payload)
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
	err = docker.StreamLogs(ctx, name, *follow, os.Stdout, os.Stderr)
	if ctx.Err() != nil {
		return nil
	}
	return err
}
