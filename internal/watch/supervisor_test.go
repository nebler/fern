package watch

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func TestReduceActivityRequiresCurrentConnectedEpoch(t *testing.T) {
	t.Parallel()
	model := activityModel{active: make(map[string]bool)}
	model.apply(Observation{Epoch: 1, Kind: ObservationConnected})
	model.apply(status(1, "one", "busy"))
	action := model.apply(status(1, "one", "idle"))
	if action != timerArm {
		t.Fatalf("action = %v, want timerArm", action)
	}
	action = model.apply(Observation{Epoch: 1, Kind: ObservationDisconnected})
	if action != timerCancel || model.connected || model.seenBusy {
		t.Fatalf("disconnect did not invalidate eligibility: %+v action=%v", model, action)
	}
	action = model.apply(status(1, "one", "busy"))
	if action != timerNone || model.seenBusy {
		t.Fatal("stale status changed disconnected model")
	}
	model.apply(Observation{Epoch: 2, Kind: ObservationConnected})
	action = model.apply(status(1, "one", "busy"))
	if action != timerNone || model.seenBusy {
		t.Fatal("old epoch changed current model")
	}
}

func TestReduceActivityArmsOnlyOnActiveToIdleTransition(t *testing.T) {
	t.Parallel()
	model := activityModel{active: make(map[string]bool)}
	model.apply(Observation{Epoch: 1, Kind: ObservationConnected})
	action := model.apply(status(1, "unknown", "idle"))
	if action != timerNone {
		t.Fatalf("unrelated idle action = %v, want timerNone", action)
	}
	model.apply(status(1, "one", "busy"))
	action = model.apply(status(1, "one", "idle"))
	if action != timerArm {
		t.Fatalf("active-to-idle action = %v, want timerArm", action)
	}
}

func TestRequestInvalidatesPreviousIdleBoundary(t *testing.T) {
	t.Parallel()
	model := activityModel{active: make(map[string]bool)}
	model.apply(Observation{Epoch: 1, Kind: ObservationConnected})
	model.apply(status(1, "one", "busy"))
	action := model.apply(status(1, "one", "idle"))
	if action != timerArm {
		t.Fatalf("action = %v, want timerArm", action)
	}
	action = model.apply(Observation{Kind: ObservationRequest})
	if action != timerCancel || model.seenBusy {
		t.Fatalf("request did not invalidate boundary: model=%+v action=%v", model, action)
	}
}

func TestSupervisorPausesAfterAllSessionsBecomeIdle(t *testing.T) {
	t.Parallel()
	observations := make(chan Observation, 8)
	paused := make(chan struct{}, 1)
	supervisor := Supervisor{
		IdleAfter: 25 * time.Millisecond,
		OnPause: func(context.Context) error {
			paused <- struct{}{}
			return nil
		},
		Log: testLogger(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go supervisor.Run(ctx, observations)

	observations <- Observation{Epoch: 1, Kind: ObservationConnected}
	observations <- status(1, "one", "busy")
	observations <- status(1, "two", "busy")
	observations <- status(1, "one", "idle")
	assertNoSignal(t, paused, 50*time.Millisecond)
	observations <- status(1, "two", "idle")
	assertSignal(t, paused, 200*time.Millisecond)
}

func TestSupervisorDisconnectCancelsPause(t *testing.T) {
	t.Parallel()
	observations := make(chan Observation, 8)
	var pauses atomic.Int32
	supervisor := Supervisor{
		IdleAfter: 30 * time.Millisecond,
		OnPause: func(context.Context) error {
			pauses.Add(1)
			return nil
		},
		Log: testLogger(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go supervisor.Run(ctx, observations)
	observations <- Observation{Epoch: 1, Kind: ObservationConnected}
	observations <- status(1, "one", "busy")
	observations <- status(1, "one", "idle")
	observations <- Observation{Epoch: 1, Kind: ObservationDisconnected}
	time.Sleep(60 * time.Millisecond)
	if pauses.Load() != 0 {
		t.Fatal("supervisor paused after observation disconnect")
	}
}

func TestSupervisorAcknowledgesRequestInvalidation(t *testing.T) {
	t.Parallel()
	observations := make(chan Observation, 8)
	paused := make(chan struct{}, 1)
	supervisor := Supervisor{
		IdleAfter: 25 * time.Millisecond,
		OnPause: func(context.Context) error {
			paused <- struct{}{}
			return nil
		},
		Log: testLogger(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go supervisor.Run(ctx, observations)
	observations <- Observation{Epoch: 1, Kind: ObservationConnected}
	observations <- status(1, "one", "busy")
	observations <- status(1, "one", "idle")
	handled := make(chan struct{})
	observations <- Observation{Kind: ObservationRequest, Handled: handled}
	assertSignal(t, handled, time.Second)
	assertNoSignal(t, paused, 60*time.Millisecond)
}

func status(epoch uint64, sessionID, value string) Observation {
	return Observation{Epoch: epoch, Kind: ObservationStatus, SessionID: sessionID, Status: value}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func assertSignal(t *testing.T, channel <-chan struct{}, timeout time.Duration) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for signal")
	}
}

func assertNoSignal(t *testing.T, channel <-chan struct{}, duration time.Duration) {
	t.Helper()
	select {
	case <-channel:
		t.Fatal("received unexpected signal")
	case <-time.After(duration):
	}
}
