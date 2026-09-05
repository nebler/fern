package backgroundruncoord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskstore"
)

var ErrInterventionUnavailable = errors.New("background run intervention is unavailable")

type terminalSession struct {
	terminal *taskenvdocker.Terminal
	done     chan struct{}
	once     sync.Once
}

type InterventionStatus struct {
	State       string
	Active      bool
	Questions   int
	Permissions int
}

func (c *Coordinator) ObserveIntervention(ctx context.Context, run taskstore.BackgroundRun, ownership taskstore.BackgroundRunOwnership) (InterventionStatus, error) {
	current, err := c.currentAgentOwnership(ctx, run, ownership.Revision)
	if err != nil {
		return InterventionStatus{}, err
	}
	effective := effectiveRun(run, current)
	client, err := c.client(ctx, effective)
	if err != nil {
		return InterventionStatus{State: "uncertain"}, err
	}
	observation, err := client.ObservePending(ctx, string(effective.OpenCodeSessionID))
	if err != nil {
		return InterventionStatus{State: "uncertain"}, err
	}
	state := "local_idle"
	if observation.Active {
		state = "working"
	}
	if observation.Questions > 0 || observation.Permissions > 0 {
		state = "needs_you"
	}
	return InterventionStatus{State: state, Active: observation.Active, Questions: observation.Questions, Permissions: observation.Permissions}, nil
}

func (c *Coordinator) OpenTerminal(ctx context.Context, run taskstore.BackgroundRun, ownership taskstore.BackgroundRunOwnership, role string) (*taskenvdocker.Terminal, func(), error) {
	c.scan.Lock()
	defer c.scan.Unlock()
	current, err := c.store.GetBackgroundRunOwnership(ctx, run.WorkspaceID, run.TaskID)
	if err != nil || current.Revision != ownership.Revision {
		return nil, nil, errors.Join(ErrInterventionUnavailable, err)
	}
	c.terminalMu.Lock()
	if c.terminals[run.TaskID] != nil {
		c.terminalMu.Unlock()
		return nil, nil, errors.New("a terminal is already attached to this run")
	}
	c.terminalMu.Unlock()
	var runtime taskenvdocker.RuntimeIdentity
	if role == taskenvdocker.ShellRoleHuman {
		if current.Mode != taskstore.BackgroundRunHumanOwned || current.Phase != taskstore.BackgroundRunOwnershipHumanActive {
			return nil, nil, ErrInterventionUnavailable
		}
		runtime = taskenvdocker.RuntimeIdentity{ContainerID: current.ContainerID, StartedAt: current.ContainerStartedAt, Token: current.RuntimeToken}
	} else if role == taskenvdocker.ShellRoleInspector {
		current, err = c.currentAgentOwnership(ctx, run, ownership.Revision)
		if err != nil {
			return nil, nil, err
		}
		effective := effectiveRun(run, current)
		agentRuntime, err := c.provider.CommittedRuntime(effective)
		if err != nil {
			return nil, nil, err
		}
		if _, err := c.provider.Health(ctx, effective, agentRuntime); err != nil {
			return nil, nil, err
		}
		created, err := c.provider.EnsureShellContainer(ctx, run, current.WriterGeneration, taskenvdocker.ShellRoleInspector)
		if err != nil {
			return nil, nil, err
		}
		started, err := c.provider.StartShellContainer(ctx, run, current.WriterGeneration, taskenvdocker.ShellRoleInspector, created.ContainerID)
		if err != nil {
			return nil, nil, err
		}
		runtime = started.RuntimeIdentity()
	} else {
		return nil, nil, ErrInterventionUnavailable
	}
	terminal, err := c.provider.AttachShell(ctx, run, current.WriterGeneration, role, runtime)
	if err != nil {
		if role == taskenvdocker.ShellRoleInspector {
			cleanup, cancel := context.WithTimeout(context.Background(), c.config.OperationTimeout)
			_, cleanupErr := c.provider.StopShellContainer(cleanup, run, current.WriterGeneration, role, runtime)
			_, removeErr := c.provider.RemoveShellContainer(cleanup, run, current.WriterGeneration, role, runtime)
			cancel()
			err = errors.Join(err, cleanupErr, removeErr)
		}
		return nil, nil, err
	}
	c.terminalMu.Lock()
	if c.terminals[run.TaskID] != nil {
		c.terminalMu.Unlock()
		_ = terminal.Close()
		return nil, nil, errors.New("a terminal is already attached to this run")
	}
	session := &terminalSession{terminal: terminal, done: make(chan struct{})}
	c.terminals[run.TaskID] = session
	c.terminalMu.Unlock()
	release := func() {
		session.once.Do(func() {
			c.terminalMu.Lock()
			if c.terminals[run.TaskID] == session {
				delete(c.terminals, run.TaskID)
			}
			c.terminalMu.Unlock()
			_ = terminal.Close()
			if role == taskenvdocker.ShellRoleInspector {
				cleanup, cancel := context.WithTimeout(context.Background(), c.config.OperationTimeout+c.config.LeaseDuration/2)
				_, _ = c.provider.StopShellContainer(cleanup, run, current.WriterGeneration, role, runtime)
				_, _ = c.provider.RemoveShellContainer(cleanup, run, current.WriterGeneration, role, runtime)
				cancel()
			}
			close(session.done)
		})
	}
	return terminal, release, nil
}

func (c *Coordinator) currentAgentOwnership(ctx context.Context, run taskstore.BackgroundRun, expectedRevision int64) (taskstore.BackgroundRunOwnership, error) {
	current, err := c.store.GetBackgroundRunOwnership(ctx, run.WorkspaceID, run.TaskID)
	if err != nil || current.Revision != expectedRevision || current.Mode != taskstore.BackgroundRunAgentOwned ||
		current.Phase != taskstore.BackgroundRunOwnershipAgentActive || run.CancelEpoch != 0 || run.EffectPhase != taskstore.BackgroundRunEffectPromptAdmitted {
		return taskstore.BackgroundRunOwnership{}, errors.Join(ErrInterventionUnavailable, err)
	}
	if current.ContainerID == "" {
		digest := sha256.Sum256([]byte(run.ObservedContainerID + "\x00" + run.ObservedContainerStartedAt))
		current.ContainerIdentity, current.ContainerID = run.ContainerIdentity, run.ObservedContainerID
		current.ContainerStartedAt, current.RuntimeEpoch, current.RuntimeToken = run.ObservedContainerStartedAt, run.RuntimeEpoch, hex.EncodeToString(digest[:])
		current.VolumeIdentity, current.EndpointIdentity, current.HostPort = run.VolumeIdentity, run.EndpointIdentity, run.HostPort
		current.OpenCodeSessionID, current.OpenCodeMessageID = run.OpenCodeSessionID, run.OpenCodeMessageID
	}
	if current.ContainerID == "" || current.RuntimeEpoch <= 0 {
		return taskstore.BackgroundRunOwnership{}, ErrInterventionUnavailable
	}
	return current, nil
}

func (c *Coordinator) drainTerminal(ctx context.Context, taskID task.TaskID) error {
	c.terminalMu.Lock()
	session := c.terminals[taskID]
	c.terminalMu.Unlock()
	if session == nil {
		return nil
	}
	closeErr := session.terminal.Close()
	select {
	case <-session.done:
		return closeErr
	case <-ctx.Done():
		return errors.Join(closeErr, ctx.Err())
	}
}

func (c *Coordinator) ResizeTerminal(ctx context.Context, runtime taskenvdocker.RuntimeIdentity, rows, columns uint) error {
	operation, cancel := context.WithTimeout(ctx, min(c.config.OperationTimeout, 10*time.Second))
	defer cancel()
	return c.provider.ResizeShell(operation, runtime, rows, columns)
}

func (c *Coordinator) TerminalRuntime(ownership taskstore.BackgroundRunOwnership) (taskenvdocker.RuntimeIdentity, error) {
	runtime := taskenvdocker.RuntimeIdentity{ContainerID: ownership.ContainerID, StartedAt: ownership.ContainerStartedAt, Token: ownership.RuntimeToken}
	if runtime.ContainerID == "" || runtime.StartedAt == "" || runtime.Token == "" {
		return taskenvdocker.RuntimeIdentity{}, fmt.Errorf("%w: terminal runtime is incomplete", ErrInterventionUnavailable)
	}
	return runtime, nil
}
