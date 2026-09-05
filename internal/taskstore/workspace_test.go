package taskstore

import (
	"context"
	"errors"
	"os"
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
		{"GitHub authority", func(value *Workspace) { value.GitHubAuthority = GitHubAuthorityWorkspaceGH; value.InstallationID = 0 }},
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

func TestWorkspaceGHAuthorityUsesNoPublicInstallationID(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	defer store.Close()
	desired := testWorkspaceBinding()
	desired.GitHubAuthority = GitHubAuthorityWorkspaceGH
	desired.InstallationID = 0
	created, err := store.EnsureWorkspace(context.Background(), desired)
	if err != nil {
		t.Fatal(err)
	}
	if created.GitHubAuthority != GitHubAuthorityWorkspaceGH || created.InstallationID != 0 {
		t.Fatalf("created = %+v", created)
	}
	var authority string
	var storedInstallation int64
	if err := store.db.QueryRow(`SELECT github_authority,installation_id FROM workspaces WHERE id=?`, desired.ID).Scan(&authority, &storedInstallation); err != nil {
		t.Fatal(err)
	}
	if authority != string(GitHubAuthorityWorkspaceGH) || storedInstallation != workspaceGHDatabaseInstallationID {
		t.Fatalf("stored authority=%q discriminator=%d", authority, storedInstallation)
	}

	invalid := testWorkspaceBinding()
	invalid.GitHubAuthority = GitHubAuthorityWorkspaceGH
	if err := store.CreateWorkspace(context.Background(), invalid); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("workspace-gh installation error = %v", err)
	}
	invalid = testWorkspaceBinding()
	invalid.InstallationID = 0
	if err := store.CreateWorkspace(context.Background(), invalid); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("App installation error = %v", err)
	}
}

func TestMigrationFivePreservesExistingAppAuthority(t *testing.T) {
	path := testDBPath(t)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	raw := openRaw(t, path)
	for _, migration := range migrations[:4] {
		if _, err := raw.Exec(migration.sql); err != nil {
			t.Fatalf("install migration %d: %v", migration.version, err)
		}
		if _, err := raw.Exec(`INSERT INTO schema_migrations(version,name,checksum) VALUES(?,?,?)`, migration.version, migration.name, migrationChecksum(migration)); err != nil {
			t.Fatal(err)
		}
	}
	desired := testWorkspaceBinding()
	if _, err := raw.Exec(`INSERT INTO workspaces(id,name,state,repository_path,installation_id,repository_id,repository_full_name,image_digest,opencode_protocol,runtime_desired_state,reconciliation_epoch,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		desired.ID, desired.Name, desired.State, desired.RepositoryPath, desired.InstallationID, desired.RepositoryID,
		desired.RepositoryFullName, desired.ImageDigest, desired.OpenCodeProtocol, desired.RuntimeDesiredState,
		desired.ReconciliationEpoch, 1, desired.CreatedAt.UnixMilli(), desired.CreatedAt.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version=4`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	store := openTestStore(t, path)
	defer store.Close()
	workspace, err := store.GetWorkspace(context.Background(), desired.ID)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.GitHubAuthority != GitHubAuthorityAppBroker || workspace.InstallationID != desired.InstallationID {
		t.Fatalf("migrated workspace = %+v", workspace)
	}
}

func TestFindReceiptByIdempotencyIsReadOnlyAndExactScoped(t *testing.T) {
	t.Parallel()
	store := openTestStore(t, testDBPath(t))
	defer store.Close()
	createTestWorkspace(t, store)
	admission, err := store.AdmitBackgroundRun(context.Background(), testAdmission(88, "receipt-lookup", "work"))
	if err != nil {
		t.Fatal(err)
	}
	receipt, found, err := store.FindReceiptByIdempotency(context.Background(), testWorkspaceID(), CreateBackgroundRunCommand, "receipt-lookup")
	if err != nil || !found || receipt.ID != admission.Receipt.ID {
		t.Fatalf("receipt=%+v found=%t err=%v", receipt, found, err)
	}
	if _, found, err := store.FindReceiptByIdempotency(context.Background(), testWorkspaceID(), CreateBackgroundRunCommand, "other-key"); err != nil || found {
		t.Fatalf("missing found=%t err=%v", found, err)
	}
	if _, _, err := store.FindReceiptByIdempotency(context.Background(), "bad", CreateBackgroundRunCommand, "receipt-lookup"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid scope error = %v", err)
	}
}

func testWorkspaceBinding() Workspace {
	return Workspace{
		ID: testWorkspaceID(), Name: "demo", State: WorkspaceActive,
		RepositoryPath: "/srv/fern/workspaces/demo", GitHubAuthority: GitHubAuthorityAppBroker, InstallationID: 123, RepositoryID: 987654321,
		RepositoryFullName: "owner/repository", ImageDigest: "sha256:image", OpenCodeProtocol: "v2",
		RuntimeDesiredState: "running", ReconciliationEpoch: 1, CreatedAt: testTime,
	}
}
