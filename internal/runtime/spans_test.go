package runtime

import (
	"context"
	"testing"
	"time"
)

func TestRecordSpanWithoutRecorderIsNoOp(t *testing.T) {
	t.Parallel()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("recordSpan panicked without a recorder: %v", recovered)
		}
	}()
	recordSpan(context.Background(), "phase", time.Now())
}

func TestWithSpanRecorderReceivesNamedDurations(t *testing.T) {
	t.Parallel()
	var names []string
	ctx := WithSpanRecorder(context.Background(), func(name string, elapsed time.Duration) {
		names = append(names, name)
		if elapsed < 0 {
			t.Errorf("negative elapsed for %q: %v", name, elapsed)
		}
	})
	start := time.Now()
	recordSpan(ctx, "docker_start", start)
	recordSpan(ctx, "health_probe", start)
	if len(names) != 2 || names[0] != "docker_start" || names[1] != "health_probe" {
		t.Fatalf("recorded spans = %v", names)
	}
}

func TestNilRecorderReturnsSameContext(t *testing.T) {
	t.Parallel()
	parent := context.Background()
	if WithSpanRecorder(parent, nil) != parent {
		t.Fatal("nil recorder should return the parent context unchanged")
	}
}
