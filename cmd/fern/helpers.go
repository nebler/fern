package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/docker/docker/client"
	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/hostlease"
)

func loopbackURL(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("invalid loopback address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	number, portErr := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || portErr != nil || number < 1 || number > 65535 {
		return "", fmt.Errorf("invalid loopback address %q", address)
	}
	return (&url.URL{Scheme: "http", Host: address}).String(), nil
}

func workspaceFlags(command string) (*flag.FlagSet, *string, *string) {
	descriptions := map[string]string{"debug quarantine-publications": "Quarantine unresolved retired publication records."}
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
// expand protected values without copying host-only secrets into run state.
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
	return cfg, values, nil
}

func loadBackgroundCommandConfig(configPath string, configRequired bool, envPath string, overrides config.BackgroundOverrides) (config.BackgroundConfig, error) {
	values, err := readProtectedEnvironment(envPath)
	if err != nil {
		return config.BackgroundConfig{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return config.BackgroundConfig{}, err
	}
	return config.LoadBackgroundWithEnvironment(configPath, cwd, configRequired, overrides, values)
}

func acquireHostLease(name string) (*hostlease.Lease, error) {
	lockDir, err := statePath("locks")
	if err != nil {
		return nil, err
	}
	return hostlease.Acquire(lockDir, name)
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
	return fmt.Errorf("unsupported DOCKER_HOST %q: %s; Fern requires local Docker for disposable bind mounts and loopback routing", host, reason)
}

func statePath(child string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".fern", child), nil
}
