package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
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
	"github.com/nebler/fern/internal/domain"
	"github.com/nebler/fern/internal/store"
)

var (
	expectedMaskedPaths   = []string{"/proc/asound", "/proc/acpi", "/proc/kcore", "/proc/keys", "/proc/latency_stats", "/proc/timer_list", "/proc/timer_stats", "/proc/sched_debug", "/proc/scsi", "/sys/firmware", "/sys/devices/virtual/powercap"}
	expectedReadonlyPaths = []string{"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"}
)

// Provision reconciles the clone, volume, and container from their
// deterministic identities and starts the exact container, returning its
// runtime. Each step inspects before it creates, so a repeat after a crash or
// lost response converges. Host Git inspection of the clone happens only while
// no run container exists: once one does, the clone may be agent-written.
func (p *Provider) Provision(ctx context.Context, run store.BackgroundRun) (Observation, error) {
	if _, err := p.validateRun(run); err != nil {
		return Observation{}, err
	}
	if err := ctx.Err(); err != nil {
		return Observation{}, err
	}
	operation, cancel := context.WithTimeout(ctx, p.config.DockerTimeout)
	_, err := p.docker.ContainerInspect(operation, run.ContainerIdentity)
	cancel()
	if errdefs.IsNotFound(err) {
		if _, err := p.EnsureClone(ctx, run); err != nil {
			return Observation{}, err
		}
		if _, err := p.EnsureVolume(ctx, run); err != nil {
			return Observation{}, err
		}
	} else if err != nil {
		return Observation{}, fmt.Errorf("inspect background run container: %w", err)
	}
	created, err := p.EnsureContainer(ctx, run)
	if err != nil {
		return Observation{}, err
	}
	return p.StartContainer(ctx, run, created.ContainerID)
}

// EnsureVolume creates or reconciles the exact labeled local OpenCode volume.
func (p *Provider) EnsureVolume(ctx context.Context, run store.BackgroundRun) (_ Observation, resultErr error) {
	digest, err := p.validateRun(run)
	if err != nil {
		return Observation{}, err
	}
	unlock, err := p.acquireCloneAuthority(ctx, run, digest)
	if err != nil {
		return Observation{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	operation, cancel := context.WithTimeout(ctx, p.config.DockerTimeout)
	defer cancel()
	existing, err := p.docker.VolumeInspect(operation, run.VolumeIdentity)
	status := "reconciled"
	if errdefs.IsNotFound(err) {
		status = "created"
		if err := p.prepareVolumeBacking(run); err != nil {
			return Observation{}, err
		}
		_, createErr := p.docker.VolumeCreate(operation, volume.CreateOptions{Name: run.VolumeIdentity, Driver: "local", Labels: p.labels(run, digest), DriverOpts: p.volumeOptions(run)})
		read, readCancel := p.freshDockerContext(ctx)
		defer readCancel()
		existing, err = p.docker.VolumeInspect(read, run.VolumeIdentity)
		if err != nil {
			return Observation{}, errors.Join(fmt.Errorf("create background run volume: %w", createErr), fmt.Errorf("reconcile created volume: %w", err))
		}
	}
	if err != nil {
		return Observation{}, fmt.Errorf("ensure background run volume: %w", err)
	}
	if err := p.attestRunVolume(run, digest, existing); err != nil {
		return Observation{}, err
	}
	e, _ := makeEvidence(evidence{Effect: "volume", Identity: run.VolumeIdentity, Spec: digest, Status: status})
	return Observation{Evidence: e}, nil
}

// inspectRunVolume inspects the run's volume and proves it is the exact
// quota-backed volume execution may mount.
func (p *Provider) inspectRunVolume(ctx context.Context, run store.BackgroundRun, digest string) (volume.Volume, error) {
	operation, cancel := context.WithTimeout(ctx, p.config.DockerTimeout)
	defer cancel()
	item, err := p.docker.VolumeInspect(operation, run.VolumeIdentity)
	if err != nil {
		return volume.Volume{}, fmt.Errorf("inspect background run volume: %w", err)
	}
	return item, p.attestRunVolume(run, digest, item)
}

func (p *Provider) attestRunVolume(run store.BackgroundRun, digest string, item volume.Volume) error {
	if err := p.attestVolume(run, digest, item); err != nil {
		return &IdentityError{Resource: "volume", Identity: run.VolumeIdentity, Reason: err.Error()}
	}
	return p.attestExecutionVolume(run, item)
}

func (p *Provider) attestVolume(run store.BackgroundRun, digest string, item volume.Volume) error {
	if item.Name != run.VolumeIdentity || !maps.Equal(item.Labels, p.labels(run, digest)) {
		return errors.New("Docker name or labels do not match the immutable run")
	}
	if item.Driver != "local" || item.Scope != "local" || (len(item.Options) != 0 && !maps.Equal(item.Options, p.volumeOptions(run))) || item.ClusterVolume != nil || len(item.Status) != 0 {
		return errors.New("Docker volume is not an option-free local-scope local-driver volume")
	}
	if item.Mountpoint == "" || !filepath.IsAbs(item.Mountpoint) {
		return errors.New("Docker local volume mountpoint is not an absolute daemon path")
	}
	return nil
}

// EnsureContainer creates the exact stopped container or reconciles a lost
// create response. It never starts a container.
func (p *Provider) EnsureContainer(ctx context.Context, run store.BackgroundRun) (_ Observation, resultErr error) {
	digest, err := p.validateRun(run)
	if err != nil {
		return Observation{}, err
	}
	unlock, err := p.acquireCloneAuthority(ctx, run, digest)
	if err != nil {
		return Observation{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	if _, err := p.inspectRunVolume(ctx, run, digest); err != nil {
		return Observation{}, err
	}
	operation, cancel := context.WithTimeout(ctx, p.config.DockerTimeout)
	defer cancel()
	info, err := p.docker.ContainerInspect(operation, run.ContainerIdentity)
	status := "reconciled"
	if errdefs.IsNotFound(err) {
		useInit := true
		pids := containerPIDs
		environment := p.expectedEnvironment(run)
		labels := p.containerLabels(run, digest)
		response, createErr := p.docker.ContainerCreate(operation, &container.Config{
			Image: run.ImageIdentity, User: containerUser, Env: environment,
			Entrypoint: []string{}, Cmd: []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096"},
			WorkingDir: workspaceTarget, ExposedPorts: nat.PortSet{serverPort: struct{}{}}, Volumes: map[string]struct{}{workspaceTarget: {}, opencodeTarget: {}}, Labels: labels,
		}, &container.HostConfig{
			NetworkMode: "bridge", IpcMode: "private", CgroupnsMode: "private", Runtime: "runc", ShmSize: 64 << 20,
			PortBindings: nat.PortMap{serverPort: []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "0"}}},
			Resources:    container.Resources{Memory: p.config.MemoryBytes, MemorySwap: p.config.MemoryBytes * 2, NanoCPUs: containerNanoCPUs, PidsLimit: &pids},
			Init:         &useInit, RestartPolicy: container.RestartPolicy{Name: "no"}, CapDrop: []string{"ALL"}, SecurityOpt: workerSecurityOptions(), ReadonlyRootfs: true, Tmpfs: workerTmpfs(),
			Mounts: []mount.Mount{
				{Type: mount.TypeBind, Source: filepath.Join(p.root, run.CloneIdentity), Target: workspaceTarget},
				{Type: mount.TypeVolume, Source: run.VolumeIdentity, Target: opencodeTarget},
			},
			LogConfig:     container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": p.config.LogMaxSize, "max-file": strconv.Itoa(p.config.LogMaxFiles)}},
			MaskedPaths:   slices.Clone(expectedMaskedPaths),
			ReadonlyPaths: slices.Clone(expectedReadonlyPaths),
		}, &network.NetworkingConfig{}, nil, run.ContainerIdentity)
		status = "created"
		read, readCancel := p.freshDockerContext(ctx)
		defer readCancel()
		info, err = p.docker.ContainerInspect(read, run.ContainerIdentity)
		if err != nil {
			return Observation{}, errors.Join(fmt.Errorf("create background run container: %w", createErr), fmt.Errorf("reconcile created container: %w", err))
		}
		if createErr == nil && response.ID != info.ID {
			return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "create response ID differs from named container"}
		}
		if createErr != nil {
			status = "reconciled"
		}
	} else if err != nil {
		return Observation{}, fmt.Errorf("inspect background run container: %w", err)
	}
	// A container Fern just created from its own request needs no attestation;
	// one found by name (pre-existing or a lost create response) does.
	if status == "reconciled" {
		if err := p.attestContainer(run, digest, info, false); err != nil {
			return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: err.Error()}
		}
	}
	e, _ := makeEvidence(evidence{Effect: "container_create", Identity: run.ContainerIdentity, Spec: digest, Status: status, Container: info.ID})
	return Observation{Evidence: e, ContainerID: info.ID}, nil
}

// StartContainer starts only an exactly attested created container and returns
// its exact Docker process epoch.
func (p *Provider) StartContainer(ctx context.Context, run store.BackgroundRun, expectedID string) (_ Observation, resultErr error) {
	digest, err := p.validateRun(run)
	if err != nil {
		return Observation{}, err
	}
	unlock, err := p.acquireCloneAuthority(ctx, run, digest)
	if err != nil {
		return Observation{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	storage, err := p.inspectRunVolume(ctx, run, digest)
	if err != nil {
		return Observation{}, err
	}
	storageCtx, storageCancel := context.WithTimeout(ctx, p.config.DockerTimeout)
	defer storageCancel()
	if err := p.attestExecutionTree(storageCtx, filepath.Join(p.root, run.CloneIdentity)); err != nil {
		return Observation{}, err
	}
	if len(storage.Options) != 0 {
		if err := p.attestExecutionTree(storageCtx, p.volumeBackingPath(run)); err != nil {
			return Observation{}, err
		}
	}
	operation, cancel := context.WithTimeout(ctx, p.config.DockerTimeout)
	defer cancel()
	info, err := p.docker.ContainerInspect(operation, run.ContainerIdentity)
	if err != nil {
		return Observation{}, fmt.Errorf("inspect background run container: %w", err)
	}
	if expectedID == "" || info.ID != expectedID {
		return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "container ID does not match committed observation"}
	}
	if err := p.attestContainer(run, digest, info, false); err != nil {
		return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: err.Error()}
	}
	status := "running"
	if !info.State.Running {
		if info.State.Status != "created" {
			return Observation{}, fmt.Errorf("container is %s: %w", info.State.Status, ErrRuntimeExited)
		}
		status = "started"
		startErr := p.docker.ContainerStart(operation, expectedID, container.StartOptions{})
		read, readCancel := p.freshDockerContext(ctx)
		defer readCancel()
		info, err = p.docker.ContainerInspect(read, run.ContainerIdentity)
		if err != nil || info.ID != expectedID || info.State == nil || !info.State.Running {
			return Observation{}, errors.Join(fmt.Errorf("start background run container: %w", startErr), err)
		}
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

// attestContainer proves an inspected container is this run's: canonical name,
// qualified image, and Fern's ownership and spec-digest labels. The rest of its
// configuration is Fern's own create request, which the spec label binds;
// destructive and credential operations additionally require the exact
// committed runtime (requireRuntime).
func (p *Provider) attestContainer(run store.BackgroundRun, digest string, info container.InspectResponse, requireRunning bool) error {
	if info.ContainerJSONBase == nil || info.Config == nil || info.State == nil {
		return errors.New("Docker returned incomplete container inspection")
	}
	if info.ID == "" || info.Name != "/"+run.ContainerIdentity || info.Image != run.ImageIdentity || !containsMap(info.Config.Labels, p.labels(run, digest)) {
		return errors.New("container name, image, or Fern labels differ")
	}
	if requireRunning && (!info.State.Running || info.State.Paused || info.State.Restarting || info.State.Dead || info.State.StartedAt == "") {
		return errors.New("container is not exactly running")
	}
	return nil
}

func (p *Provider) expectedEnvironment(run store.BackgroundRun) []string {
	environment := make(map[string]string, len(p.imageEnv)+2)
	maps.Copy(environment, p.imageEnv)
	environment[usernameEnv] = basicUsername
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
	identity, err := domain.NewRuntime(info.ID, info.State.StartedAt)
	if err != nil {
		return RuntimeIdentity{}, 0, errors.New("Docker returned a noncanonical container start timestamp")
	}
	return runtimeFromIdentity(identity), identity.Epoch(), nil
}

func runtimeFromIdentity(identity domain.Runtime) RuntimeIdentity {
	return RuntimeIdentity{ContainerID: identity.ContainerID(), StartedAt: identity.StartedAt(), Token: identity.Token()}
}

func validateCommittedRuntime(runtime RuntimeIdentity) error {
	_, err := domain.ParseRuntime(runtime.ContainerID, runtime.StartedAt, runtime.Token)
	return err
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

func (p *Provider) freshDockerContext(parent context.Context) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(p.config.DockerTimeout)
	if parentDeadline, ok := parent.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	return context.WithDeadline(context.WithoutCancel(parent), deadline)
}

// Health proves exact Basic-auth failures and success for the committed runtime.
func (p *Provider) Health(ctx context.Context, run store.BackgroundRun, runtime RuntimeIdentity) (Observation, error) {
	if err := validateCommittedRuntime(runtime); err != nil {
		return Observation{}, err
	}
	digest, err := p.validateRun(run)
	if err != nil {
		return Observation{}, err
	}
	deadline, cancel := context.WithTimeout(ctx, p.config.HealthTimeout)
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

func (p *Provider) healthOnce(ctx context.Context, run store.BackgroundRun, digest string, runtime RuntimeIdentity) (Observation, error) {
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
	}{{"missing", "", "", http.StatusUnauthorized, []byte(`{"_tag":"UnauthorizedError","message":"Authentication required"}`)}, {"wrong", basicUsername, password + "-wrong", http.StatusUnauthorized, []byte(`{"_tag":"UnauthorizedError","message":"Authentication required"}`)}, {"correct", basicUsername, password, http.StatusOK, []byte(`{"healthy":true}`)}} {
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
func (p *Provider) StopContainer(ctx context.Context, run store.BackgroundRun, runtime RuntimeIdentity) (Observation, error) {
	digest, err := p.validateRunForCleanup(run)
	if err != nil {
		return Observation{}, err
	}
	if err := validateCommittedRuntime(runtime); err != nil {
		return Observation{}, err
	}
	operation, cancel := context.WithTimeout(ctx, p.config.DockerTimeout+p.config.StopGrace)
	defer cancel()
	inactive := func(status string) (Observation, error) {
		e, _ := makeEvidence(evidence{Effect: "writer_inactive", Identity: run.ContainerIdentity, Spec: digest, Status: status, Container: runtime.ContainerID, Started: runtime.StartedAt, Runtime: runtime.Token})
		return Observation{Evidence: e, ContainerID: runtime.ContainerID, ContainerStarted: runtime.StartedAt, RuntimeToken: runtime.Token}, nil
	}
	info, err := p.docker.ContainerInspect(operation, run.ContainerIdentity)
	if errdefs.IsNotFound(err) {
		if err := p.requireNoStrayContainer(operation, run, digest, runtime.ContainerID); err != nil {
			return Observation{}, err
		}
		return inactive("absent")
	}
	if err != nil {
		return Observation{}, fmt.Errorf("inspect background run container: %w", err)
	}
	if info.ID != runtime.ContainerID {
		return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "container ID does not match stop authority"}
	}
	if err := p.attestContainer(run, digest, info, false); err != nil {
		return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: err.Error()}
	}
	if err := requireRuntime(info, runtime); err != nil {
		return Observation{}, err
	}
	if !info.State.Running {
		return inactive("already_stopped")
	}
	seconds := int(p.config.StopGrace / time.Second)
	stopErr := p.docker.ContainerStop(operation, runtime.ContainerID, container.StopOptions{Timeout: &seconds})
	read, readCancel := p.freshDockerContext(ctx)
	defer readCancel()
	info, err = p.docker.ContainerInspect(read, run.ContainerIdentity)
	if err != nil || info.ID != runtime.ContainerID || info.State == nil || info.State.Running {
		return Observation{}, errors.Join(fmt.Errorf("stop exact container: %w", stopErr), err)
	}
	if err := requireRuntime(info, runtime); err != nil {
		return Observation{}, err
	}
	return inactive("stopped")
}

// ProveWriterInactive resolves one explicit writer fence from exact
// provider-owned labels. It never creates a resource and never deletes an
// identity that was not fully attested.
func (p *Provider) ProveWriterInactive(ctx context.Context, run store.BackgroundRun) (Observation, WriterFence, error) {
	digest, err := p.validateRunForCleanup(run)
	if err != nil {
		return Observation{}, WriterFence{}, err
	}
	uncommitted := run.ObservedContainerID == "" && run.ObservedContainerStartedAt == "" && run.RuntimeEpoch == 0
	operation, cancel := context.WithTimeout(ctx, p.config.DockerTimeout)
	defer cancel()
	info, err := p.docker.ContainerInspect(operation, run.ContainerIdentity)
	if errdefs.IsNotFound(err) {
		if err := p.requireNoStrayContainer(operation, run, digest, ""); err != nil {
			return Observation{}, WriterFence{}, err
		}
		if uncommitted {
			e, _ := makeEvidence(evidence{Effect: "writer_inactive", Identity: run.ContainerIdentity, Spec: digest, Status: "never_created"})
			return Observation{Evidence: e}, NeverCreatedAuthority(), nil
		}
		committed, err := committedRuntimeFromRun(run)
		if err != nil {
			return Observation{}, WriterFence{}, err
		}
		e, _ := makeEvidence(evidence{Effect: "writer_inactive", Identity: run.ContainerIdentity, Spec: digest, Status: "committed_runtime_absent",
			Container: committed.ContainerID, Started: committed.StartedAt, Runtime: committed.Token})
		return Observation{Evidence: e, ContainerID: committed.ContainerID, ContainerStarted: committed.StartedAt,
			RuntimeToken: committed.Token}, RuntimeCleanupAuthority(committed), nil
	}
	if err != nil {
		return Observation{}, WriterFence{}, fmt.Errorf("inspect background run container: %w", err)
	}
	if err := p.attestContainer(run, digest, info, false); err != nil {
		return Observation{}, WriterFence{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: err.Error()}
	}
	if info.State.Status == "created" {
		if !uncommitted {
			return Observation{}, WriterFence{}, &IdentityError{Resource: "runtime", Identity: run.ContainerIdentity, Reason: "created container differs from committed runtime"}
		}
		e, _ := makeEvidence(evidence{Effect: "writer_inactive", Identity: run.ContainerIdentity, Spec: digest, Status: "never_started", Container: info.ID})
		return Observation{Evidence: e, ContainerID: info.ID}, CreatedContainerAuthority(info.ID), nil
	}
	runtime, _, err := runtimeIdentity(info)
	if err != nil {
		return Observation{}, WriterFence{}, err
	}
	// A stop or timeout can commit between StartContainer and recording its
	// runtime, or Fern can crash there. The exactly attested container under
	// this run's canonical name is then this run's only possible writer, so
	// adopt its observed runtime as cleanup authority instead of failing
	// forever. Any partially committed identity still requires an exact match.
	if !uncommitted {
		committed, err := committedRuntimeFromRun(run)
		if err != nil || runtime != committed {
			return Observation{}, WriterFence{}, &IdentityError{Resource: "runtime", Identity: run.ContainerIdentity, Reason: "container ID or exact start epoch differs from committed observation"}
		}
	}
	if info.State.Running {
		observation, stopErr := p.StopContainer(ctx, run, runtime)
		return observation, RuntimeCleanupAuthority(runtime), stopErr
	}
	e, _ := makeEvidence(evidence{Effect: "writer_inactive", Identity: run.ContainerIdentity, Spec: digest, Status: "already_stopped", Container: info.ID, Started: runtime.StartedAt, Runtime: runtime.Token})
	return Observation{Evidence: e, ContainerID: info.ID, ContainerStarted: runtime.StartedAt, RuntimeToken: runtime.Token}, RuntimeCleanupAuthority(runtime), nil
}

// RemoveContainer removes only the exact attested stopped runtime.
func (p *Provider) RemoveContainer(ctx context.Context, run store.BackgroundRun, authority WriterFence) (Observation, error) {
	digest, err := p.validateRunForCleanup(run)
	if err != nil {
		return Observation{}, err
	}
	kind, err := validateCleanupAuthority(authority)
	if err != nil {
		return Observation{}, err
	}
	operation, cancel := context.WithTimeout(ctx, p.config.DockerTimeout)
	defer cancel()
	info, err := p.docker.ContainerInspect(operation, run.ContainerIdentity)
	if errdefs.IsNotFound(err) {
		if err := p.requireNoStrayContainer(operation, run, digest, authority.ContainerID()); err != nil {
			return Observation{}, err
		}
		e, _ := makeEvidence(evidence{Effect: "container_remove", Identity: run.ContainerIdentity, Spec: digest, Status: "absent"})
		return Observation{Evidence: e}, nil
	}
	if err != nil {
		return Observation{}, fmt.Errorf("inspect background run container: %w", err)
	}
	if kind == WriterFenceNeverCreated || authority.ContainerID() == "" || info.ID != authority.ContainerID() {
		return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "container ID does not match removal authority"}
	}
	if err := p.attestContainer(run, digest, info, false); err != nil {
		return Observation{}, &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: err.Error()}
	}
	if info.State.Status == "created" {
		if kind != WriterFenceCreatedNeverStarted {
			return Observation{}, errors.New("created container cleanup requires its exact ID without a process epoch")
		}
	} else {
		if kind != WriterFenceStoppedRuntime {
			return Observation{}, errors.New("a container that started requires full committed runtime cleanup authority")
		}
		if err := requireRuntime(info, authority.runtimeIdentity()); err != nil {
			return Observation{}, err
		}
	}
	if info.State.Running {
		return Observation{}, errors.New("refusing to remove a running background container")
	}
	removeErr := p.docker.ContainerRemove(operation, authority.ContainerID(), container.RemoveOptions{})
	read, readCancel := p.freshDockerContext(ctx)
	defer readCancel()
	_, nameErr := p.docker.ContainerInspect(read, run.ContainerIdentity)
	_, idErr := p.docker.ContainerInspect(read, authority.ContainerID())
	if !errdefs.IsNotFound(nameErr) || !errdefs.IsNotFound(idErr) {
		return Observation{}, errors.Join(fmt.Errorf("remove exact container: %w", removeErr), fmt.Errorf("canonical-name post-remove inspect: %w", nameErr), fmt.Errorf("container-ID post-remove inspect: %w", idErr))
	}
	e, _ := makeEvidence(evidence{Effect: "container_remove", Identity: run.ContainerIdentity, Spec: digest, Status: "removed", Container: authority.ContainerID()})
	return Observation{Evidence: e}, nil
}

// RemoveVolume removes only the exact attested volume after the exact runtime is absent.
func (p *Provider) RemoveVolume(ctx context.Context, run store.BackgroundRun, authority WriterFence) (_ Observation, resultErr error) {
	digest, err := p.validateRunForCleanup(run)
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
	if err := p.requireContainerAbsent(ctx, run, digest, authority.ContainerID()); err != nil {
		return Observation{}, err
	}
	operation, cancel := context.WithTimeout(ctx, p.config.DockerTimeout)
	defer cancel()
	item, err := p.docker.VolumeInspect(operation, run.VolumeIdentity)
	if errdefs.IsNotFound(err) {
		if err := p.requireNoStrayVolume(operation, run, digest); err != nil {
			return Observation{}, err
		}
		if clonePresent {
			if err := p.removeVolumeBacking(run); err != nil {
				return Observation{}, err
			}
		}
		e, _ := makeEvidence(evidence{Effect: "volume_remove", Identity: run.VolumeIdentity, Spec: digest, Status: "absent"})
		return Observation{Evidence: e}, nil
	}
	if err != nil {
		return Observation{}, fmt.Errorf("inspect background run volume: %w", err)
	}
	if !clonePresent {
		return Observation{}, &IdentityError{Resource: "volume", Identity: run.VolumeIdentity, Reason: "volume exists without private clone authority"}
	}
	if err := p.attestVolume(run, digest, item); err != nil {
		return Observation{}, &IdentityError{Resource: "volume", Identity: run.VolumeIdentity, Reason: err.Error()}
	}
	removeErr := p.docker.VolumeRemove(operation, run.VolumeIdentity, false)
	read, readCancel := p.freshDockerContext(ctx)
	defer readCancel()
	_, inspectErr := p.docker.VolumeInspect(read, run.VolumeIdentity)
	if !errdefs.IsNotFound(inspectErr) {
		return Observation{}, errors.Join(fmt.Errorf("remove exact volume: %w", removeErr), fmt.Errorf("post-remove volume inspect: %w", inspectErr))
	}
	listed, listErr := p.listRunVolumes(read, run, digest)
	if listErr != nil || len(listed) != 0 {
		return Observation{}, errors.Join(listErr, &IdentityError{Resource: "volume", Identity: run.VolumeIdentity, Reason: "exact-labeled run volume remains after removal"})
	}
	if len(item.Options) != 0 {
		if err := p.removeVolumeBacking(run); err != nil {
			return Observation{}, fmt.Errorf("remove quota-backed volume contents: %w", err)
		}
	}
	e, _ := makeEvidence(evidence{Effect: "volume_remove", Identity: run.VolumeIdentity, Spec: digest, Status: "removed"})
	return Observation{Evidence: e}, nil
}

// requireContainerAbsent proves that no container of this run exists: not
// under its canonical name, not as containerID (when set), and not under any
// other name with the run's exact labels.
func (p *Provider) requireContainerAbsent(ctx context.Context, run store.BackgroundRun, digest, containerID string) error {
	operation, cancel := context.WithTimeout(ctx, p.config.DockerTimeout)
	defer cancel()
	if info, err := p.docker.ContainerInspect(operation, run.ContainerIdentity); err == nil {
		return &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "run container still exists: " + info.ID}
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspect background run container: %w", err)
	}
	return p.requireNoStrayContainer(operation, run, digest, containerID)
}

// requireNoStrayContainer is requireContainerAbsent for callers that have
// already found the canonical name absent.
func (p *Provider) requireNoStrayContainer(ctx context.Context, run store.BackgroundRun, digest, containerID string) error {
	if containerID != "" {
		if _, err := p.docker.ContainerInspect(ctx, containerID); err == nil {
			return &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "expected container ID exists under another name"}
		} else if !errdefs.IsNotFound(err) {
			return fmt.Errorf("inspect expected container ID: %w", err)
		}
	}
	listed, err := p.listRunContainers(ctx, run, digest)
	if err != nil {
		return err
	}
	if len(listed) != 0 {
		return &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "exact-labeled run container exists under a noncanonical name"}
	}
	return nil
}

func (p *Provider) requireVolumeAbsent(ctx context.Context, run store.BackgroundRun, digest string) error {
	operation, cancel := context.WithTimeout(ctx, p.config.DockerTimeout)
	defer cancel()
	if item, err := p.docker.VolumeInspect(operation, run.VolumeIdentity); err == nil {
		return &IdentityError{Resource: "volume", Identity: run.VolumeIdentity, Reason: "volume still exists before clone cleanup: " + item.Name}
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspect background run volume: %w", err)
	}
	return p.requireNoStrayVolume(operation, run, digest)
}

func (p *Provider) requireNoStrayVolume(ctx context.Context, run store.BackgroundRun, digest string) error {
	listed, err := p.listRunVolumes(ctx, run, digest)
	if err != nil {
		return err
	}
	if len(listed) != 0 {
		return &IdentityError{Resource: "volume", Identity: run.VolumeIdentity, Reason: "exact-labeled run volume exists under a noncanonical name"}
	}
	return nil
}

func (p *Provider) listRunContainers(ctx context.Context, run store.BackgroundRun, digest string) ([]container.Summary, error) {
	items, err := p.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: p.labelFilter(run, digest)})
	if err != nil {
		return nil, fmt.Errorf("list exact-labeled run containers: %w", err)
	}
	return items, nil
}

func (p *Provider) listRunVolumes(ctx context.Context, run store.BackgroundRun, digest string) ([]*volume.Volume, error) {
	response, err := p.docker.VolumeList(ctx, volume.ListOptions{Filters: p.labelFilter(run, digest)})
	if err != nil {
		return nil, fmt.Errorf("list exact-labeled run volumes: %w", err)
	}
	return response.Volumes, nil
}

// labelFilter matches resources carrying every one of the run's exact labels.
func (p *Provider) labelFilter(run store.BackgroundRun, digest string) filters.Args {
	args := filters.NewArgs()
	for key, value := range p.labels(run, digest) {
		args.Add("label", key+"="+value)
	}
	return args
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
