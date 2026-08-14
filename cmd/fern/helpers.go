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

func clientEnvironment(client config.Client) map[string]string {
	env := make(map[string]string, 2)
	if client.Username != "" {
		env["OPENCODE_SERVER_USERNAME"] = client.Username
	}
	if client.Password != "" {
		env["OPENCODE_SERVER_PASSWORD"] = client.Password
	}
	return env
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
	stateDirectory, err := statePath("state")
	if err != nil {
		return nil, err
	}
	return runtime.NewDocker(log, registry.NewIntentStore(stateDirectory))
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
