package taskstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	rundomain "github.com/nebler/fern/internal/run"
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
	run, err := store.GetBackgroundRun(context.Background(), testWorkspaceID(), admission.Run.RunID, params.Claim.Actor)
	if err != nil || run.RunID != admission.Run.RunID || run.BaseOID != params.BaseSHA || run.ImageIdentity != params.ImageIdentity ||
		run.Profile != rundomain.SourceProfile || run.Agent != params.Agent || !run.Deadline.Equal(params.Deadline.Truncate(time.Millisecond)) ||
		run.State != rundomain.Queued || run.EffectPhase != "absent" {
		t.Fatalf("background run = %+v, error = %v", run, err)
	}
	resources := rundomain.NewResources(run.RunID)
	if !resources.Matches(run.CloneIdentity, run.VolumeIdentity, run.ContainerIdentity, run.EndpointIdentity) {
		t.Fatalf("admission did not derive run identities: %+v", run)
	}
	stopHash := sha256.Sum256([]byte("stop"))
	stopParams := StopBackgroundRunParams{WorkspaceID: testWorkspaceID(), RunID: run.RunID,
		Claim: params.Claim, APIContractVersion: "run-v1", StoppedAt: testTime.Truncate(time.Millisecond).Add(time.Minute)}
	stopParams.Claim.Scope.CommandKind = StopBackgroundRunCommand
	stopParams.Claim.Key = "run-stop"
	stopParams.Claim.RequestHash = stopHash
	stopped, err := store.StopBackgroundRun(context.Background(), stopParams)
	if err != nil || stopped.Run.State != rundomain.Failed || stopped.Run.StopReceiptID == 0 || stopped.Run.StopReceiptID != stopped.Receipt.ID {
		t.Fatalf("stop = %+v, error = %v", stopped, err)
	}
	replay, err := store.StopBackgroundRun(context.Background(), stopParams)
	if err != nil || !replay.Replayed || replay.Receipt.ID != stopped.Receipt.ID {
		t.Fatalf("stop replay = %+v, error = %v", replay, err)
	}
	wrongTarget := stopParams
	wrongTarget.RunID = testRunID(1699)
	if _, err := store.StopBackgroundRun(context.Background(), wrongTarget); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("stop replay target mismatch = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	persisted, err := store.GetBackgroundRun(context.Background(), testWorkspaceID(), run.RunID, params.Claim.Actor)
	if err != nil || persisted.State != rundomain.Failed || persisted.Revision != 2 || persisted.LastError != BackgroundRunStoppedBeforeStart {
		t.Fatalf("restarted run = %+v, error = %v", persisted, err)
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
	assertCounts(t, store, 1, 1)
	var runs int
	if err := store.db.QueryRow(`SELECT count(*) FROM runs`).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("run count = %d, error = %v", runs, err)
	}
	other := first.Claim.Actor
	other.ID, other.CredentialID = "pc_other", "pc_other"
	listed, err := store.ListBackgroundRuns(context.Background(), testWorkspaceID(), other, 100)
	if err != nil || len(listed) != 0 {
		t.Fatalf("cross-credential list = %+v, error = %v", listed, err)
	}
	if _, err := store.GetBackgroundRun(context.Background(), testWorkspaceID(), first.RunID, other); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-credential read = %v", err)
	}
	operator := task.ActorSnapshot{Type: task.ActorOperator, ID: "operator", DisplayName: "Operator",
		CredentialID: "operator", Authentication: "basic", RequestID: "request"}
	listed, err = store.ListBackgroundRuns(context.Background(), testWorkspaceID(), operator, 100)
	if err != nil || len(listed) != 1 || listed[0].RunID != first.RunID {
		t.Fatalf("operator list = %+v, error = %v", listed, err)
	}
	if run, err := store.GetBackgroundRun(context.Background(), testWorkspaceID(), first.RunID, operator); err != nil || run.RunID != first.RunID {
		t.Fatalf("operator read = %+v, error = %v", run, err)
	}
	if _, err := store.db.Exec(`UPDATE runs SET repository_remote='https://github.com/other/repo' WHERE id=?`, first.RunID); err == nil {
		t.Fatal("immutable run input changed")
	}
}

func TestBackgroundRunListLazyCapacityAndOwnership(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	ctx := context.Background()
	first := testBackgroundRunAdmission(1800, "list-first")
	assertList := func(actor task.ActorSnapshot, limit, count, capacity int, want task.RunID) {
		t.Helper()
		runs, err := store.ListBackgroundRuns(ctx, testWorkspaceID(), actor, limit)
		if err != nil || runs == nil || len(runs) != count || cap(runs) != capacity {
			t.Fatalf("list: len=%d cap=%d nil=%v err=%v", len(runs), cap(runs), runs == nil, err)
		}
		if count > 0 && runs[0].RunID != want {
			t.Fatalf("first task = %s, want %s", runs[0].RunID, want)
		}
	}
	assertList(first.Claim.Actor, MaxBackgroundRunListLimit, 0, 0, "")
	if _, err := store.AdmitBackgroundRun(ctx, first); err != nil {
		t.Fatal(err)
	}
	assertList(first.Claim.Actor, MaxBackgroundRunListLimit, 1, 1, first.RunID)
	assertList(first.Claim.Actor, 1, 1, 1, first.RunID)
	second := testBackgroundRunAdmission(1801, "list-second")
	second.Claim.Actor.ID, second.Claim.Actor.CredentialID = "pc_other", "pc_other"
	if _, err := store.AdmitBackgroundRun(ctx, second); err != nil {
		t.Fatal(err)
	}
	// The newer foreign row must not consume the SQL limit before ownership.
	assertList(first.Claim.Actor, 1, 1, 1, first.RunID)
	operator := task.ActorSnapshot{Type: task.ActorOperator, ID: "operator", DisplayName: "Operator",
		CredentialID: "operator", Authentication: "basic", RequestID: "request"}
	assertList(operator, 1, 1, 1, second.RunID)
	other := first.Claim.Actor
	other.CredentialID = "pc_absent"
	assertList(other, MaxBackgroundRunListLimit, 0, 0, "")
	for i := 2; i <= MaxBackgroundRunListLimit; i++ {
		p := testBackgroundRunAdmission(1800+i, fmt.Sprintf("list-owned-%d", i))
		if _, err := store.AdmitBackgroundRun(ctx, p); err != nil {
			t.Fatal(err)
		}
		if i == 10 {
			assertList(first.Claim.Actor, MaxBackgroundRunListLimit, 10, 16, p.RunID)
			assertList(first.Claim.Actor, 7, 7, 7, p.RunID)
		}
		if i == MaxBackgroundRunListLimit {
			assertList(first.Claim.Actor, MaxBackgroundRunListLimit, i, i, p.RunID)
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
	if _, err := store.GetBackgroundRun(context.Background(), otherWorkspace, admission.Run.RunID, params.Claim.Actor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace read = %v", err)
	}
	wrongWorkspaceStop := StopBackgroundRunParams{WorkspaceID: otherWorkspace, RunID: admission.Run.RunID,
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
		"skip provisioning":      `UPDATE runs SET state='setting_up',effect_phase='admitted',prompt_request_attempted_at=?,revision=revision+1,updated_at=? WHERE id=?`,
		"skip cleanup":           `UPDATE runs SET state='failed',effect_phase='cleanup_complete',cleanup_proof='x',last_error=?,revision=revision+1,updated_at=? WHERE id=?`,
		"prompt without runtime": `UPDATE runs SET state='uncertain',effect_phase='prompt_pending',prompt_request_attempted_at=?,revision=revision+1,updated_at=? WHERE id=?`,
		"seal without request":   `UPDATE runs SET state='canceling',effect_phase='sealing',last_error=?,revision=revision+1,updated_at=? WHERE id=?`,
	} {
		if _, err := store.db.Exec(statement, now, now, admission.Run.RunID); err == nil {
			t.Fatalf("closed transition %q succeeded", name)
		}
	}
}

func TestBackgroundRunAdmissionRejectsMismatchedIntentAtomically(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*AdmitBackgroundRunParams)
	}{
		{"profile", func(p *AdmitBackgroundRunParams) { p.Profile = "other" }},
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
			assertCounts(t, store, 0, 0)
		})
	}
	t.Run("repository binding SQL", func(t *testing.T) {
		store := openTestStore(t, testDBPath(t))
		t.Cleanup(func() { _ = store.Close() })
		createTestWorkspace(t, store)
		params := testBackgroundRunAdmission(1960, "repository-binding")
		params.RepositoryRemote = "https://github.com/other/repository"
		if _, err := store.AdmitBackgroundRun(context.Background(), params); err == nil {
			t.Fatal("workspace repository mismatch admitted")
		}
		assertCounts(t, store, 0, 0)
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
	if err != nil || run.RunID != first.RunID || run.State != rundomain.SettingUp || run.EffectPhase != rundomain.Provisioning {
		t.Fatalf("first start = %+v, error = %v", run, err)
	}
	continued, err := store.NextBackgroundRun(context.Background(), testWorkspaceID(), rundomain.SourceProfile)
	if err != nil || continued.RunID != run.RunID || continued.Revision != run.Revision {
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
	if err != nil || run.EffectPhase != rundomain.Provisioning || run.ObservedContainerID != "aabbcc" {
		t.Fatalf("runtime record = %+v, error = %v", run, err)
	}
	advanceBackgroundRef(&ref, run)
	ref.Now = ref.Now.Add(time.Second)
	if _, err := store.RecordBackgroundRunRuntime(context.Background(), RecordBackgroundRunRuntimeParams{BackgroundRunRef: ref,
		ContainerID: "ddeeff", ContainerStartedAt: "2026-08-31T12:02:00Z", RuntimeEpoch: 2, HostPort: 49153, Evidence: "replacement"}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("committed runtime was replaced: %v", err)
	}
	run, err = store.RecordBackgroundRunPromptRequestAttempted(context.Background(), ref)
	if err != nil || run.State != rundomain.SettingUp || run.EffectPhase != rundomain.PromptPending {
		t.Fatalf("prompt fence = %+v, error = %v", run, err)
	}
	restartedAt := now.Add(2 * time.Minute)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	restarted, err := startNextBackgroundRun(context.Background(), store, restartedAt)
	if err != nil || restarted.RunID != run.RunID || restarted.Revision != run.Revision {
		t.Fatalf("restarted next run=%+v error=%v", restarted, err)
	}
	run = restarted

	stop := StopBackgroundRunParams{WorkspaceID: testWorkspaceID(), RunID: run.RunID,
		Claim:              first.Claim,
		APIContractVersion: "run-v1", StoppedAt: restartedAt.Add(time.Second)}
	stop.Claim.Scope.CommandKind = StopBackgroundRunCommand
	stop.Claim.Key = "active-stop"
	stop.Claim.RequestHash = sha256.Sum256([]byte("active-stop"))
	stopped, err := store.StopBackgroundRun(context.Background(), stop)
	if err != nil || stopped.Run.State != rundomain.Canceling || stopped.Run.EffectPhase != rundomain.Cleaning ||
		stopped.Run.Revision != run.Revision+1 || stopped.Run.StopReceiptID == 0 {
		t.Fatalf("active stop = %+v, error = %v", stopped, err)
	}
	if replay, replayErr := store.StopBackgroundRun(context.Background(), stop); replayErr != nil || !replay.Replayed || replay.Receipt.ID != stopped.Receipt.ID || string(replay.Receipt.ResponseProjection) != string(stopped.Receipt.ResponseProjection) {
		t.Fatalf("active stop replay = %+v, error = %v", replay, replayErr)
	}
	if stopped.Run.EffectPhase != rundomain.Cleaning {
		t.Fatalf("active stop falsely terminalized run: %+v", stopped.Run)
	}

	stopRun, err := startNextBackgroundRun(context.Background(), store, stop.StoppedAt.Add(time.Second))
	if err != nil || stopRun.RunID != stopped.Run.RunID || stopRun.State != rundomain.Canceling || stopRun.StopReceiptID == 0 {
		t.Fatalf("stopped next run = %+v, error = %v", stopRun, err)
	}
	// A coordinator that read the run before the stop committed holds a stale
	// revision; the compare-and-swap rejects its write.
	stale := BackgroundRunRef{WorkspaceID: stopRun.WorkspaceID, RunID: stopRun.RunID,
		ExpectedRevision: run.Revision,
		ExpectedState:    stopRun.State, ExpectedPhase: stopRun.EffectPhase, Now: stop.StoppedAt.Add(2 * time.Second)}
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
	if err != nil || final.State != rundomain.Failed || final.EffectPhase != rundomain.CleanupComplete || final.LastError != "background_run_stopped" {
		t.Fatalf("active finalization = %+v, error = %v", final, err)
	}
	if replay, replayErr := store.StopBackgroundRun(context.Background(), stop); replayErr != nil || !replay.Replayed || replay.Receipt.ID != stopped.Receipt.ID {
		t.Fatalf("final stop replay = %+v, error = %v", replay, replayErr)
	}
	next, err := startNextBackgroundRun(context.Background(), store, cleanupRef.Now.Add(time.Second))
	if err != nil || next.RunID != second.RunID {
		t.Fatalf("capacity after final cleanup = %+v, error = %v", next, err)
	}
}

func TestNextBackgroundRunRequiresProfileButRecoversAcrossImageRotation(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	first := testBackgroundRunAdmission(2050, "profile-image-first")
	first.ImageIdentity = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
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
	if err != nil || started.RunID != first.RunID || started.ImageIdentity != first.ImageIdentity {
		t.Fatalf("rotated image recovery start = %+v, error = %v", started, err)
	}
	if next, err := startNextBackgroundRun(context.Background(), store, now); err != nil || next.RunID != first.RunID {
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
	work, err := store.NextBackgroundRunWork(context.Background(), testWorkspaceID(), rundomain.SourceProfile)
	if err != nil || work.Run.Revision != ref.ExpectedRevision || work.Prompt != params.Prompt ||
		!work.Run.Deadline.Equal(params.Deadline.Truncate(time.Millisecond)) {
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
	persisted, err := store.GetBackgroundRun(context.Background(), run.WorkspaceID, run.RunID, params.Claim.Actor)
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
	if err != nil || timedOut.State != rundomain.CleanupRequired || timedOut.EffectPhase != rundomain.Cleaning ||
		timedOut.TimeoutRequestedAt == nil || timedOut.StopReceiptID != 0 {
		t.Fatalf("system timeout = %+v, error=%v", timedOut, err)
	}
	var receipts int
	if err := store.db.QueryRow(`SELECT count(*) FROM receipts WHERE run_id=? AND command_kind='run.stop'`, timedOut.RunID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 0 {
		t.Fatalf("timeout plugin receipts=%d", receipts)
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
		BackgroundRunRef: cleanupRef, Reason: "run_timeout", Evidence: "resources absent", CleanupProof: "exact timeout cleanup",
	})
	if err != nil || final.State != rundomain.Failed || final.LastError != "run_timeout" {
		t.Fatalf("timeout finalization = %+v, error=%v", final, err)
	}
}

func TestBackgroundRunCleanupFailuresPreservePhaseAndPermitRetry(t *testing.T) {
	for index, state := range []rundomain.State{rundomain.Canceling, rundomain.CleanupRequired} {
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
			if err != nil || failed.State != rundomain.CleanupRequired || failed.EffectPhase != rundomain.Cleaning ||
				failed.LastError != "cleanup observation unavailable" || failed.Revision != run.Revision+1 {
				t.Fatalf("durable cleanup failure = %+v, error=%v", failed, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openTestStore(t, path)
			retry, err := startNextBackgroundRun(context.Background(), store, ref.Now.Add(time.Second))
			if err != nil || retry.State != rundomain.CleanupRequired || retry.EffectPhase != rundomain.Cleaning || retry.Revision != failed.Revision {
				t.Fatalf("cleanup retry run = %+v, error=%v", retry, err)
			}
		})
	}
}

func prepareBackgroundRunCleanup(t *testing.T, store *Store, params AdmitBackgroundRunParams, state rundomain.State, now time.Time, n int) (BackgroundRun, BackgroundRunRef) {
	t.Helper()
	run, ref := advanceBackgroundRunToPrompt(t, store, params.ImageIdentity, now)
	var err error
	switch state {
	case rundomain.Canceling:
		stop := StopBackgroundRunParams{
			WorkspaceID: testWorkspaceID(), RunID: run.RunID,
			Claim:              params.Claim,
			APIContractVersion: "run-v1", StoppedAt: ref.Now,
		}
		stop.Claim.Scope.CommandKind = StopBackgroundRunCommand
		stop.Claim.Key = task.IdempotencyKey(fmt.Sprintf("cleanup-stop-%d", n))
		stop.Claim.RequestHash = sha256.Sum256([]byte(stop.Claim.Key))
		if _, err := store.StopBackgroundRun(context.Background(), stop); err != nil {
			t.Fatal(err)
		}
	case rundomain.CleanupRequired:
		if _, err := store.MarkBackgroundRunCleanupRequired(context.Background(), MarkBackgroundRunCleanupRequiredParams{
			BackgroundRunRef: ref, Error: "prompt admitted but coordinator unavailable",
		}); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported cleanup state %s", state)
	}
	run, err = startNextBackgroundRun(context.Background(), store, ref.Now.Add(time.Second))
	if err != nil || run.State != state || run.EffectPhase != rundomain.Cleaning {
		t.Fatalf("cleanup run = %+v, error=%v", run, err)
	}
	return run, backgroundRunRef(run, ref.Now.Add(2*time.Second))
}

func backgroundRunRef(run BackgroundRun, now time.Time) BackgroundRunRef {
	return BackgroundRunRef{
		WorkspaceID: run.WorkspaceID, RunID: run.RunID,
		ExpectedRevision: run.Revision, ExpectedState: run.State, ExpectedPhase: run.EffectPhase, Now: now,
	}
}

// startNextBackgroundRun mirrors one coordinator scan: read the next run and,
// when it is queued, consume the provisioning slot.
func startNextBackgroundRun(ctx context.Context, store *Store, now time.Time) (BackgroundRun, error) {
	run, err := store.NextBackgroundRun(ctx, testWorkspaceID(), rundomain.SourceProfile)
	if err != nil || run.State != rundomain.Queued {
		return run, err
	}
	return store.StartBackgroundRunProvisioning(ctx, backgroundRunRef(run, now))
}

func startNextBackgroundRunWork(ctx context.Context, store *Store, now time.Time) (BackgroundRunWork, error) {
	if _, err := startNextBackgroundRun(ctx, store, now); err != nil {
		return BackgroundRunWork{}, err
	}
	return store.NextBackgroundRunWork(ctx, testWorkspaceID(), rundomain.SourceProfile)
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
	return testAdmission(n, key, "Run in the background")
}
