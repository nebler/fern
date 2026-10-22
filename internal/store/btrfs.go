package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func btrfs(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "btrfs", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// deleteSubvolumes deletes those of paths that are subvolumes, with any
// subvolumes a box made inside them, in one btrfs call. Paths that don't
// exist or aren't subvolumes are left alone.
func deleteSubvolumes(ctx context.Context, paths ...string) error {
	args := []string{"subvolume", "delete", "--recursive"}
	n := len(args)
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if isSubvolume(fi) {
			args = append(args, p)
		}
	}
	if len(args) == n {
		return nil
	}
	return btrfs(ctx, args...)
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
