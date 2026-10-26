package actor_test

import (
	"context"
	"log/slog"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/driver"
	"github.com/nebler/fern/internal/store"
)

// A change's spans are children of its caller's, though the actor's own
// goroutine makes it: the wait for the actor, the disk and the box.
func TestTraceSpans(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := tracetest.NewSpanRecorder()
		tr := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)).Tracer("test")
		st, drv := newFakeStore(), driver.NewFake("box")
		g := actor.New(st, drv, idle, "box", tr, slog.New(slog.DiscardHandler))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { _ = g.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		if err := g.Create(ctx, "demo", store.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		rec.Reset()

		reqCtx, req := tr.Start(ctx, "request")
		if err := g.Wake(reqCtx, "demo"); err != nil {
			t.Fatal(err)
		}
		req.End()

		names := make(map[string]string) // span ID: name
		for _, s := range rec.Ended() {
			names[s.SpanContext().SpanID().String()] = s.Name()
		}
		got := make(map[string]string) // span: its parent
		for _, s := range rec.Ended() {
			if s.Name() != "request" {
				got[s.Name()] = names[s.Parent().SpanID().String()]
			}
		}
		want := map[string]string{"wake": "request", "queue": "wake", "store.get": "wake", "box.wake": "wake"}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("spans' parents mismatch (-want +got):\n%s", diff)
		}
	})
}
