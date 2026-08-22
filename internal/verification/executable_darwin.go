//go:build darwin

package verification

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const darwinMaterializationRoot = "/private/var/tmp"

func isNativeExecutable(header []byte) bool {
	if len(header) < 4 {
		return false
	}
	magic := binary.BigEndian.Uint32(header[:4])
	switch magic {
	case 0xfeedface, 0xfeedfacf, 0xcefaedfe, 0xcffaedfe, 0xcafebabe, 0xbebafeca, 0xcafebabf, 0xbfbafeca:
		return true
	default:
		return false
	}
}

type preparedExecutable struct {
	path       string
	extraFiles []*os.File
	close      func()
}

// Darwin's devfs rejects execve of /dev/fd/N. Materialize only from the pinned
// descriptor into a new private directory, whose entry cannot be replaced by
// another user between validation and execve.
func preparePinnedExecutable(path string, identity executableIdentity) (preparedExecutable, error) {
	source, err := openPinnedExecutable(path, identity)
	if err != nil {
		return preparedExecutable{}, err
	}
	// This root is absolute, root-owned, and sticky. Unlike os.TempDir it is not
	// selected by ambient TMPDIR. The random 0700 child is private to this uid.
	directory, err := os.MkdirTemp(darwinMaterializationRoot, ".fern-executable-")
	if err != nil {
		source.Close()
		return preparedExecutable{}, err
	}
	cleanup := func() {
		_ = source.Close()
		_ = os.Chmod(directory, 0700)
		_ = os.RemoveAll(directory)
	}
	copyPath := filepath.Join(directory, "executable")
	copyFile, err := os.OpenFile(copyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0500)
	if err != nil {
		cleanup()
		return preparedExecutable{}, err
	}
	if _, err = io.Copy(copyFile, source); err == nil {
		err = copyFile.Sync()
	}
	closeErr := copyFile.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return preparedExecutable{}, err
	}
	if err := os.Chmod(directory, 0500); err != nil {
		cleanup()
		return preparedExecutable{}, err
	}
	return preparedExecutable{path: copyPath, close: cleanup}, nil
}

// Some Apple platform shims, notably /usr/bin/git, are killed after a
// byte-for-byte relocation. Probe the private copy during construction rather
// than silently weakening pinning to a path-based launch.
func validateGitExecutable(path string, identity executableIdentity, timeout time.Duration, environment []string) error {
	executable, err := preparePinnedExecutable(path, identity)
	if err != nil {
		return err
	}
	defer executable.close()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, executable.path, "--version")
	command.Args[0] = path
	command.Env = append([]string(nil), environment...)
	waitErr, cleanupErr, executed := runContainedCommand(command, io.Discard, io.Discard, nil)
	if !executed || waitErr != nil || cleanupErr != nil || ctx.Err() != nil {
		return errors.New("private Git copy failed its launch probe")
	}
	return nil
}
