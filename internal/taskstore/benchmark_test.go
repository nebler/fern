package taskstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// benchmarkStore retains production WAL, FULL synchronization, triggers, and
// foreign keys. Database creation and workspace admission are setup, not samples.
func benchmarkStore(b *testing.B) *Store {
	b.Helper()
	dir := filepath.Join(b.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		b.Fatal(err)
	}
	s, err := Open(context.Background(), filepath.Join(dir, "tasks.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := s.Close(); err != nil {
			b.Error(err)
		}
	})
	if err := s.CreateWorkspace(context.Background(), testWorkspaceBinding()); err != nil {
		b.Fatal(err)
	}
	return s
}

// Each iteration creates fresh durable rows, never an idempotency replay.
// Parameter construction is included; secure ID generation is not.
func BenchmarkBackgroundRunAdmissionFresh(b *testing.B) {
	s := benchmarkStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := testAdmission(i+1, fmt.Sprintf("bench-%d", i), "Implement the requested change and retain the result.")
		admission, err := s.AdmitBackgroundRun(ctx, p)
		if err != nil || admission.Replayed || admission.Task.ID != p.TaskID {
			b.Fatalf("fresh admission: replay=%v err=%v", admission.Replayed, err)
		}
	}
}

// Recovery reselects the existing provisioning run. It measures the
// selection/scan path, not first provisioning or I/O.
func BenchmarkNextBackgroundRunRecovery(b *testing.B) {
	s := benchmarkStore(b)
	ctx := context.Background()
	p := testAdmission(1, "claim", "Work")
	if _, err := s.AdmitBackgroundRun(ctx, p); err != nil {
		b.Fatal(err)
	}
	initial, err := startNextBackgroundRun(ctx, s, testTime.Truncate(time.Millisecond))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		current, err := s.NextBackgroundRun(ctx, testWorkspaceID(), BackgroundRunSourceProfile)
		if err != nil || current.TaskID != p.TaskID || current.Revision != initial.Revision {
			b.Fatalf("recovery read: revision=%d err=%v", current.Revision, err)
		}
	}
}

// Sparse lists still request the maximum bound, exposing growth-policy costs.
// Empty lists must not reserve a backing array.
func BenchmarkBackgroundRunListSparse(b *testing.B) {
	for _, count := range []int{0, 1, 10} {
		b.Run(fmt.Sprintf("Rows%dLimit100", count), func(b *testing.B) {
			s := benchmarkStore(b)
			ctx := context.Background()
			p := testAdmission(1, "sparse-1", "Work")
			for i := 1; i <= count; i++ {
				if _, err := s.AdmitBackgroundRun(ctx, testAdmission(i, fmt.Sprintf("sparse-%d", i), "Work")); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				runs, err := s.ListBackgroundRuns(ctx, testWorkspaceID(), p.Claim.Actor, MaxBackgroundRunListLimit)
				if err != nil || runs == nil || len(runs) != count {
					b.Fatalf("sparse list: count=%d nil=%v err=%v", len(runs), runs == nil, err)
				}
			}
		})
	}
}

func BenchmarkBackgroundRunRead(b *testing.B) {
	s := benchmarkStore(b)
	ctx := context.Background()
	p := testAdmission(1, "read-1", "Work")
	for i := 1; i <= MaxBackgroundRunListLimit; i++ {
		if _, err := s.AdmitBackgroundRun(ctx, testAdmission(i, fmt.Sprintf("read-%d", i), "Work")); err != nil {
			b.Fatal(err)
		}
	}
	b.Run("OwnedGet", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			current, err := s.GetBackgroundRun(ctx, testWorkspaceID(), p.TaskID, p.Claim.Actor)
			if err != nil || current.TaskID != p.TaskID {
				b.Fatalf("owned read: %v", err)
			}
		}
	})
	b.Run("OwnedList100", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			runs, err := s.ListBackgroundRuns(ctx, testWorkspaceID(), p.Claim.Actor, MaxBackgroundRunListLimit)
			if err != nil || len(runs) != MaxBackgroundRunListLimit {
				b.Fatalf("owned list: count=%d err=%v", len(runs), err)
			}
		}
	})
	b.Run("ReceiptLookup", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			receipt, found, err := s.FindReceiptByIdempotency(ctx, testWorkspaceID(), CreateBackgroundRunCommand, p.Claim.Key)
			if err != nil || !found || receipt.TargetID != p.TaskID {
				b.Fatalf("receipt lookup: found=%v err=%v", found, err)
			}
		}
	})
}
