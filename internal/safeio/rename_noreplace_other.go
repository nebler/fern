//go:build !darwin && !linux

package safeio

import "errors"

// RenameNoReplace is unsupported on this platform.
func RenameNoReplace(string, string) error {
	return errors.New("atomic no-replace rename is unsupported on this platform")
}
