package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskstore"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "background-run-docker:", err)
		os.Exit(1)
	}
}

func run() (resultErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	imageID := os.Getenv("FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID")
	if imageID == "" {
		return errors.New("FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID is required; run integration/background-run-qualification/run.sh or export the exact local image ID")
	}
	if !canonicalImageID(imageID) {
		return errors.New("FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID must be canonical sha256:<64 lowercase hex>")
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()
	image, err := cli.ImageInspect(ctx, "fern/opencode-background-source:dev")
	if err != nil {
		return fmt.Errorf("inspect already-built source image: %w", err)
	}
	if image.ID != imageID {
		return fmt.Errorf("operator-pinned image ID is %s, local tag resolves to %s", imageID, image.ID)
	}
	temporary, err := os.MkdirTemp("", "fern-background-run-docker-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	temporary, err = filepath.EvalSymlinks(temporary)
	if err != nil {
		return err
	}
	state, repository := filepath.Join(temporary, "state"), filepath.Join(temporary, "repository")
	if err := os.Mkdir(state, 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(repository, 0o700); err != nil {
		return err
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return err
	}
	gitPath, err = filepath.EvalSymlinks(gitPath)
	if err != nil {
		return err
	}
	for _, args := range [][]string{{"init", "--initial-branch=main"}, {"config", "user.name", "Fern Integration"}, {"config", "user.email", "fern@example.invalid"}, {"remote", "add", "origin", "https://github.com/fern-integration/background-run"}} {
		if err := git(repository, gitPath, args...); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("background run fixture\n"), 0o644); err != nil {
		return err
	}
	if err := git(repository, gitPath, "add", "README.md"); err != nil {
		return err
	}
	if err := git(repository, gitPath, "commit", "-m", "fixture"); err != nil {
		return err
	}
	base, err := gitOutput(repository, gitPath, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	ids := task.NewSecureGenerator()
	workspaceID, err := ids.WorkspaceID()
	if err != nil {
		return err
	}
	taskID, err := ids.TaskID()
	if err != nil {
		return err
	}
	attemptID, err := ids.AttemptID()
	if err != nil {
		return err
	}
	sessionID, err := ids.OpenCodeSessionID()
	if err != nil {
		return err
	}
	messageID, err := ids.OpenCodeMessageID()
	if err != nil {
		return err
	}
	compact := strings.ReplaceAll(strings.TrimPrefix(string(taskID), "tsk_"), "-", "")
	run := taskstore.BackgroundRun{WorkspaceID: workspaceID, TaskID: taskID, AttemptID: attemptID, Generation: 1, RepositoryRemote: "https://github.com/fern-integration/background-run", BaseOID: task.GitOID(base), Profile: taskstore.BackgroundRunSourceProfile, EnvironmentSHA256: taskenvdocker.EnvironmentSHA256(nil), ResourceSpecVersion: 9, ImageIdentity: imageID, CloneIdentity: "run-" + compact + "-g1-clone", VolumeIdentity: "fern-run-" + compact + "-g1-opencode", ContainerIdentity: "fern-run-" + compact + "-g1", EndpointIdentity: "run-" + compact + "-g1-endpoint", OpenCodeSessionID: sessionID, OpenCodeMessageID: messageID}
	config := taskenvdocker.Config{StateRoot: state, Repository: repository, GitExecutable: gitPath, ImageReference: "fern/opencode-background-source:dev", ImageID: imageID, MemoryBytes: 512 << 20, NanoCPUs: 2_000_000_000, PIDs: 512, WallTimeout: 2 * time.Minute, GitTimeout: 30 * time.Second, DockerTimeout: 20 * time.Second, HealthTimeout: 60 * time.Second, GitOutputBytes: 1 << 20, SourceSizeAdmissionBytes: 128 << 20, CloneObservedLimitBytes: 128 << 20, DiskFreeAdmissionBytes: 128 << 20, LogMaxSize: "1m", LogMaxFiles: 2, StopGrace: 3 * time.Second}
	provider, err := taskenvdocker.New(ctx, config, nil)
	if err != nil {
		return err
	}
	defer func() { _ = provider.Close() }()
	var containerID string
	var runtime taskenvdocker.RuntimeIdentity
	humanIdentity, err := taskenvdocker.ShellContainerIdentity(run, 2, taskenvdocker.ShellRoleHuman)
	if err != nil {
		return err
	}
	inspectorIdentity, err := taskenvdocker.ShellContainerIdentity(run, 1, taskenvdocker.ShellRoleInspector)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, cleanupHarness(provider, cli, run, containerID, runtime, filepath.Join(state, "background-runs", run.CloneIdentity)))
		resultErr = errors.Join(resultErr, rawRemoveContainer(cli, inspectorIdentity), rawRemoveContainer(cli, humanIdentity))
	}()

	otherTask, err := ids.TaskID()
	if err != nil {
		return err
	}
	otherAttempt, err := ids.AttemptID()
	if err != nil {
		return err
	}
	other := run
	other.TaskID, other.AttemptID = otherTask, otherAttempt
	otherCompact := strings.ReplaceAll(strings.TrimPrefix(string(otherTask), "tsk_"), "-", "")
	other.CloneIdentity = "run-" + otherCompact + "-g1-clone"
	other.VolumeIdentity = "fern-run-" + otherCompact + "-g1-opencode"
	other.ContainerIdentity = "fern-run-" + otherCompact + "-g1"
	other.EndpointIdentity = "run-" + otherCompact + "-g1-endpoint"
	unknown := filepath.Join(state, "background-runs", other.CloneIdentity)
	if err := os.Mkdir(unknown, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(unknown, "unknown"), []byte("retain"), 0o600); err != nil {
		return err
	}
	if _, err := provider.EnsureClone(ctx, other); !errors.Is(err, taskenvdocker.ErrQuarantined) {
		return fmt.Errorf("unknown clone was not quarantined: %v", err)
	}
	if _, err := os.Stat(filepath.Join(unknown, "unknown")); err != nil {
		return errors.New("quarantine removed unknown data")
	}
	if err := os.RemoveAll(unknown); err != nil {
		return err
	}

	cloneObservation, err := provider.EnsureClone(ctx, run)
	if err != nil {
		return err
	}
	clonePath := filepath.Join(state, "background-runs", run.CloneIdentity)
	if common, err := gitOutput(clonePath, gitPath, "rev-parse", "--git-common-dir"); err != nil || common != ".git" {
		return fmt.Errorf("clone common dir=%q error=%v", common, err)
	}
	if _, err := os.Stat(filepath.Join(clonePath, ".git", "objects", "info", "alternates")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("clone has alternates")
	}
	if err := ensureNoSharedFiles(repository, clonePath); err != nil {
		return err
	}
	if cloneObservation.Evidence == "" {
		return errors.New("clone evidence is empty")
	}
	if _, err := provider.EnsureVolume(ctx, run); err != nil {
		return err
	}
	created, err := provider.EnsureContainer(ctx, run)
	if err != nil {
		return err
	}
	containerID = created.ContainerID
	started, err := provider.StartContainer(ctx, run, containerID)
	if err != nil {
		return err
	}
	runtime = started.RuntimeIdentity()
	execution, err := cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{User: "1001:1001", WorkingDir: "/home/user/workspace", Cmd: []string{"touch", "container-uid-1001-write"}})
	if err != nil {
		return err
	}
	if err := cli.ContainerExecStart(ctx, execution.ID, container.ExecStartOptions{}); err != nil {
		return err
	}
	for {
		status, err := cli.ContainerExecInspect(ctx, execution.ID)
		if err != nil {
			return err
		}
		if !status.Running {
			if status.ExitCode != 0 {
				return errors.New("container UID 1001 could not write the clone")
			}
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	info, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return err
	}
	if info.Config.User != "1001:1001" || info.HostConfig.Memory != config.MemoryBytes || info.HostConfig.NanoCPUs != 2_000_000_000 || info.HostConfig.PidsLimit == nil || *info.HostConfig.PidsLimit != 512 || info.HostConfig.Init == nil || !*info.HostConfig.Init || !info.HostConfig.RestartPolicy.IsNone() || len(info.HostConfig.CapDrop) != 1 || info.HostConfig.CapDrop[0] != "ALL" || len(info.HostConfig.Devices) != 0 || len(info.HostConfig.DeviceRequests) != 0 || info.HostConfig.LogConfig.Config["max-size"] != "1m" || len(info.Mounts) != 2 {
		return errors.New("real container security, resources, logs, or mounts differ")
	}
	if info.Config.Labels["dev.fern.background-run.task"] != string(taskID) || info.Config.Labels["dev.fern.background-run.spec"] == "" {
		return errors.New("real container immutable labels differ")
	}
	volumeInfo, err := cli.VolumeInspect(ctx, run.VolumeIdentity)
	if err != nil || volumeInfo.Labels["dev.fern.background-run.task"] != string(taskID) {
		return errors.New("real volume immutable labels differ")
	}
	if started.Endpoint == "" || started.HostPort == 0 {
		return errors.New("real loopback endpoint is absent")
	}
	inspectorCreated, err := provider.EnsureShellContainer(ctx, run, 1, taskenvdocker.ShellRoleInspector)
	if err != nil {
		return fmt.Errorf("create real read-only inspector: %w", err)
	}
	inspectorStarted, err := provider.StartShellContainer(ctx, run, 1, taskenvdocker.ShellRoleInspector, inspectorCreated.ContainerID)
	if err != nil {
		return fmt.Errorf("start real read-only inspector: %w", err)
	}
	inspectorOutput, err := runShellCommand(ctx, provider, run, 1, taskenvdocker.ShellRoleInspector, inspectorStarted.RuntimeIdentity(),
		"touch /home/user/workspace/inspector-must-not-write 2>/dev/null; printf 'INSPECTOR_RC=%s\\n' \"$?\"; exit\n")
	if err != nil || !strings.Contains(inspectorOutput, "INSPECTOR_RC=1") {
		return fmt.Errorf("read-only inspector write fence output=%q error=%v", inspectorOutput, err)
	}
	if _, err := os.Stat(filepath.Join(clonePath, "inspector-must-not-write")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("read-only inspector changed the clone")
	}
	if _, err := provider.RemoveInspector(ctx, run, 1); err != nil {
		return fmt.Errorf("remove real inspector: %w", err)
	}
	if err := provider.Close(); err != nil {
		return err
	}
	provider, err = taskenvdocker.New(ctx, config, nil)
	if err != nil {
		return err
	}
	if _, err := provider.Health(ctx, run, runtime); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(clonePath, "agent-output.txt"), []byte("dirty result\n"), 0o666); err != nil {
		return err
	}
	stopped, err := provider.StopContainer(ctx, run, runtime)
	if err != nil {
		return err
	}
	info, err = cli.ContainerInspect(ctx, containerID)
	if err != nil || info.State.Running || !strings.Contains(stopped.Evidence, "writer_inactive") {
		return errors.New("stop did not prove exact non-running writer inactivity")
	}
	if err := cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("manually restart stopped container: %w", err)
	}
	if _, err := provider.Health(ctx, run, runtime); !errors.Is(err, taskenvdocker.ErrIdentityMismatch) {
		return fmt.Errorf("manual same-container restart was not rejected: %v", err)
	}
	if err := rawRemoveContainer(cli, run.ContainerIdentity); err != nil {
		return err
	}
	containerID = ""
	authority := taskenvdocker.RuntimeCleanupAuthority(runtime)
	if _, err := provider.RemoveVolume(ctx, run, authority); err != nil {
		return err
	}
	if boundary, err := provider.ObserveGitBoundary(ctx, run); err != nil || !strings.Contains(boundary, `"workspace_sha256"`) {
		return fmt.Errorf("agent-to-human Git boundary=%q error=%v", boundary, err)
	}
	humanCreated, err := provider.EnsureShellContainer(ctx, run, 2, taskenvdocker.ShellRoleHuman)
	if err != nil {
		return fmt.Errorf("create real human writer: %w", err)
	}
	humanStarted, err := provider.StartShellContainer(ctx, run, 2, taskenvdocker.ShellRoleHuman, humanCreated.ContainerID)
	if err != nil {
		return fmt.Errorf("start real human writer: %w", err)
	}
	humanInfo, err := cli.ContainerInspect(ctx, humanStarted.ContainerID)
	if err != nil {
		return err
	}
	if !humanInfo.Config.NetworkDisabled || humanInfo.HostConfig.NetworkMode != "none" || len(humanInfo.NetworkSettings.Networks) != 0 ||
		len(humanInfo.HostConfig.PortBindings) != 0 || !humanInfo.HostConfig.ReadonlyRootfs || humanInfo.Config.User != "1001:1001" ||
		len(humanInfo.Mounts) != 1 || !hasExactShellMounts(humanInfo.Mounts, clonePath, false) || len(humanInfo.HostConfig.Tmpfs) != 2 ||
		humanInfo.HostConfig.Tmpfs["/tmp"] != "rw,noexec,nosuid,nodev,size=67108864,mode=1777,uid=1001,gid=1001" ||
		humanInfo.HostConfig.Tmpfs["/home/user/.local/share/opencode"] != "rw,noexec,nosuid,nodev,size=67108864,mode=0700,uid=1001,gid=1001" ||
		slices.ContainsFunc(humanInfo.Config.Env, func(value string) bool {
			upper := strings.ToUpper(value)
			return strings.Contains(upper, "OPENCODE_SERVER_PASSWORD=") || strings.Contains(upper, "API_KEY=") || strings.Contains(upper, "TOKEN=")
		}) {
		return errors.New("real human shell network, credential, rootfs, or mount policy differs")
	}
	humanOutput, err := runShellCommand(ctx, provider, run, 2, taskenvdocker.ShellRoleHuman, humanStarted.RuntimeIdentity(),
		"printf 'human-owned\\n' > /home/user/workspace/human-output.txt; printf 'HUMAN_WRITE_OK\\n'; exit\n")
	if err != nil || !strings.Contains(humanOutput, "HUMAN_WRITE_OK") {
		return fmt.Errorf("human writer output=%q error=%v", humanOutput, err)
	}
	if contents, err := os.ReadFile(filepath.Join(clonePath, "human-output.txt")); err != nil || string(contents) != "human-owned\n" {
		return fmt.Errorf("human writer result=%q error=%v", contents, err)
	}
	if _, err := provider.StopShellContainer(ctx, run, 2, taskenvdocker.ShellRoleHuman, humanStarted.RuntimeIdentity()); err != nil {
		return fmt.Errorf("stop real human writer: %w", err)
	}
	if _, err := provider.RemoveShellContainer(ctx, run, 2, taskenvdocker.ShellRoleHuman, humanStarted.RuntimeIdentity()); err != nil {
		return fmt.Errorf("remove real human writer: %w", err)
	}
	if boundary, err := provider.ObserveGitBoundary(ctx, run); err != nil || !strings.Contains(boundary, `"workspace_sha256"`) {
		return fmt.Errorf("human-to-agent Git boundary=%q error=%v", boundary, err)
	}
	if _, err := provider.EnsureVolume(ctx, run); err != nil {
		return fmt.Errorf("create fresh handback volume: %w", err)
	}
	replacementCreated, err := provider.EnsureContainer(ctx, run)
	if err != nil {
		return fmt.Errorf("create fresh handback agent: %w", err)
	}
	replacementStarted, err := provider.StartContainer(ctx, run, replacementCreated.ContainerID)
	if err != nil {
		return fmt.Errorf("start fresh handback agent: %w", err)
	}
	if replacementStarted.ContainerID == runtime.ContainerID || replacementStarted.RuntimeToken == runtime.Token {
		return errors.New("handback reused the destroyed agent runtime epoch")
	}
	runtime, containerID = replacementStarted.RuntimeIdentity(), replacementCreated.ContainerID
	if _, err := provider.Health(ctx, run, runtime); err != nil {
		return fmt.Errorf("fresh handback agent health: %w", err)
	}
	if stopped, err := provider.StopContainer(ctx, run, runtime); err != nil || !strings.Contains(stopped.Evidence, "writer_inactive") {
		return fmt.Errorf("fresh handback agent stop evidence=%q error=%v", stopped.Evidence, err)
	}
	if _, err := provider.RemoveContainer(ctx, run, taskenvdocker.RuntimeCleanupAuthority(runtime)); err != nil {
		return err
	}
	containerID = ""
	if _, err := provider.RemoveVolume(ctx, run, taskenvdocker.RuntimeCleanupAuthority(runtime)); err != nil {
		return err
	}
	if _, err := provider.RemoveClone(ctx, run, taskenvdocker.RuntimeCleanupAuthority(runtime)); err != nil {
		return err
	}
	if _, err := cli.ContainerInspect(ctx, run.ContainerIdentity); !client.IsErrNotFound(err) {
		return errors.New("container remains after cleanup")
	}
	if _, err := cli.VolumeInspect(ctx, run.VolumeIdentity); !client.IsErrNotFound(err) {
		return errors.New("volume remains after cleanup")
	}
	if _, err := os.Stat(clonePath); !errors.Is(err, os.ErrNotExist) {
		return errors.New("clone remains after cleanup")
	}
	fmt.Printf("PASS image_id=%s container_id=%s endpoint=%s key_mode=0600 clone_isolated=true auth=missing_wrong_correct reconstruction=true restart_fenced=true inspector=readonly_networkless cold_takeover=human_writable_networkless handback=fresh_agent cleanup=complete\n", imageID, created.ContainerID, started.Endpoint)
	return nil
}

func runShellCommand(ctx context.Context, provider *taskenvdocker.Provider, run taskstore.BackgroundRun, writerGeneration int64, role string, runtime taskenvdocker.RuntimeIdentity, command string) (string, error) {
	terminal, err := provider.AttachShell(ctx, run, writerGeneration, role, runtime)
	if err != nil {
		return "", err
	}
	defer terminal.Close()
	if _, err := terminal.Write([]byte(command)); err != nil {
		return "", err
	}
	output, err := io.ReadAll(io.LimitReader(terminal, 1<<20))
	return string(output), err
}

func hasExactShellMounts(mounts []container.MountPoint, clonePath string, readOnly bool) bool {
	if len(mounts) != 1 {
		return false
	}
	workspace := false
	for _, item := range mounts {
		switch item.Destination {
		case "/home/user/workspace":
			workspace = item.Type == mount.TypeBind && item.Source == clonePath && item.RW != readOnly
		}
	}
	return workspace
}

func canonicalImageID(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func git(directory, binary string, args ...string) error {
	command := exec.Command(binary, args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "LC_ALL=C")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, output)
	}
	return nil
}
func gitOutput(directory, binary string, args ...string) (string, error) {
	command := exec.Command(binary, args...)
	command.Dir = directory
	output, err := command.Output()
	return strings.TrimSpace(string(output)), err
}

func ensureNoSharedFiles(source, clone string) error {
	var sourceFiles []os.FileInfo
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			sourceFiles = append(sourceFiles, info)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return filepath.WalkDir(clone, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		for _, sourceInfo := range sourceFiles {
			if os.SameFile(sourceInfo, info) {
				return fmt.Errorf("clone file %s is hard-linked to the trusted repository", path)
			}
		}
		return nil
	})
}

func cleanupHarness(provider *taskenvdocker.Provider, cli *client.Client, run taskstore.BackgroundRun, containerID string, runtime taskenvdocker.RuntimeIdentity, clonePath string) error {
	var cleanupErr error
	call := func(name string, operation func(context.Context) error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := operation(ctx)
		cancel()
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup %s: %w", name, err))
		}
	}
	if runtime.ContainerID != "" {
		call("provider stop", func(ctx context.Context) error {
			_, err := provider.StopContainer(ctx, run, runtime)
			return err
		})
		call("provider container remove", func(ctx context.Context) error {
			_, err := provider.RemoveContainer(ctx, run, taskenvdocker.RuntimeCleanupAuthority(runtime))
			return err
		})
	}
	authority := taskenvdocker.NeverCreatedAuthority()
	if runtime.ContainerID != "" {
		authority = taskenvdocker.RuntimeCleanupAuthority(runtime)
	} else if containerID != "" {
		authority = taskenvdocker.CreatedContainerAuthority(containerID)
	}
	call("raw exact-name container fallback", func(context.Context) error {
		return rawRemoveContainer(cli, run.ContainerIdentity)
	})
	call("provider volume remove", func(ctx context.Context) error {
		_, err := provider.RemoveVolume(ctx, run, authority)
		return err
	})
	call("raw exact-name volume fallback", func(ctx context.Context) error {
		return rawRemoveVolume(cli, run.VolumeIdentity)
	})
	call("provider clone remove", func(ctx context.Context) error {
		_, err := provider.RemoveClone(ctx, run, authority)
		return err
	})
	call("raw exact-path clone fallback", func(context.Context) error { return os.RemoveAll(clonePath) })
	call("residue assertion", func(context.Context) error {
		var residue error
		containerCtx, containerCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, containerErr := cli.ContainerInspect(containerCtx, run.ContainerIdentity)
		containerCancel()
		if !client.IsErrNotFound(containerErr) {
			residue = errors.Join(residue, fmt.Errorf("container residue: %v", containerErr))
		}
		volumeCtx, volumeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, volumeErr := cli.VolumeInspect(volumeCtx, run.VolumeIdentity)
		volumeCancel()
		if !client.IsErrNotFound(volumeErr) {
			residue = errors.Join(residue, fmt.Errorf("volume residue: %v", volumeErr))
		}
		if _, err := os.Lstat(clonePath); !errors.Is(err, os.ErrNotExist) {
			residue = errors.Join(residue, fmt.Errorf("clone residue: %v", err))
		}
		return residue
	})
	return cleanupErr
}

func rawRemoveContainer(cli *client.Client, exactName string) error {
	var attempts error
	for attempt := 0; attempt < 2; attempt++ {
		inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 10*time.Second)
		info, err := cli.ContainerInspect(inspectCtx, exactName)
		inspectCancel()
		if client.IsErrNotFound(err) {
			return nil
		}
		if err != nil {
			attempts = errors.Join(attempts, fmt.Errorf("attempt %d inspect: %w", attempt+1, err))
			continue
		}
		if info.State != nil && info.State.Running {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
			seconds := 3
			stopErr := cli.ContainerStop(stopCtx, info.ID, container.StopOptions{Timeout: &seconds})
			stopCancel()
			if stopErr != nil && !client.IsErrNotFound(stopErr) {
				attempts = errors.Join(attempts, fmt.Errorf("attempt %d ambiguous stop: %w", attempt+1, stopErr))
			}
		}
		// Force removal is still attempted when graceful stop was ambiguous.
		removeCtx, removeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		removeErr := cli.ContainerRemove(removeCtx, info.ID, container.RemoveOptions{Force: true})
		removeCancel()
		if removeErr != nil && !client.IsErrNotFound(removeErr) {
			attempts = errors.Join(attempts, fmt.Errorf("attempt %d force remove: %w", attempt+1, removeErr))
		}
		nameCtx, nameCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, nameErr := cli.ContainerInspect(nameCtx, exactName)
		nameCancel()
		idCtx, idCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, idErr := cli.ContainerInspect(idCtx, info.ID)
		idCancel()
		if client.IsErrNotFound(nameErr) && client.IsErrNotFound(idErr) {
			return nil
		}
		attempts = errors.Join(attempts, fmt.Errorf("attempt %d residue: name=%v id=%v", attempt+1, nameErr, idErr))
	}
	return attempts
}

func rawRemoveVolume(cli *client.Client, exactName string) error {
	var attempts error
	for attempt := 0; attempt < 2; attempt++ {
		removeCtx, removeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		removeErr := cli.VolumeRemove(removeCtx, exactName, true)
		removeCancel()
		if removeErr != nil && !client.IsErrNotFound(removeErr) {
			attempts = errors.Join(attempts, fmt.Errorf("attempt %d remove: %w", attempt+1, removeErr))
		}
		inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, inspectErr := cli.VolumeInspect(inspectCtx, exactName)
		inspectCancel()
		if client.IsErrNotFound(inspectErr) {
			return nil
		}
		attempts = errors.Join(attempts, fmt.Errorf("attempt %d residue: %v", attempt+1, inspectErr))
	}
	return attempts
}
