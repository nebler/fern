//go:build !unix

package taskartifact

import (
	"errors"
	"os"
)

func exactRegular(string, bool) (os.FileInfo, error) { return nil, errors.New("unsupported platform") }
func exactDirectory(string, bool) (os.FileInfo, error) {
	return nil, errors.New("unsupported platform")
}
func privateRoot(string) error           { return errors.New("unsupported platform") }
func safeDirectoryInfo(os.FileInfo) bool { return false }
func fileIdentity(os.FileInfo) (uint64, uint64, error) {
	return 0, 0, errors.New("unsupported platform")
}
func openPrivateExclusive(string) (*os.File, error) { return nil, errors.New("unsupported platform") }
func writePrivateFile(string, []byte) error         { return errors.New("unsupported platform") }
func openPrivateRead(string, os.FileMode, bool) (*os.File, os.FileInfo, error) {
	return nil, nil, errors.New("unsupported platform")
}
func syncDirectory(string) error                        { return errors.New("unsupported platform") }
func removeExactDirectory(string, uint64, uint64) error { return errors.New("unsupported platform") }
