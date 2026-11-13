// Package disk says what uses a Fern node's disk and frees what nothing
// needs: images a tag has moved off, Podman's copies of images the pool has
// unpacked, snapshots past their retention. A pool that is a file on the
// server's disk shares that disk with all of it, and a full disk fails
// every thread at once (architecture §9).
package disk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nebler/fern/internal/api"
	"github.com/nebler/fern/internal/image"
	"github.com/nebler/fern/internal/store"
)

// Pool is what a Keeper reads of the pool.
type Pool interface {
	Space() (store.Space, error)
	List(ctx context.Context) ([]store.Env, error)
	Usage(ctx context.Context, id string) (store.Usage, error)
	Quota(ctx context.Context, env store.Env) (store.Quota, error)
	ClearGroups(ctx context.Context) (int, error)
}

// Envs applies retention to every environment, each through its actor.
type Envs interface {
	PruneAll(ctx context.Context) error
}

// Config is the node a Keeper looks at.
type Config struct {
	Root string // the pool, /var/lib/fern
	Proc string // /proc
	// Podman runs podman with args and returns what it printed; an error
	// that is exec.ErrNotFound means the node has none.
	Podman func(ctx context.Context, args ...string) ([]byte, error)
	// Sizes returns the disk space of paths in the pool (store.PathUsage).
	Sizes func(ctx context.Context, paths ...string) (store.Usage, error)
}

// Podman runs the node's podman.
func Podman(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "podman", args...).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("podman %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(exit.Stderr))
		}
		return nil, fmt.Errorf("podman %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// podmanKeep is how young an image of Podman's is left alone, whatever uses
// it.
const podmanKeep = time.Hour

// Keeper reports and frees a node's disk space.
type Keeper struct {
	pool Pool
	envs Envs
	cfg  Config
	log  *slog.Logger
}

// New returns a Keeper for the node cfg describes.
func New(pool Pool, envs Envs, cfg Config, log *slog.Logger) *Keeper {
	return &Keeper{pool: pool, envs: envs, cfg: cfg, log: log}
}

func (k *Keeper) images() string { return filepath.Join(k.cfg.Root, "images") }

// Usage returns what uses the disk. It asks btrfs for every environment's
// size, which reads each file's extents: seconds on a pool with large
// workspaces.
func (k *Keeper) Usage(ctx context.Context) (api.Disk, error) {
	sp, err := k.pool.Space()
	if err != nil {
		return api.Disk{}, err
	}
	d := spaceOf(sp)
	envs, err := k.pool.List(ctx)
	if err != nil {
		return api.Disk{}, err
	}
	for _, env := range envs {
		u, err := k.pool.Usage(ctx, env.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue // deleted since it was listed
		}
		if err != nil {
			return api.Disk{}, err
		}
		part := api.DiskPart{Kind: "environment", Name: env.ID, Total: u.Total, Exclusive: u.Exclusive}
		if env.Group != 0 {
			q, err := k.pool.Quota(ctx, env)
			if err != nil {
				return api.Disk{}, err
			}
			part.Written, part.Limit = q.Used, q.Limit
		}
		if store.IsBuilder(env.ID) {
			part.Kind = "golden"
		} else if rel, err := filepath.Rel(k.cfg.Root, env.Workspace); err == nil {
			part.Note = rel
		}
		d.Parts = append(d.Parts, part)
	}
	imgs, err := image.List(k.images())
	if err != nil {
		return api.Disk{}, err
	}
	for _, img := range imgs {
		u, err := k.cfg.Sizes(ctx, filepath.Join(k.images(), img.ID))
		if err != nil {
			return api.Disk{}, err
		}
		part := api.DiskPart{Kind: "image", Name: strings.Join(img.Tags, ", "), Total: u.Total, Exclusive: u.Exclusive}
		if len(img.Tags) == 0 {
			part.Name = img.ID[:12]
			part.Note = "a tag pointed to it before; a box still runs from it"
			if !k.inUse(img.ID) {
				part.Unused, part.Note = true, "a tag pointed to it before; nothing runs from it"
			}
		}
		d.Parts = append(d.Parts, part)
	}
	for _, group := range []struct {
		name  string
		paths []string
	}{
		{"T3's data", []string{"userdata", "caches", "tools"}},
		{"repositories", []string{"repos", "projects"}},
	} {
		var paths []string
		for _, p := range group.paths {
			if _, err := os.Stat(filepath.Join(k.cfg.Root, p)); err == nil {
				paths = append(paths, filepath.Join(k.cfg.Root, p))
			}
		}
		if len(paths) == 0 {
			continue
		}
		u, err := k.cfg.Sizes(ctx, paths...)
		if err != nil {
			return api.Disk{}, err
		}
		d.Parts = append(d.Parts, api.DiskPart{Kind: "data", Name: group.name, Total: u.Total, Exclusive: u.Exclusive})
	}
	if part, ok := k.podmanUsage(ctx); ok {
		d.Parts = append(d.Parts, part)
	}
	return d, nil
}

// podmanUsage returns what Podman's images take, on the server's disk and
// not in the pool; false if the node has no Podman, or it doesn't say.
func (k *Keeper) podmanUsage(ctx context.Context) (api.DiskPart, bool) {
	if k.cfg.Podman == nil {
		return api.DiskPart{}, false
	}
	out, err := k.cfg.Podman(ctx, "system", "df", "--format", "json")
	if err != nil {
		if !errors.Is(err, exec.ErrNotFound) {
			k.log.WarnContext(ctx, "asking Podman what it stores failed", slog.Any("error", err))
		}
		return api.DiskPart{}, false
	}
	var rows []struct {
		Type           string
		RawSize        uint64
		RawReclaimable uint64
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		k.log.WarnContext(ctx, "reading what Podman stores failed", slog.Any("error", err))
		return api.DiskPart{}, false
	}
	for _, row := range rows {
		if row.Type == "Images" && row.RawSize > 0 {
			return api.DiskPart{
				Kind: "podman", Name: "Podman's images", Total: row.RawSize, Exclusive: row.RawReclaimable,
				Unused: row.RawReclaimable > 0,
				Note:   "on the server's disk, outside the pool: what image builds and installs left",
			}, true
		}
	}
	return api.DiskPart{}, false
}

// inUse reports whether something runs from image id: a box, whose root is
// an overlay on it, or T3's server, whose root it is. Each shows in a mount
// table. An image it can't tell of counts as in use.
func (k *Keeper) inUse(id string) bool {
	needle := []byte("/images/" + id + "/")
	procs, err := os.ReadDir(k.cfg.Proc)
	if err != nil {
		return true
	}
	seen := map[string]bool{}
	for _, p := range procs {
		if _, err := strconv.Atoi(p.Name()); err != nil {
			continue
		}
		// Most processes share a few mount namespaces.
		ns, err := os.Readlink(filepath.Join(k.cfg.Proc, p.Name(), "ns/mnt"))
		if err != nil || seen[ns] {
			continue // it has ended, or its table was read
		}
		seen[ns] = true
		table, err := os.ReadFile(filepath.Join(k.cfg.Proc, p.Name(), "mountinfo"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ESRCH) {
			return true
		}
		if bytes.Contains(table, needle) {
			return true
		}
	}
	return len(seen) == 0
}

// Free removes what nothing needs and returns what it removed: the images in
// the pool no tag points to and nothing runs from, Podman's images no
// container uses (the pool has its own copy of Fern's; the next build or
// install fetches again what it needs), the snapshots past their
// retention, and the quota groups of deleted environments. What it couldn't remove is in the answer, not an error.
func (k *Keeper) Free(ctx context.Context) (api.Freed, error) {
	return k.free(ctx, true)
}

// free is Free, with Podman's images or without them: they are on the
// server's disk, not in the pool, so removing them gives a pool that is low
// by itself nothing, and costs the next image build its layers.
func (k *Keeper) free(ctx context.Context, podman bool) (api.Freed, error) {
	var freed api.Freed
	imgs, err := image.List(k.images())
	if err != nil {
		return api.Freed{}, err
	}
	for _, img := range imgs {
		if len(img.Tags) > 0 || k.inUse(img.ID) {
			continue
		}
		if err := image.Remove(k.images(), img.ID); err != nil {
			freed.Failed = append(freed.Failed, err.Error())
			continue
		}
		freed.Removed = append(freed.Removed, fmt.Sprintf("image %.12s, which a tag pointed to before", img.ID))
	}
	if podman && k.cfg.Podman != nil {
		// Not the last hour's: an image build under way has made images that
		// no container uses yet, and its results wait to be loaded into the
		// pool. On the VPS (2026-11-13) a deploy's T3 build took the disk
		// low, and this removed the daemon's and the box's images it had
		// just built.
		out, err := k.cfg.Podman(ctx, "image", "prune", "--all", "--force", "--filter", "until="+podmanKeep.String())
		switch n := len(bytes.Fields(out)); {
		case errors.Is(err, exec.ErrNotFound):
		case err != nil:
			freed.Failed = append(freed.Failed, err.Error())
		case n > 0:
			freed.Removed = append(freed.Removed, fmt.Sprintf("%d of Podman's images, which no container used", n))
		}
	}
	if err := k.envs.PruneAll(ctx); err != nil {
		freed.Failed = append(freed.Failed, "applying retention to the environments: "+err.Error())
	}
	// 40 bytes each, counting nothing anyone writes: only so they don't
	// pile up.
	if n, err := k.pool.ClearGroups(ctx); err != nil {
		freed.Failed = append(freed.Failed, "destroying deleted environments' quota groups: "+err.Error())
	} else if n > 0 {
		freed.Removed = append(freed.Removed, fmt.Sprintf("%d quota groups of deleted environments", n))
	}
	sp, err := k.pool.Space()
	if err != nil {
		return api.Freed{}, err
	}
	d := spaceOf(sp)
	freed.Pool, freed.Under = d.Pool, d.Under
	return freed, nil
}

func spaceOf(sp store.Space) api.Disk {
	d := api.Disk{Pool: api.Volume(sp.Pool), Least: sp.Least}
	if sp.Under != nil {
		under := api.Volume(*sp.Under)
		d.Under = &under
	}
	return d
}

// again is how long Run waits after freeing before it frees again: a disk
// that is still low then has nothing left that Free removes.
const again = time.Hour

// Run frees what nothing needs whenever the pool, or the disk under it, is
// low at one of its looks, every interval, until ctx is done: below twice
// what the store refuses new snapshots at, and for the disk under the pool
// also below a tenth of it. A thread can write gigabytes in a minute, so
// the looks are frequent; they cost a statfs.
func (k *Keeper) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		sp, err := k.pool.Space()
		if err != nil {
			k.log.WarnContext(ctx, "reading the pool's free space failed", slog.Any("error", err))
			continue
		}
		poolLow, underLow := low(sp)
		if !poolLow && !underLow || !last.IsZero() && time.Since(last) < again {
			continue
		}
		last = time.Now()
		freed, err := k.free(ctx, underLow)
		if err != nil {
			k.log.WarnContext(ctx, "freeing disk space failed", slog.Any("error", err))
			continue
		}
		attrs := []any{
			slog.Bool("pool_low", poolLow), slog.Bool("disk_low", underLow),
			slog.Any("removed", freed.Removed), slog.Any("failed", freed.Failed),
			slog.Uint64("pool_free", freed.Pool.Free), slog.Uint64("pool_least", sp.Least),
		}
		if len(freed.Removed) == 0 {
			// Until 2026-11-13 this said "freed what nothing needs" once an
			// hour, having freed nothing, and pruned Podman each time.
			k.log.WarnContext(ctx, "the disk is low and nothing is left to free: delete threads that are done, or grow the pool; "+
				"fern df shows what uses it, and below pool_least new snapshots are refused", attrs...)
			continue
		}
		k.log.WarnContext(ctx, "the disk was low: freed what nothing needs", attrs...)
	}
}

// low reports whether the pool, and whether the disk under it, is close to
// where the store refuses new snapshots.
func low(sp store.Space) (pool, under bool) {
	return sp.Pool.Free < 2*sp.Least, sp.Under != nil && sp.Under.Free < max(2*sp.Least, sp.Under.Total/10)
}
