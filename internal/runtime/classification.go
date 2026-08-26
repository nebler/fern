package runtime

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
)

type workspaceInspection struct {
	observation Observation
	info        container.InspectResponse
}

func (d *Docker) Status(ctx context.Context, name string) (Observation, error) {
	return d.statusByReference(ctx, name, name)
}

func (d *Docker) statusByReference(ctx context.Context, reference, workspace string) (Observation, error) {
	inspection, err := d.inspectByReference(ctx, reference, workspace)
	return inspection.observation, err
}

// inspectByReference maps one Docker inspect response onto an Observation.
// Classification is order-dependent: see the case comments before reordering.
func (d *Docker) inspectByReference(ctx context.Context, reference, workspace string) (workspaceInspection, error) {
	info, err := d.cli.ContainerInspect(ctx, reference)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return workspaceInspection{observation: Observation{State: StateAbsent}}, nil
		}
		return workspaceInspection{}, fmt.Errorf("inspect %q: %w", workspace, err)
	}
	if info.Config == nil || info.State == nil || info.NetworkSettings == nil {
		return workspaceInspection{}, fmt.Errorf("inspect %q: incomplete Docker state", workspace)
	}
	if info.Config.Labels[managedLabel] != labelTrue || info.Config.Labels[workspaceLabel] != workspace {
		return workspaceInspection{}, fmt.Errorf("%w: container %q", ErrUnmanaged, workspace)
	}
	if !ValidImageID(info.Image) {
		return workspaceInspection{}, fmt.Errorf("%w: Docker returned an invalid actual image ID for container %q", ErrSpecDrift, workspace)
	}

	observation := Observation{
		ContainerID:     info.ID,
		ImageID:         info.Image,
		DockerStatus:    info.State.Status,
		Running:         info.State.Running,
		Frozen:          info.State.Paused,
		OOMKilled:       info.State.OOMKilled,
		ExitCode:        info.State.ExitCode,
		SpecFingerprint: info.Config.Labels[specFingerprintLabel],
	}
	if bindings := info.NetworkSettings.Ports[nat.Port(workspacePort)]; len(bindings) > 0 {
		if len(bindings) != 1 || !isLoopbackBinding(bindings[0].HostIP) {
			return workspaceInspection{}, fmt.Errorf("%w: OpenCode port is not bound exclusively to loopback", ErrSpecDrift)
		}
		port, err := strconv.Atoi(bindings[0].HostPort)
		if err != nil {
			return workspaceInspection{}, fmt.Errorf("parse workspace port %q: %w", bindings[0].HostPort, err)
		}
		if port <= 0 || port > 65535 {
			return workspaceInspection{}, fmt.Errorf("invalid workspace port %d", port)
		}
		observation.Endpoint = Endpoint{Host: bindings[0].HostIP, Port: port}
		observation.HasEndpoint = true
	}

	switch {
	case info.State.Restarting:
		// Deliberate mapping: a mid-restart container exposes no safe mutating
		// action, and Docker's restart policy means it may still settle into
		// any terminal state. Provisioning semantics (wait only) are sound;
		// surfacing it as failed would invite destructive recovery too early.
		observation.State = StateProvisioning
	case info.State.Status == "created":
		intent, err := d.intents.PauseStatus(workspace, info.ID, time.Time{})
		if err != nil {
			return workspaceInspection{}, fmt.Errorf("read pause intent: %w", err)
		}
		if intent == PauseIntentFailedStart {
			observation.State = StateFailed
		} else if intent == PauseIntentCommitted {
			observation.State = StatePaused
		} else {
			observation.State = StateProvisioning
		}
	case info.State.OOMKilled || info.State.Dead:
		// Order-dependent: OOMKilled/Dead must be tested before the exited
		// case below, because Docker reports Status=="exited" alongside them
		// and that would misclassify a crash as a repairable intentional stop.
		observation.State = StateFailed
	case info.State.Status == "exited":
		var stoppedAt time.Time
		if info.State.FinishedAt != "" {
			stoppedAt, err = time.Parse(time.RFC3339Nano, info.State.FinishedAt)
			if err != nil {
				return workspaceInspection{}, fmt.Errorf("parse container finish time: %w", err)
			}
		}
		intent, err := d.intents.PauseStatus(workspace, info.ID, stoppedAt)
		if err != nil {
			return workspaceInspection{}, fmt.Errorf("read pause intent: %w", err)
		}
		switch intent {
		case PauseIntentFailedStart:
			observation.State = StateFailed
		case PauseIntentCommitted, PauseIntentShutdown:
			observation.State = StatePaused
		case PauseIntentPending:
			// Interrupted-pause repairable: the pending intent proves Fern
			// itself ordered this stop, so the workspace is mid-transition
			// rather than crashed. Committing or clearing the intent is a
			// later, deliberate step; classification only refuses to guess.
			observation.State = StateProvisioning
		default:
			observation.State = StateFailed
		}
	case info.State.Running && !info.State.Paused:
		observation.State = StateRunning
	case info.State.Running && info.State.Paused:
		intent, err := d.intents.PauseStatus(workspace, info.ID, time.Time{})
		if err != nil {
			return workspaceInspection{}, fmt.Errorf("read pause intent: %w", err)
		}
		if intent == PauseIntentFailedStart {
			observation.State = StateFailed
		} else {
			observation.State = StatePaused
		}
	default:
		return workspaceInspection{}, fmt.Errorf("unsupported Docker state %q for workspace %q", info.State.Status, workspace)
	}
	return workspaceInspection{observation: observation, info: info}, nil
}

// ValidImageID accepts only Docker's canonical immutable image identifier.
func ValidImageID(imageID string) bool {
	if len(imageID) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(imageID, "sha256:") {
		return false
	}
	for _, char := range imageID[len("sha256:"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func isLoopbackBinding(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
