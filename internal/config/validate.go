package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nebler/fern/internal/gitref"
)

var workspaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Validate enforces the full supervisor gauntlet: workspace shape, credential
// presence and separation, task policy bounds, listeners, and remote origin.
func Validate(config Config) error {
	if err := ValidateWorkspace(config); err != nil {
		return err
	}
	if config.Workspace.Env["OPENCODE_PASSWORD"] == "" {
		return errors.New("OPENCODE_PASSWORD is required")
	}
	if config.Control.Password == "" {
		return errors.New("FERN_CONTROL_PASSWORD is required through control.password")
	}
	if len(config.Control.Password) < 32 {
		return errors.New("FERN_CONTROL_PASSWORD must be at least 32 characters")
	}
	if config.Control.Password == config.Workspace.Env["OPENCODE_PASSWORD"] {
		return errors.New("Fern control and OpenCode passwords must be different")
	}
	for key, value := range config.Workspace.Env {
		if value == config.Control.Password {
			return fmt.Errorf("workspace.env.%s duplicates the host-only Fern control credential", key)
		}
	}
	if err := validateTasks(config); err != nil {
		return err
	}
	if config.IdleAfter <= 0 {
		return fmt.Errorf("idle duration must be positive")
	}
	switch config.IdleMode {
	case IdleModeStop, IdleModeFreeze:
	default:
		return fmt.Errorf("idle.mode must be %q or %q, got %q", IdleModeStop, IdleModeFreeze, config.IdleMode)
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
		return fmt.Errorf("invalid proxy.remoteOrigin: %w", err)
	}
	return nil
}

func validateTasks(config Config) error {
	if config.Tasks == nil {
		return nil
	}
	if config.Workspace.GitHub == nil {
		return errors.New("workspace.github is required when tasks are configured")
	}
	if config.Workspace.GitHub.Mode == GitHubModeGitHubAppBroker && config.Workspace.GitHub.InstallationID <= 0 {
		return errors.New("workspace.github.installationId must be positive in github-app-broker mode")
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
	if config.Tasks.AttemptTimeout < time.Minute || config.Tasks.AttemptTimeout > 24*time.Hour {
		return errors.New("tasks.attemptTimeout must be between 1m and 24h")
	}
	if config.Tasks.LeaseDuration <= 0 || config.Tasks.LeaseDuration > 5*time.Minute {
		return errors.New("tasks.leaseDuration must be greater than zero and at most 5m")
	}
	if config.Tasks.LeaseDuration > config.Tasks.AttemptTimeout {
		return errors.New("tasks.leaseDuration must not exceed tasks.attemptTimeout")
	}
	if config.Tasks.Budget.MaxTurns < 1 || config.Tasks.Budget.MaxTurns > 1000 {
		return errors.New("tasks.budget.maxTurns must be between 1 and 1000")
	}
	if verification := config.Tasks.Verification; verification != nil {
		if !validPolicyName(verification.CheckName) {
			return errors.New("tasks.verification.checkName must be 1-64 lowercase policy characters")
		}
		if len(verification.Argv) < 1 || len(verification.Argv) > 256 || !filepath.IsAbs(verification.Argv[0]) || filepath.Clean(verification.Argv[0]) != verification.Argv[0] {
			return errors.New("tasks.verification.argv must start with an absolute clean executable and contain at most 256 arguments")
		}
		if executable, err := filepath.EvalSymlinks(verification.Argv[0]); err == nil && pathWithin(config.Workspace.Repo, executable) {
			return errors.New("tasks.verification executable must be outside the writable workspace repository")
		}
		argumentBytes := 0
		for _, argument := range verification.Argv {
			argumentBytes += len(argument)
			if strings.IndexByte(argument, 0) >= 0 {
				return errors.New("tasks.verification.argv must not contain NUL")
			}
		}
		if argumentBytes > 32<<10 {
			return errors.New("tasks.verification.argv exceeds 32768 bytes")
		}
		if verification.WorkingDirectory != "" && verification.WorkingDirectory != "." &&
			(filepath.IsAbs(verification.WorkingDirectory) || filepath.Clean(verification.WorkingDirectory) != verification.WorkingDirectory || verification.WorkingDirectory == ".." || strings.HasPrefix(verification.WorkingDirectory, ".."+string(filepath.Separator))) {
			return errors.New("tasks.verification.workingDirectory must remain within the repository")
		}
		if verification.Timeout <= 0 || verification.Timeout > time.Hour {
			return errors.New("tasks.verification.timeout must be positive and at most 1h")
		}
		if verification.OutputBytes < 1 || verification.OutputBytes > 1<<20 {
			return errors.New("tasks.verification.outputBytes must be between 1 and 1048576")
		}
		if len(verification.Environment) > 64 {
			return errors.New("tasks.verification.environment has too many entries")
		}
		environmentBytes := 0
		for name, value := range verification.Environment {
			environmentBytes += len(name) + len(value) + 1
			if !validEnvironmentName(name) || strings.IndexByte(value, 0) >= 0 {
				return errors.New("tasks.verification.environment contains an invalid entry")
			}
			switch name {
			case "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "GIT_TERMINAL_PROMPT", "HOME", "LANG", "LC_ALL", "PATH":
				return fmt.Errorf("tasks.verification.environment.%s is reserved by the runner", name)
			}
		}
		if environmentBytes > 32<<10 {
			return errors.New("tasks.verification.environment exceeds 32768 bytes")
		}
	}
	return nil
}

func pathWithin(parent, candidate string) bool {
	parent, parentErr := filepath.EvalSymlinks(parent)
	if parentErr != nil {
		return false
	}
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validPolicyName(value string) bool {
	if len(value) < 1 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range []byte(value) {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func validEnvironmentName(value string) bool {
	if value == "" || (value[0] < 'A' || value[0] > 'Z') && (value[0] < 'a' || value[0] > 'z') && value[0] != '_' {
		return false
	}
	for _, character := range []byte(value[1:]) {
		if (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
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

// ValidateWorkspace enforces the workspace section: name pattern, image,
// memory, GitHub binding, repository path, and forwarded environment rules.
func ValidateWorkspace(config Config) error {
	if !workspaceNamePattern.MatchString(config.Workspace.Name) {
		return fmt.Errorf("invalid workspace name %q", config.Workspace.Name)
	}
	if strings.TrimSpace(config.Workspace.Image) == "" {
		return fmt.Errorf("workspace image is required")
	}
	if _, err := ParseMemoryBytes(config.Workspace.Memory); err != nil {
		return err
	}
	if config.Workspace.GitHub != nil {
		github := config.Workspace.GitHub
		if github.Mode != GitHubModeWorkspaceGH && github.Mode != GitHubModeGitHubAppBroker {
			return errors.New("workspace GitHub mode must be workspace-gh or github-app-broker")
		}
		if github.Hostname != "github.com" {
			return errors.New("workspace GitHub hostname must be github.com")
		}
		if github.Mode == GitHubModeWorkspaceGH && github.InstallationID != 0 {
			return errors.New("workspace GitHub installation ID is forbidden in workspace-gh mode")
		}
		if github.Mode == GitHubModeGitHubAppBroker && github.InstallationID <= 0 {
			return errors.New("workspace GitHub installation ID must be positive in github-app-broker mode")
		}
		if config.Workspace.GitHub.Repository.ID <= 0 {
			return errors.New("workspace GitHub repository ID must be positive")
		}
		if err := ValidateGitHubRepositoryFullName(config.Workspace.GitHub.Repository.FullName); err != nil {
			return fmt.Errorf("invalid workspace GitHub repository full name: %w", err)
		}
	}
	stat, err := os.Stat(config.Workspace.Repo)
	if err != nil {
		return fmt.Errorf("inspect repository path %q: %w", config.Workspace.Repo, err)
	}
	if !stat.IsDir() {
		return fmt.Errorf("repository path %q is not a directory", config.Workspace.Repo)
	}
	if password, exists := config.Workspace.Env["OPENCODE_PASSWORD"]; exists && password == "" {
		return fmt.Errorf("OPENCODE_PASSWORD must not be explicitly empty")
	}
	for key := range config.Workspace.Env {
		switch key {
		case "FERN_CONTROL_PASSWORD", "FERN_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", "GH_CONFIG_DIR":
			return fmt.Errorf("%s is host-only and cannot be forwarded to the workspace", key)
		}
		if key == "" || strings.ContainsAny(key, "=\x00\r\n") {
			return fmt.Errorf("invalid environment key %q", key)
		}
	}
	for key, value := range config.Workspace.Env {
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("environment value %q contains NUL", key)
		}
	}
	return nil
}

// ValidateWorkspaceName accepts only the printable identifier pattern used for
// workspace directory and container names.
func ValidateWorkspaceName(name string) error {
	if !workspaceNamePattern.MatchString(name) {
		return fmt.Errorf("invalid workspace name %q", name)
	}
	return nil
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

// ParseMemoryBytes preserves decimal and binary unit semantics. A bare value is
// interpreted as MiB for CLI compatibility.
func ParseMemoryBytes(value string) (int64, error) {
	normalized := strings.TrimSpace(strings.ToLower(value))
	units := []struct {
		suffix     string
		multiplier int64
	}{
		{"gib", 1024 * 1024 * 1024}, {"gb", 1000 * 1000 * 1000},
		{"gi", 1024 * 1024 * 1024}, {"g", 1000 * 1000 * 1000},
		{"mib", 1024 * 1024}, {"mb", 1000 * 1000},
		{"mi", 1024 * 1024}, {"m", 1000 * 1000},
	}
	for _, unit := range units {
		if strings.HasSuffix(normalized, unit.suffix) {
			amount, err := positiveInt(strings.TrimSpace(strings.TrimSuffix(normalized, unit.suffix)), value)
			if err != nil {
				return 0, err
			}
			if amount > math.MaxInt64/unit.multiplier {
				return 0, fmt.Errorf("memory %q overflows bytes", value)
			}
			return amount * unit.multiplier, nil
		}
	}
	amount, err := positiveInt(normalized, value)
	if err != nil {
		return 0, err
	}
	if amount > math.MaxInt64/(1024*1024) {
		return 0, fmt.Errorf("memory %q overflows bytes", value)
	}
	return amount * 1024 * 1024, nil
}

func positiveInt(number, original string) (int64, error) {
	amount, err := strconv.ParseInt(number, 10, 64)
	if err != nil || amount <= 0 {
		return 0, fmt.Errorf("invalid memory %q", original)
	}
	return amount, nil
}

// ValidateGitHubRepositoryFullName accepts only canonical GitHub
// OWNER/REPOSITORY full names. It delegates to the shared gitref rules so
// configuration cannot drift from the publication validators.
func ValidateGitHubRepositoryFullName(value string) error {
	return gitref.ValidateOwnerRepo(value)
}
