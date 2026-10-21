package boxexec_test

import (
	"io"
	"strconv"
	"testing"

	"github.com/nebler/fern/internal/boxexec"
)

// targets returns the box itself and a relay in front of it, the two ways a
// client reaches a command.
func targets(tb testing.TB) []struct{ name, url string } {
	tb.Helper()
	box, _ := newBox(tb)
	return []struct{ name, url string }{
		{"direct", box},
		{"proxy", newRelay(tb, box)},
	}
}

// BenchmarkRunThroughput measures streaming a command's output to the client.
func BenchmarkRunThroughput(b *testing.B) {
	const size = 64 << 20
	req := boxexec.Request{Argv: []string{"head", "-c", strconv.Itoa(size), "/dev/zero"}}
	for _, target := range targets(b) {
		b.Run(target.name, func(b *testing.B) {
			b.SetBytes(size)
			for b.Loop() {
				conn, err := boxexec.Dial(b.Context(), target.url, token, nil)
				if err != nil {
					b.Fatalf("Dial(%s) error: %v", target.url, err)
				}
				n := &counter{}
				code, err := boxexec.Run(b.Context(), conn, req, nil, n, io.Discard, nil)
				_ = conn.CloseNow()
				if err != nil || code != 0 || n.n != size {
					b.Fatalf("Run(%q) = %d, %v with %d bytes of output, want 0, nil with %d", req.Argv, code, err, n.n, size)
				}
			}
		})
	}
}

// BenchmarkRunLatency measures a small round trip through a running
// command: a line to cat's stdin and back from its stdout.
func BenchmarkRunLatency(b *testing.B) {
	req := boxexec.Request{Argv: []string{"cat"}}
	for _, target := range targets(b) {
		b.Run(target.name, func(b *testing.B) {
			conn, err := boxexec.Dial(b.Context(), target.url, token, nil)
			if err != nil {
				b.Fatalf("Dial(%s) error: %v", target.url, err)
			}
			defer func() { _ = conn.CloseNow() }()
			stdin, stdinW := io.Pipe()
			echoed := make(chan struct{}, 1)
			stdout := writerFunc(func(p []byte) (int, error) {
				echoed <- struct{}{} // every line fits in one frame
				return len(p), nil
			})
			var code int
			done := make(chan struct{})
			go func() {
				defer close(done)
				code, err = boxexec.Run(b.Context(), conn, req, stdin, stdout, io.Discard, nil)
				_ = stdin.Close() // fail a write nobody will read
			}()
			line := []byte("ping\n")
			for b.Loop() {
				if _, werr := stdinW.Write(line); werr != nil {
					<-done
					b.Fatalf("Run(%q) ended early: %v", req.Argv, err)
				}
				select {
				case <-echoed:
				case <-done:
					b.Fatalf("Run(%q) ended early: %v", req.Argv, err)
				}
			}
			_ = stdinW.Close() // cat sees EOF and exits
			<-done
			if err != nil || code != 0 {
				b.Fatalf("Run(%q) = %d, %v, want 0, nil", req.Argv, code, err)
			}
		})
	}
}

// counter counts the bytes written to it.
type counter struct{ n int }

func (c *counter) Write(p []byte) (int, error) {
	c.n += len(p)
	return len(p), nil
}
