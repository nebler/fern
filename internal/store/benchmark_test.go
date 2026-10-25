package store_test

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/store"
)

// BenchmarkStore exercises the real btrfs and SQLite paths. Run in the pool
// provided by lab/test-store.sh; fixture creation and cleanup are not timed.
func BenchmarkStore(b *testing.B) {
	for _, name := range []string{"CreateEmpty", "CreateSeed20000", "Adopt20000", "BuildSource20000", "SnapshotClean", "SnapshotDirty8MiB", "DeleteSnapshot", "Restore", "Delete10Snapshots", "Reconcile", "Reconcile100Orphans", "List", "Get", "FindByPath"} {
		b.Run(name, func(b *testing.B) { benchmarkStore(b, name) })
	}
}

func benchmarkStore(b *testing.B, name string) {
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
		if err := st.Close(); err != nil {
			b.Error(err)
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
	var index *sql.DB
	if name == "Reconcile100Orphans" {
		for i := range 100 {
			check(st.Snapshot(b.Context(), "demo", fmt.Sprintf("recover-%03d", i)))
		}
		index, err = sql.Open("sqlite", "file:"+filepath.Join(dir, "state", "fern.sqlite")+"?_pragma=busy_timeout(5000)")
		check(err)
		b.Cleanup(func() {
			if err := index.Close(); err != nil {
				b.Error(err)
			}
		})
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
		case "Reconcile100Orphans":
			_, err := index.ExecContext(b.Context(), `DELETE FROM snapshots WHERE env = 'demo' AND id != 'base'`)
			check(err)
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
		case "Reconcile", "Reconcile100Orphans":
			_, err := st.Reconcile(b.Context())
			check(err)
		case "List":
			_, err := st.List(b.Context())
			check(err)
		case "Get":
			_, err := st.Get(b.Context(), "demo")
			check(err)
		case "FindByPath":
			_, err := st.FindByPath(b.Context(), filepath.Join(env.Workspace, "src", "nested", "file.go"))
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

// BenchmarkIndexCommit separates statement preparation from WAL durability.
// NORMAL is a scratch-database experiment, not a Store setting: the index also
// holds box tokens that cannot be recovered from workspace snapshots.
func BenchmarkIndexCommit(b *testing.B) {
	for _, name := range []string{"FULL", "PreparedFULL", "NORMAL"} {
		b.Run(name, func(b *testing.B) {
			root := os.Getenv("FERN_TEST_BTRFS")
			if root == "" {
				b.Skip("run lab/test-store.sh")
			}
			dir, err := os.MkdirTemp(root, "index-bench")
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				if err := os.RemoveAll(dir); err != nil {
					b.Error(err)
				}
			})
			synchronous := "FULL"
			if name == "NORMAL" {
				synchronous = "NORMAL"
			}
			db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "index.sqlite")+"?_pragma=journal_mode(WAL)&_pragma=synchronous("+synchronous+")")
			if err != nil {
				b.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			b.Cleanup(func() {
				if err := db.Close(); err != nil {
					b.Error(err)
				}
			})
			if _, err := db.ExecContext(b.Context(), `CREATE TABLE entries (id INTEGER PRIMARY KEY, value INTEGER)`); err != nil {
				b.Fatal(err)
			}
			const query = `INSERT OR REPLACE INTO entries VALUES (1, ?)`
			exec := db.ExecContext
			if name == "PreparedFULL" {
				stmt, err := db.PrepareContext(b.Context(), query)
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() {
					if err := stmt.Close(); err != nil {
						b.Error(err)
					}
				})
				exec = func(ctx context.Context, _ string, args ...any) (sql.Result, error) {
					return stmt.ExecContext(ctx, args...)
				}
			}
			for i := 0; b.Loop(); i++ {
				if _, err := exec(b.Context(), query, i); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
