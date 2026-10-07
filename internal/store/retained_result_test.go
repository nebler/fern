package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nebler/fern/internal/domain"
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
	run, _ := advanceBackgroundRunToPrompt(t, store, admission.ImageIdentity, now)
	sealClaim := domain.IdempotencyClaim{
		Scope: domain.IdempotencyScope{WorkspaceID: run.WorkspaceID, CommandKind: SealBackgroundRunCommand},
		Key:   "retained-seal", RequestHash: sha256.Sum256([]byte("retained-seal")), Actor: admission.Claim.Actor,
	}
	seal := SealBackgroundRunParams{
		WorkspaceID: run.WorkspaceID, RunID: run.RunID, ExpectedRunRevision: run.Revision,
		ResultID: testResultID(5106), Claim: sealClaim,
		PolicyVersion: "background-retained.v1", APIContractVersion: "v1", AcceptedAt: now.Add(20 * time.Second),
	}
	sealed, err := store.SealBackgroundRun(context.Background(), seal)
	if err != nil || sealed.Run.State != domain.Canceling || sealed.Run.EffectPhase != domain.Sealing ||
		sealed.Run.Seal == nil || sealed.Run.Seal.ResultID != seal.ResultID || sealed.Run.Seal.CommitEpochSeconds() != seal.AcceptedAt.Unix() {
		t.Fatalf("seal admission = %+v, error=%v", sealed, err)
	}
	replay, err := store.SealBackgroundRun(context.Background(), seal)
	if err != nil || !replay.Replayed || replay.Run.Seal.ReceiptID != sealed.Receipt.ID {
		t.Fatalf("seal replay = %+v, error=%v", replay, err)
	}
	ownerMismatch := seal
	ownerMismatch.Claim.Actor.ID = "other-owner"
	ownerMismatch.Claim.Actor.CredentialID = "other-owner"
	if _, err := store.SealBackgroundRun(context.Background(), ownerMismatch); !errors.Is(err, ErrNotFound) {
		t.Fatalf("seal owner mismatch = %v", err)
	}
	stop := StopBackgroundRunParams{WorkspaceID: run.WorkspaceID, RunID: run.RunID,
		Claim: domain.IdempotencyClaim{
			Scope: domain.IdempotencyScope{WorkspaceID: run.WorkspaceID, CommandKind: StopBackgroundRunCommand}, Key: "stop-after-seal",
			RequestHash: sha256.Sum256([]byte("stop-after-seal")), Actor: admission.Claim.Actor,
		}, APIContractVersion: "v1", StoppedAt: seal.AcceptedAt.Add(time.Second)}
	if _, err := store.StopBackgroundRun(context.Background(), stop); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stop after winning seal = %v", err)
	}

	sealedWork, err := startNextBackgroundRunWork(context.Background(), store, seal.AcceptedAt.Add(2*time.Second))
	sealedRun := sealedWork.Run
	if err != nil || sealedRun.State != domain.Canceling || sealedRun.EffectPhase != domain.Sealing {
		t.Fatal(err)
	}
	writerAt := seal.AcceptedAt.Add(3 * time.Second)
	stoppedAt := writerAt
	selectionTooEarly := SelectBackgroundRunSnapshotParams{BackgroundRunRef: backgroundRunRef(sealedRun, writerAt)}
	if _, err := store.SelectBackgroundRunSnapshot(context.Background(), selectionTooEarly); err == nil {
		t.Fatal("selection before the writer fence was accepted")
	}
	wrongRuntime := RecordBackgroundRunWriterFenceParams{BackgroundRunRef: backgroundRunRef(sealedRun, writerAt), WriterFence: WriterFence{
		Kind: WriterFenceRuntimeStopped, ContainerID: "other-container", ContainerStartedAt: run.ObservedContainerStartedAt,
		RuntimeToken: "runtime-token", StoppedAt: &stoppedAt}}
	if _, err := store.RecordBackgroundRunWriterFence(context.Background(), wrongRuntime); err == nil {
		t.Fatal("writer fence for a different runtime was accepted")
	}
	writerParams := RecordBackgroundRunWriterFenceParams{BackgroundRunRef: backgroundRunRef(sealedRun, writerAt), WriterFence: WriterFence{
		Kind: WriterFenceRuntimeStopped, ContainerID: run.ObservedContainerID, ContainerStartedAt: run.ObservedContainerStartedAt,
		RuntimeToken: "runtime-token", StoppedAt: &stoppedAt}}
	writerInactive, err := store.RecordBackgroundRunWriterFence(context.Background(), writerParams)
	if err != nil || writerInactive.EffectPhase != domain.Sealing || writerInactive.Revision != sealedRun.Revision+1 ||
		writerInactive.WriterFence == nil || writerInactive.WriterFence.RuntimeToken != "runtime-token" {
		t.Fatalf("writer fence = %+v, error=%v", writerInactive, err)
	}
	if _, err := store.db.Exec(`UPDATE runs SET writer_fence_token='other',revision=revision+1,updated_at=updated_at+1 WHERE id=?`, run.RunID); err == nil {
		t.Fatal("writer fence accepted raw mutation")
	}

	exportNow := writerAt.Add(time.Second)
	// A failed pass records why on the run; the run stays sealing for the next
	// pass, and a stale revision cannot record over it.
	recovery, err := store.MarkBackgroundRunExportRecoveryRequired(context.Background(), backgroundRunRef(writerInactive, exportNow), "injected export interruption")
	if err != nil || recovery.EffectPhase != domain.Sealing || recovery.LastError != "injected export interruption" {
		t.Fatalf("export recovery = %+v, error=%v", recovery, err)
	}
	if _, err := store.MarkBackgroundRunExportRecoveryRequired(context.Background(), backgroundRunRef(writerInactive, exportNow), "different"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stale export recovery = %v", err)
	}
	reselected, err := startNextBackgroundRun(context.Background(), store, exportNow.Add(time.Second))
	if err != nil || reselected.EffectPhase != domain.Sealing {
		t.Fatalf("reselect failed export run = %+v, error=%v", reselected, err)
	}
	exportNow = exportNow.Add(2 * time.Second)
	resultCommit := domain.GitOID("1111111111111111111111111111111111111111")
	// /A== is standard Base64 for a non-UTF8 Git path prefix. It is artifact
	// data, not host-path authority, and must survive the manifest guard.
	artifactManifest := json.RawMessage(`{"version":3,"changes":[{"path_base64":"/A=="}]}`)
	selection := SelectBackgroundRunSnapshotParams{
		BackgroundRunRef: backgroundRunRef(recovery, exportNow), ResultCommit: resultCommit,
		TreeOID: domain.GitOID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), ChangeCount: 1, ChangesSHA256: sha256.Sum256([]byte("changes")),
		ArtifactManifest:       json.RawMessage(`{"host_path":"/private/work"}`),
		ArtifactManifestSHA256: sha256.Sum256([]byte(`{"host_path":"/private/work"}`)),
		BundleSHA256:           sha256.Sum256([]byte("bundle")), BundleBytes: 6, CollectedAt: exportNow,
	}
	if _, err := store.SelectBackgroundRunSnapshot(context.Background(), selection); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unsafe artifact manifest = %v", err)
	}
	selection.ArtifactManifest, selection.ArtifactManifestSHA256 = artifactManifest, sha256.Sum256(artifactManifest)
	selected, err := store.SelectBackgroundRunSnapshot(context.Background(), selection)
	if err != nil || selected.State != ResultSelected || selected.Outcome != domain.ResultChanged || selected.BundleBytes != 6 ||
		selected.CASLocator() != "sha256:"+hex.EncodeToString(selection.ArtifactManifestSHA256[:]) {
		t.Fatalf("selected result = %+v, error=%v", selected, err)
	}
	if replayed, err := store.SelectBackgroundRunSnapshot(context.Background(), selection); err != nil || replayed.ManifestSHA256 != selected.ManifestSHA256 {
		t.Fatalf("selection replay = %+v, error=%v", replayed, err)
	}
	different := selection
	different.TreeOID = domain.GitOID("cccccccccccccccccccccccccccccccccccccccc")
	if _, err := store.SelectBackgroundRunSnapshot(context.Background(), different); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("different selection = %v", err)
	}
	if _, err := store.db.Exec(`UPDATE results SET tree_oid=? WHERE id=?`, different.TreeOID, seal.ResultID); err == nil {
		t.Fatal("selected result accepted raw mutation")
	}
	if _, err := store.db.Exec(`UPDATE runs SET state='result_ready',effect_phase='cleaning',
revision=revision+1,updated_at=updated_at+1 WHERE id=?`, run.RunID); err == nil {
		t.Fatal("sealed run released its resources without a committed result")
	}
	exportNow = exportNow.Add(time.Second)
	materialProof := sha256.Sum256([]byte("acceptance materialization"))
	stale := CommitBackgroundRunRetainedResultParams{BackgroundRunRef: backgroundRunRef(writerInactive, exportNow), MaterializationProof: materialProof}
	if _, err := store.CommitBackgroundRunRetainedResult(context.Background(), stale); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stale retained commit = %v", err)
	}
	commit := CommitBackgroundRunRetainedResultParams{BackgroundRunRef: backgroundRunRef(recovery, exportNow), MaterializationProof: materialProof}
	committed, err := store.CommitBackgroundRunRetainedResult(context.Background(), commit)
	if err != nil || committed.Run.State != domain.ResultReady || committed.Run.EffectPhase != domain.Cleaning ||
		committed.Run.LastError != "" || committed.Result.State != ResultSealed || committed.Result.MaterializationSHA256 != materialProof ||
		committed.Result.SealedAt == nil || !committed.Result.SealedAt.Equal(exportNow) || committed.Result.ManifestSHA256 != sha256.Sum256(artifactManifest) {
		t.Fatalf("retained result commit = %+v, error=%v", committed, err)
	}
	if _, err := store.CommitBackgroundRunRetainedResult(context.Background(), commit); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second retained commit = %v", err)
	}
	if _, err := store.db.Exec(`UPDATE results SET bundle_size=bundle_size+1 WHERE id=?`, seal.ResultID); err == nil {
		t.Fatal("sealed result accepted raw mutation")
	}
	if _, err := store.db.Exec(`DELETE FROM results WHERE id=?`, seal.ResultID); err == nil {
		t.Fatal("sealed result accepted deletion")
	}
	digests, err := store.ReferencedArtifactManifestSHA256(context.Background())
	if err != nil || len(digests) != 1 || digests[0] != committed.Result.ManifestSHA256 {
		t.Fatalf("referenced manifests = %x, error=%v", digests, err)
	}
	cleanupWork, err := startNextBackgroundRunWork(context.Background(), store, exportNow.Add(time.Second))
	cleanupRun := cleanupWork.Run
	if err != nil || cleanupRun.State != domain.ResultReady || cleanupRun.EffectPhase != domain.Cleaning {
		t.Fatal(err)
	}
	cleanupRef := backgroundRunRef(cleanupRun, exportNow.Add(2*time.Second))
	failed, err := store.MarkBackgroundRunCleanupRequired(context.Background(), MarkBackgroundRunCleanupRequiredParams{
		BackgroundRunRef: cleanupRef, Error: "container removal unavailable"})
	if err != nil || failed.State != domain.ResultReady || failed.EffectPhase != domain.Cleaning {
		t.Fatalf("retained cleanup failure = %+v, error=%v", failed, err)
	}
	advanceBackgroundRef(&cleanupRef, failed)
	cleanupRef.Now = cleanupRef.Now.Add(time.Second)
	cleanupRun, err = store.CompleteBackgroundRunResultCleanup(context.Background(), CompleteBackgroundRunResultCleanupParams{
		BackgroundRunRef: cleanupRef, CleanupProof: "all resources absent",
	})
	if err != nil || cleanupRun.EffectPhase != domain.CleanupComplete {
		t.Fatalf("retained cleanup completion = %+v, error=%v", cleanupRun, err)
	}
	projection, err := store.GetBackgroundRunResult(context.Background(), run.WorkspaceID, run.RunID, admission.Claim.Actor)
	if err != nil || projection.Result.ID != seal.ResultID || projection.Run.RunID != run.RunID {
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
