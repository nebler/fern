package taskstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	runidentity "github.com/nebler/fern/internal/run"
	"github.com/nebler/fern/internal/task"
)

func TestBackgroundRunAdmissionStopAndRestart(t *testing.T) {
	path := testDBPath(t)
	store := openTestStore(t, path)
	createTestWorkspace(t, store)
	params := testBackgroundRunAdmission(1500, "run-create")
	admission, err := store.AdmitBackgroundRun(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.GetBackgroundRun(context.Background(), testWorkspaceID(), admission.Task.ID, params.Claim.Actor)
	if err != nil || run.TaskID != admission.Task.ID || run.AttemptID != admission.Attempt.ID || run.Generation != admission.Attempt.Sequence ||
		run.BaseOID != admission.Attempt.BaseSHA || run.ImageIdentity != params.BackgroundRun.ImageIdentity || run.ImageIdentity != admission.Attempt.ImageDigest ||
		admission.Attempt.OpenCodeProtocol != BackgroundRunSourceProfile || run.State != BackgroundRunQueued || run.EffectPhase != "absent" {
		t.Fatalf("background run = %+v, error = %v", run, err)
	}
	resources, _ := runidentity.NewResources(run.TaskID, 1)
	if !resources.Matches(run.CloneIdentity, run.VolumeIdentity, run.ContainerIdentity, run.EndpointIdentity) ||
		run.InstructionSHA256 != sha256.Sum256([]byte(params.Prompt)) || run.ProfileSHA256 != sha256.Sum256([]byte(run.Profile)) {
		t.Fatalf("admission did not derive run identities: %+v", run)
	}
	stopHash := sha256.Sum256([]byte("stop"))
	stopParams := StopBackgroundRunParams{WorkspaceID: testWorkspaceID(), TaskID: run.TaskID, ReceiptID: testReceiptID(1600),
		Claim: params.Claim, APIContractVersion: "run-v1", StoppedAt: testTime.Truncate(time.Millisecond).Add(time.Minute)}
	stopParams.Claim.Scope.CommandKind = StopBackgroundRunCommand
	stopParams.Claim.Key = "run-stop"
	stopParams.Claim.RequestHash = stopHash
	stopped, err := store.StopBackgroundRun(context.Background(), stopParams)
	if err != nil || stopped.Run.State != BackgroundRunFailed || stopped.Run.StopReceiptID == "" || stopped.Run.StopReceiptID != stopParams.ReceiptID {
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
	if _, err := store.AdmitBackgroundRun(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := testBackgroundRunAdmission(1701, "second-run")
	second.OpenCodeSessionID = first.OpenCodeSessionID
	if _, err := store.AdmitBackgroundRun(context.Background(), second); err == nil {
		t.Fatal("duplicate session identity did not abort admission")
	}
	assertCounts(t, store, 1, 1, 1)
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
	operator := task.ActorSnapshot{Type: task.ActorOperator, ID: "operator", DisplayName: "Operator",
		CredentialID: "operator", Authentication: "basic", RequestID: "request"}
	listed, err = store.ListBackgroundRuns(context.Background(), testWorkspaceID(), operator, 100)
	if err != nil || len(listed) != 1 || listed[0].TaskID != first.TaskID {
		t.Fatalf("operator list = %+v, error = %v", listed, err)
	}
	if run, err := store.GetBackgroundRun(context.Background(), testWorkspaceID(), first.TaskID, operator); err != nil || run.TaskID != first.TaskID {
		t.Fatalf("operator read = %+v, error = %v", run, err)
	}
	if _, err := store.db.Exec(`UPDATE background_runs SET repository_remote='https://github.com/other/repo' WHERE task_id=?`, first.TaskID); err == nil {
		t.Fatal("immutable run input changed")
	}
}

func TestBackgroundRunListLazyCapacityAndOwnership(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	ctx := context.Background()
	first := testBackgroundRunAdmission(1800, "list-first")
	assertList := func(actor task.ActorSnapshot, limit, count, capacity int, want task.TaskID) {
		t.Helper()
		runs, err := store.ListBackgroundRuns(ctx, testWorkspaceID(), actor, limit)
		if err != nil || runs == nil || len(runs) != count || cap(runs) != capacity {
			t.Fatalf("list: len=%d cap=%d nil=%v err=%v", len(runs), cap(runs), runs == nil, err)
		}
		if count > 0 && runs[0].TaskID != want {
			t.Fatalf("first task = %s, want %s", runs[0].TaskID, want)
		}
	}
	assertList(first.Claim.Actor, MaxBackgroundRunListLimit, 0, 0, "")
	if _, err := store.AdmitBackgroundRun(ctx, first); err != nil {
		t.Fatal(err)
	}
	assertList(first.Claim.Actor, MaxBackgroundRunListLimit, 1, 1, first.TaskID)
	assertList(first.Claim.Actor, 1, 1, 1, first.TaskID)
	second := testBackgroundRunAdmission(1801, "list-second")
	second.Claim.Actor.ID, second.Claim.Actor.CredentialID = "pc_other", "pc_other"
	if _, err := store.AdmitBackgroundRun(ctx, second); err != nil {
		t.Fatal(err)
	}
	// The newer foreign row must not consume the SQL limit before ownership.
	assertList(first.Claim.Actor, 1, 1, 1, first.TaskID)
	operator := task.ActorSnapshot{Type: task.ActorOperator, ID: "operator", DisplayName: "Operator",
		CredentialID: "operator", Authentication: "basic", RequestID: "request"}
	assertList(operator, 1, 1, 1, second.TaskID)
	other := first.Claim.Actor
	other.CredentialID = "pc_absent"
	assertList(other, MaxBackgroundRunListLimit, 0, 0, "")
	for i := 2; i <= MaxBackgroundRunListLimit; i++ {
		p := testBackgroundRunAdmission(1800+i, fmt.Sprintf("list-owned-%d", i))
		if _, err := store.AdmitBackgroundRun(ctx, p); err != nil {
			t.Fatal(err)
		}
		if i == 10 {
			assertList(first.Claim.Actor, MaxBackgroundRunListLimit, 10, 16, p.TaskID)
			assertList(first.Claim.Actor, 7, 7, 7, p.TaskID)
		}
		if i == MaxBackgroundRunListLimit {
			assertList(first.Claim.Actor, MaxBackgroundRunListLimit, i, i, p.TaskID)
		}
	}
	// Invalid bounds are rejected before SQL, even when the context is canceled.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, limit := range []int{-1, 0, MaxBackgroundRunListLimit + 1} {
		runs, err := store.ListBackgroundRuns(canceled, testWorkspaceID(), first.Claim.Actor, limit)
		if !errors.Is(err, ErrInvalidInput) || runs != nil {
			t.Fatalf("invalid limit %d: runs=%v err=%v", limit, runs, err)
		}
	}
}

func TestBackgroundRunWorkspaceFenceAndLifecycleAlgebra(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	params := testBackgroundRunAdmission(1900, "lifecycle")
	admission, err := store.AdmitBackgroundRun(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	otherWorkspace := task.WorkspaceID("wsp_0198d34d-6a50-75fb-b1f2-000000000002")
	if _, err := store.GetBackgroundRun(context.Background(), otherWorkspace, admission.Task.ID, params.Claim.Actor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace read = %v", err)
	}
	wrongWorkspaceStop := StopBackgroundRunParams{WorkspaceID: otherWorkspace, TaskID: admission.Task.ID, ReceiptID: testReceiptID(1910),
		Claim:              params.Claim,
		APIContractVersion: "run-v1", StoppedAt: testTime.Truncate(time.Millisecond).Add(time.Minute)}
	wrongWorkspaceStop.Claim.Scope.WorkspaceID = otherWorkspace
	wrongWorkspaceStop.Claim.Scope.CommandKind = StopBackgroundRunCommand
	wrongWorkspaceStop.Claim.Key = "wrong-workspace-stop"
	wrongWorkspaceStop.Claim.RequestHash = sha256.Sum256([]byte("wrong-workspace-stop"))
	if _, err := store.StopBackgroundRun(context.Background(), wrongWorkspaceStop); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace stop = %v", err)
	}
	started := testTime.Truncate(time.Millisecond).Add(time.Minute)
	if _, err := startNextBackgroundRun(context.Background(), store, started); err != nil {
		t.Fatal(err)
	}
	now := started.Add(time.Second).UnixMilli()
	for name, statement := range map[string]string{
		"skip provisioning":      `UPDATE background_runs SET state='setting_up',effect_phase='admitted',prompt_request_attempted_at=?,revision=revision+1,updated_at=? WHERE task_id=?`,
		"skip cleanup":           `UPDATE background_runs SET state='failed',effect_phase='cleanup_complete',cleanup_proof='x',last_error=?,revision=revision+1,updated_at=? WHERE task_id=?`,
		"prompt without runtime": `UPDATE background_runs SET state='uncertain',effect_phase='prompt_pending',prompt_request_attempted_at=?,revision=revision+1,updated_at=? WHERE task_id=?`,
		"seal without request":   `UPDATE background_runs SET state='canceling',effect_phase='sealing',last_error=?,revision=revision+1,updated_at=? WHERE task_id=?`,
	} {
		if _, err := store.db.Exec(statement, now, now, admission.Task.ID); err == nil {
			t.Fatalf("closed transition %q succeeded", name)
		}
	}
}

func TestBackgroundRunAdmissionRejectsMismatchedIntentAtomically(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*AdmitBackgroundRunParams)
	}{
		{"profile", func(p *AdmitBackgroundRunParams) { p.BackgroundRun.Profile = "other" }},
		{"creator actor", func(p *AdmitBackgroundRunParams) { p.Claim.Actor.Type = task.ActorOperator }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t, testDBPath(t))
			t.Cleanup(func() { _ = store.Close() })
			createTestWorkspace(t, store)
			params := testBackgroundRunAdmission(1950, "invalid-"+test.name)
			test.mutate(&params)
			if _, err := store.AdmitBackgroundRun(context.Background(), params); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("admission error = %v", err)
			}
			assertCounts(t, store, 0, 0, 0)
		})
	}
	t.Run("repository binding SQL", func(t *testing.T) {
		store := openTestStore(t, testDBPath(t))
		t.Cleanup(func() { _ = store.Close() })
		createTestWorkspace(t, store)
		params := testBackgroundRunAdmission(1960, "repository-binding")
		params.BackgroundRun.RepositoryRemote = "https://github.com/other/repository"
		if _, err := store.AdmitBackgroundRun(context.Background(), params); err == nil {
			t.Fatal("workspace repository mismatch admitted")
		}
		assertCounts(t, store, 0, 0, 0)
	})
}

func TestBackgroundRunCapacityRecoveryAndActiveStop(t *testing.T) {
	path := testDBPath(t)
	store := openTestStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	first := testBackgroundRunAdmission(1970, "claim-first")
	second := testBackgroundRunAdmission(1971, "claim-second")
	if _, err := store.AdmitBackgroundRun(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdmitBackgroundRun(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	now := testTime.Truncate(time.Millisecond).Add(time.Minute)
	run, err := startNextBackgroundRun(context.Background(), store, now)
	if err != nil || run.TaskID != first.TaskID || run.State != BackgroundRunSettingUp || run.EffectPhase != BackgroundRunEffectProvisioning {
		t.Fatalf("first start = %+v, error = %v", run, err)
	}
	continued, err := store.NextBackgroundRun(context.Background(), testWorkspaceID(), BackgroundRunSourceProfile)
	if err != nil || continued.TaskID != run.TaskID || continued.Revision != run.Revision {
		t.Fatalf("capacity-one next run = %+v, error=%v", continued, err)
	}
	if _, err := store.StartBackgroundRunProvisioning(context.Background(), backgroundRunRef(run, now)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("restarted provisioning = %v", err)
	}
	staleStart := backgroundRunRef(run, now)
	staleStart.ExpectedRevision--
	if _, err := store.RecordBackgroundRunRuntime(context.Background(), RecordBackgroundRunRuntimeParams{BackgroundRunRef: staleStart,
		ContainerID: "aabbcc", ContainerStartedAt: "2026-08-31T12:01:00Z", RuntimeEpoch: 1, HostPort: 49152, Evidence: "stale start"}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stale revision mutated run: %v", err)
	}
	ref := backgroundRunRef(run, now.Add(2*time.Second))
	run, err = store.RecordBackgroundRunRuntime(context.Background(), RecordBackgroundRunRuntimeParams{
		BackgroundRunRef: ref, ContainerID: "aabbcc", ContainerStartedAt: "2026-08-31T12:01:00Z", RuntimeEpoch: 1,
		HostPort: 49152, Evidence: "exact container inspect",
	})
	if err != nil || run.EffectPhase != BackgroundRunEffectProvisioning || run.ObservedContainerID != "aabbcc" {
		t.Fatalf("runtime record = %+v, error = %v", run, err)
	}
	advanceBackgroundRef(&ref, run)
	ref.Now = ref.Now.Add(time.Second)
	if _, err := store.RecordBackgroundRunRuntime(context.Background(), RecordBackgroundRunRuntimeParams{BackgroundRunRef: ref,
		ContainerID: "ddeeff", ContainerStartedAt: "2026-08-31T12:02:00Z", RuntimeEpoch: 2, HostPort: 49153, Evidence: "replacement"}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("committed runtime was replaced: %v", err)
	}
	run, err = store.RecordBackgroundRunPromptRequestAttempted(context.Background(), ref)
	if err != nil || run.State != BackgroundRunSettingUp || run.EffectPhase != BackgroundRunEffectPromptPending {
		t.Fatalf("prompt fence = %+v, error = %v", run, err)
	}
	restartedAt := now.Add(2 * time.Minute)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	restarted, err := startNextBackgroundRun(context.Background(), store, restartedAt)
	if err != nil || restarted.TaskID != run.TaskID || restarted.Revision != run.Revision {
		t.Fatalf("restarted next run=%+v error=%v", restarted, err)
	}
	run = restarted

	stop := StopBackgroundRunParams{WorkspaceID: testWorkspaceID(), TaskID: run.TaskID, ReceiptID: testReceiptID(1980),
		Claim:              first.Claim,
		APIContractVersion: "run-v1", StoppedAt: restartedAt.Add(time.Second)}
	stop.Claim.Scope.CommandKind = StopBackgroundRunCommand
	stop.Claim.Key = "active-stop"
	stop.Claim.RequestHash = sha256.Sum256([]byte("active-stop"))
	stopped, err := store.StopBackgroundRun(context.Background(), stop)
	if err != nil || stopped.Run.State != BackgroundRunCanceling || stopped.Run.EffectPhase != BackgroundRunEffectCleaning ||
		stopped.Run.Revision != run.Revision+1 || stopped.Run.StopReceiptID == "" {
		t.Fatalf("active stop = %+v, error = %v", stopped, err)
	}
	if replay, replayErr := store.StopBackgroundRun(context.Background(), stop); replayErr != nil || !replay.Replayed || replay.Receipt.ID != stopped.Receipt.ID || string(replay.Receipt.ResponseProjection) != string(stopped.Receipt.ResponseProjection) {
		t.Fatalf("active stop replay = %+v, error = %v", replay, replayErr)
	}
	var taskState, attemptState string
	if err := store.db.QueryRow(`SELECT t.state,a.state FROM tasks t JOIN attempts a ON a.id=t.current_attempt_id WHERE t.id=?`, run.TaskID).Scan(&taskState, &attemptState); err != nil || taskState != "queued" || attemptState != "prepared" {
		t.Fatalf("active stop falsely terminalized task=%q attempt=%q error=%v", taskState, attemptState, err)
	}

	stopRun, err := startNextBackgroundRun(context.Background(), store, stop.StoppedAt.Add(time.Second))
	if err != nil || stopRun.TaskID != stopped.Run.TaskID || stopRun.State != BackgroundRunCanceling || stopRun.StopReceiptID == "" {
		t.Fatalf("stopped next run = %+v, error = %v", stopRun, err)
	}
	// A coordinator that read the run before the stop committed holds a stale
	// revision; the compare-and-swap rejects its write.
	stale := BackgroundRunRef{WorkspaceID: stopRun.WorkspaceID, TaskID: stopRun.TaskID, AttemptID: stopRun.AttemptID,
		Generation: stopRun.Generation, ExpectedRevision: run.Revision,
		ExpectedState: stopRun.State, ExpectedPhase: stopRun.EffectPhase, Now: stop.StoppedAt.Add(2 * time.Second)}
	if _, err := store.MarkBackgroundRunCleanupRequired(context.Background(), MarkBackgroundRunCleanupRequiredParams{
		BackgroundRunRef: stale, Error: "stale cleanup observation",
	}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stale revision mutated run: %v", err)
	}
	cleanupRef := backgroundRunRef(stopRun, stop.StoppedAt.Add(2*time.Second))
	final, err := store.FinalizeBackgroundRunFailure(context.Background(), FinalizeBackgroundRunFailureParams{
		BackgroundRunRef: cleanupRef, Reason: "background_run_stopped", Evidence: "writer inactive and resources absent",
		CleanupProof: "route, container, volume, and clone absent",
	})
	if err != nil || final.State != BackgroundRunFailed || final.EffectPhase != BackgroundRunEffectCleanupComplete {
		t.Fatalf("active finalization = %+v, error = %v", final, err)
	}
	if err := store.db.QueryRow(`SELECT t.state,a.state,t.terminal_reason,a.terminal_reason FROM tasks t JOIN attempts a ON a.id=t.current_attempt_id WHERE t.id=?`, final.TaskID).
		Scan(&taskState, &attemptState, new(string), new(string)); err != nil || taskState != "failed" || attemptState != "failed" {
		t.Fatalf("final parent task=%q attempt=%q error=%v", taskState, attemptState, err)
	}
	if replay, replayErr := store.StopBackgroundRun(context.Background(), stop); replayErr != nil || !replay.Replayed || replay.Receipt.ID != stopped.Receipt.ID {
		t.Fatalf("final stop replay = %+v, error = %v", replay, replayErr)
	}
	next, err := startNextBackgroundRun(context.Background(), store, cleanupRef.Now.Add(time.Second))
	if err != nil || next.TaskID != second.TaskID {
		t.Fatalf("capacity after final cleanup = %+v, error = %v", next, err)
	}
}

func TestNextBackgroundRunRequiresProfileButRecoversAcrossImageRotation(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	first := testBackgroundRunAdmission(2050, "profile-image-first")
	first.BackgroundRun.ImageIdentity = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	second := testBackgroundRunAdmission(2051, "profile-image-second")
	if _, err := store.AdmitBackgroundRun(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdmitBackgroundRun(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	now := testTime.Truncate(time.Millisecond).Add(time.Minute)
	if _, err := store.NextBackgroundRun(context.Background(), testWorkspaceID(), "opencode-1.18.16"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("old profile next run = %v", err)
	}
	started, err := startNextBackgroundRun(context.Background(), store, now)
	if err != nil || started.TaskID != first.TaskID || started.ImageIdentity != first.BackgroundRun.ImageIdentity {
		t.Fatalf("rotated image recovery start = %+v, error = %v", started, err)
	}
	if next, err := startNextBackgroundRun(context.Background(), store, now); err != nil || next.TaskID != first.TaskID {
		t.Fatalf("second image bypassed workspace capacity: %+v, %v", next, err)
	}
}

func TestBackgroundRunWorkProjectionAndPromptAttemptFenceSurviveRestart(t *testing.T) {
	path := testDBPath(t)
	store := openTestStore(t, path)
	createTestWorkspace(t, store)
	params := testBackgroundRunAdmission(2070, "prompt-fence")
	if _, err := store.AdmitBackgroundRun(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	now := testTime.Truncate(time.Millisecond).Add(time.Minute)
	run, ref := advanceBackgroundRunToRuntime(t, store, now)
	work, err := store.NextBackgroundRunWork(context.Background(), testWorkspaceID(), BackgroundRunSourceProfile)
	if err != nil || work.Run.Revision != ref.ExpectedRevision || work.Prompt != params.Prompt ||
		!work.Deadline.Equal(params.Deadline.Truncate(time.Millisecond)) || work.AttemptTimeout != time.Hour {
		t.Fatalf("next work = %+v, error=%v", work, err)
	}
	if run.PromptRequestAttemptedAt != nil {
		t.Fatal("prompt was attempted before the irreversible fence")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	run, err = startNextBackgroundRun(context.Background(), store, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ref = backgroundRunRef(run, now.Add(3*time.Minute))
	run, err = store.RecordBackgroundRunPromptRequestAttempted(context.Background(), ref)
	if err != nil || run.PromptRequestAttemptedAt == nil {
		t.Fatalf("prompt attempt fence = %+v, error=%v", run, err)
	}
	// A coordinator that read the run before the fence holds the provisioning
	// revision; it cannot set the fence, and so dispatch, a second time.
	ref.Now = ref.Now.Add(time.Millisecond)
	if _, err := store.RecordBackgroundRunPromptRequestAttempted(context.Background(), ref); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second prompt attempt fence = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	persisted, err := store.GetBackgroundRun(context.Background(), run.WorkspaceID, run.TaskID, params.Claim.Actor)
	if err != nil || persisted.PromptRequestAttemptedAt == nil || !persisted.PromptRequestAttemptedAt.Equal(*run.PromptRequestAttemptedAt) {
		t.Fatalf("persisted attempt fence = %+v, error=%v", persisted, err)
	}
}

func TestBackgroundRunSystemTimeoutHasNoPluginReceipt(t *testing.T) {
	path := testDBPath(t)
	store := openTestStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	params := testBackgroundRunAdmission(2080, "system-timeout")
	if _, err := store.AdmitBackgroundRun(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	now := params.Deadline.Truncate(time.Millisecond).Add(time.Millisecond)
	work, err := startNextBackgroundRunWork(context.Background(), store, now)
	if err != nil {
		t.Fatal(err)
	}
	early := backgroundRunRef(work.Run, params.Deadline.Add(-time.Second).Truncate(time.Millisecond))
	if _, err := store.RequestBackgroundRunTimeout(context.Background(), early); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("timeout before deadline = %v", err)
	}
	stale := backgroundRunRef(work.Run, now)
	stale.ExpectedRevision--
	if _, err := store.RequestBackgroundRunTimeout(context.Background(), stale); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stale timeout = %v", err)
	}
	timedOut, err := store.RequestBackgroundRunTimeout(context.Background(), backgroundRunRef(work.Run, now))
	if err != nil || timedOut.State != BackgroundRunCleanupRequired || timedOut.EffectPhase != BackgroundRunEffectCleaning ||
		timedOut.TimeoutRequestedAt == nil || timedOut.StopReceiptID != "" {
		t.Fatalf("system timeout = %+v, error=%v", timedOut, err)
	}
	var receipts int
	if err := store.db.QueryRow(`SELECT count(*) FROM receipts WHERE target_id=? AND command_kind='run.stop'`, timedOut.TaskID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 0 {
		t.Fatalf("timeout plugin receipts=%d", receipts)
	}
	var taskState, attemptState string
	if err := store.db.QueryRow(`SELECT t.state,a.state FROM tasks t
JOIN attempts a ON a.id=t.current_attempt_id WHERE t.id=?`, timedOut.TaskID).Scan(&taskState, &attemptState); err != nil {
		t.Fatal(err)
	}
	if taskState != "queued" || attemptState != "prepared" {
		t.Fatalf("timeout parent before cleanup task=%s attempt=%s", taskState, attemptState)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	restarted, err := startNextBackgroundRun(context.Background(), store, now.Add(time.Second))
	if err != nil || restarted.TimeoutRequestedAt == nil {
		t.Fatalf("restarted timeout run = %+v, error=%v", restarted, err)
	}
	cleanupRef := backgroundRunRef(restarted, now.Add(2*time.Second))
	if _, err := store.FinalizeBackgroundRunFailure(context.Background(), FinalizeBackgroundRunFailureParams{
		BackgroundRunRef: cleanupRef, Reason: "user_stopped", Evidence: "resources absent", CleanupProof: "exact timeout cleanup",
	}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("timeout finalized with a different reason = %v", err)
	}
	final, err := store.FinalizeBackgroundRunFailure(context.Background(), FinalizeBackgroundRunFailureParams{
		BackgroundRunRef: cleanupRef, Reason: "attempt_timeout", Evidence: "resources absent", CleanupProof: "exact timeout cleanup",
	})
	if err != nil || final.State != BackgroundRunFailed {
		t.Fatalf("timeout finalization = %+v, error=%v", final, err)
	}
	var taskReason, attemptReason string
	if err := store.db.QueryRow(`SELECT t.state,a.state,t.terminal_reason,a.terminal_reason FROM tasks t
JOIN attempts a ON a.id=t.current_attempt_id WHERE t.id=?`, final.TaskID).Scan(&taskState, &attemptState, &taskReason, &attemptReason); err != nil {
		t.Fatal(err)
	}
	if taskState != "failed" || attemptState != "failed" || taskReason != "attempt_timeout" || attemptReason != taskReason {
		t.Fatalf("timeout terminal parent task=%s attempt=%s reasons=%s/%s", taskState, attemptState, taskReason, attemptReason)
	}
}

func TestBackgroundRunCleanupFailuresPreservePhaseAndPermitRetry(t *testing.T) {
	for index, state := range []BackgroundRunState{BackgroundRunCanceling, BackgroundRunCleanupRequired} {
		t.Run(string(state), func(t *testing.T) {
			path := testDBPath(t)
			store := openTestStore(t, path)
			t.Cleanup(func() { _ = store.Close() })
			createTestWorkspace(t, store)
			n := 2200 + index
			params := testBackgroundRunAdmission(n, fmt.Sprintf("cleanup-failure-%s", state))
			if _, err := store.AdmitBackgroundRun(context.Background(), params); err != nil {
				t.Fatal(err)
			}
			now := testTime.Truncate(time.Millisecond).Add(time.Minute)
			run, ref := prepareBackgroundRunCleanup(t, store, params, state, now, n)
			failed, err := store.MarkBackgroundRunCleanupRequired(context.Background(), MarkBackgroundRunCleanupRequiredParams{
				BackgroundRunRef: ref, Error: "cleanup observation unavailable",
			})
			if err != nil || failed.State != BackgroundRunCleanupRequired || failed.EffectPhase != BackgroundRunEffectCleaning ||
				failed.LastError != "cleanup observation unavailable" || failed.Revision != run.Revision+1 {
				t.Fatalf("durable cleanup failure = %+v, error=%v", failed, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openTestStore(t, path)
			retry, err := startNextBackgroundRun(context.Background(), store, ref.Now.Add(time.Second))
			if err != nil || retry.State != BackgroundRunCleanupRequired || retry.EffectPhase != BackgroundRunEffectCleaning || retry.Revision != failed.Revision {
				t.Fatalf("cleanup retry run = %+v, error=%v", retry, err)
			}
		})
	}
}

func prepareBackgroundRunCleanup(t *testing.T, store *Store, params AdmitBackgroundRunParams, state BackgroundRunState, now time.Time, n int) (BackgroundRun, BackgroundRunRef) {
	t.Helper()
	run, ref := advanceBackgroundRunToPrompt(t, store, params.BackgroundRun.ImageIdentity, now)
	var err error
	switch state {
	case BackgroundRunCanceling:
		stop := StopBackgroundRunParams{
			WorkspaceID: testWorkspaceID(), TaskID: run.TaskID, ReceiptID: testReceiptID(5000 + n),
			Claim:              params.Claim,
			APIContractVersion: "run-v1", StoppedAt: ref.Now,
		}
		stop.Claim.Scope.CommandKind = StopBackgroundRunCommand
		stop.Claim.Key = task.IdempotencyKey(fmt.Sprintf("cleanup-stop-%d", n))
		stop.Claim.RequestHash = sha256.Sum256([]byte(stop.Claim.Key))
		if _, err := store.StopBackgroundRun(context.Background(), stop); err != nil {
			t.Fatal(err)
		}
	case BackgroundRunCleanupRequired:
		if _, err := store.MarkBackgroundRunCleanupRequired(context.Background(), MarkBackgroundRunCleanupRequiredParams{
			BackgroundRunRef: ref, Error: "prompt admitted but coordinator unavailable",
		}); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported cleanup state %s", state)
	}
	run, err = startNextBackgroundRun(context.Background(), store, ref.Now.Add(time.Second))
	if err != nil || run.State != state || run.EffectPhase != BackgroundRunEffectCleaning {
		t.Fatalf("cleanup run = %+v, error=%v", run, err)
	}
	return run, backgroundRunRef(run, ref.Now.Add(2*time.Second))
}

func backgroundRunRef(run BackgroundRun, now time.Time) BackgroundRunRef {
	return BackgroundRunRef{
		WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID, Generation: run.Generation,
		ExpectedRevision: run.Revision, ExpectedState: run.State, ExpectedPhase: run.EffectPhase, Now: now,
	}
}

// startNextBackgroundRun mirrors one coordinator scan: read the next run and,
// when it is queued, consume the provisioning slot.
func startNextBackgroundRun(ctx context.Context, store *Store, now time.Time) (BackgroundRun, error) {
	run, err := store.NextBackgroundRun(ctx, testWorkspaceID(), BackgroundRunSourceProfile)
	if err != nil || run.State != BackgroundRunQueued {
		return run, err
	}
	return store.StartBackgroundRunProvisioning(ctx, backgroundRunRef(run, now))
}

func startNextBackgroundRunWork(ctx context.Context, store *Store, now time.Time) (BackgroundRunWork, error) {
	if _, err := startNextBackgroundRun(ctx, store, now); err != nil {
		return BackgroundRunWork{}, err
	}
	return store.NextBackgroundRunWork(ctx, testWorkspaceID(), BackgroundRunSourceProfile)
}

func advanceBackgroundRunToPrompt(t *testing.T, store *Store, image string, now time.Time) (BackgroundRun, BackgroundRunRef) {
	t.Helper()
	_, ref := advanceBackgroundRunToPromptPending(t, store, now)
	run, err := store.RecordBackgroundRunPromptAdmitted(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunRef: ref, Evidence: "prompt admitted"})
	if err != nil {
		t.Fatal(err)
	}
	advanceBackgroundRef(&ref, run)
	ref.Now = ref.Now.Add(time.Second)
	return run, ref
}

// advanceBackgroundRunToRuntime starts provisioning and commits a runtime, the
// only durable record provisioning makes before the prompt fence.
func advanceBackgroundRunToRuntime(t *testing.T, store *Store, now time.Time) (BackgroundRun, BackgroundRunRef) {
	t.Helper()
	run, err := startNextBackgroundRun(context.Background(), store, now)
	if err != nil {
		t.Fatal(err)
	}
	ref := backgroundRunRef(run, now.Add(time.Second))
	run, err = store.RecordBackgroundRunRuntime(context.Background(), RecordBackgroundRunRuntimeParams{
		BackgroundRunRef: ref, ContainerID: "result-container", ContainerStartedAt: "2026-08-31T12:01:00Z",
		RuntimeEpoch: 1, HostPort: 49153, Evidence: "container started",
	})
	if err != nil {
		t.Fatal(err)
	}
	advanceBackgroundRef(&ref, run)
	ref.Now = ref.Now.Add(time.Second)
	return run, ref
}

func advanceBackgroundRunToPromptPending(t *testing.T, store *Store, now time.Time) (BackgroundRun, BackgroundRunRef) {
	t.Helper()
	_, ref := advanceBackgroundRunToRuntime(t, store, now)
	run, err := store.RecordBackgroundRunPromptRequestAttempted(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	advanceBackgroundRef(&ref, run)
	ref.Now = ref.Now.Add(time.Second)
	return run, ref
}

func advanceBackgroundRef(ref *BackgroundRunRef, run BackgroundRun) {
	ref.ExpectedRevision = run.Revision
	ref.ExpectedState = run.State
	ref.ExpectedPhase = run.EffectPhase
}

func testBackgroundRunAdmission(n int, key string) AdmitBackgroundRunParams {
	params := testAdmission(n, key, "Run in the background")
	params.BackgroundRun = &BackgroundRunIntent{
		RepositoryRemote: "https://github.com/owner/repository", Branch: "main", Profile: "source-39fb919a054190498f6d5b7985bde231f93ad7a6", EnvironmentSHA256: sha256.Sum256([]byte("{}")),
		ImageIdentity: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	return params
}
