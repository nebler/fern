package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nebler/fern/internal/backgroundopencode"
	"github.com/nebler/fern/internal/gitref"
)

var workspaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Validate requires a complete, installed Background Run execution binding.
func Validate(config Config) error {
	return validateBackground(config, false)
}

// ValidateBootstrap accepts an omitted App installation ID so Fern can serve
// its control plane (devices, plugin authorization) before GitHub setup is
// complete. It does not authorize task
// composition; callers must still use Validate before execution.
func ValidateBootstrap(config Config) error {
	return validateBackground(config, true)
}

func validateBackground(config Config, allowPendingInstallation bool) error {
	if err := ValidateWorkspace(config); err != nil {
		return err
	}
	if config.Control.Password == "" {
		return errors.New("FERN_CONTROL_PASSWORD is required through control.password")
	}
	if len(config.Control.Password) < 32 {
		return errors.New("FERN_CONTROL_PASSWORD must be at least 32 characters")
	}
	if config.Workspace.GitHub.InstallationID < 0 || !allowPendingInstallation && config.Workspace.GitHub.InstallationID == 0 {
		return errors.New("background runs require a positive workspace.github.installationId")
	}
	if err := validateTasks(config); err != nil {
		return err
	}
	if !allowPendingInstallation && config.Tasks.RuntimeStorageRoot == "" {
		return errors.New("tasks.runtimeStorageRoot is required for execution; provision a Linux XFS project-quota root")
	}
	if config.Tasks.BackgroundImage == "" || config.Tasks.BackgroundImageID == "" || config.Tasks.BackgroundRoute == nil {
		return errors.New("a qualified Background Run image and route are required")
	}
	if err := validateListen("proxy.listen", config.Listen); err != nil {
		return err
	}
	if err := validateListen("proxy.operatorListen", config.OperatorListen); err != nil {
		return err
	}
	if sameListenPort(config.Listen, config.OperatorListen) {
		return errors.New("proxy.listen and proxy.operatorListen must use different ports")
	}
	if _, err := ParseRemoteOrigin(config.RemoteOrigin); err != nil {
		return err
	}
	return nil
}

// ValidateWorkspace checks the current repository identity and path for offline
// operations such as backups, without requiring live control credentials.
func ValidateWorkspace(config Config) error {
	workspace := config.Workspace
	if !workspaceNamePattern.MatchString(workspace.Name) {
		return fmt.Errorf("invalid workspace name %q", workspace.Name)
	}
	if workspace.GitHub.Repository.ID <= 0 {
		return errors.New("workspace GitHub repository ID must be positive")
	}
	if err := ValidateGitHubRepositoryFullName(workspace.GitHub.Repository.FullName); err != nil {
		return fmt.Errorf("invalid workspace GitHub repository full name: %w", err)
	}
	stat, err := os.Stat(workspace.Repo)
	if err != nil {
		return fmt.Errorf("inspect repository path %q: %w", workspace.Repo, err)
	}
	if !stat.IsDir() {
		return fmt.Errorf("repository path %q is not a directory", workspace.Repo)
	}
	return nil
}

func validateTasks(config Config) error {
	if root := config.Tasks.RuntimeStorageRoot; root != "" {
		if !validTaskText(root, 1, 4096) || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
			return errors.New("tasks.runtimeStorageRoot must be an absolute, cleaned, non-root path")
		}
	}
	if !validTaskText(config.Tasks.Agent, 1, 128) {
		return errors.New("tasks.agent must be 1-128 bytes of valid text")
	}
	if !validTaskText(config.Tasks.Model.Provider, 1, 128) {
		return errors.New("tasks.model.provider must be 1-128 bytes of valid text")
	}
	if !validTaskText(config.Tasks.Model.ID, 1, 256) {
		return errors.New("tasks.model.id must be 1-256 bytes of valid text")
	}
	if config.Tasks.RunTimeout < time.Minute || config.Tasks.RunTimeout > 24*time.Hour {
		return errors.New("tasks.runTimeout must be between 1m and 24h")
	}
	if config.Tasks.BackgroundImage != "" && (!validTaskText(config.Tasks.BackgroundImage, 1, 256) || strings.TrimSpace(config.Tasks.BackgroundImage) != config.Tasks.BackgroundImage) {
		return errors.New("tasks.backgroundImage must be an exact nonempty image reference of at most 256 bytes")
	}
	if (config.Tasks.BackgroundImage == "") != (config.Tasks.BackgroundImageID == "") {
		return errors.New("tasks.backgroundImage and tasks.backgroundImageID must be configured together")
	}
	if config.Tasks.BackgroundImageID != "" && !validCanonicalImageID(config.Tasks.BackgroundImageID) {
		return errors.New("tasks.backgroundImageID must be a canonical sha256 image ID")
	}
	if config.Tasks.BackgroundImage == "" && config.Tasks.BackgroundRoute != nil {
		return errors.New("tasks.backgroundRoute is forbidden without tasks.backgroundImage")
	}
	if config.Tasks.BackgroundImage != "" && config.Tasks.BackgroundRoute == nil {
		return errors.New("tasks.backgroundRoute is required with tasks.backgroundImage")
	}
	if route := config.Tasks.BackgroundRoute; route != nil {
		if err := validateListen("tasks.backgroundRoute.listen", route.Listen); err != nil {
			return err
		}
		if sameListenPort(route.Listen, config.Listen) || sameListenPort(route.Listen, config.OperatorListen) {
			return errors.New("tasks.backgroundRoute.listen must use a distinct port")
		}
		origin, err := ParseRemoteOrigin(route.Origin)
		if err != nil {
			return fmt.Errorf("invalid tasks.backgroundRoute.origin: %w", err)
		}
		if origin == "" {
			return errors.New("invalid tasks.backgroundRoute.origin: a private HTTPS origin is required")
		}
		remote, err := url.Parse(config.RemoteOrigin)
		if err != nil || config.RemoteOrigin == "" {
			return errors.New("proxy.remoteOrigin is required with tasks.backgroundRoute")
		}
		parsed, _ := url.Parse(origin)
		if _, err := backgroundopencode.ParseTrustedOrigin(origin); err != nil {
			return errors.New("tasks.backgroundRoute.origin must be a canonical non-loopback private HTTPS origin supported by the pinned OpenCode UI")
		}
		remotePort := remote.Port()
		if remotePort == "" {
			remotePort = "443"
		}
		if parsed.Hostname() != remote.Hostname() || parsed.Port() == "" || parsed.Port() == "443" || parsed.Port() == remotePort {
			return errors.New("tasks.backgroundRoute.origin must use the proxy.remoteOrigin hostname and an explicit non-443 port")
		}
	}
	return nil
}

func validCanonicalImageID(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[7:] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validTaskText(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

// ParseRemoteOrigin validates the single canonical spelling accepted for a
// remotely published Fern origin. An empty value preserves local-only mode.
func ParseRemoteOrigin(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" {
		return "", errors.New("must be an absolute HTTPS root origin")
	}
	if parsed.Path != "" || parsed.RawPath != "" {
		return "", errors.New("must not contain a path, including a trailing slash")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(value, "#") {
		return "", errors.New("must not contain a query or fragment")
	}
	hostname := parsed.Hostname()
	if hostname == "" || strings.HasSuffix(hostname, ".") {
		return "", errors.New("must contain a canonical DNS name or IP address")
	}
	canonicalHost := ""
	if ip := net.ParseIP(hostname); ip != nil {
		canonicalHost = ip.String()
		if strings.Contains(canonicalHost, ":") {
			canonicalHost = "[" + canonicalHost + "]"
		}
	} else {
		if !validDNSName(hostname) {
			return "", errors.New("must contain a valid DNS name or IP address")
		}
		canonicalHost = strings.ToLower(hostname)
	}
	port := parsed.Port()
	if port != "" {
		number, portErr := strconv.Atoi(port)
		if portErr != nil || number <= 0 || number > 65535 {
			return "", errors.New("contains an invalid port")
		}
		if number == 443 {
			return "", errors.New("must omit the default HTTPS port 443")
		}
		if port != strconv.Itoa(number) {
			return "", errors.New("port is not canonical")
		}
		canonicalHost += ":" + port
	}
	canonical := "https://" + canonicalHost
	if value != canonical {
		return "", fmt.Errorf("must use canonical spelling %q", canonical)
	}
	return canonical, nil
}

func validDNSName(host string) bool {
	if len(host) > 253 || host != strings.ToLower(host) {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if character != '-' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
				return false
			}
		}
	}
	return true
}

func sameListenPort(first, second string) bool {
	_, firstPort, firstErr := net.SplitHostPort(first)
	_, secondPort, secondErr := net.SplitHostPort(second)
	if firstErr != nil || secondErr != nil {
		return false
	}
	firstNumber, firstErr := strconv.Atoi(firstPort)
	secondNumber, secondErr := strconv.Atoi(secondPort)
	return firstErr == nil && secondErr == nil && firstNumber == secondNumber
}

func validateListen(field, address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid %s address %q: %w", field, address, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("invalid %s port %q", field, portText)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%s must use a numeric loopback IP", field)
	}
	return nil
}

// ValidateGitHubRepositoryFullName accepts only canonical GitHub
// OWNER/REPOSITORY full names. It delegates to the shared gitref rules so
// configuration cannot drift from the repository identity validators.
func ValidateGitHubRepositoryFullName(value string) error {
	return gitref.ValidateOwnerRepo(value)
}
