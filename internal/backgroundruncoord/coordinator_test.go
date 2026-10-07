package backgroundruncoord

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nebler/fern/internal/opencode"
	"github.com/nebler/fern/internal/store"
)

func TestWorkObservationUsesRuntimeState(t *testing.T) {
	for _, tt := range []struct {
		observation opencode.PendingObservation
		state       store.BackgroundRunState
		status      string
	}{
		{opencode.PendingObservation{State: opencode.WorkWorking, Questions: 2, Permissions: 1}, store.BackgroundRunWorking, "positive_active"},
		{opencode.PendingObservation{State: opencode.WorkNeedsYou, Active: true}, store.BackgroundRunNeedsYou, "owned_pending"},
		{opencode.PendingObservation{State: opencode.WorkUnknown, Active: true, Questions: 2}, "", ""},
		{opencode.PendingObservation{State: "unrecognized", Active: true}, "", ""},
	} {
		state, status := workObservation(tt.observation.State)
		if state != tt.state || status != tt.status {
			t.Fatalf("observation %+v = %q/%q, want %q/%q", tt.observation, state, status, tt.state, tt.status)
		}
	}
}

func TestRunReturnsCorruptStoreWithoutDegrading(t *testing.T) {
	for _, err := range []error{store.ErrCorruptStore, fmt.Errorf("claim work: %w", store.ErrCorruptStore), errors.Join(errors.New("external failure"), store.ErrCorruptStore), errors.Join(ErrNoWork, store.ErrCorruptStore)} {
		t.Run(err.Error(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			degraded, succeeded, scans := 0, 0, 0
			coordinator := &Coordinator{wake: make(chan struct{}, 1), config: Config{
				PollInterval: time.Hour,
				OnError:      func(error) { degraded++; cancel() },
				OnSuccess:    func() { succeeded++ },
			}}
			got := coordinator.supervise(ctx, func(context.Context) (bool, error) { scans++; return false, err })
			if !errors.Is(got, store.ErrCorruptStore) || scans != 1 || degraded != 0 || succeeded != 0 {
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
			err := coordinator.supervise(ctx, func(context.Context) (bool, error) {
				cancel()
				return false, tt.scanErr
			})
			if !errors.Is(err, context.Canceled) || degraded != tt.degraded || succeeded != tt.succeeded {
				t.Fatalf("run = %v, degraded=%d succeeded=%d", err, degraded, succeeded)
			}
		})
	}
}

func TestSuperviseRunsAgainImmediatelyOnlyAfterProgress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	scans := 0
	coordinator := &Coordinator{wake: make(chan struct{}, 1), config: Config{PollInterval: time.Hour}}
	// Three progressing scans, then a steady one; with an hour-long tick the
	// fifth scan would only happen on a tick or wake.
	err := coordinator.supervise(ctx, func(context.Context) (bool, error) {
		scans++
		if scans == 5 {
			t.Error("supervisor re-ran after a scan without progress")
		}
		if scans == 4 {
			go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		}
		return scans < 4, nil
	})
	if !errors.Is(err, context.Canceled) || scans != 4 {
		t.Fatalf("supervise = %v, scans=%d", err, scans)
	}
}

func TestEffectContextAllowsCleanupAfterAttemptDeadline(t *testing.T) {
	now := time.Now().UTC()
	coordinator := &Coordinator{config: Config{Now: func() time.Time { return now }, OperationTimeout: 10 * time.Second}}
	work := store.BackgroundRunWork{
		Run: store.BackgroundRun{
			State:       store.BackgroundRunCanceling,
			EffectPhase: store.BackgroundRunEffectCleaning,
			Deadline:    now.Add(-time.Second),
		},
	}

	ctx, cancel, _, err := coordinator.effectContext(context.Background(), work, classify(work.Run).Executing)
	if err != nil {
		t.Fatalf("cleanup context after run deadline: %v", err)
	}
	cancel()
	if ctx.Err() != context.Canceled {
		t.Fatalf("cleanup context cancellation = %v", ctx.Err())
	}
	work.Run.State, work.Run.EffectPhase = store.BackgroundRunWorking, store.BackgroundRunEffectAdmitted
	if _, cancel, _, err := coordinator.effectContext(context.Background(), work, classify(work.Run).Executing); !errors.Is(err, context.DeadlineExceeded) {
		if cancel != nil {
			cancel()
		}
		t.Fatalf("non-cleanup context after run deadline = %v", err)
	}
}

func TestEffectContextBoundsEffectsAndPromptDeadline(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	deadline := now.Add(2 * time.Second)
	coordinator := &Coordinator{config: Config{Now: func() time.Time { return now }, OperationTimeout: 10 * time.Second}}
	work := store.BackgroundRunWork{Run: store.BackgroundRun{Deadline: deadline}}

	ctx, cancel, _, err := coordinator.effectContext(context.Background(), work, false)
	if err != nil {
		t.Fatal(err)
	}
	if bound, ok := ctx.Deadline(); !ok || !bound.Equal(now.Add(10*time.Second)) {
		t.Fatalf("effect deadline = %v, %v", bound, ok)
	}
	cancel()
	ctx, cancel, _, err = coordinator.effectContext(context.Background(), work, true)
	if err != nil {
		t.Fatal(err)
	}
	if bound, ok := ctx.Deadline(); !ok || !bound.Equal(deadline) {
		t.Fatalf("run-bounded effect deadline = %v, %v", bound, ok)
	}
	cancel()
	if err := coordinator.promptDispatchAuthority(work); err != nil {
		t.Fatalf("prompt authority before deadline = %v", err)
	}
	now = deadline
	if err := coordinator.promptDispatchAuthority(work); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired prompt deadline authority = %v", err)
	}
}
