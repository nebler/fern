package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nebler/fern/internal/config"
	qrcode "github.com/skip2/go-qrcode"
)

type doctorCheck struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Summary     string `json:"summary"`
	Remediation string `json:"remediation,omitempty"`
}

type doctorReport struct {
	Ready    bool          `json:"ready"`
	PhoneURL string        `json:"phoneUrl,omitempty"`
	Checks   []doctorCheck `json:"checks"`
}

func runDoctor(args []string) error {
	flags := newFlagSet("doctor", "Verify Fern and the private phone-demo path.")
	configPath := flags.String("config", "fern.yaml", "configuration file")
	envPath := flags.String("env-file", "fern.env", "protected environment file")
	phone := flags.Bool("phone", false, "require and verify a Tailscale HTTPS route")
	fieldDemo := flags.Bool("field-demo", false, "require all locally verifiable field-demo prerequisites")
	remoteURL := flags.String("url", "", "explicit private HTTPS origin")
	jsonOutput := flags.Bool("json", false, "output a stable JSON report")
	qr := flags.Bool("qr", true, "print a terminal QR code for a ready phone URL")
	if err := parseFlags(flags, args); err != nil {
		return err
	}
	report := diagnose(*configPath, *envPath, *phone || *fieldDemo, *fieldDemo, *remoteURL)
	if *jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(report); err != nil {
			return err
		}
	} else {
		writeDoctorReport(os.Stdout, report, *qr)
	}
	if !report.Ready {
		return fmt.Errorf("requested Fern readiness checks failed; resolve failed checks above")
	}
	return nil
}

func diagnose(configPath, envPath string, phone, fieldDemo bool, explicitURL string) doctorReport {
	report := doctorReport{Ready: true}
	add := func(id, status, summary, remediation string) {
		report.Checks = append(report.Checks, doctorCheck{ID: id, Status: status, Summary: summary, Remediation: remediation})
		if status == "fail" {
			report.Ready = false
		}
	}
	values, err := readEnvFile(envPath)
	if err != nil {
		add("secrets", "fail", err.Error(), "Run fern init or pass --env-file.")
		return report
	}
	add("secrets", "pass", "protected environment file loaded", "")
	cwd, err := os.Getwd()
	if err != nil {
		add("config", "fail", err.Error(), "Run doctor from an accessible directory.")
		return report
	}
	cfg, err := config.LoadWithEnvironment(configPath, cwd, true, config.Overrides{}, values)
	if err != nil {
		add("config", "fail", err.Error(), "Fix the strict Fern configuration.")
		return report
	}
	cfg.Workspace.Env = forwardedEnvironment(mergeWorkspaceEnvironment(cfg.Workspace.Env, values))
	if err := config.Validate(cfg); err != nil {
		add("config", "fail", err.Error(), "Fix the Fern configuration or secret file.")
		return report
	}
	add("config", "pass", "configuration is valid", "")
	if info, err := os.Stat(filepath.Join(cfg.Workspace.Repo, ".git")); err != nil || !info.IsDir() {
		add("repository", "fail", "workspace is not a standard Git checkout", "Use a repository with a .git directory.")
	} else {
		add("repository", "pass", "Git repository is available", "")
	}
	if err := validateDockerTopology(); err != nil {
		add("docker", "fail", err.Error(), "Use the local Docker Unix socket.")
	} else if err := checkCommand(5*time.Second, "docker", "info"); err != nil {
		add("docker", "fail", err.Error(), "Start Docker and grant this user access.")
	} else {
		add("docker", "pass", "local Docker daemon is reachable", "")
		if err := checkCommand(5*time.Second, "docker", "image", "inspect", cfg.Workspace.Image); err != nil {
			add("image", "fail", "workspace image is unavailable", "Run make image or pull the configured image.")
		} else {
			add("image", "pass", "workspace image is available", "")
		}
	}
	if err := checkCommand(5*time.Second, "gh", "auth", "status", "--hostname", "github.com"); err != nil {
		status := "warn"
		if fieldDemo {
			status = "fail"
		}
		add("github", status, "GitHub CLI is not authenticated", "Run gh auth login --hostname github.com before publishing a PR.")
	} else {
		add("github", "pass", "GitHub CLI authentication is available on the host", "")
	}
	localURL, err := attachURL(cfg.OperatorListen)
	if err != nil {
		add("gateway", "fail", err.Error(), "Fix proxy.operatorListen.")
		if fieldDemo {
			add("provider", "fail", "OpenCode provider availability could not be checked", "Fix proxy.operatorListen, start Fern, connect a provider in OpenCode, and retry.")
		}
	} else if err := checkReady(localURL, cfg.Control.Password); err != nil {
		add("gateway", "fail", "local Fern gateway is not ready", "Start fern up with the same --config and --env-file.")
		if fieldDemo {
			add("provider", "fail", "OpenCode provider availability could not be checked", "Start Fern, connect a provider in the official OpenCode UI, and retry.")
		}
	} else {
		add("gateway", "pass", "local Fern gateway is serving", "")
		if fieldDemo {
			count, providerErr := checkProviderConnection(localURL, cfg.Workspace.Env["OPENCODE_PASSWORD"])
			if providerErr != nil {
				add("provider", "fail", providerErr.Error(), "Connect any supported provider in the official OpenCode UI, then retry.")
			} else {
				add("provider", "pass", fmt.Sprintf("OpenCode reports %d active provider(s)", count), "")
			}
		}
	}
	if fieldDemo {
		add("live-checks", "warn", "provider execution and GitHub mutation are not run by doctor", "Run the opt-in provider and disposable-repository rehearsals before the phone demo.")
	}
	if phone || explicitURL != "" {
		if cfg.RemoteOrigin == "" {
			add("tailscale", "fail", "proxy.remoteOrigin is required for phone mode", "Set proxy.remoteOrigin to the exact canonical HTTPS root origin reported for this host, then retry.")
			return report
		}
		if explicitURL != "" && explicitURL != cfg.RemoteOrigin {
			add("tailscale", "fail", "--url does not exactly match proxy.remoteOrigin", "Remove --url or pass the exact configured canonical origin.")
			return report
		}
		servedOrigin, serveErr := discoverTailscaleURL(cfg.Listen, cfg.OperatorListen)
		if serveErr != nil || servedOrigin == "" {
			add("tailscale", "fail", "no Tailscale Serve HTTPS origin was found", fmt.Sprintf("Run tailscale serve --bg http://%s, then retry.", cfg.Listen))
		} else {
			localOrigin, localErr := localTailscaleOrigin()
			if topologyErr := validatePhoneTopology(cfg.RemoteOrigin, explicitURL, servedOrigin, localOrigin, localErr); topologyErr != nil {
				add("tailscale", "fail", topologyErr.Error(), "Make proxy.remoteOrigin, the root Serve origin, and this host's tailnet HTTPS origin identical.")
				return report
			}
			code, pairErr := issuePairingCode(localURL, cfg.Control.Password)
			if pairErr != nil {
				add("pairing", "fail", "could not create a one-time phone pairing link", "Ensure the local Fern process is the current build.")
			} else if err := checkPairingPreview(cfg.RemoteOrigin, code); err != nil {
				add("pairing", "fail", err.Error(), "Check that Tailscale Serve targets proxy.listen and Fern is current.")
			} else if err := checkRemoteCredentialRejected(cfg.RemoteOrigin, cfg.Workspace.Env["OPENCODE_PASSWORD"]); err != nil {
				add("phone", "fail", err.Error(), "Serve only proxy.listen; never proxy.operatorListen or the OpenCode backend.")
			} else {
				report.PhoneURL = cfg.RemoteOrigin + "/fern/pair?code=" + url.QueryEscape(code)
				add("tailscale", "pass", "private HTTPS route reaches Fern", "")
				add("pairing", "pass", "one-time phone pairing link created", "")
				add("phone", "pass", "phone-demo transport is ready", "")
			}
		}
	}
	return report
}

func validatePhoneTopology(configured, asserted, served, local string, localErr error) error {
	if configured == "" {
		return fmt.Errorf("proxy.remoteOrigin is required for phone mode")
	}
	if asserted != "" && asserted != configured {
		return fmt.Errorf("--url does not exactly match proxy.remoteOrigin")
	}
	if served != configured {
		return fmt.Errorf("Tailscale Serve origin %q does not exactly match proxy.remoteOrigin %q", served, configured)
	}
	if localErr != nil {
		return fmt.Errorf("discover this host's tailnet origin: %w", localErr)
	}
	if local != configured {
		return fmt.Errorf("local tailnet origin %q does not exactly match proxy.remoteOrigin %q", local, configured)
	}
	return nil
}

func checkCommand(timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	command.WaitDelay = time.Second
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s timed out", name)
		}
		return fmt.Errorf("%s check failed", name)
	}
	return nil
}

func checkReady(origin, password string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(origin, "/")+"/fern/ready", nil)
	if err != nil {
		return err
	}
	request.SetBasicAuth("fern", password)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Fern readiness returned %s", response.Status)
	}
	var readiness struct {
		Ready bool `json:"ready"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<10))
	if err := decoder.Decode(&readiness); err != nil || !readiness.Ready {
		return fmt.Errorf("Fern readiness returned an invalid response")
	}
	return nil
}

func checkProviderConnection(origin, password string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(origin, "/")+"/api/provider", nil)
	if err != nil {
		return 0, err
	}
	request.SetBasicAuth("opencode", password)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return 0, fmt.Errorf("query OpenCode providers: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("OpenCode provider readiness returned %s", response.Status)
	}
	var result struct {
		Data []struct {
			ID       string `json:"id"`
			Disabled bool   `json:"disabled"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return 0, fmt.Errorf("OpenCode provider readiness returned an invalid response")
	}
	count := 0
	for _, provider := range result.Data {
		if provider.ID != "" && !provider.Disabled {
			count++
		}
	}
	if count == 0 {
		return 0, fmt.Errorf("OpenCode has no active provider connection")
	}
	return count, nil
}

func issuePairingCode(origin, password string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(origin, "/")+"/fern/pair/new", nil)
	if err != nil {
		return "", err
	}
	request.SetBasicAuth("fern", password)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("pairing endpoint returned %s", response.Status)
	}
	var result struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(&result); err != nil {
		return "", err
	}
	if result.Code == "" {
		return "", fmt.Errorf("pairing endpoint returned no code")
	}
	return result.Code, nil
}

func validatePhoneOrigin(origin string) (string, error) {
	if origin == "" {
		return "", fmt.Errorf("invalid phone URL %q: a private HTTPS origin is required", origin)
	}
	validated, err := config.ParseRemoteOrigin(origin)
	if err != nil {
		return "", fmt.Errorf("invalid phone URL %q: %w", origin, err)
	}
	return validated, nil
}

func checkPairingPreview(origin, code string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(origin, "/")+"/fern/pair?code="+url.QueryEscape(code), nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "Fern-Doctor-Pairing-Scanner/1")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return fmt.Errorf("pairing preview failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("pairing preview returned %s", response.Status)
	}
	if len(response.Cookies()) != 0 {
		return fmt.Errorf("pairing preview unexpectedly set a cookie")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil || !strings.Contains(string(body), "Pair this phone?") {
		return fmt.Errorf("pairing preview returned an invalid response")
	}
	return nil
}

func checkRemoteCredentialRejected(origin, password string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(origin, "/")+"/api/health", nil)
	if err != nil {
		return err
	}
	request.SetBasicAuth("opencode", password)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return fmt.Errorf("remote credential rejection check failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("remote ingress accepted or mishandled the backend credential (%s)", response.Status)
	}
	return nil
}

var httpsOriginPattern = regexp.MustCompile(`https://[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::[0-9]{1,5})?`)

func discoverTailscaleURL(listen, operatorListen string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "tailscale", "serve", "status")
	command.WaitDelay = time.Second
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return tailscaleOriginForTopology(string(output), listen, operatorListen)
}

func localTailscaleOrigin() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "tailscale", "status", "--json")
	command.WaitDelay = time.Second
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return tailscaleLocalOrigin(output)
}

func tailscaleLocalOrigin(output []byte) (string, error) {
	var status struct {
		BackendState string `json:"BackendState"`
		Self         struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		return "", err
	}
	if status.BackendState != "Running" {
		return "", fmt.Errorf("Tailscale backend is %q, not Running", status.BackendState)
	}
	host := strings.TrimSuffix(status.Self.DNSName, ".")
	if host == "" || !strings.HasSuffix(strings.ToLower(host), ".ts.net") {
		return "", fmt.Errorf("Tailscale did not report a private DNS name")
	}
	return "https://" + host, nil
}

func tailscaleOriginForTarget(output, listen string) (string, error) {
	if strings.Contains(strings.ToLower(output), "funnel on") || strings.Contains(strings.ToLower(output), "available on the internet") {
		return "", fmt.Errorf("Tailscale Funnel must be disabled")
	}
	want := "|-- / proxy http://" + listen
	currentOrigin := ""
	matches := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		if origin := httpsOriginPattern.FindString(line); origin != "" {
			currentOrigin = origin
			continue
		}
		if strings.TrimSpace(line) == want {
			if currentOrigin == "" {
				return "", fmt.Errorf("Tailscale Serve route has no HTTPS origin")
			}
			matches[currentOrigin] = true
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("expected one Tailscale HTTPS origin for http://%s, found %d", listen, len(matches))
	}
	for origin := range matches {
		return origin, nil
	}
	return "", fmt.Errorf("Tailscale Serve root does not proxy http://%s", listen)
}

func tailscaleOriginForTopology(output, listen, operatorListen string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		_, target, found := strings.Cut(strings.TrimSpace(line), "proxy ")
		if found && serveTargetUsesListener(strings.TrimSpace(target), operatorListen) {
			return "", fmt.Errorf("Tailscale Serve exposes proxy.operatorListen; only proxy.listen may be served")
		}
	}
	return tailscaleOriginForTarget(output, listen)
}

func serveTargetUsesListener(target, listener string) bool {
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	_, listenerPort, err := net.SplitHostPort(listener)
	if err != nil {
		return false
	}
	targetPort, targetErr := strconv.Atoi(parsed.Port())
	configuredPort, listenerErr := strconv.Atoi(listenerPort)
	return targetErr == nil && listenerErr == nil && targetPort == configuredPort
}

func tailscaleOrigin(output string) (string, error) {
	matches := httpsOriginPattern.FindAllString(output, -1)
	unique := make(map[string]bool)
	for _, match := range matches {
		unique[match] = true
	}
	if len(unique) != 1 {
		return "", fmt.Errorf("expected one Tailscale HTTPS origin, found %d", len(unique))
	}
	for origin := range unique {
		return origin, nil
	}
	return "", fmt.Errorf("no Tailscale HTTPS origin")
}

func writeDoctorReport(writer io.Writer, report doctorReport, showQR bool) {
	for _, check := range report.Checks {
		fmt.Fprintf(writer, "%-5s %-12s %s\n", strings.ToUpper(check.Status), check.ID, check.Summary)
		if check.Remediation != "" && check.Status != "pass" {
			fmt.Fprintf(writer, "      %s\n", check.Remediation)
		}
	}
	if report.PhoneURL == "" {
		return
	}
	fmt.Fprintf(writer, "\nOne-time phone URL (expires in 5 minutes):\n%s\n", report.PhoneURL)
	if showQR {
		_ = writeQR(writer, report.PhoneURL)
	}
	fmt.Fprintln(writer, "Transport checks passed. Real phone interaction still requires your confirmation.")
}

func writeQR(writer io.Writer, value string) error {
	code, err := qrcode.New(value, qrcode.Medium)
	if err != nil {
		return err
	}
	bitmap := code.Bitmap()
	for row := 0; row < len(bitmap); row += 2 {
		_, _ = io.WriteString(writer, "\x1b[30;47m")
		for column := range bitmap[row] {
			top := bitmap[row][column]
			bottom := row+1 < len(bitmap) && bitmap[row+1][column]
			switch {
			case top && bottom:
				_, _ = io.WriteString(writer, "█")
			case top:
				_, _ = io.WriteString(writer, "▀")
			case bottom:
				_, _ = io.WriteString(writer, "▄")
			default:
				_, _ = io.WriteString(writer, " ")
			}
		}
		_, _ = io.WriteString(writer, "\x1b[0m\n")
	}
	return nil
}
