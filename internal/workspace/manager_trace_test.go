package workspace

import (
	"context"
	"testing"

	"github.com/nebler/fern/internal/runtime"
)

func TestLastWakeTraceAbsentBeforeFirstWake(t *testing.T) {
	t.Parallel()
	manager := NewManager(context.Background(), newFakeRuntime(runtime.StateRunning), runtime.Spec{Name: "demo"}, nil, nil, nil)
	if _, ok := manager.LastWakeTrace(); ok {
		t.Fatal("fresh manager reported a completed wake")
	}
}

func TestWorkRequestRecordsOrderedTraceSpans(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StatePaused)
	manager := NewManager(
		context.Background(),
		fake,
		runtime.Spec{Name: "demo"},
		func(context.Context, runtime.Endpoint, bool) error { return nil },
		nil,
		nil,
	)
	target, release, err := manager.AcquireRequest(context.Background(), RequestWork)
	if err != nil {
		t.Fatalf("acquire request: %v", err)
	}
	release()
	if target.Generation == 0 {
		t.Fatal("wake published no endpoint generation")
	}
	trace, ok := manager.LastWakeTrace()
	if !ok {
		t.Fatal("work request produced no wake trace")
	}
	if trace.Workspace != "demo" {
		t.Fatalf("trace workspace = %q, want %q", trace.Workspace, "demo")
	}
	if trace.TotalMillis < 0 {
		t.Fatalf("negative total: %d", trace.TotalMillis)
	}
	names := make([]string, 0, len(trace.Spans))
	lastOffset := int64(-1)
	for _, span := range trace.Spans {
		names = append(names, span.Name)
		if span.OffsetMs < lastOffset {
			t.Fatalf("span offsets not ascending: %+v", trace.Spans)
		}
		if span.Millis < 0 {
			t.Fatalf("negative span duration: %+v", span)
		}
		lastOffset = span.OffsetMs
	}
	for _, want := range []string{"lifecycle_token", "runtime_total", "observer_attach"} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %q span in %v", want, names)
		}
	}
}
