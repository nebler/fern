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

// addConfigFlags registers the --config and --env-file flags every
// host-side command shares, defaulting to the files fern init writes.
func addConfigFlags(fs *flag.FlagSet) (configPath, envPath *string) {
	return fs.String("config", "fern.yaml", "configuration file"),
		fs.String("env-file", "fern.env", "protected environment file holding FERN_CONTROL_PASSWORD")
}

// loadCommandConfig owns the shared command preamble: read the protected
// environment file, then load configuration against it without copying
// host-only secrets into run state.
func loadCommandConfig(configPath, envPath string) (config.Config, error) {
	values, err := readEnvFile(envPath)
	if err != nil {
		return config.Config{}, err
	}
	return config.Load(configPath, values)
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
