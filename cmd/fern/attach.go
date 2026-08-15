package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/nebler/fern/internal/config"
)

func runAttach(args []string) error {
	flags := flag.NewFlagSet("attach", flag.ContinueOnError)
	configPath := flags.String("config", "fern.yaml", "configuration file")
	listenAddress := flags.String("listen", "", "proxy listen address")
	clientOrigin := flags.String("url", "", "explicit OpenCode server origin")
	if err := parseFlags(flags, args); err != nil {
		return err
	}
	client, err := config.LoadAttach(*configPath, flagSet(flags, "config"), optionalFlag(flags, "listen", listenAddress))
	if err != nil {
		return err
	}
	target, err := attachTarget(optionalFlag(flags, "url", clientOrigin), client.Listen)
	if err != nil {
		return err
	}
	executable, err := exec.LookPath("opencode")
	if err != nil {
		return errors.New("opencode is not installed or not in PATH; install it from https://opencode.ai")
	}
	command := exec.Command(executable, "attach", target)
	command.Env = attachEnvironment(os.Environ(), forwardedEnvironment(client.Env))
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("opencode attach: %w", err)
	}
	return nil
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
	if host == "" || ip != nil && ip.IsUnspecified() {
		if ip != nil && ip.To4() == nil {
			host = "::1"
		} else {
			host = "127.0.0.1"
		}
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, portText)}).String(), nil
}

func attachEnvironment(base []string, configured map[string]string) []string {
	const username = "OPENCODE_SERVER_USERNAME"
	const password = "OPENCODE_SERVER_PASSWORD"
	result := make([]string, 0, len(base)+2)
	for _, value := range base {
		key, _, _ := strings.Cut(value, "=")
		if key != username && key != password {
			result = append(result, value)
		}
	}
	if value := configured[username]; value != "" {
		result = append(result, username+"="+value)
	}
	if value := configured[password]; value != "" {
		result = append(result, password+"="+value)
	}
	return result
}
