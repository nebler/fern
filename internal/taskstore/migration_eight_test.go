package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/nebler/fern/internal/task"
)

type legacyBackgroundRunCase struct {
	state   BackgroundRunState
	phase   BackgroundRunEffectPhase
	stopped bool
}

func TestMigrationEightAcceptsEveryVersionSevenStatePhase(t *testing.T) {
	path := testDBPath(t)
	store := openVersionSevenBackgroundDatabase(t, path)
	cases := legacyBackgroundRunCases()
	seedLegacyBackgroundRuns(t, store, cases)

	var receiptsBefore, actorsBefore, eventsBefore int
	for table, destination := range map[string]*int{
		"receipts":        &receiptsBefore,
		"actor_snapshots": &actorsBefore,
		"events":          &eventsBefore,
	} {
		if err := store.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(destination); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != CurrentSchemaVersion() {
		t.Fatalf("migrated version = %d, error = %v", version, err)
	}

	for index, legacy := range cases {
		taskID := testTaskID(50000 + index)
		attemptID := testAttemptID(50000 + index)
		var state BackgroundRunState
		var phase BackgroundRunEffectPhase
		var profile, image, clone, volume, container, endpoint, taskState, attemptState, taskReason, attemptReason, lastEvidence string
		var runRevision, taskRevision, attemptRevision, latestCursor, cancelEpoch int64
		var cleanupProof sql.NullString
		var stopReceipt sql.NullString
		var stopActor, stopAt sql.NullInt64
		if err := store.db.QueryRow(`SELECT r.state,r.effect_phase,r.profile,r.image_identity,r.clone_identity,r.volume_identity,
r.container_identity,r.endpoint_identity,r.revision,r.cleanup_proof,r.last_evidence,r.cancel_epoch,r.stop_receipt_id,
r.stop_actor_snapshot_id,r.stop_requested_at,t.revision,a.revision,t.latest_event_cursor,t.state,a.state,
coalesce(t.terminal_reason,''),coalesce(a.terminal_reason,'')
FROM background_runs r JOIN tasks t ON t.id=r.task_id JOIN attempts a ON a.id=r.attempt_id WHERE r.task_id=?`, taskID).
			Scan(&state, &phase, &profile, &image, &clone, &volume, &container, &endpoint, &runRevision, &cleanupProof,
				&lastEvidence, &cancelEpoch, &stopReceipt, &stopActor, &stopAt, &taskRevision, &attemptRevision, &latestCursor,
				&taskState, &attemptState, &taskReason, &attemptReason); err != nil {
			t.Fatalf("read migrated %s/%s: %v", legacy.state, legacy.phase, err)
		}
		if profile != "opencode-1.18.16" || image != "sha256:image" ||
			clone != legacyCloneIdentity(taskID) || volume != legacyVolumeIdentity(taskID) ||
			container != legacyContainerIdentity(taskID) || endpoint != legacyEndpointIdentity(taskID) {
			t.Fatalf("immutable legacy tuple changed for %s/%s", legacy.state, legacy.phase)
		}
		if state != BackgroundRunFailed || phase != BackgroundRunEffectCleanupComplete || runRevision != 3 ||
			taskState != "failed" || attemptState != "failed" || taskRevision != 2 || attemptRevision != 2 ||
			taskReason != "legacy_profile_unqualified" || attemptReason != taskReason ||
			!cleanupProof.Valid || cleanupProof.String != "legacy_profile_unqualified:no_schema_7_effect_provider" {
			t.Fatalf("legacy terminalization %s/%s = %s/%s run=%d task=%d attempt=%d proof=%q reason=%q",
				legacy.state, legacy.phase, state, phase, runRevision, taskRevision, attemptRevision, cleanupProof.String, taskReason+"/"+attemptReason)
		}
		var evidence struct {
			Reason            string                   `json:"reason"`
			LegacyState       BackgroundRunState       `json:"legacyState"`
			LegacyEffectPhase BackgroundRunEffectPhase `json:"legacyEffectPhase"`
		}
		if err := json.Unmarshal([]byte(lastEvidence), &evidence); err != nil || evidence.Reason != "legacy_profile_unqualified" ||
			evidence.LegacyState != legacy.state || evidence.LegacyEffectPhase != legacy.phase {
			t.Fatalf("legacy evidence %s/%s = %+v, error=%v", legacy.state, legacy.phase, evidence, err)
		}
		var eventCount, attributedCount int
		if err := store.db.QueryRow(`SELECT count(*),sum(CASE WHEN e.actor_snapshot_id=CASE WHEN r.cancel_epoch=1 THEN r.stop_actor_snapshot_id ELSE r.creator_actor_snapshot_id END THEN 1 ELSE 0 END)
FROM events e JOIN background_runs r ON r.task_id=e.task_id
WHERE e.task_id=? AND e.type IN ('attempt.failed','task.failed') AND json_extract(e.payload,'$.reason')='legacy_profile_unqualified'
AND json_extract(e.payload,'$.legacyState')=? AND json_extract(e.payload,'$.legacyEffectPhase')=?`, taskID, legacy.state, legacy.phase).
			Scan(&eventCount, &attributedCount); err != nil || eventCount != 2 || attributedCount != 2 {
			t.Fatalf("migration events %s/%s = %d attributed=%d error=%v", legacy.state, legacy.phase, eventCount, attributedCount, err)
		}
		var terminalTaskCursor int64
		if err := store.db.QueryRow(`SELECT cursor FROM events WHERE task_id=? AND attempt_id IS NULL AND type='task.failed' AND
json_extract(payload,'$.reason')='legacy_profile_unqualified'`, taskID).Scan(&terminalTaskCursor); err != nil || latestCursor != terminalTaskCursor {
			t.Fatalf("latest terminal cursor %s/%s = %d/%d error=%v", legacy.state, legacy.phase, latestCursor, terminalTaskCursor, err)
		}
		if legacy.stopped {
			expectedReceipt := string(testReceiptID(61000 + index))
			if cancelEpoch != 1 || !stopReceipt.Valid || stopReceipt.String != expectedReceipt || !stopActor.Valid || !stopAt.Valid {
				t.Fatalf("stopped tuple %s/%s = epoch=%d receipt=%q actor=%v at=%v", legacy.state, legacy.phase, cancelEpoch, stopReceipt.String, stopActor, stopAt)
			}
			var linkedEvents int
			if err := store.db.QueryRow(`SELECT count(*) FROM events WHERE task_id=? AND type IN ('attempt.failed','task.failed') AND
json_extract(payload,'$.reason')='legacy_profile_unqualified' AND json_extract(payload,'$.stopReceiptId')=?`, taskID, expectedReceipt).
				Scan(&linkedEvents); err != nil || linkedEvents != 2 {
				t.Fatalf("stop receipt event linkage %s/%s = %d error=%v", legacy.state, legacy.phase, linkedEvents, err)
			}
		} else if cancelEpoch != 0 || stopReceipt.Valid || stopActor.Valid || stopAt.Valid {
			t.Fatalf("unstopped tuple gained stop metadata for %s/%s", legacy.state, legacy.phase)
		}
		var readAttemptState, readAttemptReason string
		if err := store.db.QueryRow(`SELECT state,terminal_reason FROM attempts WHERE id=?`, attemptID).Scan(&readAttemptState, &readAttemptReason); err != nil ||
			readAttemptState != "failed" || readAttemptReason != "legacy_profile_unqualified" {
			t.Fatalf("attempt terminalization %s/%s = %q/%q error=%v", legacy.state, legacy.phase, readAttemptState, readAttemptReason, err)
		}
	}

	var receiptsAfter, actorsAfter, eventsAfter int
	for table, destination := range map[string]*int{
		"receipts":        &receiptsAfter,
		"actor_snapshots": &actorsAfter,
		"events":          &eventsAfter,
	} {
		if err := store.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(destination); err != nil {
			t.Fatal(err)
		}
	}
	if receiptsAfter != receiptsBefore || actorsAfter != actorsBefore || eventsAfter != eventsBefore+2*len(cases) {
		t.Fatalf("preservation counts receipts %d/%d actors %d/%d events %d/%d",
			receiptsAfter, receiptsBefore, actorsAfter, actorsBefore, eventsAfter, eventsBefore+2*len(cases))
	}
	var preservedCreateReceipts int
	if err := store.db.QueryRow(`SELECT count(*) FROM background_runs b JOIN receipts r
ON r.workspace_id=b.workspace_id AND r.target_id=b.task_id AND r.actor_snapshot_id=b.creator_actor_snapshot_id
WHERE r.command_kind='run.create' AND r.state='accepted' AND r.response_status=202 AND
json_extract(r.response_projection,'$.run_id')=b.task_id AND json_extract(r.response_projection,'$.committed')=1`).Scan(&preservedCreateReceipts); err != nil || preservedCreateReceipts != len(cases) {
		t.Fatalf("preserved create receipts = %d, error = %v", preservedCreateReceipts, err)
	}
}

func TestMigrationEightRollsBackAtomically(t *testing.T) {
	path := testDBPath(t)
	store := openVersionSevenBackgroundDatabase(t, path)
	seedLegacyBackgroundRuns(t, store, []legacyBackgroundRunCase{{BackgroundRunSettingUp, backgroundRunEffectLegacyProvisionStarted, false}})
	var receiptsBefore, actorsBefore int
	if err := store.db.QueryRow(`SELECT count(*) FROM receipts`).Scan(&receiptsBefore); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM actor_snapshots`).Scan(&actorsBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER migration_eight_test_abort BEFORE UPDATE ON tasks
WHEN NEW.terminal_reason='legacy_profile_unqualified'
BEGIN SELECT RAISE(ABORT, 'injected migration failure'); END`); err != nil {
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
	var version, runs, failureEvents, claimColumns, receiptsAfter, actorsAfter int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM background_runs WHERE state='setting_up' AND effect_phase='provision_started'`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM events WHERE type IN ('attempt.failed','task.failed')`).Scan(&failureEvents); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM receipts`).Scan(&receiptsAfter); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM actor_snapshots`).Scan(&actorsAfter); err != nil {
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
		if name == "claim_owner" {
			claimColumns++
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	if version != 7 || runs != 1 || failureEvents != 0 || claimColumns != 0 || receiptsAfter != receiptsBefore || actorsAfter != actorsBefore {
		t.Fatalf("rollback version=%d runs=%d failureEvents=%d claimColumns=%d receipts=%d/%d actors=%d/%d",
			version, runs, failureEvents, claimColumns, receiptsAfter, receiptsBefore, actorsAfter, actorsBefore)
	}
}

func TestMigrationEightRejectsMismatchedLegacyStopReceipt(t *testing.T) {
	path := testDBPath(t)
	store := openVersionSevenBackgroundDatabase(t, path)
	legacy := legacyBackgroundRunCase{BackgroundRunResultReady, backgroundRunEffectLegacyExportStarted, true}
	seedLegacyBackgroundRunsWithStopProjection(t, store, []legacyBackgroundRunCase{legacy}, BackgroundRunResultReady)
	taskID := testTaskID(50000)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if migrated, err := Open(context.Background(), path); err == nil {
		_ = migrated.Close()
		t.Fatal("migration accepted a mismatched stop receipt")
	}
	raw := openRaw(t, path)
	defer raw.Close()
	var version, failureEvents int
	var state BackgroundRunState
	var phase BackgroundRunEffectPhase
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT state,effect_phase FROM background_runs WHERE task_id=?`, taskID).Scan(&state, &phase); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM events WHERE task_id=? AND json_extract(payload,'$.reason')='legacy_profile_unqualified'`, taskID).Scan(&failureEvents); err != nil {
		t.Fatal(err)
	}
	if version != 7 || state != legacy.state || phase != legacy.phase || failureEvents != 0 {
		t.Fatalf("receipt validation rollback version=%d run=%s/%s failureEvents=%d", version, state, phase, failureEvents)
	}
}

func legacyBackgroundRunCases() []legacyBackgroundRunCase {
	return []legacyBackgroundRunCase{
		{BackgroundRunQueued, BackgroundRunEffectAbsent, false},
		{BackgroundRunSettingUp, backgroundRunEffectLegacyProvisionStarted, false},
		{BackgroundRunWorking, backgroundRunEffectLegacyPromptStarted, false},
		{BackgroundRunNeedsYou, backgroundRunEffectLegacyPromptStarted, false},
		{BackgroundRunCanceling, backgroundRunEffectLegacyStopStarted, true},
		{BackgroundRunUncertain, backgroundRunEffectLegacyProvisionStarted, true},
		{BackgroundRunUncertain, backgroundRunEffectLegacyPromptStarted, true},
		{BackgroundRunUncertain, backgroundRunEffectLegacyStopStarted, true},
		{BackgroundRunUncertain, backgroundRunEffectLegacyExportStarted, true},
		{BackgroundRunUncertain, backgroundRunEffectLegacyCleanupStarted, true},
		{BackgroundRunResultReady, backgroundRunEffectLegacyExportStarted, true},
		{BackgroundRunResultReady, backgroundRunEffectLegacyCleanupStarted, true},
		{BackgroundRunFailed, BackgroundRunEffectAbsent, true},
		{BackgroundRunFailed, backgroundRunEffectLegacyProvisionStarted, true},
		{BackgroundRunFailed, backgroundRunEffectLegacyPromptStarted, true},
		{BackgroundRunFailed, backgroundRunEffectLegacyStopStarted, true},
		{BackgroundRunFailed, backgroundRunEffectLegacyExportStarted, true},
		{BackgroundRunFailed, backgroundRunEffectLegacyCleanupStarted, true},
		{BackgroundRunCleanupRequired, backgroundRunEffectLegacyCleanupStarted, true},
	}
}

func openVersionSevenBackgroundDatabase(t *testing.T, path string) *Store {
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
	for _, migration := range migrations[:7] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("apply fixture migration %d: %v", migration.version, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(version,name,checksum) VALUES(?,?,?)`,
			migration.version, migration.name, migrationChecksum(migration)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version=7`); err != nil {
		t.Fatal(err)
	}
	return &Store{db: db, path: path}
}

func seedLegacyBackgroundRuns(t *testing.T, store *Store, cases []legacyBackgroundRunCase) {
	t.Helper()
	seedLegacyBackgroundRunsWithStopProjection(t, store, cases, BackgroundRunCanceling)
}

func seedLegacyBackgroundRunsWithStopProjection(t *testing.T, store *Store, cases []legacyBackgroundRunCase, stopProjectionState BackgroundRunState) {
	t.Helper()
	createTestWorkspace(t, store)
	profile := "opencode-1.18.16"
	profileHash := sha256.Sum256([]byte(profile))
	stopper := task.ActorSnapshot{Type: task.ActorOpenCode, ID: "pc_stopper", DisplayName: "Stopper", CredentialID: "pc_stopper", Authentication: "fern_plugin_bearer", RequestID: "legacy-stop"}
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	stopperID, err := ensureActor(context.Background(), tx, stopper)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for index, legacy := range cases {
		params := testAdmission(50000+index, fmt.Sprintf("legacy-task-%d", index), fmt.Sprintf("legacy prompt %d", index))
		params.Claim.Actor = task.ActorSnapshot{Type: task.ActorOpenCode, ID: "pc_owner", DisplayName: "Owner", CredentialID: "pc_owner", Authentication: "fern_plugin_bearer", RequestID: "legacy-migration"}
		admission, err := store.AdmitTask(context.Background(), params)
		if err != nil {
			t.Fatalf("admit legacy parent %s/%s: %v", legacy.state, legacy.phase, err)
		}
		var actorID int64
		if err := store.db.QueryRow(`SELECT actor_snapshot_id FROM tasks WHERE id=?`, admission.Task.ID).Scan(&actorID); err != nil {
			t.Fatal(err)
		}
		createProjection, _ := json.Marshal(map[string]any{"run_id": admission.Task.ID, "committed": true})
		createHash := sha256.Sum256([]byte(fmt.Sprintf("legacy-run-%d", index)))
		if _, err := store.db.Exec(`INSERT INTO receipts(
id,workspace_id,command_kind,state,idempotency_key,request_hash,actor_snapshot_id,accepted_at,api_contract_version,target_type,target_id,response_status,response_projection)
VALUES(?,?,?,'accepted',?,?,?,?,?,'task',?,202,?)`, testReceiptID(60000+index), testWorkspaceID(), CreateBackgroundRunCommand,
			fmt.Sprintf("legacy-run-%d", index), createHash[:], actorID, admission.Task.CreatedAt.UnixMilli(), "run-v1", admission.Task.ID, string(createProjection)); err != nil {
			t.Fatal(err)
		}
		cancelEpoch := 0
		var stopReceipt any
		var stopActor any
		var stopAt any
		if legacy.stopped {
			cancelEpoch = 1
			stopReceipt = testReceiptID(61000 + index)
			stopActor = stopperID
			stopAt = admission.Task.CreatedAt.UnixMilli()
			stopProjection, _ := json.Marshal(map[string]any{"run_id": admission.Task.ID, "state": stopProjectionState})
			stopHash := sha256.Sum256([]byte(fmt.Sprintf("legacy-stop-%d", index)))
			if _, err := store.db.Exec(`INSERT INTO receipts(
id,workspace_id,command_kind,state,idempotency_key,request_hash,actor_snapshot_id,accepted_at,api_contract_version,target_type,target_id,response_status,response_projection)
VALUES(?,?,?,'accepted',?,?,?,?,?,'task',?,202,?)`, stopReceipt, testWorkspaceID(), StopBackgroundRunCommand,
				fmt.Sprintf("legacy-stop-%d", index), stopHash[:], stopperID, stopAt, "run-v1", admission.Task.ID, string(stopProjection)); err != nil {
				t.Fatal(err)
			}
		}
		branch := "main"
		if _, err := store.db.Exec(`INSERT INTO background_runs(
task_id,attempt_id,workspace_id,generation,repository_id,repository_remote,base_oid,branch,instruction_sha256,
profile,profile_sha256,image_identity,clone_identity,volume_identity,container_identity,endpoint_identity,
opencode_session_id,opencode_message_id,state,effect_phase,cancel_epoch,stop_receipt_id,stop_actor_snapshot_id,
stop_requested_at,creator_actor_snapshot_id,revision,created_at,updated_at)
VALUES(?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?)`,
			admission.Task.ID, admission.Attempt.ID, testWorkspaceID(), admission.Task.RepositoryID,
			"https://github.com/owner/repository", admission.Task.BaseSHA, branch, admission.Task.PromptSHA256[:],
			profile, profileHash[:], admission.Attempt.ImageDigest, legacyCloneIdentity(admission.Task.ID), legacyVolumeIdentity(admission.Task.ID),
			legacyContainerIdentity(admission.Task.ID), legacyEndpointIdentity(admission.Task.ID), admission.Attempt.OpenCodeSessionID,
			admission.Attempt.OpenCodeMessageID, legacy.state, legacy.phase, cancelEpoch, stopReceipt, stopActor, stopAt, actorID,
			admission.Task.CreatedAt.UnixMilli(), admission.Task.UpdatedAt.UnixMilli()); err != nil {
			t.Fatalf("insert legacy run %s/%s: %v", legacy.state, legacy.phase, err)
		}
	}
}

func legacyCloneIdentity(id task.TaskID) string {
	return "run-" + compactLegacyTaskID(id) + "-g1-clone"
}

func legacyVolumeIdentity(id task.TaskID) string {
	return "fern-run-" + compactLegacyTaskID(id) + "-g1-opencode"
}

func legacyContainerIdentity(id task.TaskID) string {
	return "fern-run-" + compactLegacyTaskID(id) + "-g1"
}

func legacyEndpointIdentity(id task.TaskID) string {
	return "run-" + compactLegacyTaskID(id) + "-g1-endpoint"
}

func compactLegacyTaskID(id task.TaskID) string {
	value := string(id)[4:]
	return value[:8] + value[9:13] + value[14:18] + value[19:23] + value[24:]
}
