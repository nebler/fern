//go:build unix

package taskstore

import (
	"fmt"
	"os"
	"syscall"
)

func validateOwnership(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%w: path is not owned by the current user", ErrUnsafePath)
	}
	return nil
}
