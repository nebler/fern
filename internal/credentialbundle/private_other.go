//go:build !darwin && !linux

package credentialbundle

import "os"

// Fail closed where descriptor-based private-file checks are not supported.
func openPrivateFile(string) (*os.File, error) {
	return nil, ErrUnsafeFile
}
