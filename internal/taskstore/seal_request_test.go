package taskstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

func TestUserSealAdmissionReplayAndCompletionDoesNotClaimSuccess(t *testing.T) {
	s, preview := userSealFixture(t, 1300)
	p := testSealRequest(preview, 1300, "seal-once")
	admission, err := s.RequestSeal(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if admission.Replayed || admission.Request.State != SealRequestPending || admission.Request.Authorizer != p.Claim.Actor ||
		admission.Receipt.CommandKind != SealTaskCommand || admission.Request.RequestHash != p.Claim.RequestHash {
		t.Fatalf("seal admission: %+v", admission)
	}

	replayParams := p
	replayParams.SealRequestID = task.SealRequestID(testID("slr_", 9991))
	replayParams.ReceiptID = testReceiptID(9991)
	replayParams.ResultID = testResultID(9991)
	replayParams.ResultEventID = testEventID(19982)
	replayParams.TaskEventID = testEventID(19983)
	replayParams.AcceptedAt = p.AcceptedAt.Add(time.Second)
	replayParams.Claim.Actor.RequestID = "retry-request"
	replay, err := s.RequestSeal(context.Background(), replayParams)
	if err != nil || !replay.Replayed || replay.Request.ID != p.SealRequestID || replay.Receipt.ID != p.ReceiptID {
		t.Fatalf("seal replay: %+v, %v", replay, err)
	}

	claimAt := p.AcceptedAt.Add(time.Millisecond)
	claimed, err := s.ClaimSealRequest(context.Background(), ClaimSealRequestParams{
		WorkspaceID: preview.Workspace.ID, ClaimOwner: "seal-worker", Now: claimAt, LeaseExpiresAt: claimAt.Add(time.Minute),
	})
	if err != nil || claimed.Request.State != SealRequestClaimed || claimed.Request.ClaimRevision != 1 {
		t.Fatalf("claim: %+v, %v", claimed, err)
	}
	sealParams := authorizedSealParams(claimed, p.AcceptedAt.Add(2*time.Millisecond))
	sealed, err := s.SealAuthorizedResult(context.Background(), sealParams)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.Result.CompletionAuthority != SealAuthorityUser || sealed.Result.SealRequestID != p.SealRequestID ||
		sealed.Result.Authorizer == nil || *sealed.Result.Authorizer != p.Claim.Actor || sealed.Attempt.State != task.AttemptSuperseded ||
		sealed.Task.State != task.TaskCompleted || sealed.Attempt.State == task.AttemptSucceeded {
		t.Fatalf("authorized result: %+v", sealed)
	}
	request, err := s.GetSealRequest(context.Background(), p.SealRequestID)
	if err != nil || request.State != SealRequestCompleted || request.CompletedAt == nil || request.ClaimOwner != "" {
		t.Fatalf("completed request: %+v, %v", request, err)
	}
	replayedSeal, err := s.SealAuthorizedResult(context.Background(), sealParams)
	if err != nil || !replayedSeal.Replayed || replayedSeal.Result.ID != sealed.Result.ID {
		t.Fatalf("completed seal replay: %+v, %v", replayedSeal, err)
	}
	source, err := s.FindResultAwaitingVerification(context.Background(), preview.Workspace.ID)
	if err != nil || source.Result.ID != sealed.Result.ID || source.Attempt.State != task.AttemptSuperseded {
		t.Fatalf("user seal verification source: %+v, %v", source, err)
	}
	verification := prepareJournalVerification(t, s, sealed, 45000)
	if verification.Verification.ResultID != sealed.Result.ID {
		t.Fatalf("user seal verification: %+v", verification)
	}
	var successEvents int
	if err := s.db.QueryRow(`SELECT count(*) FROM events WHERE attempt_id=? AND type='attempt.succeeded'`, preview.Attempt.ID).Scan(&successEvents); err != nil || successEvents != 0 {
		t.Fatalf("user seal claimed OpenCode success: count=%d err=%v", successEvents, err)
	}
}

func TestUserSealRejectsConflictingReplayAndActor(t *testing.T) {
	s, preview := userSealFixture(t, 1310)
	p := testSealRequest(preview, 1310, "owned-seal")
	accepted, err := s.RequestSeal(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	changed := p
	changed.Claim.RequestHash = sha256.Sum256([]byte("different"))
	if _, err := s.RequestSeal(context.Background(), changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed replay = %v", err)
	}
	other := p
	other.Claim.Actor.ID = "other-device"
	if _, err := s.RequestSeal(context.Background(), other); !errors.Is(err, ErrIdempotencyOwnerMismatch) {
		t.Fatalf("actor replay = %v", err)
	}
	stored, err := s.GetSealRequest(context.Background(), accepted.Request.ID)
	if err != nil || stored.State != SealRequestPending || stored.Authorizer.ID != p.Claim.Actor.ID {
		t.Fatalf("stored authorization changed: %+v, %v", stored, err)
	}
}

func TestUserSealExactSnapshotAndRevisionBinding(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SealAuthorizedResultParams)
	}{
		{"head", func(p *SealAuthorizedResultParams) {
			p.Result.ResultCommit = "1111111111111111111111111111111111111111"
			p.Result.Outcome = task.ResultChanged
		}},
		{"tree", func(p *SealAuthorizedResultParams) { p.Result.TreeOID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" }},
		{"manifest", func(p *SealAuthorizedResultParams) { p.Result.ManifestSHA256 = [32]byte{} }},
		{"task-revision", func(p *SealAuthorizedResultParams) { p.Result.ExpectedTaskRevision++ }},
		{"attempt-revision", func(p *SealAuthorizedResultParams) { p.Result.ExpectedAttemptRevision++ }},
		{"actor", func(p *SealAuthorizedResultParams) { p.Result.Authorizer.ID = "different-user" }},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, preview := userSealFixture(t, 1320+i)
			request := testSealRequest(preview, 1320+i, "binding-"+tt.name)
			if _, err := s.RequestSeal(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			claimAt := request.AcceptedAt.Add(time.Millisecond)
			claimed, err := s.ClaimSealRequest(context.Background(), ClaimSealRequestParams{WorkspaceID: preview.Workspace.ID,
				ClaimOwner: "seal-worker", Now: claimAt, LeaseExpiresAt: claimAt.Add(time.Minute)})
			if err != nil {
				t.Fatal(err)
			}
			params := authorizedSealParams(claimed, request.AcceptedAt.Add(2*time.Millisecond))
			tt.mutate(&params)
			if _, err := s.SealAuthorizedResult(context.Background(), params); err == nil {
				t.Fatal("changed authorization sealed")
			}
			if _, err := s.GetResult(context.Background(), request.ResultID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("invalid seal wrote a result: %v", err)
			}
		})
	}
}

func TestUserSealCancellationRaceIsAtomic(t *testing.T) {
	for run := 0; run < 8; run++ {
		s, preview := userSealFixture(t, 1340+run)
		request := testSealRequest(preview, 1340+run, "cancel-race")
		if _, err := s.RequestSeal(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		claimAt := request.AcceptedAt.Add(time.Millisecond)
		claimed, err := s.ClaimSealRequest(context.Background(), ClaimSealRequestParams{WorkspaceID: preview.Workspace.ID,
			ClaimOwner: "seal-worker", Now: claimAt, LeaseExpiresAt: claimAt.Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		seal := authorizedSealParams(claimed, request.AcceptedAt.Add(2*time.Millisecond))
		cancel := testCancellation(preview.Task.ID, 1340+run, "user-seal-race", "stop")
		start := make(chan struct{})
		var sealErr, cancelErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, sealErr = s.SealAuthorizedResult(context.Background(), seal) }()
		go func() { defer wg.Done(); <-start; _, cancelErr = s.RequestCancellation(context.Background(), cancel) }()
		close(start)
		wg.Wait()
		if (sealErr == nil) == (cancelErr == nil) {
			t.Fatalf("race winners seal=%v cancel=%v", sealErr, cancelErr)
		}
		stored, err := s.GetTask(context.Background(), preview.Task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if sealErr == nil {
			if stored.State != task.TaskCompleted || stored.CancelEpoch != 0 {
				t.Fatalf("seal winner: %+v", stored)
			}
		} else if stored.State != task.TaskCancelRequested || stored.SealedResultID != "" {
			t.Fatalf("cancel winner: %+v sealErr=%v", stored, sealErr)
		}
		_ = s.Close()
	}
}

func TestUserSealClaimRecoveryAfterRestart(t *testing.T) {
	path := testDBPath(t)
	s := openTestStore(t, path)
	createTestWorkspace(t, s)
	a, err := s.AdmitTask(context.Background(), testAdmission(1360, "restart-task", "Restart seal"))
	if err != nil {
		t.Fatal(err)
	}
	setCancellationFixtureState(t, s, a, task.AttemptRunning)
	preview, _ := s.GetSealPreview(context.Background(), a.Task.ID)
	request := testSealRequest(preview, 1360, "restart-seal")
	if _, err := s.RequestSeal(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	claimAt := request.AcceptedAt.Add(time.Millisecond)
	first, err := s.ClaimSealRequest(context.Background(), ClaimSealRequestParams{WorkspaceID: preview.Workspace.ID,
		ClaimOwner: "crashed-worker", Now: claimAt, LeaseExpiresAt: claimAt.Add(10 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.ClaimSealRequest(context.Background(), ClaimSealRequestParams{WorkspaceID: preview.Workspace.ID,
		ClaimOwner: "new-worker", Now: claimAt.Add(5 * time.Millisecond), LeaseExpiresAt: claimAt.Add(time.Minute)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("active lease reclaimed: %v", err)
	}
	recovered, err := s.ClaimSealRequest(context.Background(), ClaimSealRequestParams{WorkspaceID: preview.Workspace.ID,
		ClaimOwner: "new-worker", Now: claimAt.Add(11 * time.Millisecond), LeaseExpiresAt: claimAt.Add(time.Minute)})
	if err != nil || recovered.Request.ClaimRevision != first.Request.ClaimRevision+1 || recovered.Request.ClaimOwner != "new-worker" {
		t.Fatalf("recovered claim: %+v, %v", recovered, err)
	}
}

func TestMigrationFourFromVersionThree(t *testing.T) {
	path := testDBPath(t)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	raw := openRaw(t, path)
	for _, migration := range migrations[:3] {
		if _, err := raw.Exec(migration.sql); err != nil {
			t.Fatalf("install migration %d: %v", migration.version, err)
		}
		if _, err := raw.Exec(`INSERT INTO schema_migrations(version,name,checksum) VALUES(?,?,?)`, migration.version, migration.name, migrationChecksum(migration)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`PRAGMA user_version=3`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	s := openTestStore(t, path)
	t.Cleanup(func() { _ = s.Close() })
	var version, tables, columns int
	_ = s.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	_ = s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='seal_requests'`).Scan(&tables)
	_ = s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('results') WHERE name IN ('completion_authority','seal_request_id','authorizer_actor_snapshot_id')`).Scan(&columns)
	if version != 6 || tables != 1 || columns != 3 {
		t.Fatalf("migration version=%d tables=%d columns=%d", version, tables, columns)
	}
}

func userSealFixture(t *testing.T, n int) (*Store, SealPreview) {
	t.Helper()
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	a, err := s.AdmitTask(context.Background(), testAdmission(n, "seal-task", "Seal snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	setCancellationFixtureState(t, s, a, task.AttemptRunning)
	preview, err := s.GetSealPreview(context.Background(), a.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, preview
}

func testSealRequest(preview SealPreview, n int, key string) RequestSealParams {
	hash := sha256.Sum256([]byte("task.seal\n" + key))
	return RequestSealParams{
		SealRequestID: task.SealRequestID(testID("slr_", n)), ReceiptID: testReceiptID(20000 + n),
		ResultID: testResultID(20000 + n), ResultEventID: testEventID(30000 + n*2), TaskEventID: testEventID(30001 + n*2),
		TaskID: preview.Task.ID,
		Claim: task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: preview.Workspace.ID, CommandKind: SealTaskCommand},
			Key: task.IdempotencyKey(key), RequestHash: hash,
			Actor: task.ActorSnapshot{Type: task.ActorDevice, ID: "device-1", DisplayName: "Phone", CredentialID: "credential-1", Authentication: "fern_device_cookie", RequestID: "seal-request"}},
		ExpectedWorkspaceRevision: preview.Workspace.Revision, ExpectedTaskRevision: preview.Task.Revision,
		ExpectedAttemptRevision: preview.Attempt.Revision, RepositoryID: preview.Task.RepositoryID, BaseSHA: preview.Task.BaseSHA,
		ExpectedResultCommit: preview.Task.BaseSHA, ExpectedTreeOID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ExpectedOutcome: task.ResultNoChanges, ExpectedManifestEntries: 0, ExpectedManifestSHA256: sha256.Sum256([]byte("[]")),
		ExpectedWorktreeClean: true,
		APIContractVersion:    "v1", AcceptedAt: preview.Attempt.UpdatedAt.Add(time.Millisecond),
	}
}

func authorizedSealParams(work SealRequestWork, collectedAt time.Time) SealAuthorizedResultParams {
	evidence := json.RawMessage(`{"authority":"user_seal","sealRequestId":"` + string(work.Request.ID) + `"}`)
	authorizer := work.Request.Authorizer
	return SealAuthorizedResultParams{
		SealRequestID: work.Request.ID, ClaimOwner: work.Request.ClaimOwner, ExpectedClaimRevision: work.Request.ClaimRevision,
		Result: SealResultParams{
			ResultID: work.Request.ResultID, TaskID: work.Request.TaskID, AttemptID: work.Request.AttemptID,
			ExpectedAttemptRevision: work.Request.ExpectedAttemptRevision, ExpectedTaskRevision: work.Request.ExpectedTaskRevision,
			ResultEventID: work.Request.ResultEventID, TaskEventID: work.Request.TaskEventID,
			RepositoryID: work.Request.RepositoryID, BaseSHA: work.Request.BaseSHA, ResultCommit: work.Request.ExpectedResultCommit,
			TreeOID: work.Request.ExpectedTreeOID, Outcome: work.Request.ExpectedOutcome, WorktreeClean: true,
			Manifest: []ManifestEntry{}, ManifestSHA256: work.Request.ExpectedManifestSHA256,
			OpenCodeSessionID: work.Preview.Attempt.OpenCodeSessionID, OpenCodeMessageID: work.Preview.Attempt.OpenCodeMessageID,
			EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), PolicyVersion: "result-v1",
			CollectedAt: collectedAt, SealedAt: collectedAt.Add(time.Millisecond), Actor: testDeliveryActor(),
			CompletionAuthority: SealAuthorityUser, SealRequestID: work.Request.ID, Authorizer: &authorizer,
		},
	}
}
