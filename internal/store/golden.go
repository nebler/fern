package store

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// A golden snapshot is a snapshot of a builder environment's workspace that
// holds only build output (architecture §7.5): a build copies a checkout in,
// runs it, and strips what it copied, so that what is left is what the
// build made, such as .fern/pg or node_modules. A new environment's
// workspace is then adopted onto a copy of it (adopt, overlay). What a build
// makes came from the repository's code, so it is the agent's: every path
// into it goes through an os.Root, which follows no symlink out of it.

// sourceList is the builder environment's list of the files the last build
// copied in, NUL-separated, which StripSource removes. A build writes it
// before it copies, so a build that stopped half-way is stripped too.
func (s *Store) sourceList(id string) string { return filepath.Join(s.envDir(id), "source.list") }

// BuildSource makes builder environment id's workspace a fresh copy of
// snapshot src, a checkout, and lists its files for StripSource. What an
// earlier build left in the workspace goes: it could hold symlinks that
// lead a copy elsewhere. The box must be asleep. After a crash at any step,
// Reconcile leaves a workspace, and BuildSource can be run again.
func (s *Store) BuildSource(ctx context.Context, id string, src SnapshotRef) error {
	env, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if env.Workspace != s.livePath(id) {
		return fmt.Errorf("build source for %s: a builder's workspace is envs/<id>/live: %w", id, ErrInvalidPath)
	}
	from, err := s.snapshotPath(ctx, src)
	if err != nil {
		return err
	}
	if err := s.listSource(id, from); err != nil {
		return fmt.Errorf("build source for %s: %w", id, err)
	}
	if err := s.step("build source: listed"); err != nil {
		return err
	}
	if err := s.deleteSubvolumes(ctx, env.Workspace); err != nil {
		return err
	}
	if err := s.step("build source: cleared"); err != nil {
		return err
	}
	// A writable snapshot preserves the checkout's metadata and shares its
	// tree as well as its extents. A file-by-file reflink copy would still
	// allocate one inode per source file on every build.
	if err := snapshotSubvolume(ctx, from, env.Workspace, false); err != nil {
		return err
	}
	return s.step("build source: made")
}

// listSource writes the files and symlinks in dir, a read-only snapshot, as
// id's source list, replacing the last build's.
func (s *Store) listSource(id, dir string) error {
	tmp := s.sourceList(id) + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if _, err := w.WriteString(rel); err != nil {
			return err
		}
		return w.WriteByte(0)
	})
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if err := errors.Join(err, f.Close()); err != nil {
		return err
	}
	return os.Rename(tmp, s.sourceList(id))
}

// StripSource removes from builder environment id's workspace the files its
// last build copied in, keeping what it made, and clears the list.
func (s *Store) StripSource(ctx context.Context, id string) error {
	env, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(s.sourceList(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(env.Workspace)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for rel := range strings.SplitSeq(string(data), "\x00") {
		if rel == "" || !filepath.IsLocal(rel) {
			continue
		}
		// A build may have replaced a file with a directory; then it is the
		// build's.
		if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) && !isNotEmpty(err) {
			return fmt.Errorf("strip %s from %s: %w", rel, id, err)
		}
	}
	return os.Remove(s.sourceList(id))
}

func isNotEmpty(err error) bool { return errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) }

// Sources returns the golden sources kept for builds: snapshots named name,
// of any environment. A build queued when the daemon stopped is found here.
func (s *Store) Sources(ctx context.Context, name string) ([]SnapshotRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT env FROM snapshots WHERE id = ? ORDER BY created_at, rowid`, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var refs []SnapshotRef
	for rows.Next() {
		ref := SnapshotRef{Snapshot: name}
		if err := rows.Scan(&ref.Env); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// overlay copies the tree at src, a worktree, into dst, a writable copy of
// a golden snapshot, replacing what is there: files as reflinks, with their
// owners, modes and times. Both are opened as os.Roots: dst came from a
// build, and src can change while it is copied, so a symlink in either can't
// lead the copy, as root, to a file outside them.
func overlay(dst, src string) error {
	to, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer func() { _ = to.Close() }()
	from, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer func() { _ = from.Close() }()
	return fs.WalkDir(from.FS(), ".", func(rel string, _ fs.DirEntry, err error) error {
		if err != nil || rel == "." {
			return err
		}
		fi, err := from.Lstat(rel)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			if err := to.Mkdir(rel, fi.Mode().Perm()); errors.Is(err, fs.ErrExist) {
				// Keep a directory the build made; replace anything else.
				if cur, err := to.Lstat(rel); err != nil {
					return err
				} else if !cur.IsDir() {
					if err := to.RemoveAll(rel); err != nil {
						return err
					}
					if err := to.Mkdir(rel, fi.Mode().Perm()); err != nil {
						return err
					}
				}
			} else if err != nil {
				return err
			}
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := from.Readlink(rel)
			if err != nil {
				return err
			}
			if err := to.RemoveAll(rel); err != nil {
				return err
			}
			if err := to.Symlink(target, rel); err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			if fi, err = cloneFile(to, from, rel); err != nil || fi == nil {
				return err
			}
		default:
			return nil // sockets, pipes and devices aren't checkout files
		}
		return setAttrs(to, rel, fi)
	})
}

// cloneFile makes rel in to a reflink of rel in from, and returns what it
// cloned, or nil if rel is no longer a regular file.
func cloneFile(to, from *os.Root, rel string) (fs.FileInfo, error) {
	in, err := from.OpenFile(rel, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ELOOP) {
		return nil, nil // replaced by a symlink since the walk saw it
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = in.Close() }()
	fi, err := in.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, err
	}
	if err := to.RemoveAll(rel); err != nil {
		return nil, err
	}
	out, err := to.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, fi.Mode().Perm())
	if err != nil {
		return nil, err
	}
	err = unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	return fi, errors.Join(err, out.Close())
}

// setAttrs gives rel in root fi's owner and, unless it is a symlink, mode
// and times.
func setAttrs(root *os.Root, rel string, fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: no owner in its file info", rel)
	}
	if err := root.Lchown(rel, int(st.Uid), int(st.Gid)); err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return nil
	}
	if err := root.Chmod(rel, fi.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)); err != nil {
		return err
	}
	return root.Chtimes(rel, fi.ModTime(), fi.ModTime())
}
