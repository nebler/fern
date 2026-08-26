package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// ExecWorkspaceGH executes the pinned gh binary in one attested managed
// workspace. Callers must supply a deadline and retain their manager request
// lease until this method returns.
func (d *Docker) ExecWorkspaceGH(ctx context.Context, workspace, expectedImageID string, arguments ...string) (CommandResult, error) {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Minute {
		return CommandResult{}, fmt.Errorf("%w: caller deadline is missing, expired, or beyond the 5 minute execution budget", ErrCommandFailed)
	}
	if !ValidImageID(expectedImageID) {
		return CommandResult{}, fmt.Errorf("%w: expected image ID is not a canonical Docker image ID", ErrCommandFailed)
	}
	if len(arguments) == 0 || len(arguments) > 64 {
		return CommandResult{}, fmt.Errorf("%w: gh requires between 1 and 64 arguments", ErrCommandFailed)
	}
	for _, argument := range arguments {
		if len(argument) > 64<<10 || strings.ContainsRune(argument, 0) {
			return CommandResult{}, fmt.Errorf("%w: gh argument exceeds 64 KiB or contains NUL", ErrCommandFailed)
		}
	}
	inspection, err := d.inspectByReference(ctx, workspace, workspace)
	if err != nil {
		return CommandResult{}, errors.Join(ErrCommandFailed, fmt.Errorf("inspect workspace %q: %w", workspace, err))
	}
	if inspection.observation.State != StateRunning || inspection.observation.ImageID != expectedImageID || inspection.observation.Frozen {
		return CommandResult{}, fmt.Errorf("%w: workspace %q is %s with image %s", ErrCommandFailed, workspace, inspection.observation.State, inspection.observation.ImageID)
	}
	seconds := int(time.Until(deadline) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	command := []string{"/usr/bin/timeout", "--signal=KILL", "--kill-after=1s", strconv.Itoa(seconds) + "s", workspaceGHBinary}
	command = append(command, arguments...)
	created, err := d.cli.ContainerExecCreate(ctx, inspection.observation.ContainerID, container.ExecOptions{
		User: workspaceUser, AttachStdout: true, AttachStderr: true, WorkingDir: repoMountTarget, Cmd: command,
	})
	if err != nil {
		return CommandResult{}, errors.Join(ErrCommandFailed, fmt.Errorf("create gh exec: %w", err))
	}
	if created.ID == "" {
		return CommandResult{}, fmt.Errorf("%w: exec create returned no id", ErrCommandFailed)
	}
	attached, err := d.cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return CommandResult{}, errors.Join(ErrCommandFailed, fmt.Errorf("attach gh exec: %w", err))
	}
	defer attached.Close()
	stdout, stderr := &boundedCommandBuffer{limit: 64 << 10}, &boundedCommandBuffer{limit: 64 << 10}
	_, copyErr := stdcopy.StdCopy(stdout, stderr, attached.Reader)
	inspectContext, cancelInspect := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelInspect()
	executed, inspectErr := d.cli.ContainerExecInspect(inspectContext, created.ID)
	result := CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: executed.ExitCode}
	if stdout.exceeded || stderr.exceeded {
		return result, ErrCommandOutputLimit
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if copyErr != nil || inspectErr != nil || executed.Running || executed.ExitCode != 0 {
		return result, errors.Join(ErrCommandFailed, fmt.Errorf("gh exec outcome: copy=%v inspect=%v running=%t exit=%d", copyErr, inspectErr, executed.Running, executed.ExitCode))
	}
	return result, nil
}

type boundedCommandBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedCommandBuffer) Write(value []byte) (int, error) {
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		retained := value
		if len(retained) > remaining {
			retained = retained[:remaining]
		}
		_, _ = b.buffer.Write(retained)
	}
	if len(value) > remaining {
		b.exceeded = true
	}
	return len(value), nil
}

func (b *boundedCommandBuffer) Bytes() []byte { return append([]byte(nil), b.buffer.Bytes()...) }
