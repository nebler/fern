//go:build !darwin && !linux

package verification

import (
	"errors"
	"os"
	"time"
)

type preparedExecutable struct {
	path       string
	extraFiles []*os.File
	close      func()
}

func inspectExecutable(string) (executableIdentity, error) {
	return executableIdentity{}, errors.New("pinned executable launch is unsupported")
}

func preparePinnedExecutable(string, executableIdentity) (preparedExecutable, error) {
	return preparedExecutable{}, errors.New("pinned executable launch is unsupported")
}

func validateGitExecutable(string, executableIdentity, time.Duration, []string) error {
	return errors.New("pinned Git launch is unsupported")
}
