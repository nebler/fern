//go:build !unix

package taskenvdocker

import (
	"errors"
	"os"
)

func fileIdentity(os.FileInfo) (uint64, uint64, error) {
	return 0, 0, errors.New("background run Docker state requires Unix")
}
