package store_test

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/store"
)

// benchmarks are BenchmarkStore's cases.
var benchmarks = []string{"CreateEmpty", "CreateSeed20000", "Adopt20000", "BuildSource20000", "SnapshotClean", "SnapshotDirty8MiB", "DeleteSnapshot", "Restore", "Delete10Snapshots", "Reconcile", "List", "List100", "Get", "FindByPath", "FindByPath100", "Snapshots50"}

// BenchmarkStore exercises the real btrfs paths. Run in the pool
// provided by lab/test-store.sh; fixture creation and cleanup are not timed.
func BenchmarkStore(b *testing.B) {
	for _, name := range benchmarks {
		b.Run(name, func(b *testing.B) { benchmarkStore(b, name, 0) })
	}
}

// BenchmarkStoreLimited is BenchmarkStore with a disk limit per
// environment: simple quotas on, every subvolume joining its environment's
// quota group. Each case turns quotas off again when it ends, which forgets
// the groups, so BenchmarkStore's cases after it run as a pool without them.
func BenchmarkStoreLimited(b *testing.B) {
	for _, name := range benchmarks {
		b.Run(name, func(b *testing.B) { benchmarkStore(b, name, 100<<30) })
	}
}

func benchmarkStore(b *testing.B, name string, limit uint64) {
	root := os.Getenv("FERN_TEST_BTRFS")
	if root == "" {
		b.Skip("run lab/test-store.sh")
	}
	dir, err := os.MkdirTemp(root, "bench")
	if err != nil {
		b.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	st, err := store.Open(b.Context(), dir)
	if err != nil {
		b.Fatal(err)
	}
	// The test pool is smaller than the free space a real one keeps.
	store.SetRetention(st, 50, 0)
	if limit > 0 {
		if err := st.EnableLimits(b.Context(), limit); err != nil {
			b.Fatal(err)
		}
		// Last, as cleanups run in reverse: on the filesystem, as dir is gone.
		b.Cleanup(func() {
			if err := store.DisableQuotas(root); err != nil {
				b.Error(err)
			}
		})
	}
	b.Cleanup(func() {
		ctx := context.WithoutCancel(b.Context())
		envs, err := st.List(ctx)
		if err != nil {
			b.Error(err)
		}
		for _, env := range envs {
			if err := st.Delete(ctx, env.ID); err != nil {
				b.Error(err)
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			b.Error(err)
		}
	})
	check := func(err error) {
		b.Helper()
		if err != nil {
			b.Fatal(err)
		}
	}
	create := func(id string, opts store.CreateOptions) store.Env {
		b.Helper()
		env, err := st.Create(b.Context(), id, opts)
		check(err)
		return env
	}
	var seed bytes.Buffer
	if name == "CreateSeed20000" || name == "BuildSource20000" || name == "SnapshotClean" || name == "Restore" || name == "Delete10Snapshots" {
		tw := tar.NewWriter(&seed)
		body := bytes.Repeat([]byte("x"), 1024)
		for i := range 20000 {
			check(tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("f-%05d", i), Mode: 0o644, Size: int64(len(body))}))
			_, err := tw.Write(body)
			check(err)
		}
		check(tw.Close())
	}
	env := create("demo", store.CreateOptions{})
	if name == "BuildSource20000" || name == "SnapshotClean" || name == "Restore" || name == "Delete10Snapshots" {
		check(st.Delete(b.Context(), "demo"))
		env = create("demo", store.CreateOptions{Seed: bytes.NewReader(seed.Bytes())})
	}
	check(st.Snapshot(b.Context(), "demo", "base"))
	if name == "BuildSource20000" {
		create("builder", store.CreateOptions{})
	}
	// What a lookup costs on a node with a hundred threads, and a listing of
	// a thread's snapshots at the most Prune leaves.
	if name == "List100" || name == "FindByPath100" {
		for i := range 99 {
			create(fmt.Sprintf("other-%02d", i), store.CreateOptions{})
		}
	}
	if name == "Snapshots50" {
		for i := range 49 { // base is the fiftieth snapshot.
			check(st.Snapshot(b.Context(), "demo", fmt.Sprintf("turn-%02d", i)))
		}
	}
	dirty := make([]byte, 8<<20)
	var samples []time.Duration
	stages := make(map[string]time.Duration)
	var last time.Time
	var measuring bool
	store.SetFailAt(st, func(step string) error {
		if measuring {
			now := time.Now()
			stages[step] += now.Sub(last)
			last = now
		}
		return nil
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		id := fmt.Sprintf("op-%d", i)
		opts := store.CreateOptions{}
		switch name {
		case "CreateSeed20000":
			opts.Seed = bytes.NewReader(seed.Bytes())
		case "Adopt20000":
			opts.Workspace = filepath.Join(dir, "worktrees", "repo", id)
			check(os.MkdirAll(opts.Workspace, 0o755))
			for j := range 20000 {
				check(os.WriteFile(filepath.Join(opts.Workspace, fmt.Sprintf("f-%05d", j)), dirty[:1024], 0o644))
			}
		case "DeleteSnapshot":
			check(st.Snapshot(b.Context(), "demo", id))
		case "SnapshotDirty8MiB":
			check(os.WriteFile(filepath.Join(env.Workspace, "dirty"), dirty, 0o644))
		case "Delete10Snapshots":
			for j := range 9 { // base is the tenth snapshot.
				check(st.Snapshot(b.Context(), "demo", fmt.Sprintf("s-%d", j)))
			}
		}
		b.StartTimer()
		last = time.Now()
		start := last
		measuring = true
		switch name {
		case "CreateEmpty", "CreateSeed20000", "Adopt20000":
			create(id, opts)
		case "BuildSource20000":
			check(st.BuildSource(b.Context(), "builder", store.SnapshotRef{Env: "demo", Snapshot: "base"}))
		case "SnapshotClean", "SnapshotDirty8MiB":
			check(st.Snapshot(b.Context(), "demo", id))
		case "DeleteSnapshot":
			check(st.DeleteSnapshot(b.Context(), "demo", id))
		case "Restore":
			check(st.Restore(b.Context(), "demo", "base"))
		case "Delete10Snapshots":
			check(st.Delete(b.Context(), "demo"))
		case "Reconcile":
			_, err := st.Reconcile(b.Context())
			check(err)
		case "List", "List100":
			_, err := st.List(b.Context())
			check(err)
		case "Get":
			_, err := st.Get(b.Context(), "demo")
			check(err)
		case "FindByPath", "FindByPath100":
			_, err := st.FindByPath(b.Context(), filepath.Join(env.Workspace, "src", "nested", "file.go"))
			check(err)
		case "Snapshots50":
			_, err := st.Snapshots(b.Context(), "demo")
			check(err)
		}
		elapsed := time.Since(start)
		stages["tail"] += time.Since(last)
		measuring = false
		b.StopTimer()
		samples = append(samples, elapsed)
		switch name {
		case "CreateEmpty", "CreateSeed20000", "Adopt20000":
			check(st.Delete(b.Context(), id))
		case "Delete10Snapshots":
			env = create("demo", store.CreateOptions{Seed: bytes.NewReader(seed.Bytes())})
			check(st.Snapshot(b.Context(), "demo", "base"))
		}
		b.StartTimer()
	}
	b.StopTimer()
	slices.Sort(samples)
	b.ReportMetric(float64(samples[len(samples)/2].Nanoseconds()), "p50-ns")
	b.ReportMetric(float64(samples[(len(samples)*95-1)/100].Nanoseconds()), "p95-ns")
	for step, elapsed := range stages {
		b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N), strings.ReplaceAll(step, " ", "_")+"-ns/op")
	}
}
