package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/runtime"
)

func TestIntentStoreRecordsContainerIdentity(t *testing.T) {
	store := NewIntentStore(t.TempDir())
	if err := store.BeginPause("demo", "container-one"); err != nil {
		t.Fatal(err)
	}
	status, err := store.PauseStatus("demo", "container-one", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if status != runtime.PauseIntentPending {
		t.Fatalf("uncommitted pause status = %d", status)
	}
	if err := store.CommitPause("demo", "container-one"); err != nil {
		t.Fatal(err)
	}
	status, err = store.PauseStatus("demo", "container-one", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if status != runtime.PauseIntentCommitted {
		t.Fatal("matching container was not marked paused")
	}
	status, err = store.PauseStatus("demo", "container-two", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if status != runtime.PauseIntentNone {
		t.Fatal("pause intent applied to replacement container")
	}
	if err := store.Clear("demo"); err != nil {
		t.Fatal(err)
	}
	status, err = store.PauseStatus("demo", "container-one", time.Time{})
	if err != nil || status != runtime.PauseIntentNone {
		t.Fatalf("pause intent remained after clear: status=%d err=%v", status, err)
	}
}

func TestShutdownIntentExpires(t *testing.T) {
	store := NewIntentStore(t.TempDir())
	if err := store.CommitShutdown("demo", "container-one", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	status, err := store.PauseStatus("demo", "container-one", time.Now())
	if err != nil || status != runtime.PauseIntentShutdown {
		t.Fatalf("active shutdown intent: status=%d err=%v", status, err)
	}
	if err := store.CommitShutdown("demo", "container-one", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	status, err = store.PauseStatus("demo", "container-one", time.Now())
	if err != nil || status != runtime.PauseIntentNone {
		t.Fatalf("expired shutdown intent: status=%d err=%v", status, err)
	}
}

func TestIntentStoreRejectsSymlinkDirectory(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	directory := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(target, directory); err != nil {
		t.Fatal(err)
	}
	store := NewIntentStore(directory)
	if err := store.BeginPause("demo", "container-one"); err == nil {
		t.Fatal("intent store accepted a symlink directory")
	}
}

func TestIntentStoreRejectsOversizedFile(t *testing.T) {
	directory := t.TempDir()
	store := NewIntentStore(directory)
	if err := os.WriteFile(store.path("demo"), []byte(strings.Repeat("x", 5<<10)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PauseStatus("demo", "container-one", time.Time{}); err == nil {
		t.Fatal("oversized pause intent was accepted")
	}
}
