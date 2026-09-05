package backgroundruncoord

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
)

func TestEffectContextAllowsCleanupAfterAttemptDeadline(t *testing.T) {
	now := time.Now().UTC()
	expires := now.Add(time.Minute)
	coordinator := &Coordinator{config: Config{Now: func() time.Time { return now }, OperationTimeout: 10 * time.Second}}
	work := taskstore.BackgroundRunWork{
		Run: taskstore.BackgroundRun{
			EffectPhase:    taskstore.BackgroundRunEffectStopIntent,
			ClaimExpiresAt: &expires,
		},
		Deadline: now.Add(-time.Second),
	}

	ctx, cancel, _, err := coordinator.effectContext(context.Background(), work, false)
	if err != nil {
		t.Fatalf("cleanup context after attempt deadline: %v", err)
	}
	cancel()
	if ctx.Err() != context.Canceled {
		t.Fatalf("cleanup context cancellation = %v", ctx.Err())
	}
	if _, cancel, _, err := coordinator.effectContext(context.Background(), work, true); !errors.Is(err, context.DeadlineExceeded) {
		if cancel != nil {
			cancel()
		}
		t.Fatalf("non-cleanup context after attempt deadline = %v", err)
	}
}

func TestEffectiveRunUsesReplacementAgentDuringHandback(t *testing.T) {
	run := taskstore.BackgroundRun{TaskID: task.TaskID("tsk_0198d34d-6a50-75fb-b1f2-000000000201"), ContainerIdentity: "agent",
		ObservedContainerID: "old", ObservedContainerStartedAt: "old-time", RuntimeEpoch: 1, HostPort: 4000}
	ownership := taskstore.BackgroundRunOwnership{Mode: taskstore.BackgroundRunHandbackRequested, Phase: taskstore.BackgroundRunOwnershipAgentSession,
		WriterGeneration: 3, ContainerIdentity: "agent", ContainerID: "new", ContainerStartedAt: "new-time", RuntimeEpoch: 3, HostPort: 5000}
	effective := effectiveRun(run, ownership)
	if effective.WriterGeneration != 3 || effective.ObservedContainerID != "new" || effective.ObservedContainerStartedAt != "new-time" ||
		effective.RuntimeEpoch != 3 || effective.HostPort != 5000 {
		t.Fatalf("effective replacement run = %+v", effective)
	}
}

func TestEffectContextRejectsExpiredClaimAndPromptDeadline(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	expires := now.Add(time.Second)
	deadline := now.Add(2 * time.Second)
	coordinator := &Coordinator{config: Config{Now: func() time.Time { return now }, OperationTimeout: 10 * time.Second}}
	work := taskstore.BackgroundRunWork{Run: taskstore.BackgroundRun{ClaimExpiresAt: &expires}, Deadline: deadline}

	now = expires
	if _, cancel, _, err := coordinator.effectContext(context.Background(), work, false); !errors.Is(err, context.DeadlineExceeded) {
		if cancel != nil {
			cancel()
		}
		t.Fatalf("expired claim context = %v", err)
	}
	if err := coordinator.promptDispatchAuthority(work); !errors.Is(err, taskstore.ErrInvalidState) {
		t.Fatalf("expired claim prompt authority = %v", err)
	}

	expires = deadline.Add(time.Minute)
	work.Run.ClaimExpiresAt = &expires
	now = deadline
	if err := coordinator.promptDispatchAuthority(work); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired prompt deadline authority = %v", err)
	}
}
