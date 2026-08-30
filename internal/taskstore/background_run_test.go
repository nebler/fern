package taskstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

func TestBackgroundRunAdmissionStopAndRestart(t *testing.T) {
	path := testDBPath(t)
	store := openTestStore(t, path)
	createTestWorkspace(t, store)
	params := testBackgroundRunAdmission(1500, "run-create")
	admission, err := store.AdmitTask(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.GetBackgroundRun(context.Background(), testWorkspaceID(), admission.Task.ID, params.Claim.Actor)
	if err != nil || run.TaskID != admission.Task.ID || run.AttemptID != admission.Attempt.ID || run.Generation != admission.Attempt.Sequence ||
		run.BaseOID != admission.Attempt.BaseSHA || run.ImageIdentity != admission.Attempt.ImageDigest || run.State != BackgroundRunQueued || run.EffectPhase != "absent" {
		t.Fatalf("background run = %+v, error = %v", run, err)
	}
	if _, err := store.FindPreparedAttempt(context.Background(), testWorkspaceID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("background run reached persistent delivery manager: %v", err)
	}
	stopHash := sha256.Sum256([]byte("stop"))
	stopParams := StopBackgroundRunParams{WorkspaceID: testWorkspaceID(), TaskID: run.TaskID, ReceiptID: testReceiptID(1600),
		AttemptEventID: testEventID(1601), TaskEventID: testEventID(1602),
		Claim: params.Claim, APIContractVersion: "run-v1", StoppedAt: testTime.Truncate(time.Millisecond).Add(time.Minute)}
	stopParams.Claim.Scope.CommandKind = StopBackgroundRunCommand
	stopParams.Claim.Key = "run-stop"
	stopParams.Claim.RequestHash = stopHash
	stopped, err := store.StopBackgroundRun(context.Background(), stopParams)
	if err != nil || stopped.Run.State != BackgroundRunFailed || stopped.Run.CancelEpoch != 1 || stopped.Run.StopReceiptID != stopParams.ReceiptID {
		t.Fatalf("stop = %+v, error = %v", stopped, err)
	}
	replay, err := store.StopBackgroundRun(context.Background(), stopParams)
	if err != nil || !replay.Replayed || replay.Receipt.ID != stopped.Receipt.ID {
		t.Fatalf("stop replay = %+v, error = %v", replay, err)
	}
	wrongTarget := stopParams
	wrongTarget.TaskID = testTaskID(1699)
	if _, err := store.StopBackgroundRun(context.Background(), wrongTarget); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("stop replay target mismatch = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	persisted, err := store.GetBackgroundRun(context.Background(), testWorkspaceID(), run.TaskID, params.Claim.Actor)
	if err != nil || persisted.State != BackgroundRunFailed || persisted.Revision != 2 {
		t.Fatalf("restarted run = %+v, error = %v", persisted, err)
	}
	var taskState, attemptState, taskReason, attemptReason string
	if err := store.db.QueryRow(`SELECT t.state,a.state,t.terminal_reason,a.terminal_reason FROM tasks t JOIN attempts a ON a.id=t.current_attempt_id WHERE t.id=?`, run.TaskID).
		Scan(&taskState, &attemptState, &taskReason, &attemptReason); err != nil || taskState != "failed" || attemptState != "failed" ||
		taskReason != BackgroundRunStoppedBeforeStart || attemptReason != BackgroundRunStoppedBeforeStart {
		t.Fatalf("terminal projection = %q %q %q %q, error = %v", taskState, attemptState, taskReason, attemptReason, err)
	}
}

func TestBackgroundRunAdmissionIsAtomicAndActorFiltered(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	first := testBackgroundRunAdmission(1700, "first-run")
	if _, err := store.AdmitTask(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := testBackgroundRunAdmission(1701, "second-run")
	second.BackgroundRun.CloneIdentity = first.BackgroundRun.CloneIdentity
	if _, err := store.AdmitTask(context.Background(), second); err == nil {
		t.Fatal("duplicate environment identity did not abort admission")
	}
	assertCounts(t, store, 1, 1, 1, 2)
	var runs int
	if err := store.db.QueryRow(`SELECT count(*) FROM background_runs`).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("run count = %d, error = %v", runs, err)
	}
	other := first.Claim.Actor
	other.ID, other.CredentialID = "pc_other", "pc_other"
	listed, err := store.ListBackgroundRuns(context.Background(), testWorkspaceID(), other, 100)
	if err != nil || len(listed) != 0 {
		t.Fatalf("cross-credential list = %+v, error = %v", listed, err)
	}
	if _, err := store.GetBackgroundRun(context.Background(), testWorkspaceID(), first.TaskID, other); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-credential read = %v", err)
	}
	if _, err := store.db.Exec(`UPDATE background_runs SET repository_remote='https://github.com/other/repo' WHERE task_id=?`, first.TaskID); err == nil {
		t.Fatal("immutable run input changed")
	}
}

func TestBackgroundRunIsExcludedFromLegacyTaskDomain(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	params := testBackgroundRunAdmission(1800, "legacy-isolation")
	admission, err := store.AdmitTask(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if listed, err := store.ListTasks(context.Background(), testWorkspaceID(), 100); err != nil || len(listed) != 0 {
		t.Fatalf("legacy task list = %+v, error = %v", listed, err)
	}
	if _, err := store.GetTask(context.Background(), admission.Task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy exact task read = %v", err)
	}
	if _, err := store.GetAttempt(context.Background(), admission.Attempt.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy exact attempt read = %v", err)
	}
	if _, err := store.GetReceipt(context.Background(), admission.Receipt.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy exact receipt read = %v", err)
	}
	if _, err := store.GetTaskSnapshot(context.Background(), testWorkspaceID(), admission.Task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy task snapshot = %v", err)
	}
	cancel := testCancellation(admission.Task.ID, 1810, "legacy-cancel", "stop")
	if _, err := store.RequestCancellation(context.Background(), cancel); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy cancellation = %v", err)
	}
	if _, err := store.InspectCancellation(context.Background(), admission.Task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy cancellation read = %v", err)
	}
	if _, err := store.FindPendingCancellation(context.Background(), testWorkspaceID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending cancellation = %v", err)
	}
	if _, err := store.InspectDeliveryAttempt(context.Background(), admission.Attempt.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy delivery read = %v", err)
	}
	if page, err := store.ListEvents(context.Background(), testWorkspaceID(), 0, 100); err != nil || len(page.Events) != 0 || page.Watermark != 0 || !page.CaughtUp {
		t.Fatalf("legacy event list = %+v, error = %v", page, err)
	}
}

func TestBackgroundRunWorkspaceFenceAndLifecycleAlgebra(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	params := testBackgroundRunAdmission(1900, "lifecycle")
	admission, err := store.AdmitTask(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	otherWorkspace := task.WorkspaceID("wsp_0198d34d-6a50-75fb-b1f2-000000000002")
	if _, err := store.GetBackgroundRun(context.Background(), otherWorkspace, admission.Task.ID, params.Claim.Actor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace read = %v", err)
	}
	wrongWorkspaceStop := StopBackgroundRunParams{WorkspaceID: otherWorkspace, TaskID: admission.Task.ID, ReceiptID: testReceiptID(1910),
		AttemptEventID: testEventID(1911), TaskEventID: testEventID(1912), Claim: params.Claim,
		APIContractVersion: "run-v1", StoppedAt: testTime.Truncate(time.Millisecond).Add(time.Minute)}
	wrongWorkspaceStop.Claim.Scope.WorkspaceID = otherWorkspace
	wrongWorkspaceStop.Claim.Scope.CommandKind = StopBackgroundRunCommand
	wrongWorkspaceStop.Claim.Key = "wrong-workspace-stop"
	wrongWorkspaceStop.Claim.RequestHash = sha256.Sum256([]byte("wrong-workspace-stop"))
	if _, err := store.StopBackgroundRun(context.Background(), wrongWorkspaceStop); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace stop = %v", err)
	}
	now := testTime.Truncate(time.Millisecond).Add(time.Minute)
	updates := []struct {
		state BackgroundRunState
		phase BackgroundRunEffectPhase
	}{
		{BackgroundRunSettingUp, BackgroundRunEffectProvisionStarted},
		{BackgroundRunWorking, BackgroundRunEffectPromptStarted},
		{BackgroundRunNeedsYou, BackgroundRunEffectPromptStarted},
		{BackgroundRunWorking, BackgroundRunEffectPromptStarted},
		{BackgroundRunUncertain, BackgroundRunEffectPromptStarted},
	}
	for index, update := range updates {
		if _, err := store.db.Exec(`UPDATE background_runs SET state=?,effect_phase=?,revision=revision+1,updated_at=? WHERE task_id=?`,
			update.state, update.phase, now.Add(time.Duration(index)*time.Millisecond).UnixMilli(), admission.Task.ID); err != nil {
			t.Fatalf("transition %d to %s/%s: %v", index, update.state, update.phase, err)
		}
	}
	if _, err := store.db.Exec(`UPDATE background_runs SET state='queued',effect_phase='absent',revision=revision+1,updated_at=? WHERE task_id=?`,
		now.Add(10*time.Millisecond).UnixMilli(), admission.Task.ID); err == nil {
		t.Fatal("backward lifecycle transition succeeded")
	}
	var actorID int64
	if err := store.db.QueryRow(`SELECT creator_actor_snapshot_id FROM background_runs WHERE task_id=?`, admission.Task.ID).Scan(&actorID); err != nil {
		t.Fatal(err)
	}
	receiptID := testReceiptID(1901)
	stopAt := now.Add(20 * time.Millisecond).UnixMilli()
	stopHash := sha256.Sum256([]byte("effectful-stop"))
	if _, err := store.db.Exec(`INSERT INTO receipts(id,workspace_id,command_kind,state,idempotency_key,request_hash,actor_snapshot_id,accepted_at,api_contract_version,target_type,target_id,response_status,response_projection)
VALUES(?,?,?,'accepted',?,?,?,?,?,'task',?,202,?)`, receiptID, testWorkspaceID(), StopBackgroundRunCommand, "effectful-stop", stopHash[:], actorID,
		stopAt, "run-v1", admission.Task.ID, `{"run_id":"`+string(admission.Task.ID)+`","state":"canceling"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE background_runs SET state='canceling',effect_phase='stop_started',cancel_epoch=1,stop_receipt_id=?,stop_actor_snapshot_id=?,stop_requested_at=?,revision=revision+1,updated_at=? WHERE task_id=?`,
		receiptID, actorID, stopAt, stopAt, admission.Task.ID); err != nil {
		t.Fatalf("effectful stop transition: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE background_runs SET state='result_ready',effect_phase='export_started',revision=revision+1,updated_at=? WHERE task_id=?`,
		stopAt+1, admission.Task.ID); err != nil {
		t.Fatalf("cancel finalization transition: %v", err)
	}
}

func TestBackgroundRunAdmissionRejectsMismatchedIntentAtomically(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*AdmitTaskParams)
	}{
		{"instruction hash", func(p *AdmitTaskParams) { p.BackgroundRun.InstructionSHA256 = [32]byte{} }},
		{"profile hash", func(p *AdmitTaskParams) { p.BackgroundRun.ProfileSHA256 = [32]byte{} }},
		{"creator actor", func(p *AdmitTaskParams) { p.Claim.Actor.Type = task.ActorOperator }},
		{"environment identity", func(p *AdmitTaskParams) { p.BackgroundRun.ContainerIdentity += "-other" }},
		{"noncanonical remote", func(p *AdmitTaskParams) { p.BackgroundRun.RepositoryRemote += ".git" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t, testDBPath(t))
			t.Cleanup(func() { _ = store.Close() })
			createTestWorkspace(t, store)
			params := testBackgroundRunAdmission(1950, "invalid-"+test.name)
			test.mutate(&params)
			if _, err := store.AdmitTask(context.Background(), params); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("admission error = %v", err)
			}
			assertCounts(t, store, 0, 0, 0, 0)
		})
	}
	t.Run("repository binding SQL", func(t *testing.T) {
		store := openTestStore(t, testDBPath(t))
		t.Cleanup(func() { _ = store.Close() })
		createTestWorkspace(t, store)
		params := testBackgroundRunAdmission(1960, "repository-binding")
		params.BackgroundRun.RepositoryRemote = "https://github.com/other/repository"
		if _, err := store.AdmitTask(context.Background(), params); err == nil {
			t.Fatal("workspace repository mismatch admitted")
		}
		assertCounts(t, store, 0, 0, 0, 0)
	})
}

func testBackgroundRunAdmission(n int, key string) AdmitTaskParams {
	params := testAdmission(n, key, "Run in the background")
	params.Claim.Scope.CommandKind = CreateBackgroundRunCommand
	params.Claim.Actor.Type = "opencode"
	params.Claim.Actor.ID = "pc_owner"
	params.Claim.Actor.CredentialID = "pc_owner"
	params.Claim.Actor.Authentication = "fern_plugin_bearer"
	params.Claim.RequestHash = sha256.Sum256([]byte(key))
	compact := strings.ReplaceAll(strings.TrimPrefix(string(params.TaskID), "tsk_"), "-", "")
	params.BackgroundRun = &BackgroundRunIntent{
		RepositoryRemote: "https://github.com/owner/repository", Branch: "main",
		InstructionSHA256: sha256.Sum256([]byte(params.Prompt)), Profile: "opencode-1.18.16",
		ProfileSHA256: sha256.Sum256([]byte("opencode-1.18.16")),
		CloneIdentity: "run-" + compact + "-g1-clone", VolumeIdentity: "fern-run-" + compact + "-g1-opencode",
		ContainerIdentity: "fern-run-" + compact + "-g1", EndpointIdentity: "run-" + compact + "-g1-endpoint",
	}
	return params
}
