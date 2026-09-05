package taskstore

import (
	"os"
	"testing"
)

func TestMigrationFourFromVersionThree(t *testing.T) {
	path := testDBPath(t)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	raw := openRaw(t, path)
	for _, migration := range migrations[:3] {
		if _, err := raw.Exec(migration.sql); err != nil {
			t.Fatalf("install migration %d: %v", migration.version, err)
		}
		if _, err := raw.Exec(`INSERT INTO schema_migrations(version,name,checksum) VALUES(?,?,?)`, migration.version, migration.name, migrationChecksum(migration)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`PRAGMA user_version=3`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	var version, tables, columns int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='seal_requests'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM pragma_table_info('results') WHERE name IN ('completion_authority','seal_request_id','authorizer_actor_snapshot_id')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion() || tables != 1 || columns != 3 {
		t.Fatalf("migration version=%d tables=%d columns=%d", version, tables, columns)
	}
}
