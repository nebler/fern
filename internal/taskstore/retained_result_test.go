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
		Claim:              sealClaim,
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
		Claim: task.IdempotencyClaim{
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

	export, err := store.GetBackgroundRunExport(context.Background(), seal.ExportID)
	if err != nil || export.Phase != BackgroundRunExportPhasePrepared {
		t.Fatalf("prepared export = %+v, error=%v", export, err)
	}
	exportNow := writerAt.Add(time.Second)
	exportRef := func() BackgroundRunExportRef {
		return BackgroundRunExportRef{ExportID: export.ID, TaskID: export.TaskID, AttemptID: export.AttemptID,
			Generation: export.Generation, ExpectedRevision: export.Revision, ExpectedPhase: export.Phase, Now: exportNow}
	}
	// A failed pass records why; the export keeps its phase for the next pass,
	// and a stale revision cannot record over it.
	recovery, err := store.MarkBackgroundRunExportRecoveryRequired(context.Background(), exportRef(), "injected export interruption")
	if err != nil || recovery.Phase != BackgroundRunExportPhasePrepared || recovery.RecoveryReason != "injected export interruption" {
		t.Fatalf("export recovery = %+v, error=%v", recovery, err)
	}
	if replayed, err := store.MarkBackgroundRunExportRecoveryRequired(context.Background(), exportRef(), "injected export interruption"); err != nil || replayed.Revision != recovery.Revision {
		t.Fatalf("export recovery replay = %+v, error=%v", replayed, err)
	}
	if _, err := store.MarkBackgroundRunExportRecoveryRequired(context.Background(), exportRef(), "different interruption"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stale export recovery = %v", err)
	}
	reselected, err := startNextBackgroundRun(context.Background(), store, exportNow.Add(time.Second))
	if err != nil || reselected.EffectPhase != BackgroundRunEffectSealing {
		t.Fatalf("reselect failed export run = %+v, error=%v", reselected, err)
	}
	export = recovery
	exportNow = exportNow.Add(2 * time.Second)
	mode, blob, size := "100644", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", int64(12)
	resultManifest := []ManifestEntry{{PathBase64: "Y2hhbmdlLnR4dA==", ChangeKind: "added", NewMode: &mode, NewBlobOID: &blob, NewSize: &size}}
	resultManifestJSON, _ := json.Marshal(resultManifest)
	resultCommit := task.GitOID("1111111111111111111111111111111111111111")
	// /A== is standard Base64 for a non-UTF8 Git path prefix. It is artifact
	// data, not host-path authority, and must survive both Go and SQL guards.
	artifactManifest := json.RawMessage(`{"version":1,"changes":[{"path_base64":"/A=="}]}`)
	selection := SelectBackgroundRunSnapshotParams{
		BackgroundRunExportRef: exportRef(), ResultCommit: resultCommit, TreeOID: task.GitOID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Outcome: task.ResultChanged, ResultManifest: resultManifest, ChangesSHA256: sha256.Sum256(resultManifestJSON),
		ArtifactManifest:       json.RawMessage(`{"host_path":"/private/work"}`),
		ArtifactManifestSHA256: sha256.Sum256([]byte(`{"host_path":"/private/work"}`)),
		BundleSHA256:           sha256.Sum256([]byte("bundle")), BundleBytes: 6,
		OpenCodeSessionID: run.OpenCodeSessionID, OpenCodeMessageID: run.OpenCodeMessageID, CollectedAt: exportNow,
	}
	if _, err := store.SelectBackgroundRunSnapshot(context.Background(), selection); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unsafe artifact manifest = %v", err)
	}
	selection.ArtifactManifest, selection.ArtifactManifestSHA256 = artifactManifest, sha256.Sum256(artifactManifest)
	export, err = store.SelectBackgroundRunSnapshot(context.Background(), selection)
	if err != nil || export.Phase != BackgroundRunExportPhaseSelected || export.RecoveryReason != "" || export.BundleBytes != 6 {
		t.Fatalf("selected export = %+v, error=%v", export, err)
	}
	if replayed, err := store.SelectBackgroundRunSnapshot(context.Background(), selection); err != nil || replayed.Revision != export.Revision {
		t.Fatalf("selection replay = %+v, error=%v", replayed, err)
	}
	different := selection
	different.TreeOID = task.GitOID("cccccccccccccccccccccccccccccccccccccccc")
	if _, err := store.SelectBackgroundRunSnapshot(context.Background(), different); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("different selection = %v", err)
	}
	exportNow = exportNow.Add(time.Second)
	materialProof := sha256.Sum256([]byte("acceptance materialization"))
	commit := CommitBackgroundRunRetainedResultParams{BackgroundRunExportRef: exportRef(),
		MaterializationID: seal.MaterializationID, MaterializationProof: materialProof, ArtifactID: seal.ArtifactID, ResultID: seal.ResultID,
		SealedAt: exportNow,
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
		committed.Artifact.CASLocator != "sha256:"+hex.EncodeToString(committed.Artifact.ManifestSHA256[:]) ||
		committed.Export.Phase != BackgroundRunExportPhaseCommitted || committed.Materialization.State != ArtifactMaterializationReady ||
		committed.Materialization.ProofSHA256 != materialProof || committed.Materialization.ResultCommit != resultCommit {
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
