package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/runtime"
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
	protocol := client.OpenCode
	if protocol == config.OpenCodeAuto {
		env := forwardedEnvironmentFor(client.OpenCode, client.Env)
		detected, err := runtime.WaitHealthyURL(context.Background(), target, runtime.ServerAuth{
			Protocol: runtime.ProtocolAuto, Username: env["OPENCODE_SERVER_USERNAME"],
			Password: env["OPENCODE_SERVER_PASSWORD"], V2Password: env["OPENCODE_PASSWORD"],
		}, runtime.ProtocolAuto, 60*time.Second)
		if err != nil {
			return fmt.Errorf("detect OpenCode protocol: %w", err)
		}
		protocol = config.OpenCodeProtocol(detected)
	}
	executableName, commandArgs := attachCommand(protocol, target)
	executable, err := exec.LookPath(executableName)
	if err != nil {
		return fmt.Errorf("%s is not installed or not in PATH; install it from https://opencode.ai", executableName)
	}
	command := exec.Command(executable, commandArgs...)
	command.Env = attachEnvironmentFor(protocol, os.Environ(), forwardedEnvironmentFor(protocol, client.Env))
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s client: %w", executableName, err)
	}
	return nil
}

func attachCommand(protocol config.OpenCodeProtocol, target string) (string, []string) {
	if protocol == config.OpenCodeV2 {
		return "opencode2", []string{"--server", target}
	}
	return "opencode", []string{"attach", target}
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
	return attachEnvironmentFor(config.OpenCodeV1, base, configured)
}

func attachEnvironmentFor(protocol config.OpenCodeProtocol, base []string, configured map[string]string) []string {
	const username = "OPENCODE_SERVER_USERNAME"
	const password = "OPENCODE_SERVER_PASSWORD"
	const v2Password = "OPENCODE_PASSWORD"
	result := make([]string, 0, len(base)+3)
	for _, value := range base {
		key, _, _ := strings.Cut(value, "=")
		if key != username && key != password && key != v2Password {
			result = append(result, value)
		}
	}
	if protocol == config.OpenCodeV2 {
		if value := configured[v2Password]; value != "" {
			result = append(result, v2Password+"="+value)
		}
		return result
	}
	if value := configured[username]; value != "" {
		result = append(result, username+"="+value)
	}
	if value := configured[password]; value != "" {
		result = append(result, password+"="+value)
	}
	if protocol == config.OpenCodeAuto {
		if value := configured[v2Password]; value != "" {
			result = append(result, v2Password+"="+value)
		}
	}
	return result
}
