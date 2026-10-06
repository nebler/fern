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
	if err != nil || sealed.Run.State != BackgroundRunCanceling || sealed.Run.EffectPhase != BackgroundRunEffectSealing ||
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

	sealedWork, err := startNextBackgroundRunWork(context.Background(), store, seal.AcceptedAt.Add(2*time.Second))
	sealedRun := sealedWork.Run
	if err != nil || sealedRun.State != BackgroundRunCanceling || sealedRun.EffectPhase != BackgroundRunEffectSealing {
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
	writerParams := RecordBackgroundRunWriterFenceParams{BackgroundRunRef: backgroundRunRef(sealedRun, writerAt),
		SealRequestID: seal.SealRequestID, ExportID: seal.ExportID, Kind: WriterFenceRuntimeStopped,
		ContainerID: run.ObservedContainerID, ContainerStartedAt: run.ObservedContainerStartedAt, RuntimeEpoch: run.RuntimeEpoch,
		RuntimeToken: "runtime-token", StoppedAt: &stoppedAt, ProofSHA256: sha256.Sum256(encodedWriterProof)}
	writerInactive, err := store.RecordBackgroundRunWriterFence(context.Background(), writerParams)
	if err != nil || writerInactive.EffectPhase != BackgroundRunEffectSealing || writerInactive.Revision != sealedRun.Revision+1 {
		t.Fatalf("writer fence = %+v, error=%v", writerInactive, err)
	}
	if _, err := store.RecordBackgroundRunWriterFence(context.Background(), writerParams); err != nil {
		t.Fatalf("writer fence replay: %v", err)
	}
	writerRun, err := startNextBackgroundRun(context.Background(), store, writerAt.Add(time.Millisecond))
	if err != nil || writerRun.EffectPhase != BackgroundRunEffectSealing {
		t.Fatalf("writer-inactive production claim = %+v, error=%v", writerRun, err)
	}

	export, err := store.StartBackgroundRunExport(context.Background(), BackgroundRunExportRef{
		ExportID: seal.ExportID, TaskID: run.TaskID, AttemptID: run.AttemptID, Generation: run.Generation,
		ExpectedRevision: 1, ExpectedPhase: BackgroundRunExportPhasePrepared,
		Now: writerAt.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	// A stale export revision is rejected; restarting a running export at its
	// current revision is allowed so a restarted coordinator can replay.
	if _, err := store.StartBackgroundRunExport(context.Background(), BackgroundRunExportRef{
		ExportID: export.ID, TaskID: run.TaskID, AttemptID: run.AttemptID, Generation: run.Generation,
		ExpectedRevision: export.Revision - 1, ExpectedPhase: export.Phase,
		Now: writerAt.Add(1500 * time.Millisecond),
	}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stale export start = %v", err)
	}
	restartedRevision := export.Revision
	export, err = store.StartBackgroundRunExport(context.Background(), BackgroundRunExportRef{
		ExportID: export.ID, TaskID: run.TaskID, AttemptID: run.AttemptID, Generation: run.Generation,
		ExpectedRevision: export.Revision, ExpectedPhase: export.Phase,
		Now: writerAt.Add(1500 * time.Millisecond),
	})
	if err != nil || export.State != BackgroundRunExportRunning || export.Revision != restartedRevision+1 {
		t.Fatalf("restarted export = %+v, error=%v", export, err)
	}
	failureAt := writerAt.Add(2 * time.Second)
	failureRef := BackgroundRunExportRef{ExportID: export.ID, TaskID: export.TaskID, AttemptID: export.AttemptID,
		Generation: export.Generation, ExpectedRevision: export.Revision, ExpectedPhase: export.Phase,
		Now: failureAt}
	recovery, err := store.MarkBackgroundRunExportRecoveryRequired(context.Background(), failureRef, "injected export interruption")
	if err != nil || recovery.State != BackgroundRunExportRecoveryRequired {
		t.Fatalf("export recovery = %+v, error=%v", recovery, err)
	}
	if replayed, err := store.MarkBackgroundRunExportRecoveryRequired(context.Background(), failureRef, "injected export interruption"); err != nil || replayed.Revision != recovery.Revision {
		t.Fatalf("export recovery replay = %+v, error=%v", replayed, err)
	}
	if _, err := store.RecordBackgroundRunSnapshotStarted(context.Background(), BackgroundRunExportRef{ExportID: recovery.ID,
		TaskID: recovery.TaskID, AttemptID: recovery.AttemptID, Generation: recovery.Generation, ExpectedRevision: recovery.Revision,
		ExpectedPhase: recovery.Phase, Now: failureAt}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("recovery-required export advanced before restart: %v", err)
	}
	reselected, err := startNextBackgroundRun(context.Background(), store, failureAt.Add(time.Second))
	if err != nil || reselected.EffectPhase != BackgroundRunEffectSealing {
		t.Fatalf("reselect failed export run = %+v, error=%v", reselected, err)
	}
	export, err = store.StartBackgroundRunExport(context.Background(), BackgroundRunExportRef{
		ExportID: export.ID, TaskID: export.TaskID, AttemptID: export.AttemptID, Generation: export.Generation,
		ExpectedRevision: recovery.Revision, ExpectedPhase: recovery.Phase,
		Now: failureAt.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	exportNow := failureAt.Add(3 * time.Second)
	exportRef := func() BackgroundRunExportRef {
		return BackgroundRunExportRef{ExportID: export.ID, TaskID: export.TaskID, AttemptID: export.AttemptID,
			Generation: export.Generation, ExpectedRevision: export.Revision, ExpectedPhase: export.Phase,
			Now: exportNow}
	}
	advance := func(call func(context.Context, BackgroundRunExportRef) (BackgroundRunExport, error)) {
		var stepErr error
		export, stepErr = call(context.Background(), exportRef())
		if stepErr != nil {
			t.Fatalf("advance export from %s: %v", exportRef().ExpectedPhase, stepErr)
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
		BackgroundRunExportRef: exportRef(), ResultCommit: resultCommit, TreeOID: task.GitOID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Outcome: task.ResultChanged, ResultManifest: resultManifest, ChangesSHA256: sha256.Sum256(resultManifestJSON),
		ArtifactManifest:       json.RawMessage(`{"host_path":"/private/work"}`),
		ArtifactManifestSHA256: sha256.Sum256([]byte(`{"host_path":"/private/work"}`)),
		OpenCodeSessionID:      run.OpenCodeSessionID, OpenCodeMessageID: run.OpenCodeMessageID, CollectedAt: exportNow,
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unsafe artifact manifest = %v", err)
	}
	export, err = store.SelectBackgroundRunSnapshot(context.Background(), SelectBackgroundRunSnapshotParams{
		BackgroundRunExportRef: exportRef(), ResultCommit: resultCommit, TreeOID: task.GitOID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Outcome: task.ResultChanged, ResultManifest: resultManifest, ChangesSHA256: sha256.Sum256(resultManifestJSON),
		ArtifactManifest: artifactManifest, ArtifactManifestSHA256: sha256.Sum256(artifactManifest),
		OpenCodeSessionID: run.OpenCodeSessionID, OpenCodeMessageID: run.OpenCodeMessageID, CollectedAt: exportNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	exportNow = exportNow.Add(time.Second)
	advance(store.RecordBackgroundRunBundleWriteStarted)
	export, err = store.RecordBackgroundRunBundleVerified(context.Background(), RecordBackgroundRunBundleVerifiedParams{
		BackgroundRunExportRef: exportRef(), BundleSHA256: sha256.Sum256([]byte("bundle")), BundleBytes: 6,
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
		BackgroundRunExportRef: exportRef(), MaterializationID: seal.MaterializationID, ArtifactID: seal.ArtifactID,
		ResultID: seal.ResultID, ResultCommit: export.ResultCommit, TreeOID: export.TreeOID, ProofSHA256: materialProof,
	})
	if err != nil {
		t.Fatal(err)
	}
	exportNow = exportNow.Add(time.Second)
	evidence := json.RawMessage(`{"authorityRead":true,"objects":1}`)
	commit := CommitBackgroundRunRetainedResultParams{BackgroundRunExportRef: exportRef(),
		MaterializationID: seal.MaterializationID, ArtifactID: seal.ArtifactID, ResultID: seal.ResultID,
		ResultEventID: seal.ResultEventID, TaskEventID: seal.TaskEventID, EvidencePayload: evidence,
		EvidenceSHA256: sha256.Sum256(evidence), Actor: testSystemActor(), SealedAt: exportNow,
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
	if _, err := store.db.Exec(`UPDATE background_runs SET state='result_ready',effect_phase='cleaning',
revision=revision+1,updated_at=updated_at+1 WHERE task_id=?`, run.TaskID); err == nil {
		t.Fatal("sealed run released its resources without a committed result")
	}
	committed, err := store.CommitBackgroundRunRetainedResult(context.Background(), commit)
	if err != nil || committed.Run.State != BackgroundRunResultReady || committed.Run.EffectPhase != BackgroundRunEffectCleaning ||
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
	readArtifact, err := store.GetRetainedArtifact(context.Background(), committed.Artifact.ID)
	if err != nil || readArtifact.ID != committed.Artifact.ID {
		t.Fatalf("read retained artifact = %+v, error=%v", readArtifact, err)
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
	cleanupWork, err := startNextBackgroundRunWork(context.Background(), store, exportNow.Add(time.Second))
	cleanupRun := cleanupWork.Run
	if err != nil || cleanupRun.State != BackgroundRunResultReady || cleanupRun.EffectPhase != BackgroundRunEffectCleaning {
		t.Fatal(err)
	}
	cleanupRef := backgroundRunRef(cleanupRun, exportNow.Add(2*time.Second))
	failed, err := store.MarkBackgroundRunCleanupRequired(context.Background(), MarkBackgroundRunCleanupRequiredParams{
		BackgroundRunRef: cleanupRef, Error: "container removal unavailable"})
	if err != nil || failed.State != BackgroundRunResultReady || failed.EffectPhase != BackgroundRunEffectCleaning {
		t.Fatalf("retained cleanup failure = %+v, error=%v", failed, err)
	}
	advanceBackgroundRef(&cleanupRef, failed)
	cleanupRef.Now = cleanupRef.Now.Add(time.Second)
	cleanupRun, err = store.CompleteBackgroundRunResultCleanup(context.Background(), CompleteBackgroundRunResultCleanupParams{
		BackgroundRunRef: cleanupRef, CleanupProof: "all resources absent",
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
