package taskstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	if _, err := store.db.Exec(`UPDATE background_runs SET repository_remote='https://github.com/other/repo' WHERE task_id=?`, first.TaskID); err == nil {
		t.Fatal("immutable run input changed")
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

func TestBackgroundRunClaimsCapacityRecoveryAndActiveStop(t *testing.T) {
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
	run, err := store.ClaimNextBackgroundRun(context.Background(), ClaimNextBackgroundRunParams{
		WorkspaceID: testWorkspaceID(), ClaimOwner: "worker-a", Now: now, LeaseDuration: time.Minute,
		Profile: BackgroundRunSourceProfile, ImageIdentity: first.BackgroundRun.ImageIdentity,
	})
	if err != nil || run.TaskID != first.TaskID || run.State != BackgroundRunSettingUp || run.EffectPhase != BackgroundRunEffectProvisionIntent ||
		run.ClaimGeneration != 1 || run.ClaimOwner != "worker-a" || run.ProvisionIntentAt == nil {
		t.Fatalf("first claim = %+v, error = %v", run, err)
	}
	if _, err := store.ClaimNextBackgroundRun(context.Background(), ClaimNextBackgroundRunParams{
		WorkspaceID: testWorkspaceID(), ClaimOwner: "worker-b", Now: now, LeaseDuration: time.Minute,
		Profile: BackgroundRunSourceProfile, ImageIdentity: first.BackgroundRun.ImageIdentity,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("capacity-one competing claim = %v", err)
	}
	continued, err := store.ClaimNextBackgroundRun(context.Background(), ClaimNextBackgroundRunParams{
		WorkspaceID: testWorkspaceID(), ClaimOwner: "worker-a", Now: now.Add(time.Second), LeaseDuration: time.Minute,
		Profile: BackgroundRunSourceProfile, ImageIdentity: first.BackgroundRun.ImageIdentity,
	})
	if err != nil || continued.TaskID != run.TaskID || continued.ClaimGeneration != run.ClaimGeneration || continued.Revision != run.Revision+1 {
		t.Fatalf("same-owner claim continuation = %+v, error=%v", continued, err)
	}
	run = continued
	claim := BackgroundRunClaim{WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID,
		Generation: run.Generation, ClaimOwner: run.ClaimOwner, ClaimGeneration: run.ClaimGeneration,
		ExpectedRevision: run.Revision, ExpectedState: run.State, ExpectedPhase: run.EffectPhase, CancelEpoch: run.CancelEpoch, Now: now.Add(2 * time.Second)}
	advance := func(next BackgroundRun, at time.Time) {
		run = next
		claim.ExpectedRevision, claim.ExpectedState, claim.ExpectedPhase, claim.CancelEpoch, claim.Now = run.Revision, run.State, run.EffectPhase, run.CancelEpoch, at
	}
	run, err = store.RecordBackgroundRunCloneObserved(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunClaim: claim, Evidence: "clone exact"})
	if err != nil {
		t.Fatal(err)
	}
	advance(run, claim.Now.Add(time.Second))
	run, err = store.RecordBackgroundRunVolumeObserved(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunClaim: claim, Evidence: "volume exact"})
	if err != nil {
		t.Fatal(err)
	}
	advance(run, claim.Now.Add(time.Second))
	run, err = store.RecordBackgroundRunContainerObserved(context.Background(), RecordBackgroundRunContainerObservedParams{
		BackgroundRunClaim: claim, ContainerID: "aabbcc", ContainerStartedAt: "2026-08-31T12:01:00Z", RuntimeEpoch: 1,
		HostPort: 49152, Evidence: "exact container inspect",
	})
	if err != nil || run.EffectPhase != BackgroundRunEffectContainerObserved || run.ObservedContainerID != "aabbcc" {
		t.Fatalf("provision observation = %+v, error = %v", run, err)
	}
	advance(run, claim.Now.Add(time.Second))
	run, err = store.RecordBackgroundRunHealthObserved(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunClaim: claim, Evidence: "health exact"})
	if err != nil {
		t.Fatal(err)
	}
	advance(run, claim.Now.Add(time.Second))
	run, err = store.RecordBackgroundRunReady(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunClaim: claim, Evidence: "health ready"})
	if err != nil || run.EffectPhase != BackgroundRunEffectReady {
		t.Fatalf("ready = %+v, error = %v", run, err)
	}
	advance(run, claim.Now.Add(time.Second))
	run, err = store.RecordBackgroundRunSessionObserved(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunClaim: claim, Evidence: "session exact"})
	if err != nil {
		t.Fatal(err)
	}
	advance(run, claim.Now.Add(time.Second))
	run, err = store.RecordBackgroundRunPromptIntent(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunClaim: claim, Evidence: "prompt request begun"})
	if err != nil || run.State != BackgroundRunUncertain || run.EffectPhase != BackgroundRunEffectPromptIntent {
		t.Fatalf("prompt start = %+v, error = %v", run, err)
	}
	expired := claim
	expired.ExpectedRevision, expired.ExpectedState, expired.ExpectedPhase = run.Revision, run.State, run.EffectPhase
	expired.Now = now.Add(2 * time.Minute)
	if _, err := store.ReadClaimedBackgroundRun(context.Background(), expired); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expired lease retained authority: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	takeover, err := store.ClaimActiveBackgroundRun(context.Background(), ClaimBackgroundRunParams{
		WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID, Generation: run.Generation,
		ExpectedRevision: run.Revision, ExpectedState: run.State, ExpectedPhase: run.EffectPhase, CancelEpoch: run.CancelEpoch,
		ClaimOwner: "worker-b", Now: expired.Now, LeaseDuration: time.Minute, Profile: BackgroundRunSourceProfile, ImageIdentity: run.ImageIdentity,
	})
	if err != nil || takeover.ClaimGeneration != 2 {
		t.Fatalf("expired takeover=%+v error=%v", takeover, err)
	}
	run = takeover

	stop := StopBackgroundRunParams{WorkspaceID: testWorkspaceID(), TaskID: run.TaskID, ReceiptID: testReceiptID(1980),
		AttemptEventID: testEventID(1981), TaskEventID: testEventID(1982), Claim: first.Claim,
		APIContractVersion: "run-v1", StoppedAt: expired.Now.Add(time.Second)}
	stop.Claim.Scope.CommandKind = StopBackgroundRunCommand
	stop.Claim.Key = "active-stop"
	stop.Claim.RequestHash = sha256.Sum256([]byte("active-stop"))
	stopped, err := store.StopBackgroundRun(context.Background(), stop)
	if err != nil || stopped.Run.State != BackgroundRunCanceling || stopped.Run.EffectPhase != BackgroundRunEffectStopIntent ||
		stopped.Run.ClaimOwner != "" || stopped.Run.CancelEpoch != 1 {
		t.Fatalf("active stop = %+v, error = %v", stopped, err)
	}
	if replay, replayErr := store.StopBackgroundRun(context.Background(), stop); replayErr != nil || !replay.Replayed || replay.Receipt.ID != stopped.Receipt.ID || string(replay.Receipt.ResponseProjection) != string(stopped.Receipt.ResponseProjection) {
		t.Fatalf("active stop replay = %+v, error = %v", replay, replayErr)
	}
	var taskState, attemptState string
	if err := store.db.QueryRow(`SELECT t.state,a.state FROM tasks t JOIN attempts a ON a.id=t.current_attempt_id WHERE t.id=?`, run.TaskID).Scan(&taskState, &attemptState); err != nil || taskState != "queued" || attemptState != "prepared" {
		t.Fatalf("active stop falsely terminalized task=%q attempt=%q error=%v", taskState, attemptState, err)
	}

	stopClaim, err := store.ClaimBackgroundRunStop(context.Background(), ClaimBackgroundRunParams{
		WorkspaceID: stopped.Run.WorkspaceID, TaskID: stopped.Run.TaskID, AttemptID: stopped.Run.AttemptID,
		Generation: stopped.Run.Generation, ExpectedRevision: stopped.Run.Revision, ExpectedState: stopped.Run.State, ExpectedPhase: stopped.Run.EffectPhase, CancelEpoch: 1,
		ClaimOwner: "stopper", Now: stop.StoppedAt.Add(time.Second), LeaseDuration: time.Minute,
		Profile: BackgroundRunSourceProfile, ImageIdentity: stopped.Run.ImageIdentity,
	})
	if err != nil || stopClaim.ClaimGeneration != 3 {
		t.Fatalf("stop claim = %+v, error = %v", stopClaim, err)
	}
	stale := BackgroundRunClaim{WorkspaceID: stopClaim.WorkspaceID, TaskID: stopClaim.TaskID, AttemptID: stopClaim.AttemptID,
		Generation: stopClaim.Generation, ClaimOwner: "worker-a", ClaimGeneration: 1, ExpectedRevision: stopClaim.Revision,
		ExpectedState: stopClaim.State, ExpectedPhase: stopClaim.EffectPhase, CancelEpoch: 0, Now: stop.StoppedAt.Add(2 * time.Second)}
	if _, err := store.ReadClaimedBackgroundRun(context.Background(), stale); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stale generation retained authority: %v", err)
	}
	if _, err := store.RecordBackgroundRunWriterInactive(context.Background(), RecordBackgroundRunEvidenceParams{
		BackgroundRunClaim: stale, Evidence: "stale writer observation",
	}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stale generation mutated run: %v", err)
	}
	cleanupClaim := BackgroundRunClaim{WorkspaceID: stopClaim.WorkspaceID, TaskID: stopClaim.TaskID, AttemptID: stopClaim.AttemptID,
		Generation: stopClaim.Generation, ClaimOwner: stopClaim.ClaimOwner, ClaimGeneration: stopClaim.ClaimGeneration,
		ExpectedRevision: stopClaim.Revision, ExpectedState: stopClaim.State, ExpectedPhase: stopClaim.EffectPhase,
		CancelEpoch: stopClaim.CancelEpoch, Now: stop.StoppedAt.Add(2 * time.Second)}
	advanceCleanup := func(next BackgroundRun) {
		cleanupClaim.ExpectedRevision, cleanupClaim.ExpectedState, cleanupClaim.ExpectedPhase = next.Revision, next.State, next.EffectPhase
		cleanupClaim.Now = cleanupClaim.Now.Add(time.Second)
	}
	for _, step := range []func(context.Context, RecordBackgroundRunEvidenceParams) (BackgroundRun, error){
		store.RecordBackgroundRunWriterInactive,
		store.RecordBackgroundRunRouteRemoved,
		store.RecordBackgroundRunContainerRemoved,
		store.RecordBackgroundRunVolumeRemoved,
		store.RecordBackgroundRunCloneRemoved,
	} {
		next, stepErr := step(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunClaim: cleanupClaim, Evidence: "exact absence proof"})
		if stepErr != nil {
			t.Fatalf("cleanup phase %s: %v", cleanupClaim.ExpectedPhase, stepErr)
		}
		advanceCleanup(next)
	}
	final, err := store.FinalizeBackgroundRunFailure(context.Background(), FinalizeBackgroundRunFailureParams{
		BackgroundRunClaim: cleanupClaim, AttemptEventID: testEventID(1990), TaskEventID: testEventID(1991),
		Actor: testDeliveryActor(), Reason: "background_run_stopped", Evidence: "writer inactive and resources absent",
		CleanupProof: "route, container, volume, and clone absent",
	})
	if err != nil || final.State != BackgroundRunFailed || final.EffectPhase != BackgroundRunEffectCleanupComplete || final.ClaimOwner != "" {
		t.Fatalf("active finalization = %+v, error = %v", final, err)
	}
	ownership, ownershipErr := store.GetBackgroundRunOwnership(context.Background(), final.WorkspaceID, final.TaskID)
	if ownershipErr != nil || ownership.Mode != BackgroundRunOwnershipClosed || ownership.Phase != BackgroundRunOwnershipClosedPhase {
		t.Fatalf("terminal ownership = %+v, error = %v", ownership, ownershipErr)
	}
	if err := store.db.QueryRow(`SELECT t.state,a.state,t.terminal_reason,a.terminal_reason FROM tasks t JOIN attempts a ON a.id=t.current_attempt_id WHERE t.id=?`, final.TaskID).
		Scan(&taskState, &attemptState, new(string), new(string)); err != nil || taskState != "failed" || attemptState != "failed" {
		t.Fatalf("final parent task=%q attempt=%q error=%v", taskState, attemptState, err)
	}
	if replay, replayErr := store.StopBackgroundRun(context.Background(), stop); replayErr != nil || !replay.Replayed || replay.Receipt.ID != stopped.Receipt.ID {
		t.Fatalf("final stop replay = %+v, error = %v", replay, replayErr)
	}
	next, err := store.ClaimNextBackgroundRun(context.Background(), ClaimNextBackgroundRunParams{
		WorkspaceID: testWorkspaceID(), ClaimOwner: "worker-next", Now: cleanupClaim.Now.Add(time.Second), LeaseDuration: time.Minute,
		Profile: BackgroundRunSourceProfile, ImageIdentity: second.BackgroundRun.ImageIdentity,
	})
	if err != nil || next.TaskID != second.TaskID {
		t.Fatalf("capacity after final cleanup = %+v, error = %v", next, err)
	}
}

func TestConcurrentBackgroundRunClaimHasOneWinner(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	for index := range 2 {
		params := testBackgroundRunAdmission(2000+index, fmt.Sprintf("claim-race-%d", index))
		if _, err := store.AdmitBackgroundRun(context.Background(), params); err != nil {
			t.Fatal(err)
		}
	}

	const workers = 16
	start := make(chan struct{})
	results := make(chan BackgroundRun, workers)
	errs := make(chan error, workers)
	var wait sync.WaitGroup
	for index := range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			run, err := store.ClaimNextBackgroundRun(context.Background(), ClaimNextBackgroundRunParams{
				WorkspaceID: testWorkspaceID(), ClaimOwner: fmt.Sprintf("worker-%d", index),
				Now: testTime.Truncate(time.Millisecond).Add(time.Minute), LeaseDuration: time.Minute,
				Profile: BackgroundRunSourceProfile, ImageIdentity: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			})
			if err != nil {
				errs <- err
				return
			}
			results <- run
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errs)
	winners := 0
	for range results {
		winners++
	}
	losers := 0
	for err := range errs {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("claim race error = %v", err)
		}
		losers++
	}
	var active int
	if err := store.db.QueryRow(`SELECT count(*) FROM background_runs WHERE effect_phase='provision_intent'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if winners != 1 || losers != workers-1 || active != 1 {
		t.Fatalf("claim race winners=%d losers=%d active=%d", winners, losers, active)
	}
}

func TestBackgroundRunClaimRequiresProfileButRecoversAcrossImageRotation(t *testing.T) {
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
	if _, err := store.ClaimNextBackgroundRun(context.Background(), ClaimNextBackgroundRunParams{
		WorkspaceID: testWorkspaceID(), ClaimOwner: "wrong-profile", Now: now, LeaseDuration: time.Minute,
		Profile: "opencode-1.18.16", ImageIdentity: second.BackgroundRun.ImageIdentity,
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("old profile claim = %v", err)
	}
	claimed, err := store.ClaimNextBackgroundRun(context.Background(), ClaimNextBackgroundRunParams{
		WorkspaceID: testWorkspaceID(), ClaimOwner: "wrong-image", Now: now, LeaseDuration: time.Minute,
		Profile: BackgroundRunSourceProfile, ImageIdentity: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	})
	if err != nil || claimed.TaskID != first.TaskID || claimed.ImageIdentity != first.BackgroundRun.ImageIdentity {
		t.Fatalf("rotated image recovery claim = %+v, error = %v", claimed, err)
	}
	if _, err := store.ClaimNextBackgroundRun(context.Background(), ClaimNextBackgroundRunParams{
		WorkspaceID: testWorkspaceID(), ClaimOwner: "second-image", Now: now, LeaseDuration: time.Minute,
		Profile: BackgroundRunSourceProfile, ImageIdentity: second.BackgroundRun.ImageIdentity,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second image bypassed workspace capacity: %v", err)
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
	run, claim := advanceBackgroundRunToPromptIntent(t, store, params.BackgroundRun.ImageIdentity, now)
	work, err := store.ReadClaimedBackgroundRunWork(context.Background(), claim)
	if err != nil || work.Prompt != params.Prompt || !work.Deadline.Equal(params.Deadline.Truncate(time.Millisecond)) || work.AttemptTimeout != time.Hour {
		t.Fatalf("claimed work = %+v, error=%v", work, err)
	}
	if run.PromptRequestAttemptedAt != nil {
		t.Fatal("prompt was attempted before the irreversible fence")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	run, err = store.ClaimActiveBackgroundRun(context.Background(), ClaimBackgroundRunParams{
		WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID, Generation: run.Generation,
		ExpectedRevision: run.Revision, ExpectedState: run.State, ExpectedPhase: run.EffectPhase, CancelEpoch: run.CancelEpoch,
		ClaimOwner: "prompt-takeover", Now: now.Add(3 * time.Minute), LeaseDuration: time.Minute,
		Profile: BackgroundRunSourceProfile, ImageIdentity: run.ImageIdentity,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim = backgroundRunClaim(run, now.Add(3*time.Minute))
	run, err = store.RecordBackgroundRunPromptRequestAttempted(context.Background(), claim)
	if err != nil || run.PromptRequestAttemptedAt == nil {
		t.Fatalf("prompt attempt fence = %+v, error=%v", run, err)
	}
	advanceBackgroundClaim(&claim, run)
	claim.Now = claim.Now.Add(time.Millisecond)
	if _, err := store.RecordBackgroundRunPromptRequestAttempted(context.Background(), claim); !errors.Is(err, ErrInvalidState) {
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
	work, err := store.ClaimNextBackgroundRunWork(context.Background(), ClaimNextBackgroundRunParams{
		WorkspaceID: testWorkspaceID(), ClaimOwner: "timeout-worker", Now: now, LeaseDuration: time.Minute,
		Profile: BackgroundRunSourceProfile, ImageIdentity: params.BackgroundRun.ImageIdentity,
	})
	if err != nil {
		t.Fatal(err)
	}
	actor := testDeliveryActor()
	actor.Type, actor.ID, actor.DisplayName = task.ActorSystem, "background-timeout", "Background timeout"
	timedOut, err := store.RequestBackgroundRunTimeout(context.Background(), RequestBackgroundRunTimeoutParams{
		BackgroundRunClaim: backgroundRunClaim(work.Run, now), AttemptEventID: testEventID(2081), TaskEventID: testEventID(2082), Actor: actor,
	})
	if err != nil || timedOut.State != BackgroundRunCleanupRequired || timedOut.EffectPhase != BackgroundRunEffectStopIntent ||
		timedOut.TimeoutRequestedAt == nil || timedOut.CancelEpoch != 0 || timedOut.StopReceiptID != "" {
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
	claimed, err := store.ClaimActiveBackgroundRun(context.Background(), ClaimBackgroundRunParams{
		WorkspaceID: timedOut.WorkspaceID, TaskID: timedOut.TaskID, AttemptID: timedOut.AttemptID, Generation: timedOut.Generation,
		ExpectedRevision: timedOut.Revision, ExpectedState: timedOut.State, ExpectedPhase: timedOut.EffectPhase,
		CancelEpoch: timedOut.CancelEpoch, ClaimOwner: "timeout-cleanup-restart", Now: now.Add(time.Second), LeaseDuration: 2 * time.Minute,
		Profile: BackgroundRunSourceProfile, ImageIdentity: timedOut.ImageIdentity,
	})
	if err != nil || claimed.TimeoutActor == nil || *claimed.TimeoutActor != actor {
		t.Fatalf("restarted timeout claim = %+v, error=%v", claimed, err)
	}
	cleanupClaim := backgroundRunClaim(claimed, now.Add(2*time.Second))
	for _, step := range []func(context.Context, RecordBackgroundRunEvidenceParams) (BackgroundRun, error){
		store.RecordBackgroundRunWriterInactive, store.RecordBackgroundRunRouteRemoved, store.RecordBackgroundRunContainerRemoved,
		store.RecordBackgroundRunVolumeRemoved, store.RecordBackgroundRunCloneRemoved,
	} {
		claimed, err = step(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunClaim: cleanupClaim, Evidence: "exact timeout cleanup"})
		if err != nil {
			t.Fatalf("timeout cleanup from %s: %v", cleanupClaim.ExpectedPhase, err)
		}
		advanceBackgroundClaim(&cleanupClaim, claimed)
		cleanupClaim.Now = cleanupClaim.Now.Add(time.Second)
	}
	wrongActor := actor
	wrongActor.ID, wrongActor.RequestID = "different-timeout", "different-timeout"
	if _, err := store.FinalizeBackgroundRunFailure(context.Background(), FinalizeBackgroundRunFailureParams{
		BackgroundRunClaim: cleanupClaim, AttemptEventID: testEventID(2083), TaskEventID: testEventID(2084), Actor: wrongActor,
		Reason: "attempt_timeout", Evidence: "resources absent", CleanupProof: "exact timeout cleanup",
	}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("different timeout actor finalization = %v", err)
	}
	final, err := store.FinalizeBackgroundRunFailure(context.Background(), FinalizeBackgroundRunFailureParams{
		BackgroundRunClaim: cleanupClaim, AttemptEventID: testEventID(2085), TaskEventID: testEventID(2086), Actor: actor,
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
				run, claim := prepareBackgroundRunCleanup(t, store, params, state, phase, now, n)
				failed, err := store.MarkBackgroundRunCleanupRequired(context.Background(), MarkBackgroundRunCleanupRequiredParams{
					BackgroundRunClaim: claim, Error: "cleanup observation unavailable",
				})
				wantState := state
				if state == BackgroundRunCanceling {
					wantState = BackgroundRunCleanupRequired
				}
				if err != nil || failed.State != wantState || failed.EffectPhase != phase || failed.LastError != "cleanup observation unavailable" ||
					failed.ClaimOwner != "" || failed.ClaimExpiresAt != nil || failed.Revision != run.Revision+1 {
					t.Fatalf("durable cleanup failure = %+v, error=%v", failed, err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				store = openTestStore(t, path)
				retry, err := store.ClaimActiveBackgroundRun(context.Background(), ClaimBackgroundRunParams{
					WorkspaceID: failed.WorkspaceID, TaskID: failed.TaskID, AttemptID: failed.AttemptID, Generation: failed.Generation,
					ExpectedRevision: failed.Revision, ExpectedState: failed.State, ExpectedPhase: failed.EffectPhase, CancelEpoch: failed.CancelEpoch,
					ClaimOwner: "cleanup-retry", Now: claim.Now.Add(time.Second), LeaseDuration: time.Minute,
					Profile: BackgroundRunSourceProfile, ImageIdentity: failed.ImageIdentity,
				})
				if err != nil || retry.State != wantState || retry.EffectPhase != phase || retry.ClaimGeneration != failed.ClaimGeneration+1 {
					t.Fatalf("cleanup retry claim = %+v, error=%v", retry, err)
				}
			})
		}
	}
}

func TestBackgroundRunDiagnosticResultReadyIsRejected(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	first := testBackgroundRunAdmission(2100, "result-cleanup-first")
	if _, err := store.AdmitBackgroundRun(context.Background(), first); err != nil {
		t.Fatal(err)
	}

	now := testTime.Truncate(time.Millisecond).Add(time.Minute)
	run, claim := advanceBackgroundRunToPrompt(t, store, first.BackgroundRun.ImageIdentity, now)
	run, err := store.RecordBackgroundRunResultReady(context.Background(), RecordBackgroundRunEvidenceParams{
		BackgroundRunClaim: claim, Evidence: "sealed result exact",
	})
	if !errors.Is(err, ErrInvalidState) || run.TaskID != "" {
		t.Fatalf("diagnostic readiness = %+v, error = %v", run, err)
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
	run, err := store.ClaimNextBackgroundRun(context.Background(), ClaimNextBackgroundRunParams{
		WorkspaceID: testWorkspaceID(), ClaimOwner: "pre-effect-worker", Now: now, LeaseDuration: time.Minute,
		Profile: BackgroundRunSourceProfile, ImageIdentity: first.BackgroundRun.ImageIdentity,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim := BackgroundRunClaim{WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID,
		Generation: run.Generation, ClaimOwner: run.ClaimOwner, ClaimGeneration: run.ClaimGeneration,
		ExpectedRevision: run.Revision, ExpectedState: run.State, ExpectedPhase: run.EffectPhase,
		CancelEpoch: run.CancelEpoch, Now: now.Add(time.Second)}
	if _, err := store.FinalizeBackgroundRunFailure(context.Background(), FinalizeBackgroundRunFailureParams{
		BackgroundRunClaim: claim, AttemptEventID: testEventID(2152), TaskEventID: testEventID(2153),
		Actor: testDeliveryActor(), Reason: "background_image_unavailable", Evidence: "image inspect returned deterministic absence",
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("pre-effect failure without absence proof = %v", err)
	}
	final, err := store.FinalizeBackgroundRunFailure(context.Background(), FinalizeBackgroundRunFailureParams{
		BackgroundRunClaim: claim, AttemptEventID: testEventID(2152), TaskEventID: testEventID(2153),
		Actor: testDeliveryActor(), Reason: "background_image_unavailable", Evidence: "image inspect returned deterministic absence",
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
	next, err := store.ClaimNextBackgroundRun(context.Background(), ClaimNextBackgroundRunParams{
		WorkspaceID: testWorkspaceID(), ClaimOwner: "after-pre-effect", Now: claim.Now.Add(time.Second), LeaseDuration: time.Minute,
		Profile: BackgroundRunSourceProfile, ImageIdentity: second.BackgroundRun.ImageIdentity,
	})
	if err != nil || next.TaskID != second.TaskID {
		t.Fatalf("capacity after pre-effect failure = %+v, error = %v", next, err)
	}
}

func prepareBackgroundRunCleanup(t *testing.T, store *Store, params AdmitBackgroundRunParams, state BackgroundRunState, phase BackgroundRunEffectPhase, now time.Time, n int) (BackgroundRun, BackgroundRunClaim) {
	t.Helper()
	run, claim := advanceBackgroundRunToPrompt(t, store, params.BackgroundRun.ImageIdentity, now)
	var err error
	switch state {
	case BackgroundRunCanceling:
		stop := StopBackgroundRunParams{
			WorkspaceID: testWorkspaceID(), TaskID: run.TaskID, ReceiptID: testReceiptID(5000 + n),
			AttemptEventID: testEventID(5001 + n), TaskEventID: testEventID(5002 + n), Claim: params.Claim,
			APIContractVersion: "run-v1", StoppedAt: claim.Now,
		}
		stop.Claim.Scope.CommandKind = StopBackgroundRunCommand
		stop.Claim.Key = task.IdempotencyKey(fmt.Sprintf("cleanup-stop-%d", n))
		stop.Claim.RequestHash = sha256.Sum256([]byte(stop.Claim.Key))
		stopped, stopErr := store.StopBackgroundRun(context.Background(), stop)
		if stopErr != nil {
			t.Fatal(stopErr)
		}
		run, err = store.ClaimBackgroundRunStop(context.Background(), ClaimBackgroundRunParams{
			WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID, Generation: run.Generation,
			ExpectedRevision: stopped.Run.Revision, ExpectedState: stopped.Run.State, ExpectedPhase: stopped.Run.EffectPhase,
			CancelEpoch: stopped.Run.CancelEpoch, ClaimOwner: "cleanup-worker", Now: stop.StoppedAt.Add(time.Second),
			LeaseDuration: 2 * time.Minute, Profile: BackgroundRunSourceProfile, ImageIdentity: run.ImageIdentity,
		})
		if err != nil {
			t.Fatal(err)
		}
		claim = backgroundRunClaim(run, stop.StoppedAt.Add(2*time.Second))
	case BackgroundRunCleanupRequired:
		run, err = store.MarkBackgroundRunCleanupRequired(context.Background(), MarkBackgroundRunCleanupRequiredParams{
			BackgroundRunClaim: claim, Error: "prompt admitted but coordinator unavailable",
		})
		if err != nil {
			t.Fatal(err)
		}
		run, err = store.ClaimActiveBackgroundRun(context.Background(), ClaimBackgroundRunParams{
			WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID, Generation: run.Generation,
			ExpectedRevision: run.Revision, ExpectedState: run.State, ExpectedPhase: run.EffectPhase, CancelEpoch: run.CancelEpoch,
			ClaimOwner: "cleanup-worker", Now: claim.Now.Add(time.Second), LeaseDuration: 2 * time.Minute,
			Profile: BackgroundRunSourceProfile, ImageIdentity: run.ImageIdentity,
		})
		if err != nil {
			t.Fatal(err)
		}
		claim = backgroundRunClaim(run, claim.Now.Add(2*time.Second))
	case BackgroundRunResultReady:
		run, err = store.RecordBackgroundRunResultReady(context.Background(), RecordBackgroundRunEvidenceParams{
			BackgroundRunClaim: claim, Evidence: "sealed result exact",
		})
		if err != nil {
			t.Fatal(err)
		}
		advanceBackgroundClaim(&claim, run)
		claim.Now = claim.Now.Add(time.Second)
		run, err = store.RequestBackgroundRunResultCleanup(context.Background(), RecordBackgroundRunEvidenceParams{
			BackgroundRunClaim: claim, Evidence: "result retention permits cleanup",
		})
		if err != nil {
			t.Fatal(err)
		}
		advanceBackgroundClaim(&claim, run)
		claim.Now = claim.Now.Add(time.Second)
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
			BackgroundRunClaim: claim, Evidence: "exact cleanup observation",
		})
		if err != nil {
			t.Fatalf("advance cleanup from %s: %v", claim.ExpectedPhase, err)
		}
		advanceBackgroundClaim(&claim, run)
		claim.Now = claim.Now.Add(time.Second)
	}
	return run, claim
}

func backgroundRunClaim(run BackgroundRun, now time.Time) BackgroundRunClaim {
	return BackgroundRunClaim{
		WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID, Generation: run.Generation,
		ClaimOwner: run.ClaimOwner, ClaimGeneration: run.ClaimGeneration, ExpectedRevision: run.Revision,
		ExpectedState: run.State, ExpectedPhase: run.EffectPhase, CancelEpoch: run.CancelEpoch, Now: now,
	}
}

func advanceBackgroundRunToPrompt(t *testing.T, store *Store, image string, now time.Time) (BackgroundRun, BackgroundRunClaim) {
	run, claim := advanceBackgroundRunToPromptIntent(t, store, image, now)
	var err error
	run, err = store.RecordBackgroundRunPromptRequestAttempted(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	advanceBackgroundClaim(&claim, run)
	claim.Now = claim.Now.Add(time.Second)
	run, err = store.RecordBackgroundRunPromptAdmitted(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunClaim: claim, Evidence: "prompt admitted"})
	if err != nil {
		t.Fatal(err)
	}
	advanceBackgroundClaim(&claim, run)
	claim.Now = claim.Now.Add(time.Second)
	return run, claim
}

func advanceBackgroundRunToPromptIntent(t *testing.T, store *Store, image string, now time.Time) (BackgroundRun, BackgroundRunClaim) {
	t.Helper()
	run, err := store.ClaimNextBackgroundRun(context.Background(), ClaimNextBackgroundRunParams{
		WorkspaceID: testWorkspaceID(), ClaimOwner: "result-worker", Now: now, LeaseDuration: 2 * time.Minute,
		Profile: BackgroundRunSourceProfile, ImageIdentity: image,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim := BackgroundRunClaim{WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID,
		Generation: run.Generation, ClaimOwner: run.ClaimOwner, ClaimGeneration: run.ClaimGeneration,
		ExpectedRevision: run.Revision, ExpectedState: run.State, ExpectedPhase: run.EffectPhase, CancelEpoch: run.CancelEpoch, Now: now.Add(time.Second)}
	evidenceStep := func(step func(context.Context, RecordBackgroundRunEvidenceParams) (BackgroundRun, error), evidence string) {
		run, err = step(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunClaim: claim, Evidence: evidence})
		if err != nil {
			t.Fatalf("advance from %s: %v", claim.ExpectedPhase, err)
		}
		advanceBackgroundClaim(&claim, run)
		claim.Now = claim.Now.Add(time.Second)
	}
	evidenceStep(store.RecordBackgroundRunCloneObserved, "clone observed")
	evidenceStep(store.RecordBackgroundRunVolumeObserved, "volume observed")
	run, err = store.RecordBackgroundRunContainerObserved(context.Background(), RecordBackgroundRunContainerObservedParams{
		BackgroundRunClaim: claim, ContainerID: "result-container", ContainerStartedAt: "2026-08-31T12:01:00Z",
		RuntimeEpoch: 1, HostPort: 49153, Evidence: "container observed",
	})
	if err != nil {
		t.Fatal(err)
	}
	advanceBackgroundClaim(&claim, run)
	claim.Now = claim.Now.Add(time.Second)
	evidenceStep(store.RecordBackgroundRunHealthObserved, "health observed")
	evidenceStep(store.RecordBackgroundRunReady, "ready observed")
	evidenceStep(store.RecordBackgroundRunSessionObserved, "session observed")
	evidenceStep(store.RecordBackgroundRunPromptIntent, "prompt intent")
	return run, claim
}

func advanceBackgroundClaim(claim *BackgroundRunClaim, run BackgroundRun) {
	claim.ExpectedRevision = run.Revision
	claim.ExpectedState = run.State
	claim.ExpectedPhase = run.EffectPhase
	claim.CancelEpoch = run.CancelEpoch
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
