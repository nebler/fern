package backgroundruncoord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nebler/fern/internal/backgroundopencode"
	"github.com/nebler/fern/internal/backgroundroute"
	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskstore"
)

const handbackPreamble = "A human had exclusive control of this workspace. Re-read the repository and all current changes before continuing. Do not assume the previous process or session state survived.\n\nOriginal task:\n"

func ownershipClaim(value taskstore.BackgroundRunOwnership, now time.Time) taskstore.BackgroundRunOwnershipClaim {
	return taskstore.BackgroundRunOwnershipClaim{
		WorkspaceID: value.WorkspaceID, TaskID: value.TaskID, AttemptID: value.AttemptID, RunGeneration: value.RunGeneration,
		ExpectedRevision: value.Revision, ExpectedMode: value.Mode, ExpectedPhase: value.Phase,
		ClaimOwner: value.ClaimOwner, ClaimGeneration: value.ClaimGeneration, Now: now,
	}
}

func effectiveRun(run taskstore.BackgroundRun, ownership taskstore.BackgroundRunOwnership) taskstore.BackgroundRun {
	run.WriterGeneration = ownership.WriterGeneration
	if ownership.ContainerID != "" && ownership.ContainerIdentity == run.ContainerIdentity {
		run.ObservedContainerID = ownership.ContainerID
		run.ObservedContainerStartedAt = ownership.ContainerStartedAt
		run.RuntimeEpoch = ownership.RuntimeEpoch
		run.HostPort = ownership.HostPort
	}
	return run
}

func (c *Coordinator) processOwnership(ctx, parent context.Context, work taskstore.BackgroundRunOwnershipWork) error {
	ownership := work.Ownership
	run := effectiveRun(work.Run, ownership)
	runtime := taskenvdocker.RuntimeIdentity{ContainerID: ownership.ContainerID, StartedAt: ownership.ContainerStartedAt, Token: ownership.RuntimeToken}
	switch ownership.Phase {
	case taskstore.BackgroundRunOwnershipAgentRouteRemoval:
		if err := c.drainTerminal(ctx, run.TaskID); err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		if _, err := c.provider.RemoveInspector(ctx, work.Run, ownership.WriterGeneration); err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		identity := backgroundroute.Identity{WorkspaceID: string(run.WorkspaceID), TaskID: string(run.TaskID), AttemptID: string(run.AttemptID),
			Generation: run.Generation, WriterGeneration: ownership.WriterGeneration, Role: "agent", RuntimeEpoch: ownership.RuntimeEpoch,
			ContainerID: ownership.ContainerID, StartedAt: ownership.ContainerStartedAt, RuntimeToken: ownership.RuntimeToken}
		evidence, err := c.config.Route.Remove(ctx, identity)
		if err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		return c.advanceOwnership(parent, work, taskstore.BackgroundRunTakeoverRequested, taskstore.BackgroundRunOwnershipAgentStop, func(p *taskstore.AdvanceBackgroundRunOwnershipParams) {
			p.RouteEvidence = evidence
		})
	case taskstore.BackgroundRunOwnershipAgentStop:
		identity := backgroundroute.Identity{WorkspaceID: string(run.WorkspaceID), TaskID: string(run.TaskID), AttemptID: string(run.AttemptID),
			Generation: run.Generation, WriterGeneration: ownership.WriterGeneration, Role: "agent", RuntimeEpoch: ownership.RuntimeEpoch,
			ContainerID: ownership.ContainerID, StartedAt: ownership.ContainerStartedAt, RuntimeToken: ownership.RuntimeToken}
		if err := c.config.Route.ConfirmRemoval(identity); err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		observation, err := c.provider.StopContainer(ctx, run, runtime)
		if err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		return c.advanceOwnership(parent, work, taskstore.BackgroundRunTakeoverRequested, taskstore.BackgroundRunOwnershipAgentRemove, func(p *taskstore.AdvanceBackgroundRunOwnershipParams) {
			p.WriterEvidence = observation.Evidence
		})
	case taskstore.BackgroundRunOwnershipAgentRemove:
		observation, err := c.provider.RemoveContainer(ctx, run, taskenvdocker.RuntimeCleanupAuthority(runtime))
		if err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		return c.advanceOwnership(parent, work, taskstore.BackgroundRunTakeoverRequested, taskstore.BackgroundRunOwnershipAgentVolumeRemove, func(p *taskstore.AdvanceBackgroundRunOwnershipParams) {
			p.ResourceEvidence = observation.Evidence
		})
	case taskstore.BackgroundRunOwnershipAgentVolumeRemove:
		observation, err := c.provider.RemoveVolume(ctx, run, taskenvdocker.RuntimeCleanupAuthority(runtime))
		if err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		gitEvidence, err := c.provider.ObserveGitBoundary(ctx, work.Run)
		if err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		return c.advanceOwnership(parent, work, taskstore.BackgroundRunTakeoverRequested, taskstore.BackgroundRunOwnershipHumanCreate, func(p *taskstore.AdvanceBackgroundRunOwnershipParams) {
			p.ResourceEvidence = observation.Evidence
			p.GitEvidence = gitEvidence
		})
	case taskstore.BackgroundRunOwnershipHumanCreate:
		if _, err := c.provider.EnsureShellContainer(ctx, work.Run, ownership.TargetWriterGeneration, taskenvdocker.ShellRoleHuman); err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		return c.advanceOwnership(parent, work, taskstore.BackgroundRunTakeoverRequested, taskstore.BackgroundRunOwnershipHumanStart, nil)
	case taskstore.BackgroundRunOwnershipHumanStart:
		created, err := c.provider.EnsureShellContainer(ctx, work.Run, ownership.TargetWriterGeneration, taskenvdocker.ShellRoleHuman)
		if err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		started, err := c.provider.StartShellContainer(ctx, work.Run, ownership.TargetWriterGeneration, taskenvdocker.ShellRoleHuman, created.ContainerID)
		if err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		return c.advanceOwnership(parent, work, taskstore.BackgroundRunHumanOwned, taskstore.BackgroundRunOwnershipHumanActive, func(p *taskstore.AdvanceBackgroundRunOwnershipParams) {
			p.WriterGeneration = ownership.TargetWriterGeneration
			p.ContainerIdentity = ownership.TargetContainerIdentity
			p.ContainerID, p.ContainerStartedAt, p.RuntimeEpoch, p.RuntimeToken = started.ContainerID, started.ContainerStarted, started.RuntimeEpoch, started.RuntimeToken
			p.VolumeIdentity, p.EndpointIdentity, p.HostPort, p.OpenCodeSessionID, p.OpenCodeMessageID = "", "", 0, "", ""
			p.WriterEvidence = started.Evidence
		})
	case taskstore.BackgroundRunOwnershipHumanRouteRemoval:
		if err := c.drainTerminal(ctx, run.TaskID); err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		if c.config.DrainTerminal != nil {
			if err := c.config.DrainTerminal(ctx, run.TaskID); err != nil {
				return c.ownershipFailure(parent, work, err)
			}
		}
		return c.advanceOwnership(parent, work, taskstore.BackgroundRunHandbackRequested, taskstore.BackgroundRunOwnershipHumanStop, func(p *taskstore.AdvanceBackgroundRunOwnershipParams) {
			p.RouteEvidence = `{"effect":"terminal_route","status":"drained"}`
		})
	case taskstore.BackgroundRunOwnershipHumanStop:
		observation, err := c.provider.StopShellContainer(ctx, work.Run, ownership.WriterGeneration, taskenvdocker.ShellRoleHuman, runtime)
		if err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		return c.advanceOwnership(parent, work, taskstore.BackgroundRunHandbackRequested, taskstore.BackgroundRunOwnershipHumanRemove, func(p *taskstore.AdvanceBackgroundRunOwnershipParams) {
			p.WriterEvidence = observation.Evidence
		})
	case taskstore.BackgroundRunOwnershipHumanRemove:
		observation, err := c.provider.RemoveShellContainer(ctx, work.Run, ownership.WriterGeneration, taskenvdocker.ShellRoleHuman, runtime)
		if err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		gitEvidence, err := c.provider.ObserveGitBoundary(ctx, work.Run)
		if err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		return c.advanceOwnership(parent, work, taskstore.BackgroundRunHandbackRequested, taskstore.BackgroundRunOwnershipAgentVolumeCreate, func(p *taskstore.AdvanceBackgroundRunOwnershipParams) {
			p.ResourceEvidence = observation.Evidence
			p.GitEvidence = gitEvidence
		})
	case taskstore.BackgroundRunOwnershipAgentVolumeCreate:
		observation, err := c.provider.EnsureVolume(ctx, work.Run)
		if err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		return c.advanceOwnership(parent, work, taskstore.BackgroundRunHandbackRequested, taskstore.BackgroundRunOwnershipAgentCreate, func(p *taskstore.AdvanceBackgroundRunOwnershipParams) {
			p.ResourceEvidence = observation.Evidence
		})
	case taskstore.BackgroundRunOwnershipAgentCreate:
		if _, err := c.provider.EnsureContainer(ctx, work.Run); err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		return c.advanceOwnership(parent, work, taskstore.BackgroundRunHandbackRequested, taskstore.BackgroundRunOwnershipAgentStart, nil)
	case taskstore.BackgroundRunOwnershipAgentStart:
		created, err := c.provider.EnsureContainer(ctx, work.Run)
		if err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		started, err := c.provider.StartContainer(ctx, work.Run, created.ContainerID)
		if err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		return c.advanceOwnership(parent, work, taskstore.BackgroundRunHandbackRequested, taskstore.BackgroundRunOwnershipAgentHealth, func(p *taskstore.AdvanceBackgroundRunOwnershipParams) {
			p.WriterGeneration = ownership.TargetWriterGeneration
			p.ContainerIdentity, p.VolumeIdentity, p.EndpointIdentity = work.Run.ContainerIdentity, work.Run.VolumeIdentity, work.Run.EndpointIdentity
			p.ContainerID, p.ContainerStartedAt, p.RuntimeEpoch, p.RuntimeToken, p.HostPort = started.ContainerID, started.ContainerStarted, started.RuntimeEpoch, started.RuntimeToken, started.HostPort
			p.OpenCodeSessionID, p.OpenCodeMessageID = work.Run.OpenCodeSessionID, work.Run.OpenCodeMessageID
			p.WriterEvidence = started.Evidence
		})
	case taskstore.BackgroundRunOwnershipAgentHealth:
		effective := effectiveRun(work.Run, ownership)
		observation, err := c.provider.Health(ctx, effective, runtime)
		if err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		return c.advanceOwnership(parent, work, taskstore.BackgroundRunHandbackRequested, taskstore.BackgroundRunOwnershipAgentSession, func(p *taskstore.AdvanceBackgroundRunOwnershipParams) {
			p.ResourceEvidence = observation.Evidence
		})
	case taskstore.BackgroundRunOwnershipAgentSession:
		effective := effectiveRun(work.Run, ownership)
		client, err := c.provider.OpenCodeClient(effective, runtime, c.config.HTTPClient)
		if err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		spec := backgroundopencode.SessionSpec{ID: string(effective.OpenCodeSessionID), Agent: c.config.Agent,
			ProviderID: c.config.ModelProvider, ModelID: c.config.Model, Directory: sessionDirectory}
		state, err := client.ReconcileSession(ctx, spec)
		if err == nil && state == backgroundopencode.ReconcileAbsent {
			createErr := client.CreateSessionOnce(ctx, spec)
			state, err = client.ReconcileSession(ctx, spec)
			if err == nil && state == backgroundopencode.ReconcileAbsent {
				err = createErr
			}
		}
		if err != nil || state != backgroundopencode.ReconcileExact {
			return c.ownershipFailure(parent, work, errors.Join(err, fmt.Errorf("handback session state %s", state)))
		}
		return c.advanceOwnership(parent, work, taskstore.BackgroundRunHandbackRequested, taskstore.BackgroundRunOwnershipAgentPrompt, nil)
	case taskstore.BackgroundRunOwnershipAgentPrompt:
		effective := effectiveRun(work.Run, ownership)
		client, err := c.provider.OpenCodeClient(effective, runtime, c.config.HTTPClient)
		if err != nil {
			return c.ownershipFailure(parent, work, err)
		}
		spec := backgroundopencode.PromptSpec{ID: string(effective.OpenCodeMessageID), Text: handbackPreamble + work.Prompt, Delivery: "steer", Resume: true}
		state, err := client.ReconcilePrompt(ctx, string(effective.OpenCodeSessionID), spec, c.config.HistoryBounds)
		if err == nil && state == backgroundopencode.ReconcileAbsent {
			callErr := client.AdmitPromptOnce(ctx, string(effective.OpenCodeSessionID), spec)
			state, err = client.ReconcilePrompt(ctx, string(effective.OpenCodeSessionID), spec, c.config.HistoryBounds)
			if err == nil && state == backgroundopencode.ReconcileAbsent {
				err = callErr
			}
		}
		if err != nil || state != backgroundopencode.ReconcileExact {
			return c.ownershipFailure(parent, work, errors.Join(err, fmt.Errorf("handback prompt state %s", state)))
		}
		return c.advanceOwnership(parent, work, taskstore.BackgroundRunAgentOwned, taskstore.BackgroundRunOwnershipAgentActive, nil)
	default:
		return c.ownershipFailure(parent, work, errors.New("unsupported ownership phase"))
	}
}

func (c *Coordinator) advanceOwnership(ctx context.Context, work taskstore.BackgroundRunOwnershipWork, mode taskstore.BackgroundRunOwnershipMode,
	phase taskstore.BackgroundRunOwnershipPhase, mutate func(*taskstore.AdvanceBackgroundRunOwnershipParams)) error {
	now, err := c.freshNow()
	if err != nil {
		return err
	}
	o := work.Ownership
	p := taskstore.AdvanceBackgroundRunOwnershipParams{
		BackgroundRunOwnershipClaim: ownershipClaim(o, now), Mode: mode, Phase: phase, WriterGeneration: o.WriterGeneration,
		ContainerIdentity: o.ContainerIdentity, ContainerID: o.ContainerID, ContainerStartedAt: o.ContainerStartedAt,
		RuntimeEpoch: o.RuntimeEpoch, RuntimeToken: o.RuntimeToken, VolumeIdentity: o.VolumeIdentity, EndpointIdentity: o.EndpointIdentity,
		HostPort: o.HostPort, OpenCodeSessionID: o.OpenCodeSessionID, OpenCodeMessageID: o.OpenCodeMessageID,
	}
	if mutate != nil {
		mutate(&p)
	}
	_, err = c.store.AdvanceBackgroundRunOwnership(ctx, p)
	return err
}

func (c *Coordinator) ownershipFailure(ctx context.Context, work taskstore.BackgroundRunOwnershipWork, external error) error {
	if !errors.Is(external, taskenvdocker.ErrIdentityMismatch) && !errors.Is(external, taskenvdocker.ErrQuarantined) &&
		!errors.Is(external, backgroundroute.ErrMismatch) {
		return external
	}
	err := c.advanceOwnership(ctx, work, taskstore.BackgroundRunOwnershipUncertain, taskstore.BackgroundRunOwnershipUncertainPhase, func(p *taskstore.AdvanceBackgroundRunOwnershipParams) {
		p.LastError = external.Error()
	})
	return errors.Join(external, err)
}
