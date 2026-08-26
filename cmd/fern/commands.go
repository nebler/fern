package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/watch"
	"github.com/nebler/fern/internal/workspace"
)

func runDown(args []string, log *slog.Logger) error {
	fs, nameFlag, configPath := workspaceFlags("down")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	name, err := workspaceName(fs, *nameFlag, *configPath)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext()
	defer cancel()
	lease, err := acquireWorkspaceLease(name)
	if err != nil {
		return err
	}
	defer lease.Release()
	docker, err := newDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
	return docker.Destroy(ctx, name)
}

func runStatus(args []string, log *slog.Logger) error {
	fs, nameFlag, configPath := workspaceFlags("status")
	jsonOutput := fs.Bool("json", false, "output a stable JSON object")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	name, err := workspaceName(fs, *nameFlag, *configPath)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext()
	defer cancel()
	docker, err := newDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
	observation, err := docker.Status(ctx, name)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeStatusJSON(os.Stdout, name, observation)
	}
	fmt.Printf("%s\t%s", name, observation.State)
	if observation.State != runtime.StateAbsent {
		fmt.Printf("\tdocker=%s exit=%d oom=%t", observation.DockerStatus, observation.ExitCode, observation.OOMKilled)
	}
	fmt.Println()
	return nil
}

type statusJSON struct {
	Workspace    string        `json:"workspace"`
	State        runtime.State `json:"state"`
	DockerStatus string        `json:"dockerStatus"`
	ExitCode     int           `json:"exitCode"`
	OOMKilled    bool          `json:"oomKilled"`
}

func writeStatusJSON(output io.Writer, workspace string, observation runtime.Observation) error {
	return json.NewEncoder(output).Encode(statusJSON{
		Workspace: workspace, State: observation.State, DockerStatus: observation.DockerStatus,
		ExitCode: observation.ExitCode, OOMKilled: observation.OOMKilled,
	})
}

// eventBufferSize deep-queues streamed backend events so a burst while the
// printer is busy does not drop or stall the stream.
const eventBufferSize = 128

// statusDetailLimit caps each printed event payload so one huge event cannot
// wash out the terminal.
const statusDetailLimit = 160

func runEvents(args []string, log *slog.Logger) error {
	fs, nameFlag, configPath := workspaceFlags("debug events")
	envPath := fs.String("env-file", "", "protected environment file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	values, err := readProtectedEnvironment(*envPath)
	if err != nil {
		return err
	}
	client, err := config.LoadEventsWithEnvironment(*configPath, flagProvided(fs, "config"), optionalFlag(fs, "name", nameFlag), environmentLookup(values))
	if err != nil {
		return err
	}
	client.Env = mergeWorkspaceEnvironment(client.Env, values)
	if err := config.ValidateWorkspaceName(client.Name); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	docker, err := newDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
	observation, err := docker.Status(ctx, client.Name)
	if err != nil {
		return err
	}
	if observation.State != runtime.StateRunning || !observation.HasEndpoint {
		return fmt.Errorf("workspace %q is %s; start it before reading events", client.Name, observation.State)
	}
	env := forwardedEnvironment(client.Env)
	auth := runtime.ServerAuth{Password: env["OPENCODE_PASSWORD"]}
	if err := runtime.WaitHealthy(ctx, observation.Endpoint, auth, 60*time.Second); err != nil {
		return fmt.Errorf("wait for OpenCode health: %w", err)
	}
	events := make(chan watch.Event, eventBufferSize)
	// Intentionally untracked: StreamForever exits when ctx is canceled, and
	// ctx cancellation is this command's only exit path, so an errgroup would
	// never observe anything but that cancellation.
	go watch.StreamForever(ctx, watch.StreamOptions{
		BaseURL: observation.Endpoint.URL(), Auth: auth,
	}, events, log)
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-events:
			payload := event.Properties
			if len(payload) == 0 {
				payload = event.Data
			}
			properties := string(payload)
			if len(properties) > statusDetailLimit {
				properties = properties[:statusDetailLimit] + "..."
			}
			fmt.Printf("[%s] +%-8s %-28s %s\n", time.Now().Format("15:04:05.000"), time.Since(last).Round(time.Millisecond), event.Type, properties)
			last = time.Now()
		}
	}
}

// runDebugWake traces one workspace wake end to end through the running
// supervisor's operator listener: admission lease, lifecycle token, Docker
// mutation, health probe, and activity-observer attach. It requires the
// operator control credential and never talks to Docker directly, so it always
// measures the same path real client traffic takes.
func runDebugWake(args []string, log *slog.Logger) error {
	fs, nameFlag, configPath := workspaceFlags("debug wake")
	envPath := fs.String("env-file", "", "protected environment file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, _, err := loadCommandConfig(*configPath, true, *envPath, config.Overrides{
		Name: optionalFlag(fs, "name", nameFlag),
	})
	if err != nil {
		return err
	}
	ctx, cancel := commandContext()
	defer cancel()
	wakeCtx, cancelWake := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelWake()

	endpoint := "http://" + cfg.OperatorListen + "/fern/api/v1/debug/wake-trace"
	request, err := http.NewRequestWithContext(wakeCtx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	request.SetBasicAuth("fern", cfg.Control.Password)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("reach the operator listener at %s: %w (is 'fern up' running?)", cfg.OperatorListen, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read wake trace response: %w", err)
	}
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return errors.New("operator listener rejected the fern control credential")
	case http.StatusServiceUnavailable:
		return errors.New("workspace wake failed; see the supervisor logs for the classified reason")
	default:
		return fmt.Errorf("wake trace returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var trace workspace.WakeTrace
	if err := json.Unmarshal(body, &trace); err != nil {
		return fmt.Errorf("decode wake trace response: %w", err)
	}
	printWakeWaterfall(os.Stdout, trace)
	log.Debug("wake trace completed", "workspace", trace.Workspace, "total_ms", trace.TotalMillis)
	return nil
}

// wakeWaterfallWidth is the widest bar, in characters, of the debug wake span
// waterfall; shorter spans scale down proportionally.
const wakeWaterfallWidth = 24

func printWakeWaterfall(output io.Writer, trace workspace.WakeTrace) {
	fmt.Fprintf(output, "wake trace (%s)  total %dms  started %s\n",
		trace.Workspace, trace.TotalMillis, trace.StartedAt.Format("15:04:05.000"))
	widestName, widestOffset, longest := 0, 0, int64(1)
	for _, span := range trace.Spans {
		if len(span.Name) > widestName {
			widestName = len(span.Name)
		}
		if digits := len(fmt.Sprint(span.OffsetMs)); digits > widestOffset {
			widestOffset = digits
		}
		if span.Millis > longest {
			longest = span.Millis
		}
	}
	for _, span := range trace.Spans {
		bars := int(float64(span.Millis) / float64(longest) * wakeWaterfallWidth)
		if bars == 0 {
			bars = 1
		}
		fmt.Fprintf(output, "  %-*s +%*dms %6dms %s\n",
			widestName, span.Name, widestOffset, span.OffsetMs, span.Millis, strings.Repeat("█", bars))
	}
}

func runLogs(args []string, log *slog.Logger) error {

	fs, nameFlag, configPath := workspaceFlags("logs")
	follow := fs.Bool("follow", true, "follow log output")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	name, err := workspaceName(fs, *nameFlag, *configPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	docker, err := newDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
	err = docker.StreamLogs(ctx, name, *follow, os.Stdout, os.Stderr)
	if ctx.Err() != nil {
		return nil
	}
	return err
}
