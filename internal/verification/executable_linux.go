//go:build linux

package verification

import (
	"os"
	"time"
)

func isNativeExecutable(header []byte) bool {
	return len(header) >= 4 && header[0] == 0x7f && header[1] == 'E' && header[2] == 'L' && header[3] == 'F'
}

func validateGitExecutable(string, executableIdentity, time.Duration, []string) error { return nil }

type preparedExecutable struct {
	path       string
	extraFiles []*os.File
	close      func()
}

func preparePinnedExecutable(path string, identity executableIdentity) (preparedExecutable, error) {
	file, err := openPinnedExecutable(path, identity)
	if err != nil {
		return preparedExecutable{}, err
	}
	return preparedExecutable{
		path:       "/dev/fd/3",
		extraFiles: []*os.File{file},
		close:      func() { _ = file.Close() },
	}, nil
}
