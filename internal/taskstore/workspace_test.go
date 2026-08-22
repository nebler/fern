package taskstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

func TestEnsureWorkspaceCreatesAndAdoptsExactBinding(t *testing.T) {
	t.Parallel()
	store := openTestStore(t, testDBPath(t))
	defer store.Close()
	desired := testWorkspaceBinding()
	created, err := store.EnsureWorkspace(context.Background(), desired)
	if err != nil {
		t.Fatal(err)
	}
	storedTime := time.UnixMilli(testTime.UnixMilli()).UTC()
	if created.ID != desired.ID || created.Revision != 1 || !created.CreatedAt.Equal(storedTime) {
		t.Fatalf("created = %+v", created)
	}
	otherCandidate := desired
	otherCandidate.ID = task.WorkspaceID(testID("wsp_", 42))
	otherCandidate.CreatedAt = testTime.AddDate(0, 0, 1)
	adopted, err := store.EnsureWorkspace(context.Background(), otherCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.ID != desired.ID || !adopted.CreatedAt.Equal(storedTime) {
		t.Fatalf("adopted = %+v", adopted)
	}
	byName, err := store.GetWorkspaceByName(context.Background(), desired.Name)
	if err != nil || byName != adopted {
		t.Fatalf("by name = %+v, %v", byName, err)
	}
	byID, err := store.GetWorkspace(context.Background(), desired.ID)
	if err != nil || byID != adopted {
		t.Fatalf("by ID = %+v, %v", byID, err)
	}
}

func TestEnsureWorkspaceRejectsEveryBindingDrift(t *testing.T) {
	t.Parallel()
	mutations := []struct {
		name   string
		change func(*Workspace)
	}{
		{"state", func(value *Workspace) { value.State = WorkspaceMaintenance }},
		{"path", func(value *Workspace) { value.RepositoryPath = "/srv/other" }},
		{"installation", func(value *Workspace) { value.InstallationID++ }},
		{"repository", func(value *Workspace) { value.RepositoryID++ }},
		{"full name", func(value *Workspace) { value.RepositoryFullName = "owner/other" }},
		{"image", func(value *Workspace) { value.ImageDigest = "sha256:other" }},
		{"protocol", func(value *Workspace) { value.OpenCodeProtocol = "v3" }},
		{"desired state", func(value *Workspace) { value.RuntimeDesiredState = "paused" }},
		{"epoch", func(value *Workspace) { value.ReconciliationEpoch++ }},
	}
	for _, test := range mutations {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := openTestStore(t, testDBPath(t))
			defer store.Close()
			desired := testWorkspaceBinding()
			if _, err := store.EnsureWorkspace(context.Background(), desired); err != nil {
				t.Fatal(err)
			}
			test.change(&desired)
			if _, err := store.EnsureWorkspace(context.Background(), desired); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("drift error = %v", err)
			}
		})
	}
}

func TestWorkspaceReadsValidateAndHideMissingRows(t *testing.T) {
	t.Parallel()
	store := openTestStore(t, testDBPath(t))
	defer store.Close()
	if _, err := store.GetWorkspace(context.Background(), "bad"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid ID error = %v", err)
	}
	if _, err := store.GetWorkspace(context.Background(), testWorkspaceID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing ID error = %v", err)
	}
	if _, err := store.GetWorkspaceByName(context.Background(), "bad\nname"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid name error = %v", err)
	}
	if _, err := store.GetWorkspaceByName(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing name error = %v", err)
	}
}

func TestFindReceiptByIdempotencyIsReadOnlyAndExactScoped(t *testing.T) {
	t.Parallel()
	store := openTestStore(t, testDBPath(t))
	defer store.Close()
	createTestWorkspace(t, store)
	admission, err := store.AdmitTask(context.Background(), testAdmission(88, "receipt-lookup", "work"))
	if err != nil {
		t.Fatal(err)
	}
	receipt, found, err := store.FindReceiptByIdempotency(context.Background(), testWorkspaceID(), SubmitTaskCommand, "receipt-lookup")
	if err != nil || !found || receipt.ID != admission.Receipt.ID {
		t.Fatalf("receipt=%+v found=%t err=%v", receipt, found, err)
	}
	if _, found, err := store.FindReceiptByIdempotency(context.Background(), testWorkspaceID(), SubmitTaskCommand, "other-key"); err != nil || found {
		t.Fatalf("missing found=%t err=%v", found, err)
	}
	if _, _, err := store.FindReceiptByIdempotency(context.Background(), "bad", SubmitTaskCommand, "receipt-lookup"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid scope error = %v", err)
	}
}

func testWorkspaceBinding() Workspace {
	return Workspace{
		ID: testWorkspaceID(), Name: "demo", State: WorkspaceActive,
		RepositoryPath: "/srv/fern/workspaces/demo", InstallationID: 123, RepositoryID: 987654321,
		RepositoryFullName: "owner/repository", ImageDigest: "sha256:image", OpenCodeProtocol: "v2",
		RuntimeDesiredState: "running", ReconciliationEpoch: 1, CreatedAt: testTime,
	}
}
