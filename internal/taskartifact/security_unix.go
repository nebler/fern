//go:build unix

package taskartifact

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func exactRegular(path string, executable bool) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("path is not exact and absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return nil, errors.New("path contains a symlink")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || executable && info.Mode()&0o111 == 0 {
		return nil, errors.New("path is not a regular file")
	}
	return info, nil
}

func exactDirectory(path string, private bool) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("path is not exact and absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return nil, errors.New("path contains a symlink")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("path is not a directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || private && info.Mode().Perm() != 0o700 {
		return nil, errors.New("directory owner or mode is unsafe")
	}
	return info, nil
}

func privateRoot(path string) error {
	_, err := exactDirectory(path, true)
	return err
}

func safeDirectoryInfo(info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func fileIdentity(info os.FileInfo) (uint64, uint64, error) {
	if info == nil {
		return 0, 0, errors.New("filesystem object has no durable identity")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Dev == 0 || stat.Ino == 0 {
		return 0, 0, errors.New("filesystem object has no durable identity")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}

func openPrivateExclusive(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func writePrivateFile(path string, value []byte) error {
	file, err := openPrivateExclusive(path)
	if err != nil {
		return err
	}
	writeErr := func() error {
		if _, err := file.Write(value); err != nil {
			return err
		}
		return file.Sync()
	}()
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}

func openPrivateRead(path string, mode os.FileMode, singleLink bool) (*os.File, os.FileInfo, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) ||
		mode != 0 && os.FileMode(stat.Mode&0o777) != mode || singleLink && stat.Nlink != 1 {
		_ = file.Close()
		return nil, nil, errors.New("opened file is unsafe")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func removeExactDirectory(path string, device, inode uint64) error {
	info, err := os.Lstat(path)
	if err != nil || !safeDirectoryInfo(info) {
		return fmt.Errorf("%w: removal target", ErrStorage)
	}
	currentDevice, currentInode, err := fileIdentity(info)
	if err != nil || currentDevice != device || currentInode != inode {
		return fmt.Errorf("%w: removal identity", ErrStorage)
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	quarantine := filepath.Join(filepath.Dir(path), ".remove-"+token)
	if err := renameNoReplace(path, quarantine); err != nil {
		return err
	}
	quarantined, err := os.Lstat(quarantine)
	qDevice, qInode, identityErr := fileIdentity(quarantined)
	if err != nil || identityErr != nil || qDevice != device || qInode != inode {
		return fmt.Errorf("%w: quarantined removal identity", ErrStorage)
	}
	if err := os.RemoveAll(quarantine); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
