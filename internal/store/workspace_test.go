package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nebler/fern/internal/domain"
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
	otherCandidate.ID = domain.WorkspaceID(testID("wsp_", 42))
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
		{"path", func(value *Workspace) { value.RepositoryPath = "/srv/other" }},
		{"installation", func(value *Workspace) { value.InstallationID++ }},
		{"repository", func(value *Workspace) { value.RepositoryID++ }},
		{"full name", func(value *Workspace) { value.RepositoryFullName = "owner/other" }},
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

func TestWorkspaceRequiresAppInstallationID(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	defer store.Close()
	desired := testWorkspaceBinding()
	created, err := store.EnsureWorkspace(context.Background(), desired)
	if err != nil {
		t.Fatal(err)
	}
	if created.GitHubAuthority != GitHubAuthorityAppBroker || created.InstallationID != desired.InstallationID {
		t.Fatalf("created = %+v", created)
	}
	var authority string
	var storedInstallation int64
	if err := store.db.QueryRow(`SELECT github_authority,installation_id FROM workspaces WHERE id=?`, desired.ID).Scan(&authority, &storedInstallation); err != nil {
		t.Fatal(err)
	}
	if authority != string(GitHubAuthorityAppBroker) || storedInstallation != int64(desired.InstallationID) {
		t.Fatalf("stored authority=%q installation=%d", authority, storedInstallation)
	}

	invalid := testWorkspaceBinding()
	invalid.GitHubAuthority = "invalid"
	if err := store.CreateWorkspace(context.Background(), invalid); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid authority error = %v", err)
	}
	invalid = testWorkspaceBinding()
	invalid.InstallationID = 0
	if err := store.CreateWorkspace(context.Background(), invalid); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("App installation error = %v", err)
	}
}

func TestFindReceiptByIdempotencyIsReadOnlyAndExactScoped(t *testing.T) {
	t.Parallel()
	store := openTestStore(t, testDBPath(t))
	defer store.Close()
	createTestWorkspace(t, store)
	admission, err := store.AdmitRun(context.Background(), testAdmission(88, "receipt-lookup", "work"))
	if err != nil {
		t.Fatal(err)
	}
	receipt, found, err := store.FindReceiptByIdempotency(context.Background(), testWorkspaceID(), CreateRunCommand, "receipt-lookup")
	if err != nil || !found || receipt.ID != admission.Receipt.ID {
		t.Fatalf("receipt=%+v found=%t err=%v", receipt, found, err)
	}
	if _, found, err := store.FindReceiptByIdempotency(context.Background(), testWorkspaceID(), CreateRunCommand, "other-key"); err != nil || found {
		t.Fatalf("missing found=%t err=%v", found, err)
	}
}

func testWorkspaceBinding() Workspace {
	return Workspace{
		ID: testWorkspaceID(), Name: "demo", State: WorkspaceActive,
		RepositoryPath: "/srv/fern/workspaces/demo", GitHubAuthority: GitHubAuthorityAppBroker, InstallationID: 123, RepositoryID: 987654321,
		RepositoryFullName: "owner/repository", CreatedAt: testTime,
	}
}
