package store

import (
	"bytes"
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

func btrfs(ctx context.Context, args ...string) error {
	if err := run(exec.CommandContext(ctx, "btrfs", args...)); err != nil {
		return fmt.Errorf("btrfs %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// run runs cmd and, if it fails, returns an error quoting the start of its
// output. The rest is counted, not kept: an archive's untar can print a
// line per file, and the daemon must not hold all of it.
func run(cmd *exec.Cmd) error {
	out := &headBuffer{max: maxOutput}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// maxOutput is how much of a failed command's output its error quotes.
const maxOutput = 4 << 10

// headBuffer keeps the first max bytes written to it and counts the rest.
// As a command's Stdout and Stderr both, os/exec writes to it from one
// goroutine at a time.
type headBuffer struct {
	max     int
	buf     []byte
	dropped int
}

func (b *headBuffer) Write(p []byte) (int, error) {
	n := min(len(p), b.max-len(b.buf))
	b.buf = append(b.buf, p[:n]...)
	b.dropped += len(p) - n
	return len(p), nil
}

func (b *headBuffer) String() string {
	s := string(bytes.TrimSpace(b.buf))
	if b.dropped > 0 {
		return fmt.Sprintf("%s… (%d more bytes)", s, b.dropped)
	}
	return s
}

// deleteSubvolumes deletes those of paths that are subvolumes, with any
// subvolumes a box made inside them. Paths that don't exist or aren't
// subvolumes are left alone.
func (s *Store) deleteSubvolumes(ctx context.Context, paths ...string) error {
	var nested []string
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !isSubvolume(fi) {
			continue
		}
		// Most trees have no nested subvolumes. Try the deletion directly rather
		// than walking btrfs's root tree for every snapshot. A failed ENOTEMPTY
		// leaves the tree intact; btrfs-progs handles the uncommon recursive case.
		if err := deleteSubvolume(ctx, p); errors.Is(err, unix.ENOTEMPTY) {
			nested = append(nested, p)
		} else if err != nil {
			return err
		} else if err := s.step("delete subvolume: " + filepath.Base(p)); err != nil {
			return err
		}
	}
	if len(nested) == 0 {
		return nil
	}
	if err := btrfs(ctx, append([]string{"subvolume", "delete", "--recursive"}, nested...)...); err != nil {
		return err
	}
	return s.step("delete subvolumes: nested removed")
}

// isSubvolume reports whether fi describes the root of a btrfs subvolume,
// which always has inode 256.
func isSubvolume(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && fi.IsDir() && st.Ino == 256
}

// syncfs flushes the filesystem holding dir.
func syncfs(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return unix.Syncfs(int(f.Fd()))
}
