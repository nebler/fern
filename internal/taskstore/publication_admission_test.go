package taskstore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

func TestPublicationAdmissionDerivesTupleAndReplaySurvivesRestart(t *testing.T) {
	s, sealed, verification := publicationAdmissionFixture(t, 3100)
	p := testPublicationAdmission(sealed, verification, 31000, "publish-once")
	first, err := s.AdmitPublication(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.Publication.AdmissionReceiptID != first.Receipt.ID || first.Event.Type != "publication.prepared" ||
		first.Publication.Tuple.InstallationID != 123 || first.Publication.Tuple.RepositoryID != sealed.Result.RepositoryID ||
		first.Publication.Tuple.RepositoryFullName != "owner/repository" || first.Publication.Tuple.BaseRef != sealed.Task.BaseRef ||
		first.Publication.Tuple.BaseSHA != sealed.Result.BaseSHA || first.Publication.Tuple.ResultCommit != sealed.Result.ResultCommit ||
		first.Publication.Tuple.Branch != task.PublicationBranch("demo", p.OperationID) {
		t.Fatalf("admission did not derive exact tuple: %+v", first)
	}
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, path)
	t.Cleanup(func() { _ = reopened.Close() })
	p.PublicationID = task.PublicationID(testID("pub_", 31990))
	p.OperationID = task.PublicationOperationID(testID("op_", 31991))
	p.ReceiptID = testReceiptID(31992)
	p.EventID = testEventID(31993)
	p.AcceptedAt = p.AcceptedAt.Add(time.Hour)
	replay, err := reopened.AdmitPublication(context.Background(), p)
	if err != nil || !replay.Replayed || replay.Publication.ID != first.Publication.ID || replay.Receipt.ID != first.Receipt.ID ||
		replay.Event.ID != first.Event.ID || string(replay.Receipt.ResponseProjection) != string(first.Receipt.ResponseProjection) {
		t.Fatalf("restart replay = %+v, %v", replay, err)
	}
}

func TestPublicationAdmissionConcurrentKeyWritesOnce(t *testing.T) {
	s, sealed, verification := publicationAdmissionFixture(t, 3110)
	const contenders = 12
	start := make(chan struct{})
	results := make(chan PublicationAdmission, contenders)
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := range contenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := testPublicationAdmission(sealed, verification, 31100+i*10, "concurrent-publish")
			<-start
			got, err := s.AdmitPublication(context.Background(), p)
			results <- got
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var fresh int
	var publicationID task.PublicationID
	for result := range results {
		if !result.Replayed {
			fresh++
		}
		if publicationID == "" {
			publicationID = result.Publication.ID
		} else if result.Publication.ID != publicationID {
			t.Fatalf("concurrent replay IDs differ: %s and %s", publicationID, result.Publication.ID)
		}
	}
	var receipts, events, publications int
	_ = s.db.QueryRow(`SELECT count(*) FROM receipts WHERE command_kind=?`, PublishResultCommand).Scan(&receipts)
	_ = s.db.QueryRow(`SELECT count(*) FROM journal_events WHERE type='publication.prepared'`).Scan(&events)
	_ = s.db.QueryRow(`SELECT count(*) FROM publications WHERE admission_receipt_id IS NOT NULL`).Scan(&publications)
	if fresh != 1 || receipts != 1 || events != 1 || publications != 1 {
		t.Fatalf("fresh=%d receipts=%d events=%d publications=%d", fresh, receipts, events, publications)
	}
}

func TestPublicationAdmissionConflictsAndIneligibleStatesWriteNothing(t *testing.T) {
	s, sealed, verification := publicationAdmissionFixture(t, 3120)
	p := testPublicationAdmission(sealed, verification, 31200, "publish-conflict")
	if _, err := s.AdmitPublication(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	conflict := p
	conflict.Claim.RequestHash = sha256.Sum256([]byte("different request"))
	if _, err := s.AdmitPublication(context.Background(), conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict = %v", err)
	}
	ownerMismatch := p
	ownerMismatch.Claim.Actor.ID = "other-device"
	if _, err := s.AdmitPublication(context.Background(), ownerMismatch); !errors.Is(err, ErrIdempotencyOwnerMismatch) {
		t.Fatalf("owner mismatch = %v", err)
	}

	ineligible := p
	ineligible.Claim.Key = "ineligible-result"
	ineligible.Claim.RequestHash = sha256.Sum256([]byte("ineligible-result"))
	ineligible.ReceiptID = testReceiptID(31220)
	ineligible.PublicationID = task.PublicationID(testID("pub_", 31221))
	ineligible.OperationID = task.PublicationOperationID(testID("op_", 31222))
	ineligible.EventID = testEventID(31223)
	if _, err := s.AdmitPublication(context.Background(), ineligible); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("already-published result = %v", err)
	}
	var receipts, events, publications int
	_ = s.db.QueryRow(`SELECT count(*) FROM receipts WHERE command_kind=?`, PublishResultCommand).Scan(&receipts)
	_ = s.db.QueryRow(`SELECT count(*) FROM journal_events WHERE type='publication.prepared'`).Scan(&events)
	_ = s.db.QueryRow(`SELECT count(*) FROM publications`).Scan(&publications)
	if receipts != 1 || events != 1 || publications != 1 {
		t.Fatalf("failed commands wrote state: receipts=%d events=%d publications=%d", receipts, events, publications)
	}
}

func publicationAdmissionFixture(t *testing.T, n int) (*Store, SealedResult, VerificationRecord) {
	t.Helper()
	s, success := succeededTestAttempt(t, n)
	seal := testSealResult(success, n+10000)
	seal.Outcome = task.ResultChanged
	seal.ResultCommit = task.GitOID("1111111111111111111111111111111111111111")
	mode, blob, size := "100644", "2222222222222222222222222222222222222222", int64(12)
	seal.Manifest = []ManifestEntry{{PathBase64: base64.StdEncoding.EncodeToString([]byte("change.txt")), ChangeKind: "added",
		NewMode: &mode, NewBlobOID: &blob, NewSize: &size}}
	seal.ManifestSHA256 = manifestDigest(seal.Manifest)
	sealed, err := s.SealResult(context.Background(), seal)
	if err != nil {
		t.Fatal(err)
	}
	prepared := prepareJournalVerification(t, s, sealed, n+20000)
	evidence := journalEvidence()
	running, err := s.AdvanceVerification(context.Background(), AdvanceVerificationParams{
		VerificationID: prepared.Verification.ID, ExpectedRevision: prepared.Verification.Revision,
		ExpectedTaskRevision: sealed.Task.Revision, ExpectedAttemptRevision: sealed.Attempt.Revision,
		EventID: testEventID(n + 20001), StartedAt: prepared.Verification.UpdatedAt.Add(time.Millisecond),
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	exit := 0
	emptyHash := sha256.Sum256(nil)
	output := VerificationOutput{SHA256: emptyHash}
	completed, err := s.CompleteVerification(context.Background(), CompleteVerificationParams{
		VerificationID: running.Verification.ID, ExpectedRevision: running.Verification.Revision,
		ExpectedTaskRevision: sealed.Task.Revision, ExpectedAttemptRevision: sealed.Attempt.Revision,
		EventID: testEventID(n + 20002), State: VerificationSucceeded, Outcome: "passed", ExitCode: &exit,
		Stdout: output, Stderr: output, EndedAt: running.Verification.UpdatedAt.Add(time.Millisecond),
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, sealed, completed
}

func testPublicationAdmission(sealed SealedResult, verification VerificationRecord, n int, key task.IdempotencyKey) AdmitPublicationParams {
	requestHash := sha256.Sum256([]byte("result.publish\n" + string(sealed.Result.ID) + "\n" + string(verification.Verification.ID)))
	return AdmitPublicationParams{
		PublicationID: task.PublicationID(testID("pub_", n)), OperationID: task.PublicationOperationID(testID("op_", n+1)),
		ReceiptID: testReceiptID(n + 2), EventID: testEventID(n + 3), ResultID: sealed.Result.ID,
		VerificationID: verification.Verification.ID,
		Claim: task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: sealed.Result.WorkspaceID, CommandKind: PublishResultCommand},
			Key: key, RequestHash: requestHash, Actor: task.ActorSnapshot{Type: task.ActorDevice, ID: "device-1", DisplayName: "Phone",
				CredentialID: "credential-1", Authentication: "fern_device_cookie", RequestID: "publication-request"}},
		BrokerPolicyVersion: "fern.github-app-publication.v1", BrokerPolicySHA256: sha256.Sum256([]byte("broker-policy")),
		APIContractVersion: "fern.task.v1", AcceptedAt: verification.Verification.UpdatedAt.Add(time.Millisecond),
	}
}
