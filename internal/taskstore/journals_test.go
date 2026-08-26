package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

func TestVerificationAndPublicationJournalsEndToEnd(t *testing.T) {
	s, sealed := sealedJournalFixture(t, 1300)
	source, err := s.FindResultAwaitingVerification(context.Background(), testWorkspaceID())
	if err != nil || source.Result.ID != sealed.Result.ID || source.Task.ID != sealed.Task.ID ||
		source.Attempt.ID != sealed.Attempt.ID || source.Task.Revision != sealed.Task.Revision || source.Attempt.Revision != sealed.Attempt.Revision {
		t.Fatalf("find result awaiting verification: %+v, %v", source, err)
	}
	verification := prepareJournalVerification(t, s, sealed, 20000)
	if _, err := s.FindResultAwaitingVerification(context.Background(), testWorkspaceID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("result with verification remained discoverable: %v", err)
	}
	if verification.Verification.State != VerificationPrepared || verification.Verification.VerifiedCommit != sealed.Result.ResultCommit ||
		verification.Event.Type != "verification.prepared" || verification.Verification.Revision != 1 {
		t.Fatalf("prepared verification: %+v", verification)
	}
	s = reopenJournalStore(t, s)
	prepared, err := s.FindPreparedVerification(context.Background(), testWorkspaceID())
	if err != nil || prepared.Verification.ID != verification.Verification.ID {
		t.Fatalf("find prepared verification: %+v, %v", prepared, err)
	}
	evidence := journalEvidence()
	running, err := s.AdvanceVerification(context.Background(), AdvanceVerificationParams{
		VerificationID: verification.Verification.ID, ExpectedRevision: 1,
		ExpectedTaskRevision: sealed.Task.Revision, ExpectedAttemptRevision: sealed.Attempt.Revision,
		EventID: testEventID(20001), StartedAt: verification.Verification.UpdatedAt.Add(time.Millisecond),
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	})
	if err != nil || running.Verification.State != VerificationRunning || running.Verification.EffectAttempt != 1 {
		t.Fatalf("start verification: %+v, %v", running, err)
	}
	s = reopenJournalStore(t, s)
	running, err = s.FindRunningVerification(context.Background(), testWorkspaceID())
	if err != nil || running.Verification.State != VerificationRunning || running.Verification.EffectAttempt != 1 {
		t.Fatalf("restarted running verification: %+v, %v", running, err)
	}
	emptyHash := sha256.Sum256(nil)
	zeroOutput := VerificationOutput{SHA256: emptyHash}
	exit := 0
	completed, err := s.CompleteVerification(context.Background(), CompleteVerificationParams{
		VerificationID: running.Verification.ID, ExpectedRevision: running.Verification.Revision,
		ExpectedTaskRevision: sealed.Task.Revision, ExpectedAttemptRevision: sealed.Attempt.Revision,
		EventID: testEventID(20002), State: VerificationSucceeded, Outcome: "passed", ExitCode: &exit,
		Stdout: zeroOutput, Stderr: zeroOutput, EndedAt: running.Verification.UpdatedAt.Add(time.Millisecond),
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	})
	if err != nil || completed.Verification.State != VerificationSucceeded || completed.Verification.Stdout == nil ||
		completed.Verification.Stdout.SHA256 != emptyHash || completed.Verification.EndedAt == nil {
		t.Fatalf("complete verification: %+v, %v", completed, err)
	}
	s = reopenJournalStore(t, s)
	completed, err = s.InspectVerification(context.Background(), completed.Verification.ID)
	if err != nil || completed.Verification.State != VerificationSucceeded {
		t.Fatalf("restarted completed verification: %+v, %v", completed, err)
	}

	publication := prepareJournalPublication(t, s, sealed, completed, 21000)
	if publication.Publication.State != PublicationPrepared || publication.Publication.Tuple.ResultCommit != sealed.Result.ResultCommit {
		t.Fatalf("prepared publication: %+v", publication)
	}
	s = reopenJournalStore(t, s)
	work, err := s.FindPublicationWork(context.Background(), testWorkspaceID())
	if err != nil || work.Publication.ID != publication.Publication.ID || work.Task.ID != sealed.Task.ID ||
		work.Attempt.ID != sealed.Attempt.ID || work.Result.ID != sealed.Result.ID ||
		work.Verification.ID != completed.Verification.ID || work.Event.ID != publication.Event.ID ||
		work.Task.Revision != sealed.Task.Revision || work.Attempt.Revision != sealed.Attempt.Revision {
		t.Fatalf("exact publication work: %+v, %v", work, err)
	}
	publication = advanceJournalPublication(t, s, sealed, publication, PublicationPhaseNone, PublicationPhasePushStarted, "", 21001)
	s = reopenJournalStore(t, s)
	assertPublicationPhase(t, s, publication.Publication.ID, PublicationPhasePushStarted)
	publication = advanceJournalPublication(t, s, sealed, publication, PublicationPhasePushStarted, PublicationPhasePushObserved, sealed.Result.ResultCommit, 21002)
	s = reopenJournalStore(t, s)
	assertPublicationPhase(t, s, publication.Publication.ID, PublicationPhasePushObserved)
	publication = advanceJournalPublication(t, s, sealed, publication, PublicationPhasePushObserved, PublicationPhasePRCreateStarted, "", 21003)
	s = reopenJournalStore(t, s)
	assertPublicationPhase(t, s, publication.Publication.ID, PublicationPhasePRCreateStarted)
	observation := journalPublicationObservation(publication.Publication.Tuple)
	publishedAt := publication.Publication.UpdatedAt.Add(time.Millisecond)
	published, err := s.CompletePublication(context.Background(), CompletePublicationParams{
		PublicationID: publication.Publication.ID, ExpectedRevision: publication.Publication.Revision,
		ExpectedTaskRevision: sealed.Task.Revision, ExpectedAttemptRevision: sealed.Attempt.Revision,
		EventID: testEventID(21004), Observation: observation, CompletedAt: publishedAt,
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	})
	if err != nil || published.Publication.State != PublicationPublished || published.Publication.Observation == nil ||
		published.Publication.Observation.PullRequest.Number != 42 || published.Publication.ObservedRemoteSHA != sealed.Result.ResultCommit {
		t.Fatalf("complete publication: %+v, %v", published, err)
	}
	s = reopenJournalStore(t, s)
	persisted, err := s.InspectPublication(context.Background(), published.Publication.ID)
	if err != nil || persisted.Publication.State != PublicationPublished || persisted.Publication.Observation == nil ||
		persisted.Publication.Observation.PullRequest.Number != 42 {
		t.Fatalf("restarted publication: %+v, %v", persisted, err)
	}
	replay, err := s.CompletePublication(context.Background(), CompletePublicationParams{
		PublicationID: publication.Publication.ID, ExpectedRevision: publication.Publication.Revision,
		ExpectedTaskRevision: sealed.Task.Revision, ExpectedAttemptRevision: sealed.Attempt.Revision,
		EventID: testEventID(21004), Observation: observation, CompletedAt: publishedAt,
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	})
	if err != nil || !replay.Replayed {
		t.Fatalf("publication replay: %+v, %v", replay, err)
	}
}

func reopenJournalStore(t *testing.T, store *Store) *Store {
	t.Helper()
	path := store.Path()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, path)
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

func assertPublicationPhase(t *testing.T, store *Store, id task.PublicationID, phase PublicationPhase) {
	t.Helper()
	record, err := store.InspectPublication(context.Background(), id)
	if err != nil || record.Publication.EffectPhase != phase {
		t.Fatalf("restarted publication phase = %s, want %s: %v", record.Publication.EffectPhase, phase, err)
	}
}

func TestJournalStaleRevisionRecoveryAndDirectSQLFences(t *testing.T) {
	s, sealed := sealedJournalFixture(t, 1310)
	prepared := prepareJournalVerification(t, s, sealed, 22000)
	evidence := journalEvidence()
	start := AdvanceVerificationParams{VerificationID: prepared.Verification.ID, ExpectedRevision: 1,
		ExpectedTaskRevision: sealed.Task.Revision, ExpectedAttemptRevision: sealed.Attempt.Revision,
		EventID: testEventID(22001), StartedAt: prepared.Verification.UpdatedAt.Add(time.Millisecond),
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor()}
	running, err := s.AdvanceVerification(context.Background(), start)
	if err != nil {
		t.Fatal(err)
	}
	start.EventID = testEventID(22002)
	if _, err := s.AdvanceVerification(context.Background(), start); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("changed replay = %v", err)
	}
	if _, err := s.db.Exec(`UPDATE verifications SET state='succeeded',revision=revision+1,updated_at=updated_at+1 WHERE id=?`, running.Verification.ID); err == nil {
		t.Fatal("eventless verification transition succeeded")
	}
	if _, err := s.db.Exec(`UPDATE verifications SET policy_name='other' WHERE id=?`, running.Verification.ID); err == nil {
		t.Fatal("verification tuple drift succeeded")
	}
	empty := VerificationOutput{SHA256: sha256.Sum256(nil)}
	exit := 1
	if _, err := s.CompleteVerification(context.Background(), CompleteVerificationParams{
		VerificationID: running.Verification.ID, ExpectedRevision: prepared.Verification.Revision,
		ExpectedTaskRevision: sealed.Task.Revision, ExpectedAttemptRevision: sealed.Attempt.Revision,
		EventID: testEventID(22004), State: VerificationFailed, Outcome: "exit_nonzero", ExitCode: &exit,
		Stdout: empty, Stderr: empty, Reason: "check_failed", EndedAt: running.Verification.UpdatedAt.Add(time.Millisecond),
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	}); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale verification completion = %v", err)
	}
	recovered, err := s.RecoverVerification(context.Background(), RecoverVerificationParams{
		VerificationID: running.Verification.ID, ExpectedRevision: running.Verification.Revision,
		ExpectedTaskRevision: sealed.Task.Revision, ExpectedAttemptRevision: sealed.Attempt.Revision,
		EventID: testEventID(22003), Reason: "runner_observation_ambiguous", Outcome: "runner_failure", Stdout: &empty, Stderr: &empty,
		RecoveredAt: running.Verification.UpdatedAt.Add(time.Millisecond), EvidencePayload: evidence,
		EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	})
	if err != nil || recovered.Verification.State != VerificationRecoveryRequired {
		t.Fatalf("recover verification: %+v, %v", recovered, err)
	}
	if _, err := s.db.Exec(`UPDATE verifications SET state='failed' WHERE id=?`, recovered.Verification.ID); err == nil {
		t.Fatal("terminal verification mutated")
	}
}

func TestVerificationStartRaceHasOneEffectingAttempt(t *testing.T) {
	s, sealed := sealedJournalFixture(t, 1320)
	prepared := prepareJournalVerification(t, s, sealed, 23000)
	evidence := journalEvidence()
	p := AdvanceVerificationParams{VerificationID: prepared.Verification.ID, ExpectedRevision: prepared.Verification.Revision,
		ExpectedTaskRevision: sealed.Task.Revision, ExpectedAttemptRevision: sealed.Attempt.Revision,
		EventID: testEventID(23001), StartedAt: prepared.Verification.UpdatedAt.Add(time.Millisecond),
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor()}
	const workers = 12
	start := make(chan struct{})
	results := make(chan VerificationRecord, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := s.AdvanceVerification(context.Background(), p)
			if err != nil {
				errs <- err
				return
			}
			results <- got
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Errorf("concurrent start: %v", err)
	}
	first := 0
	for result := range results {
		if !result.Replayed {
			first++
		}
	}
	if first != 1 {
		t.Fatalf("effecting starts = %d", first)
	}
}

func TestVerificationWorkspaceEffectFence(t *testing.T) {
	s, sealed := sealedJournalFixture(t, 1321)
	first := prepareJournalVerification(t, s, sealed, 23100)
	second := prepareJournalVerification(t, s, sealed, 23110)
	evidence := journalEvidence()
	params := []AdvanceVerificationParams{
		{VerificationID: first.Verification.ID, ExpectedRevision: 1, ExpectedTaskRevision: sealed.Task.Revision,
			ExpectedAttemptRevision: sealed.Attempt.Revision, EventID: testEventID(23101), StartedAt: second.Verification.UpdatedAt.Add(time.Millisecond),
			EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor()},
		{VerificationID: second.Verification.ID, ExpectedRevision: 1, ExpectedTaskRevision: sealed.Task.Revision,
			ExpectedAttemptRevision: sealed.Attempt.Revision, EventID: testEventID(23111), StartedAt: second.Verification.UpdatedAt.Add(time.Millisecond),
			EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor()},
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, p := range params {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.AdvanceVerification(context.Background(), p)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	succeeded, failed := 0, 0
	for err := range errs {
		if err == nil {
			succeeded++
		} else {
			failed++
		}
	}
	if succeeded != 1 || failed != 1 {
		t.Fatalf("workspace effect fence successes=%d failures=%d", succeeded, failed)
	}
}

func TestPublicationUncertainReadOnlyRecoveryAndSQLFences(t *testing.T) {
	s, sealed := sealedJournalFixture(t, 1330)
	verification := prepareJournalVerification(t, s, sealed, 24000)
	evidence := journalEvidence()
	running, err := s.AdvanceVerification(context.Background(), AdvanceVerificationParams{
		VerificationID: verification.Verification.ID, ExpectedRevision: 1, ExpectedTaskRevision: sealed.Task.Revision,
		ExpectedAttemptRevision: sealed.Attempt.Revision, EventID: testEventID(24001),
		StartedAt: verification.Verification.UpdatedAt.Add(time.Millisecond), EvidencePayload: evidence,
		EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	exit := 0
	empty := VerificationOutput{SHA256: sha256.Sum256(nil)}
	verified, err := s.CompleteVerification(context.Background(), CompleteVerificationParams{
		VerificationID: running.Verification.ID, ExpectedRevision: running.Verification.Revision,
		ExpectedTaskRevision: sealed.Task.Revision, ExpectedAttemptRevision: sealed.Attempt.Revision,
		EventID: testEventID(24002), State: VerificationSucceeded, Outcome: "passed", ExitCode: &exit,
		Stdout: empty, Stderr: empty, EndedAt: running.Verification.UpdatedAt.Add(time.Millisecond),
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	publication := prepareJournalPublication(t, s, sealed, verified, 24100)
	if _, err := s.db.Exec(`UPDATE publications SET state='running',effect_phase='push_observed',observed_remote_sha=result_commit,revision=revision+1,updated_at=updated_at+1 WHERE id=?`, publication.Publication.ID); err == nil {
		t.Fatal("eventless publication phase skip succeeded")
	}
	publication = advanceJournalPublication(t, s, sealed, publication, PublicationPhaseNone, PublicationPhasePushStarted, "", 24101)
	uncertain, err := s.RecoverPublication(context.Background(), RecoverPublicationParams{
		PublicationID: publication.Publication.ID, ExpectedRevision: publication.Publication.Revision,
		ExpectedTaskRevision: sealed.Task.Revision, ExpectedAttemptRevision: sealed.Attempt.Revision,
		EventID: testEventID(24102), State: PublicationUncertain, Reason: "push_response_lost",
		RecoveredAt: publication.Publication.UpdatedAt.Add(time.Millisecond), EvidencePayload: evidence,
		EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	})
	if err != nil || uncertain.Publication.State != PublicationUncertain || uncertain.Publication.EffectPhase != PublicationPhasePushStarted {
		t.Fatalf("uncertain publication: %+v, %v", uncertain, err)
	}
	observed := advanceJournalPublication(t, s, sealed, uncertain, PublicationPhasePushStarted, PublicationPhasePushObserved, sealed.Result.ResultCommit, 24103)
	if _, err := s.db.Exec(`UPDATE publications SET effect_phase='push_started',revision=revision+1,updated_at=updated_at+1 WHERE id=?`, observed.Publication.ID); err == nil {
		t.Fatal("publication phase regression succeeded")
	}
	observation := journalPublicationObservation(observed.Publication.Tuple)
	published, err := s.CompletePublication(context.Background(), CompletePublicationParams{
		PublicationID: observed.Publication.ID, ExpectedRevision: observed.Publication.Revision,
		ExpectedTaskRevision: sealed.Task.Revision, ExpectedAttemptRevision: sealed.Attempt.Revision,
		EventID: testEventID(24104), Observation: observation, CompletedAt: observed.Publication.UpdatedAt.Add(time.Millisecond),
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	})
	if err != nil || published.Publication.State != PublicationPublished {
		t.Fatalf("read-only reconciled publication: %+v, %v", published, err)
	}
	if _, err := s.db.Exec(`UPDATE publications SET state='uncertain' WHERE id=?`, published.Publication.ID); err == nil {
		t.Fatal("terminal publication mutated")
	}
}

func TestVerificationPreparationCancellationSealRace(t *testing.T) {
	for run := 0; run < 10; run++ {
		s, success := succeededTestAttempt(t, 1400+run)
		sealParams := testSealResult(success, 25000+run*20)
		cancelParams := testCancellation(success.Task.ID, 1400+run, "journal-seal-cancel", "stop")
		start := make(chan struct{})
		var sealed SealedResult
		var sealErr, cancelErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			sealed, sealErr = s.SealResult(context.Background(), sealParams)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, cancelErr = s.RequestCancellation(context.Background(), cancelParams)
		}()
		close(start)
		wg.Wait()
		if sealErr == nil {
			if !errors.Is(cancelErr, ErrTaskAlreadyTerminal) {
				t.Fatalf("seal winner cancellation error = %v", cancelErr)
			}
			prepared := prepareJournalVerification(t, s, sealed, 25100+run*20)
			if prepared.Verification.ResultID != sealed.Result.ID {
				t.Fatalf("verification escaped sealed result: %+v", prepared)
			}
		} else {
			if cancelErr != nil || (!errors.Is(sealErr, ErrStaleRevision) && !errors.Is(sealErr, ErrInvalidState)) {
				t.Fatalf("cancellation winner seal=%v cancellation=%v", sealErr, cancelErr)
			}
			var count int
			if err := s.db.QueryRow(`SELECT count(*) FROM verifications`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("cancellation winner verification count=%d err=%v", count, err)
			}
		}
		_ = s.Close()
	}
}

func TestMigrationThreeFromVersionTwo(t *testing.T) {
	path := testDBPath(t)
	f, err := openVersionTwoDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, path)
	t.Cleanup(func() { _ = s.Close() })
	var version, tables int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('journal_events','verifications','publications')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if version != 6 || tables != 3 {
		t.Fatalf("migration version=%d tables=%d", version, tables)
	}
}

func sealedJournalFixture(t *testing.T, n int) (*Store, SealedResult) {
	t.Helper()
	s, success := succeededTestAttempt(t, n)
	p := testSealResult(success, n+10000)
	sealed, err := s.SealResult(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return s, sealed
}

func prepareJournalVerification(t *testing.T, s *Store, sealed SealedResult, n int) VerificationRecord {
	t.Helper()
	evidence := journalEvidence()
	p := PrepareVerificationParams{
		VerificationID: task.VerificationID(testID("ver_", n)), ResultID: sealed.Result.ID,
		ExpectedTaskRevision: sealed.Task.Revision, ExpectedAttemptRevision: sealed.Attempt.Revision,
		EventID: testEventID(n), PolicyName: "go-test", PolicySHA256: sha256.Sum256([]byte("policy")),
		VerifiedCommit: sealed.Result.ResultCommit, WorkingDirectory: "", Timeout: time.Minute, OutputLimitBytes: 1024,
		RunnerName: "fern-verifier", RunnerVersion: "v1", ImageDigest: "sha256:image",
		EnvironmentSHA256: sha256.Sum256([]byte("environment")), PreparedAt: sealed.Result.UpdatedAt.Add(time.Millisecond),
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	}
	got, err := s.PrepareVerification(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.PrepareVerification(context.Background(), p)
	if err != nil || !replay.Replayed {
		t.Fatalf("verification preparation replay: %+v, %v", replay, err)
	}
	return got
}

func prepareJournalPublication(t *testing.T, s *Store, sealed SealedResult, verification VerificationRecord, n int) PublicationRecord {
	t.Helper()
	evidence := journalEvidence()
	operationID := task.PublicationOperationID(testID("op_", n))
	tuple := task.PublicationTuple{OperationID: operationID, InstallationID: 123, RepositoryID: sealed.Result.RepositoryID,
		RepositoryFullName: "owner/repository", WorkspaceName: "demo", BaseRef: sealed.Task.BaseRef,
		BaseSHA: sealed.Result.BaseSHA, ResultCommit: sealed.Result.ResultCommit, Branch: task.PublicationBranch("demo", operationID)}
	p := PreparePublicationParams{PublicationID: task.PublicationID(testID("pub_", n)), ResultID: sealed.Result.ID,
		VerificationID: verification.Verification.ID, ExpectedTaskRevision: sealed.Task.Revision,
		ExpectedAttemptRevision: sealed.Attempt.Revision, EventID: testEventID(n), Tuple: tuple,
		BrokerPolicyVersion: "github-v1", BrokerPolicySHA256: sha256.Sum256([]byte("broker-policy")),
		PreparedAt: verification.Verification.UpdatedAt.Add(time.Millisecond), EvidencePayload: evidence,
		EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor()}
	got, err := s.PreparePublication(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func advanceJournalPublication(t *testing.T, s *Store, sealed SealedResult, publication PublicationRecord, from, to PublicationPhase, remote task.GitOID, n int) PublicationRecord {
	t.Helper()
	evidence := journalEvidence()
	got, err := s.AdvancePublication(context.Background(), AdvancePublicationParams{PublicationID: publication.Publication.ID,
		ExpectedRevision: publication.Publication.Revision, ExpectedTaskRevision: sealed.Task.Revision,
		ExpectedAttemptRevision: sealed.Attempt.Revision, From: from, To: to, ObservedRemoteSHA: remote,
		EventID: testEventID(n), AdvancedAt: publication.Publication.UpdatedAt.Add(time.Millisecond), EvidencePayload: evidence,
		EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor()})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func journalPublicationObservation(tuple task.PublicationTuple) task.PublicationObservation {
	return task.PublicationObservation{RemoteSHA: tuple.ResultCommit, PullRequest: task.PullRequestObservation{
		RepositoryID: tuple.RepositoryID, RepositoryFullName: tuple.RepositoryFullName, Number: 42,
		URL: "https://github.com/owner/repository/pull/42", State: "open", Draft: true,
		BaseRepositoryID: tuple.RepositoryID, BaseRepositoryFullName: tuple.RepositoryFullName,
		BaseRef: tuple.BaseRef, BaseSHA: tuple.BaseSHA, HeadRepositoryID: tuple.RepositoryID,
		HeadRepositoryFullName: tuple.RepositoryFullName, HeadRepositoryOwner: "owner", HeadRepositoryName: "repository",
		HeadRef: tuple.Branch, HeadSHA: tuple.ResultCommit,
	}}
}

func journalEvidence() json.RawMessage { return json.RawMessage(`{"authorityRead":true,"objects":1}`) }

func openVersionTwoDatabase(path string) (*sql.DB, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	for i, migration := range migrations[:2] {
		if _, err := db.Exec(migration.sql); err != nil {
			_ = db.Close()
			return nil, err
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(version,name,checksum) VALUES(?,?,?)`, i+1, migration.name, migrationChecksum(migration)); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if _, err := db.Exec(`PRAGMA user_version=2`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}
