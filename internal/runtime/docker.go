package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

const (
	workspacePort           = "4096/tcp"
	githubConfigDir         = "/home/user/.config/gh"
	githubConfigEnv         = "GH_CONFIG_DIR"
	workspaceGHBinary       = "/usr/local/bin/gh"
	healthTimeout           = 60 * time.Second
	workspaceNanoCPUs       = int64(2_000_000_000)
	workspacePIDs           = int64(512)
	managedLabel            = "dev.fern.managed"
	workspaceLabel          = "dev.fern.workspace"
	specFingerprintLabel    = "dev.fern.spec"
	backgroundRevisionLabel = "org.opencontainers.image.revision"
	backgroundSourceLabel   = "org.opencontainers.image.source"
	backgroundVersionLabel  = "org.opencontainers.image.version"
	backgroundProfileLabel  = "ai.fern.opencode.profile"

	// labelTrue is the value every managed-resource label must carry.
	labelTrue = "true"
	// workspaceUser is the unprivileged UID:GID used for the container's
	// configured user and for every exec into a running workspace.
	workspaceUser = "1001:1001"

	repoMountTarget = "/home/user/workspace"
	dataMountTarget = "/home/user/.local/share/opencode"

	// stopGraceSeconds bounds how long Docker lets a container exit on its own
	// after SIGTERM before SIGKILL during an intentional stop.
	stopGraceSeconds = 10
	// cleanupTimeout bounds best-effort rollback API calls issued after the
	// caller's context has failed or can no longer be trusted.
	cleanupTimeout = 15 * time.Second
)

const (
	BackgroundOpenCodeRevision = "39fb919a054190498f6d5b7985bde231f93ad7a6"
	BackgroundOpenCodeProfile  = "source-39fb919a054190498f6d5b7985bde231f93ad7a6"
	BackgroundOpenCodeVersion  = "0.0.0-source-39fb919a054190498f6d5b7985bde231f93ad7a6"
	BackgroundOpenCodeSource   = "https://github.com/anomalyco/opencode"
)

var (
	ErrCommandFailed      = errors.New("workspace command failed")
	ErrCommandOutputLimit = errors.New("workspace command output exceeded limit")
)

// Docker manages Fern workspace containers through the local Docker daemon,
// attesting ownership, configuration, and image identity on every operation.
type Docker struct {
	cli     *client.Client
	log     *slog.Logger
	intents IntentStore
	suspend SuspendKind
}

func NewDocker(log *slog.Logger, intents IntentStore, suspend SuspendKind) (*Docker, error) {
	if intents == nil {
		return nil, errors.New("runtime intent store is required")
	}
	switch suspend {
	case SuspendStop, SuspendFreeze:
	case "":
		suspend = SuspendStop
	default:
		return nil, fmt.Errorf("unsupported idle suspend mechanism %q", suspend)
	}
	if log == nil {
		log = slog.Default()
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	return &Docker{cli: cli, log: log, intents: intents, suspend: suspend}, nil
}

func (d *Docker) Close() error {
	return d.cli.Close()
}

// ResolveImageID reads Docker's immutable local image identity for a configured
// reference without creating, starting, or mutating a container.
func (d *Docker) ResolveImageID(ctx context.Context, reference string) (string, error) {
	if strings.TrimSpace(reference) != reference || reference == "" {
		return "", fmt.Errorf("%w: image reference", ErrSpecDrift)
	}
	inspection, err := d.cli.ImageInspect(ctx, reference)
	if err != nil {
		return "", fmt.Errorf("inspect workspace image: %w", err)
	}
	if !ValidImageID(inspection.ID) {
		return "", fmt.Errorf("%w: Docker returned a noncanonical image ID", ErrSpecDrift)
	}
	return inspection.ID, nil
}

// ResolveBackgroundRunImageID qualifies the immutable source image without
// creating, starting, or changing any Docker resource.
func (d *Docker) ResolveBackgroundRunImageID(ctx context.Context, reference, expectedID string) (string, error) {
	if strings.TrimSpace(reference) != reference || reference == "" || !ValidImageID(expectedID) {
		return "", fmt.Errorf("%w: background image reference", ErrSpecDrift)
	}
	inspection, err := d.cli.ImageInspect(ctx, reference)
	if err != nil {
		return "", fmt.Errorf("inspect background run image: %w", err)
	}
	if inspection.ID != expectedID || inspection.Config == nil ||
		inspection.Config.Labels[backgroundSourceLabel] != BackgroundOpenCodeSource ||
		inspection.Config.Labels[backgroundRevisionLabel] != BackgroundOpenCodeRevision ||
		inspection.Config.Labels[backgroundVersionLabel] != BackgroundOpenCodeVersion ||
		inspection.Config.Labels[backgroundProfileLabel] != BackgroundOpenCodeProfile ||
		inspection.Config.User != "1001:1001" ||
		len(inspection.Config.Entrypoint) != 0 ||
		!slices.Equal(inspection.Config.Cmd, []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096"}) ||
		!hasExactExposedPort(inspection.Config.ExposedPorts, "4096/tcp") ||
		containsEnvironmentKey(inspection.Config.Env, "OPENCODE_SERVER_PASSWORD") {
		return "", fmt.Errorf("%w: background image does not match the qualified source profile", ErrSpecDrift)
	}
	return inspection.ID, nil
}

func hasExactExposedPort(ports nat.PortSet, expected nat.Port) bool {
	_, exists := ports[expected]
	return exists && len(ports) == 1
}

func containsEnvironmentKey(environment []string, key string) bool {
	prefix := key + "="
	for _, value := range environment {
		if value == key || strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func (d *Docker) StreamLogs(ctx context.Context, name string, follow bool, stdout, stderr io.Writer) error {
	observation, err := d.Status(ctx, name)
	if err != nil {
		return err
	}
	if observation.State == StateAbsent {
		return fmt.Errorf("logs %q: workspace is absent", name)
	}
	stream, err := d.cli.ContainerLogs(ctx, observation.ContainerID, container.LogsOptions{ShowStdout: true, ShowStderr: true, Follow: follow})
	if err != nil {
		return fmt.Errorf("read logs for %q: %w", name, err)
	}
	defer stream.Close()
	_, err = stdcopy.StdCopy(stdout, stderr, stream)
	return err
}

// detachedContext derives a bounded context for post-failure cleanup that
// survives caller cancellation while retaining parent trace values. The
// parent's cancellation is deliberately dropped: rollback must reach the
// Docker API precisely when the original operation failed or was canceled.
func detachedContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}
