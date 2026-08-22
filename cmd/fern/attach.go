package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/nebler/fern/internal/config"
)

func runAttach(args []string) error {
	flags := newFlagSet("attach", "Open the official client through the Fern proxy.")
	configPath := flags.String("config", "fern.yaml", "configuration file")
	envPath := flags.String("env-file", "", "protected environment file")
	listenAddress := flags.String("listen", "", "operator proxy listen address")
	clientOrigin := flags.String("url", "", "explicit loopback OpenCode server origin")
	if err := parseFlags(flags, args); err != nil {
		return err
	}
	var values map[string]string
	if *envPath != "" {
		var err error
		values, err = readEnvFile(*envPath)
		if err != nil {
			return err
		}
	}
	client, err := config.LoadAttachWithEnvironment(*configPath, flagSet(flags, "config"), optionalFlag(flags, "listen", listenAddress), environmentLookup(values))
	if err != nil {
		return err
	}
	client.Env = mergeWorkspaceEnvironment(client.Env, values)
	target, err := attachTarget(optionalFlag(flags, "url", clientOrigin), client.Listen)
	if err != nil {
		return err
	}
	executableName, commandArgs := attachCommand(target)
	executable, err := exec.LookPath(executableName)
	if err != nil {
		return fmt.Errorf("%s is not installed or not in PATH; install it from https://opencode.ai", executableName)
	}
	command := exec.Command(executable, commandArgs...)
	command.Env = attachEnvironment(os.Environ(), forwardedEnvironment(client.Env))
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() && (status.Signal() == os.Interrupt || status.Signal() == syscall.SIGTERM) {
				return nil
			}
			if code := exitError.ExitCode(); code > 0 {
				return commandExitError{err: fmt.Errorf("%s client: %w", executableName, err), code: code}
			}
		}
		return fmt.Errorf("%s client: %w", executableName, err)
	}
	return nil
}

func attachCommand(target string) (string, []string) {
	return "opencode2", []string{"--server", target}
}

func attachTarget(explicitOrigin *string, listenAddress string) (string, error) {
	if explicitOrigin == nil {
		return attachURL(listenAddress)
	}

	origin, err := url.Parse(*explicitOrigin)
	if err != nil {
		return "", fmt.Errorf("invalid attach URL %q: %w", *explicitOrigin, err)
	}
	if origin.Scheme != "http" && origin.Scheme != "https" {
		return "", fmt.Errorf("invalid attach URL %q: scheme must be http or https", *explicitOrigin)
	}
	if origin.Host == "" || origin.Hostname() == "" || origin.Opaque != "" {
		return "", fmt.Errorf("invalid attach URL %q: host is required", *explicitOrigin)
	}
	if origin.User != nil {
		return "", fmt.Errorf("invalid attach URL %q: user information is not allowed", *explicitOrigin)
	}
	ip := net.ParseIP(origin.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("invalid attach URL %q: host must be a numeric loopback IP", *explicitOrigin)
	}
	if strings.Contains(*explicitOrigin, "#") {
		return "", fmt.Errorf("invalid attach URL %q: fragments are not allowed", *explicitOrigin)
	}
	if origin.RawQuery != "" || origin.ForceQuery {
		return "", fmt.Errorf("invalid attach URL %q: query parameters are not allowed", *explicitOrigin)
	}
	if path := origin.EscapedPath(); path != "" && path != "/" {
		return "", fmt.Errorf("invalid attach URL %q: path must be root", *explicitOrigin)
	}

	return (&url.URL{Scheme: origin.Scheme, Host: origin.Host}).String(), nil
}

func attachURL(address string) (string, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("invalid proxy listen address %q: %w", address, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return "", fmt.Errorf("invalid proxy port %q", portText)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("invalid operator listen address %q: host must be a numeric loopback IP", address)
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, portText)}).String(), nil
}

func attachEnvironment(base []string, configured map[string]string) []string {
	const password = "OPENCODE_PASSWORD"
	allowed := map[string]bool{
		"COLORTERM": true, "HOME": true, "LANG": true, "LOGNAME": true,
		"PATH": true, "SHELL": true, "TERM": true, "TMPDIR": true, "USER": true,
		"XDG_CACHE_HOME": true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true,
	}
	result := make([]string, 0, len(allowed)+1)
	for _, value := range base {
		key, _, _ := strings.Cut(value, "=")
		if allowed[key] || strings.HasPrefix(key, "LC_") {
			result = append(result, value)
		}
	}
	if value := configured[password]; value != "" {
		result = append(result, password+"="+value)
	}
	return result
}
