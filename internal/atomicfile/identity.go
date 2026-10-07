package atomicfile

import (
	"errors"
	"os"
	"syscall"
)

// SyncDir fsyncs the directory at path so renames and removals in it are
// durable.
func SyncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// Identity returns the (device, inode) pair of info, failing when the platform
// reports no durable identity.
func Identity(info os.FileInfo) (device, inode uint64, err error) {
	if info == nil {
		return 0, 0, errors.New("filesystem object has no durable identity")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Dev == 0 || stat.Ino == 0 {
		return 0, 0, errors.New("filesystem object has no durable identity")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}
