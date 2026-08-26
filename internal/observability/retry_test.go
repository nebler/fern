package observability

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryIsExponentiallyBoundedAndResettable(t *testing.T) {
	retry := NewRetry(100*time.Millisecond, 500*time.Millisecond)
	retry.random = func(uint64) uint64 { return 0 }
	want := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 250 * time.Millisecond, 250 * time.Millisecond}
	for i, expected := range want {
		if got := retry.Next(); got != expected {
			t.Fatalf("delay %d = %s, want %s", i, got, expected)
		}
	}
	retry.Reset()
	if got := retry.Next(); got != want[0] {
		t.Fatalf("delay after reset = %s, want %s", got, want[0])
	}
}

func TestRetryJitterIncludesUpperBound(t *testing.T) {
	retry := NewRetry(100*time.Millisecond, time.Second)
	retry.random = func(limit uint64) uint64 { return limit - 1 }
	if got := retry.Next(); got != 100*time.Millisecond {
		t.Fatalf("upper jitter bound = %s", got)
	}
}

func TestWaitIsCancellableAndWakeable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Wait(ctx, nil, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait error = %v", err)
	}
	wake := make(chan struct{}, 1)
	wake <- struct{}{}
	if err := Wait(context.Background(), wake, time.Hour); err != nil {
		t.Fatalf("wake wait error = %v", err)
	}
}
