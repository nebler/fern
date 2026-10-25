package store

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
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

// sourceList is the builder environment's list of the files a build copied
// in, which StripSource removes. A build adds to it before it copies, so a
// build that stopped half-way is stripped too.
func (s *Store) sourceList(id string) string { return filepath.Join(s.envDir(id), "source.list") }

// BuildSource makes builder environment id's workspace a fresh copy of
// snapshot src, a checkout, and lists its files for StripSource. What an
// earlier build left in the workspace goes: it could hold symlinks that
// lead a copy elsewhere. The box must be asleep.
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
	if err := deleteSubvolumes(ctx, env.Workspace); err != nil {
		return err
	}
	if err := s.makeWorkspace(ctx, id, env.Workspace, ""); err != nil {
		return err
	}
	// The workspace is new and empty, so nothing in it can lead the copy out.
	if err := run(exec.CommandContext(ctx, "cp", "-a", "--reflink=always", "-T", from, env.Workspace)); err != nil {
		return fmt.Errorf("build source for %s: cp: %w", id, err)
	}
	return nil
}

// listSource adds the files and symlinks in dir to id's source list.
func (s *Store) listSource(id, dir string) error {
	f, err := os.OpenFile(s.sourceList(id), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
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
		_, err = fmt.Fprintln(w, rel)
		return err
	})
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// StripSource removes from builder environment id's workspace the files its
// builds copied in, keeping what they made, and clears the list.
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
	for rel := range strings.Lines(string(data)) {
		rel = strings.TrimSuffix(rel, "\n")
		if !filepath.IsLocal(rel) {
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

// overlay copies the tree at src into dst, a writable copy of a golden
// snapshot, replacing what is there: files as reflinks, with their owners,
// modes and times. dst came from a build, so it is opened as an os.Root:
// a symlink in it can't lead the copy out of it.
func overlay(dst, src string) error {
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if err := root.Mkdir(rel, fi.Mode().Perm()); errors.Is(err, fs.ErrExist) {
				// Keep a directory the build made; replace anything else.
				if cur, err := root.Lstat(rel); err != nil {
					return err
				} else if !cur.IsDir() {
					if err := root.RemoveAll(rel); err != nil {
						return err
					}
					if err := root.Mkdir(rel, fi.Mode().Perm()); err != nil {
						return err
					}
				}
			} else if err != nil {
				return err
			}
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := root.RemoveAll(rel); err != nil {
				return err
			}
			if err := root.Symlink(target, rel); err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			if err := cloneFile(root, rel, path, fi); err != nil {
				return err
			}
		default:
			return nil // sockets, pipes and devices aren't checkout files
		}
		return setAttrs(root, rel, fi)
	})
}

// cloneFile makes rel in root a reflink of the file at path.
func cloneFile(root *os.Root, rel, path string, fi fs.FileInfo) error {
	if err := root.RemoveAll(rel); err != nil {
		return err
	}
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, fi.Mode().Perm())
	if err != nil {
		return err
	}
	err = unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	return errors.Join(err, out.Close())
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
