//go:build darwin || linux

package verification

import (
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const maxExecutableBytes = 512 << 20

func inspectExecutable(path string) (executableIdentity, error) {
	file, err := openExecutable(path)
	if err != nil {
		return executableIdentity{}, err
	}
	defer file.Close()
	return identityOf(file)
}

func openPinnedExecutable(path string, expected executableIdentity) (*os.File, error) {
	file, err := openExecutable(path)
	if err != nil {
		return nil, err
	}
	actual, err := identityOf(file)
	if err != nil || actual != expected {
		file.Close()
		return nil, errors.New("executable identity changed")
	}
	return file, nil
}

func openExecutable(path string) (*os.File, error) {
	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	if len(components) == 0 || components[len(components)-1] == "" {
		return nil, errors.New("invalid executable path")
	}
	directory, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(directory) }()
	if err := validateDirectory(directory); err != nil {
		return nil, err
	}
	for _, component := range components[:len(components)-1] {
		next, openErr := unix.Openat(directory, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			return nil, openErr
		}
		unix.Close(directory)
		directory = next
		if err := validateDirectory(directory); err != nil {
			return nil, err
		}
	}
	fd, err := unix.Openat(directory, components[len(components)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		file.Close()
		return nil, errors.New("executable is not a trusted host file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !trustedOwner(uint32(stat.Uid)) || info.Mode().Perm()&022 != 0 ||
		(uint32(stat.Uid) == uint32(os.Geteuid()) && info.Mode().Perm()&0200 != 0) {
		file.Close()
		return nil, errors.New("executable is not a trusted host file")
	}
	return file, nil
}

func validateDirectory(fd int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || uint32(stat.Mode)&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("invalid executable parent")
	}
	mode := uint32(stat.Mode)
	if !trustedOwner(uint32(stat.Uid)) {
		return errors.New("untrusted executable parent owner")
	}
	if mode&(unix.S_IWGRP|unix.S_IWOTH) != 0 &&
		!(uint32(stat.Uid) == 0 && uint32(os.Geteuid()) != 0 && mode&unix.S_ISVTX != 0) {
		return errors.New("writable executable parent")
	}
	return nil
}

func trustedOwner(uid uint32) bool {
	return uid == 0 || uid == uint32(os.Geteuid())
}

func identityOf(file *os.File) (executableIdentity, error) {
	before, err := metadataOf(file)
	if err != nil {
		return executableIdentity{}, err
	}
	if before.size <= 0 || before.size > maxExecutableBytes {
		return executableIdentity{}, errors.New("executable size is not allowed")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return executableIdentity{}, err
	}
	hasher := sha256.New()
	header := make([]byte, 8)
	reader := io.TeeReader(io.LimitReader(file, before.size+1), hasher)
	n, err := io.ReadFull(reader, header)
	if err != nil {
		return executableIdentity{}, errors.New("executable header is unavailable")
	}
	written, err := io.Copy(io.Discard, reader)
	if err != nil || int64(n)+written != before.size {
		return executableIdentity{}, errors.New("executable changed while hashing")
	}
	after, err := metadataOf(file)
	if err != nil || after != before {
		return executableIdentity{}, errors.New("executable metadata changed while hashing")
	}
	if !isNativeExecutable(header) {
		return executableIdentity{}, errors.New("verification executable must be a native binary")
	}
	copy(before.sha256[:], hasher.Sum(nil))
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return executableIdentity{}, err
	}
	return before, nil
}

func metadataOf(file *os.File) (executableIdentity, error) {
	info, err := file.Stat()
	if err != nil {
		return executableIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return executableIdentity{}, errors.New("executable identity unavailable")
	}
	return executableIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino), size: info.Size(),
		mode: uint32(stat.Mode), uid: uint32(stat.Uid), gid: uint32(stat.Gid)}, nil
}
