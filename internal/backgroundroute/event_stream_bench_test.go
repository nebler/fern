package backgroundroute

import (
	"bytes"
	"io"
	"testing"
)

// BenchmarkAttachmentEventStream exercises the actual pipe/worker ownership
// path, JSON identity filtering, and Close, not just the event predicate.
func BenchmarkAttachmentEventStream(b *testing.B) {
	allowed := []byte("data: {\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"ses_owned\",\"text\":\"hello\"}}\n\n")
	foreign := []byte("data: {\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"ses_foreign\",\"text\":\"hidden\"}}\n\n")
	for _, tc := range []struct {
		name  string
		pairs int
	}{{"Small16Events", 8}, {"Large4096Events", 2048}} {
		b.Run(tc.name, func(b *testing.B) {
			pair := append(append([]byte(nil), allowed...), foreign...)
			input := bytes.Repeat(pair, tc.pairs)
			want := bytes.Repeat(allowed, tc.pairs)
			// Check exact filtering outside the timer; each timed iteration also
			// checks output length and errors while discarding output allocations.
			stream := filterAttachmentEvents(io.NopCloser(bytes.NewReader(input)), "ses_owned")
			got, err := io.ReadAll(stream)
			closeErr := stream.Close()
			if err != nil || closeErr != nil || !bytes.Equal(got, want) {
				b.Fatalf("filter fixture: read=%v close=%v output matches=%t", err, closeErr, bytes.Equal(got, want))
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stream := filterAttachmentEvents(io.NopCloser(bytes.NewReader(input)), "ses_owned")
				n, err := io.Copy(io.Discard, stream)
				closeErr := stream.Close()
				if err != nil || closeErr != nil || n != int64(len(want)) {
					b.Fatalf("filter: read=%v close=%v bytes=%d want=%d", err, closeErr, n, len(want))
				}
			}
		})
	}
}
