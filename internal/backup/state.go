package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	_ "modernc.org/sqlite" // the "sqlite" driver, to copy T3's databases

	"github.com/nebler/fern/internal/store"
)

// The state archive holds, besides the workspaces: each environment's record,
// T3's folders and its databases' copies, and each environment's Claude
// folder.

// stateDirs are the pool's folders the archive holds whole: T3's data and
// the projects' repositories, which T3's worktrees point into.
var stateDirs = []string{"userdata", "repos", "projects"}

// lockFile is T3's lock on its state folder: a SQLite file it holds an
// exclusive transaction on while it runs, with no data. It isn't backed up.
const lockFile = "server-owner.sqlite"

// isDatabase says whether a file of T3's is part of a SQLite database,
// which goes into the archive as SQLite copies it instead.
func isDatabase(name string) bool {
	for _, suffix := range []string{".sqlite", ".sqlite-journal", ".sqlite-wal", ".sqlite-shm", ".db", ".db-wal", ".db-shm"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// copyDatabases copies the SQLite databases under dir into tmp with VACUUM
// INTO, while T3 may be writing them, and returns the copies by their path
// relative to dir.
func copyDatabases(ctx context.Context, dir, tmp string) (map[string]string, error) {
	copies := make(map[string]string)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() || (!strings.HasSuffix(p, ".sqlite") && !strings.HasSuffix(p, ".db")) || d.Name() == lockFile {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(tmp, fmt.Sprintf("db-%d", len(copies)))
		if err := vacuumInto(ctx, p, dst); err != nil {
			return fmt.Errorf("copy %s: %w", p, err)
		}
		copies[rel] = dst
		return nil
	})
	return copies, err
}

func vacuumInto(ctx context.Context, src, dst string) error {
	db, err := sql.Open("sqlite", "file:"+src+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(ctx, "VACUUM INTO ?", dst)
	return err
}

// writeState writes the state archive to w: the environments' records, the
// pool's stateDirs with T3's databases replaced by their copies (by their
// path in userdata), and envs/*/claude. A record is replaced by a rename, so
// one read while the daemon runs is whole.
func writeState(w io.Writer, pool string, t3dbs map[string]string) error {
	tw := tar.NewWriter(w)
	records, err := filepath.Glob(filepath.Join(pool, "envs", "*", store.RecordFile))
	if err != nil {
		return err
	}
	for _, r := range records {
		rel, err := filepath.Rel(pool, r)
		if err != nil {
			return err
		}
		if err := addFile(tw, r, rel); err != nil {
			return err
		}
	}
	for _, dir := range stateDirs {
		if err := addTree(tw, pool, dir, dir == "userdata"); err != nil {
			return err
		}
	}
	for rel, copied := range t3dbs {
		if err := addFile(tw, copied, filepath.Join("userdata", rel)); err != nil {
			return err
		}
	}
	claudes, err := filepath.Glob(filepath.Join(pool, "envs", "*", "claude"))
	if err != nil {
		return err
	}
	for _, c := range claudes {
		rel, err := filepath.Rel(pool, c)
		if err != nil {
			return err
		}
		if err := addTree(tw, pool, rel, false); err != nil {
			return err
		}
	}
	return tw.Close()
}

// addFile adds file src to tw as name.
func addFile(tw *tar.Writer, src, name string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return err
	}
	hdr.Name = name
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// addTree adds folder rel of pool to tw, with owners and modes; skipDBs
// leaves out SQLite files, which go in as copies.
func addTree(tw *tar.Writer, pool, rel string, skipDBs bool) error {
	return filepath.WalkDir(filepath.Join(pool, rel), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if skipDBs && isDatabase(d.Name()) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		case !fi.Mode().IsRegular() && !fi.IsDir():
			return nil // sockets, pipes: nothing to keep
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		if hdr.Name, err = filepath.Rel(pool, p); err != nil {
			return err
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		// A file that grew since its header was written is cut to its size.
		_, err = io.CopyN(tw, f, hdr.Size)
		return err
	})
}

// RestoringFile is in a pool while a restore into it is under way, from
// before its first change to after its last: the daemon doesn't start on a
// pool that has it, and a restore that finds it finishes the one that
// stopped.
const RestoringFile = "restoring.json"

// restoring is RestoringFile: the backup being restored.
type restoring struct {
	Node  string `json:"node"`
	State string `json:"state"` // the manifest's state archive, which names the backup
}

// receiver makes the pool's subvolumes from send streams: btrfs on a node,
// a fake in tests.
type receiver interface {
	// Receive receives the send stream r into folder dir.
	Receive(ctx context.Context, r io.Reader, dir string) error
	// Snapshot makes writable snapshot dst of src.
	Snapshot(ctx context.Context, src, dst string) error
	// Delete deletes subvolume dir.
	Delete(ctx context.Context, dir string) error
}

// Restore brings node's latest backup in cfg's bucket into pool, which must
// hold no environments: each workspace, received from its sends, and the
// state around them. The daemon and T3 must be stopped. A restore that
// stopped, whatever the step, is finished by running it again.
func Restore(ctx context.Context, cfg *Config, node, pool string, log *slog.Logger) error {
	r := restorer{cfg: cfg, pool: pool, vol: btrfs{}, log: log}
	return r.restore(ctx, node)
}

// restorer restores a backup into a pool.
type restorer struct {
	cfg  *Config
	pool string
	vol  receiver
	log  *slog.Logger
	// failAt, if set, is called at every step, which stops with its error:
	// the tests' crash at that step.
	failAt func(step string) error
}

func (r restorer) step(name string) error {
	if r.failAt == nil {
		return nil
	}
	return r.failAt(name)
}

func (r restorer) restore(ctx context.Context, node string) error {
	m, err := readManifest(ctx, r.cfg, nodePrefix(node))
	if err != nil {
		return err
	}
	if m.State == "" {
		return fmt.Errorf("no backup of %s in the bucket", node)
	}
	marker := filepath.Join(r.pool, RestoringFile)
	want := restoring{Node: node, State: m.State}
	var was restoring
	data, err := os.ReadFile(marker)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &was); err != nil {
			return fmt.Errorf("%s: %w", marker, err)
		}
		if was != want {
			return fmt.Errorf("a restore of %s's backup into %s stopped, and the bucket has had another backup of %s since: restore into a new server's pool", was.Node, r.pool, node)
		}
		r.log.InfoContext(ctx, "finishing a restore that stopped", slog.String("pool", r.pool))
	case errors.Is(err, fs.ErrNotExist):
		if entries, err := os.ReadDir(filepath.Join(r.pool, "envs")); err == nil && len(entries) > 0 {
			return fmt.Errorf("%s has environments already: restore into a new server's pool", r.pool)
		}
		if err := writeRestoring(marker, want); err != nil {
			return err
		}
	default:
		return err
	}
	if err := r.step("marker"); err != nil {
		return err
	}
	for _, id := range slices.Sorted(maps.Keys(m.Envs)) {
		if err := r.env(ctx, id, m.Envs[id]); err != nil {
			return fmt.Errorf("restore %s: %w", id, err)
		}
	}
	// The archive's files are written whole each time: one cut short is
	// written again.
	if err := r.step("state"); err != nil {
		return err
	}
	if err := extractState(ctx, r.cfg, m.State, r.pool); err != nil {
		return fmt.Errorf("restore the state: %w", err)
	}
	if err := r.step("done"); err != nil {
		return err
	}
	if err := os.Remove(marker); err != nil {
		return err
	}
	r.log.InfoContext(ctx, "restored the backup", slog.Time("of", m.Time), slog.Int("envs", len(m.Envs)))
	return nil
}

// writeRestoring writes the marker of a restore of v, in one rename.
func writeRestoring(marker string, v restoring) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(marker+".new", append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(marker+".new", marker)
}

// env restores environment id's workspace from eb. Its sends are received
// into envs/<id>/snaps.part, which is renamed to snaps once they all are,
// and the workspace is a snapshot of the last: snaps.part says the receive
// stopped (it is deleted and received again), snaps without the workspace
// that the snapshot is left to take, and both that id is done.
func (r restorer) env(ctx context.Context, id string, eb EnvBackup) error {
	if len(eb.Chain) == 0 {
		return errors.New("its backup has no sends")
	}
	if !filepath.IsLocal(eb.Workspace) {
		return fmt.Errorf("workspace %q isn't in the pool", eb.Workspace)
	}
	last := eb.Chain[len(eb.Chain)-1].Snapshot
	if !filepath.IsLocal(last) || filepath.Base(last) != last {
		return fmt.Errorf("snapshot %q isn't a name", last)
	}
	dir := filepath.Join(r.pool, "envs", id)
	snaps, part := filepath.Join(dir, "snaps"), filepath.Join(dir, "snaps.part")
	workspace := filepath.Join(r.pool, eb.Workspace)
	haveSnaps, err := exists(snaps)
	if err != nil {
		return err
	}
	if !haveSnaps {
		if err := r.clear(ctx, part); err != nil {
			return err
		}
		if err := os.MkdirAll(part, 0o700); err != nil {
			return err
		}
		for i, send := range eb.Chain {
			if err := r.step(fmt.Sprintf("receive %s %d", id, i)); err != nil {
				return err
			}
			if err := r.receive(ctx, send.Key, part); err != nil {
				return err
			}
		}
		if err := r.step("snaps " + id); err != nil {
			return err
		}
		if err := os.Rename(part, snaps); err != nil {
			return err
		}
	}
	switch done, err := exists(workspace); {
	case err != nil:
		return err
	case done && haveSnaps:
		return nil
	case done:
		return fmt.Errorf("workspace %s is there already", workspace)
	}
	if err := r.step("workspace " + id); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(workspace), 0o755); err != nil {
		return err
	}
	if err := r.vol.Snapshot(ctx, filepath.Join(snaps, last), workspace); err != nil {
		return fmt.Errorf("its workspace: %w", err)
	}
	r.log.InfoContext(ctx, "restored a workspace", slog.String("env", id), slog.String("workspace", workspace),
		slog.Int("sends", len(eb.Chain)))
	return nil
}

// clear deletes folder dir of received subvolumes, if it is there: what a
// receive that stopped left, one subvolume perhaps half received.
func (r restorer) clear(ctx context.Context, dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := r.vol.Delete(ctx, filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return os.Remove(dir)
}

// receive receives the send stream key into folder dir.
func (r restorer) receive(ctx context.Context, key, dir string) error {
	obj, closeIt, err := openObject(ctx, r.cfg, key)
	if err != nil {
		return err
	}
	defer closeIt()
	if err := r.vol.Receive(ctx, obj, dir); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

// exists says whether path is there.
func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Receive runs btrfs receive.
func (btrfs) Receive(ctx context.Context, r io.Reader, dir string) error {
	cmd := exec.CommandContext(ctx, "btrfs", "receive", "-q", dir)
	cmd.Stdin = r
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("btrfs receive: %w: %s", err, out)
	}
	return nil
}

// Snapshot runs btrfs subvolume snapshot.
func (btrfs) Snapshot(ctx context.Context, src, dst string) error {
	if out, err := exec.CommandContext(ctx, "btrfs", "subvolume", "snapshot", src, dst).CombinedOutput(); err != nil {
		return fmt.Errorf("btrfs subvolume snapshot: %w: %s", err, out)
	}
	return nil
}

// Delete runs btrfs subvolume delete.
func (btrfs) Delete(ctx context.Context, dir string) error {
	if out, err := exec.CommandContext(ctx, "btrfs", "subvolume", "delete", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("btrfs subvolume delete: %w: %s", err, out)
	}
	return nil
}

// open returns object key, decrypted and decompressed; close closes it.
func openObject(ctx context.Context, cfg *Config, key string) (io.Reader, func(), error) {
	r, err := cfg.S3.Get(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	plain, err := Decrypt(cfg.Key, r)
	if err != nil {
		_ = r.Close()
		return nil, nil, err
	}
	zr, err := gzip.NewReader(plain)
	if err != nil {
		_ = r.Close()
		return nil, nil, err
	}
	return zr, func() { _ = r.Close() }, nil
}

// extractState unpacks the state archive key into pool.
func extractState(ctx context.Context, cfg *Config, key, pool string) error {
	r, closeIt, err := openObject(ctx, cfg, key)
	if err != nil {
		return err
	}
	defer closeIt()
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("the archive names %q", hdr.Name)
		}
		dst := filepath.Join(pool, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		mode := fs.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			_ = os.Remove(dst)
			if err := os.Symlink(hdr.Linkname, dst); err != nil {
				return err
			}
		case tar.TypeReg:
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			continue
		}
		if err := os.Lchown(dst, hdr.Uid, hdr.Gid); err != nil {
			return err
		}
	}
}
