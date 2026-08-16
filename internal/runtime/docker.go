package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

const (
	workspacePort        = "4096/tcp"
	healthTimeout        = 60 * time.Second
	stopReconcileTimeout = 12 * time.Second
	managedLabel         = "dev.fern.managed"
	workspaceLabel       = "dev.fern.workspace"
	specFingerprintLabel = "dev.fern.spec"
)

type Docker struct {
	cli     *client.Client
	log     *slog.Logger
	intents IntentStore
}

func NewDocker(log *slog.Logger, intents IntentStore) (*Docker, error) {
	if intents == nil {
		return nil, errors.New("runtime intent store is required")
	}
	if log == nil {
		log = slog.Default()
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	return &Docker{cli: cli, log: log, intents: intents}, nil
}

func (d *Docker) Close() error {
	return d.cli.Close()
}

func (d *Docker) Create(ctx context.Context, spec Spec) (Endpoint, error) {
	if err := spec.Validate(); err != nil {
		return Endpoint{}, err
	}
	existing, err := d.Status(ctx, spec.Name)
	if err != nil {
		return Endpoint{}, err
	}
	if existing.State != StateAbsent {
		return Endpoint{}, fmt.Errorf("create %q: workspace already exists", spec.Name)
	}
	return d.create(ctx, spec)
}

func (d *Docker) create(ctx context.Context, spec Spec) (Endpoint, error) {
	if err := d.ensureVolume(ctx, spec); err != nil {
		return Endpoint{}, err
	}
	if err := d.intents.Clear(spec.Name); err != nil {
		return Endpoint{}, fmt.Errorf("clear stale pause intent: %w", err)
	}

	env := sortedEnv(spec.Env)
	fingerprint, err := specFingerprint(spec)
	if err != nil {
		return Endpoint{}, err
	}
	port := nat.Port(workspacePort)
	useInit := true
	created, err := d.cli.ContainerCreate(
		ctx,
		&container.Config{
			Image:        spec.Image,
			Env:          env,
			ExposedPorts: nat.PortSet{port: struct{}{}},
			Labels: map[string]string{
				managedLabel:         "true",
				workspaceLabel:       spec.Name,
				specFingerprintLabel: fingerprint,
			},
		},
		&container.HostConfig{
			PortBindings: nat.PortMap{port: []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "0"}}},
			Resources:    container.Resources{Memory: spec.MemoryBytes},
			Init:         &useInit,
			Mounts: []mount.Mount{
				{Type: mount.TypeBind, Source: spec.RepoPath, Target: "/home/user/workspace"},
				{Type: mount.TypeVolume, Source: specDataVolumeName(spec), Target: "/home/user/.local/share/opencode"},
			},
		},
		&network.NetworkingConfig{},
		nil,
		spec.Name,
	)
	if err != nil {
		return Endpoint{}, fmt.Errorf("create container %q: %w", spec.Name, err)
	}

	start := time.Now()
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		cause := fmt.Errorf("start container %q: %w", spec.Name, err)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		removeErr := d.cli.ContainerRemove(cleanupCtx, created.ID, container.RemoveOptions{Force: true})
		return Endpoint{}, errors.Join(cause, removeErr)
	}
	observation, err := d.statusByReference(ctx, created.ID, spec.Name)
	if err != nil {
		return Endpoint{}, d.rollbackStarted(spec.Name, created.ID, err)
	}
	if !observation.HasEndpoint {
		return Endpoint{}, d.rollbackStarted(spec.Name, created.ID, fmt.Errorf("workspace %q has no %s port binding", spec.Name, workspacePort))
	}
	protocol, err := WaitHealthyProtocol(ctx, observation.Endpoint, spec.ServerAuth(), spec.Protocol, healthTimeout)
	if err != nil {
		return Endpoint{}, d.rollbackStarted(spec.Name, created.ID, fmt.Errorf("container %q never became healthy: %w", spec.Name, err))
	}
	observation.Endpoint.Protocol = protocol
	d.log.Info("state", "workspace", spec.Name, "from", StateProvisioning, "to", StateRunning, "elapsed_ms", time.Since(start).Milliseconds())
	return observation.Endpoint, nil
}

func (d *Docker) Pause(ctx context.Context, name string) error {
	observation, err := d.Status(ctx, name)
	if err != nil {
		return err
	}
	return d.pauseObserved(ctx, name, observation)
}

func (d *Docker) pauseObserved(ctx context.Context, name string, observation Observation) error {
	if observation.State == StateAbsent {
		return fmt.Errorf("pause %q: workspace is absent", name)
	}
	if observation.State == StateFailed {
		return fmt.Errorf("%w: %s exited with code %d (oom=%t)", ErrFailed, name, observation.ExitCode, observation.OOMKilled)
	}
	if !observation.Running && !observation.Frozen {
		intent, err := d.intents.PauseStatus(name, observation.ContainerID)
		if err != nil {
			return fmt.Errorf("read pause intent: %w", err)
		}
		if intent == PauseIntentNone {
			if err := d.intents.BeginPause(name, observation.ContainerID); err != nil {
				return fmt.Errorf("record pause intent: %w", err)
			}
			intent = PauseIntentPending
		}
		if intent == PauseIntentPending {
			if err := d.intents.CommitPause(name, observation.ContainerID); err != nil {
				return fmt.Errorf("commit pending pause intent: %w", err)
			}
		}
		return nil
	}
	if err := d.intents.BeginPause(name, observation.ContainerID); err != nil {
		return fmt.Errorf("record pause intent: %w", err)
	}
	if observation.Frozen {
		if err := d.cli.ContainerUnpause(ctx, observation.ContainerID); err != nil {
			return fmt.Errorf("unpause %q before stop: %w", name, err)
		}
	}
	start := time.Now()
	timeout := 10
	if err := d.cli.ContainerStop(ctx, observation.ContainerID, container.StopOptions{Timeout: &timeout}); err != nil {
		return d.reconcileStopError(name, observation.ContainerID, err)
	}
	if err := d.intents.CommitPause(name, observation.ContainerID); err != nil {
		return fmt.Errorf("commit pause intent: %w", err)
	}
	d.log.Info("state", "workspace", name, "from", StateRunning, "to", StatePaused, "elapsed_ms", time.Since(start).Milliseconds())
	return nil
}

func (d *Docker) reconcileStopError(name, containerID string, stopErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), stopReconcileTimeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		observation, err := d.statusByReference(ctx, containerID, name)
		if err == nil && observation.State == StateProvisioning && !observation.Running {
			if err := d.intents.CommitPause(name, containerID); err != nil {
				return errors.Join(fmt.Errorf("pause %q has an unknown stop outcome: %w", name, stopErr), fmt.Errorf("commit reconciled pause intent: %w", err))
			}
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return d.finishStopReconciliation(name, containerID, stopErr)
		}
	}
}

func (d *Docker) finishStopReconciliation(name, containerID string, stopErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	observation, inspectErr := d.statusByReference(ctx, containerID, name)
	if inspectErr == nil && observation.State == StateProvisioning && !observation.Running {
		if err := d.intents.CommitPause(name, containerID); err == nil {
			return nil
		} else {
			return errors.Join(fmt.Errorf("pause %q has an unknown stop outcome: %w", name, stopErr), fmt.Errorf("commit reconciled pause intent: %w", err))
		}
	}
	var clearErr error
	if inspectErr == nil && observation.Running {
		clearErr = d.intents.Clear(name)
	}
	return errors.Join(fmt.Errorf("pause %q has an unknown stop outcome: %w", name, stopErr), inspectErr, clearErr)
}

func (d *Docker) Resume(ctx context.Context, spec Spec) (Endpoint, error) {
	if err := spec.Validate(); err != nil {
		return Endpoint{}, err
	}
	inspection, err := d.inspectByReference(ctx, spec.Name, spec.Name)
	if err != nil {
		return Endpoint{}, err
	}
	return d.resumeObserved(ctx, spec, inspection)
}

func (d *Docker) EnsureRunning(ctx context.Context, spec Spec) (Endpoint, bool, error) {
	if err := spec.Validate(); err != nil {
		return Endpoint{}, false, err
	}
	inspection, err := d.inspectByReference(ctx, spec.Name, spec.Name)
	if err != nil {
		return Endpoint{}, false, err
	}
	observation := inspection.observation
	switch observation.State {
	case StateAbsent:
		ep, err := d.create(ctx, spec)
		return ep, true, err
	case StatePaused, StateProvisioning:
		ep, err := d.resumeObserved(ctx, spec, inspection)
		return ep, true, err
	case StateRunning:
		ep, err := d.resumeObserved(ctx, spec, inspection)
		return ep, false, err
	case StateFailed:
		return Endpoint{}, false, fmt.Errorf("%w: inspect logs and run 'fern down' before recreating", ErrFailed)
	default:
		return Endpoint{}, false, fmt.Errorf("unexpected workspace state %q", observation.State)
	}
}

func (d *Docker) resumeObserved(ctx context.Context, spec Spec, inspection workspaceInspection) (Endpoint, error) {
	observation := inspection.observation
	if observation.State == StateAbsent {
		return Endpoint{}, fmt.Errorf("resume %q: workspace is absent", spec.Name)
	}
	wantFingerprint, err := specFingerprint(spec)
	if err != nil {
		return Endpoint{}, err
	}
	if observation.SpecFingerprint != wantFingerprint {
		return Endpoint{}, fmt.Errorf("%w: run 'fern down' before applying changed image, repository, memory, or environment", ErrSpecDrift)
	}
	if err := verifyActualSpec(inspection.info, spec); err != nil {
		return Endpoint{}, err
	}
	if observation.State == StateFailed {
		return Endpoint{}, fmt.Errorf("%w: %s exited with code %d (oom=%t); inspect logs, then run 'fern down' to recreate", ErrFailed, spec.Name, observation.ExitCode, observation.OOMKilled)
	}

	start := time.Now()
	old := observation.State
	transitioned := observation.Frozen || !observation.Running
	containerID := observation.ContainerID
	if observation.Frozen {
		if err := d.cli.ContainerUnpause(ctx, observation.ContainerID); err != nil {
			return Endpoint{}, d.rollbackStarted(spec.Name, containerID, fmt.Errorf("unpause %q: %w", spec.Name, err))
		}
	} else if !observation.Running {
		if err := d.cli.ContainerStart(ctx, observation.ContainerID, container.StartOptions{}); err != nil {
			return Endpoint{}, d.rollbackStarted(spec.Name, containerID, fmt.Errorf("start %q: %w", spec.Name, err))
		}
	}
	if transitioned {
		observation, err = d.statusByReference(ctx, containerID, spec.Name)
		if err != nil {
			return Endpoint{}, d.rollbackIfTransitioned(true, spec.Name, containerID, err)
		}
	}
	if !observation.HasEndpoint {
		return Endpoint{}, d.rollbackIfTransitioned(transitioned, spec.Name, containerID, fmt.Errorf("workspace %q has no %s port binding", spec.Name, workspacePort))
	}
	protocol, err := WaitHealthyProtocol(ctx, observation.Endpoint, spec.ServerAuth(), spec.Protocol, healthTimeout)
	if err != nil {
		return Endpoint{}, d.rollbackIfTransitioned(transitioned, spec.Name, containerID, fmt.Errorf("workspace %q did not become healthy: %w", spec.Name, err))
	}
	observation.Endpoint.Protocol = protocol
	if err := d.intents.Clear(spec.Name); err != nil {
		return Endpoint{}, d.rollbackIfTransitioned(transitioned, spec.Name, containerID, fmt.Errorf("clear pause intent after resume: %w", err))
	}
	if old != StateRunning {
		d.log.Info("state", "workspace", spec.Name, "from", old, "to", StateRunning, "elapsed_ms", time.Since(start).Milliseconds())
	}
	return observation.Endpoint, nil
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

func (d *Docker) rollbackIfTransitioned(transitioned bool, name, containerID string, cause error) error {
	if !transitioned {
		return cause
	}
	return d.rollbackStarted(name, containerID, cause)
}

// Destroy removes compute but deliberately retains the Fern-owned data volume.
func (d *Docker) Destroy(ctx context.Context, name string) error {
	observation, err := d.Status(ctx, name)
	if err != nil {
		return err
	}
	if observation.State == StateAbsent {
		return nil
	}
	if observation.Frozen {
		if err := d.cli.ContainerUnpause(ctx, observation.ContainerID); err != nil {
			return fmt.Errorf("unpause %q before destroy: %w", name, err)
		}
		observation.Running = true
	}
	if observation.Running {
		timeout := 10
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

func (d *Docker) Status(ctx context.Context, name string) (Observation, error) {
	return d.statusByReference(ctx, name, name)
}

func (d *Docker) statusByReference(ctx context.Context, reference, workspace string) (Observation, error) {
	inspection, err := d.inspectByReference(ctx, reference, workspace)
	return inspection.observation, err
}

type workspaceInspection struct {
	observation Observation
	info        container.InspectResponse
}

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
	if info.Config.Labels[managedLabel] != "true" || info.Config.Labels[workspaceLabel] != workspace {
		return workspaceInspection{}, fmt.Errorf("%w: container %q", ErrUnmanaged, workspace)
	}

	observation := Observation{
		ContainerID:     info.ID,
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
		observation.State = StateProvisioning
	case info.State.Status == "created":
		intent, err := d.intents.PauseStatus(workspace, info.ID)
		if err != nil {
			return workspaceInspection{}, fmt.Errorf("read pause intent: %w", err)
		}
		if intent == PauseIntentCommitted {
			observation.State = StatePaused
		} else {
			observation.State = StateProvisioning
		}
	case info.State.OOMKilled || info.State.Dead:
		observation.State = StateFailed
	case info.State.Status == "exited":
		intent, err := d.intents.PauseStatus(workspace, info.ID)
		if err != nil {
			return workspaceInspection{}, fmt.Errorf("read pause intent: %w", err)
		}
		switch intent {
		case PauseIntentCommitted:
			observation.State = StatePaused
		case PauseIntentPending:
			observation.State = StateProvisioning
		default:
			observation.State = StateFailed
		}
	case info.State.Running && !info.State.Paused:
		observation.State = StateRunning
	case info.State.Running && info.State.Paused:
		observation.State = StatePaused
	default:
		return workspaceInspection{}, fmt.Errorf("unsupported Docker state %q for workspace %q", info.State.Status, workspace)
	}
	return workspaceInspection{observation: observation, info: info}, nil
}

func (d *Docker) rollbackStarted(name, containerID string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	observation, err := d.statusByReference(cleanupCtx, containerID, name)
	if err != nil {
		return errors.Join(cause, err)
	}
	return errors.Join(cause, d.pauseObserved(cleanupCtx, name, observation))
}

func (d *Docker) ensureVolume(ctx context.Context, spec Spec) error {
	workspace := spec.Name
	name := specDataVolumeName(spec)
	existing, err := d.cli.VolumeInspect(ctx, name)
	if err == nil {
		if existing.Labels[managedLabel] != "true" || existing.Labels[workspaceLabel] != workspace {
			return fmt.Errorf("%w: volume %q", ErrUnmanaged, name)
		}
		return nil
	}
	if !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspect data volume %q: %w", name, err)
	}
	created, err := d.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name: name,
		Labels: map[string]string{
			managedLabel:   "true",
			workspaceLabel: workspace,
		},
	})
	if err != nil {
		return fmt.Errorf("create data volume %q: %w", name, err)
	}
	// VolumeCreate is idempotent by name. Another actor can create the volume
	// after our inspect, so the returned object must be treated as untrusted.
	if created.Labels[managedLabel] != "true" || created.Labels[workspaceLabel] != workspace {
		return fmt.Errorf("%w: volume %q", ErrUnmanaged, name)
	}
	return nil
}

func sortedEnv(env map[string]string) []string {
	result := make([]string, 0, len(env))
	for key, value := range env {
		result = append(result, key+"="+value)
	}
	sort.Strings(result)
	return result
}

type fingerprintValue struct {
	Version     int
	Name        string
	Image       string
	RepoPath    string
	MemoryBytes int64
	Env         []string
	Init        bool
	Port        string
	DataVolume  string
	Protocol    Protocol `json:",omitempty"`
}

func specFingerprint(spec Spec) (string, error) {
	return fingerprint(fingerprintValue{
		Version:     1,
		Name:        spec.Name,
		Image:       spec.Image,
		RepoPath:    spec.RepoPath,
		MemoryBytes: spec.MemoryBytes,
		Env:         sortedEnv(spec.Env),
		Init:        true,
		Port:        workspacePort,
		DataVolume:  specDataVolumeName(spec),
		Protocol:    fingerprintProtocol(spec.Protocol),
	})
}

func fingerprintProtocol(protocol Protocol) Protocol {
	if protocol.Normalize() == ProtocolV1 {
		return ""
	}
	return protocol.Normalize()
}

func (d *Docker) verifyActualSpec(ctx context.Context, containerID string, spec Spec) error {
	info, err := d.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return fmt.Errorf("inspect actual workspace configuration: %w", err)
	}
	return verifyActualSpec(info, spec)
}

func verifyActualSpec(info container.InspectResponse, spec Spec) error {
	if info.Config == nil || info.HostConfig == nil {
		return fmt.Errorf("%w: Docker returned incomplete workspace configuration", ErrSpecDrift)
	}
	if info.Config.Image != spec.Image || info.HostConfig.Memory != spec.MemoryBytes || info.HostConfig.Init == nil || !*info.HostConfig.Init || !info.HostConfig.RestartPolicy.IsNone() {
		return fmt.Errorf("%w: Docker image, memory, init, or restart setting was modified; run 'fern down' to recreate", ErrSpecDrift)
	}
	if info.HostConfig.Privileged || info.HostConfig.ReadonlyRootfs || len(info.HostConfig.CapAdd) != 0 || len(info.HostConfig.Devices) != 0 || len(info.HostConfig.DeviceRequests) != 0 || len(info.HostConfig.SecurityOpt) != 0 {
		return fmt.Errorf("%w: Docker privilege, capability, device, or security settings were modified; run 'fern down' to recreate", ErrSpecDrift)
	}
	bindings := info.HostConfig.PortBindings[nat.Port(workspacePort)]
	if _, ok := info.Config.ExposedPorts[nat.Port(workspacePort)]; !ok || len(bindings) != 1 || !isLoopbackBinding(bindings[0].HostIP) {
		return fmt.Errorf("%w: OpenCode port configuration was modified", ErrSpecDrift)
	}
	actualEnv := make(map[string]string, len(info.Config.Env))
	for _, entry := range info.Config.Env {
		key, value, _ := strings.Cut(entry, "=")
		actualEnv[key] = value
	}
	for key, value := range spec.Env {
		actual, exists := actualEnv[key]
		if !exists || actual != value {
			return fmt.Errorf("%w: environment %s was modified", ErrSpecDrift, key)
		}
	}
	var repoPath, dataVolume string
	var repoCount, dataCount int
	for _, actualMount := range info.Mounts {
		switch actualMount.Destination {
		case "/home/user/workspace":
			repoCount++
			if actualMount.Type != mount.TypeBind || !actualMount.RW {
				return fmt.Errorf("%w: repository mount type or access was modified", ErrSpecDrift)
			}
			repoPath = actualMount.Source
		case "/home/user/.local/share/opencode":
			dataCount++
			if actualMount.Type != mount.TypeVolume || !actualMount.RW {
				return fmt.Errorf("%w: data mount type or access was modified", ErrSpecDrift)
			}
			dataVolume = actualMount.Name
		}
	}
	if len(info.Mounts) != 2 || repoCount != 1 || dataCount != 1 || filepath.Clean(repoPath) != filepath.Clean(spec.RepoPath) || dataVolume != specDataVolumeName(spec) {
		return fmt.Errorf("%w: repository or data mount was modified", ErrSpecDrift)
	}
	return nil
}

func isLoopbackBinding(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func fingerprint(value fingerprintValue) (string, error) {
	sort.Strings(value.Env)
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("fingerprint workspace spec: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func dataVolumeName(workspace string) string {
	return "fern-" + workspace + "-data"
}

func specDataVolumeName(spec Spec) string {
	switch spec.Protocol.Normalize() {
	case ProtocolV2:
		return "fern-" + spec.Name + "-v2-data"
	case ProtocolAuto:
		return "fern-" + spec.Name + "-auto-data"
	default:
		return dataVolumeName(spec.Name)
	}
}
