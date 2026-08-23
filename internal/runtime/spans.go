package runtime

import (
	"context"
	"time"
)

type spanRecorderKey struct{}

// SpanRecorder receives named phase durations emitted along a context during
// runtime operations such as a workspace wake.
type SpanRecorder func(name string, elapsed time.Duration)

// WithSpanRecorder returns a context that routes runtime phase spans to the
// recorder. A nil recorder returns ctx unchanged.
func WithSpanRecorder(ctx context.Context, recorder SpanRecorder) context.Context {
	if recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, spanRecorderKey{}, recorder)
}

// recordSpan emits one elapsed duration to the recorder carried by ctx, if
// any. It is deliberately nil-safe: call sites never need to branch on
// whether tracing is active.
func recordSpan(ctx context.Context, name string, started time.Time) {
	if recorder, ok := ctx.Value(spanRecorderKey{}).(SpanRecorder); ok && recorder != nil {
		recorder(name, time.Since(started))
	}
}
