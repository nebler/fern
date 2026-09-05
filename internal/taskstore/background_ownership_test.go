package taskstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

func TestBackgroundRunColdOwnershipTransferAndHandback(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	params := testBackgroundRunAdmission(8800, "ownership-run")
	run, staleClaim := prepareWorkingOwnershipRun(t, store, params)
	actor := task.ActorSnapshot{Type: task.ActorDevice, ID: "device-1", DisplayName: "Phone", CredentialID: "device-1",
		Authentication: "paired_cookie", RequestID: "request-1"}
	takeoverClaim := task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: run.WorkspaceID, CommandKind: RequestBackgroundRunTakeoverCommand},
		Key: "takeover-1", RequestHash: sha256.Sum256([]byte("takeover-1")), Actor: actor}
	takeover, err := store.RequestBackgroundRunTakeover(context.Background(), RequestBackgroundRunTakeoverParams{
		WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, ReceiptID: testReceiptID(8810), Claim: takeoverClaim,
		APIContractVersion: "control-v1", RequestedAt: staleClaim.Now.Add(time.Second),
	})
	if err != nil || takeover.Ownership.Mode != BackgroundRunTakeoverRequested || takeover.Ownership.Phase != BackgroundRunOwnershipAgentRouteRemoval ||
		takeover.Ownership.WriterGeneration != 1 || takeover.Ownership.TargetWriterGeneration != 2 || takeover.Ownership.ContainerID != run.ObservedContainerID {
		t.Fatalf("takeover=%+v error=%v", takeover, err)
	}
	if replay, replayErr := store.RequestBackgroundRunTakeover(context.Background(), RequestBackgroundRunTakeoverParams{
		WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, ReceiptID: testReceiptID(8811), Claim: takeoverClaim,
		APIContractVersion: "control-v1", RequestedAt: staleClaim.Now.Add(2 * time.Second),
	}); replayErr != nil || !replay.Replayed || replay.Receipt.ID != takeover.Receipt.ID {
		t.Fatalf("takeover replay=%+v error=%v", replay, replayErr)
	}
	if _, err := store.RecordBackgroundRunWorkObservation(context.Background(), RecordBackgroundRunEvidenceParams{
		BackgroundRunClaim: staleClaim, Evidence: "stale writer"}, BackgroundRunWorking); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stale agent claim retained authority: %v", err)
	}

	now := staleClaim.Now.Add(3 * time.Second)
	ownership := takeover.Ownership
	for _, next := range []BackgroundRunOwnershipPhase{
		BackgroundRunOwnershipAgentStop, BackgroundRunOwnershipAgentRemove, BackgroundRunOwnershipAgentVolumeRemove,
		BackgroundRunOwnershipHumanCreate, BackgroundRunOwnershipHumanStart,
	} {
		ownership = claimAndAdvanceOwnership(t, store, ownership, now, BackgroundRunTakeoverRequested, next, nil)
		now = now.Add(time.Second)
	}
	humanStarted := time.Date(2026, 8, 31, 13, 0, 0, 123, time.UTC).Format(time.RFC3339Nano)
	humanID := hex.EncodeToString(sha256.New().Sum([]byte("human-runtime")))
	if len(humanID) > 64 {
		humanID = humanID[:64]
	}
	humanToken := runtimeTokenForTest(humanID, humanStarted)
	ownership = claimAndAdvanceOwnership(t, store, ownership, now, BackgroundRunHumanOwned, BackgroundRunOwnershipHumanActive,
		func(p *AdvanceBackgroundRunOwnershipParams) {
			p.WriterGeneration = 2
			p.ContainerIdentity = ownership.TargetContainerIdentity
			p.ContainerID, p.ContainerStartedAt, p.RuntimeEpoch, p.RuntimeToken = humanID, humanStarted, mustStartedEpoch(t, humanStarted), humanToken
			p.VolumeIdentity, p.EndpointIdentity, p.HostPort, p.OpenCodeSessionID, p.OpenCodeMessageID = "", "", 0, "", ""
		})
	if ownership.Mode != BackgroundRunHumanOwned || ownership.WriterGeneration != 2 || ownership.VolumeIdentity != "" {
		t.Fatalf("human ownership=%+v", ownership)
	}
	stop := StopBackgroundRunParams{WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, ReceiptID: testReceiptID(8820),
		AttemptEventID: testEventID(8821), TaskEventID: testEventID(8822), Claim: params.Claim, APIContractVersion: "run-v1", StoppedAt: now.Add(time.Second)}
	stop.Claim.Scope.CommandKind, stop.Claim.Key, stop.Claim.RequestHash = StopBackgroundRunCommand, "stop-human", sha256.Sum256([]byte("stop-human"))
	if _, err := store.StopBackgroundRun(context.Background(), stop); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("ordinary stop bypassed human ownership: %v", err)
	}

	handbackClaim := task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: run.WorkspaceID, CommandKind: RequestBackgroundRunHandbackCommand},
		Key: "handback-1", RequestHash: sha256.Sum256([]byte("handback-1")), Actor: actor}
	handback, err := store.RequestBackgroundRunHandback(context.Background(), RequestBackgroundRunHandbackParams{
		WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, ReceiptID: testReceiptID(8830), Claim: handbackClaim,
		APIContractVersion: "control-v1", RequestedAt: now.Add(2 * time.Second),
	})
	if err != nil || handback.Ownership.Mode != BackgroundRunHandbackRequested || handback.Ownership.TargetWriterGeneration != 3 {
		t.Fatalf("handback=%+v error=%v", handback, err)
	}
	ownership, now = handback.Ownership, now.Add(3*time.Second)
	for _, next := range []BackgroundRunOwnershipPhase{
		BackgroundRunOwnershipHumanStop, BackgroundRunOwnershipHumanRemove, BackgroundRunOwnershipAgentVolumeCreate,
		BackgroundRunOwnershipAgentCreate, BackgroundRunOwnershipAgentStart,
	} {
		ownership = claimAndAdvanceOwnership(t, store, ownership, now, BackgroundRunHandbackRequested, next, nil)
		now = now.Add(time.Second)
	}
	agentStarted := time.Date(2026, 8, 31, 14, 0, 0, 456, time.UTC).Format(time.RFC3339Nano)
	agentID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	ownership = claimAndAdvanceOwnership(t, store, ownership, now, BackgroundRunHandbackRequested, BackgroundRunOwnershipAgentHealth,
		func(p *AdvanceBackgroundRunOwnershipParams) {
			p.WriterGeneration = 3
			p.ContainerIdentity, p.VolumeIdentity, p.EndpointIdentity = run.ContainerIdentity, run.VolumeIdentity, run.EndpointIdentity
			p.ContainerID, p.ContainerStartedAt, p.RuntimeEpoch, p.RuntimeToken, p.HostPort = agentID, agentStarted, mustStartedEpoch(t, agentStarted), runtimeTokenForTest(agentID, agentStarted), 49153
			p.OpenCodeSessionID, p.OpenCodeMessageID = run.OpenCodeSessionID, run.OpenCodeMessageID
		})
	for _, next := range []BackgroundRunOwnershipPhase{BackgroundRunOwnershipAgentSession, BackgroundRunOwnershipAgentPrompt} {
		now = now.Add(time.Second)
		ownership = claimAndAdvanceOwnership(t, store, ownership, now, BackgroundRunHandbackRequested, next, nil)
	}
	now = now.Add(time.Second)
	ownership = claimAndAdvanceOwnership(t, store, ownership, now, BackgroundRunAgentOwned, BackgroundRunOwnershipAgentActive, nil)
	if ownership.Mode != BackgroundRunAgentOwned || ownership.WriterGeneration != 3 || ownership.ContainerID != agentID {
		t.Fatalf("fresh agent ownership=%+v", ownership)
	}
}

func TestBackgroundRunControlJournalFencesWorkAndRecovers(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	params := testBackgroundRunAdmission(8900, "control-run")
	run, staleClaim := prepareWorkingOwnershipRun(t, store, params)
	actor := task.ActorSnapshot{Type: task.ActorDevice, ID: "device-2", DisplayName: "Laptop", CredentialID: "device-2",
		Authentication: "paired_cookie", RequestID: "request-2"}
	claim := task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: run.WorkspaceID, CommandKind: SteerBackgroundRunCommand},
		Key: "steer-1", RequestHash: sha256.Sum256([]byte("steer-1")), Actor: actor}
	admission, err := store.AdmitBackgroundRunControl(context.Background(), AdmitBackgroundRunControlParams{
		WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, ReceiptID: testReceiptID(8910), OpenCodeMessageID: "msg_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Instruction: "Inspect the failing test and continue.", Claim: claim, APIContractVersion: "control-v1", RequestedAt: staleClaim.Now.Add(time.Second),
	})
	if err != nil || admission.Control.State != BackgroundRunControlRequested || admission.Control.WriterGeneration != 1 {
		t.Fatalf("control admission=%+v error=%v", admission, err)
	}
	if replay, replayErr := store.AdmitBackgroundRunControl(context.Background(), AdmitBackgroundRunControlParams{
		WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, ReceiptID: testReceiptID(8911), OpenCodeMessageID: "msg_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Instruction: "Inspect the failing test and continue.", Claim: claim, APIContractVersion: "control-v1", RequestedAt: staleClaim.Now.Add(2 * time.Second),
	}); replayErr != nil || !replay.Replayed || replay.Control.OpenCodeMessageID != admission.Control.OpenCodeMessageID {
		t.Fatalf("control replay=%+v error=%v", replay, replayErr)
	}
	if _, err := store.RecordBackgroundRunWorkObservation(context.Background(), RecordBackgroundRunEvidenceParams{
		BackgroundRunClaim: staleClaim, Evidence: "stale observation"}, BackgroundRunWorking); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("control admission did not fence stale work: %v", err)
	}
	now := staleClaim.Now.Add(3 * time.Second)
	control, err := store.ClaimNextBackgroundRunControl(context.Background(), ClaimNextBackgroundRunControlParams{
		WorkspaceID: run.WorkspaceID, ClaimOwner: "control-worker", Now: now, LeaseDuration: time.Minute})
	if err != nil || control.ReceiptID != admission.Control.ReceiptID {
		t.Fatalf("claim control=%+v error=%v", control, err)
	}
	control, err = store.MarkBackgroundRunControlAttempted(context.Background(), BackgroundRunControlClaim{
		WorkspaceID: control.WorkspaceID, ReceiptID: control.ReceiptID, ExpectedRevision: control.Revision, ExpectedState: control.State,
		ClaimOwner: control.ClaimOwner, ClaimGeneration: control.ClaimGeneration, Now: now.Add(time.Second),
	})
	if err != nil || control.State != BackgroundRunControlAttempted || control.AttemptedAt == nil {
		t.Fatalf("attempt control=%+v error=%v", control, err)
	}
	control, err = store.CompleteBackgroundRunControl(context.Background(), BackgroundRunControlClaim{
		WorkspaceID: control.WorkspaceID, ReceiptID: control.ReceiptID, ExpectedRevision: control.Revision, ExpectedState: control.State,
		ClaimOwner: control.ClaimOwner, ClaimGeneration: control.ClaimGeneration, Now: now.Add(2 * time.Second),
	}, BackgroundRunControlSucceeded, "")
	if err != nil || control.State != BackgroundRunControlSucceeded || control.CompletedAt == nil || control.ClaimOwner != "" {
		t.Fatalf("complete control=%+v error=%v", control, err)
	}
	latest, err := store.LatestBackgroundRunControl(context.Background(), run.WorkspaceID, run.TaskID)
	if err != nil || latest.ReceiptID != control.ReceiptID || latest.State != BackgroundRunControlSucceeded {
		t.Fatalf("latest control=%+v error=%v", latest, err)
	}
}

func TestExpiredHumanOwnershipAutomaticallyBeginsHandback(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	params := testBackgroundRunAdmission(8950, "expired-human")
	run, _ := prepareWorkingOwnershipRun(t, store, params)
	started := time.Date(2026, 9, 5, 9, 0, 0, 123, time.UTC).Format(time.RFC3339Nano)
	containerID := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	now := testTime.Add(2 * time.Hour).UTC().Truncate(time.Millisecond)
	if _, err := store.db.Exec(`UPDATE background_run_ownerships SET mode='human_owned',phase='human_active',writer_generation=2,
container_identity='human-shell',container_id=?,container_started_at=?,runtime_epoch=?,runtime_token=?,volume_identity=NULL,endpoint_identity=NULL,
host_port=NULL,opencode_session_id=NULL,opencode_message_id=NULL,target_writer_generation=2,target_container_identity='human-shell',
target_volume_identity=NULL,target_endpoint_identity=NULL,target_opencode_session_id=NULL,target_opencode_message_id=NULL,
revision=revision+1,updated_at=? WHERE task_id=?`, containerID, started, mustStartedEpoch(t, started), runtimeTokenForTest(containerID, started), unixMillis(now), run.TaskID); err != nil {
		t.Fatal(err)
	}
	ownership, err := store.ClaimNextBackgroundRunOwnership(context.Background(), ClaimNextBackgroundRunOwnershipParams{
		WorkspaceID: run.WorkspaceID, ClaimOwner: "deadline-worker", Now: now.Add(time.Second), LeaseDuration: time.Minute})
	if err != nil || ownership.Mode != BackgroundRunHandbackRequested || ownership.Phase != BackgroundRunOwnershipHumanRouteRemoval ||
		ownership.TargetWriterGeneration != 3 || ownership.TargetContainerIdentity != run.ContainerIdentity || ownership.RequestReceiptID != "" {
		t.Fatalf("expired ownership=%+v error=%v", ownership, err)
	}
}

func prepareWorkingOwnershipRun(t *testing.T, store *Store, params AdmitBackgroundRunParams) (BackgroundRun, BackgroundRunClaim) {
	t.Helper()
	if _, err := store.AdmitBackgroundRun(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	now := testTime.Truncate(time.Millisecond).Add(time.Minute)
	run, err := store.ClaimNextBackgroundRun(context.Background(), ClaimNextBackgroundRunParams{WorkspaceID: params.Claim.Scope.WorkspaceID,
		ClaimOwner: "ownership-worker", Now: now, LeaseDuration: 2 * time.Minute, Profile: BackgroundRunSourceProfile, ImageIdentity: params.BackgroundRun.ImageIdentity})
	if err != nil {
		t.Fatal(err)
	}
	claim := BackgroundRunClaim{WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID, Generation: run.Generation,
		ClaimOwner: run.ClaimOwner, ClaimGeneration: run.ClaimGeneration, ExpectedRevision: run.Revision, ExpectedState: run.State,
		ExpectedPhase: run.EffectPhase, CancelEpoch: run.CancelEpoch, Now: now.Add(time.Second)}
	advance := func(next BackgroundRun) {
		run = next
		claim.ExpectedRevision, claim.ExpectedState, claim.ExpectedPhase, claim.Now = run.Revision, run.State, run.EffectPhase, claim.Now.Add(time.Second)
	}
	run, err = store.RecordBackgroundRunCloneObserved(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunClaim: claim, Evidence: "clone"})
	if err != nil {
		t.Fatal(err)
	}
	advance(run)
	run, err = store.RecordBackgroundRunVolumeObserved(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunClaim: claim, Evidence: "volume"})
	if err != nil {
		t.Fatal(err)
	}
	advance(run)
	started := time.Date(2026, 8, 31, 12, 0, 0, 123, time.UTC).Format(time.RFC3339Nano)
	run, err = store.RecordBackgroundRunContainerObserved(context.Background(), RecordBackgroundRunContainerObservedParams{BackgroundRunClaim: claim,
		ContainerID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ContainerStartedAt: started,
		RuntimeEpoch: mustStartedEpoch(t, started), HostPort: 49152, Evidence: "container"})
	if err != nil {
		t.Fatal(err)
	}
	advance(run)
	for _, record := range []func(context.Context, RecordBackgroundRunEvidenceParams) (BackgroundRun, error){
		store.RecordBackgroundRunHealthObserved, store.RecordBackgroundRunReady, store.RecordBackgroundRunSessionObserved,
	} {
		run, err = record(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunClaim: claim, Evidence: "exact"})
		if err != nil {
			t.Fatal(err)
		}
		advance(run)
	}
	run, err = store.RecordBackgroundRunPromptIntent(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunClaim: claim, Evidence: "prompt intent"})
	if err != nil {
		t.Fatal(err)
	}
	advance(run)
	run, err = store.RecordBackgroundRunPromptRequestAttempted(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	advance(run)
	run, err = store.RecordBackgroundRunPromptAdmitted(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunClaim: claim, Evidence: "prompt admitted"})
	if err != nil {
		t.Fatal(err)
	}
	claim.ExpectedRevision, claim.ExpectedState, claim.ExpectedPhase, claim.Now = run.Revision, run.State, run.EffectPhase, claim.Now.Add(time.Second)
	return run, claim
}

func claimAndAdvanceOwnership(t *testing.T, store *Store, current BackgroundRunOwnership, now time.Time, mode BackgroundRunOwnershipMode,
	phase BackgroundRunOwnershipPhase, mutate func(*AdvanceBackgroundRunOwnershipParams)) BackgroundRunOwnership {
	t.Helper()
	claimed, err := store.ClaimNextBackgroundRunOwnership(context.Background(), ClaimNextBackgroundRunOwnershipParams{
		WorkspaceID: current.WorkspaceID, ClaimOwner: "ownership-worker", Now: now, LeaseDuration: time.Minute})
	if err != nil || claimed.TaskID != current.TaskID {
		t.Fatalf("claim ownership=%+v error=%v", claimed, err)
	}
	p := AdvanceBackgroundRunOwnershipParams{BackgroundRunOwnershipClaim: BackgroundRunOwnershipClaim{
		WorkspaceID: claimed.WorkspaceID, TaskID: claimed.TaskID, AttemptID: claimed.AttemptID, RunGeneration: claimed.RunGeneration,
		ExpectedRevision: claimed.Revision, ExpectedMode: claimed.Mode, ExpectedPhase: claimed.Phase, ClaimOwner: claimed.ClaimOwner,
		ClaimGeneration: claimed.ClaimGeneration, Now: now.Add(time.Millisecond)}, Mode: mode, Phase: phase, WriterGeneration: claimed.WriterGeneration,
		ContainerIdentity: claimed.ContainerIdentity, ContainerID: claimed.ContainerID, ContainerStartedAt: claimed.ContainerStartedAt,
		RuntimeEpoch: claimed.RuntimeEpoch, RuntimeToken: claimed.RuntimeToken, VolumeIdentity: claimed.VolumeIdentity,
		EndpointIdentity: claimed.EndpointIdentity, HostPort: claimed.HostPort, OpenCodeSessionID: claimed.OpenCodeSessionID, OpenCodeMessageID: claimed.OpenCodeMessageID}
	if mutate != nil {
		mutate(&p)
	}
	updated, err := store.AdvanceBackgroundRunOwnership(context.Background(), p)
	if err != nil {
		t.Fatalf("advance %s -> %s/%s: %v", claimed.Phase, mode, phase, err)
	}
	return updated
}

func runtimeTokenForTest(containerID, started string) string {
	digest := sha256.Sum256([]byte(containerID + "\x00" + started))
	return hex.EncodeToString(digest[:])
}

func mustStartedEpoch(t *testing.T, started string) int64 {
	t.Helper()
	value, err := time.Parse(time.RFC3339Nano, started)
	if err != nil {
		t.Fatal(err)
	}
	return value.UnixNano()
}
