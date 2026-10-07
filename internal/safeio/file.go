package safeio

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

var (
	// ErrTooLarge reports that ReadFile found more than maxBytes.
	ErrTooLarge = errors.New("file exceeds size limit")
	// ErrUnsafeDir reports that PrivateDir found a directory that is not a real,
	// owner-only directory owned by the current user.
	ErrUnsafeDir = errors.New("state directory must be a real directory owned by the current user with no group or other access")
)

// WriteFile atomically replaces path with data. It writes a temporary file in the
// same directory, syncs it, renames it over path, and syncs the directory.
// Failures before the rename leave path untouched; a failed directory sync
// after it means the new content is visible but may not survive a crash.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	return write(path, data, perm, os.Rename)
}

// WriteFileExclusive is WriteFile that fails, leaving the existing file untouched,
// if path already exists.
func WriteFileExclusive(path string, data []byte, perm os.FileMode) error {
	return write(path, data, perm, RenameNoReplace)
}

func write(path string, data []byte, perm os.FileMode, rename func(string, string) error) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	err = temporary.Chmod(perm)
	if err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = rename(temporaryPath, path)
	}
	if err != nil {
		return err
	}
	handle, err := os.Open(directory)
	if err == nil {
		err = errors.Join(handle.Sync(), handle.Close())
	}
	if err != nil {
		return fmt.Errorf("file replaced but directory sync failed: %w", err)
	}
	return nil
}

// ReadFile returns the contents of the regular file at path, failing with
// ErrTooLarge if it holds more than maxBytes. A missing file returns an error
// matching os.ErrNotExist.
func ReadFile(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s: %w (%d bytes)", path, ErrTooLarge, maxBytes)
	}
	return data, nil
}

// PrivateDir creates path (mode 0700) if needed and verifies it is a real
// directory, not a symlink, owned by the current user, with no group or other
// permission bits. This is the single trust check for files kept inside it.
func PrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return ErrUnsafeDir
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
		return ErrUnsafeDir
	}
	return nil
}
