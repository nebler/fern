package taskstore

import (
	"context"
	"errors"
	"testing"
)

func TestTaskSnapshotsUseWorkspaceScopedDurableState(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	admission, err := store.AdmitTask(context.Background(), testAdmission(71, "snapshot", "snapshot prompt"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.GetTaskSnapshot(context.Background(), testWorkspaceID(), admission.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Task.ID != admission.Task.ID || snapshot.Attempt.ID != admission.Attempt.ID || snapshot.Verifications == nil ||
		snapshot.SealRequest != nil || snapshot.Result != nil || snapshot.Publication != nil {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	listed, err := store.ListTasks(context.Background(), testWorkspaceID(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Task.ID != admission.Task.ID || listed[0].Attempt.ID != admission.Attempt.ID {
		t.Fatalf("listed snapshots: %+v", listed)
	}
	if _, err := store.GetTaskSnapshot(context.Background(), testWorkspaceID()+"0", admission.Task.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid workspace error = %v", err)
	}
}
