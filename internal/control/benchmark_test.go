package control

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// Keep LastSeen fresh and time fixed: this measures the authenticated read
// path, not periodic full-file persistence or credential expiry.
func BenchmarkAuthenticateDeviceIdentityCached(b *testing.B) {
	for _, count := range []int{1, 64} {
		b.Run(fmt.Sprintf("devices=%d", count), func(b *testing.B) {
			directory := b.TempDir()
			if err := os.Chmod(directory, 0o700); err != nil {
				b.Fatal(err)
			}
			store, err := Open(directory, "bench")
			if err != nil {
				b.Fatal(err)
			}
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for i := 0; i < count; i++ {
				if _, err := store.AddDevice(fmt.Sprintf("benchmark-token-%d", i), "Bench", now, now.Add(24*time.Hour)); err != nil {
					b.Fatal(err)
				}
			}
			revision := store.data.Revision
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok, err := store.AuthenticateDeviceIdentity("benchmark-token-0", now.Add(time.Minute)); err != nil || !ok {
					b.Fatalf("authenticate: ok=%v err=%v", ok, err)
				}
			}
			b.StopTimer()
			if store.data.Revision != revision {
				b.Fatal("cached authentication unexpectedly persisted state")
			}
		})
	}
}
