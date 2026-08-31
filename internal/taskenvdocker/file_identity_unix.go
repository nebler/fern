//go:build unix

package taskenvdocker

import (
	"errors"
	"os"
	"syscall"
)

func fileIdentity(info os.FileInfo) (uint64, uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Dev == 0 || stat.Ino == 0 {
		return 0, 0, errors.New("filesystem object has no durable Unix identity")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}
