package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestOtherSchemaVersionsRejectedWithoutMutation(t *testing.T) {
	for _, version := range []int{1, schemaVersion - 1, schemaVersion + 1} {
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

func TestSchemaIsPinned(t *testing.T) {
	const expectedChecksum = "13c247c0f623c59b4081f3899aa46e452afeb316acf72eb914d5ed5e4ee62956"
	if sum := sha256.Sum256([]byte(schema)); hex.EncodeToString(sum[:]) != expectedChecksum {
		t.Fatalf("schema checksum=%x; bump schemaVersion and update the pin", sum)
	}
	store := openTestStore(t, testDBPath(t))
	defer store.Close()
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("user_version=%d, %v", version, err)
	}
}

func TestInitialSchemaRejectsPromptAdmissionWithoutAttemptFence(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	defer store.Close()
	createTestWorkspace(t, store)
	params := testRunAdmission(2910, "initial-schema-prompt-fence")
	if _, err := store.AdmitRun(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	now := testTime.Truncate(time.Millisecond).Add(time.Minute)
	run, _ := advanceRunToRuntime(t, store, now)
	if _, err := store.db.Exec(`UPDATE runs SET state='working',effect_phase='admitted',
last_evidence='raw admission',revision=revision+1,updated_at=?
WHERE id=?`, now.Add(20*time.Second).UnixMilli(), run.RunID); err == nil {
		t.Fatal("raw prompt admission without the one-shot fence was accepted")
	}
}
