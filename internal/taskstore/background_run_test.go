package taskstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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
	stopHash := sha256.Sum256([]byte("stop"))
	stopParams := StopBackgroundRunParams{WorkspaceID: testWorkspaceID(), TaskID: run.TaskID, ReceiptID: testReceiptID(1600),
		AttemptEventID: testEventID(1601), TaskEventID: testEventID(1602),
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
	second.BackgroundRun.CloneIdentity = first.BackgroundRun.CloneIdentity
	if _, err := store.AdmitBackgroundRun(context.Background(), second); err == nil {
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
		AttemptEventID: testEventID(1911), TaskEventID: testEventID(1912), Claim: params.Claim,
		APIContractVersion: "run-v1", StoppedAt: testTime.Truncate(time.Millisecond).Add(time.Minute)}
	wrongWorkspaceStop.Claim.Scope.WorkspaceID = otherWorkspace
	wrongWorkspaceStop.Claim.Scope.CommandKind = StopBackgroundRunCommand
	wrongWorkspaceStop.Claim.Key = "wrong-workspace-stop"
	wrongWorkspaceStop.Claim.RequestHash = sha256.Sum256([]byte("wrong-workspace-stop"))
	if _, err := store.StopBackgroundRun(context.Background(), wrongWorkspaceStop); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace stop = %v", err)
	}
	now := testTime.Truncate(time.Millisecond).Add(time.Minute).UnixMilli()
	for name, statement := range map[string]string{
		"skip clone":             `UPDATE background_runs SET state='setting_up',effect_phase='ready',ready_at=?,ready_evidence='x',revision=revision+1,updated_at=? WHERE task_id=?`,
		"skip cleanup":           `UPDATE background_runs SET state='failed',effect_phase='cleanup_complete',cleanup_completed_at=?,cleanup_proof='x',revision=revision+1,updated_at=? WHERE task_id=?`,
		"prompt without session": `UPDATE background_runs SET state='uncertain',effect_phase='prompt_intent',prompt_intent_at=?,revision=revision+1,updated_at=? WHERE task_id=?`,
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
		{"instruction hash", func(p *AdmitBackgroundRunParams) { p.BackgroundRun.InstructionSHA256 = [32]byte{} }},
		{"profile hash", func(p *AdmitBackgroundRunParams) { p.BackgroundRun.ProfileSHA256 = [32]byte{} }},
		{"creator actor", func(p *AdmitBackgroundRunParams) { p.Claim.Actor.Type = task.ActorOperator }},
		{"environment identity", func(p *AdmitBackgroundRunParams) { p.BackgroundRun.ContainerIdentity += "-other" }},
		{"noncanonical remote", func(p *AdmitBackgroundRunParams) { p.BackgroundRun.RepositoryRemote += ".git" }},
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
			assertCounts(t, store, 0, 0, 0, 0)
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
		assertCounts(t, store, 0, 0, 0, 0)
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
	if err != nil || run.TaskID != first.TaskID || run.State != BackgroundRunSettingUp || run.EffectPhase != BackgroundRunEffectProvisionIntent ||
		run.ProvisionIntentAt == nil {
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
	if _, err := store.RecordBackgroundRunCloneObserved(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunRef: staleStart, Evidence: "stale clone"}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stale revision mutated run: %v", err)
	}
	ref := BackgroundRunRef{WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID,
		Generation: run.Generation, ExpectedRevision: run.Revision, ExpectedState: run.State, ExpectedPhase: run.EffectPhase, Now: now.Add(2 * time.Second)}
	advance := func(next BackgroundRun, at time.Time) {
		run = next
		ref.ExpectedRevision, ref.ExpectedState, ref.ExpectedPhase, ref.Now = run.Revision, run.State, run.EffectPhase, at
	}
	run, err = store.RecordBackgroundRunCloneObserved(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunRef: ref, Evidence: "clone exact"})
	if err != nil {
		t.Fatal(err)
	}
	advance(run, ref.Now.Add(time.Second))
	run, err = store.RecordBackgroundRunVolumeObserved(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunRef: ref, Evidence: "volume exact"})
	if err != nil {
		t.Fatal(err)
	}
	advance(run, ref.Now.Add(time.Second))
	run, err = store.RecordBackgroundRunContainerObserved(context.Background(), RecordBackgroundRunContainerObservedParams{
		BackgroundRunRef: ref, ContainerID: "aabbcc", ContainerStartedAt: "2026-08-31T12:01:00Z", RuntimeEpoch: 1,
		HostPort: 49152, Evidence: "exact container inspect",
	})
	if err != nil || run.EffectPhase != BackgroundRunEffectContainerObserved || run.ObservedContainerID != "aabbcc" {
		t.Fatalf("provision observation = %+v, error = %v", run, err)
	}
	advance(run, ref.Now.Add(time.Second))
	run, err = store.RecordBackgroundRunHealthObserved(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunRef: ref, Evidence: "health exact"})
	if err != nil {
		t.Fatal(err)
	}
	advance(run, ref.Now.Add(time.Second))
	run, err = store.RecordBackgroundRunReady(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunRef: ref, Evidence: "health ready"})
	if err != nil || run.EffectPhase != BackgroundRunEffectReady {
		t.Fatalf("ready = %+v, error = %v", run, err)
	}
	advance(run, ref.Now.Add(time.Second))
	run, err = store.RecordBackgroundRunSessionObserved(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunRef: ref, Evidence: "session exact"})
	if err != nil {
		t.Fatal(err)
	}
	advance(run, ref.Now.Add(time.Second))
	run, err = store.RecordBackgroundRunPromptIntent(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunRef: ref, Evidence: "prompt request begun"})
	if err != nil || run.State != BackgroundRunUncertain || run.EffectPhase != BackgroundRunEffectPromptIntent {
		t.Fatalf("prompt start = %+v, error = %v", run, err)
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
		AttemptEventID: testEventID(1981), TaskEventID: testEventID(1982), Claim: first.Claim,
		APIContractVersion: "run-v1", StoppedAt: restartedAt.Add(time.Second)}
	stop.Claim.Scope.CommandKind = StopBackgroundRunCommand
	stop.Claim.Key = "active-stop"
	stop.Claim.RequestHash = sha256.Sum256([]byte("active-stop"))
	stopped, err := store.StopBackgroundRun(context.Background(), stop)
	if err != nil || stopped.Run.State != BackgroundRunCanceling || stopped.Run.EffectPhase != BackgroundRunEffectStopIntent ||
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
	if _, err := store.RecordBackgroundRunWriterInactive(context.Background(), RecordBackgroundRunEvidenceParams{
		BackgroundRunRef: stale, Evidence: "stale writer observation",
	}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stale revision mutated run: %v", err)
	}
	cleanupRef := BackgroundRunRef{WorkspaceID: stopRun.WorkspaceID, TaskID: stopRun.TaskID, AttemptID: stopRun.AttemptID,
		Generation: stopRun.Generation, ExpectedRevision: stopRun.Revision, ExpectedState: stopRun.State, ExpectedPhase: stopRun.EffectPhase, Now: stop.StoppedAt.Add(2 * time.Second)}
	advanceCleanup := func(next BackgroundRun) {
		cleanupRef.ExpectedRevision, cleanupRef.ExpectedState, cleanupRef.ExpectedPhase = next.Revision, next.State, next.EffectPhase
		cleanupRef.Now = cleanupRef.Now.Add(time.Second)
	}
	for _, step := range []func(context.Context, RecordBackgroundRunEvidenceParams) (BackgroundRun, error){
		store.RecordBackgroundRunWriterInactive,
		store.RecordBackgroundRunRouteRemoved,
		store.RecordBackgroundRunContainerRemoved,
		store.RecordBackgroundRunVolumeRemoved,
		store.RecordBackgroundRunCloneRemoved,
	} {
		next, stepErr := step(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunRef: cleanupRef, Evidence: "exact absence proof"})
		if stepErr != nil {
			t.Fatalf("cleanup phase %s: %v", cleanupRef.ExpectedPhase, stepErr)
		}
		advanceCleanup(next)
	}
	final, err := store.FinalizeBackgroundRunFailure(context.Background(), FinalizeBackgroundRunFailureParams{
		BackgroundRunRef: cleanupRef, AttemptEventID: testEventID(1990), TaskEventID: testEventID(1991),
		Actor: testSystemActor(), Reason: "background_run_stopped", Evidence: "writer inactive and resources absent",
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
	run, ref := advanceBackgroundRunToPromptIntent(t, store, params.BackgroundRun.ImageIdentity, now)
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
	advanceBackgroundRef(&ref, run)
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
	actor := testSystemActor()
	actor.Type, actor.ID, actor.DisplayName = task.ActorSystem, "background-timeout", "Background timeout"
	timedOut, err := store.RequestBackgroundRunTimeout(context.Background(), RequestBackgroundRunTimeoutParams{
		BackgroundRunRef: backgroundRunRef(work.Run, now), AttemptEventID: testEventID(2081), TaskEventID: testEventID(2082), Actor: actor,
	})
	if err != nil || timedOut.State != BackgroundRunCleanupRequired || timedOut.EffectPhase != BackgroundRunEffectStopIntent ||
		timedOut.TimeoutRequestedAt == nil || timedOut.StopReceiptID != "" {
		t.Fatalf("system timeout = %+v, error=%v", timedOut, err)
	}
	var events, receipts int
	if err := store.db.QueryRow(`SELECT count(*) FROM events WHERE task_id=? AND type IN ('attempt.timeout_requested','task.timeout_requested') AND json_extract(payload,'$.reason')='attempt_timeout'`, timedOut.TaskID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM receipts WHERE target_id=? AND command_kind='run.stop'`, timedOut.TaskID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if events != 2 || receipts != 0 || timedOut.TimeoutActor == nil || *timedOut.TimeoutActor != actor {
		t.Fatalf("timeout evidence events=%d plugin receipts=%d", events, receipts)
	}
	var taskState, attemptState string
	var latestCursor, taskRevision int64
	if err := store.db.QueryRow(`SELECT t.state,a.state,t.latest_event_cursor,t.revision FROM tasks t
JOIN attempts a ON a.id=t.current_attempt_id WHERE t.id=?`, timedOut.TaskID).Scan(&taskState, &attemptState, &latestCursor, &taskRevision); err != nil {
		t.Fatal(err)
	}
	var timeoutTaskCursor int64
	if err := store.db.QueryRow(`SELECT cursor FROM events WHERE task_id=? AND attempt_id IS NULL AND type='task.timeout_requested'`, timedOut.TaskID).Scan(&timeoutTaskCursor); err != nil {
		t.Fatal(err)
	}
	if taskState != "queued" || attemptState != "prepared" || latestCursor != timeoutTaskCursor || taskRevision != 2 {
		t.Fatalf("timeout parent before cleanup task=%s attempt=%s cursor=%d/%d revision=%d", taskState, attemptState, latestCursor, timeoutTaskCursor, taskRevision)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	restarted, err := startNextBackgroundRun(context.Background(), store, now.Add(time.Second))
	if err != nil || restarted.TimeoutActor == nil || *restarted.TimeoutActor != actor {
		t.Fatalf("restarted timeout run = %+v, error=%v", restarted, err)
	}
	cleanupRef := backgroundRunRef(restarted, now.Add(2*time.Second))
	for _, step := range []func(context.Context, RecordBackgroundRunEvidenceParams) (BackgroundRun, error){
		store.RecordBackgroundRunWriterInactive, store.RecordBackgroundRunRouteRemoved, store.RecordBackgroundRunContainerRemoved,
		store.RecordBackgroundRunVolumeRemoved, store.RecordBackgroundRunCloneRemoved,
	} {
		restarted, err = step(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunRef: cleanupRef, Evidence: "exact timeout cleanup"})
		if err != nil {
			t.Fatalf("timeout cleanup from %s: %v", cleanupRef.ExpectedPhase, err)
		}
		advanceBackgroundRef(&cleanupRef, restarted)
		cleanupRef.Now = cleanupRef.Now.Add(time.Second)
	}
	wrongActor := actor
	wrongActor.ID, wrongActor.RequestID = "different-timeout", "different-timeout"
	if _, err := store.FinalizeBackgroundRunFailure(context.Background(), FinalizeBackgroundRunFailureParams{
		BackgroundRunRef: cleanupRef, AttemptEventID: testEventID(2083), TaskEventID: testEventID(2084), Actor: wrongActor,
		Reason: "attempt_timeout", Evidence: "resources absent", CleanupProof: "exact timeout cleanup",
	}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("different timeout actor finalization = %v", err)
	}
	final, err := store.FinalizeBackgroundRunFailure(context.Background(), FinalizeBackgroundRunFailureParams{
		BackgroundRunRef: cleanupRef, AttemptEventID: testEventID(2085), TaskEventID: testEventID(2086), Actor: actor,
		Reason: "attempt_timeout", Evidence: "resources absent", CleanupProof: "exact timeout cleanup",
	})
	if err != nil || final.State != BackgroundRunFailed || final.TimeoutActor == nil || *final.TimeoutActor != actor {
		t.Fatalf("timeout finalization = %+v, error=%v", final, err)
	}
	var taskReason, attemptReason string
	if err := store.db.QueryRow(`SELECT t.state,a.state,t.terminal_reason,a.terminal_reason FROM tasks t
JOIN attempts a ON a.id=t.current_attempt_id WHERE t.id=?`, final.TaskID).Scan(&taskState, &attemptState, &taskReason, &attemptReason); err != nil {
		t.Fatal(err)
	}
	var attributed int
	if err := store.db.QueryRow(`SELECT count(*) FROM events terminal
JOIN actor_snapshots actor ON actor.id=terminal.actor_snapshot_id
WHERE terminal.task_id=? AND terminal.type IN ('attempt.failed','task.failed') AND actor.actor_id=?`, final.TaskID, actor.ID).Scan(&attributed); err != nil {
		t.Fatal(err)
	}
	if taskState != "failed" || attemptState != "failed" || taskReason != "attempt_timeout" || attemptReason != taskReason || attributed != 2 {
		t.Fatalf("timeout terminal parent task=%s attempt=%s reasons=%s/%s actor events=%d", taskState, attemptState, taskReason, attemptReason, attributed)
	}
}

func TestBackgroundRunCleanupFailuresPreservePhaseAndPermitRetry(t *testing.T) {
	phases := []BackgroundRunEffectPhase{
		BackgroundRunEffectStopIntent,
		BackgroundRunEffectWriterInactive,
		BackgroundRunEffectRouteRemoved,
		BackgroundRunEffectContainerRemoved,
		BackgroundRunEffectVolumeRemoved,
		BackgroundRunEffectCloneRemoved,
	}
	states := []BackgroundRunState{BackgroundRunCanceling, BackgroundRunCleanupRequired}
	for stateIndex, state := range states {
		for phaseIndex, phase := range phases {
			t.Run(string(state)+"/"+string(phase), func(t *testing.T) {
				path := testDBPath(t)
				store := openTestStore(t, path)
				t.Cleanup(func() { _ = store.Close() })
				createTestWorkspace(t, store)
				n := 2200 + stateIndex*100 + phaseIndex
				params := testBackgroundRunAdmission(n, fmt.Sprintf("cleanup-failure-%s-%s", state, phase))
				if _, err := store.AdmitBackgroundRun(context.Background(), params); err != nil {
					t.Fatal(err)
				}
				now := testTime.Truncate(time.Millisecond).Add(time.Minute)
				run, ref := prepareBackgroundRunCleanup(t, store, params, state, phase, now, n)
				failed, err := store.MarkBackgroundRunCleanupRequired(context.Background(), MarkBackgroundRunCleanupRequiredParams{
					BackgroundRunRef: ref, Error: "cleanup observation unavailable",
				})
				wantState := state
				if state == BackgroundRunCanceling {
					wantState = BackgroundRunCleanupRequired
				}
				if err != nil || failed.State != wantState || failed.EffectPhase != phase || failed.LastError != "cleanup observation unavailable" ||
					failed.Revision != run.Revision+1 {
					t.Fatalf("durable cleanup failure = %+v, error=%v", failed, err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				store = openTestStore(t, path)
				retry, err := startNextBackgroundRun(context.Background(), store, ref.Now.Add(time.Second))
				if err != nil || retry.State != wantState || retry.EffectPhase != phase || retry.Revision != failed.Revision {
					t.Fatalf("cleanup retry run = %+v, error=%v", retry, err)
				}
			})
		}
	}
}

func TestBackgroundRunPreEffectFailureRequiresAbsenceProofAndFinalizesParents(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	first := testBackgroundRunAdmission(2150, "pre-effect-failure")
	second := testBackgroundRunAdmission(2151, "after-pre-effect-failure")
	if _, err := store.AdmitBackgroundRun(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdmitBackgroundRun(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	now := testTime.Truncate(time.Millisecond).Add(time.Minute)
	run, err := startNextBackgroundRun(context.Background(), store, now)
	if err != nil {
		t.Fatal(err)
	}
	ref := BackgroundRunRef{WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID,
		Generation: run.Generation, ExpectedRevision: run.Revision, ExpectedState: run.State, ExpectedPhase: run.EffectPhase, Now: now.Add(time.Second)}
	if _, err := store.FinalizeBackgroundRunFailure(context.Background(), FinalizeBackgroundRunFailureParams{
		BackgroundRunRef: ref, AttemptEventID: testEventID(2152), TaskEventID: testEventID(2153),
		Actor: testSystemActor(), Reason: "background_image_unavailable", Evidence: "image inspect returned deterministic absence",
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("pre-effect failure without absence proof = %v", err)
	}
	final, err := store.FinalizeBackgroundRunFailure(context.Background(), FinalizeBackgroundRunFailureParams{
		BackgroundRunRef: ref, AttemptEventID: testEventID(2152), TaskEventID: testEventID(2153),
		Actor: testSystemActor(), Reason: "background_image_unavailable", Evidence: "image inspect returned deterministic absence",
		CleanupProof: "clone, volume, container, and route were never created",
	})
	if err != nil || final.State != BackgroundRunFailed || final.EffectPhase != BackgroundRunEffectPreEffectFailed ||
		final.AbsenceProof != "clone, volume, container, and route were never created" || final.CleanupCompletedAt != nil {
		t.Fatalf("pre-effect finalization = %+v, error = %v", final, err)
	}
	var taskState, attemptState, taskReason, attemptReason string
	if err := store.db.QueryRow(`SELECT t.state,a.state,t.terminal_reason,a.terminal_reason
FROM tasks t JOIN attempts a ON a.id=t.current_attempt_id WHERE t.id=?`, final.TaskID).
		Scan(&taskState, &attemptState, &taskReason, &attemptReason); err != nil || taskState != "failed" || attemptState != "failed" ||
		taskReason != "background_image_unavailable" || attemptReason != taskReason {
		t.Fatalf("pre-effect parents = %q/%q reasons=%q/%q error=%v", taskState, attemptState, taskReason, attemptReason, err)
	}
	next, err := startNextBackgroundRun(context.Background(), store, ref.Now.Add(time.Second))
	if err != nil || next.TaskID != second.TaskID {
		t.Fatalf("capacity after pre-effect failure = %+v, error = %v", next, err)
	}
}

func prepareBackgroundRunCleanup(t *testing.T, store *Store, params AdmitBackgroundRunParams, state BackgroundRunState, phase BackgroundRunEffectPhase, now time.Time, n int) (BackgroundRun, BackgroundRunRef) {
	t.Helper()
	run, ref := advanceBackgroundRunToPrompt(t, store, params.BackgroundRun.ImageIdentity, now)
	var err error
	switch state {
	case BackgroundRunCanceling:
		stop := StopBackgroundRunParams{
			WorkspaceID: testWorkspaceID(), TaskID: run.TaskID, ReceiptID: testReceiptID(5000 + n),
			AttemptEventID: testEventID(5001 + n), TaskEventID: testEventID(5002 + n), Claim: params.Claim,
			APIContractVersion: "run-v1", StoppedAt: ref.Now,
		}
		stop.Claim.Scope.CommandKind = StopBackgroundRunCommand
		stop.Claim.Key = task.IdempotencyKey(fmt.Sprintf("cleanup-stop-%d", n))
		stop.Claim.RequestHash = sha256.Sum256([]byte(stop.Claim.Key))
		stopped, stopErr := store.StopBackgroundRun(context.Background(), stop)
		if stopErr != nil {
			t.Fatal(stopErr)
		}
		run, err = startNextBackgroundRun(context.Background(), store, stop.StoppedAt.Add(time.Second))
		if err != nil || run.TaskID != stopped.Run.TaskID || run.State != BackgroundRunCanceling {
			t.Fatalf("stopped run = %+v, error=%v", run, err)
		}
		ref = backgroundRunRef(run, stop.StoppedAt.Add(2*time.Second))
	case BackgroundRunCleanupRequired:
		run, err = store.MarkBackgroundRunCleanupRequired(context.Background(), MarkBackgroundRunCleanupRequiredParams{
			BackgroundRunRef: ref, Error: "prompt admitted but coordinator unavailable",
		})
		if err != nil {
			t.Fatal(err)
		}
		run, err = startNextBackgroundRun(context.Background(), store, ref.Now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		ref = backgroundRunRef(run, ref.Now.Add(2*time.Second))
	default:
		t.Fatalf("unsupported cleanup state %s", state)
	}

	steps := []func(context.Context, RecordBackgroundRunEvidenceParams) (BackgroundRun, error){
		store.RecordBackgroundRunWriterInactive,
		store.RecordBackgroundRunRouteRemoved,
		store.RecordBackgroundRunContainerRemoved,
		store.RecordBackgroundRunVolumeRemoved,
		store.RecordBackgroundRunCloneRemoved,
	}
	phases := []BackgroundRunEffectPhase{
		BackgroundRunEffectStopIntent,
		BackgroundRunEffectWriterInactive,
		BackgroundRunEffectRouteRemoved,
		BackgroundRunEffectContainerRemoved,
		BackgroundRunEffectVolumeRemoved,
		BackgroundRunEffectCloneRemoved,
	}
	target := -1
	for index, candidate := range phases {
		if candidate == phase {
			target = index
			break
		}
	}
	if target < 0 {
		t.Fatalf("unsupported cleanup phase %s", phase)
	}
	for index := 0; index < target; index++ {
		run, err = steps[index](context.Background(), RecordBackgroundRunEvidenceParams{
			BackgroundRunRef: ref, Evidence: "exact cleanup observation",
		})
		if err != nil {
			t.Fatalf("advance cleanup from %s: %v", ref.ExpectedPhase, err)
		}
		advanceBackgroundRef(&ref, run)
		ref.Now = ref.Now.Add(time.Second)
	}
	return run, ref
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
	_, ref := advanceBackgroundRunToPromptIntent(t, store, image, now)
	run, err := store.RecordBackgroundRunPromptRequestAttempted(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	advanceBackgroundRef(&ref, run)
	ref.Now = ref.Now.Add(time.Second)
	run, err = store.RecordBackgroundRunPromptAdmitted(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunRef: ref, Evidence: "prompt admitted"})
	if err != nil {
		t.Fatal(err)
	}
	advanceBackgroundRef(&ref, run)
	ref.Now = ref.Now.Add(time.Second)
	return run, ref
}

func advanceBackgroundRunToPromptIntent(t *testing.T, store *Store, image string, now time.Time) (BackgroundRun, BackgroundRunRef) {
	t.Helper()
	run, err := startNextBackgroundRun(context.Background(), store, now)
	if err != nil {
		t.Fatal(err)
	}
	ref := BackgroundRunRef{WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID,
		Generation: run.Generation, ExpectedRevision: run.Revision, ExpectedState: run.State, ExpectedPhase: run.EffectPhase, Now: now.Add(time.Second)}
	evidenceStep := func(step func(context.Context, RecordBackgroundRunEvidenceParams) (BackgroundRun, error), evidence string) {
		run, err = step(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunRef: ref, Evidence: evidence})
		if err != nil {
			t.Fatalf("advance from %s: %v", ref.ExpectedPhase, err)
		}
		advanceBackgroundRef(&ref, run)
		ref.Now = ref.Now.Add(time.Second)
	}
	evidenceStep(store.RecordBackgroundRunCloneObserved, "clone observed")
	evidenceStep(store.RecordBackgroundRunVolumeObserved, "volume observed")
	run, err = store.RecordBackgroundRunContainerObserved(context.Background(), RecordBackgroundRunContainerObservedParams{
		BackgroundRunRef: ref, ContainerID: "result-container", ContainerStartedAt: "2026-08-31T12:01:00Z",
		RuntimeEpoch: 1, HostPort: 49153, Evidence: "container observed",
	})
	if err != nil {
		t.Fatal(err)
	}
	advanceBackgroundRef(&ref, run)
	ref.Now = ref.Now.Add(time.Second)
	evidenceStep(store.RecordBackgroundRunHealthObserved, "health observed")
	evidenceStep(store.RecordBackgroundRunReady, "ready observed")
	evidenceStep(store.RecordBackgroundRunSessionObserved, "session observed")
	evidenceStep(store.RecordBackgroundRunPromptIntent, "prompt intent")
	return run, ref
}

func advanceBackgroundRef(ref *BackgroundRunRef, run BackgroundRun) {
	ref.ExpectedRevision = run.Revision
	ref.ExpectedState = run.State
	ref.ExpectedPhase = run.EffectPhase
}

func testBackgroundRunAdmission(n int, key string) AdmitBackgroundRunParams {
	params := testAdmission(n, key, "Run in the background")
	compact := strings.ReplaceAll(strings.TrimPrefix(string(params.TaskID), "tsk_"), "-", "")
	params.BackgroundRun = &BackgroundRunIntent{
		RepositoryRemote: "https://github.com/owner/repository", Branch: "main",
		InstructionSHA256: sha256.Sum256([]byte(params.Prompt)), Profile: "source-39fb919a054190498f6d5b7985bde231f93ad7a6",
		ProfileSHA256: sha256.Sum256([]byte("source-39fb919a054190498f6d5b7985bde231f93ad7a6")), EnvironmentSHA256: sha256.Sum256([]byte("{}")),
		ImageIdentity: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		CloneIdentity: "run-" + compact + "-g1-clone", VolumeIdentity: "fern-run-" + compact + "-g1-opencode",
		ContainerIdentity: "fern-run-" + compact + "-g1", EndpointIdentity: "run-" + compact + "-g1-endpoint",
	}
	return params
}
