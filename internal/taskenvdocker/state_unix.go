//go:build unix

package taskenvdocker

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func prepareRoot(stateRoot string) (string, [32]byte, error) {
	var zero [32]byte
	resolved, err := filepath.EvalSymlinks(stateRoot)
	if err != nil || resolved != stateRoot {
		return "", zero, errors.New("Fern state root must be an exact path without symlinks")
	}
	root := filepath.Join(stateRoot, runRootName)
	if _, err := os.Lstat(root); err == nil {
		key, err := loadExistingRoot(root)
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
	if _, err := io.ReadFull(rand.Reader, generated[:]); err != nil {
		return "", zero, fmt.Errorf("generate background run host key: %w", err)
	}
	if err := writeInitialHostKey(filepath.Join(staging, hostKeyName), generated); err != nil {
		return "", zero, err
	}
	if err := syncDirectory(staging); err != nil {
		return "", zero, err
	}
	if err := renameNoReplace(staging, root); err != nil {
		if _, statErr := os.Lstat(root); statErr == nil {
			key, loadErr := loadExistingRoot(root)
			return root, key, loadErr
		}
		return "", zero, fmt.Errorf("publish staged background run root: %w", err)
	}
	stagingLive = false
	if err := syncDirectory(stateRoot); err != nil {
		return "", zero, err
	}
	committed, err := loadExistingRoot(root)
	return root, committed, err
}

func writeInitialHostKey(path string, key [32]byte) error {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create staged background run host key: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	writeErr := func() error {
		if _, err := file.Write(key[:]); err != nil {
			return err
		}
		return file.Sync()
	}()
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.Join(writeErr, closeErr)
	}
	return nil
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
	path := filepath.Join(root, hostKeyName)
	if err := recoverHostKeyLinks(root, path); err != nil {
		return key, err
	}
	if err := readHostKey(path, &key); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return key, errors.New("background run host key is missing from initialized state")
		}
		return key, err
	}
	return key, nil
}

func recoverHostKeyLinks(root, path string) error {
	keyInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !keyInfo.Mode().IsRegular() || keyInfo.Mode().Perm() != 0o600 || keyInfo.Mode()&os.ModeSymlink != 0 || keyInfo.Size() != 32 {
		return errors.New("background run host key is unsafe")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	removed := false
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".host-key-") {
			continue
		}
		candidate := filepath.Join(root, entry.Name())
		candidateInfo, err := os.Lstat(candidate)
		if err != nil || !candidateInfo.Mode().IsRegular() || candidateInfo.Mode().Perm() != 0o600 || !os.SameFile(keyInfo, candidateInfo) {
			continue
		}
		if err := os.Remove(candidate); err != nil {
			return fmt.Errorf("remove stale host key link: %w", err)
		}
		removed = true
	}
	if removed {
		return syncDirectory(root)
	}
	return nil
}

func readHostKey(path string, destination *[32]byte) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSymlink != 0 || info.Size() != 32 {
		return errors.New("background run host key is unsafe")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open background run host key: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o777 != 0o600 || stat.Size != 32 || stat.Nlink != 1 {
		return errors.New("opened background run host key is unsafe")
	}
	if _, err := io.ReadFull(file, destination[:]); err != nil {
		return fmt.Errorf("read background run host key: %w", err)
	}
	var extra [1]byte
	if count, err := file.Read(extra[:]); err != io.EOF || count != 0 {
		return errors.New("background run host key has trailing data")
	}
	return nil
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
