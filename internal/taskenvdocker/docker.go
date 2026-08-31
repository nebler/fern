package taskenvdocker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
	"github.com/nebler/fern/internal/taskstore"
)

var (
	expectedMaskedPaths   = []string{"/proc/asound", "/proc/acpi", "/proc/kcore", "/proc/keys", "/proc/latency_stats", "/proc/timer_list", "/proc/timer_stats", "/proc/sched_debug", "/proc/scsi", "/sys/firmware", "/sys/devices/virtual/powercap"}
	expectedReadonlyPaths = []string{"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"}
)

// EnsureVolume creates or reconciles the exact labeled local OpenCode volume.
func (p *Provider) EnsureVolume(ctx context.Context, run taskstore.BackgroundRun) (_ Observation, resultErr error) {
	digest, err := p.validateRun(run)
	if err != nil {
		return Observation{}, err
	}
	unlock, err := p.acquireCloneAuthority(ctx, run, digest)
	if err != nil {
		return Observation{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	want := p.labels(run, digest)
	operation, cancel := operationContext(ctx, p.config.DockerTimeout)
	existing, err := p.docker.VolumeInspect(operation, run.VolumeIdentity)
	status := "reconciled"
	if errdefs.IsNotFound(err) {
		status = "created"
		_, createErr := p.docker.VolumeCreate(operation, volume.CreateOptions{Name: run.VolumeIdentity, Driver: "local", Labels: want})
		cancel()
		read, readCancel := p.freshDockerContext()
		existing, err = p.docker.VolumeInspect(read, run.VolumeIdentity)
		readCancel()
		if err != nil {
			return Observation{}, errors.Join(fmt.Errorf("create background run volume: %w", createErr), fmt.Errorf("reconcile created volume: %w", err))
		}
	} else {
		cancel()
	}
	if err != nil {
		return Observation{}, fmt.Errorf("ensure background run volume: %w", err)
	}
	if err := p.attestVolume(run, digest, existing); err != nil {
		return Observation{}, &IdentityError{Resource: "volume", Identity: run.VolumeIdentity, Reason: err.Error()}
	}
	e, _ := makeEvidence(evidence{Effect: "volume", Identity: run.VolumeIdentity, Spec: digest, Status: status})
	return Observation{Evidence: e}, nil
}

func (p *Provider) attestVolume(run taskstore.BackgroundRun, digest string, item volume.Volume) error {
	if item.Name != run.VolumeIdentity || !equalMap(item.Labels, p.labels(run, digest)) {
		return errors.New("Docker name or labels do not match the immutable run")
	}
	if item.Driver != "local" || item.Scope != "local" || len(item.Options) != 0 || item.ClusterVolume != nil || len(item.Status) != 0 {
		return errors.New("Docker volume is not an option-free local-scope local-driver volume")
	}
	if item.Mountpoint == "" || !filepath.IsAbs(item.Mountpoint) {
		return errors.New("Docker local volume mountpoint is not an absolute daemon path")
	}
	return nil
}

// EnsureContainer creates the exact stopped container or reconciles a lost
// create response. It never starts a container.
func (p *Provider) EnsureContainer(ctx context.Context, run taskstore.BackgroundRun) (_ Observation, resultErr error) {
	digest, err := p.validateRun(run)
	if err != nil {
		return Observation{}, err
	}
	unlock, err := p.acquireCloneAuthority(ctx, run, digest)
	if err != nil {
		return Observation{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	volumeOperation, volumeCancel := operationContext(ctx, p.config.DockerTimeout)
	item, err := p.docker.VolumeInspect(volumeOperation, run.VolumeIdentity)
	volumeCancel()
	if err != nil {
		return Observation{}, fmt.Errorf("inspect background run volume before container create: %w", err)
	}
	if err := p.attestVolume(run, digest, item); err != nil {
		return Observation{}, &IdentityError{Resource: "volume", Identity: run.VolumeIdentity, Reason: err.Error()}
	}
	operation, cancel := operationContext(ctx, p.config.DockerTimeout)
	info, err := p.docker.ContainerInspect(operation, run.ContainerIdentity)
	status := "reconciled"
	if errdefs.IsNotFound(err) {
		useInit := true
		pids := p.config.PIDs
		environment := p.expectedEnvironment(run)
		labels := p.containerLabels(run, digest)
		response, createErr := p.docker.ContainerCreate(operation, &container.Config{
			Image: run.ImageIdentity, User: containerUser, Env: environment,
			Entrypoint: []string{}, Cmd: []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096"},
			WorkingDir: workspaceTarget, ExposedPorts: nat.PortSet{serverPort: struct{}{}}, Volumes: map[string]struct{}{workspaceTarget: {}, opencodeTarget: {}}, Labels: labels,
		}, &container.HostConfig{
			NetworkMode: "bridge", IpcMode: "private", CgroupnsMode: "private", Runtime: "runc", ShmSize: 64 << 20,
			PortBindings: nat.PortMap{serverPort: []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "0"}}},
			Resources:    container.Resources{Memory: p.config.MemoryBytes, MemorySwap: p.config.MemoryBytes * 2, NanoCPUs: p.config.NanoCPUs, PidsLimit: &pids},
			Init:         &useInit, RestartPolicy: container.RestartPolicy{Name: "no"}, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"},
			Mounts: []mount.Mount{
				{Type: mount.TypeBind, Source: filepath.Join(p.root, run.CloneIdentity), Target: workspaceTarget},
				{Type: mount.TypeVolume, Source: run.VolumeIdentity, Target: opencodeTarget},
			},
			LogConfig:     container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": p.config.LogMaxSize, "max-file": strconv.Itoa(p.config.LogMaxFiles)}},
			MaskedPaths:   slices.Clone(expectedMaskedPaths),
			ReadonlyPaths: slices.Clone(expectedReadonlyPaths),
		}, &network.NetworkingConfig{}, nil, run.ContainerIdentity)
		status = "created"
		cancel()
		read, readCancel := p.freshDockerContext()
		info, err = p.docker.ContainerInspect(read, run.ContainerIdentity)
		readCancel()
		if err != nil {
			return Observation{}, errors.Join(fmt.Errorf("create background run container: %w", createErr), fmt.Errorf("reconcile created container: %w", err))
		}
		if createErr == nil && response.ID != info.ID {
			return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "create response ID differs from named container"}
		}
	} else {
		cancel()
		if err != nil {
			return Observation{}, fmt.Errorf("inspect background run container: %w", err)
		}
	}
	if err := p.attestContainer(run, digest, info, false); err != nil {
		return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: err.Error()}
	}
	e, _ := makeEvidence(evidence{Effect: "container_create", Identity: run.ContainerIdentity, Spec: digest, Status: status, Container: info.ID})
	return Observation{Evidence: e, ContainerID: info.ID}, nil
}

// StartContainer starts only an exactly attested created container and returns
// its exact Docker process epoch.
func (p *Provider) StartContainer(ctx context.Context, run taskstore.BackgroundRun, expectedID string) (_ Observation, resultErr error) {
	digest, err := p.validateRun(run)
	if err != nil {
		return Observation{}, err
	}
	unlock, err := p.acquireCloneAuthority(ctx, run, digest)
	if err != nil {
		return Observation{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	operation, cancel := operationContext(ctx, p.config.DockerTimeout)
	info, err := p.docker.ContainerInspect(operation, run.ContainerIdentity)
	if err != nil {
		cancel()
		return Observation{}, err
	}
	if expectedID == "" || info.ID != expectedID {
		cancel()
		return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "container ID does not match committed observation"}
	}
	if err := p.attestContainer(run, digest, info, false); err != nil {
		cancel()
		return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: err.Error()}
	}
	status := "running"
	if !info.State.Running {
		if info.State.Status != "created" {
			cancel()
			return Observation{}, fmt.Errorf("container is %s and may not be restarted", info.State.Status)
		}
		status = "started"
		startErr := p.docker.ContainerStart(operation, expectedID, container.StartOptions{})
		cancel()
		read, readCancel := p.freshDockerContext()
		info, err = p.docker.ContainerInspect(read, run.ContainerIdentity)
		readCancel()
		if err != nil || info.ID != expectedID || info.State == nil || !info.State.Running {
			return Observation{}, errors.Join(fmt.Errorf("start background run container: %w", startErr), err)
		}
	} else {
		cancel()
	}
	if err := p.attestContainer(run, digest, info, true); err != nil {
		return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: err.Error()}
	}
	runtime, epoch, err := runtimeIdentity(info)
	if err != nil {
		return Observation{}, err
	}
	port, err := hostPort(info)
	if err != nil {
		return Observation{}, err
	}
	e, _ := makeEvidence(evidence{Effect: "container_start", Identity: run.ContainerIdentity, Spec: digest, Status: status, Container: info.ID, Started: runtime.StartedAt, Runtime: runtime.Token, Port: port})
	return Observation{Evidence: e, ContainerID: info.ID, ContainerStarted: runtime.StartedAt, RuntimeEpoch: epoch, RuntimeToken: runtime.Token, HostPort: port, Endpoint: "http://127.0.0.1:" + strconv.Itoa(port)}, nil
}

func (p *Provider) attestContainer(run taskstore.BackgroundRun, digest string, info container.InspectResponse, requireRunning bool) error {
	return p.attestContainerMode(run, digest, info, requireRunning, true)
}

func (p *Provider) attestContainerForCleanup(run taskstore.BackgroundRun, digest string, info container.InspectResponse, requireRunning bool) error {
	return p.attestContainerMode(run, digest, info, requireRunning, false)
}

func (p *Provider) attestContainerMode(run taskstore.BackgroundRun, digest string, info container.InspectResponse, requireRunning, enforceCurrentPolicy bool) error {
	if info.ContainerJSONBase == nil || info.Config == nil || info.HostConfig == nil || info.State == nil || info.NetworkSettings == nil {
		return errors.New("Docker returned incomplete container inspection")
	}
	c := info.Config
	labelsMatch := equalMap(c.Labels, p.containerLabels(run, digest))
	if !enforceCurrentPolicy {
		labelsMatch = containsMap(c.Labels, p.labels(run, digest))
	}
	if info.ID == "" || info.Name != "/"+run.ContainerIdentity || info.Image != run.ImageIdentity || c.Image != run.ImageIdentity || c.User != containerUser || c.WorkingDir != workspaceTarget || !slices.Equal(c.Cmd, []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096"}) || len(c.Entrypoint) != 0 || !labelsMatch {
		return errors.New("container name, image, user, command, workdir, or labels differ")
	}
	if len(info.ID) < 12 || c.Hostname != info.ID[:12] || c.Domainname != "" {
		return errors.New("container hostname or domain differs")
	}
	if c.AttachStdin || c.AttachStdout || c.AttachStderr || c.Tty || c.OpenStdin || c.StdinOnce || c.NetworkDisabled {
		return errors.New("container interactive or network-disabled flags differ")
	}
	if c.Healthcheck != nil || c.ArgsEscaped || !equalMap(c.Volumes, map[string]struct{}{workspaceTarget: {}, opencodeTarget: {}}) || len(c.OnBuild) != 0 || c.StopSignal != "" || c.StopTimeout != nil || len(c.Shell) != 0 || c.MacAddress != "" || len(c.ExposedPorts) != 1 {
		return fmt.Errorf("container inherited portable options differ (health=%t volumes=%d onbuild=%d stop_signal=%q stop_timeout=%t shell=%d mac=%t ports=%d)", c.Healthcheck != nil, len(c.Volumes), len(c.OnBuild), c.StopSignal, c.StopTimeout != nil, len(c.Shell), c.MacAddress != "", len(c.ExposedPorts))
	}
	if _, ok := c.ExposedPorts[serverPort]; !ok {
		return errors.New("container exposed port differs")
	}
	if enforceCurrentPolicy {
		wantEnv, err := parseEnvironment(p.expectedEnvironment(run))
		if err != nil {
			return err
		}
		gotEnv, err := parseEnvironment(c.Env)
		if err != nil || !equalMap(gotEnv, wantEnv) {
			return errors.New("container environment differs")
		}
	}
	h := info.HostConfig
	if h.NetworkMode != "bridge" || h.IpcMode != "private" || h.CgroupnsMode != "private" || h.PidMode != "" || h.UTSMode != "" || h.UsernsMode != "" || h.Runtime != "runc" || h.ShmSize != 64<<20 || h.AutoRemove || h.Privileged || h.ReadonlyRootfs || h.PublishAllPorts || h.ContainerIDFile != "" || h.VolumeDriver != "" || h.Cgroup != "" || h.Isolation != "" || h.ConsoleSize != [2]uint{} {
		return errors.New("container namespace, network, auto-remove, or security flags differ")
	}
	if len(h.Binds) != 0 || len(h.VolumesFrom) != 0 || len(h.CapAdd) != 0 || !slices.Equal(h.CapDrop, []string{"ALL"}) || !slices.Equal(h.SecurityOpt, []string{"no-new-privileges"}) || len(h.DNS) != 0 || len(h.DNSOptions) != 0 || len(h.DNSSearch) != 0 || len(h.ExtraHosts) != 0 || len(h.GroupAdd) != 0 || len(h.Links) != 0 || len(h.Devices) != 0 || len(h.DeviceRequests) != 0 || len(h.DeviceCgroupRules) != 0 || len(h.Ulimits) != 0 || len(h.Sysctls) != 0 || len(h.StorageOpt) != 0 || len(h.Tmpfs) != 0 || len(h.Annotations) != 0 {
		return errors.New("container DNS, links, devices, capabilities, or mutable host options differ")
	}
	if enforceCurrentPolicy {
		gotPIDs := int64(-1)
		if h.PidsLimit != nil {
			gotPIDs = *h.PidsLimit
		}
		if h.Memory != p.config.MemoryBytes || h.MemorySwap != p.config.MemoryBytes*2 || h.MemoryReservation != 0 || h.NanoCPUs != p.config.NanoCPUs || gotPIDs != p.config.PIDs || h.CPUShares != 0 || h.CPUPeriod != 0 || h.CPUQuota != 0 || h.CpusetCpus != "" || h.CpusetMems != "" || (h.OomKillDisable != nil && *h.OomKillDisable) || h.MemorySwappiness != nil || h.OomScoreAdj != 0 || h.Init == nil || !*h.Init || !h.RestartPolicy.IsNone() {
			return fmt.Errorf("container resource, init, or restart limits differ (memory=%d swap=%d reservation=%d nano_cpus=%d pids=%d shares=%d period=%d quota=%d oom_disable=%t swappiness=%t oom_score=%d init=%t restart=%s)", h.Memory, h.MemorySwap, h.MemoryReservation, h.NanoCPUs, gotPIDs, h.CPUShares, h.CPUPeriod, h.CPUQuota, h.OomKillDisable != nil, h.MemorySwappiness != nil, h.OomScoreAdj, h.Init != nil && *h.Init, h.RestartPolicy.Name)
		}
	}
	if h.CgroupParent != "" || h.BlkioWeight != 0 || len(h.BlkioWeightDevice) != 0 || len(h.BlkioDeviceReadBps) != 0 || len(h.BlkioDeviceWriteBps) != 0 || len(h.BlkioDeviceReadIOps) != 0 || len(h.BlkioDeviceWriteIOps) != 0 || h.CPURealtimePeriod != 0 || h.CPURealtimeRuntime != 0 || h.KernelMemory != 0 || h.KernelMemoryTCP != 0 || h.CPUCount != 0 || h.CPUPercent != 0 || h.IOMaximumIOps != 0 || h.IOMaximumBandwidth != 0 {
		return errors.New("container secondary CPU, memory, block-I/O, or cgroup dimensions differ")
	}
	if (enforceCurrentPolicy && (h.LogConfig.Type != "json-file" || !equalMap(h.LogConfig.Config, map[string]string{"max-size": p.config.LogMaxSize, "max-file": strconv.Itoa(p.config.LogMaxFiles)}))) || !slices.Equal(h.MaskedPaths, expectedMaskedPaths) || !slices.Equal(h.ReadonlyPaths, expectedReadonlyPaths) {
		return errors.New("container log or protected-path limits differ")
	}
	bindings := h.PortBindings[serverPort]
	if len(h.PortBindings) != 1 || len(bindings) != 1 || bindings[0] != (nat.PortBinding{HostIP: "127.0.0.1", HostPort: "0"}) {
		return errors.New("container is not configured for one random loopback port")
	}
	if err := p.attestMounts(run, h.Mounts, info.Mounts); err != nil {
		return err
	}
	if len(info.NetworkSettings.Networks) != 1 {
		return errors.New("container has an unexpected network attachment count")
	}
	bridge, ok := info.NetworkSettings.Networks["bridge"]
	if !ok || bridge == nil || bridge.IPAMConfig != nil || len(bridge.Links) != 0 || len(bridge.Aliases) != 0 || len(bridge.DriverOpts) != 0 || bridge.GwPriority != 0 {
		return errors.New("container bridge attachment options differ")
	}
	if requireRunning {
		if !info.State.Running || info.State.Paused || info.State.Restarting || info.State.Dead || info.State.StartedAt == "" {
			return errors.New("container is not exactly running")
		}
		if _, err := hostPort(info); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) attestMounts(run taskstore.BackgroundRun, configured []mount.Mount, observed []container.MountPoint) error {
	clonePath := filepath.Join(p.root, run.CloneIdentity)
	want := []mount.Mount{{Type: mount.TypeBind, Source: clonePath, Target: workspaceTarget}, {Type: mount.TypeVolume, Source: run.VolumeIdentity, Target: opencodeTarget}}
	if !slices.Equal(configured, want) || len(observed) != 2 {
		return errors.New("container configured mounts differ")
	}
	seenClone, seenVolume := false, false
	for _, item := range observed {
		switch item.Destination {
		case workspaceTarget:
			seenClone = item.Type == mount.TypeBind && item.Source == clonePath && item.Name == "" && item.Driver == "" && item.Mode == "" && item.RW && item.Propagation == mount.PropagationRPrivate
		case opencodeTarget:
			seenVolume = item.Type == mount.TypeVolume && item.Name == run.VolumeIdentity && item.Source != "" && filepath.IsAbs(item.Source) && item.Driver == "local" && item.RW && item.Propagation == ""
		default:
			return errors.New("container has an unknown mount")
		}
	}
	if !seenClone || !seenVolume {
		return errors.New("container mount source, mode, propagation, or options differ")
	}
	return nil
}

func (p *Provider) expectedEnvironment(run taskstore.BackgroundRun) []string {
	environment := cloneMap(p.imageEnv)
	for key, value := range p.config.Environment {
		environment[key] = value
	}
	environment[usernameEnv] = p.config.BasicUsername
	environment[passwordEnv] = p.password(run)
	result := make([]string, 0, len(environment))
	for key, value := range environment {
		result = append(result, key+"="+value)
	}
	slices.Sort(result)
	return result
}

func hostPort(info container.InspectResponse) (int, error) {
	if info.NetworkSettings == nil {
		return 0, errors.New("container has no network settings")
	}
	bindings := info.NetworkSettings.Ports[serverPort]
	if len(info.NetworkSettings.Ports) != 1 || len(bindings) != 1 || bindings[0].HostIP != "127.0.0.1" {
		return 0, errors.New("container runtime port is not exact loopback")
	}
	port, err := strconv.Atoi(bindings[0].HostPort)
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("container runtime port is invalid")
	}
	return port, nil
}

func runtimeIdentity(info container.InspectResponse) (RuntimeIdentity, int64, error) {
	if info.State == nil || info.ID == "" {
		return RuntimeIdentity{}, 0, errors.New("Docker returned incomplete runtime identity")
	}
	started, err := time.Parse(time.RFC3339Nano, info.State.StartedAt)
	if err != nil || started.IsZero() || started.Format(time.RFC3339Nano) != info.State.StartedAt {
		return RuntimeIdentity{}, 0, errors.New("Docker returned a noncanonical container start timestamp")
	}
	identity := RuntimeIdentity{ContainerID: info.ID, StartedAt: info.State.StartedAt, Token: runtimeToken(info.ID, info.State.StartedAt)}
	return identity, started.UnixNano(), nil
}

func runtimeToken(containerID, startedAt string) string {
	sum := sha256.Sum256([]byte(containerID + "\x00" + startedAt))
	return hex.EncodeToString(sum[:])
}

func validateCommittedRuntime(runtime RuntimeIdentity) error {
	started, err := time.Parse(time.RFC3339Nano, runtime.StartedAt)
	if runtime.ContainerID == "" || err != nil || started.IsZero() || started.Format(time.RFC3339Nano) != runtime.StartedAt || runtime.Token != runtimeToken(runtime.ContainerID, runtime.StartedAt) {
		return errors.New("exact canonical committed runtime identity is required")
	}
	return nil
}

type cleanupAuthorityKind uint8

const (
	cleanupNeverCreated cleanupAuthorityKind = iota + 1
	cleanupCreated
	cleanupRuntime
)

func validateCleanupAuthority(authority CleanupAuthority) (cleanupAuthorityKind, error) {
	if authority.NeverCreated {
		if authority.ContainerID == "" && authority.StartedAt == "" && authority.Token == "" {
			return cleanupNeverCreated, nil
		}
		return 0, errors.New("NeverCreated cleanup authority must have empty identity fields")
	}
	if authority.ContainerID != "" && authority.StartedAt == "" && authority.Token == "" {
		return cleanupCreated, nil
	}
	if err := validateCommittedRuntime(authority.runtimeIdentity()); err == nil {
		return cleanupRuntime, nil
	}
	return 0, errors.New("cleanup authority must be NeverCreated, an exact created container ID, or a full committed runtime")
}

func requireRuntime(info container.InspectResponse, expected RuntimeIdentity) error {
	got, _, err := runtimeIdentity(info)
	if err != nil {
		return err
	}
	if expected.ContainerID == "" || expected.StartedAt == "" || expected.Token == "" || got != expected {
		return &IdentityError{Resource: "runtime", Identity: expected.ContainerID, Reason: "container ID or exact start epoch differs from committed observation"}
	}
	return nil
}

func (p *Provider) freshDockerContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), p.config.DockerTimeout)
}

// Health proves exact Basic-auth failures and success for the committed runtime.
func (p *Provider) Health(ctx context.Context, run taskstore.BackgroundRun, runtime RuntimeIdentity) (Observation, error) {
	if err := validateCommittedRuntime(runtime); err != nil {
		return Observation{}, err
	}
	digest, err := p.validateRun(run)
	if err != nil {
		return Observation{}, err
	}
	deadline, cancel := operationContext(ctx, p.config.HealthTimeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		observation, err := p.healthOnce(deadline, run, digest, runtime)
		if err == nil {
			return observation, nil
		}
		if errors.Is(err, ErrIdentityMismatch) {
			return Observation{}, err
		}
		last = err
		select {
		case <-deadline.Done():
			return Observation{}, fmt.Errorf("authenticated health timed out: %v: %w", last, deadline.Err())
		case <-ticker.C:
		}
	}
}

func (p *Provider) healthOnce(ctx context.Context, run taskstore.BackgroundRun, digest string, runtime RuntimeIdentity) (Observation, error) {
	info, err := p.docker.ContainerInspect(ctx, run.ContainerIdentity)
	if err != nil {
		return Observation{}, err
	}
	if info.ID != runtime.ContainerID {
		return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "named container ID differs from committed runtime"}
	}
	if err := p.attestContainer(run, digest, info, true); err != nil {
		return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: err.Error()}
	}
	if err := requireRuntime(info, runtime); err != nil {
		return Observation{}, err
	}
	port, err := hostPort(info)
	if err != nil {
		return Observation{}, err
	}
	endpoint := "http://127.0.0.1:" + strconv.Itoa(port)
	password := p.password(run)
	for _, probe := range []struct {
		name, user, password string
		want                 int
		body                 []byte
	}{{"missing", "", "", http.StatusUnauthorized, []byte(`{"_tag":"UnauthorizedError","message":"Authentication required"}`)}, {"wrong", p.config.BasicUsername, password + "-wrong", http.StatusUnauthorized, []byte(`{"_tag":"UnauthorizedError","message":"Authentication required"}`)}, {"correct", p.config.BasicUsername, password, http.StatusOK, []byte(`{"healthy":true}`)}} {
		response, err := p.requestHealth(ctx, endpoint, probe.user, probe.password)
		if err != nil {
			return Observation{}, fmt.Errorf("%s credential health probe: %w", probe.name, err)
		}
		if err := validateHealthProbe(response, probe.want, probe.body); err != nil {
			return Observation{}, fmt.Errorf("%s credential health response: %w", probe.name, err)
		}
	}
	e, _ := makeEvidence(evidence{Effect: "health", Identity: run.EndpointIdentity, Spec: digest, Status: "authenticated", Container: info.ID, Started: runtime.StartedAt, Runtime: runtime.Token, Port: port})
	_, epoch, _ := runtimeIdentity(info)
	return Observation{Evidence: e, ContainerID: info.ID, ContainerStarted: runtime.StartedAt, RuntimeEpoch: epoch, RuntimeToken: runtime.Token, HostPort: port, Endpoint: endpoint}, nil
}

type healthResponse struct {
	status     int
	body       []byte
	challenges []string
}

func validateHealthProbe(response healthResponse, status int, body []byte) error {
	if response.status != status || !bytes.Equal(response.body, body) {
		return errors.New("status or canonical JSON body is not exact")
	}
	if status == http.StatusUnauthorized {
		if len(response.challenges) != 1 || response.challenges[0] != `Basic realm="Secure Area"` {
			return errors.New("Basic challenge cardinality or value is not exact")
		}
	} else if len(response.challenges) != 0 {
		return errors.New("successful health response unexpectedly contains an authentication challenge")
	}
	return nil
}

func (p *Provider) requestHealth(ctx context.Context, endpoint, user, password string) (healthResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/health", nil)
	if err != nil {
		return healthResponse{}, err
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	response, err := p.http.Do(req)
	if err != nil {
		return healthResponse{}, err
	}
	defer response.Body.Close()
	contentTypes := response.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return healthResponse{}, errors.New("health response must contain exactly one Content-Type header")
	}
	mediaType, parameters, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/json" || len(parameters) != 0 {
		return healthResponse{}, errors.New("health response Content-Type is not exact application/json")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxHealthBytes+1))
	if err != nil {
		return healthResponse{}, err
	}
	if len(body) > maxHealthBytes {
		return healthResponse{}, errors.New("health response exceeds bound")
	}
	return healthResponse{status: response.StatusCode, body: body, challenges: slices.Clone(response.Header.Values("WWW-Authenticate"))}, nil
}

// StopContainer attests the exact process epoch before stopping and returns
// positive non-running writer-inactivity evidence.
func (p *Provider) StopContainer(ctx context.Context, run taskstore.BackgroundRun, runtime RuntimeIdentity) (Observation, error) {
	digest, err := p.cleanupDigest(run)
	if err != nil {
		return Observation{}, err
	}
	if err := validateCommittedRuntime(runtime); err != nil {
		return Observation{}, err
	}
	operation, cancel := operationContext(ctx, p.config.DockerTimeout+p.config.StopGrace)
	info, err := p.docker.ContainerInspect(operation, run.ContainerIdentity)
	if errdefs.IsNotFound(err) {
		if _, idErr := p.docker.ContainerInspect(operation, runtime.ContainerID); idErr == nil {
			cancel()
			return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "committed container ID exists under another name"}
		} else if !errdefs.IsNotFound(idErr) {
			cancel()
			return Observation{}, idErr
		}
		cancel()
		e, _ := makeEvidence(evidence{Effect: "writer_inactive", Identity: run.ContainerIdentity, Spec: digest, Status: "absent", Container: runtime.ContainerID, Started: runtime.StartedAt, Runtime: runtime.Token})
		return Observation{Evidence: e, ContainerID: runtime.ContainerID, ContainerStarted: runtime.StartedAt, RuntimeToken: runtime.Token}, nil
	}
	if err != nil {
		cancel()
		return Observation{}, err
	}
	if info.ID != runtime.ContainerID {
		cancel()
		return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "container ID does not match stop authority"}
	}
	if err := p.attestContainerForCleanup(run, digest, info, info.State.Running); err != nil {
		cancel()
		return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: err.Error()}
	}
	if err := requireRuntime(info, runtime); err != nil {
		cancel()
		return Observation{}, err
	}
	status := "already_stopped"
	if info.State.Running {
		seconds := int(p.config.StopGrace / time.Second)
		status = "stopped"
		stopErr := p.docker.ContainerStop(operation, runtime.ContainerID, container.StopOptions{Timeout: &seconds})
		cancel()
		read, readCancel := p.freshDockerContext()
		info, err = p.docker.ContainerInspect(read, run.ContainerIdentity)
		readCancel()
		if err != nil || info.ID != runtime.ContainerID || info.State == nil || info.State.Running {
			return Observation{}, errors.Join(fmt.Errorf("stop exact container: %w", stopErr), err)
		}
	} else {
		cancel()
	}
	if err := p.attestContainerForCleanup(run, digest, info, false); err != nil {
		return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "post-stop attestation failed: " + err.Error()}
	}
	if err := requireRuntime(info, runtime); err != nil {
		return Observation{}, err
	}
	e, _ := makeEvidence(evidence{Effect: "writer_inactive", Identity: run.ContainerIdentity, Spec: digest, Status: status, Container: runtime.ContainerID, Started: runtime.StartedAt, Runtime: runtime.Token})
	return Observation{Evidence: e, ContainerID: runtime.ContainerID, ContainerStarted: runtime.StartedAt, RuntimeToken: runtime.Token}, nil
}

// ProveWriterInactive resolves one explicit writer fence from exact
// provider-owned labels. It never creates a resource and never deletes an
// identity that was not fully attested.
func (p *Provider) ProveWriterInactive(ctx context.Context, run taskstore.BackgroundRun) (Observation, WriterFence, error) {
	digest, err := p.cleanupDigest(run)
	if err != nil {
		return Observation{}, WriterFence{}, err
	}
	operation, cancel := operationContext(ctx, p.config.DockerTimeout)
	info, err := p.docker.ContainerInspect(operation, run.ContainerIdentity)
	if errdefs.IsNotFound(err) {
		listed, listErr := p.listRunContainers(operation, run, digest)
		cancel()
		if listErr != nil {
			return Observation{}, WriterFence{}, listErr
		}
		if len(listed) != 0 {
			return Observation{}, WriterFence{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "exact-labeled run container exists under a noncanonical name"}
		}
		e, _ := makeEvidence(evidence{Effect: "writer_inactive", Identity: run.ContainerIdentity, Spec: digest, Status: "never_created"})
		return Observation{Evidence: e}, NeverCreatedAuthority(), nil
	}
	cancel()
	if err != nil {
		return Observation{}, WriterFence{}, err
	}
	if err := p.attestContainerForCleanup(run, digest, info, info.State.Running); err != nil {
		return Observation{}, WriterFence{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: err.Error()}
	}
	if info.State.Status == "created" {
		e, _ := makeEvidence(evidence{Effect: "writer_inactive", Identity: run.ContainerIdentity, Spec: digest, Status: "never_started", Container: info.ID})
		return Observation{Evidence: e, ContainerID: info.ID}, CreatedContainerAuthority(info.ID), nil
	}
	runtime, _, err := runtimeIdentity(info)
	if err != nil {
		return Observation{}, WriterFence{}, err
	}
	if info.State.Running {
		observation, stopErr := p.StopContainer(ctx, run, runtime)
		return observation, RuntimeCleanupAuthority(runtime), stopErr
	}
	e, _ := makeEvidence(evidence{Effect: "writer_inactive", Identity: run.ContainerIdentity, Spec: digest, Status: "already_stopped", Container: info.ID, Started: runtime.StartedAt, Runtime: runtime.Token})
	return Observation{Evidence: e, ContainerID: info.ID, ContainerStarted: runtime.StartedAt, RuntimeToken: runtime.Token}, RuntimeCleanupAuthority(runtime), nil
}

// RemoveContainer removes only the exact attested stopped runtime.
func (p *Provider) RemoveContainer(ctx context.Context, run taskstore.BackgroundRun, authority CleanupAuthority) (Observation, error) {
	digest, err := p.cleanupDigest(run)
	if err != nil {
		return Observation{}, err
	}
	kind, err := validateCleanupAuthority(authority)
	if err != nil {
		return Observation{}, err
	}
	operation, cancel := operationContext(ctx, p.config.DockerTimeout)
	info, err := p.docker.ContainerInspect(operation, run.ContainerIdentity)
	if errdefs.IsNotFound(err) {
		if authority.ContainerID != "" {
			if _, idErr := p.docker.ContainerInspect(operation, authority.ContainerID); idErr == nil {
				cancel()
				return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "expected container ID exists under another name"}
			} else if !errdefs.IsNotFound(idErr) {
				cancel()
				return Observation{}, idErr
			}
		}
		listed, listErr := p.listRunContainers(operation, run, digest)
		cancel()
		if listErr != nil {
			return Observation{}, listErr
		}
		if len(listed) != 0 {
			return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "exact-labeled run container exists under a noncanonical name"}
		}
		e, _ := makeEvidence(evidence{Effect: "container_remove", Identity: run.ContainerIdentity, Spec: digest, Status: "absent"})
		return Observation{Evidence: e}, nil
	}
	if err != nil {
		cancel()
		return Observation{}, err
	}
	if kind == cleanupNeverCreated || authority.ContainerID == "" || info.ID != authority.ContainerID {
		cancel()
		return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "container ID does not match removal authority"}
	}
	if err := p.attestContainerForCleanup(run, digest, info, false); err != nil {
		cancel()
		return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: err.Error()}
	}
	if info.State.Status == "created" {
		if kind != cleanupCreated {
			cancel()
			return Observation{}, errors.New("created container cleanup requires its exact ID without a process epoch")
		}
	} else {
		if kind != cleanupRuntime {
			cancel()
			return Observation{}, errors.New("a container that started requires full committed runtime cleanup authority")
		}
		if err := requireRuntime(info, authority.runtimeIdentity()); err != nil {
			cancel()
			return Observation{}, err
		}
	}
	if info.State.Running {
		cancel()
		return Observation{}, errors.New("refusing to remove a running background container")
	}
	removeErr := p.docker.ContainerRemove(operation, authority.ContainerID, container.RemoveOptions{})
	cancel()
	read, readCancel := p.freshDockerContext()
	defer readCancel()
	_, nameErr := p.docker.ContainerInspect(read, run.ContainerIdentity)
	_, idErr := p.docker.ContainerInspect(read, authority.ContainerID)
	if !errdefs.IsNotFound(nameErr) || !errdefs.IsNotFound(idErr) {
		return Observation{}, errors.Join(fmt.Errorf("remove exact container: %w", removeErr), fmt.Errorf("canonical-name post-remove inspect: %w", nameErr), fmt.Errorf("container-ID post-remove inspect: %w", idErr))
	}
	e, _ := makeEvidence(evidence{Effect: "container_remove", Identity: run.ContainerIdentity, Spec: digest, Status: "removed", Container: authority.ContainerID})
	return Observation{Evidence: e}, nil
}

// RemoveVolume removes only the exact attested volume after the exact runtime is absent.
func (p *Provider) RemoveVolume(ctx context.Context, run taskstore.BackgroundRun, authority CleanupAuthority) (_ Observation, resultErr error) {
	digest, err := p.cleanupDigest(run)
	if err != nil {
		return Observation{}, err
	}
	if _, err := validateCleanupAuthority(authority); err != nil {
		return Observation{}, err
	}
	unlock, clonePresent, err := p.acquireCloneAuthorityIfPresent(ctx, run, digest)
	if err != nil {
		return Observation{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	if err := p.requireContainerAbsent(ctx, run, digest, authority); err != nil {
		return Observation{}, err
	}
	operation, cancel := operationContext(ctx, p.config.DockerTimeout)
	item, err := p.docker.VolumeInspect(operation, run.VolumeIdentity)
	if errdefs.IsNotFound(err) {
		cancel()
		e, _ := makeEvidence(evidence{Effect: "volume_remove", Identity: run.VolumeIdentity, Spec: digest, Status: "absent"})
		return Observation{Evidence: e}, nil
	}
	if err != nil {
		cancel()
		return Observation{}, err
	}
	if !clonePresent {
		cancel()
		return Observation{}, &IdentityError{Resource: "volume", Identity: run.VolumeIdentity, Reason: "volume exists without private clone authority"}
	}
	if err := p.attestVolume(run, digest, item); err != nil {
		cancel()
		return Observation{}, &IdentityError{Resource: "volume", Identity: run.VolumeIdentity, Reason: err.Error()}
	}
	removeErr := p.docker.VolumeRemove(operation, run.VolumeIdentity, false)
	cancel()
	read, readCancel := p.freshDockerContext()
	defer readCancel()
	_, inspectErr := p.docker.VolumeInspect(read, run.VolumeIdentity)
	if !errdefs.IsNotFound(inspectErr) {
		return Observation{}, errors.Join(fmt.Errorf("remove exact volume: %w", removeErr), fmt.Errorf("post-remove volume inspect: %w", inspectErr))
	}
	e, _ := makeEvidence(evidence{Effect: "volume_remove", Identity: run.VolumeIdentity, Spec: digest, Status: "removed"})
	return Observation{Evidence: e}, nil
}

func (p *Provider) requireContainerAbsent(ctx context.Context, run taskstore.BackgroundRun, digest string, authority CleanupAuthority) error {
	operation, cancel := operationContext(ctx, p.config.DockerTimeout)
	defer cancel()
	if info, err := p.docker.ContainerInspect(operation, run.ContainerIdentity); err == nil {
		return &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "container still exists before disposable storage cleanup: " + info.ID}
	} else if !errdefs.IsNotFound(err) {
		return err
	}
	if authority.ContainerID != "" {
		if _, err := p.docker.ContainerInspect(operation, authority.ContainerID); err == nil {
			return &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "expected container ID still exists under another name"}
		} else if !errdefs.IsNotFound(err) {
			return err
		}
	}
	listed, err := p.listRunContainers(operation, run, digest)
	if err != nil {
		return err
	}
	if len(listed) != 0 {
		return &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "exact-labeled run container still exists under another name"}
	}
	return nil
}

func (p *Provider) requireVolumeAbsent(ctx context.Context, run taskstore.BackgroundRun) error {
	operation, cancel := operationContext(ctx, p.config.DockerTimeout)
	defer cancel()
	if item, err := p.docker.VolumeInspect(operation, run.VolumeIdentity); err == nil {
		return &IdentityError{Resource: "volume", Identity: run.VolumeIdentity, Reason: "volume still exists before clone cleanup: " + item.Name}
	} else if !errdefs.IsNotFound(err) {
		return err
	}
	return nil
}

func (p *Provider) requireNoRunContainer(ctx context.Context, run taskstore.BackgroundRun, digest string) error {
	operation, cancel := operationContext(ctx, p.config.DockerTimeout)
	defer cancel()
	if info, err := p.docker.ContainerInspect(operation, run.ContainerIdentity); err == nil {
		return &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "container exists while host Git inspection is requested: " + info.ID}
	} else if !errdefs.IsNotFound(err) {
		return err
	}
	items, err := p.listRunContainers(operation, run, digest)
	if err != nil {
		return err
	}
	if len(items) != 0 {
		return &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "exact-labeled run container exists while host Git inspection is requested"}
	}
	return nil
}

func (p *Provider) listRunContainers(ctx context.Context, run taskstore.BackgroundRun, digest string) ([]container.Summary, error) {
	labels := p.labels(run, digest)
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	args := filters.NewArgs()
	for _, key := range keys {
		args.Add("label", key+"="+labels[key])
	}
	items, err := p.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil, fmt.Errorf("list exact-labeled run containers: %w", err)
	}
	return items, nil
}

func equalMap[K comparable, V comparable](left, right map[K]V) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func containsMap[K comparable, V comparable](got, required map[K]V) bool {
	for key, value := range required {
		gotValue, exists := got[key]
		if !exists || gotValue != value {
			return false
		}
	}
	return true
}
