package disk_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/api"
	"github.com/nebler/fern/internal/disk"
	"github.com/nebler/fern/internal/store"
)

const gib = 1 << 30

// fakePool is a pool with the room and environments a test gives it.
type fakePool struct {
	space atomic.Pointer[store.Space]
	envs  []store.Env
}

func (p *fakePool) Space() (store.Space, error) { return *p.space.Load(), nil }

func (p *fakePool) List(context.Context) ([]store.Env, error) { return p.envs, nil }

func (p *fakePool) Usage(_ context.Context, id string) (store.Usage, error) {
	if id == "gone" {
		return store.Usage{}, fmt.Errorf("environment %q: %w", id, store.ErrNotFound)
	}
	return store.Usage{Total: 3 * gib, Exclusive: gib}, nil
}

// fakeEnvs counts the times retention was applied.
type fakeEnvs struct{ pruned atomic.Int32 }

func (e *fakeEnvs) PruneAll(context.Context) error {
	e.pruned.Add(1)
	return nil
}

// The three images of a node whose tag has moved twice: the one the tag
// points to, the one before it, which a box still runs from, and an older
// one nothing runs from.
var (
	tagged = strings.Repeat("a", 64)
	inUse  = strings.Repeat("b", 64)
	unused = strings.Repeat("c", 64)
)

// node makes a pool's folder with those images, and a /proc in which one
// mount table names the image in use. Podman answers as podman does, or
// with err.
func node(t *testing.T, podmanErr error) (disk.Config, *[]string) {
	t.Helper()
	root, proc := t.TempDir(), t.TempDir()
	for _, id := range []string{tagged, inUse, unused} {
		if err := os.MkdirAll(filepath.Join(root, "images", id, "root"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(tagged, filepath.Join(root, "images", "fern-box:dev")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "userdata"), 0o755); err != nil {
		t.Fatal(err)
	}
	for pid, table := range map[string]string{
		"1":  "22 1 8:1 / / rw - ext4 /dev/sda1 rw\n",
		"70": "148 35 0:92 / /run/fern/boxes/demo/rootfs rw - overlay overlay rw,lowerdir=/var/lib/fern/images/" + inUse + "/root,upperdir=/run/up\n",
	} {
		if err := os.MkdirAll(filepath.Join(proc, pid, "ns"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("mnt:["+pid+"]", filepath.Join(proc, pid, "ns", "mnt")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(proc, pid, "mountinfo"), []byte(table), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var calls []string
	return disk.Config{
		Root: root, Proc: proc,
		Podman: func(_ context.Context, args ...string) ([]byte, error) {
			calls = append(calls, strings.Join(args, " "))
			if podmanErr != nil {
				return nil, podmanErr
			}
			if args[0] == "system" {
				return []byte(`[{"Type":"Images","RawSize":12964349015,"RawReclaimable":12964349015},{"Type":"Containers","RawSize":0}]`), nil
			}
			return []byte("3f2a\n9c1d\n"), nil
		},
		Sizes: func(context.Context, ...string) (store.Usage, error) {
			return store.Usage{Total: 2 * gib, Exclusive: 2 * gib}, nil
		},
	}, &calls
}

func roomy() *store.Space {
	return &store.Space{
		Pool:  store.Volume{Path: "/var/lib/fern", Total: 50 * gib, Free: 27 * gib},
		Under: &store.Volume{Path: "/var/lib/fern-img", Total: 75 * gib, Free: 21 * gib},
		Least: 5 * gib / 2,
	}
}

func TestUsage(t *testing.T) {
	cfg, _ := node(t, nil)
	pool := &fakePool{envs: []store.Env{
		{ID: "golden-app-1a2b3c4d", Workspace: "/var/lib/fern/envs/golden-app-1a2b3c4d/live"},
		{ID: "gone"},
		{ID: "t3-8f2c", Workspace: filepath.Join(cfg.Root, "worktrees/app/t3-8f2c")},
	}}
	pool.space.Store(roomy())
	got, err := disk.New(pool, &fakeEnvs{}, cfg, slog.New(slog.DiscardHandler)).Usage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := api.Disk{
		Pool:  api.Volume{Path: "/var/lib/fern", Total: 50 * gib, Free: 27 * gib},
		Under: &api.Volume{Path: "/var/lib/fern-img", Total: 75 * gib, Free: 21 * gib},
		Least: 5 * gib / 2,
		Parts: []api.DiskPart{
			{Kind: "golden", Name: "golden-app-1a2b3c4d", Total: 3 * gib, Exclusive: gib},
			{Kind: "environment", Name: "t3-8f2c", Total: 3 * gib, Exclusive: gib, Note: "worktrees/app/t3-8f2c"},
			{Kind: "image", Name: "fern-box:dev", Total: 2 * gib, Exclusive: 2 * gib},
			{Kind: "image", Name: inUse[:12], Total: 2 * gib, Exclusive: 2 * gib, Note: "a tag pointed to it before; a box still runs from it"},
			{Kind: "image", Name: unused[:12], Total: 2 * gib, Exclusive: 2 * gib, Unused: true, Note: "a tag pointed to it before; nothing runs from it"},
			{Kind: "data", Name: "T3's data", Total: 2 * gib, Exclusive: 2 * gib},
			{
				Kind: "podman", Name: "Podman's images", Total: 12964349015, Exclusive: 12964349015, Unused: true,
				Note: "on the server's disk, outside the pool: what image builds and installs left",
			},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Usage() mismatch (-want +got):\n%s", diff)
	}
}

// Free removes the image nothing runs from and Podman's unused images, and
// applies retention; the image a box runs from and the tagged one stay.
func TestFree(t *testing.T) {
	tests := []struct {
		name        string
		podman      error
		wantRemoved []string
		wantFailed  []string
	}{
		{
			name: "a node with Podman",
			wantRemoved: []string{
				"image cccccccccccc, which a tag pointed to before",
				"2 of Podman's images, which no container used",
			},
		},
		{
			name:        "a node without Podman",
			podman:      fmt.Errorf("podman image prune: %w", exec.ErrNotFound),
			wantRemoved: []string{"image cccccccccccc, which a tag pointed to before"},
		},
		{
			name:        "a Podman that fails",
			podman:      errors.New("podman image prune: exit status 125: storage is locked"),
			wantRemoved: []string{"image cccccccccccc, which a tag pointed to before"},
			wantFailed:  []string{"podman image prune: exit status 125: storage is locked"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, calls := node(t, tt.podman)
			pool, envs := &fakePool{}, &fakeEnvs{}
			pool.space.Store(roomy())
			got, err := disk.New(pool, envs, cfg, slog.New(slog.DiscardHandler)).Free(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			want := api.Freed{
				Removed: tt.wantRemoved, Failed: tt.wantFailed,
				Pool:  api.Volume{Path: "/var/lib/fern", Total: 50 * gib, Free: 27 * gib},
				Under: &api.Volume{Path: "/var/lib/fern-img", Total: 75 * gib, Free: 21 * gib},
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("Free() mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]string{"image prune --all --force"}, *calls); diff != "" {
				t.Errorf("podman's calls mismatch (-want +got):\n%s", diff)
			}
			for id, want := range map[string]bool{tagged: true, inUse: true, unused: false} {
				if _, err := os.Stat(filepath.Join(cfg.Root, "images", id)); (err == nil) != want {
					t.Errorf("image %.4s exists: %v, want %v", id, err == nil, want)
				}
			}
			if got := envs.pruned.Load(); got != 1 {
				t.Errorf("retention was applied %d times, want once", got)
			}
		})
	}
}

// Run frees when the pool or the disk under it is low, and then not again
// for an hour, however low it stays.
func TestRunFreesWhenLow(t *testing.T) {
	low := func(change func(*store.Space)) *store.Space {
		sp := roomy()
		change(sp)
		return sp
	}
	tests := []struct {
		name  string
		space *store.Space
		want  int32 // times freed in 90 minutes
	}{
		{"room on both", roomy(), 0},
		{"a pool close to refusing snapshots", low(func(sp *store.Space) { sp.Pool.Free = 4 * gib }), 2},
		{"a disk under the pool with less than a tenth free", low(func(sp *store.Space) { sp.Under.Free = 7 * gib }), 2},
		{"a pool on a partition of its own", low(func(sp *store.Space) { sp.Under = nil }), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg, _ := node(t, nil)
				pool, envs := &fakePool{}, &fakeEnvs{}
				pool.space.Store(tt.space)
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() {
					defer close(done)
					disk.New(pool, envs, cfg, slog.New(slog.DiscardHandler)).Run(ctx, time.Minute)
				}()
				time.Sleep(90 * time.Minute)
				synctest.Wait()
				cancel()
				<-done
				if got := envs.pruned.Load(); got != tt.want {
					t.Errorf("freed %d times in 90 minutes, want %d", got, tt.want)
				}
			})
		})
	}
}
