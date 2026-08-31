//go:build !darwin && !linux

package taskenvdocker

import "errors"

func renameNoReplace(string, string) error {
	return errors.New("atomic no-replace rename is unsupported on this platform")
}
