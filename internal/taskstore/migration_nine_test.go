package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"
)

func TestMigrationNineBackfillsSchemaEightPromptFences(t *testing.T) {
	for _, test := range []struct {
		name     string
		n        int
		admitted bool
		terminal bool
	}{
		{"prompt_intent", 2900, false, false},
		{"prompt_admitted", 2901, true, false},
		{"terminal_result", 2902, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := testDBPath(t)
			store := openVersionEightBackgroundDatabase(t, path)
			params := testBackgroundRunAdmission(test.n, "migration-nine-"+test.name)
			if _, err := store.AdmitBackgroundRun(context.Background(), params); err != nil {
				t.Fatal(err)
			}
			want := seedVersionEightPromptRun(t, store.db, params.TaskID, test.admitted, testTime.Add(time.Minute))
			if test.terminal {
				seedVersionEightTerminalPromptRun(t, store.db, params.TaskID, testTime.Add(time.Minute+9*time.Millisecond))
			}
			stripVersionNineFixtureColumn(t, store.db)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openTestStore(t, path)
			defer store.Close()
			run, err := store.GetBackgroundRun(context.Background(), testWorkspaceID(), params.TaskID, params.Claim.Actor)
			if err != nil || run.PromptRequestAttemptedAt == nil || !run.PromptRequestAttemptedAt.Equal(want) {
				t.Fatalf("migrated prompt fence for %s = %+v, error=%v", params.TaskID, run.PromptRequestAttemptedAt, err)
			}
			if run.ResourceSpecVersion != 8 || run.EnvironmentSHA256 != sha256.Sum256([]byte("{}")) {
				t.Fatalf("migrated resource identity for %s = version %d hash %x", params.TaskID, run.ResourceSpecVersion, run.EnvironmentSHA256)
			}
			if _, err := store.db.Exec(`UPDATE background_runs SET prompt_request_attempted_at=prompt_request_attempted_at+1,
revision=revision+1,updated_at=updated_at+1 WHERE task_id=?`, params.TaskID); err == nil {
				t.Fatalf("migrated prompt fence for %s was mutable", params.TaskID)
			}
			if _, err := store.db.Exec(`UPDATE background_runs SET environment_sha256=zeroblob(32),
revision=revision+1,updated_at=updated_at+1 WHERE task_id=?`, params.TaskID); err == nil {
				t.Fatalf("migrated environment identity for %s was mutable", params.TaskID)
			}
		})
	}
}

func seedVersionEightTerminalPromptRun(t *testing.T, db *sql.DB, taskID interface{}, now time.Time) {
	t.Helper()
	now = now.UTC().Truncate(time.Millisecond)
	statements := []string{
		`UPDATE background_runs SET state='result_ready',revision=revision+1,updated_at=? WHERE task_id=?`,
		`UPDATE background_runs SET effect_phase='stop_intent',stop_intent_at=?,revision=revision+1,updated_at=? WHERE task_id=?`,
		`UPDATE background_runs SET effect_phase='writer_inactive',writer_inactive_at=?,writer_inactive_evidence='inactive',revision=revision+1,updated_at=? WHERE task_id=?`,
		`UPDATE background_runs SET effect_phase='route_removed',route_removed_at=?,route_removed_evidence='route',revision=revision+1,updated_at=? WHERE task_id=?`,
		`UPDATE background_runs SET effect_phase='container_removed',container_removed_at=?,container_removed_evidence='container',revision=revision+1,updated_at=? WHERE task_id=?`,
		`UPDATE background_runs SET effect_phase='volume_removed',volume_removed_at=?,volume_removed_evidence='volume removed',revision=revision+1,updated_at=? WHERE task_id=?`,
		`UPDATE background_runs SET effect_phase='clone_removed',clone_removed_at=?,clone_removed_evidence='clone removed',revision=revision+1,updated_at=? WHERE task_id=?`,
		`UPDATE background_runs SET effect_phase='cleanup_complete',cleanup_completed_at=?,cleanup_proof='complete',claim_owner=NULL,claim_expires_at=NULL,revision=revision+1,updated_at=? WHERE task_id=?`,
	}
	for index, statement := range statements {
		at := now.Add(time.Duration(index) * time.Millisecond).UnixMilli()
		args := []any{at, taskID}
		if index != 0 {
			args = []any{at, at, taskID}
		}
		if _, err := db.Exec(statement, args...); err != nil {
			t.Fatalf("seed schema-8 terminal prompt phase %d: %v", index, err)
		}
	}
}

func TestMigrationNinePromptAdmissionTriggerRejectsMissingFence(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	defer store.Close()
	createTestWorkspace(t, store)
	params := testBackgroundRunAdmission(2910, "migration-nine-trigger")
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

func TestMigrationNineRollsBackAtomically(t *testing.T) {
	path := testDBPath(t)
	store := openVersionEightBackgroundDatabase(t, path)
	params := testBackgroundRunAdmission(2920, "migration-nine-rollback")
	if _, err := store.AdmitBackgroundRun(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	seedVersionEightPromptRun(t, store.db, params.TaskID, false, testTime.Add(time.Minute))
	stripVersionNineFixtureColumn(t, store.db)
	if _, err := store.db.Exec(`CREATE TRIGGER migration_nine_test_abort BEFORE UPDATE ON background_runs
WHEN OLD.prompt_intent_at IS NOT NULL BEGIN SELECT RAISE(ABORT, 'injected migration failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if migrated, err := Open(context.Background(), path); err == nil {
		_ = migrated.Close()
		t.Fatal("migration unexpectedly succeeded")
	}
	raw := openRaw(t, path)
	defer raw.Close()
	var version, fenceColumns int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	rows, err := raw.Query(`PRAGMA table_info(background_runs)`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "prompt_request_attempted_at" || name == "timeout_requested_at" || name == "timeout_actor_snapshot_id" || name == "environment_sha256" || name == "resource_spec_version" {
			fenceColumns++
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	var state BackgroundRunState
	var phase BackgroundRunEffectPhase
	if err := raw.QueryRow(`SELECT state,effect_phase FROM background_runs WHERE task_id=?`, params.TaskID).Scan(&state, &phase); err != nil {
		t.Fatal(err)
	}
	if version != 8 || fenceColumns != 0 || state != BackgroundRunUncertain || phase != BackgroundRunEffectPromptIntent {
		t.Fatalf("rollback version=%d columns=%d run=%s/%s", version, fenceColumns, state, phase)
	}
}

func openVersionEightBackgroundDatabase(t *testing.T, path string) *Store {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:8] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("apply fixture migration %d: %v", migration.version, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(version,name,checksum) VALUES(?,?,?)`,
			migration.version, migration.name, migrationChecksum(migration)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`ALTER TABLE background_runs ADD COLUMN environment_sha256 BLOB`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE background_runs ADD COLUMN resource_spec_version INTEGER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version=8`); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db, path: path}
	createTestWorkspace(t, store)
	return store
}

func stripVersionNineFixtureColumn(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`ALTER TABLE background_runs DROP COLUMN resource_spec_version`); err != nil {
		t.Fatalf("remove schema-9 fixture resource version: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE background_runs DROP COLUMN environment_sha256`); err != nil {
		t.Fatalf("remove schema-9 fixture column: %v", err)
	}
}

func seedVersionEightPromptRun(t *testing.T, db *sql.DB, taskID interface{}, admitted bool, now time.Time) time.Time {
	t.Helper()
	now = now.UTC().Truncate(time.Millisecond)
	statements := []string{
		`UPDATE background_runs SET state='setting_up',effect_phase='provision_intent',claim_owner='migration-worker',
claim_expires_at=?,claim_generation=1,provision_intent_at=?,revision=revision+1,updated_at=? WHERE task_id=?`,
		`UPDATE background_runs SET effect_phase='clone_observed',clone_observed_at=?,clone_evidence='clone',last_evidence='clone',revision=revision+1,updated_at=? WHERE task_id=?`,
		`UPDATE background_runs SET effect_phase='volume_observed',volume_observed_at=?,volume_evidence='volume',last_evidence='volume',revision=revision+1,updated_at=? WHERE task_id=?`,
		`UPDATE background_runs SET effect_phase='container_observed',observed_container_id='container',observed_container_started_at='2026-08-31T12:00:00Z',runtime_epoch=1,host_port=49153,container_observed_at=?,last_evidence='container',revision=revision+1,updated_at=? WHERE task_id=?`,
		`UPDATE background_runs SET effect_phase='health_observed',health_observed_at=?,health_evidence='health',last_evidence='health',revision=revision+1,updated_at=? WHERE task_id=?`,
		`UPDATE background_runs SET effect_phase='ready',ready_at=?,ready_evidence='ready',last_evidence='ready',revision=revision+1,updated_at=? WHERE task_id=?`,
		`UPDATE background_runs SET effect_phase='session_observed',session_observed_at=?,session_evidence='session',last_evidence='session',revision=revision+1,updated_at=? WHERE task_id=?`,
		`UPDATE background_runs SET state='uncertain',effect_phase='prompt_intent',prompt_intent_at=?,last_evidence='intent',revision=revision+1,updated_at=? WHERE task_id=?`,
	}
	for index, statement := range statements {
		at := now.Add(time.Duration(index) * time.Millisecond).UnixMilli()
		var args []any
		if index == 0 {
			args = []any{now.Add(time.Minute).UnixMilli(), at, at, taskID}
		} else {
			args = []any{at, at, taskID}
		}
		if _, err := db.Exec(statement, args...); err != nil {
			t.Fatalf("seed schema-8 prompt phase %d: %v", index, err)
		}
	}
	promptIntentAt := now.Add(7 * time.Millisecond)
	if admitted {
		at := now.Add(8 * time.Millisecond).UnixMilli()
		if _, err := db.Exec(`UPDATE background_runs SET state='working',effect_phase='prompt_admitted',prompt_admitted_at=?,
prompt_evidence='admitted',last_evidence='admitted',revision=revision+1,updated_at=? WHERE task_id=?`, at, at, taskID); err != nil {
			t.Fatalf("seed schema-8 prompt admission: %v", err)
		}
	}
	return promptIntentAt
}
