package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/task"
)

const (
	fernRunUsername       = "fern"
	fernCredentialService = "dev.fern.opencode"
	attachOpenCodeVersion = "1.18.16"
	maxAttachmentTTL      = 2 * time.Hour
	maxRunResponseBytes   = 1 << 20
)

type runCLIOptions struct {
	endpoint       string
	configPath     string
	envPath        string
	configRequired bool
	all            bool
	json           bool
	opencode       string
}

type runConnection struct {
	apiOrigin        *url.URL
	apiAuthorization string
	attachOrigin     string
	client           *http.Client
}

type runSummary struct {
	ID         task.TaskID `json:"id"`
	State      string      `json:"state"`
	Repository string      `json:"repository"`
	Head       string      `json:"head"`
	Branch     *string     `json:"branch"`
	Attachable bool        `json:"attachable"`
}

type runListResponse struct {
	Runs []runSummary `json:"runs"`
}

type runAttachResponse struct {
	RunID     task.TaskID            `json:"run_id"`
	URL       string                 `json:"url"`
	SessionID task.OpenCodeSessionID `json:"session_id"`
	Username  string                 `json:"username"`
	Password  string                 `json:"password"`
	ExpiresAt time.Time              `json:"expires_at"`
}

func runRuns(ctx context.Context, args []string) error {
	options, err := parseRunsFlags(args)
	if err != nil {
		return err
	}
	connection, err := resolveRunConnection(ctx, options)
	if err != nil {
		return err
	}
	runs, err := connection.list(ctx)
	if err != nil {
		return err
	}
	if !options.all {
		runs = activeRuns(runs)
	}
	if options.json {
		return json.NewEncoder(os.Stdout).Encode(runListResponse{Runs: runs})
	}
	if len(runs) == 0 {
		fmt.Fprintln(os.Stdout, "No running Fern sessions.")
		return nil
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "RUN ID\tSTATE\tREPOSITORY\tBRANCH")
	for _, run := range runs {
		branch := "-"
		if run.Branch != nil && *run.Branch != "" {
			branch = *run.Branch
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", run.ID, run.State, run.Repository, branch)
	}
	return writer.Flush()
}

func runAttach(ctx context.Context, args []string) error {
	options, positional, err := parseAttachFlags(args)
	if err != nil {
		return err
	}
	connection, err := resolveRunConnection(ctx, options)
	if err != nil {
		return err
	}
	var runID task.TaskID
	if len(positional) == 1 {
		runID, err = task.ParseTaskID(positional[0])
		if err != nil {
			return invocationError{message: "attach requires a valid Fern run ID"}
		}
	} else {
		runs, listErr := connection.list(ctx)
		if listErr != nil {
			return listErr
		}
		candidates := attachableRuns(runs)
		if len(candidates) == 0 {
			return errors.New("no Fern session is ready to attach")
		}
		if len(candidates) == 1 {
			runID = candidates[0].ID
		} else {
			input, statErr := os.Stdin.Stat()
			if statErr != nil || input.Mode()&os.ModeCharDevice == 0 {
				return errors.New("more than one Fern session is attachable; pass a run ID from 'fern runs'")
			}
			runID, err = selectAttachRun(os.Stdin, os.Stderr, candidates)
			if err != nil {
				return err
			}
		}
	}
	attachment, err := connection.attach(ctx, runID)
	if err != nil {
		return err
	}
	return launchOpenCodeAttach(ctx, options.opencode, connection.attachURL(attachment.URL), attachment)
}

func selectAttachRun(input io.Reader, output io.Writer, runs []runSummary) (task.TaskID, error) {
	if len(runs) == 0 {
		return "", errors.New("no Fern session is ready to attach")
	}
	_, _ = fmt.Fprintln(output, "Select a Fern session:")
	for index, run := range runs {
		branch := "-"
		if run.Branch != nil && *run.Branch != "" {
			branch = *run.Branch
		}
		_, _ = fmt.Fprintf(output, "  %d. %s  %-12s  %s  %s\n", index+1, run.ID, run.State, run.Repository, branch)
	}
	_, _ = fmt.Fprint(output, "Run: ")
	line, err := bufio.NewReader(io.LimitReader(input, 64)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read run selection: %w", err)
	}
	selection, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || selection < 1 || selection > len(runs) {
		return "", errors.New("invalid Fern session selection")
	}
	return runs[selection-1].ID, nil
}

func parseRunsFlags(args []string) (runCLIOptions, error) {
	fs := newFlagSet("runs", "List Fern Background Run sessions.")
	endpoint := fs.String("endpoint", os.Getenv("FERN_ENDPOINT"), "remote Fern root HTTPS origin")
	configPath := fs.String("config", "fern.yaml", "local Fern configuration file")
	envPath := fs.String("env-file", "", "local Fern protected environment file")
	all := fs.Bool("all", false, "include completed and failed runs")
	jsonOutput := fs.Bool("json", false, "write JSON output")
	if err := parseFlagValues(fs, args); err != nil {
		return runCLIOptions{}, err
	}
	if fs.NArg() != 0 {
		return runCLIOptions{}, invocationError{message: "usage: fern runs [flags]"}
	}
	return runCLIOptions{endpoint: *endpoint, configPath: *configPath, envPath: *envPath,
		configRequired: flagProvided(fs, "config"), all: *all, json: *jsonOutput}, nil
}

func parseAttachFlags(args []string) (runCLIOptions, []string, error) {
	fs := newFlagSet("attach", "Attach the OpenCode TUI to a live Fern session.")
	endpoint := fs.String("endpoint", os.Getenv("FERN_ENDPOINT"), "remote Fern root HTTPS origin")
	configPath := fs.String("config", "fern.yaml", "local Fern configuration file")
	envPath := fs.String("env-file", "", "local Fern protected environment file")
	opencode := fs.String("opencode", "opencode", "OpenCode executable")
	if err := parseFlagValues(fs, args); err != nil {
		return runCLIOptions{}, nil, err
	}
	if fs.NArg() > 1 {
		return runCLIOptions{}, nil, invocationError{message: "usage: fern attach [flags] [run-id]"}
	}
	return runCLIOptions{endpoint: *endpoint, configPath: *configPath, envPath: *envPath,
		configRequired: flagProvided(fs, "config"), opencode: *opencode}, fs.Args(), nil
}

func resolveRunConnection(ctx context.Context, options runCLIOptions) (*runConnection, error) {
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if options.endpoint != "" {
		origin, err := parseFernClientOrigin(options.endpoint)
		if err != nil {
			return nil, err
		}
		token := os.Getenv("FERN_TOKEN")
		if token == "" {
			token, err = readFernKeyringCredential(ctx, origin.String())
			if err != nil {
				return nil, err
			}
		}
		if !validFernCredential(token) {
			return nil, errors.New("Fern has no valid plugin credential for this endpoint")
		}
		return &runConnection{apiOrigin: origin, apiAuthorization: "Bearer " + token, client: client}, nil
	}
	cfg, err := loadBackgroundCommandConfig(options.configPath, options.configRequired, options.envPath, config.BackgroundOverrides{})
	if err != nil {
		return nil, err
	}
	api, err := loopbackURL(cfg.OperatorListen)
	if err != nil {
		return nil, err
	}
	attach, err := loopbackURL(cfg.Tasks.BackgroundRoute.Listen)
	if err != nil {
		return nil, err
	}
	return &runConnection{apiOrigin: mustURL(api), apiAuthorization: basicAuthorization(fernRunUsername, cfg.Control.Password),
		attachOrigin: attach, client: client}, nil
}

func parseFernClientOrigin(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	localHTTP := err == nil && parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())
	if err != nil || parsed.User != nil || parsed.Host == "" || parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && !localHTTP) {
		return nil, errors.New("remote Fern endpoint must be a root HTTPS origin")
	}
	parsed.Path, parsed.RawPath = "", ""
	return parsed, nil
}

func (connection *runConnection) list(ctx context.Context) ([]runSummary, error) {
	var response runListResponse
	if err := connection.get(ctx, "/fern/api/v1/runs", &response); err != nil {
		return nil, err
	}
	for _, run := range response.Runs {
		if _, err := task.ParseTaskID(string(run.ID)); err != nil || run.State == "" || run.Repository == "" {
			return nil, errors.New("Fern returned an invalid run list")
		}
		if _, err := task.ParseGitOID(run.Head); err != nil {
			return nil, errors.New("Fern returned an invalid run list")
		}
	}
	return response.Runs, nil
}

func (connection *runConnection) attach(ctx context.Context, runID task.TaskID) (runAttachResponse, error) {
	var response runAttachResponse
	if err := connection.get(ctx, "/fern/api/v1/runs/"+url.PathEscape(string(runID))+"/attach", &response); err != nil {
		return runAttachResponse{}, err
	}
	now := time.Now()
	if response.RunID != runID || response.Username != "opencode" || !validFernCredential(response.Password) ||
		!response.ExpiresAt.After(now) || response.ExpiresAt.After(now.Add(maxAttachmentTTL+time.Minute)) {
		return runAttachResponse{}, errors.New("Fern returned an invalid attachment identity")
	}
	if _, err := task.ParseOpenCodeSessionID(string(response.SessionID)); err != nil {
		return runAttachResponse{}, errors.New("Fern returned an invalid OpenCode session identity")
	}
	returned, err := url.Parse(response.URL)
	if err != nil || returned.User != nil || returned.Scheme != "https" || returned.Host == "" || returned.Path != "" || returned.RawPath != "" ||
		returned.RawQuery != "" || returned.Fragment != "" {
		return runAttachResponse{}, errors.New("Fern returned an invalid attachment URL")
	}
	if connection.attachOrigin == "" && !strings.EqualFold(returned.Hostname(), connection.apiOrigin.Hostname()) {
		return runAttachResponse{}, errors.New("Fern returned an attachment URL on another host")
	}
	return response, nil
}

func (connection *runConnection) attachURL(returned string) string {
	if connection.attachOrigin != "" {
		return connection.attachOrigin
	}
	return returned
}

func (connection *runConnection) get(ctx context.Context, path string, destination any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, connection.apiOrigin.String()+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", connection.apiAuthorization)
	request.Header.Set("Accept", "application/json")
	response, err := connection.client.Do(request)
	if err != nil {
		return fmt.Errorf("contact Fern: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRunResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read Fern response: %w", err)
	}
	if len(body) > maxRunResponseBytes {
		return errors.New("Fern response exceeded the size limit")
	}
	if response.StatusCode != http.StatusOK {
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = response.Status
		}
		return fmt.Errorf("Fern request failed: %s", message)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("Fern returned invalid JSON")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("Fern returned more than one JSON value")
	}
	return nil
}

func launchOpenCodeAttach(ctx context.Context, executable, endpoint string, attachment runAttachResponse) error {
	resolved, err := exec.LookPath(executable)
	if err != nil {
		return fmt.Errorf("find OpenCode executable: %w", err)
	}
	versionCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	versionCommand := exec.CommandContext(versionCtx, resolved, "--version")
	versionCommand.Env = attachEnvironment("", "")
	version, versionErr := versionCommand.Output()
	cancel()
	if versionErr != nil || strings.TrimSpace(string(version)) != attachOpenCodeVersion {
		return fmt.Errorf("fern attach requires OpenCode %s", attachOpenCodeVersion)
	}
	command := exec.CommandContext(ctx, resolved, "attach", endpoint, "--session", string(attachment.SessionID), "--pure")
	command.Env = attachEnvironment(attachment.Username, attachment.Password)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return commandExitError{code: exit.ExitCode(), err: errors.New("OpenCode attachment exited unsuccessfully")}
		}
		return fmt.Errorf("launch OpenCode attachment: %w", err)
	}
	return nil
}

func attachEnvironment(username, password string) []string {
	environment := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "FERN_TOKEN", "OPENCODE_SERVER_USERNAME", "OPENCODE_SERVER_PASSWORD", "OPENCODE_PASSWORD":
			continue
		}
		environment = append(environment, entry)
	}
	if username != "" {
		environment = append(environment, "OPENCODE_SERVER_USERNAME="+username, "OPENCODE_SERVER_PASSWORD="+password)
	}
	return environment
}

func activeRuns(runs []runSummary) []runSummary {
	result := make([]runSummary, 0, len(runs))
	for _, run := range runs {
		switch run.State {
		case "queued", "setting_up", "working", "needs_you", "canceling", "uncertain":
			result = append(result, run)
		}
	}
	return result
}

func attachableRuns(runs []runSummary) []runSummary {
	result := make([]runSummary, 0, 1)
	for _, run := range runs {
		if run.Attachable {
			result = append(result, run)
		}
	}
	return result
}

func readFernKeyringCredential(ctx context.Context, origin string) (string, error) {
	keyringCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.CommandContext(keyringCtx, "/usr/bin/security", "find-generic-password", "-a", canonicalKeyringOrigin(origin), "-s", fernCredentialService, "-w")
	case "linux":
		command = exec.CommandContext(keyringCtx, "secret-tool", "lookup", "service", fernCredentialService, "origin", canonicalKeyringOrigin(origin))
	default:
		return "", errors.New("Fern credentials require macOS Keychain or Linux Secret Service")
	}
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, io.Discard
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		return "", errors.New("could not read the Fern plugin credential from the operating-system keyring")
	}
	return strings.TrimRight(output.String(), "\r\n"), nil
}

type limitedBuffer struct {
	buffer bytes.Buffer
	over   bool
}

func (buffer *limitedBuffer) Write(value []byte) (int, error) {
	const limit = 4096
	if buffer.buffer.Len()+len(value) > limit {
		buffer.over = true
		remaining := limit - buffer.buffer.Len()
		if remaining > 0 {
			_, _ = buffer.buffer.Write(value[:remaining])
		}
		return len(value), nil
	}
	return buffer.buffer.Write(value)
}

func (buffer *limitedBuffer) String() string {
	if buffer.over {
		return ""
	}
	return buffer.buffer.String()
}

func validFernCredential(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func canonicalKeyringOrigin(origin string) string { return strings.TrimRight(origin, "/") + "/" }

func basicAuthorization(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

func mustURL(value string) *url.URL {
	parsed, err := url.Parse(value)
	if err != nil {
		panic(err)
	}
	return parsed
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
