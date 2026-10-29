package server_test

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"

	"github.com/nebler/fern/internal/boxexec"
)

// BenchmarkUnixDial isolates the local connection cost from HTTP and JSON.
func BenchmarkUnixDial(b *testing.B) {
	path := filepath.Join(b.TempDir(), "s")
	ctx, cancel := context.WithCancel(b.Context())
	ln, err := new(net.ListenConfig).Listen(ctx, "unix", path)
	if err != nil {
		cancel()
		b.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() == nil {
					b.Error(err)
				}
				return
			}
			_ = conn.Close()
		}
	}()
	defer func() { cancel(); _ = ln.Close(); <-done }()
	var dialer net.Dialer
	for b.Loop() {
		conn, err := dialer.DialContext(ctx, "unix", path)
		if err != nil {
			b.Fatal(err)
		}
		_ = conn.Close()
	}
}

// BenchmarkRequests measures the Unix API and actor with an in-memory store.
// Snapshot includes the mailbox round trip, but no filesystem work.
func BenchmarkRequests(b *testing.B) {
	c, _ := newAPIWith(b, nil)
	if err := c.Create(b.Context(), "bench", "", nil); err != nil {
		b.Fatal(err)
	}
	if err := c.Wake(b.Context(), "bench"); err != nil {
		b.Fatal(err)
	}
	for _, name := range []string{"lookup", "snapshot", "exec"} {
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				var err error
				switch name {
				case "lookup":
					_, err = c.FindByPath(b.Context(), "/pool/envs/bench/live")
				case "snapshot":
					err = c.Snapshot(b.Context(), "bench", "checkpoint", true)
				case "exec":
					var code int
					code, err = c.Exec(b.Context(), "bench", boxexec.Request{Argv: []string{"true"}}, boxexec.Streams{Stdout: io.Discard, Stderr: io.Discard})
					if code != 0 {
						b.Fatalf("Exec(true) code = %d, want 0", code)
					}
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
