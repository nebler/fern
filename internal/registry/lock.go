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

// Lease is an exclusive advisory lock on one workspace, held via a flock on a
// private lock file that records the owning host and process.
type Lease struct {
	file *os.File
}

// Acquire takes the exclusive workspace lease in directory, creating and
// locking its lock file if necessary. It fails if the lock is held (reporting
// the recorded holder), if the path is not a singly linked regular private
// file, or if holder metadata cannot be written durably. The returned Lease
// must be released; Release is nil-safe.
func Acquire(directory, workspace string) (*Lease, error) {
	if err := ensurePrivateDirectory(directory, "lock"); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, fmt.Sprintf("%x.lock", sha256.Sum256([]byte(workspace))))
	// LOCK_NB below makes contention fail fast, so O_NONBLOCK is unnecessary.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
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
		data, readErr := io.ReadAll(io.LimitReader(file, 4<<10))
		file.Close()
		if readErr != nil {
			return nil, fmt.Errorf("workspace %q lease file unreadable: %w", workspace, readErr)
		}
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

func ensurePrivateDirectory(directory, purpose string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create %s directory: %w", purpose, err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("inspect %s directory: %w", purpose, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s directory must be a private real directory", purpose)
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(directory, 0o700); err != nil {
			return fmt.Errorf("make %s directory private: %w", purpose, err)
		}
	}
	return nil
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
