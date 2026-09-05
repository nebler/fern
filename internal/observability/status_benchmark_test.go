package observability

import "testing"

func BenchmarkRegistrySnapshot(b *testing.B) {
	for _, blocked := range []bool{false, true} {
		name := "Ready"
		if blocked {
			name = "Blocked"
		}
		b.Run(name, func(b *testing.B) {
			registry := NewRegistry()
			registry.Healthy(ComponentGitHubTaskDependency)
			registry.Qualified(ComponentBackgroundRunProfile)
			registry.Healthy(ComponentBackgroundRunSerial)
			if blocked {
				registry.Blocked(ComponentGitHubTaskDependency, nil)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				snapshot := registry.Snapshot()
				if snapshot.Ready == blocked || len(snapshot.Components) != len(components) {
					b.Fatal("unexpected readiness snapshot")
				}
			}
		})
	}
	b.Run("ParallelReaders", func(b *testing.B) {
		registry := NewRegistry()
		registry.Healthy(ComponentGitHubTaskDependency)
		registry.Qualified(ComponentBackgroundRunProfile)
		registry.Healthy(ComponentBackgroundRunSerial)
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				snapshot := registry.Snapshot()
				if !snapshot.Ready || len(snapshot.Components) != len(components) {
					b.Error("unexpected readiness snapshot")
					return
				}
			}
		})
	})
}
