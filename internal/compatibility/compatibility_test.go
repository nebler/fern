// Package compatibility_test is a test-only release-contract check, not a
// runtime compatibility library. It verifies that a freshly opened task store
// and the checked-in release manifest and schema under deploy/ agree on the
// current pre-release task store schema. integration/upgrade/run.sh runs it
// before its initialization/reopen/restore exercise.
//
// There is no supported historical migration baseline: old development state
// is rejected by the owning stores (taskstore.Open), not upgraded here. The
// manifest test checks selected fields only; it is not full JSON Schema
// validation.
package compatibility_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nebler/fern/internal/taskstore"
	_ "modernc.org/sqlite"
)

func TestFreshTaskStoreIsCurrentSchema(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "task-store.sqlite")
	store, err := taskstore.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var version, entries int
	var name, checksum, integrity string
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT count(*),name,checksum FROM schema_migrations`).Scan(&entries, &name, &checksum); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	var requiredTables int
	if err := database.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='table' AND name IN
('workspaces','runs','receipts','results')`).Scan(&requiredTables); err != nil {
		t.Fatal(err)
	}
	digest, digestErr := hex.DecodeString(checksum)
	if version != 8 || version != taskstore.CurrentSchemaVersion() || entries != 1 || name != "run_task_store" || digestErr != nil || len(digest) != sha256.Size || integrity != "ok" || requiredTables != 4 {
		t.Fatalf("version=%d current=%d entries=%d name=%q checksum=%q integrity=%q required_tables=%d",
			version, taskstore.CurrentSchemaVersion(), entries, name, checksum, integrity, requiredTables)
	}
	var obsoleteTables int
	if err := database.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='table' AND
(name IN ('tasks','attempts','background_runs','verifications') OR name LIKE '%publication%')`).Scan(&obsoleteTables); err != nil {
		t.Fatal(err)
	}
	if obsoleteTables != 0 {
		t.Fatalf("fresh schema retains %d obsolete tables", obsoleteTables)
	}
	rows, err := database.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("fresh schema has a foreign-key violation")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestCompatibilityManifestDeclaresPreReleaseSchemaReset(t *testing.T) {
	var schema struct {
		Properties struct {
			CurrentReleaseSchemas struct {
				AdditionalProperties bool `json:"additionalProperties"`
				Properties           struct {
					TaskStore struct {
						Const int `json:"const"`
					} `json:"task_store"`
				} `json:"properties"`
			} `json:"current_release_schemas"`
		} `json:"properties"`
	}
	var manifest struct {
		FirstSupportedBaseline any `json:"first_supported_baseline"`
		CurrentReleaseSchemas  struct {
			TaskStore int `json:"task_store"`
		} `json:"current_release_schemas"`
	}
	root := filepath.Join("..", "..", "deploy", "release")
	decodeJSONFile(t, filepath.Join(root, "compatibility-manifest.schema.json"), &schema)
	decodeJSONFile(t, filepath.Join(root, "compatibility-manifest.json"), &manifest)
	want := taskstore.CurrentSchemaVersion()
	if manifest.FirstSupportedBaseline != nil || schema.Properties.CurrentReleaseSchemas.AdditionalProperties ||
		schema.Properties.CurrentReleaseSchemas.Properties.TaskStore.Const != want || manifest.CurrentReleaseSchemas.TaskStore != want {
		t.Fatalf("baseline=%v schema=%d manifest=%d current=%d additional_properties=%t", manifest.FirstSupportedBaseline,
			schema.Properties.CurrentReleaseSchemas.Properties.TaskStore.Const, manifest.CurrentReleaseSchemas.TaskStore,
			want, schema.Properties.CurrentReleaseSchemas.AdditionalProperties)
	}
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
