package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// SyncDir fsyncs the directory at path so renames and removals in it are
// durable.
func SyncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// Identity returns the (device, inode) pair of info, failing when the platform
// reports no durable identity.
func Identity(info os.FileInfo) (device, inode uint64, err error) {
	if info == nil {
		return 0, 0, errors.New("filesystem object has no durable identity")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Dev == 0 || stat.Ino == 0 {
		return 0, 0, errors.New("filesystem object has no durable identity")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}

// ErrChanged reports that a filesystem object no longer has the identity a
// caller proved earlier.
var ErrChanged = errors.New("filesystem object changed identity")

// QuarantineRemove renames the directory at path to quarantine (a fresh name
// in the same parent), proves the renamed object is still the one identified
// by device and inode, and only then removes it with RemoveTree. If the proof
// fails, it returns ErrChanged and leaves the quarantined object in place for
// review. A missing path returns an error matching os.ErrNotExist.
func QuarantineRemove(path, quarantine string, device, inode uint64) error {
	if err := RenameNoReplace(path, quarantine); err != nil {
		return err
	}
	info, err := os.Lstat(quarantine)
	if err == nil {
		var gotDevice, gotInode uint64
		gotDevice, gotInode, err = Identity(info)
		if err == nil && (gotDevice != device || gotInode != inode) {
			err = ErrChanged
		}
	}
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(quarantine), err)
	}
	parent := filepath.Dir(quarantine)
	if err := RemoveTree(parent, filepath.Base(quarantine)); err != nil {
		return err
	}
	return SyncDir(parent)
}

// RemoveTree removes name, and everything beneath it, inside directory. The
// removal is confined to directory by os.Root, so symlinks are removed rather
// than followed. A missing name is not an error.
func RemoveTree(directory, name string) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	return errors.Join(root.RemoveAll(name), root.Close())
}
