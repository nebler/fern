package registry

import "testing"

func TestIntentStoreRecordsContainerIdentity(t *testing.T) {
	store := NewIntentStore(t.TempDir())
	if err := store.BeginPause("demo", "container-one"); err != nil {
		t.Fatal(err)
	}
	paused, err := store.IsPaused("demo", "container-one")
	if err != nil {
		t.Fatal(err)
	}
	if paused {
		t.Fatal("uncommitted pause intent was treated as paused")
	}
	if err := store.CommitPause("demo", "container-one"); err != nil {
		t.Fatal(err)
	}
	paused, err = store.IsPaused("demo", "container-one")
	if err != nil {
		t.Fatal(err)
	}
	if !paused {
		t.Fatal("matching container was not marked paused")
	}
	paused, err = store.IsPaused("demo", "container-two")
	if err != nil {
		t.Fatal(err)
	}
	if paused {
		t.Fatal("pause intent applied to replacement container")
	}
	if err := store.Clear("demo"); err != nil {
		t.Fatal(err)
	}
	paused, err = store.IsPaused("demo", "container-one")
	if err != nil || paused {
		t.Fatalf("pause intent remained after clear: paused=%t err=%v", paused, err)
	}
}
