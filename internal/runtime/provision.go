package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
)

func (d *Docker) create(ctx context.Context, spec Spec) (observation Observation, resultErr error) {
	var createdVolumes []string
	retainVolume := false
	defer func() {
		if retainVolume {
			return
		}
		cleanupCtx, cancel := detachedContext(ctx, cleanupTimeout)
		defer cancel()
		for _, name := range createdVolumes {
			resultErr = errors.Join(resultErr, d.cli.VolumeRemove(cleanupCtx, name, false))
		}
	}()
	for _, name := range specVolumeNames(spec) {
		created, err := d.ensureVolume(ctx, spec.Name, name)
		if err != nil {
			return Observation{}, err
		}
		if created {
			createdVolumes = append(createdVolumes, name)
		}
	}
	if err := d.intents.Clear(spec.Name); err != nil {
		return Observation{}, fmt.Errorf("clear stale pause intent: %w", err)
	}

	env := sortedEnv(specEnvironment(spec))
	fingerprint, err := specFingerprint(spec)
	if err != nil {
		return Observation{}, err
	}
	port := nat.Port(workspacePort)
	useInit := true
	pidsLimit := workspacePIDs
	createStart := time.Now()
	created, err := d.cli.ContainerCreate(
		ctx,
		&container.Config{
			Image:        spec.Image,
			User:         workspaceUser,
			Env:          env,
			ExposedPorts: nat.PortSet{port: struct{}{}},
			Labels: map[string]string{
				managedLabel:         labelTrue,
				workspaceLabel:       spec.Name,
				specFingerprintLabel: fingerprint,
			},
		},
		&container.HostConfig{
			PortBindings: nat.PortMap{port: []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "0"}}},
			Resources: container.Resources{
				Memory: spec.MemoryBytes, NanoCPUs: workspaceNanoCPUs, PidsLimit: &pidsLimit,
			},
			Init:        &useInit,
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			Mounts:      specMounts(spec),
		},
		&network.NetworkingConfig{},
		nil,
		spec.Name,
	)
	if err != nil {
		return Observation{}, fmt.Errorf("create container %q: %w", spec.Name, err)
	}

	recordSpan(ctx, "docker_create", createStart)

	start := time.Now()
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		cause := fmt.Errorf("start container %q: %w", spec.Name, err)
		cleanupCtx, cancel := detachedContext(ctx, cleanupTimeout)
		defer cancel()
		removeErr := d.cli.ContainerRemove(cleanupCtx, created.ID, container.RemoveOptions{Force: true})
		return Observation{}, errors.Join(cause, removeErr)
	}
	recordSpan(ctx, "docker_start", start)
	// Once OpenCode starts, retain its data even if health or observation setup
	// later fails. Only a never-started initial workspace is safe to roll back.
	retainVolume = true
	inspectStart := time.Now()
	observation, err = d.statusByReference(ctx, created.ID, spec.Name)
	recordSpan(ctx, "docker_inspect", inspectStart)
	if err != nil {
		return Observation{}, d.rollbackStarted(spec.Name, created.ID, err)
	}
	if observation.State != StateRunning {
		return Observation{}, d.rollbackStarted(spec.Name, created.ID, fmt.Errorf("workspace %q is %s after start", spec.Name, observation.State))
	}
	if !observation.HasEndpoint {
		return Observation{}, d.rollbackStarted(spec.Name, created.ID, fmt.Errorf("workspace %q has no %s port binding", spec.Name, workspacePort))
	}
	healthStart := time.Now()
	if err := WaitHealthy(ctx, observation.Endpoint, spec.ServerAuth(), healthTimeout); err != nil {
		recordSpan(ctx, "health_probe", healthStart)
		return Observation{}, d.rollbackStarted(spec.Name, created.ID, fmt.Errorf("container %q never became healthy: %w", spec.Name, err))
	}
	recordSpan(ctx, "health_probe", healthStart)
	d.log.Info("state", "workspace", spec.Name, "from", StateProvisioning, "to", StateRunning, "elapsed_ms", time.Since(start).Milliseconds())
	return observation, nil
}

func (d *Docker) ensureVolume(ctx context.Context, workspace, name string) (bool, error) {
	existing, err := d.cli.VolumeInspect(ctx, name)
	if err == nil {
		if existing.Labels[managedLabel] != labelTrue || existing.Labels[workspaceLabel] != workspace {
			return false, fmt.Errorf("%w: volume %q", ErrUnmanaged, name)
		}
		return false, nil
	}
	if !errdefs.IsNotFound(err) {
		return false, fmt.Errorf("inspect data volume %q: %w", name, err)
	}
	created, err := d.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name: name,
		Labels: map[string]string{
			managedLabel:   labelTrue,
			workspaceLabel: workspace,
		},
	})
	if err != nil {
		return false, fmt.Errorf("create data volume %q: %w", name, err)
	}
	// VolumeCreate is idempotent by name. Another actor can create the volume
	// after our inspect, so the returned object must be treated as untrusted.
	if created.Labels[managedLabel] != labelTrue || created.Labels[workspaceLabel] != workspace {
		return false, fmt.Errorf("%w: volume %q", ErrUnmanaged, name)
	}
	return true, nil
}

func specDataVolumeName(spec Spec) string {
	return "fern-" + spec.Name + "-v2-data"
}

func specGHVolumeName(spec Spec) string {
	if !spec.WorkspaceGH {
		return ""
	}
	return "fern-" + spec.Name + "-v1-gh-config"
}

func specVolumeNames(spec Spec) []string {
	names := []string{specDataVolumeName(spec)}
	if spec.WorkspaceGH {
		names = append(names, specGHVolumeName(spec))
	}
	return names
}
