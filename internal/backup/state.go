package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // the "sqlite" driver, to copy T3's databases
)

// The state archive holds, besides the workspaces: the index as indexName,
// T3's folders and its databases' copies, and each environment's Claude
// folder.
const indexName = "fern.sqlite"

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

// writeState writes the state archive to w: index as indexName, the pool's
// stateDirs with T3's databases replaced by their copies (by their path in
// userdata), and envs/*/claude.
func writeState(w io.Writer, pool, index string, t3dbs map[string]string) error {
	tw := tar.NewWriter(w)
	if err := addFile(tw, index, indexName); err != nil {
		return err
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

// Restore brings node's latest backup in cfg's bucket into pool, which must
// hold no environments: each workspace, received from its sends, and the
// state around them. The daemon and T3 must be stopped.
func Restore(ctx context.Context, cfg *Config, node, pool string, log *slog.Logger) error {
	if entries, err := os.ReadDir(filepath.Join(pool, "envs")); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s has environments already: restore into a new server's pool", pool)
	}
	m, err := readManifest(ctx, cfg, nodePrefix(node))
	if err != nil {
		return err
	}
	if m.State == "" {
		return fmt.Errorf("no backup of %s in the bucket", node)
	}
	for id, eb := range m.Envs {
		snaps := filepath.Join(pool, "envs", id, "snaps")
		if err := os.MkdirAll(snaps, 0o700); err != nil {
			return err
		}
		for _, send := range eb.Chain {
			if err := receive(ctx, cfg, send.Key, snaps); err != nil {
				return fmt.Errorf("restore %s: %w", id, err)
			}
		}
		last := filepath.Join(snaps, eb.Chain[len(eb.Chain)-1].Snapshot)
		if !filepath.IsLocal(eb.Workspace) {
			return fmt.Errorf("restore %s: workspace %q isn't in the pool", id, eb.Workspace)
		}
		workspace := filepath.Join(pool, eb.Workspace)
		if err := os.MkdirAll(filepath.Dir(workspace), 0o755); err != nil {
			return err
		}
		if out, err := exec.CommandContext(ctx, "btrfs", "subvolume", "snapshot", last, workspace).CombinedOutput(); err != nil {
			return fmt.Errorf("restore %s's workspace: %w: %s", id, err, out)
		}
		log.InfoContext(ctx, "restored a workspace", slog.String("env", id), slog.String("workspace", workspace),
			slog.Int("sends", len(eb.Chain)))
	}
	// A new node's daemon made an empty index at its first start.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(pool, "state", indexName+suffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := extractState(ctx, cfg, m.State, pool); err != nil {
		return fmt.Errorf("restore the state: %w", err)
	}
	log.InfoContext(ctx, "restored the backup", slog.Time("of", m.Time), slog.Int("envs", len(m.Envs)))
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

// receive receives the send stream key into folder dir.
func receive(ctx context.Context, cfg *Config, key, dir string) error {
	r, closeIt, err := openObject(ctx, cfg, key)
	if err != nil {
		return err
	}
	defer closeIt()
	cmd := exec.CommandContext(ctx, "btrfs", "receive", "-q", dir)
	cmd.Stdin = r
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("btrfs receive %s: %w: %s", key, err, out)
	}
	return nil
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
		if name == indexName {
			dst = filepath.Join(pool, "state", indexName)
		}
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
