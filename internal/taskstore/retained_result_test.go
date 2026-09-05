package taskstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

func TestBackgroundRunRetainedResultAuthorityEndToEnd(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	admission := testBackgroundRunAdmission(5100, "retained-result")
	if _, err := store.AdmitBackgroundRun(context.Background(), admission); err != nil {
		t.Fatal(err)
	}
	now := testTime.Truncate(time.Millisecond).Add(time.Minute)
	run, _ := advanceBackgroundRunToPrompt(t, store, admission.BackgroundRun.ImageIdentity, now)
	sealClaim := task.IdempotencyClaim{
		Scope: task.IdempotencyScope{WorkspaceID: run.WorkspaceID, CommandKind: SealBackgroundRunCommand},
		Key:   "retained-seal", RequestHash: sha256.Sum256([]byte("retained-seal")), Actor: admission.Claim.Actor,
	}
	seal := SealBackgroundRunParams{
		WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID, Generation: run.Generation,
		ExpectedRunRevision: run.Revision, ExpectedTaskRevision: 1, ExpectedAttemptRevision: 1,
		SealRequestID: task.SealRequestID(testID("slr_", 5101)), ReceiptID: testReceiptID(5102),
		ExportID: task.ArtifactExportID(testID("exp_", 5103)), ArtifactID: task.RetainedArtifactID(testID("art_", 5104)),
		MaterializationID: task.MaterializationID(testID("mat_", 5105)), ResultID: testResultID(5106),
		ResultEventID: testEventID(5107), TaskEventID: testEventID(5108), Claim: sealClaim,
		CommitEpochSeconds: now.Unix(), PolicyVersion: "background-retained.v1", APIContractVersion: "v1", AcceptedAt: now.Add(20 * time.Second),
	}
	sealed, err := store.SealBackgroundRun(context.Background(), seal)
	if err != nil || sealed.Run.State != BackgroundRunCanceling || sealed.Run.EffectPhase != BackgroundRunEffectSealIntent ||
		sealed.Export.Phase != BackgroundRunExportPhasePrepared {
		t.Fatalf("seal admission = %+v, error=%v", sealed, err)
	}
	replay, err := store.SealBackgroundRun(context.Background(), seal)
	if err != nil || !replay.Replayed || replay.Request.ExportID != seal.ExportID || replay.Request.ArtifactID != seal.ArtifactID {
		t.Fatalf("seal replay = %+v, error=%v", replay, err)
	}
	ownerMismatch := seal
	ownerMismatch.Claim.Actor.ID = "other-owner"
	ownerMismatch.Claim.Actor.CredentialID = "other-owner"
	if _, err := store.SealBackgroundRun(context.Background(), ownerMismatch); !errors.Is(err, ErrNotFound) {
		t.Fatalf("seal owner mismatch = %v", err)
	}
	stop := StopBackgroundRunParams{WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, ReceiptID: testReceiptID(5109),
		AttemptEventID: testEventID(5110), TaskEventID: testEventID(5111), Claim: task.IdempotencyClaim{
			Scope: task.IdempotencyScope{WorkspaceID: run.WorkspaceID, CommandKind: StopBackgroundRunCommand}, Key: "stop-after-seal",
			RequestHash: sha256.Sum256([]byte("stop-after-seal")), Actor: admission.Claim.Actor,
		}, APIContractVersion: "v1", StoppedAt: seal.AcceptedAt.Add(time.Second)}
	if _, err := store.StopBackgroundRun(context.Background(), stop); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stop after winning seal = %v", err)
	}

	claimedWork, err := store.ClaimNextBackgroundRunWork(context.Background(), ClaimNextBackgroundRunParams{
		WorkspaceID: run.WorkspaceID, ClaimOwner: "writer-fencer", Now: seal.AcceptedAt.Add(2 * time.Second), LeaseDuration: time.Minute,
		Profile: BackgroundRunSourceProfile, ImageIdentity: run.ImageIdentity,
	})
	claimedRun := claimedWork.Run
	if err != nil || claimedRun.State != BackgroundRunCanceling || claimedRun.EffectPhase != BackgroundRunEffectSealIntent {
		t.Fatal(err)
	}
	writerAt := seal.AcceptedAt.Add(3 * time.Second)
	stoppedAt := writerAt
	writerProof := struct {
		SealRequestID      task.SealRequestID    `json:"sealRequestId"`
		ExportID           task.ArtifactExportID `json:"exportId"`
		TaskID             task.TaskID           `json:"taskId"`
		AttemptID          task.AttemptID        `json:"attemptId"`
		Generation         int64                 `json:"generation"`
		Kind               WriterFenceKind       `json:"kind"`
		ContainerID        string                `json:"containerId,omitempty"`
		ContainerStartedAt string                `json:"containerStartedAt,omitempty"`
		RuntimeEpoch       int64                 `json:"runtimeEpoch,omitempty"`
		RuntimeToken       string                `json:"runtimeToken,omitempty"`
		StoppedAtMillis    *int64                `json:"stoppedAtMillis,omitempty"`
	}{seal.SealRequestID, seal.ExportID, run.TaskID, run.AttemptID, run.Generation, WriterFenceRuntimeStopped,
		run.ObservedContainerID, run.ObservedContainerStartedAt, run.RuntimeEpoch, "runtime-token", nil}
	stoppedMillis := stoppedAt.UnixMilli()
	writerProof.StoppedAtMillis = &stoppedMillis
	encodedWriterProof, _ := json.Marshal(writerProof)
	writerParams := RecordBackgroundRunWriterFenceParams{BackgroundRunClaim: backgroundRunClaim(claimedRun, writerAt),
		SealRequestID: seal.SealRequestID, ExportID: seal.ExportID, Kind: WriterFenceRuntimeStopped,
		ContainerID: run.ObservedContainerID, ContainerStartedAt: run.ObservedContainerStartedAt, RuntimeEpoch: run.RuntimeEpoch,
		RuntimeToken: "runtime-token", StoppedAt: &stoppedAt, ProofSHA256: sha256.Sum256(encodedWriterProof)}
	writerInactive, err := store.RecordBackgroundRunWriterFence(context.Background(), writerParams)
	if err != nil || writerInactive.EffectPhase != BackgroundRunEffectWriterInactive || writerInactive.ClaimOwner != "" {
		t.Fatalf("writer fence = %+v, error=%v", writerInactive, err)
	}
	if _, err := store.RecordBackgroundRunWriterFence(context.Background(), writerParams); err != nil {
		t.Fatalf("writer fence replay: %v", err)
	}
	writerClaim, err := store.ClaimNextBackgroundRun(context.Background(), ClaimNextBackgroundRunParams{WorkspaceID: run.WorkspaceID,
		ClaimOwner: "export-dispatcher", Now: writerAt.Add(time.Millisecond), LeaseDuration: time.Minute,
		Profile: BackgroundRunSourceProfile, ImageIdentity: run.ImageIdentity})
	if err != nil || writerClaim.EffectPhase != BackgroundRunEffectWriterInactive {
		t.Fatalf("writer-inactive production claim = %+v, error=%v", writerClaim, err)
	}

	export, err := store.ClaimBackgroundRunExport(context.Background(), ClaimBackgroundRunExportParams{
		ExportID: seal.ExportID, TaskID: run.TaskID, AttemptID: run.AttemptID, Generation: run.Generation,
		ExpectedRevision: 1, ExpectedPhase: BackgroundRunExportPhasePrepared, ClaimOwner: "export-dispatcher",
		Now: writerAt.Add(time.Second), LeaseDuration: 2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	failureAt := writerAt.Add(2 * time.Second)
	failureClaim := BackgroundRunExportClaim{ExportID: export.ID, TaskID: export.TaskID, AttemptID: export.AttemptID,
		Generation: export.Generation, ExpectedRevision: export.Revision, ExpectedPhase: export.Phase,
		ClaimOwner: export.ClaimOwner, ClaimGeneration: export.ClaimGeneration, Now: failureAt}
	recovery, err := store.MarkBackgroundRunExportRecoveryRequired(context.Background(), failureClaim, "injected export interruption")
	if err != nil {
		t.Fatal(err)
	}
	released, err := store.ReleaseBackgroundRunClaimAfterExportFailure(context.Background(), failureClaim)
	if err != nil || released.ClaimOwner != "" || released.ResultAuthorityPhase != "exporting" {
		t.Fatalf("release failed export run claim = %+v, error=%v", released, err)
	}
	reclaimed, err := store.ClaimNextBackgroundRun(context.Background(), ClaimNextBackgroundRunParams{WorkspaceID: run.WorkspaceID,
		ClaimOwner: "export-dispatcher", Now: failureAt.Add(time.Second), LeaseDuration: time.Minute,
		Profile: BackgroundRunSourceProfile, ImageIdentity: run.ImageIdentity})
	if err != nil || reclaimed.EffectPhase != BackgroundRunEffectExporting {
		t.Fatalf("reclaim failed export run = %+v, error=%v", reclaimed, err)
	}
	export, err = store.ClaimBackgroundRunExport(context.Background(), ClaimBackgroundRunExportParams{
		ExportID: export.ID, TaskID: export.TaskID, AttemptID: export.AttemptID, Generation: export.Generation,
		ExpectedRevision: recovery.Revision, ExpectedPhase: recovery.Phase, ClaimOwner: "export-dispatcher",
		Now: failureAt.Add(2 * time.Second), LeaseDuration: 2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	exportNow := failureAt.Add(3 * time.Second)
	exportClaim := func() BackgroundRunExportClaim {
		return BackgroundRunExportClaim{ExportID: export.ID, TaskID: export.TaskID, AttemptID: export.AttemptID,
			Generation: export.Generation, ExpectedRevision: export.Revision, ExpectedPhase: export.Phase,
			ClaimOwner: export.ClaimOwner, ClaimGeneration: export.ClaimGeneration, Now: exportNow}
	}
	advance := func(call func(context.Context, BackgroundRunExportClaim) (BackgroundRunExport, error)) {
		var stepErr error
		export, stepErr = call(context.Background(), exportClaim())
		if stepErr != nil {
			t.Fatalf("advance export from %s: %v", exportClaim().ExpectedPhase, stepErr)
		}
		exportNow = exportNow.Add(time.Second)
	}
	advance(store.RecordBackgroundRunSnapshotStarted)
	mode, blob, size := "100644", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", int64(12)
	resultManifest := []ManifestEntry{{PathBase64: "Y2hhbmdlLnR4dA==", ChangeKind: "added", NewMode: &mode, NewBlobOID: &blob, NewSize: &size}}
	resultManifestJSON, _ := json.Marshal(resultManifest)
	resultCommit := task.GitOID("1111111111111111111111111111111111111111")
	// /A== is standard Base64 for a non-UTF8 Git path prefix. It is artifact
	// data, not host-path authority, and must survive both Go and SQL guards.
	artifactManifest := json.RawMessage(`{"version":1,"changes":[{"path_base64":"/A=="}]}`)
	if _, err := store.SelectBackgroundRunSnapshot(context.Background(), SelectBackgroundRunSnapshotParams{
		BackgroundRunExportClaim: exportClaim(), ResultCommit: resultCommit, TreeOID: task.GitOID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Outcome: task.ResultChanged, ResultManifest: resultManifest, ChangesSHA256: sha256.Sum256(resultManifestJSON),
		ArtifactManifest:       json.RawMessage(`{"host_path":"/private/work"}`),
		ArtifactManifestSHA256: sha256.Sum256([]byte(`{"host_path":"/private/work"}`)),
		OpenCodeSessionID:      run.OpenCodeSessionID, OpenCodeMessageID: run.OpenCodeMessageID, CollectedAt: exportNow,
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unsafe artifact manifest = %v", err)
	}
	export, err = store.SelectBackgroundRunSnapshot(context.Background(), SelectBackgroundRunSnapshotParams{
		BackgroundRunExportClaim: exportClaim(), ResultCommit: resultCommit, TreeOID: task.GitOID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Outcome: task.ResultChanged, ResultManifest: resultManifest, ChangesSHA256: sha256.Sum256(resultManifestJSON),
		ArtifactManifest: artifactManifest, ArtifactManifestSHA256: sha256.Sum256(artifactManifest),
		OpenCodeSessionID: run.OpenCodeSessionID, OpenCodeMessageID: run.OpenCodeMessageID, CollectedAt: exportNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	exportNow = exportNow.Add(time.Second)
	advance(store.RecordBackgroundRunBundleWriteStarted)
	export, err = store.VerifyBackgroundRunBundle(context.Background(), VerifyBackgroundRunBundleParams{
		BackgroundRunExportClaim: exportClaim(), BundleSHA256: sha256.Sum256([]byte("bundle")), BundleBytes: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	exportNow = exportNow.Add(time.Second)
	advance(store.RecordBackgroundRunCASInstallStarted)
	advance(store.RecordBackgroundRunCASInstalled)
	advance(store.RecordBackgroundRunMaterializeStarted)
	materialProof := sha256.Sum256([]byte("acceptance materialization"))
	export, err = store.RecordArtifactMaterializationReady(context.Background(), RecordArtifactMaterializationReadyParams{
		BackgroundRunExportClaim: exportClaim(), MaterializationID: seal.MaterializationID, ArtifactID: seal.ArtifactID,
		ResultID: seal.ResultID, ResultCommit: export.ResultCommit, TreeOID: export.TreeOID, ProofSHA256: materialProof,
	})
	if err != nil {
		t.Fatal(err)
	}
	exportNow = exportNow.Add(time.Second)
	evidence := journalEvidence()
	commit := CommitBackgroundRunRetainedResultParams{BackgroundRunExportClaim: exportClaim(),
		MaterializationID: seal.MaterializationID, ArtifactID: seal.ArtifactID, ResultID: seal.ResultID,
		ResultEventID: seal.ResultEventID, TaskEventID: seal.TaskEventID, EvidencePayload: evidence,
		EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(), SealedAt: exportNow,
	}
	mismatch := commit
	mismatch.ArtifactID = task.RetainedArtifactID(testID("art_", 5199))
	if _, err := store.CommitBackgroundRunRetainedResult(context.Background(), mismatch); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("retained tuple mismatch = %v", err)
	}
	var uncommitted int
	if err := store.db.QueryRow(`SELECT count(*) FROM results WHERE id=?`, seal.ResultID).Scan(&uncommitted); err != nil || uncommitted != 0 {
		t.Fatalf("mismatched commit leaked result: count=%d error=%v", uncommitted, err)
	}
	committed, err := store.CommitBackgroundRunRetainedResult(context.Background(), commit)
	if err != nil || committed.Run.State != BackgroundRunResultReady || committed.Run.EffectPhase != BackgroundRunEffectArtifactCommitted ||
		committed.Result.SourceKind != ResultSourceRetainedArtifact || committed.Artifact.ManifestSHA256 != sha256.Sum256(artifactManifest) ||
		committed.Artifact.ChangesSHA256 != sha256.Sum256(resultManifestJSON) || committed.Artifact.ManifestSHA256 == committed.Artifact.ChangesSHA256 ||
		committed.Artifact.CASLocator != "sha256:"+hex.EncodeToString(committed.Artifact.ManifestSHA256[:]) {
		t.Fatalf("retained result commit = %+v, error=%v", committed, err)
	}
	commitReplay, err := store.CommitBackgroundRunRetainedResult(context.Background(), commit)
	if err != nil || !commitReplay.Replayed {
		t.Fatalf("retained result replay = %+v, error=%v", commitReplay, err)
	}
	readResult, err := store.GetResult(context.Background(), committed.Result.ID)
	if err != nil || readResult.ID != committed.Result.ID || readResult.SourceKind != ResultSourceRetainedArtifact {
		t.Fatalf("read result = %+v, error=%v", readResult, err)
	}
	readManifest, err := store.GetResultManifest(context.Background(), committed.Result.ID)
	if err != nil || len(readManifest) != len(committed.Manifest) {
		t.Fatalf("read result manifest = %+v, error=%v", readManifest, err)
	}
	readArtifact, err := store.GetRetainedArtifact(context.Background(), committed.Artifact.ID)
	if err != nil || readArtifact.ID != committed.Artifact.ID {
		t.Fatalf("read retained artifact = %+v, error=%v", readArtifact, err)
	}
	source, err := store.FindResultAwaitingVerification(context.Background(), run.WorkspaceID)
	if err != nil || source.Result.ID != committed.Result.ID || source.Task.ID != committed.Task.ID || source.Attempt.ID != committed.Attempt.ID {
		t.Fatalf("retained verification source = %+v, error=%v", source, err)
	}
	authorized, err := store.HasRetainedResultAuthority(context.Background(), committed.Result.ID)
	if err != nil || !authorized {
		t.Fatalf("retained result authority = %t, error=%v", authorized, err)
	}
	ownedResult, ownedTask, ownedAttempt, err := store.GetResultOwners(context.Background(), committed.Result.ID)
	if err != nil || ownedResult.ID != committed.Result.ID || ownedTask.ID != committed.Task.ID || ownedAttempt.ID != committed.Attempt.ID {
		t.Fatalf("retained result owners = %+v %+v %+v, error=%v", ownedResult, ownedTask, ownedAttempt, err)
	}
	verification := prepareJournalVerification(t, store, committed.SealedResult, 5200)
	running, err := store.AdvanceVerification(context.Background(), AdvanceVerificationParams{
		VerificationID: verification.Verification.ID, ExpectedRevision: verification.Verification.Revision,
		ExpectedTaskRevision: committed.Task.Revision, ExpectedAttemptRevision: committed.Attempt.Revision,
		EventID: testEventID(5201), StartedAt: verification.Verification.UpdatedAt.Add(time.Millisecond),
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	emptyHash := sha256.Sum256(nil)
	exit := 0
	verified, err := store.CompleteVerification(context.Background(), CompleteVerificationParams{
		VerificationID: running.Verification.ID, ExpectedRevision: running.Verification.Revision,
		ExpectedTaskRevision: committed.Task.Revision, ExpectedAttemptRevision: committed.Attempt.Revision,
		EventID: testEventID(5202), State: VerificationSucceeded, Outcome: "passed", ExitCode: &exit,
		Stdout: VerificationOutput{SHA256: emptyHash}, Stderr: VerificationOutput{SHA256: emptyHash},
		EndedAt: running.Verification.UpdatedAt.Add(time.Millisecond), EvidencePayload: evidence,
		EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	publication := prepareJournalPublication(t, store, committed.SealedResult, verified, 5210)
	publicationWork, err := store.FindPublicationWork(context.Background(), run.WorkspaceID)
	if err != nil || publicationWork.Publication.ID != publication.Publication.ID || publicationWork.Result.ID != committed.Result.ID {
		t.Fatalf("retained publication work = %+v, error=%v", publicationWork, err)
	}
	if _, err := store.db.Exec(`UPDATE retained_artifacts SET bundle_size=bundle_size+1 WHERE id=?`, seal.ArtifactID); err == nil {
		t.Fatal("retained artifact accepted raw mutation")
	}
	if _, err := store.db.Exec(`UPDATE background_run_exports SET recovery_reason='changed',revision=revision+1,updated_at=updated_at+1 WHERE id=?`, seal.ExportID); err == nil {
		t.Fatal("completed export accepted raw mutation")
	}
	digests, err := store.ReferencedArtifactManifestSHA256(context.Background())
	if err != nil || len(digests) != 1 || digests[0] != committed.Artifact.ManifestSHA256 {
		t.Fatalf("referenced manifests = %x, error=%v", digests, err)
	}
	if _, err := store.db.Exec(`UPDATE background_runs SET effect_phase='route_removed',route_removed_at=updated_at+1,
route_removed_evidence='raw cleanup',revision=revision+1,updated_at=updated_at+1 WHERE task_id=?`, run.TaskID); err == nil {
		t.Fatal("result-bearing cleanup bypassed committed tuple gate")
	}
	cleanupWork, err := store.ClaimNextBackgroundRunWork(context.Background(), ClaimNextBackgroundRunParams{
		WorkspaceID: run.WorkspaceID, ClaimOwner: "result-cleaner", Now: exportNow.Add(time.Second), LeaseDuration: time.Minute,
		Profile: BackgroundRunSourceProfile, ImageIdentity: run.ImageIdentity,
	})
	cleanupRun := cleanupWork.Run
	if err != nil || cleanupRun.EffectPhase != BackgroundRunEffectArtifactCommitted {
		t.Fatal(err)
	}
	cleanupClaim := backgroundRunClaim(cleanupRun, exportNow.Add(2*time.Second))
	cleanupRun, err = store.RequestBackgroundRunResultCleanup(context.Background(), RecordBackgroundRunEvidenceParams{
		BackgroundRunClaim: cleanupClaim, Evidence: "retained tuple accepted",
	})
	if err != nil {
		t.Fatal(err)
	}
	advanceBackgroundClaim(&cleanupClaim, cleanupRun)
	for _, step := range []func(context.Context, RecordBackgroundRunEvidenceParams) (BackgroundRun, error){
		store.RecordBackgroundRunRouteRemoved, store.RecordBackgroundRunContainerRemoved,
		store.RecordBackgroundRunVolumeRemoved, store.RecordBackgroundRunCloneRemoved,
	} {
		cleanupClaim.Now = cleanupClaim.Now.Add(time.Second)
		cleanupRun, err = step(context.Background(), RecordBackgroundRunEvidenceParams{BackgroundRunClaim: cleanupClaim, Evidence: "resource absent"})
		if err != nil {
			t.Fatalf("retained cleanup from %s: %v", cleanupClaim.ExpectedPhase, err)
		}
		advanceBackgroundClaim(&cleanupClaim, cleanupRun)
	}
	cleanupClaim.Now = cleanupClaim.Now.Add(time.Second)
	cleanupRun, err = store.CompleteBackgroundRunResultCleanup(context.Background(), CompleteBackgroundRunResultCleanupParams{
		BackgroundRunClaim: cleanupClaim, CleanupProof: "all resources absent",
	})
	if err != nil || cleanupRun.EffectPhase != BackgroundRunEffectCleanupComplete {
		t.Fatalf("retained cleanup completion = %+v, error=%v", cleanupRun, err)
	}
	projection, err := store.GetBackgroundRunResult(context.Background(), run.WorkspaceID, run.TaskID, admission.Claim.Actor)
	if err != nil || projection.Result.ID != committed.Result.ID || projection.Artifact.ID != committed.Artifact.ID || projection.Materialization.ID != committed.Materialization.ID {
		t.Fatalf("background result projection = %+v, error=%v", projection, err)
	}
}

func TestArtifactManifestSafetyRejectsAuthorityKeysNotBase64Values(t *testing.T) {
	if !safeArtifactManifest(json.RawMessage(`{"changes":[{"path_base64":"/A=="}]}`)) {
		t.Fatal("valid standard-Base64 value was treated as a host path")
	}
	for _, value := range []json.RawMessage{
		json.RawMessage(`{"host_path":"/srv/private"}`),
		json.RawMessage(`{"remote_url":"https://example.invalid/repository"}`),
		json.RawMessage(`{"prompt":"secret"}`),
		json.RawMessage(`{"environment":{"TOKEN":"secret"}}`),
		json.RawMessage(`{"credential":"secret"}`),
	} {
		if safeArtifactManifest(value) {
			t.Fatalf("forbidden artifact manifest accepted: %s", value)
		}
	}
}
