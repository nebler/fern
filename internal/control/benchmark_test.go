package control

import (
	"fmt"
	"testing"
	"time"

	"github.com/nebler/fern/internal/taskstore/taskstoretest"
)

// Keep LastSeen fresh and time fixed: this measures the authenticated read
// path, not the hourly LastSeen write or credential expiry.
func BenchmarkAuthenticateDeviceIdentityCached(b *testing.B) {
	for _, count := range []int{1, 64} {
		b.Run(fmt.Sprintf("devices=%d", count), func(b *testing.B) {
			database, _ := taskstoretest.Open(b)
			store := New(database.DB())
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for i := 0; i < count; i++ {
				if _, err := store.AddDevice(fmt.Sprintf("benchmark-token-%d", i), "Bench", now, now.Add(24*time.Hour)); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok, err := store.AuthenticateDeviceIdentity("benchmark-token-0", now.Add(time.Minute)); err != nil || !ok {
					b.Fatalf("authenticate: ok=%v err=%v", ok, err)
				}
			}
		})
	}
}
