package workspace

import (
	"sync"
	"time"

	"github.com/nebler/fern/internal/runtime"
)

// TraceSpan is one named phase of a single wake, measured relative to the
// start of the coalesced wake call.
type TraceSpan struct {
	Name     string `json:"name"`
	OffsetMs int64  `json:"offset_ms"`
	Millis   int64  `json:"millis"`
}

// WakeTrace describes one completed coalesced wake: the ordered phases from
// admission through runtime mutation to activity-observer attach.
type WakeTrace struct {
	Workspace   string      `json:"workspace"`
	StartedAt   time.Time   `json:"started_at"`
	TotalMillis int64       `json:"total_ms"`
	Spans       []TraceSpan `json:"spans"`
}

// traceCollector accumulates spans for one wake call. The zero value is ready
// for use, and every method is nil-safe so call sites never branch on whether
// tracing is active (reconcile paths pass nil).
type traceCollector struct {
	mu    sync.Mutex
	start time.Time
	spans []TraceSpan
}

func newTraceCollector(started time.Time) *traceCollector {
	return &traceCollector{start: started}
}

// append records one finished phase given its own start instant.
func (t *traceCollector) append(name string, started time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	offset := started.Sub(t.start)
	if offset < 0 {
		offset = 0
	}
	t.spans = append(t.spans, TraceSpan{Name: name, OffsetMs: offset.Milliseconds(), Millis: time.Since(started).Milliseconds()})
}

// spanRecorder adapts the collector to runtime.WithSpanRecorder.
func (t *traceCollector) spanRecorder() runtime.SpanRecorder {
	if t == nil {
		return nil
	}
	return func(name string, elapsed time.Duration) {
		t.mu.Lock()
		defer t.mu.Unlock()
		endOffset := time.Since(t.start)
		offset := endOffset - elapsed
		if offset < 0 {
			offset = 0
		}
		t.spans = append(t.spans, TraceSpan{
			Name:     name,
			OffsetMs: offset.Milliseconds(),
			Millis:   elapsed.Milliseconds(),
		})
	}
}

// finish freezes the trace with the supplied completion instant.
func (t *traceCollector) finish(completed time.Time) WakeTrace {
	total := completed.Sub(t.start).Milliseconds()
	t.mu.Lock()
	defer t.mu.Unlock()
	spans := append([]TraceSpan(nil), t.spans...)
	return WakeTrace{StartedAt: t.start, TotalMillis: total, Spans: spans}
}
