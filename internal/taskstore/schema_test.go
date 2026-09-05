package taskstore

import (
	"context"
	"testing"
	"time"
)

func TestInitialSchemaIsTheOnlySupportedSchema(t *testing.T) {
	const expectedChecksum = "97cd41f3a8bead5f77954878a64fd9e70d6d7a8128507e3dfaa10ac2949db274"
	if CurrentSchemaVersion() != 1 || len(migrations) != 1 || migrations[0].version != 1 || migrations[0].name != "initial_task_store" {
		t.Fatalf("schema version=%d migrations=%+v", CurrentSchemaVersion(), migrations)
	}
	if checksum := migrationChecksum(migrations[0]); checksum != expectedChecksum {
		t.Fatalf("schema-1 checksum=%q, want %q", checksum, expectedChecksum)
	}
	store := openTestStore(t, testDBPath(t))
	defer store.Close()
	var version, entries int
	var name, checksum string
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*),name,checksum FROM schema_migrations`).Scan(&entries, &name, &checksum); err != nil {
		t.Fatal(err)
	}
	if version != 1 || entries != 1 || name != migrations[0].name || checksum != expectedChecksum {
		t.Fatalf("version=%d entries=%d name=%q checksum=%q", version, entries, name, checksum)
	}
}

func TestInitialSchemaRejectsPromptAdmissionWithoutAttemptFence(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	defer store.Close()
	createTestWorkspace(t, store)
	params := testBackgroundRunAdmission(2910, "initial-schema-prompt-fence")
	if _, err := store.AdmitBackgroundRun(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	now := testTime.Truncate(time.Millisecond).Add(time.Minute)
	run, _ := advanceBackgroundRunToPromptIntent(t, store, params.BackgroundRun.ImageIdentity, now)
	if _, err := store.db.Exec(`UPDATE background_runs SET state='working',effect_phase='prompt_admitted',
prompt_admitted_at=?,prompt_evidence='raw admission',last_evidence='raw admission',revision=revision+1,updated_at=?
WHERE task_id=?`, now.Add(20*time.Second).UnixMilli(), now.Add(20*time.Second).UnixMilli(), run.TaskID); err == nil {
		t.Fatal("raw prompt admission without the one-shot fence was accepted")
	}
}
