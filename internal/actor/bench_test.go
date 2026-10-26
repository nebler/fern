package actor_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/driver"
	"github.com/nebler/fern/internal/store"
)

// BenchmarkExec measures mailbox and accounting costs without starting a box.
// Idle sleep is disabled; this measures work rather than timer behavior.
func BenchmarkExec(b *testing.B) {
	g := actor.New(newFakeStore(), driver.NewFake("box"), actor.Idle{}, "box", notrace, slog.New(slog.DiscardHandler))
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
