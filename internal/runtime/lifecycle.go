package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
)

// Pause suspends workspace compute according to the configured suspend
// mechanism: a graceful docker stop (default) or a cgroup freezer pause.
// Domain "pause" always means stopped-or-suspended compute explained by an
// intent; it is distinct from Docker's freezer state alone, which fern
// surfaces as the Frozen observation flag and thaws before mutating.
func (d *Docker) Pause(ctx context.Context, name string) error {
	observation, err := d.Status(ctx, name)
	if err != nil {
		return err
	}
	return d.pauseObserved(ctx, name, observation)
}

// PrepareShutdown records that a managed running container may be stopped by
// Docker after Fern exits during an orderly service or host shutdown. A normal
// Fern restart clears this intent while adopting the still-running container.
func (d *Docker) PrepareShutdown(ctx context.Context, name string) error {
	observation, err := d.Status(ctx, name)
	if err != nil {
		return err
	}
	if !observation.Running || observation.ContainerID == "" || observation.State == StateFailed {
		return nil
	}
	if err := d.intents.CommitShutdown(name, observation.ContainerID, time.Now().Add(ShutdownIntentTTL)); err != nil {
		return fmt.Errorf("record orderly shutdown recovery intent: %w", err)
	}
	return nil
}

func (d *Docker) pauseObserved(ctx context.Context, name string, observation Observation) error {
	return d.pauseObservedWithOutcome(ctx, name, observation, false)
}

func (d *Docker) pauseObservedWithOutcome(ctx context.Context, name string, observation Observation, failedStart bool) error {
	if observation.State == StateAbsent {
		return fmt.Errorf("pause %q: workspace is absent", name)
	}
	if observation.State == StateFailed {
		return fmt.Errorf("%w: %s exited with code %d (oom=%t)", ErrFailed, name, observation.ExitCode, observation.OOMKilled)
	}
	if !observation.Running && !observation.Frozen {
		intent, err := d.intents.PauseStatus(name, observation.ContainerID, time.Time{})
		if err != nil {
			return fmt.Errorf("read pause intent: %w", err)
		}
		if observation.DockerStatus != "created" {
			return fmt.Errorf("pause %q has an unknown outcome: container stopped with pending pause intent", name)
		}
		if intent == PauseIntentNone {
			if err := d.intents.BeginPause(name, observation.ContainerID); err != nil {
				return fmt.Errorf("record pause intent: %w", err)
			}
			intent = PauseIntentPending
		}
		if intent == PauseIntentPending {
			if err := d.commitPauseOutcome(name, observation.ContainerID, failedStart); err != nil {
				return fmt.Errorf("commit pending pause intent: %w", err)
			}
		}
		return nil
	}
	if failedStart && observation.Frozen {
		intent, err := d.intents.PauseStatus(name, observation.ContainerID, time.Time{})
		if err != nil {
			return fmt.Errorf("read pause intent: %w", err)
		}
		if intent == PauseIntentCommitted || intent == PauseIntentShutdown {
			return nil
		}
	}
	if err := d.intents.BeginPause(name, observation.ContainerID); err != nil {
		return fmt.Errorf("record pause intent: %w", err)
	}
	if d.suspend == SuspendFreeze {
		// Freezer mode: suspend via the cgroup freezer. The process stays
		// resident, so wake is an unpause rather than a boot. The intent
		// journal still matters: frozen containers that later exit (host
		// reboot, daemon restart) classify as paused instead of failed.
		if observation.Frozen {
			if err := d.commitPauseOutcome(name, observation.ContainerID, failedStart); err != nil {
				return fmt.Errorf("commit pending pause intent: %w", err)
			}
			return nil
		}
		freezeStart := time.Now()
		if err := d.cli.ContainerPause(ctx, observation.ContainerID); err != nil {
			return d.reconcileFreezeError(ctx, name, observation.ContainerID, err)
		}
		if err := d.commitPauseOutcome(name, observation.ContainerID, failedStart); err != nil {
			return fmt.Errorf("commit pause intent: %w", err)
		}
		d.log.Info("state", "workspace", name, "from", StateRunning, "to", StatePaused,
			"mechanism", string(SuspendFreeze), "elapsed_ms", time.Since(freezeStart).Milliseconds())
		return nil
	}
	if observation.Frozen {
		if err := d.cli.ContainerUnpause(ctx, observation.ContainerID); err != nil {
			return fmt.Errorf("unpause %q before stop: %w", name, err)
		}
	}
	start := time.Now()
	timeout := stopGraceSeconds
	if err := d.cli.ContainerStop(ctx, observation.ContainerID, container.StopOptions{Timeout: &timeout}); err != nil {
		return d.reconcileStopError(ctx, name, observation.ContainerID, err)
	}
	if err := d.commitPauseOutcome(name, observation.ContainerID, failedStart); err != nil {
		return fmt.Errorf("commit pause intent: %w", err)
	}
	d.log.Info("state", "workspace", name, "from", StateRunning, "to", StatePaused,
		"mechanism", string(d.suspend), "elapsed_ms", time.Since(start).Milliseconds())
	return nil
}

func (d *Docker) commitPauseOutcome(name, containerID string, failedStart bool) error {
	if failedStart {
		return d.intents.CommitFailedStart(name, containerID)
	}
	return d.intents.CommitPause(name, containerID)
}

// reconcileFreezeError resolves an errored freeze response against observed
// reality: a container that is nonetheless frozen gets its intent committed,
// one that is still live gets the pending intent cleared, and anything else
// stays unresolved so classification never guesses.
func (d *Docker) reconcileFreezeError(ctx context.Context, name, containerID string, pauseErr error) error {
	observation, inspectErr := d.statusByReference(ctx, containerID, name)
	var followupErr error
	switch {
	case inspectErr == nil && observation.Frozen:
		followupErr = d.intents.CommitPause(name, containerID)
	case inspectErr == nil && observation.Running && !observation.Frozen:
		followupErr = d.intents.Clear(name)
	}
	return errors.Join(fmt.Errorf("pause %q has an unknown freeze outcome: %w", name, pauseErr), inspectErr, followupErr)
}

func (d *Docker) reconcileStopError(ctx context.Context, name, containerID string, stopErr error) error {
	observation, inspectErr := d.statusByReference(ctx, containerID, name)
	var clearErr error
	if inspectErr == nil && observation.Running {
		clearErr = d.intents.Clear(name)
	}
	// A failed Docker stop response cannot prove Fern caused a subsequently
	// observed exit. Preserve pending intent rather than disguising a crash as
	// an intentional pause.
	return errors.Join(fmt.Errorf("pause %q has an unknown stop outcome: %w", name, stopErr), inspectErr, clearErr)
}

// EnsureRunningObserved attests one inspection covering ownership, actual
// configuration, running state, endpoint, and immutable image ID, creating or
// resuming compute when required. On error the returned result is the zero
// value; partial results are never reported alongside failures.
func (d *Docker) EnsureRunningObserved(ctx context.Context, spec Spec) (RunningResult, error) {
	if err := spec.Validate(); err != nil {
		return RunningResult{}, err
	}
	inspection, err := d.inspectByReference(ctx, spec.Name, spec.Name)
	if err != nil {
		return RunningResult{}, err
	}
	observation := inspection.observation
	switch observation.State {
	case StateAbsent:
		observation, err := d.create(ctx, spec)
		if err != nil {
			return RunningResult{}, err
		}
		return RunningResult{Observation: observation, Transitioned: true}, nil
	case StatePaused, StateProvisioning:
		observation, err := d.resumeObserved(ctx, spec, inspection)
		if err != nil {
			return RunningResult{}, err
		}
		return RunningResult{Observation: observation, Transitioned: true}, nil
	case StateRunning:
		observation, err := d.resumeObserved(ctx, spec, inspection)
		if err != nil {
			return RunningResult{}, err
		}
		return RunningResult{Observation: observation}, nil
	case StateFailed:
		return RunningResult{}, fmt.Errorf("%w: inspect logs and run 'fern down' before recreating", ErrFailed)
	default:
		return RunningResult{}, fmt.Errorf("unexpected workspace state %q", observation.State)
	}
}

// ReconcileStartup adopts existing running compute and repairs interrupted
// provisioning without waking a paused workspace or creating an absent one.
// On error the returned result is the zero value; partial results are never
// reported alongside failures.
func (d *Docker) ReconcileStartup(ctx context.Context, spec Spec) (StartupResult, error) {
	if err := spec.Validate(); err != nil {
		return StartupResult{}, err
	}
	inspection, err := d.inspectByReference(ctx, spec.Name, spec.Name)
	if err != nil {
		return StartupResult{}, err
	}
	switch inspection.observation.State {
	case StateAbsent, StatePaused:
		return StartupResult{}, nil
	case StateRunning:
		observation, err := d.resumeObserved(ctx, spec, inspection)
		if err != nil {
			return StartupResult{}, err
		}
		return StartupResult{Endpoint: observation.Endpoint, ImageID: observation.ImageID, Running: true}, nil
	case StateProvisioning:
		observation, err := d.resumeObserved(ctx, spec, inspection)
		if err != nil {
			return StartupResult{}, err
		}
		return StartupResult{Endpoint: observation.Endpoint, ImageID: observation.ImageID, Running: true, Transitioned: true}, nil
	case StateFailed:
		return StartupResult{}, fmt.Errorf("%w: inspect logs and run 'fern down' before recreating", ErrFailed)
	default:
		return StartupResult{}, fmt.Errorf("unexpected workspace state %q", inspection.observation.State)
	}
}

func (d *Docker) resumeObserved(ctx context.Context, spec Spec, inspection workspaceInspection) (Observation, error) {
	observation := inspection.observation
	if observation.State == StateAbsent {
		return Observation{}, fmt.Errorf("resume %q: workspace is absent", spec.Name)
	}
	wantFingerprint, err := specFingerprint(spec)
	if err != nil {
		return Observation{}, err
	}
	if observation.SpecFingerprint != wantFingerprint {
		return Observation{}, fmt.Errorf("%w: run 'fern down' before applying changed image, repository, memory, or environment", ErrSpecDrift)
	}
	if err := verifyActualSpec(inspection.info, spec); err != nil {
		return Observation{}, err
	}
	if observation.State == StateFailed {
		return Observation{}, fmt.Errorf("%w: %s exited with code %d (oom=%t); inspect logs, then run 'fern down' to recreate", ErrFailed, spec.Name, observation.ExitCode, observation.OOMKilled)
	}

	start := time.Now()
	old := observation.State
	transitioned := observation.Frozen || !observation.Running
	containerID := observation.ContainerID
	if observation.Frozen {
		if err := d.cli.ContainerUnpause(ctx, observation.ContainerID); err != nil {
			return Observation{}, d.rollbackStarted(spec.Name, containerID, fmt.Errorf("unpause %q: %w", spec.Name, err))
		}
		recordSpan(ctx, "docker_unpause", start)
	} else if !observation.Running {
		if err := d.cli.ContainerStart(ctx, observation.ContainerID, container.StartOptions{}); err != nil {
			return Observation{}, d.rollbackStarted(spec.Name, containerID, fmt.Errorf("start %q: %w", spec.Name, err))
		}
		recordSpan(ctx, "docker_start", start)
	}
	if transitioned {
		inspectStart := time.Now()
		observation, err = d.statusByReference(ctx, containerID, spec.Name)
		recordSpan(ctx, "docker_inspect", inspectStart)
		if err != nil {
			return Observation{}, d.rollbackIfTransitioned(true, spec.Name, containerID, err)
		}
	}
	if observation.State != StateRunning {
		return Observation{}, d.rollbackIfTransitioned(transitioned, spec.Name, containerID, fmt.Errorf("workspace %q is %s after resume", spec.Name, observation.State))
	}
	if !observation.HasEndpoint {
		return Observation{}, d.rollbackIfTransitioned(transitioned, spec.Name, containerID, fmt.Errorf("workspace %q has no %s port binding", spec.Name, workspacePort))
	}
	healthStart := time.Now()
	if err := WaitHealthy(ctx, observation.Endpoint, spec.ServerAuth(), healthTimeout); err != nil {
		recordSpan(ctx, "health_probe", healthStart)
		return Observation{}, d.rollbackIfTransitioned(transitioned, spec.Name, containerID, fmt.Errorf("workspace %q did not become healthy: %w", spec.Name, err))
	}
	recordSpan(ctx, "health_probe", healthStart)
	if err := d.intents.Clear(spec.Name); err != nil {
		return Observation{}, d.rollbackIfTransitioned(transitioned, spec.Name, containerID, fmt.Errorf("clear pause intent after resume: %w", err))
	}
	if old != StateRunning {
		d.log.Info("state", "workspace", spec.Name, "from", old, "to", StateRunning, "elapsed_ms", time.Since(start).Milliseconds())
	}
	return observation, nil
}

// Destroy removes compute but deliberately retains the Fern-owned data volume.
func (d *Docker) Destroy(ctx context.Context, name string) error {
	observation, err := d.Status(ctx, name)
	if err != nil {
		return err
	}
	if observation.State == StateAbsent {
		if err := d.intents.Clear(name); err != nil {
			return fmt.Errorf("clear pause intent for absent workspace: %w", err)
		}
		return nil
	}
	if observation.Frozen {
		if err := d.cli.ContainerUnpause(ctx, observation.ContainerID); err != nil {
			return fmt.Errorf("unpause %q before destroy: %w", name, err)
		}
		observation.Running = true
	}
	if observation.Running {
		timeout := stopGraceSeconds
		if err := d.cli.ContainerStop(ctx, observation.ContainerID, container.StopOptions{Timeout: &timeout}); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("stop %q before destroy: %w", name, err)
		}
	}
	if err := d.cli.ContainerRemove(ctx, observation.ContainerID, container.RemoveOptions{}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove %q: %w", name, err)
	}
	if err := d.intents.Clear(name); err != nil {
		return fmt.Errorf("clear pause intent after destroy: %w", err)
	}
	d.log.Info("state", "workspace", name, "from", observation.State, "to", StateAbsent)
	return nil
}

func (d *Docker) rollbackIfTransitioned(transitioned bool, name, containerID string, cause error) error {
	if !transitioned {
		return cause
	}
	return d.rollbackStarted(name, containerID, cause)
}

func (d *Docker) rollbackStarted(name, containerID string, cause error) error {
	cleanupCtx, cancel := detachedContext(context.Background(), cleanupTimeout)
	defer cancel()
	observation, err := d.statusByReference(cleanupCtx, containerID, name)
	if err != nil {
		return errors.Join(cause, err)
	}
	return errors.Join(cause, d.pauseObservedWithOutcome(cleanupCtx, name, observation, true))
}
