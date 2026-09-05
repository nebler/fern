package taskstore

import (
	"os"
	"testing"
)

func TestExecutionResultMigrationFromVersionOne(t *testing.T) {
	path := testDBPath(t)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	raw := openRaw(t, path)
	if _, err := raw.Exec(initialSchema); err != nil {
		t.Fatalf("install v1 schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_migrations(version,name,checksum) VALUES(1,?,?)`, migrations[0].name, migrationChecksum(migrations[0])); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	var version, resultTables, sealedColumns int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('results','result_manifest')`).Scan(&resultTables); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT (SELECT count(*) FROM pragma_table_info('tasks') WHERE name='sealed_result_id') + (SELECT count(*) FROM pragma_table_info('attempts') WHERE name='sealed_result_id')`).Scan(&sealedColumns); err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion() || resultTables != 2 || sealedColumns != 2 {
		t.Fatalf("migration projection version=%d tables=%d columns=%d", version, resultTables, sealedColumns)
	}
}
