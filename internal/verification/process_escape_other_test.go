//go:build !unix

package verification_test

import "errors"

func escapeProcessGroup() error {
	return errors.New("process-group escape is unsupported")
}
