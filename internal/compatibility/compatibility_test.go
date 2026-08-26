package compatibility_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
	_ "modernc.org/sqlite"
)

const baselineDirectory = "testdata/baseline-v1"

var baselineFileChecksums = map[string]string{
	"control/2a97516c354b68848cdbd8f54a226a0a55b21ed138e207ad6c5cbb9c00aa5aea.json": "a839bb61730c650b833999fd11550f6f677c267f2e67f1ffcb14f0d34cd03b65",
	"fern.yaml":                       "125be67c7eb7f0312b75eeea116f9c8570d43caca8838995ac589bcb7fa46923",
	"github-app/app-credentials.json": "9dc22f33803ba3b4a5e945ba18d3c695fd1b95a6f1ed6a0e0d5f47318afabe27",
	"metadata.json":                   "e5e511c4e9ca2e81a4583610cb86ac2c80ac2c9ab835cbaacb7f48c3d577f896",
	"task-store.sqlite":               "2fb2541a99396b40e99704184e5a49fe44b8e31485bc5e32c26c9c4e82a03a38",
}

type migrationRecord struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	SHA256  string `json:"sha256"`
}

var baselineMigrationLedger = []migrationRecord{
	{1, "initial_task_store", "3ff013a514506ce2b74258c04c831178c5e0273cb50173a5a06b381939151d3d"},
	{2, "execution_projection_and_results", "68cf5e97208957d894d02fbe5254ce2f4738348c6fd8b1d2f5b2548b1a15141b"},
	{3, "verification_and_publication_journals", "88b52b43bf12184bc54e6ffde5f21ed98888ec42026164ec9f3becfeb37f46b6"},
	{4, "user_authorized_snapshot_seals", "218ec52d97faf9a95c1790230e47c22cad37d9974b2e8d9a118fe3935ebbf03b"},
	{5, "explicit_workspace_github_authority", "675011d6037df1b806e78e0a98576c43a0594d6c21a3d54e9f10fb8c4017ec8d"},
}

var currentMigrationLedger = append(append([]migrationRecord(nil), baselineMigrationLedger...),
	migrationRecord{6, "publication_admission_receipts", "6c54a44e10e025c2d82a1466b184c74ea8d2641530472aca02e79b4cdcd301ca"})

func TestBaselineV1UpgradesWithoutSemanticLoss(t *testing.T) {
	assertBaselineBytes(t)

	var metadata struct {
		Baseline      string `json:"baseline"`
		SupportStatus string `json:"support_status"`
		Provenance    struct {
			Kind              string  `json:"kind"`
			HistoricalRelease bool    `json:"historical_release"`
			HistoricalTag     *string `json:"historical_tag"`
		} `json:"provenance"`
		Schemas struct {
			TaskStore struct {
				FixtureVersion       int               `json:"fixture_version"`
				UpgradeTargetVersion int               `json:"upgrade_target_version"`
				MigrationLedger      []migrationRecord `json:"migration_ledger"`
			} `json:"task_store"`
			ControlState         int `json:"control_state"`
			GitHubAppCredentials int `json:"github_app_credentials"`
		} `json:"schemas"`
		Files map[string]string `json:"files"`
	}
	decodeJSONFile(t, filepath.Join(baselineDirectory, "metadata.json"), &metadata)
	if metadata.Baseline != "baseline-v1" || metadata.SupportStatus != "first-supported-baseline" || metadata.Provenance.Kind != "repository-established" || metadata.Provenance.HistoricalRelease || metadata.Provenance.HistoricalTag != nil {
		t.Fatalf("baseline provenance overstates release history: %+v", metadata)
	}
	currentSchema := taskstore.CurrentSchemaVersion()
	if metadata.Schemas.TaskStore.FixtureVersion != 4 || metadata.Schemas.TaskStore.UpgradeTargetVersion != currentSchema || metadata.Schemas.ControlState != 1 || metadata.Schemas.GitHubAppCredentials != 1 {
		t.Fatalf("unexpected baseline schema versions: %+v", metadata.Schemas)
	}
	if !reflect.DeepEqual(metadata.Schemas.TaskStore.MigrationLedger, baselineMigrationLedger) {
		t.Fatalf("metadata migration checksums changed: %+v", metadata.Schemas.TaskStore.MigrationLedger)
	}
	wantMetadataFiles := make(map[string]string, len(baselineFileChecksums)-1)
	for name, checksum := range baselineFileChecksums {
		if name != "metadata.json" {
			wantMetadataFiles[name] = checksum
		}
	}
	if !reflect.DeepEqual(metadata.Files, wantMetadataFiles) {
		t.Fatalf("metadata file checksums changed: %+v", metadata.Files)
	}

	copyRoot := filepath.Join(t.TempDir(), "baseline-v1")
	copyFixture(t, baselineDirectory, copyRoot)
	if err := os.Mkdir(filepath.Join(copyRoot, "repository"), 0o700); err != nil {
		t.Fatal(err)
	}

	databasePath := filepath.Join(copyRoot, "task-store.sqlite")
	baselineDB := openReadOnlyDB(t, databasePath)
	assertDatabaseHealth(t, baselineDB)
	assertSchemaVersionAndLedger(t, baselineDB, 4, baselineMigrationLedger[:4])
	if err := baselineDB.Close(); err != nil {
		t.Fatal(err)
	}
	if got := fileChecksum(t, databasePath); got != baselineFileChecksums["task-store.sqlite"] {
		t.Fatalf("pre-upgrade database bytes = %s", got)
	}

	store, err := taskstore.Open(context.Background(), databasePath)
	if err != nil {
		t.Fatalf("open and migrate baseline task store: %v", err)
	}
	workspaceID := task.WorkspaceID("wsp_0198d34d-6a50-75fb-b1f2-000000000001")
	workspace, err := store.GetWorkspace(context.Background(), workspaceID)
	if err != nil || workspace.Name != "demo" || workspace.GitHubAuthority != taskstore.GitHubAuthorityAppBroker || workspace.InstallationID != 123 || workspace.RepositoryID != 987654321 {
		t.Fatalf("migrated workspace = %+v, error = %v", workspace, err)
	}
	taskID := task.TaskID("tsk_0198d34d-6a50-75fb-b1f2-00000000002a")
	gotTask, err := store.GetTask(context.Background(), taskID)
	if err != nil || gotTask.Prompt != "Preserve durable compatibility" || gotTask.State != task.TaskQueued || gotTask.CurrentAttemptID != "att_0198d34d-6a50-75fb-b1f2-00000000002a" {
		t.Fatalf("migrated task = %+v, error = %v", gotTask, err)
	}
	gotAttempt, err := store.GetAttempt(context.Background(), task.AttemptID("att_0198d34d-6a50-75fb-b1f2-00000000002a"))
	if err != nil || gotAttempt.State != task.AttemptPrepared || gotAttempt.Model != "model-1" || string(gotAttempt.BudgetSnapshot) != `{"maxTokens":4096}` {
		t.Fatalf("migrated attempt = %+v, error = %v", gotAttempt, err)
	}
	gotReceipt, err := store.GetReceipt(context.Background(), task.ReceiptID("rcp_0198d34d-6a50-75fb-b1f2-00000000002a"))
	if err != nil || gotReceipt.State != taskstore.ReceiptAccepted || gotReceipt.IdempotencyKey != "baseline-submit" || gotReceipt.TargetID != taskID {
		t.Fatalf("migrated receipt = %+v, error = %v", gotReceipt, err)
	}
	events, err := store.ListEvents(context.Background(), workspaceID, 0, 10)
	if err != nil || len(events.Events) != 2 || !events.CaughtUp || events.NextCursor != 2 {
		t.Fatalf("migrated events = %+v, error = %v", events, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	upgradedDB := openReadOnlyDB(t, databasePath)
	defer upgradedDB.Close()
	assertDatabaseHealth(t, upgradedDB)
	assertSchemaVersionAndLedger(t, upgradedDB, currentSchema, currentMigrationLedger)
	if len(currentMigrationLedger) != currentSchema {
		t.Fatalf("compatibility ledger has %d migrations, task store has %d", len(currentMigrationLedger), currentSchema)
	}
	var compatibility struct {
		CurrentReleaseSchemas struct {
			TaskStore int `json:"task_store"`
		} `json:"current_release_schemas"`
	}
	decodeJSONFile(t, filepath.Join("..", "..", "deploy", "release", "compatibility-manifest.json"), &compatibility)
	if compatibility.CurrentReleaseSchemas.TaskStore != currentSchema {
		t.Fatalf("compatibility manifest task store schema = %d, want %d", compatibility.CurrentReleaseSchemas.TaskStore, currentSchema)
	}

	loadedConfig, err := config.Load(filepath.Join(copyRoot, "fern.yaml"), copyRoot, true, config.Overrides{})
	if err != nil || loadedConfig.Tasks == nil || loadedConfig.Tasks.Budget.MaxTurns != 50 || loadedConfig.Workspace.GitHub == nil || loadedConfig.Workspace.GitHub.InstallationID != 123 {
		t.Fatalf("baseline config = %+v, error = %v", loadedConfig, err)
	}
	if err := config.Validate(loadedConfig); err != nil {
		t.Fatalf("validate baseline config: %v", err)
	}

	controlStore, err := control.Open(filepath.Join(copyRoot, "control"), "demo")
	if err != nil {
		t.Fatalf("open baseline control state: %v", err)
	}
	device, valid, err := controlStore.AuthenticateDeviceIdentity("baseline-device-secret", time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC))
	if err != nil || !valid || device.Name != "Baseline browser" {
		t.Fatalf("baseline device = %+v, valid = %t, error = %v", device, valid, err)
	}
	workflow, found := controlStore.Workflow("workflow-baseline")
	if !found || workflow.Status != control.WorkflowPublished || workflow.PublicationID != "publication-baseline" {
		t.Fatalf("baseline workflow = %+v, found = %t", workflow, found)
	}
	publication, found := controlStore.Publication("publication-baseline")
	if !found || publication.State != control.PublicationPublished || publication.PullURL != "https://github.com/owner/repository/pull/42" {
		t.Fatalf("baseline publication = %+v, found = %t", publication, found)
	}
	if operatorID, err := controlStore.EnsureOperatorCredentialID(); err != nil || operatorID != "control-AAECAwQFBgcICQoLDA0ODw" {
		t.Fatalf("baseline operator credential ID = %q, error = %v", operatorID, err)
	}

	credentialStore, err := githubapp.NewCredentialStore(filepath.Join(copyRoot, "github-app"))
	if err != nil {
		t.Fatalf("open baseline GitHub App store: %v", err)
	}
	credentials, err := credentialStore.Load()
	if err != nil || credentials.AppID() != 4242 || credentials.ClientID() != "Iv1.baseline-client" || credentials.PrivateKey() == nil || credentials.PrivateKey().N.BitLen() != 2048 {
		t.Fatalf("baseline GitHub App credentials = %v, error = %v", credentials, err)
	}

	assertBaselineBytes(t)
}

func assertBaselineBytes(t *testing.T) {
	t.Helper()
	seen := make(map[string]bool, len(baselineFileChecksums))
	err := filepath.WalkDir(baselineDirectory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(baselineDirectory, path)
		if err != nil {
			return err
		}
		want, exists := baselineFileChecksums[filepath.ToSlash(relative)]
		if !exists {
			return fmt.Errorf("unregistered fixture file %s", relative)
		}
		seen[filepath.ToSlash(relative)] = true
		if got := fileChecksum(t, path); got != want {
			return fmt.Errorf("fixture %s checksum = %s, want %s", relative, got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(baselineFileChecksums) {
		t.Fatalf("found %d registered fixture files, want %d", len(seen), len(baselineFileChecksums))
	}
}

func copyFixture(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func openReadOnlyDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Ping(); err != nil {
		database.Close()
		t.Fatal(err)
	}
	return database
}

func assertDatabaseHealth(t *testing.T, database *sql.DB) {
	t.Helper()
	var integrity string
	if err := database.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q, error = %v", integrity, err)
	}
	rows, err := database.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check found a violation")
	}
}

func assertSchemaVersionAndLedger(t *testing.T, database *sql.DB, wantVersion int, want []migrationRecord) {
	t.Helper()
	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != wantVersion {
		t.Fatalf("user_version = %d, error = %v, want %d", version, err, wantVersion)
	}
	rows, err := database.Query(`SELECT version,name,checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []migrationRecord
	for rows.Next() {
		var record migrationRecord
		if err := rows.Scan(&record.Version, &record.Name, &record.SHA256); err != nil {
			t.Fatal(err)
		}
		got = append(got, record)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("migration ledger = %+v, want %+v", got, want)
	}
}

func fileChecksum(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func decodeJSONFile(t *testing.T, path string, destination any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, destination); err != nil {
		t.Fatal(err)
	}
}
