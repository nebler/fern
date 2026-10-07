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
func privateRoot(string) error                      { return errors.New("unsupported platform") }
func safeDirectoryInfo(os.FileInfo) bool            { return false }
func openPrivateExclusive(string) (*os.File, error) { return nil, errors.New("unsupported platform") }
func openPrivateRead(string, os.FileMode, bool) (*os.File, os.FileInfo, error) {
	return nil, nil, errors.New("unsupported platform")
}
func changePrivateFileMode(string, os.FileMode, os.FileMode) error {
	return errors.New("unsupported platform")
}
