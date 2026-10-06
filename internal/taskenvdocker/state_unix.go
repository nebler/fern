//go:build unix

package taskenvdocker

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nebler/fern/internal/atomicfile"
	"golang.org/x/sys/unix"
)

func prepareRoot(stateRoot string) (string, [32]byte, error) {
	return prepareRootWithKey(stateRoot, nil)
}

// prepareRootWithKey retains the existing atomic root publication protocol.
// Runtime roots use a copy of the durable state's authoritative key; they must
// never silently create a second identity or accept a different existing key.
func prepareRootWithKey(stateRoot string, authoritative *[32]byte) (string, [32]byte, error) {
	var zero [32]byte
	resolved, err := filepath.EvalSymlinks(stateRoot)
	if err != nil || resolved != stateRoot {
		return "", zero, errors.New("Fern state root must be an exact path without symlinks")
	}
	root := filepath.Join(stateRoot, runRootName)
	if _, err := os.Lstat(root); err == nil {
		key, err := loadExistingRoot(root)
		if err == nil && authoritative != nil && key != *authoritative {
			return "", zero, errors.New("runtime host key differs from durable authoritative host key")
		}
		return root, key, err
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", zero, err
	}

	suffix, err := randomSuffix()
	if err != nil {
		return "", zero, err
	}
	staging := filepath.Join(stateRoot, "."+runRootName+"-stage-"+suffix)
	if err := os.Mkdir(staging, 0o700); err != nil {
		return "", zero, fmt.Errorf("create staged background run root: %w", err)
	}
	stagingLive := true
	defer func() {
		if stagingLive {
			_ = os.RemoveAll(staging)
		}
	}()
	var generated [32]byte
	if authoritative != nil {
		generated = *authoritative
	} else {
		if _, err := io.ReadFull(rand.Reader, generated[:]); err != nil {
			return "", zero, fmt.Errorf("generate background run host key: %w", err)
		}
	}
	if err := atomicfile.Write(filepath.Join(staging, hostKeyName), generated[:], 0o600); err != nil {
		return "", zero, fmt.Errorf("write staged background run host key: %w", err)
	}
	if err := renameNoReplace(staging, root); err != nil {
		if _, statErr := os.Lstat(root); statErr == nil {
			key, loadErr := loadExistingRoot(root)
			if loadErr == nil && authoritative != nil && key != *authoritative {
				return "", zero, errors.New("runtime host key differs from durable authoritative host key")
			}
			return root, key, loadErr
		}
		return "", zero, fmt.Errorf("publish staged background run root: %w", err)
	}
	stagingLive = false
	if err := syncDirectory(stateRoot); err != nil {
		return "", zero, err
	}
	committed, err := loadExistingRoot(root)
	if err == nil && authoritative != nil && committed != *authoritative {
		return "", zero, errors.New("runtime host key differs from durable authoritative host key")
	}
	return root, committed, err
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
	data, err := atomicfile.Read(filepath.Join(root, hostKeyName), int64(len(key)))
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

func randomSuffix() (string, error) {
	var value [12]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", fmt.Errorf("generate host key temporary name: %w", err)
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	result := make([]byte, len(value))
	for i, item := range value {
		result[i] = alphabet[int(item)%len(alphabet)]
	}
	return string(result), nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func diskAvailable(path string) (int64, error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return 0, err
	}
	return int64(stats.Bavail) * int64(stats.Bsize), nil
}
