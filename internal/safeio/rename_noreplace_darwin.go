//go:build darwin

package safeio

import "golang.org/x/sys/unix"

// RenameNoReplace renames oldPath to newPath, failing if newPath exists.
func RenameNoReplace(oldPath, newPath string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, oldPath, unix.AT_FDCWD, newPath, unix.RENAME_EXCL)
}
