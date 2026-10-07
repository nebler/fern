//go:build linux

package safeio

import "golang.org/x/sys/unix"

// RenameNoReplace renames oldPath to newPath, failing if newPath exists.
func RenameNoReplace(oldPath, newPath string) error {
	return unix.Renameat2(unix.AT_FDCWD, oldPath, unix.AT_FDCWD, newPath, unix.RENAME_NOREPLACE)
}
