// Package span starts and ends the daemon's trace spans.
package span

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Start starts span name of environment (or box) id under ctx's span.
func Start(ctx context.Context, tr trace.Tracer, name, id string) (context.Context, trace.Span) {
	return tr.Start(ctx, name, trace.WithAttributes(attribute.String("fern.env", id)))
}

// End ends sp, marking it failed if err isn't nil. A cancelled operation
// didn't fail: its caller went away.
func End(sp trace.Span, err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		sp.RecordError(err)
		sp.SetStatus(codes.Error, err.Error())
	}
	sp.End()
}
