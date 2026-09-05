package backgroundruncoord

import (
	"context"
	"errors"
	"time"

	"github.com/nebler/fern/internal/backgroundopencode"
	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskstore"
)

func controlClaim(value taskstore.BackgroundRunControl, now time.Time) taskstore.BackgroundRunControlClaim {
	return taskstore.BackgroundRunControlClaim{WorkspaceID: value.WorkspaceID, ReceiptID: value.ReceiptID, ExpectedRevision: value.Revision,
		ExpectedState: value.State, ClaimOwner: value.ClaimOwner, ClaimGeneration: value.ClaimGeneration, Now: now}
}

func (c *Coordinator) processControl(ctx, parent context.Context, work taskstore.BackgroundRunControlWork) error {
	control := work.Control
	recoveredAttempt := control.State == taskstore.BackgroundRunControlAttempted
	if control.State == taskstore.BackgroundRunControlRequested {
		now, err := c.freshNow()
		if err != nil {
			return err
		}
		control, err = c.store.MarkBackgroundRunControlAttempted(parent, controlClaim(control, now))
		if err != nil {
			return err
		}
	}
	if work.Ownership.Mode != taskstore.BackgroundRunAgentOwned || work.Ownership.WriterGeneration != control.WriterGeneration ||
		work.Run.CancelEpoch != 0 || work.Run.EffectPhase != taskstore.BackgroundRunEffectPromptAdmitted {
		return c.completeControl(parent, control, taskstore.BackgroundRunControlConflict, "writer ownership changed before dispatch")
	}
	if control.CommandKind == taskstore.InterruptBackgroundRunCommand && recoveredAttempt {
		return c.completeControl(parent, control, taskstore.BackgroundRunControlUncertain, "interrupt outcome is uncertain after coordinator recovery")
	}
	run := work.Run
	run.WriterGeneration = control.WriterGeneration
	run.ObservedContainerID, run.ObservedContainerStartedAt, run.RuntimeEpoch = control.ContainerID, control.ContainerStartedAt, control.RuntimeEpoch
	if work.Ownership.HostPort > 0 {
		run.HostPort = work.Ownership.HostPort
	}
	runtime := taskenvdocker.RuntimeIdentity{ContainerID: control.ContainerID, StartedAt: control.ContainerStartedAt, Token: control.RuntimeToken}
	client, err := c.provider.OpenCodeClient(run, runtime, c.config.HTTPClient)
	if err != nil {
		return c.completeControl(parent, control, taskstore.BackgroundRunControlConflict, err.Error())
	}
	if _, err := c.provider.Health(ctx, run, runtime); err != nil {
		return c.completeControl(parent, control, taskstore.BackgroundRunControlUncertain, err.Error())
	}
	if control.CommandKind == taskstore.InterruptBackgroundRunCommand {
		if err := client.InterruptOnce(ctx, string(control.OpenCodeSessionID)); err != nil {
			state := taskstore.BackgroundRunControlConflict
			if errors.Is(err, backgroundopencode.ErrTransport) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				state = taskstore.BackgroundRunControlUncertain
			}
			return errors.Join(err, c.completeControl(parent, control, state, err.Error()))
		}
		return c.completeControl(parent, control, taskstore.BackgroundRunControlSucceeded, "")
	}
	spec := backgroundopencode.PromptSpec{ID: string(control.OpenCodeMessageID), Text: control.Instruction, Delivery: "steer", Resume: true}
	state, reconcileErr := client.ReconcilePrompt(ctx, string(control.OpenCodeSessionID), spec, c.config.HistoryBounds)
	if reconcileErr == nil && state == backgroundopencode.ReconcileAbsent {
		callErr := client.AdmitPromptOnce(ctx, string(control.OpenCodeSessionID), spec)
		state, reconcileErr = client.ReconcilePrompt(ctx, string(control.OpenCodeSessionID), spec, c.config.HistoryBounds)
		if reconcileErr == nil && state == backgroundopencode.ReconcileAbsent {
			reconcileErr = callErr
		}
	}
	if reconcileErr != nil {
		return errors.Join(reconcileErr, c.completeControl(parent, control, taskstore.BackgroundRunControlUncertain, reconcileErr.Error()))
	}
	switch state {
	case backgroundopencode.ReconcileExact:
		return c.completeControl(parent, control, taskstore.BackgroundRunControlSucceeded, "")
	case backgroundopencode.ReconcileAdmitted:
		return c.completeControl(parent, control, taskstore.BackgroundRunControlUncertain, "prompt is admitted but resume promotion is not durable")
	case backgroundopencode.ReconcileConflict:
		return c.completeControl(parent, control, taskstore.BackgroundRunControlConflict, "OpenCode message identity conflicts")
	default:
		return c.completeControl(parent, control, taskstore.BackgroundRunControlUncertain, "bounded prompt reconciliation was inconclusive")
	}
}

func (c *Coordinator) completeControl(ctx context.Context, control taskstore.BackgroundRunControl, state taskstore.BackgroundRunControlState, detail string) error {
	now, err := c.freshNow()
	if err != nil {
		return err
	}
	_, err = c.store.CompleteBackgroundRunControl(ctx, controlClaim(control, now), state, detail)
	return err
}
