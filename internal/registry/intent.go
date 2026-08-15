package registry

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
		return err
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(s.directory, ".pause-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, s.path(workspace)); err != nil {
		return err
	}
	directory, err := os.Open(s.directory)
	if err != nil {
		return err
	}
	err = directory.Sync()
	return errors.Join(err, directory.Close())
}

func (s *IntentStore) PauseStatus(workspace, containerID string) (runtime.PauseIntentStatus, error) {
	data, err := os.ReadFile(s.path(workspace))
	if errors.Is(err, os.ErrNotExist) {
		return runtime.PauseIntentNone, nil
	}
	if err != nil {
		return runtime.PauseIntentNone, err
	}
	var intent pauseIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return runtime.PauseIntentNone, fmt.Errorf("decode pause intent: %w", err)
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
		return err
	}
	directory, err := os.Open(s.directory)
	if err != nil {
		return err
	}
	err = directory.Sync()
	return errors.Join(err, directory.Close())
}

func (s *IntentStore) path(workspace string) string {
	return filepath.Join(s.directory, fmt.Sprintf("%x.json", sha256.Sum256([]byte(workspace))))
}
