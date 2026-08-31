//go:build unix

package taskenvdocker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func (p *Provider) acquireCloneLock(ctx context.Context, cloneIdentity string) (func() error, error) {
	path := filepath.Join(p.root, ".clone-lock-"+cloneIdentity)
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open private clone lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	closeOnError := func(err error) (func() error, error) {
		_ = file.Close()
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o777 != 0o600 || stat.Nlink != 1 {
		return closeOnError(errors.New("private clone lock is unsafe"))
	}
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() error {
				return errors.Join(unix.Flock(fd, unix.LOCK_UN), file.Close())
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return closeOnError(fmt.Errorf("lock private clone authority: %w", err))
		}
		select {
		case <-ctx.Done():
			return closeOnError(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
