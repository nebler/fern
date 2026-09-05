package taskstore

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"
)

func TestMigrationTenQuarantinesEveryLegacyResultReadyPhase(t *testing.T) {
	phases := []BackgroundRunEffectPhase{
		BackgroundRunEffectPromptAdmitted,
		BackgroundRunEffectStopIntent,
		BackgroundRunEffectWriterInactive,
		BackgroundRunEffectRouteRemoved,
		BackgroundRunEffectContainerRemoved,
		BackgroundRunEffectVolumeRemoved,
		BackgroundRunEffectCloneRemoved,
		BackgroundRunEffectCleanupComplete,
	}
	for index, target := range phases {
		t.Run(string(target), func(t *testing.T) {
			path := testDBPath(t)
			store := openVersionNineBackgroundDatabase(t, path)
			params := testBackgroundRunAdmission(5300+index, "migration-ten-"+string(target))
			if _, err := store.AdmitBackgroundRun(context.Background(), params); err != nil {
				t.Fatal(err)
			}
			now := testTime.Truncate(time.Millisecond).Add(time.Minute)
			promptIntent := seedVersionEightPromptRun(t, store.db, params.TaskID, false, now)
			attemptedAt := promptIntent.Add(time.Millisecond)
			if _, err := store.db.Exec(`UPDATE background_runs SET prompt_request_attempted_at=?,revision=revision+1,updated_at=? WHERE task_id=?`,
				attemptedAt.UnixMilli(), attemptedAt.UnixMilli(), params.TaskID); err != nil {
				t.Fatal(err)
			}
			admittedAt := attemptedAt.Add(time.Millisecond)
			if _, err := store.db.Exec(`UPDATE background_runs SET state='working',effect_phase='prompt_admitted',prompt_admitted_at=?,
prompt_evidence='legacy admitted',last_evidence='legacy admitted',revision=revision+1,updated_at=? WHERE task_id=?`,
				admittedAt.UnixMilli(), admittedAt.UnixMilli(), params.TaskID); err != nil {
				t.Fatal(err)
			}
			at := now.Add(20 * time.Second)
			if _, err := store.db.Exec(`UPDATE background_runs SET state='result_ready',revision=revision+1,updated_at=? WHERE task_id=?`, at.UnixMilli(), params.TaskID); err != nil {
				t.Fatal(err)
			}
			steps := []struct {
				phase      BackgroundRunEffectPhase
				assignment string
			}{
				{BackgroundRunEffectStopIntent, `stop_intent_at=?,last_evidence='legacy stop'`},
				{BackgroundRunEffectWriterInactive, `writer_inactive_at=?,writer_inactive_evidence='legacy inactive'`},
				{BackgroundRunEffectRouteRemoved, `route_removed_at=?,route_removed_evidence='legacy route'`},
				{BackgroundRunEffectContainerRemoved, `container_removed_at=?,container_removed_evidence='legacy container'`},
				{BackgroundRunEffectVolumeRemoved, `volume_removed_at=?,volume_removed_evidence='legacy volume'`},
				{BackgroundRunEffectCloneRemoved, `clone_removed_at=?,clone_removed_evidence='legacy clone'`},
				{BackgroundRunEffectCleanupComplete, `cleanup_completed_at=?,cleanup_proof='legacy cleanup',claim_owner=NULL,claim_expires_at=NULL`},
			}
			for stepIndex, step := range steps {
				if target == BackgroundRunEffectPromptAdmitted {
					break
				}
				at = at.Add(time.Millisecond)
				query := `UPDATE background_runs SET effect_phase=?,` + step.assignment + `,revision=revision+1,updated_at=? WHERE task_id=?`
				if _, err := store.db.Exec(query, step.phase, at.UnixMilli(), at.UnixMilli(), params.TaskID); err != nil {
					t.Fatalf("seed legacy phase %s: %v", step.phase, err)
				}
				if step.phase == target || stepIndex == len(steps)-1 {
					break
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openTestStore(t, path)
			defer store.Close()
			migrated, err := store.GetBackgroundRun(context.Background(), testWorkspaceID(), params.TaskID, params.Claim.Actor)
			if err != nil || migrated.State != BackgroundRunCleanupRequired || migrated.ResultAuthorityPhase != "legacy_result_not_retained" ||
				migrated.LastError != "legacy_result_not_retained" || migrated.RetainedArtifactID != "" || migrated.RetainedResultID != "" {
				t.Fatalf("migrated legacy phase %s = %+v, error=%v", target, migrated, err)
			}
			if target == BackgroundRunEffectPromptAdmitted && migrated.EffectPhase != BackgroundRunEffectStopIntent {
				t.Fatalf("prompt legacy phase = %s", migrated.EffectPhase)
			}
			if target == BackgroundRunEffectCleanupComplete && migrated.EffectPhase != BackgroundRunEffectCloneRemoved {
				t.Fatalf("complete legacy phase = %s", migrated.EffectPhase)
			}
		})
	}
}

func openVersionNineBackgroundDatabase(t *testing.T, path string) *Store {
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
	for _, migration := range migrations[:9] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("apply fixture migration %d: %v", migration.version, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(version,name,checksum) VALUES(?,?,?)`,
			migration.version, migration.name, migrationChecksum(migration)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version=9`); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db, path: path}
	createTestWorkspace(t, store)
	return store
}
