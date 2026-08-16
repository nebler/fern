package registry

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

type Lease struct {
	file *os.File
}

func Acquire(directory, workspace string) (*Lease, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect lock directory: %w", err)
	}
	if !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("lock directory must be a real directory")
	}
	path := filepath.Join(directory, fmt.Sprintf("%x.lock", sha256.Sum256([]byte(workspace))))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open workspace lock: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("inspect workspace lock: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 {
		file.Close()
		return nil, errors.New("workspace lock must be a singly linked regular file")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			file.Close()
			return nil, fmt.Errorf("lock workspace %q: %w", workspace, err)
		}
		_, _ = file.Seek(0, io.SeekStart)
		data, _ := io.ReadAll(io.LimitReader(file, 4<<10))
		file.Close()
		holder := string(data)
		if holder == "" {
			holder = "an unknown process"
		}
		return nil, fmt.Errorf("workspace %q is already managed by %s", workspace, holder)
	}
	hostname, err := os.Hostname()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("read hostname for workspace lock: %w", err)
	}
	holder := fmt.Sprintf("%s pid=%d", hostname, os.Getpid())
	if err := file.Truncate(0); err != nil {
		file.Close()
		return nil, fmt.Errorf("truncate workspace lock metadata: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, fmt.Errorf("seek workspace lock metadata: %w", err)
	}
	if _, err := file.WriteString(holder); err != nil {
		file.Close()
		return nil, fmt.Errorf("write workspace lock metadata: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return nil, fmt.Errorf("sync workspace lock metadata: %w", err)
	}
	return &Lease{file: file}, nil
}

func (l *Lease) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(err, closeErr)
}
