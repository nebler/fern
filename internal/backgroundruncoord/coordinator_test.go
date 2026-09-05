package backgroundruncoord

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nebler/fern/internal/backgroundopencode"
	"github.com/nebler/fern/internal/taskstore"
)

func TestWorkObservationUsesRuntimeState(t *testing.T) {
	for _, tt := range []struct {
		observation backgroundopencode.PendingObservation
		state       taskstore.BackgroundRunState
		status      string
	}{
		{backgroundopencode.PendingObservation{State: backgroundopencode.WorkWorking, Questions: 2, Permissions: 1}, taskstore.BackgroundRunWorking, "positive_active"},
		{backgroundopencode.PendingObservation{State: backgroundopencode.WorkNeedsYou, Active: true}, taskstore.BackgroundRunNeedsYou, "owned_pending"},
		{backgroundopencode.PendingObservation{State: backgroundopencode.WorkUnknown, Active: true, Questions: 2}, "", ""},
		{backgroundopencode.PendingObservation{State: "unrecognized", Active: true}, "", ""},
	} {
		state, status := workObservation(tt.observation.State)
		if state != tt.state || status != tt.status {
			t.Fatalf("observation %+v = %q/%q, want %q/%q", tt.observation, state, status, tt.state, tt.status)
		}
	}
}

func TestRunReturnsCorruptStoreWithoutDegrading(t *testing.T) {
	for _, err := range []error{taskstore.ErrCorruptStore, fmt.Errorf("claim work: %w", taskstore.ErrCorruptStore), errors.Join(errors.New("external failure"), taskstore.ErrCorruptStore), errors.Join(ErrNoWork, taskstore.ErrCorruptStore)} {
		t.Run(err.Error(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			degraded, succeeded, scans := 0, 0, 0
			coordinator := &Coordinator{wake: make(chan struct{}, 1), config: Config{
				PollInterval: time.Hour,
				OnError:      func(error) { degraded++; cancel() },
				OnSuccess:    func() { succeeded++ },
			}}
			got := coordinator.supervise(ctx, func(context.Context) error { scans++; return err })
			if !errors.Is(got, taskstore.ErrCorruptStore) || scans != 1 || degraded != 0 || succeeded != 0 {
				t.Fatalf("Run = %v, scans=%d degraded=%d succeeded=%d", got, scans, degraded, succeeded)
			}
		})
	}
}

func TestRunClassifiesTransientAndSuccessfulScans(t *testing.T) {
	for _, tt := range []struct {
		name                string
		scanErr             error
		degraded, succeeded int
	}{
		{"progress", nil, 0, 1},
		{"no work", ErrNoWork, 0, 1},
		{"transient failure", errors.New("Docker unavailable"), 1, 0},
		{"cancellation", context.Canceled, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			degraded, succeeded := 0, 0
			coordinator := &Coordinator{wake: make(chan struct{}, 1), config: Config{
				PollInterval: time.Hour,
				OnError:      func(error) { degraded++ },
				OnSuccess:    func() { succeeded++ },
			}}
			err := coordinator.supervise(ctx, func(context.Context) error {
				cancel()
				return tt.scanErr
			})
			if !errors.Is(err, context.Canceled) || degraded != tt.degraded || succeeded != tt.succeeded {
				t.Fatalf("run = %v, degraded=%d succeeded=%d", err, degraded, succeeded)
			}
		})
	}
}

func TestEffectContextAllowsCleanupAfterAttemptDeadline(t *testing.T) {
	now := time.Now().UTC()
	expires := now.Add(time.Minute)
	coordinator := &Coordinator{config: Config{Now: func() time.Time { return now }, OperationTimeout: 10 * time.Second}}
	work := taskstore.BackgroundRunWork{
		Run: taskstore.BackgroundRun{
			State:          taskstore.BackgroundRunCanceling,
			EffectPhase:    taskstore.BackgroundRunEffectStopIntent,
			ClaimExpiresAt: &expires,
		},
		Deadline: now.Add(-time.Second),
	}

	ctx, cancel, _, err := coordinator.effectContext(context.Background(), work, classify(work.Run).EnforceAttemptDeadline)
	if err != nil {
		t.Fatalf("cleanup context after attempt deadline: %v", err)
	}
	cancel()
	if ctx.Err() != context.Canceled {
		t.Fatalf("cleanup context cancellation = %v", ctx.Err())
	}
	work.Run.State, work.Run.EffectPhase = taskstore.BackgroundRunWorking, taskstore.BackgroundRunEffectPromptAdmitted
	if _, cancel, _, err := coordinator.effectContext(context.Background(), work, classify(work.Run).EnforceAttemptDeadline); !errors.Is(err, context.DeadlineExceeded) {
		if cancel != nil {
			cancel()
		}
		t.Fatalf("non-cleanup context after attempt deadline = %v", err)
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
