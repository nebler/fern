package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/store"
)

// fakeNode is a node's pool without btrfs: a subvolume is a folder, a send
// stream names its snapshot and its parent and holds the snapshot's files,
// and a receive refuses a stream whose parent it hasn't received. It is the
// backups' Store, Envs, sender and receiver.
type fakeNode struct {
	pool    string
	ids     []string
	gen     map[string]uint64 // a workspace's generation, by its path
	created map[string]uint64 // the generation a snapshot was taken at
	clock   uint64
}

func newFakeNode(t *testing.T, ids ...string) *fakeNode {
	t.Helper()
	return &fakeNode{pool: t.TempDir(), ids: ids, gen: make(map[string]uint64), created: make(map[string]uint64)}
}

func (n *fakeNode) workspace(id string) string { return filepath.Join(n.pool, "envs", id, "live") }

// write gives the node's state and every environment's workspace and record
// version v.
func (n *fakeNode) write(t *testing.T, v string) {
	t.Helper()
	put := func(p, data string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put(filepath.Join(n.pool, "userdata", "settings.json"), v)
	for _, id := range n.ids {
		put(filepath.Join(n.workspace(id), "data"), id+" "+v)
		put(filepath.Join(n.pool, "envs", id, store.RecordFile), id+" "+v)
		n.clock++
		n.gen[n.workspace(id)] = n.clock
	}
}

func (n *fakeNode) List(context.Context) ([]store.Env, error) {
	envs := make([]store.Env, 0, len(n.ids))
	for _, id := range n.ids {
		envs = append(envs, store.Env{ID: id, Workspace: n.workspace(id)})
	}
	return envs, nil
}

func (n *fakeNode) Snapshots(_ context.Context, id string) ([]store.Snapshot, error) {
	if !slices.Contains(n.ids, id) {
		return nil, store.ErrNotFound
	}
	entries, err := os.ReadDir(filepath.Join(n.pool, "envs", id, "snaps"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var snaps []store.Snapshot
	for _, e := range entries {
		snaps = append(snaps, store.Snapshot{ID: e.Name()})
	}
	return snaps, err
}

func (n *fakeNode) SnapshotPath(id, snapID string) string {
	return filepath.Join(n.pool, "envs", id, "snaps", snapID)
}

func (n *fakeNode) BackupSnapshot(_ context.Context, id, snapID string) error {
	if !slices.Contains(n.ids, id) {
		return store.ErrNotFound
	}
	dst := n.SnapshotPath(id, snapID)
	if err := copyFolder(n.workspace(id), dst); err != nil {
		return err
	}
	// A snapshot moves the workspace's generation to its own.
	n.clock++
	n.created[dst], n.gen[n.workspace(id)] = n.clock, n.clock
	return nil
}

func (n *fakeNode) DeleteSnapshot(_ context.Context, id, snapID string) error {
	return os.RemoveAll(n.SnapshotPath(id, snapID))
}

// snapshots returns the backup snapshots in environment id's folder.
func (n *fakeNode) snapshots(t *testing.T, id string) []string {
	t.Helper()
	snaps, err := n.Snapshots(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range snaps {
		names = append(names, s.ID)
	}
	return names
}

func (n *fakeNode) Generation(_ context.Context, dir string) (uint64, error) {
	return n.gen[dir], nil
}

func (n *fakeNode) CreatedAt(_ context.Context, dir string) (uint64, error) {
	return n.created[dir], nil
}

// stream is a fake send stream.
type stream struct {
	Name   string
	Parent string
	Files  map[string]string
}

func (n *fakeNode) Send(_ context.Context, w io.Writer, snap, parent string) error {
	s := stream{Name: filepath.Base(snap), Files: readFolder(snap)}
	if parent != "" {
		if _, err := os.Stat(parent); err != nil {
			return err
		}
		s.Parent = filepath.Base(parent)
	}
	return json.NewEncoder(w).Encode(s)
}

func (n *fakeNode) Receive(_ context.Context, r io.Reader, dir string) error {
	var s stream
	if err := json.NewDecoder(r).Decode(&s); err != nil {
		return err
	}
	if s.Parent != "" {
		if _, err := os.Stat(filepath.Join(dir, s.Parent)); err != nil {
			return fmt.Errorf("the parent of %s isn't here: %w", s.Name, err)
		}
	}
	dst := filepath.Join(dir, s.Name)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s exists already", dst)
	}
	for rel, data := range s.Files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dst, rel)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, rel), []byte(data), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (n *fakeNode) Snapshot(_ context.Context, src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s exists already", dst)
	}
	return copyFolder(src, dst)
}

func (n *fakeNode) Delete(_ context.Context, dir string) error { return os.RemoveAll(dir) }

func copyFolder(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for rel, data := range readFolder(src) {
		p := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// readFolder returns the files under dir by their path in it; a folder
// with nothing in it is "<folder>".
func readFolder(dir string) map[string]string {
	files := make(map[string]string)
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		switch {
		case d.Type().IsRegular():
			data, _ := os.ReadFile(p)
			files[rel] = string(data)
		case d.IsDir() && rel != ".":
			if entries, _ := os.ReadDir(p); len(entries) == 0 {
				files[rel] = "<folder>"
			}
		}
		return nil
	})
	return files
}

// view is what a restored pool holds that a backup is about: each file but
// the backup snapshots, whose names change from backup to backup.
func view(pool string) map[string]string {
	files := readFolder(pool)
	for rel := range files {
		if parts := strings.Split(rel, string(filepath.Separator)); len(parts) > 2 && parts[0] == "envs" && parts[2] == "snaps" {
			delete(files, rel)
		}
	}
	return files
}

// wantView is view of a pool restored from a backup of version v.
func wantView(v string, ids ...string) map[string]string {
	want := map[string]string{"userdata/settings.json": v}
	for _, id := range ids {
		want["envs/"+id+"/live/data"] = id + " " + v
		want["envs/"+id+"/"+store.RecordFile] = id + " " + v
	}
	return want
}

// errCrash stands for the daemon's crash at a step.
var errCrash = errors.New("crashed for the test")

// A backup that stops, at any step or because an upload fails, leaves the
// bucket's last complete backup as it was: a restore brings it back whole,
// workspaces and state of one time. The next backup works, its sends
// incremental from that backup's, and leaves nothing of the stopped one in
// the bucket or the pool. A stop after the manifest is written leaves the
// new backup, and the next one deletes what it replaced.
func TestBackupStops(t *testing.T) {
	ids := []string{"e1", "e2"}
	tests := []struct {
		name   string
		crash  string // the step the backup stops at
		failS3 string // the uploads that fail: keys that contain this
		newer  bool   // the stop is after the manifest: a restore gets the new backup
	}{
		{name: "before e1's snapshot", crash: "snapshot e1"},
		{name: "after e1's snapshot", crash: "send e1"},
		{name: "after e1's send", crash: "snapshot e2"},
		{name: "after e2's snapshot", crash: "send e2"},
		{name: "before the state", crash: "state"},
		{name: "before the manifest", crash: "manifest"},
		{name: "e2's send fails", failS3: "envs/e2/"},
		{name: "the state's upload fails", failS3: "state/"},
		{name: "the manifest's upload fails", failS3: "manifest.json"},
		{name: "after the manifest", crash: "sweep", newer: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s3, bucket := newFakeS3(t)
			cfg := &Config{S3: bucket, Key: make([]byte, KeySize)}
			n := newFakeNode(t, ids...)
			b := New(cfg, "node", n.pool, n, n, slog.New(slog.DiscardHandler))
			b.vol = n
			restored := func(want map[string]string) {
				t.Helper()
				into := t.TempDir()
				r := restorer{cfg: cfg, pool: into, vol: n, log: slog.New(slog.DiscardHandler)}
				if err := r.restore(t.Context(), "node"); err != nil {
					t.Fatalf("restore: %v", err)
				}
				if diff := cmp.Diff(want, view(into)); diff != "" {
					t.Errorf("restored (-want +got):\n%s", diff)
				}
			}

			n.write(t, "v1")
			if err := b.Once(t.Context()); err != nil {
				t.Fatalf("the first backup: %v", err)
			}
			n.write(t, "v2")
			b.failAt = func(step string) error {
				if step == tt.crash {
					return errCrash
				}
				return nil
			}
			s3.failPuts(tt.failS3)
			if err := b.Once(t.Context()); err == nil {
				t.Fatal("the stopped backup didn't fail")
			}
			want := "v1"
			if tt.newer {
				want = "v2"
			}
			restored(wantView(want, ids...))

			b.failAt = nil
			s3.failPuts("")
			n.write(t, "v3")
			if err := b.Once(t.Context()); err != nil {
				t.Fatalf("the backup after: %v", err)
			}
			restored(wantView("v3", ids...))
			m, err := b.manifest(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			wantKeys := []string{m.State, b.prefix + "manifest.json"}
			wantChain := 2 // incremental from the first backup's
			if tt.newer {
				wantChain = 3
			}
			for _, id := range ids {
				eb := m.Envs[id]
				if len(eb.Chain) != wantChain {
					t.Errorf("%s's chain has %d sends, want %d", id, len(eb.Chain), wantChain)
				}
				for _, s := range eb.Chain {
					wantKeys = append(wantKeys, s.Key)
				}
				last := eb.Chain[len(eb.Chain)-1].Snapshot
				if diff := cmp.Diff([]string{last}, n.snapshots(t, id)); diff != "" {
					t.Errorf("%s's backup snapshots: only the last sent is kept (-want +got):\n%s", id, diff)
				}
			}
			slices.Sort(wantKeys)
			if diff := cmp.Diff(wantKeys, s3.keys()); diff != "" {
				t.Errorf("the bucket holds what the manifest names, and nothing else (-want +got):\n%s", diff)
			}
		})
	}
}

// An environment deleted while a backup runs is left out of it, and the
// backup works.
func TestBackupOfADeletedEnvironment(t *testing.T) {
	_, bucket := newFakeS3(t)
	cfg := &Config{S3: bucket, Key: make([]byte, KeySize)}
	n := newFakeNode(t, "e1", "e2")
	b := New(cfg, "node", n.pool, n, n, slog.New(slog.DiscardHandler))
	b.vol = n
	n.write(t, "v1")
	b.failAt = func(step string) error {
		if step == "snapshot e1" {
			n.ids = []string{"e2"}
		}
		return nil
	}
	if err := b.Once(t.Context()); err != nil {
		t.Fatalf("Once() = %v", err)
	}
	m, err := b.manifest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"e2"}, slices.Sorted(maps.Keys(m.Envs))); diff != "" {
		t.Errorf("the backup's environments (-want +got):\n%s", diff)
	}
}

// A restore that stops at any step leaves its marker in the pool, and run
// again it finishes: the pool is then as one restore that didn't stop
// leaves it. A bucket with another backup since is refused, as is a pool
// with environments and no marker.
func TestRestoreStops(t *testing.T) {
	_, bucket := newFakeS3(t)
	cfg := &Config{S3: bucket, Key: make([]byte, KeySize)}
	n := newFakeNode(t, "e1", "e2")
	b := New(cfg, "node", n.pool, n, n, slog.New(slog.DiscardHandler))
	b.vol = n
	for _, v := range []string{"v1", "v2"} { // e1 and e2 have a chain of two
		n.write(t, v)
		if err := b.Once(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	restore := func(pool string, failAt func(string) error) error {
		r := restorer{cfg: cfg, pool: pool, vol: n, log: slog.New(slog.DiscardHandler), failAt: failAt}
		return r.restore(t.Context(), "node")
	}
	whole := t.TempDir()
	var steps []string
	if err := restore(whole, func(step string) error { steps = append(steps, step); return nil }); err != nil {
		t.Fatal(err)
	}
	want := readFolder(whole)
	wantSteps := []string{
		"marker", "receive e1 0", "receive e1 1", "snaps e1", "workspace e1",
		"receive e2 0", "receive e2 1", "snaps e2", "workspace e2", "state", "done",
	}
	if diff := cmp.Diff(wantSteps, steps); diff != "" {
		t.Fatalf("a restore's steps (-want +got):\n%s", diff)
	}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			pool := t.TempDir()
			if err := restore(pool, func(s string) error {
				if s == step {
					return errCrash
				}
				return nil
			}); !errors.Is(err, errCrash) {
				t.Fatalf("the stopped restore = %v, want the crash", err)
			}
			if _, err := os.Stat(filepath.Join(pool, RestoringFile)); err != nil {
				t.Errorf("after the stop, the marker: %v", err)
			}
			if err := restore(pool, nil); err != nil {
				t.Fatalf("the restore run again: %v", err)
			}
			if diff := cmp.Diff(want, readFolder(pool)); diff != "" {
				t.Errorf("the pool (-a restore that didn't stop +this one):\n%s", diff)
			}
		})
	}

	t.Run("another backup since", func(t *testing.T) {
		pool := t.TempDir()
		_ = restore(pool, func(s string) error {
			if s == "state" {
				return errCrash
			}
			return nil
		})
		n.write(t, "v3")
		if err := b.Once(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := restore(pool, nil); err == nil || !strings.Contains(err.Error(), "another backup") {
			t.Errorf("the restore run again = %v, want it refused", err)
		}
	})
	t.Run("a pool with environments", func(t *testing.T) {
		if err := restore(whole, nil); err == nil || !strings.Contains(err.Error(), "has environments") {
			t.Errorf("a restore into a restored pool = %v, want it refused", err)
		}
	})
}
