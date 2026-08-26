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
	"syscall"
	"time"

	"github.com/nebler/fern/internal/runtime"
)

type IntentStore struct {
	directory string
}

type pauseIntent struct {
	ContainerID string `json:"containerID"`
	Committed   bool   `json:"committed"`
	Shutdown    bool   `json:"shutdown,omitempty"`
	CreatedAt   string `json:"createdAt,omitempty"`
	ExpiresAt   string `json:"expiresAt,omitempty"`
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

func (s *IntentStore) CommitShutdown(workspace, containerID string, expiresAt time.Time) error {
	createdAt := time.Now().UTC()
	return s.write(workspace, pauseIntent{
		ContainerID: containerID, Committed: true, Shutdown: true,
		CreatedAt: createdAt.Format(time.RFC3339Nano),
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339Nano),
	})
}

func (s *IntentStore) write(workspace string, intent pauseIntent) error {
	if err := ensurePrivateDirectory(s.directory, "pause intent"); err != nil {
		return err
	}
	// Reclaim temp files orphaned by a crashed earlier writer: they would
	// otherwise accumulate forever because only rename or the deferred remove
	// of the same write cleans them. Files younger than a minute are left
	// alone; they may belong to a concurrent writer mid-rename.
	s.reclaimStaleTemporaries()
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

func (s *IntentStore) PauseStatus(workspace, containerID string, stoppedAt time.Time) (runtime.PauseIntentStatus, error) {
	file, err := os.OpenFile(s.path(workspace), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return runtime.PauseIntentNone, nil
	}
	if err != nil {
		return runtime.PauseIntentNone, fmt.Errorf("open pause intent: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return runtime.PauseIntentNone, fmt.Errorf("inspect pause intent: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 || info.Mode().Perm()&0o077 != 0 {
		return runtime.PauseIntentNone, errors.New("pause intent must be a private singly linked regular file")
	}
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
	if intent.Shutdown {
		createdAt, err := time.Parse(time.RFC3339Nano, intent.CreatedAt)
		if err != nil {
			return runtime.PauseIntentNone, fmt.Errorf("decode shutdown intent creation: %w", err)
		}
		expiresAt, err := time.Parse(time.RFC3339Nano, intent.ExpiresAt)
		if err != nil {
			return runtime.PauseIntentNone, fmt.Errorf("decode shutdown intent expiry: %w", err)
		}
		// The -5s creation skew tolerates host clock jitter between the
		// runtime writing the intent (with runtime.ShutdownIntentTTL expiry)
		// and this classification reading it. The TTL itself is owned by
		// internal/runtime; this package must not define its own competing
		// lifetime for shutdown intents.
		if stoppedAt.IsZero() || stoppedAt.Before(createdAt.Add(-5*time.Second)) || stoppedAt.After(expiresAt) {
			return runtime.PauseIntentNone, nil
		}
		return runtime.PauseIntentShutdown, nil
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

// reclaimStaleTemporaries removes ".pause-*.tmp" files older than one minute.
// Every step is best-effort: reclaiming must never fail or delay an intent
// write, and individual unreadable or undeletable leftovers are harmless.
func (s *IntentStore) reclaimStaleTemporaries() {
	stale, err := filepath.Glob(filepath.Join(s.directory, ".pause-*.tmp"))
	if err != nil {
		return
	}
	for _, candidate := range stale {
		info, err := os.Stat(candidate)
		if err != nil || time.Since(info.ModTime()) <= time.Minute {
			continue
		}
		_ = os.Remove(candidate)
	}
}

func (s *IntentStore) path(workspace string) string {
	return filepath.Join(s.directory, fmt.Sprintf("%x.json", sha256.Sum256([]byte(workspace))))
}
