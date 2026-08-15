package registry

import (
	"testing"

	"github.com/nebler/fern/internal/runtime"
)

func TestIntentStoreRecordsContainerIdentity(t *testing.T) {
	store := NewIntentStore(t.TempDir())
	if err := store.BeginPause("demo", "container-one"); err != nil {
		t.Fatal(err)
	}
	status, err := store.PauseStatus("demo", "container-one")
	if err != nil {
		t.Fatal(err)
	}
	if status != runtime.PauseIntentPending {
		t.Fatalf("uncommitted pause status = %d", status)
	}
	if err := store.CommitPause("demo", "container-one"); err != nil {
		t.Fatal(err)
	}
	status, err = store.PauseStatus("demo", "container-one")
	if err != nil {
		t.Fatal(err)
	}
	if status != runtime.PauseIntentCommitted {
		t.Fatal("matching container was not marked paused")
	}
	status, err = store.PauseStatus("demo", "container-two")
	if err != nil {
		t.Fatal(err)
	}
	if status != runtime.PauseIntentNone {
		t.Fatal("pause intent applied to replacement container")
	}
	if err := store.Clear("demo"); err != nil {
		t.Fatal(err)
	}
	status, err = store.PauseStatus("demo", "container-one")
	if err != nil || status != runtime.PauseIntentNone {
		t.Fatalf("pause intent remained after clear: status=%d err=%v", status, err)
	}
}
