package taskenvdocker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	runtimeRoleLabel      = "dev.fern.background-run.runtime-role"
	writerGenerationLabel = "dev.fern.background-run.writer-generation"
	ShellRoleHuman        = "human"
	ShellRoleInspector    = "inspector"
)

type Terminal struct {
	reader *bufio.Reader
	conn   net.Conn
}

func (t *Terminal) Read(value []byte) (int, error)  { return t.reader.Read(value) }
func (t *Terminal) Write(value []byte) (int, error) { return t.conn.Write(value) }
func (t *Terminal) Close() error                    { return t.conn.Close() }

type dockerAttacher interface {
	ContainerAttach(context.Context, string, container.AttachOptions) (types.HijackedResponse, error)
	ContainerResize(context.Context, string, container.ResizeOptions) error
}

func ShellContainerIdentity(run taskstore.BackgroundRun, writerGeneration int64, role string) (string, error) {
	if writerGeneration < 1 || (role != ShellRoleHuman && role != ShellRoleInspector) {
		return "", errors.New("valid shell writer generation and role are required")
	}
	compact := strings.ReplaceAll(strings.TrimPrefix(string(run.TaskID), "tsk_"), "-", "")
	if compact == "" || run.Generation < 1 {
		return "", errors.New("valid run identity is required")
	}
	return fmt.Sprintf("fern-run-%s-g%d-w%d-%s", compact, run.Generation, writerGeneration, role), nil
}

func (p *Provider) EnsureShellContainer(ctx context.Context, run taskstore.BackgroundRun, writerGeneration int64, role string) (_ Observation, resultErr error) {
	digest, err := p.validateRun(run)
	if err != nil {
		return Observation{}, err
	}
	identity, err := ShellContainerIdentity(run, writerGeneration, role)
	if err != nil {
		return Observation{}, err
	}
	unlock, err := p.acquireCloneAuthority(ctx, run, digest)
	if err != nil {
		return Observation{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	operation, cancel := operationContext(ctx, p.config.DockerTimeout)
	defer cancel()
	info, err := p.docker.ContainerInspect(operation, identity)
	status := "reconciled"
	if errdefs.IsNotFound(err) {
		shells, listErr := p.listShellContainers(operation, run, writerGeneration, role)
		if listErr != nil {
			return Observation{}, listErr
		}
		if len(shells) != 0 {
			return Observation{}, &IdentityError{Resource: "container", Identity: identity, Reason: "exact shell exists under another name"}
		}
		if role == ShellRoleHuman {
			writers, listErr := p.listWriterContainers(operation, run)
			if listErr != nil {
				return Observation{}, listErr
			}
			if len(writers) != 0 {
				return Observation{}, &IdentityError{Resource: "writer", Identity: identity, Reason: "another run writer container still exists"}
			}
		}
		useInit := true
		pids := p.config.PIDs
		readOnly := role == ShellRoleInspector
		response, createErr := p.docker.ContainerCreate(operation, &container.Config{
			Image: run.ImageIdentity, User: containerUser, Env: p.shellEnvironment(),
			Cmd: []string{"/bin/bash", "--noprofile", "--norc"}, WorkingDir: workspaceTarget,
			AttachStdin: true, AttachStdout: true, AttachStderr: true, Tty: true, OpenStdin: true, StdinOnce: false,
			NetworkDisabled: true, Labels: p.shellLabels(run, digest, identity, writerGeneration, role),
			Volumes: map[string]struct{}{workspaceTarget: {}, opencodeTarget: {}}, ExposedPorts: nat.PortSet{serverPort: struct{}{}},
		}, &container.HostConfig{
			NetworkMode: "none", IpcMode: "private", CgroupnsMode: "private", Runtime: "runc", ShmSize: 64 << 20,
			ReadonlyRootfs: true,
			Resources:      container.Resources{Memory: p.config.MemoryBytes, MemorySwap: p.config.MemoryBytes * 2, NanoCPUs: p.config.NanoCPUs, PidsLimit: &pids},
			Init:           &useInit, RestartPolicy: container.RestartPolicy{Name: "no"}, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"},
			Mounts: []mount.Mount{{Type: mount.TypeBind, Source: filepath.Join(p.root, run.CloneIdentity), Target: workspaceTarget, ReadOnly: readOnly}},
			Tmpfs: map[string]string{
				"/tmp":         "rw,noexec,nosuid,nodev,size=67108864,mode=1777,uid=1001,gid=1001",
				opencodeTarget: "rw,noexec,nosuid,nodev,size=67108864,mode=0700,uid=1001,gid=1001",
			},
			LogConfig:   container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": p.config.LogMaxSize, "max-file": strconv.Itoa(p.config.LogMaxFiles)}},
			MaskedPaths: slices.Clone(expectedMaskedPaths), ReadonlyPaths: slices.Clone(expectedReadonlyPaths),
		}, &network.NetworkingConfig{}, nil, identity)
		status = "created"
		cancel()
		read, readCancel := p.freshDockerContext(ctx)
		info, err = p.docker.ContainerInspect(read, identity)
		readCancel()
		if err != nil {
			return Observation{}, errors.Join(fmt.Errorf("create %s shell container: %w", role, createErr), err)
		}
		if createErr == nil && response.ID != info.ID {
			return Observation{}, &IdentityError{Resource: "container", Identity: identity, Reason: "create response ID differs from named shell container"}
		}
	} else if err != nil {
		return Observation{}, err
	}
	if err := p.attestShellContainer(run, digest, info, writerGeneration, role, false); err != nil {
		return Observation{}, &IdentityError{Resource: "container", Identity: identity, Reason: err.Error()}
	}
	e, _ := makeEvidence(evidence{Effect: "shell_create", Identity: identity, Spec: digest, Status: status, Container: info.ID})
	return Observation{Evidence: e, ContainerID: info.ID}, nil
}

func (p *Provider) StartShellContainer(ctx context.Context, run taskstore.BackgroundRun, writerGeneration int64, role, expectedID string) (Observation, error) {
	digest, err := p.validateRun(run)
	if err != nil {
		return Observation{}, err
	}
	identity, err := ShellContainerIdentity(run, writerGeneration, role)
	if err != nil {
		return Observation{}, err
	}
	operation, cancel := operationContext(ctx, p.config.DockerTimeout)
	info, err := p.docker.ContainerInspect(operation, identity)
	if err != nil || info.ID != expectedID {
		cancel()
		return Observation{}, errors.Join(err, &IdentityError{Resource: "container", Identity: identity, Reason: "shell container ID differs"})
	}
	if err := p.attestShellContainer(run, digest, info, writerGeneration, role, false); err != nil {
		cancel()
		return Observation{}, &IdentityError{Resource: "container", Identity: identity, Reason: err.Error()}
	}
	if !info.State.Running {
		if info.State.Status != "created" {
			cancel()
			return Observation{}, errors.New("shell container may not be restarted")
		}
		startErr := p.docker.ContainerStart(operation, expectedID, container.StartOptions{})
		cancel()
		read, readCancel := p.freshDockerContext(ctx)
		info, err = p.docker.ContainerInspect(read, identity)
		readCancel()
		if err != nil || startErr != nil {
			return Observation{}, errors.Join(startErr, err)
		}
	} else {
		cancel()
	}
	if err := p.attestShellContainer(run, digest, info, writerGeneration, role, true); err != nil {
		return Observation{}, &IdentityError{Resource: "container", Identity: identity, Reason: err.Error()}
	}
	runtime, epoch, err := runtimeIdentity(info)
	if err != nil {
		return Observation{}, err
	}
	e, _ := makeEvidence(evidence{Effect: "shell_start", Identity: identity, Spec: digest, Status: "running", Container: info.ID, Started: runtime.StartedAt, Runtime: runtime.Token})
	return Observation{Evidence: e, ContainerID: info.ID, ContainerStarted: runtime.StartedAt, RuntimeEpoch: epoch, RuntimeToken: runtime.Token}, nil
}

func (p *Provider) StopShellContainer(ctx context.Context, run taskstore.BackgroundRun, writerGeneration int64, role string, runtime RuntimeIdentity) (Observation, error) {
	digest, err := p.validateRun(run)
	if err != nil {
		return Observation{}, err
	}
	identity, err := ShellContainerIdentity(run, writerGeneration, role)
	if err != nil {
		return Observation{}, err
	}
	operation, cancel := operationContext(ctx, p.config.DockerTimeout+p.config.StopGrace)
	info, err := p.docker.ContainerInspect(operation, identity)
	if errdefs.IsNotFound(err) {
		if _, idErr := p.docker.ContainerInspect(operation, runtime.ContainerID); idErr == nil {
			cancel()
			return Observation{}, &IdentityError{Resource: "container", Identity: identity, Reason: "committed shell exists under another name"}
		} else if !errdefs.IsNotFound(idErr) {
			cancel()
			return Observation{}, idErr
		}
		shells, listErr := p.listShellContainers(operation, run, writerGeneration, role)
		if listErr != nil {
			cancel()
			return Observation{}, listErr
		}
		if len(shells) != 0 {
			cancel()
			return Observation{}, &IdentityError{Resource: "container", Identity: identity, Reason: "exact shell exists under another name"}
		}
		cancel()
		e, _ := makeEvidence(evidence{Effect: "shell_stop", Identity: identity, Spec: digest, Status: "absent", Container: runtime.ContainerID, Started: runtime.StartedAt, Runtime: runtime.Token})
		return Observation{Evidence: e, ContainerID: runtime.ContainerID, ContainerStarted: runtime.StartedAt, RuntimeToken: runtime.Token}, nil
	}
	if err != nil {
		cancel()
		return Observation{}, err
	}
	if err := p.attestShellContainer(run, digest, info, writerGeneration, role, info.State.Running); err != nil || requireRuntime(info, runtime) != nil {
		cancel()
		return Observation{}, &IdentityError{Resource: "container", Identity: identity, Reason: "shell runtime identity differs"}
	}
	if info.State.Running {
		seconds := int(p.config.StopGrace.Seconds())
		stopErr := p.docker.ContainerStop(operation, runtime.ContainerID, container.StopOptions{Timeout: &seconds})
		cancel()
		read, readCancel := p.freshDockerContext(ctx)
		info, err = p.docker.ContainerInspect(read, identity)
		readCancel()
		if err != nil || info.State == nil || info.State.Running {
			return Observation{}, errors.Join(stopErr, err)
		}
	} else {
		cancel()
	}
	if err := p.attestShellContainer(run, digest, info, writerGeneration, role, false); err != nil || requireRuntime(info, runtime) != nil {
		return Observation{}, &IdentityError{Resource: "container", Identity: identity, Reason: "post-stop shell identity differs"}
	}
	e, _ := makeEvidence(evidence{Effect: "shell_stop", Identity: identity, Spec: digest, Status: "stopped", Container: runtime.ContainerID, Started: runtime.StartedAt, Runtime: runtime.Token})
	return Observation{Evidence: e, ContainerID: runtime.ContainerID, ContainerStarted: runtime.StartedAt, RuntimeToken: runtime.Token}, nil
}

func (p *Provider) RemoveShellContainer(ctx context.Context, run taskstore.BackgroundRun, writerGeneration int64, role string, runtime RuntimeIdentity) (Observation, error) {
	digest, err := p.validateRun(run)
	if err != nil {
		return Observation{}, err
	}
	identity, err := ShellContainerIdentity(run, writerGeneration, role)
	if err != nil {
		return Observation{}, err
	}
	operation, cancel := operationContext(ctx, p.config.DockerTimeout)
	info, err := p.docker.ContainerInspect(operation, identity)
	if errdefs.IsNotFound(err) {
		if _, idErr := p.docker.ContainerInspect(operation, runtime.ContainerID); idErr == nil {
			cancel()
			return Observation{}, &IdentityError{Resource: "container", Identity: identity, Reason: "expected shell exists under another name"}
		} else if !errdefs.IsNotFound(idErr) {
			cancel()
			return Observation{}, idErr
		}
		shells, listErr := p.listShellContainers(operation, run, writerGeneration, role)
		if listErr != nil {
			cancel()
			return Observation{}, listErr
		}
		if len(shells) != 0 {
			cancel()
			return Observation{}, &IdentityError{Resource: "container", Identity: identity, Reason: "exact shell exists under another name"}
		}
		cancel()
		e, _ := makeEvidence(evidence{Effect: "shell_remove", Identity: identity, Spec: digest, Status: "absent"})
		return Observation{Evidence: e}, nil
	}
	if err != nil {
		cancel()
		return Observation{}, err
	}
	if info.State.Running || requireRuntime(info, runtime) != nil {
		cancel()
		return Observation{}, &IdentityError{Resource: "container", Identity: identity, Reason: "refusing to remove a running or replacement shell"}
	}
	if err := p.attestShellContainer(run, digest, info, writerGeneration, role, false); err != nil {
		cancel()
		return Observation{}, &IdentityError{Resource: "container", Identity: identity, Reason: err.Error()}
	}
	removeErr := p.docker.ContainerRemove(operation, runtime.ContainerID, container.RemoveOptions{})
	cancel()
	read, readCancel := p.freshDockerContext(ctx)
	defer readCancel()
	_, inspectErr := p.docker.ContainerInspect(read, identity)
	if !errdefs.IsNotFound(inspectErr) {
		return Observation{}, errors.Join(removeErr, inspectErr)
	}
	if _, idErr := p.docker.ContainerInspect(read, runtime.ContainerID); !errdefs.IsNotFound(idErr) {
		return Observation{}, errors.Join(removeErr, idErr)
	}
	shells, listErr := p.listShellContainers(read, run, writerGeneration, role)
	if listErr != nil || len(shells) != 0 {
		return Observation{}, errors.Join(removeErr, listErr, &IdentityError{Resource: "container", Identity: identity, Reason: "shell remains after removal"})
	}
	e, _ := makeEvidence(evidence{Effect: "shell_remove", Identity: identity, Spec: digest, Status: "removed", Container: runtime.ContainerID})
	return Observation{Evidence: e}, nil
}

// RemoveInspector reconciles the process-local read-only shell at takeover.
// Inspector identity is deterministic and never grants writer authority.
func (p *Provider) RemoveInspector(ctx context.Context, run taskstore.BackgroundRun, writerGeneration int64) (Observation, error) {
	digest, err := p.validateRun(run)
	if err != nil {
		return Observation{}, err
	}
	identity, err := ShellContainerIdentity(run, writerGeneration, ShellRoleInspector)
	if err != nil {
		return Observation{}, err
	}
	operation, cancel := operationContext(ctx, p.config.DockerTimeout)
	info, err := p.docker.ContainerInspect(operation, identity)
	if errdefs.IsNotFound(err) {
		shells, listErr := p.listShellContainers(operation, run, writerGeneration, ShellRoleInspector)
		cancel()
		if listErr != nil {
			return Observation{}, listErr
		}
		if len(shells) != 0 {
			return Observation{}, &IdentityError{Resource: "container", Identity: identity, Reason: "inspector exists under another name"}
		}
		e, _ := makeEvidence(evidence{Effect: "inspector_remove", Identity: identity, Spec: digest, Status: "absent"})
		return Observation{Evidence: e}, nil
	}
	cancel()
	if err != nil {
		return Observation{}, err
	}
	if err := p.attestShellContainer(run, digest, info, writerGeneration, ShellRoleInspector, info.State.Running); err != nil {
		return Observation{}, &IdentityError{Resource: "container", Identity: identity, Reason: err.Error()}
	}
	if info.State.Status == "created" {
		operation, operationCancel := operationContext(ctx, p.config.DockerTimeout)
		removeErr := p.docker.ContainerRemove(operation, info.ID, container.RemoveOptions{})
		operationCancel()
		read, readCancel := p.freshDockerContext(ctx)
		defer readCancel()
		_, inspectErr := p.docker.ContainerInspect(read, identity)
		if !errdefs.IsNotFound(inspectErr) {
			return Observation{}, errors.Join(removeErr, inspectErr)
		}
		e, _ := makeEvidence(evidence{Effect: "inspector_remove", Identity: identity, Spec: digest, Status: "removed", Container: info.ID})
		return Observation{Evidence: e}, nil
	}
	runtime, _, err := runtimeIdentity(info)
	if err != nil {
		return Observation{}, err
	}
	if _, err := p.StopShellContainer(ctx, run, writerGeneration, ShellRoleInspector, runtime); err != nil {
		return Observation{}, err
	}
	return p.RemoveShellContainer(ctx, run, writerGeneration, ShellRoleInspector, runtime)
}

func (p *Provider) AttachShell(ctx context.Context, run taskstore.BackgroundRun, writerGeneration int64, role string, runtime RuntimeIdentity) (*Terminal, error) {
	digest, err := p.validateRun(run)
	if err != nil {
		return nil, err
	}
	identity, err := ShellContainerIdentity(run, writerGeneration, role)
	if err != nil {
		return nil, err
	}
	operation, cancel := operationContext(ctx, p.config.DockerTimeout)
	info, err := p.docker.ContainerInspect(operation, identity)
	cancel()
	if err != nil || p.attestShellContainer(run, digest, info, writerGeneration, role, true) != nil || requireRuntime(info, runtime) != nil {
		return nil, errors.Join(err, ErrIdentityMismatch)
	}
	attacher, ok := p.docker.(dockerAttacher)
	if !ok {
		return nil, errors.New("Docker terminal attach is unavailable")
	}
	response, err := attacher.ContainerAttach(ctx, runtime.ContainerID, container.AttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		return nil, err
	}
	return &Terminal{reader: response.Reader, conn: response.Conn}, nil
}

func (p *Provider) ResizeShell(ctx context.Context, runtime RuntimeIdentity, height, width uint) error {
	if height < 1 || width < 1 || height > 1000 || width > 1000 || validateCommittedRuntime(runtime) != nil {
		return errors.New("valid terminal identity and dimensions are required")
	}
	attacher, ok := p.docker.(dockerAttacher)
	if !ok {
		return errors.New("Docker terminal resize is unavailable")
	}
	operation, cancel := operationContext(ctx, p.config.DockerTimeout)
	defer cancel()
	return attacher.ContainerResize(operation, runtime.ContainerID, container.ResizeOptions{Height: height, Width: width})
}

// ObserveGitBoundary records bounded Git metadata only after every writer
// container for the run is proven absent. It is a handoff boundary, not a
// retained-result manifest or a claim that the worktree is clean.
func (p *Provider) ObserveGitBoundary(ctx context.Context, run taskstore.BackgroundRun) (_ string, resultErr error) {
	digest, err := p.validateRun(run)
	if err != nil {
		return "", err
	}
	unlock, err := p.acquireCloneAuthority(ctx, run, digest)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	operation, cancel := operationContext(ctx, p.config.GitTimeout)
	defer cancel()
	writers, err := p.listWriterContainers(operation, run)
	if err != nil {
		return "", err
	}
	if len(writers) != 0 {
		return "", &IdentityError{Resource: "writer", Identity: run.ContainerIdentity, Reason: "writer exists at Git boundary"}
	}
	if _, err := p.attestClone(operation, run, digest, filepath.Join(p.root, run.CloneIdentity), false); err != nil {
		return "", err
	}
	head, err := p.git(operation, filepath.Join(p.root, run.CloneIdentity), "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	status, err := p.git(operation, filepath.Join(p.root, run.CloneIdentity), "status", "--porcelain=v2", "--untracked-files=all", "--ignored=no")
	if err != nil {
		return "", err
	}
	diff, err := p.git(operation, filepath.Join(p.root, run.CloneIdentity), "diff", "--binary", "--no-ext-diff", "HEAD", "--")
	if err != nil {
		return "", err
	}
	untracked, err := p.git(operation, filepath.Join(p.root, run.CloneIdentity), "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	workspaceHash := sha256.New()
	_, _ = workspaceHash.Write([]byte(status))
	_, _ = workspaceHash.Write([]byte{0})
	_, _ = workspaceHash.Write([]byte(diff))
	for _, name := range strings.Split(untracked, "\x00") {
		if name == "" {
			continue
		}
		oid, hashErr := p.git(operation, filepath.Join(p.root, run.CloneIdentity), "hash-object", "--no-filters", "--", name)
		if hashErr != nil {
			return "", hashErr
		}
		_, _ = workspaceHash.Write([]byte{0})
		_, _ = workspaceHash.Write([]byte(name))
		_, _ = workspaceHash.Write([]byte{0})
		_, _ = workspaceHash.Write([]byte(strings.TrimSpace(oid)))
	}
	statusDigest := sha256.Sum256([]byte(status))
	evidence, err := json.Marshal(struct {
		Schema          string `json:"schema"`
		Head            string `json:"head"`
		StatusSHA256    string `json:"status_sha256"`
		WorkspaceSHA256 string `json:"workspace_sha256"`
		StatusBytes     int    `json:"status_bytes"`
	}{"fern.background-run.git-boundary.v1", strings.TrimSpace(head), hex.EncodeToString(statusDigest[:]), hex.EncodeToString(workspaceHash.Sum(nil)), len(status)})
	if err != nil || len(evidence) > maxEvidenceBytes {
		return "", errors.Join(err, errors.New("Git boundary evidence exceeds bound"))
	}
	return string(evidence), nil
}

func (p *Provider) shellEnvironment() []string {
	environment := cloneMap(p.imageEnv)
	if environment["PATH"] == "" {
		environment["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	}
	environment["HOME"] = "/tmp/home"
	environment["TERM"] = "dumb"
	environment["XDG_CONFIG_HOME"] = "/tmp/home/.config"
	environment["XDG_DATA_HOME"] = "/tmp/home/.local/share"
	values := make([]string, 0, len(environment))
	for key, value := range environment {
		values = append(values, key+"="+value)
	}
	slices.Sort(values)
	return values
}

func (p *Provider) shellLabels(run taskstore.BackgroundRun, digest, identity string, writerGeneration int64, role string) map[string]string {
	labels := cloneMap(p.imageLabels)
	for key, value := range map[string]string{
		managedLabel: "true", workspaceLabel: string(run.WorkspaceID), taskLabel: string(run.TaskID), attemptLabel: string(run.AttemptID),
		generationLabel: strconv.FormatInt(run.Generation, 10), imageLabel: run.ImageIdentity, cloneLabel: run.CloneIdentity,
		containerLabel: identity, baseLabel: string(run.BaseOID), repositoryLabel: run.RepositoryRemote, profileLabel: run.Profile,
		specLabel: digest, runtimeRoleLabel: role, writerGenerationLabel: strconv.FormatInt(writerGeneration, 10),
	} {
		labels[key] = value
	}
	return labels
}

func (p *Provider) attestShellContainer(run taskstore.BackgroundRun, digest string, info container.InspectResponse, writerGeneration int64, role string, running bool) error {
	identity, err := ShellContainerIdentity(run, writerGeneration, role)
	if err != nil || info.ContainerJSONBase == nil || info.Config == nil || info.HostConfig == nil || info.State == nil || info.NetworkSettings == nil {
		return errors.Join(errors.New("incomplete shell container inspection"), err)
	}
	c, h := info.Config, info.HostConfig
	if info.ID == "" || info.Name != "/"+identity || info.Image != run.ImageIdentity || c.Image != run.ImageIdentity || c.User != containerUser ||
		c.WorkingDir != workspaceTarget || !slices.Equal(c.Cmd, []string{"/bin/bash", "--noprofile", "--norc"}) || len(c.Entrypoint) != 0 ||
		!equalMap(c.Labels, p.shellLabels(run, digest, identity, writerGeneration, role)) {
		return errors.New("shell image, command, environment, or labels differ")
	}
	wantEnv, envErr := parseEnvironment(p.shellEnvironment())
	gotEnv, gotEnvErr := parseEnvironment(c.Env)
	if envErr != nil || gotEnvErr != nil || !equalMap(gotEnv, wantEnv) ||
		!equalMap(c.Volumes, map[string]struct{}{workspaceTarget: {}, opencodeTarget: {}}) ||
		!equalMap(c.ExposedPorts, nat.PortSet{serverPort: struct{}{}}) {
		return errors.New("shell environment, declared volumes, or exposed ports differ")
	}
	if !c.AttachStdin || !c.AttachStdout || !c.AttachStderr || !c.Tty || !c.OpenStdin || c.StdinOnce || !c.NetworkDisabled {
		return errors.New("shell terminal or network flags differ")
	}
	if h.NetworkMode != "none" || !h.ReadonlyRootfs || h.Privileged || h.AutoRemove || !slices.Equal(h.CapDrop, []string{"ALL"}) ||
		!slices.Equal(h.SecurityOpt, []string{"no-new-privileges"}) || len(h.PortBindings) != 0 || len(info.NetworkSettings.Networks) != 0 ||
		!equalMap(h.Tmpfs, map[string]string{
			"/tmp":         "rw,noexec,nosuid,nodev,size=67108864,mode=1777,uid=1001,gid=1001",
			opencodeTarget: "rw,noexec,nosuid,nodev,size=67108864,mode=0700,uid=1001,gid=1001",
		}) {
		return errors.New("shell isolation, network, or filesystem policy differs")
	}
	gotPIDs := int64(-1)
	if h.PidsLimit != nil {
		gotPIDs = *h.PidsLimit
	}
	if h.IpcMode != "private" || h.CgroupnsMode != "private" || h.Runtime != "runc" || h.ShmSize != 64<<20 ||
		h.PidMode != "" || h.UTSMode != "" || h.UsernsMode != "" || h.PublishAllPorts || h.ContainerIDFile != "" || h.VolumeDriver != "" ||
		h.Cgroup != "" || h.Isolation != "" || h.ConsoleSize != [2]uint{} || len(h.Binds) != 0 || len(h.VolumesFrom) != 0 || len(h.CapAdd) != 0 ||
		len(h.DNS) != 0 || len(h.DNSOptions) != 0 || len(h.DNSSearch) != 0 || len(h.ExtraHosts) != 0 || len(h.GroupAdd) != 0 ||
		len(h.Links) != 0 || len(h.Devices) != 0 || len(h.DeviceRequests) != 0 || len(h.DeviceCgroupRules) != 0 || len(h.Ulimits) != 0 ||
		len(h.Sysctls) != 0 || len(h.StorageOpt) != 0 || len(h.Annotations) != 0 || h.Memory != p.config.MemoryBytes ||
		h.MemorySwap != p.config.MemoryBytes*2 || h.MemoryReservation != 0 || h.NanoCPUs != p.config.NanoCPUs || gotPIDs != p.config.PIDs ||
		h.Init == nil || !*h.Init || !h.RestartPolicy.IsNone() || h.LogConfig.Type != "json-file" ||
		!equalMap(h.LogConfig.Config, map[string]string{"max-size": p.config.LogMaxSize, "max-file": strconv.Itoa(p.config.LogMaxFiles)}) ||
		!slices.Equal(h.MaskedPaths, expectedMaskedPaths) || !slices.Equal(h.ReadonlyPaths, expectedReadonlyPaths) {
		return errors.New("shell host, resource, logging, or protected-path policy differs")
	}
	if len(h.Mounts) != 1 || len(info.Mounts) != 1 {
		return errors.New("shell mount count differs")
	}
	readOnly := role == ShellRoleInspector
	want := mount.Mount{Type: mount.TypeBind, Source: filepath.Join(p.root, run.CloneIdentity), Target: workspaceTarget, ReadOnly: readOnly}
	got, workspaceMount := h.Mounts[0], info.Mounts[0]
	if got != want || workspaceMount.Type != mount.TypeBind || workspaceMount.Source != want.Source || workspaceMount.RW == readOnly || workspaceMount.Propagation != mount.PropagationRPrivate {
		return errors.New("shell workspace mount mode differs")
	}
	if running && (!info.State.Running || info.State.Paused || info.State.Restarting || info.State.Dead || info.State.StartedAt == "") {
		return errors.New("shell is not exactly running")
	}
	if !running && info.State.Running {
		return errors.New("shell unexpectedly running")
	}
	return nil
}

func (p *Provider) listWriterContainers(ctx context.Context, run taskstore.BackgroundRun) ([]container.Summary, error) {
	result, err := p.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(
		filters.Arg("label", managedLabel+"=true"), filters.Arg("label", workspaceLabel+"="+string(run.WorkspaceID)),
		filters.Arg("label", taskLabel+"="+string(run.TaskID)), filters.Arg("label", attemptLabel+"="+string(run.AttemptID)),
	)})
	if err != nil {
		return nil, err
	}
	writers := result[:0]
	for _, item := range result {
		if item.Labels[runtimeRoleLabel] != ShellRoleInspector {
			writers = append(writers, item)
		}
	}
	return writers, nil
}

func (p *Provider) listShellContainers(ctx context.Context, run taskstore.BackgroundRun, writerGeneration int64, role string) ([]container.Summary, error) {
	result, err := p.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(
		filters.Arg("label", managedLabel+"=true"), filters.Arg("label", workspaceLabel+"="+string(run.WorkspaceID)),
		filters.Arg("label", taskLabel+"="+string(run.TaskID)), filters.Arg("label", attemptLabel+"="+string(run.AttemptID)),
		filters.Arg("label", runtimeRoleLabel+"="+role), filters.Arg("label", writerGenerationLabel+"="+strconv.FormatInt(writerGeneration, 10)),
	)})
	if err != nil {
		return nil, err
	}
	return result, nil
}

var _ io.ReadWriteCloser = (*Terminal)(nil)
