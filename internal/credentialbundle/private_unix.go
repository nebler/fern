//go:build darwin || linux

package credentialbundle

import (
	"os"

	"golang.org/x/sys/unix"
)

// openPrivateFile checks the opened descriptor, not a prior pathname lookup.
// O_NONBLOCK avoids blocking on FIFOs before their type can be rejected.
// Only the leaf is no-follow: callers must trust parent directories and must
// not treat this as protection from a hostile process running as the same UID.
func openPrivateFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrUnsafeFile
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		file.Close()
		return nil, ErrUnsafeFile
	}
	return file, nil
}
