//go:build unix

package docker

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nebler/fern/internal/safeio"
	"golang.org/x/sys/unix"
)

func prepareRoot(stateRoot string) (string, [32]byte, error) {
	return prepareRootWithKey(stateRoot, nil)
}

// prepareRootWithKey loads or atomically publishes the private run root.
// Runtime roots use a copy of the durable state's authoritative key; they must
// never silently create a second identity or accept a different existing key.
func prepareRootWithKey(stateRoot string, authoritative *[32]byte) (string, [32]byte, error) {
	resolved, err := filepath.EvalSymlinks(stateRoot)
	if err != nil || resolved != stateRoot {
		return "", [32]byte{}, errors.New("Fern state root must be an exact path without symlinks")
	}
	root := filepath.Join(stateRoot, runRootName)
	key, err := publishRoot(stateRoot, root, authoritative)
	if err == nil && authoritative != nil && key != *authoritative {
		err = errors.New("runtime host key differs from durable authoritative host key")
	}
	if err != nil {
		return "", [32]byte{}, err
	}
	return root, key, nil
}

// publishRoot loads an existing root, or stages one holding authoritative (or
// a fresh random key) and publishes it without replacing a concurrent winner.
func publishRoot(stateRoot, root string, authoritative *[32]byte) ([32]byte, error) {
	if _, err := os.Lstat(root); err == nil {
		return loadExistingRoot(root)
	} else if !errors.Is(err, os.ErrNotExist) {
		return [32]byte{}, err
	}
	staging := filepath.Join(stateRoot, "."+runRootName+"-stage-"+rand.Text())
	if err := os.Mkdir(staging, 0o700); err != nil {
		return [32]byte{}, fmt.Errorf("create staged background run root: %w", err)
	}
	defer os.RemoveAll(staging) // a no-op once published
	var key [32]byte
	if authoritative != nil {
		key = *authoritative
	} else {
		_, _ = rand.Read(key[:]) // never fails since Go 1.24
	}
	if err := safeio.WriteFile(filepath.Join(staging, hostKeyName), key[:], 0o600); err != nil {
		return [32]byte{}, fmt.Errorf("write staged background run host key: %w", err)
	}
	if err := safeio.RenameNoReplace(staging, root); err != nil {
		if _, statErr := os.Lstat(root); statErr == nil {
			return loadExistingRoot(root)
		}
		return [32]byte{}, fmt.Errorf("publish staged background run root: %w", err)
	}
	if err := safeio.SyncDir(stateRoot); err != nil {
		return [32]byte{}, err
	}
	return loadExistingRoot(root)
}

func loadExistingRoot(root string) ([32]byte, error) {
	var key [32]byte
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return key, errors.New("background run root is not a private directory")
	}
	if info.Mode().Perm() != 0o700 {
		return key, fmt.Errorf("background run root mode is %04o, want 0700", info.Mode().Perm())
	}
	data, err := safeio.ReadFile(filepath.Join(root, hostKeyName), int64(len(key)))
	if errors.Is(err, os.ErrNotExist) {
		return key, errors.New("background run host key is missing from initialized state")
	}
	if err != nil {
		return key, fmt.Errorf("read background run host key: %w", err)
	}
	if len(data) != len(key) {
		return key, errors.New("background run host key has the wrong length")
	}
	copy(key[:], data)
	return key, nil
}

func diskAvailable(path string) (int64, error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return 0, err
	}
	return int64(stats.Bavail) * int64(stats.Bsize), nil
}
