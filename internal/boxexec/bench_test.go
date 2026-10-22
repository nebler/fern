package boxexec_test

import (
	"io"
	"strconv"
	"testing"

	"github.com/nebler/fern/internal/boxexec"
)

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
				var n int
				count := writerFunc(func(p []byte) (int, error) { n += len(p); return len(p), nil })
				code, err := boxexec.Run(b.Context(), conn, req, boxexec.Streams{Stdout: count, Stderr: io.Discard})
				_ = conn.CloseNow()
				if err != nil || code != 0 || n != size {
					b.Fatalf("Run(%q) = %d, %v with %d bytes of output, want 0, nil with %d", req.Argv, code, err, n, size)
				}
			}
		})
	}
}
