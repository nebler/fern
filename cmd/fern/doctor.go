package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nebler/fern/internal/config"
	fernRuntime "github.com/nebler/fern/internal/runtime"
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
	remoteURL := flags.String("url", "", "explicit private HTTPS origin")
	jsonOutput := flags.Bool("json", false, "output a stable JSON report")
	qr := flags.Bool("qr", true, "print a terminal QR code for a ready phone URL")
	if err := parseFlags(flags, args); err != nil {
		return err
	}
	report := diagnose(*configPath, *envPath, *phone, *remoteURL)
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
		return fmt.Errorf("phone demo is not ready; resolve failed checks above")
	}
	return nil
}

func diagnose(configPath, envPath string, phone bool, explicitURL string) doctorReport {
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
	cfg, err := config.Load(configPath, cwd, true, config.Overrides{})
	if err != nil {
		add("config", "fail", err.Error(), "Fix the strict Fern configuration.")
		return report
	}
	cfg.Workspace.Env = forwardedEnvironment(mergeEnvironment(cfg.Workspace.Env, values))
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
		add("github", "warn", "GitHub CLI is not authenticated", "Run gh auth login --hostname github.com before publishing a PR.")
	} else {
		add("github", "pass", "GitHub CLI authentication is available on the host", "")
	}
	localURL, err := attachURL(cfg.Listen)
	if err != nil {
		add("gateway", "fail", err.Error(), "Fix proxy.listen.")
	} else if err := checkReady(localURL, cfg.Workspace.Env["OPENCODE_PASSWORD"]); err != nil {
		add("gateway", "fail", "local Fern gateway is not ready", "Start fern up with the same --config and --env-file.")
	} else {
		add("gateway", "pass", "local Fern gateway is serving", "")
	}
	if phone || explicitURL != "" {
		origin := explicitURL
		if origin == "" {
			origin, err = discoverTailscaleURL()
		}
		if err != nil || origin == "" {
			add("tailscale", "fail", "no Tailscale Serve HTTPS origin was found", fmt.Sprintf("Run tailscale serve --bg http://%s, then retry; or pass --url.", cfg.Listen))
		} else if validated, validateErr := attachTarget(&origin, cfg.Listen); validateErr != nil {
			add("tailscale", "fail", validateErr.Error(), "Use a private HTTPS root origin.")
		} else if localOrigin, localErr := localTailscaleOrigin(); localErr != nil || !strings.EqualFold(mustHostname(validated), mustHostname(localOrigin)) {
			add("tailscale", "fail", "phone URL does not match this Tailscale host", "Use the HTTPS URL reported by tailscale serve status on this host.")
		} else if err := checkReady(validated, cfg.Workspace.Env["OPENCODE_PASSWORD"]); err != nil {
			add("phone", "fail", "private phone URL did not reach Fern", "Check Tailscale on both devices and the Serve mapping.")
		} else {
			code, pairErr := issuePairingCode(localURL, cfg.Workspace.Env["OPENCODE_PASSWORD"])
			if pairErr != nil {
				add("pairing", "fail", "could not create a one-time phone pairing link", "Ensure the local Fern process is the current build.")
			} else {
				report.PhoneURL = strings.TrimRight(validated, "/") + "/fern/pair?code=" + url.QueryEscape(code)
				add("tailscale", "pass", "private HTTPS route reaches Fern", "")
				add("pairing", "pass", "one-time phone pairing link created", "")
				add("phone", "pass", "phone-demo transport is ready", "")
			}
		}
	}
	return report
}

func mustHostname(origin string) string {
	parsed, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func checkCommand(timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
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
	fernRuntime.ServerAuth{Password: password}.Apply(request)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Fern readiness returned %s", response.Status)
	}
	return nil
}

func issuePairingCode(origin, password string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(origin, "/")+"/fern/pair/new", nil)
	if err != nil {
		return "", err
	}
	fernRuntime.ServerAuth{Password: password}.Apply(request)
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

var httpsOriginPattern = regexp.MustCompile(`https://[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?`)

func discoverTailscaleURL() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "tailscale", "serve", "status")
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return tailscaleOrigin(string(output))
}

func localTailscaleOrigin() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "tailscale", "status", "--json")
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return tailscaleLocalOrigin(output)
}

func tailscaleLocalOrigin(output []byte) (string, error) {
	var status struct {
		Self struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		return "", err
	}
	host := strings.TrimSuffix(status.Self.DNSName, ".")
	if host == "" || !strings.HasSuffix(strings.ToLower(host), ".ts.net") {
		return "", fmt.Errorf("Tailscale did not report a private DNS name")
	}
	return "https://" + host, nil
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
