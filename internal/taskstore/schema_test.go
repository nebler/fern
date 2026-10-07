package taskstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPredecessorSchemasRejectedWithoutMutation(t *testing.T) {
	for _, version := range []int{1, 2, 3, 4, 5, 6, 7, 8} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			path := testDBPath(t)
			raw := openRaw(t, path)
			if _, err := raw.Exec(fmt.Sprintf(`CREATE TABLE old_work (value TEXT); INSERT INTO old_work VALUES ('retained'); PRAGMA user_version=%d`, version)); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			store, err := Open(context.Background(), path)
			if store != nil {
				store.Close()
			}
			if !errors.Is(err, ErrUnsupportedSchema) {
				t.Fatalf("open v%d = %v", version, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("unsupported database changed: %v", err)
			}
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
					t.Fatalf("unsupported open created %s: %v", suffix, err)
				}
			}
		})
	}
}

func TestInitialSchemaIsTheOnlySupportedSchema(t *testing.T) {
	const expectedChecksum = "37161734a190f18c7b235909fee12028e1bae67423467fee73879d72da36b26e"
	if CurrentSchemaVersion() != 9 || len(migrations) != 1 || migrations[0].version != 9 || migrations[0].name != "fern_state" {
		t.Fatalf("schema version=%d migration count=%d", CurrentSchemaVersion(), len(migrations))
	}
	if checksum := migrationChecksum(migrations[0]); checksum != expectedChecksum {
		t.Fatalf("schema checksum=%q, want %q", checksum, expectedChecksum)
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
	if version != CurrentSchemaVersion() || entries != 1 || name != migrations[0].name || checksum != expectedChecksum {
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
	run, _ := advanceBackgroundRunToRuntime(t, store, now)
	if _, err := store.db.Exec(`UPDATE runs SET state='working',effect_phase='admitted',
last_evidence='raw admission',revision=revision+1,updated_at=?
WHERE id=?`, now.Add(20*time.Second).UnixMilli(), run.RunID); err == nil {
		t.Fatal("raw prompt admission without the one-shot fence was accepted")
	}
}
