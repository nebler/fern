package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
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

// diagnoseOptions selects which doctor readiness lanes run.
type diagnoseOptions struct {
	ConfigPath   string
	EnvPath      string
	RequirePhone bool
}

func runDoctor(args []string) error {
	fs := newFlagSet("doctor", "Verify Fern and the private phone-demo path.")
	configPath, envPath := addConfigFlags(fs)
	phone := fs.Bool("phone", false, "verify the private Tailscale HTTPS route and print a one-time pairing QR code")
	jsonOutput := fs.Bool("json", false, "output a stable JSON report")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report := diagnose(ctx, diagnoseOptions{ConfigPath: *configPath, EnvPath: *envPath, RequirePhone: *phone})
	if *jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(report); err != nil {
			return err
		}
	} else {
		writeDoctorReport(os.Stdout, report)
	}
	if !report.Ready {
		return errors.New("requested Fern readiness checks failed; resolve failed checks above")
	}
	return nil
}

// diagnose runs the readiness checklist. Every probe derives its own bounded
// context from ctx so Ctrl-C aborts long lanes such as phone-mode verification.
func diagnose(ctx context.Context, opts diagnoseOptions) doctorReport {
	report := doctorReport{Ready: true}
	add := func(id, status, summary, remediation string) {
		report.Checks = append(report.Checks, doctorCheck{ID: id, Status: status, Summary: summary, Remediation: remediation})
		if status == "fail" {
			report.Ready = false
		}
	}
	cfg, err := loadCommandConfig(opts.ConfigPath, opts.EnvPath)
	if err != nil {
		add("config", "fail", err.Error(), "Run fern init, or pass the --config and --env-file it wrote.")
		return report
	}
	if err := config.ValidateBootstrap(cfg); err != nil {
		add("config", "fail", err.Error(), "Fix the Fern configuration or secret file.")
		return report
	}
	add("config", "pass", "configuration is valid", "")
	if cfg.Workspace.GitHub.InstallationID == 0 {
		add("github", "fail", "GitHub App installation ID is not configured", "Create the GitHub App, install it on the configured repository, set workspace.github.installationId, run 'fern credentials set', and restart Fern.")
		return report
	}
	if runtime.GOOS != "linux" {
		add("runtime-storage", "fail", "execution requires native Linux; Docker Desktop is unsupported", "Use a native Linux Docker host with operator-provisioned XFS project quotas for bytes and inodes.")
	} else if cfg.Runs.RuntimeStorageRoot == "" {
		add("runtime-storage", "fail", "runs.runtimeStorageRoot is required for execution", "Provision an XFS project-quota root, separate from durable DB/CAS storage, and configure runs.runtimeStorageRoot.")
	} else {
		add("runtime-storage", "warn", "path syntax only checked; doctor does not qualify quota enforcement or host reserve", "Provider startup must accept the native Linux XFS project-quota root; qualify byte and inode exhaustion and maintain host reserve before production. Docker Desktop is unsupported.")
	}
	if info, err := os.Stat(filepath.Join(cfg.Workspace.Repo, ".git")); err != nil || !info.IsDir() {
		add("repository", "fail", "repository is not a standard Git checkout", "Use a repository with a .git directory.")
	} else {
		add("repository", "pass", "Git repository is available", "")
	}
	if err := validateDockerTopology(); err != nil {
		add("docker", "fail", err.Error(), "Use the local Docker Unix socket.")
	} else if err := checkCommand(ctx, 5*time.Second, "docker", "info"); err != nil {
		add("docker", "fail", err.Error(), "Start Docker and grant this user access.")
	} else {
		add("docker", "pass", "local Docker daemon is reachable", "")
		if err := checkCommand(ctx, 5*time.Second, "docker", "image", "inspect", cfg.Runs.BackgroundImage); err != nil {
			add("image", "fail", "qualified Background Run image is unavailable", "Run make image-background-source or pull the configured image.")
		} else {
			add("image", "pass", "Background Run image is available", "")
		}
	}
	localURL, err := loopbackURL(cfg.Proxy.OperatorListen)
	if err != nil {
		add("gateway", "fail", err.Error(), "Fix proxy.operatorListen.")
	} else if err := checkReady(ctx, localURL, cfg.ControlPassword); err != nil {
		add("gateway", "fail", "local Fern gateway is not ready", "Start fern up with the same --config and --env-file.")
	} else {
		add("gateway", "pass", "local Fern gateway is serving", "")
	}
	if opts.RequirePhone {
		checkPhoneRoute(ctx, &report, add, cfg, localURL)
	}
	return report
}

// checkPhoneRoute verifies the private Tailscale HTTPS path end to end and, on
// success, records the one-time pairing URL on the report.
func checkPhoneRoute(ctx context.Context, report *doctorReport, add func(id, status, summary, remediation string), cfg config.Config, localURL string) {
	if cfg.Proxy.RemoteOrigin == "" {
		add("tailscale", "fail", "proxy.remoteOrigin is required for phone mode", "Set proxy.remoteOrigin to the exact canonical HTTPS root origin reported for this host, then retry.")
		return
	}
	servedOrigin, serveErr := discoverTailscaleURL(ctx, cfg.Proxy.Listen, cfg.Proxy.OperatorListen)
	if serveErr != nil || servedOrigin == "" {
		add("tailscale", "fail", "no Tailscale Serve HTTPS origin was found", fmt.Sprintf("Run tailscale serve --bg http://%s, then retry.", cfg.Proxy.Listen))
		return
	}
	localOrigin, localErr := localTailscaleOrigin(ctx)
	if topologyErr := validatePhoneTopology(cfg.Proxy.RemoteOrigin, servedOrigin, localOrigin, localErr); topologyErr != nil {
		add("tailscale", "fail", topologyErr.Error(), "Make proxy.remoteOrigin, the root Serve origin, and this host's tailnet HTTPS origin identical.")
		return
	}
	if cfg.Runs.BackgroundRoute != nil {
		routeOrigin, routeErr := discoverTailscaleURL(ctx, cfg.Runs.BackgroundRoute.Listen, cfg.Proxy.OperatorListen)
		parsedRouteOrigin, _ := url.Parse(cfg.Runs.BackgroundRoute.Origin)
		if routeErr != nil || routeOrigin != cfg.Runs.BackgroundRoute.Origin {
			add("background-route", "fail", "the Background Run listener is not published at its exact configured private origin",
				fmt.Sprintf("Run tailscale serve --https=%s --bg http://%s without changing any other listener.", parsedRouteOrigin.Port(), cfg.Runs.BackgroundRoute.Listen))
			return
		}
		if routeErr := checkBackgroundRouteSurface(ctx, "http://"+cfg.Runs.BackgroundRoute.Listen); routeErr != nil {
			add("background-route", "fail", "the local Background Run listener is not serving the dedicated paired-device boundary", "Start Fern with the configured Background Run listener, then retry.")
			return
		}
		if routeErr := checkBackgroundRouteSurface(ctx, cfg.Runs.BackgroundRoute.Origin); routeErr != nil {
			add("background-route", "fail", "the private Background Run origin does not reach the dedicated paired-device boundary", "Check the exact Tailscale Serve port mapping and private TLS route, then retry.")
			return
		}
		add("background-route", "pass", "private Background Run route reaches its exact loopback listener", "")
	}
	code, pairErr := issuePairingCode(ctx, localURL, cfg.ControlPassword)
	if pairErr != nil {
		add("pairing", "fail", "could not create a one-time phone pairing link", "Ensure the local Fern process is the current build.")
		return
	}
	if err := checkPairingPreview(ctx, cfg.Proxy.RemoteOrigin, code); err != nil {
		add("pairing", "fail", err.Error(), "Check that Tailscale Serve targets proxy.listen and Fern is current.")
		return
	}
	report.PhoneURL = cfg.Proxy.RemoteOrigin + "/fern/pair?code=" + url.QueryEscape(code)
	add("tailscale", "pass", "private HTTPS route reaches Fern", "")
	add("pairing", "pass", "one-time phone pairing link created", "")
	add("phone", "pass", "phone-demo transport is ready", "")
}

// doctorHTTP sends one bodiless, redirect-free request bounded by five seconds
// and returns the response with at most limit bytes of its body. A nonempty
// password authenticates as the operator.
func doctorHTTP(ctx context.Context, method, target, password string, limit int64) (*http.Response, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, nil, err
	}
	if password != "" {
		request.SetBasicAuth("fern", password)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit))
	return response, body, err
}

func checkBackgroundRouteSurface(ctx context.Context, origin string) error {
	response, _, err := doctorHTTP(ctx, http.MethodGet, strings.TrimRight(origin, "/")+"/api/health", "", 4<<10)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusUnauthorized || response.Header.Get("Cache-Control") != "no-store" {
		return fmt.Errorf("Background Run route returned %s without the dedicated no-store boundary", response.Status)
	}
	return nil
}

func validatePhoneTopology(configured, served, local string, localErr error) error {
	if configured == "" {
		return errors.New("proxy.remoteOrigin is required for phone mode")
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

func checkCommand(ctx context.Context, timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
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

func checkReady(ctx context.Context, origin, password string) error {
	response, body, err := doctorHTTP(ctx, http.MethodGet, strings.TrimRight(origin, "/")+"/fern/ready", password, 4<<10)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Fern readiness returned %s", response.Status)
	}
	var readiness struct {
		Ready bool `json:"ready"`
	}
	if err := json.Unmarshal(body, &readiness); err != nil {
		return fmt.Errorf("decode Fern readiness response: %w", err)
	}
	if !readiness.Ready {
		return errors.New("Fern readiness reported the workspace not ready")
	}
	return nil
}

func issuePairingCode(ctx context.Context, origin, password string) (string, error) {
	response, body, err := doctorHTTP(ctx, http.MethodPost, strings.TrimRight(origin, "/")+"/fern/pair/new", password, 16<<10)
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("pairing endpoint returned %s", response.Status)
	}
	var result struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	if result.Code == "" {
		return "", errors.New("pairing endpoint returned no code")
	}
	return result.Code, nil
}

func checkPairingPreview(ctx context.Context, origin, code string) error {
	response, body, err := doctorHTTP(ctx, http.MethodGet, strings.TrimRight(origin, "/")+"/fern/pair?code="+url.QueryEscape(code), "", 64<<10)
	if err != nil {
		return fmt.Errorf("pairing preview failed: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("pairing preview returned %s", response.Status)
	}
	if len(response.Cookies()) != 0 {
		return errors.New("pairing preview unexpectedly set a cookie")
	}
	if !strings.Contains(string(body), "Pair this phone?") {
		return errors.New("pairing preview returned an invalid response")
	}
	return nil
}

var httpsOriginPattern = regexp.MustCompile(`https://[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::[0-9]{1,5})?`)

func discoverTailscaleURL(ctx context.Context, listen, operatorListen string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "tailscale", "serve", "status")
	command.WaitDelay = time.Second
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return tailscaleOriginForTopology(string(output), listen, operatorListen)
}

func localTailscaleOrigin(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
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
		return "", errors.New("Tailscale did not report a private DNS name")
	}
	return "https://" + host, nil
}

func tailscaleOriginForTarget(output, listen string) (string, error) {
	if strings.Contains(strings.ToLower(output), "funnel on") || strings.Contains(strings.ToLower(output), "available on the internet") {
		return "", errors.New("Tailscale Funnel must be disabled")
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
				return "", errors.New("Tailscale Serve route has no HTTPS origin")
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
			return "", errors.New("Tailscale Serve exposes proxy.operatorListen; only proxy.listen may be served")
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

func writeDoctorReport(writer io.Writer, report doctorReport) {
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
	_ = writeQR(writer, report.PhoneURL)
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
