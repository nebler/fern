package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/registry"
	"github.com/nebler/fern/internal/runtime"
)

// commandTimeout bounds interactive commands. It sits above the 60s OpenCode
// health wait so that probe can finish and report its own error instead of the
// context deadline masking it.
const commandTimeout = 70 * time.Second

// forwardedSecretKeys are provider and control credentials implicitly
// forwarded from the host environment when the protected environment file or
// workspace configuration does not set them explicitly.
var forwardedSecretKeys = []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENCODE_PASSWORD"}

func workspaceFlags(command string) (*flag.FlagSet, *string, *string) {
	descriptions := map[string]string{
		"down":         "Remove workspace compute while retaining session data.",
		"status":       "Show the workspace runtime state.",
		"logs":         "Stream workspace container logs.",
		"debug events": "Stream the backend activity events used by Fern.",
	}
	flags := newFlagSet(command, descriptions[command])
	name := flags.String("name", "", "workspace name")
	configPath := flags.String("config", "fern.yaml", "configuration file")
	return flags, name, configPath
}

func workspaceName(flags *flag.FlagSet, explicitName, configPath string) (string, error) {
	name := explicitName
	if !flagProvided(flags, "name") {
		var err error
		name, err = config.LoadWorkspaceName(configPath, flagProvided(flags, "config"))
		if err != nil {
			return "", err
		}
	}
	if err := config.ValidateWorkspaceName(name); err != nil {
		return "", err
	}
	return name, nil
}

func flagProvided(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(flag *flag.Flag) {
		if flag.Name == name {
			set = true
		}
	})
	return set
}

func optionalFlag(fs *flag.FlagSet, name string, value *string) *string {
	if !flagProvided(fs, name) {
		return nil
	}
	return value
}

func forwardedEnvironment(configured map[string]string) map[string]string {
	env := make(map[string]string, len(configured)+len(forwardedSecretKeys))
	for key, value := range configured {
		env[key] = value
	}
	for _, key := range forwardedSecretKeys {
		if _, configured := env[key]; !configured {
			if value := os.Getenv(key); value != "" {
				env[key] = value
			}
		}
	}
	return env
}

// readProtectedEnvironment reads the --env-file values, returning nil when no
// file was requested so callers can distinguish "no file" from "empty file".
func readProtectedEnvironment(envPath string) (map[string]string, error) {
	if envPath == "" {
		return nil, nil
	}
	return readEnvFile(envPath)
}

// loadCommandConfig owns the shared command preamble: read the protected
// environment file, load configuration against it and the working directory,
// fold the file's secrets into the workspace environment, then forward
// supported host credentials. It returns the values so callers can reuse them.
func loadCommandConfig(configPath string, configRequired bool, envPath string, overrides config.Overrides) (config.Config, map[string]string, error) {
	values, err := readProtectedEnvironment(envPath)
	if err != nil {
		return config.Config{}, nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return config.Config{}, values, err
	}
	cfg, err := config.LoadWithEnvironment(configPath, cwd, configRequired, overrides, values)
	if err != nil {
		return config.Config{}, values, err
	}
	cfg.Workspace.Env = finalizeWorkspaceEnvironment(cfg.Workspace.Env, values)
	return cfg, values, nil
}

// finalizeWorkspaceEnvironment merges protected environment-file secrets under
// the configured workspace environment, then layers host-forwarded secrets on
// top. A nil values map means no file was requested and skips the merge, which
// keeps an empty file indistinguishable from no file for configured keys.
func finalizeWorkspaceEnvironment(env, values map[string]string) map[string]string {
	if values != nil {
		env = mergeWorkspaceEnvironment(env, values)
	}
	return forwardedEnvironment(env)
}

func acquireWorkspaceLease(name string) (*registry.Lease, error) {
	lockDir, err := statePath("locks")
	if err != nil {
		return nil, err
	}
	return registry.Acquire(lockDir, name)
}

func newDocker(log *slog.Logger) (*runtime.Docker, error) {
	return newDockerWithSuspend(log, runtime.SuspendStop)
}

// newDockerWithSuspend selects the idle suspension mechanism. Only the
// supervisor (fern up) suspends compute; diagnostic commands use the default.
func newDockerWithSuspend(log *slog.Logger, suspend runtime.SuspendKind) (*runtime.Docker, error) {
	if err := validateDockerTopology(); err != nil {
		return nil, err
	}
	stateDirectory, err := statePath("state")
	if err != nil {
		return nil, err
	}
	return runtime.NewDocker(log, registry.NewIntentStore(stateDirectory), suspend)
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
		return unsupportedDockerTopology(host, errors.New("Unix socket path must be absolute"))
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
	ctx, cancel := context.WithTimeout(signalCtx, commandTimeout)
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
