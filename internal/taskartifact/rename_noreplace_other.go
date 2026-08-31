//go:build !darwin && !linux

package taskartifact

import "errors"

func renameNoReplace(string, string) error {
	return errors.New("atomic no-replace rename is unsupported")
}
