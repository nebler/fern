//go:build unix

package verification_test

import "syscall"

func escapeProcessGroup() error {
	_, err := syscall.Setsid()
	return err
}
