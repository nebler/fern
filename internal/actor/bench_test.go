package actor_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
)

// BenchmarkExec measures mailbox and accounting costs without starting a box.
// Idle sleep is disabled; this measures work rather than timer behavior.
func BenchmarkExec(b *testing.B) {
	g := actor.New(newFakeStore(), runsc.NewFake("box"), actor.Config{Image: "box"}, notrace, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(b.Context())
	done := make(chan error, 1)
	go func() { done <- g.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			b.Error(err)
		}
	}()
	if err := g.Create(ctx, "bench", store.CreateOptions{}); err != nil {
		b.Fatal(err)
	}
	if err := g.Wake(ctx, "bench"); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		ex, err := g.Exec(ctx, "bench", "", func() {})
		if err != nil {
			b.Fatal(err)
		}
		ex.Close()
	}
}
