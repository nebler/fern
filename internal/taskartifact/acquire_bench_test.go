package taskartifact

import (
	"context"
	"path/filepath"
	"testing"
)

// BenchmarkAcquireVerified includes fresh Git verification, filesystem copies,
// detached materialization, and mandatory checkout cleanup on every iteration.
// Repository creation, snapshot selection, and CAS installation are untimed.
func BenchmarkAcquireVerified(b *testing.B) {
	engine, repository, base := testEngineRepository(b)
	b.Cleanup(func() {
		if err := engine.Close(); err != nil {
			b.Error(err)
		}
	})
	writeFile(b, filepath.Join(repository, "modified"), []byte("benchmark retained change\n"), 0o600)
	ctx := context.Background()
	want, staged, err := engine.Snapshot(ctx, testSnapshotSpec(b, mustSource(b, repository), base, 42))
	if err != nil {
		b.Fatal(err)
	}
	locator, err := engine.Store(ctx, staged)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snapshot, checkout, err := engine.Acquire(ctx, locator)
		if err != nil {
			b.Fatal(err)
		}
		valid := snapshot.ManifestSHA256 == want.ManifestSHA256 && snapshot.Result == want.Result && checkout.Path() != ""
		if err := checkout.Close(); err != nil {
			b.Fatal(err)
		}
		if !valid {
			b.Fatal("acquisition returned a different snapshot or empty checkout")
		}
	}
}
