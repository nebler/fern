package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nebler/fern/internal/runtime"
)

type IntentStore struct {
	directory string
}

type pauseIntent struct {
	ContainerID string `json:"containerID"`
	Committed   bool   `json:"committed"`
}

func NewIntentStore(directory string) *IntentStore {
	return &IntentStore{directory: directory}
}

func (s *IntentStore) BeginPause(workspace, containerID string) error {
	return s.write(workspace, pauseIntent{ContainerID: containerID})
}

func (s *IntentStore) CommitPause(workspace, containerID string) error {
	return s.write(workspace, pauseIntent{ContainerID: containerID, Committed: true})
}

func (s *IntentStore) write(workspace string, intent pauseIntent) error {
	if err := os.MkdirAll(s.directory, 0o700); err != nil {
		return fmt.Errorf("create pause intent directory: %w", err)
	}
	info, err := os.Lstat(s.directory)
	if err != nil {
		return fmt.Errorf("inspect pause intent directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("pause intent directory must be a real directory")
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return fmt.Errorf("encode pause intent: %w", err)
	}
	temporary, err := os.CreateTemp(s.directory, ".pause-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary pause intent: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("set temporary pause intent permissions: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary pause intent: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary pause intent: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary pause intent: %w", err)
	}
	if err := os.Rename(temporaryPath, s.path(workspace)); err != nil {
		return fmt.Errorf("replace pause intent: %w", err)
	}
	directory, err := os.Open(s.directory)
	if err != nil {
		return fmt.Errorf("open pause intent directory: %w", err)
	}
	err = directory.Sync()
	return errors.Join(wrapError("sync pause intent directory", err), wrapError("close pause intent directory", directory.Close()))
}

func (s *IntentStore) PauseStatus(workspace, containerID string) (runtime.PauseIntentStatus, error) {
	file, err := os.Open(s.path(workspace))
	if errors.Is(err, os.ErrNotExist) {
		return runtime.PauseIntentNone, nil
	}
	if err != nil {
		return runtime.PauseIntentNone, fmt.Errorf("open pause intent: %w", err)
	}
	defer file.Close()
	const maxIntentBytes = 4 << 10
	data, err := io.ReadAll(io.LimitReader(file, maxIntentBytes+1))
	if err != nil {
		return runtime.PauseIntentNone, fmt.Errorf("read pause intent: %w", err)
	}
	if len(data) > maxIntentBytes {
		return runtime.PauseIntentNone, errors.New("decode pause intent: file exceeds 4 KiB")
	}
	var intent pauseIntent
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&intent); err != nil {
		return runtime.PauseIntentNone, fmt.Errorf("decode pause intent: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return runtime.PauseIntentNone, errors.New("decode pause intent: trailing data")
	}
	if intent.ContainerID != containerID {
		return runtime.PauseIntentNone, nil
	}
	if intent.Committed {
		return runtime.PauseIntentCommitted, nil
	}
	return runtime.PauseIntentPending, nil
}

func (s *IntentStore) Clear(workspace string) error {
	err := os.Remove(s.path(workspace))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove pause intent: %w", err)
	}
	directory, err := os.Open(s.directory)
	if err != nil {
		return fmt.Errorf("open pause intent directory: %w", err)
	}
	err = directory.Sync()
	return errors.Join(wrapError("sync pause intent directory", err), wrapError("close pause intent directory", directory.Close()))
}

func wrapError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func (s *IntentStore) path(workspace string) string {
	return filepath.Join(s.directory, fmt.Sprintf("%x.json", sha256.Sum256([]byte(workspace))))
}
