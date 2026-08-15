package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/registry"
	"github.com/nebler/fern/internal/runtime"
)

func workspaceFlags(command string) (*flag.FlagSet, *string, *string) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	name := flags.String("name", "", "workspace name")
	configPath := flags.String("config", "fern.yaml", "configuration file")
	return flags, name, configPath
}

func workspaceName(flags *flag.FlagSet, explicitName, configPath string) (string, error) {
	name := explicitName
	if !flagSet(flags, "name") {
		var err error
		name, err = config.LoadWorkspaceName(configPath, flagSet(flags, "config"))
		if err != nil {
			return "", err
		}
	}
	if err := config.ValidateWorkspaceName(name); err != nil {
		return "", err
	}
	return name, nil
}

func flagSet(flags *flag.FlagSet, name string) bool {
	set := false
	flags.Visit(func(flag *flag.Flag) {
		if flag.Name == name {
			set = true
		}
	})
	return set
}

func optionalFlag(flags *flag.FlagSet, name string, value *string) *string {
	if !flagSet(flags, name) {
		return nil
	}
	return value
}

func parseFlags(flags *flag.FlagSet, args []string) error {
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	return nil
}

func forwardedEnvironment(configured map[string]string) map[string]string {
	env := make(map[string]string, len(configured)+4)
	for key, value := range configured {
		env[key] = value
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENCODE_SERVER_USERNAME", "OPENCODE_SERVER_PASSWORD"} {
		if _, configured := env[key]; !configured {
			if value := os.Getenv(key); value != "" {
				env[key] = value
			}
		}
	}
	return env
}

func acquireWorkspaceLease(name string) (*registry.Lease, error) {
	lockDir, err := statePath("locks")
	if err != nil {
		return nil, err
	}
	return registry.Acquire(lockDir, name)
}

func newDocker(log *slog.Logger) (*runtime.Docker, error) {
	if err := validateDockerTopology(); err != nil {
		return nil, err
	}
	stateDirectory, err := statePath("state")
	if err != nil {
		return nil, err
	}
	return runtime.NewDocker(log, registry.NewIntentStore(stateDirectory))
}

func validateDockerTopology() error {
	host := os.Getenv(client.EnvOverrideHost)
	if host == "" {
		return nil
	}
	hostURL, err := client.ParseHostURL(host)
	if err != nil {
		return unsupportedDockerTopology(host, err)
	}
	if hostURL.Scheme != "unix" {
		return unsupportedDockerTopology(host, nil)
	}
	if !filepath.IsAbs(hostURL.Host) {
		return unsupportedDockerTopology(host, fmt.Errorf("Unix socket path must be absolute"))
	}
	return nil
}

func unsupportedDockerTopology(host string, cause error) error {
	reason := "only local Unix socket endpoints are supported"
	if cause != nil {
		reason = cause.Error()
	}
	return fmt.Errorf("unsupported DOCKER_HOST %q: %s; Fern requires Docker on this machine for bind mounts, loopback publication, and host-local coordination", host, reason)
}

func commandContext() (context.Context, context.CancelFunc) {
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(signalCtx, 70*time.Second)
	return ctx, func() {
		cancel()
		stop()
	}
}

func statePath(child string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".fern", child), nil
}
