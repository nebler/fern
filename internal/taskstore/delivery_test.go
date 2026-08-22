package taskstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

func TestClaimPreparedAttemptTransactionAndRestart(t *testing.T) {
	path := testDBPath(t)
	s := openTestStore(t, path)
	createTestWorkspace(t, s)
	admitted, err := s.AdmitTask(context.Background(), testAdmission(100, "delivery-claim", "Do delivery"))
	if err != nil {
		t.Fatal(err)
	}
	found, err := s.FindPreparedAttempt(context.Background(), testWorkspaceID())
	if err != nil || found.Attempt.ID != admitted.Attempt.ID || found.Task.ID != admitted.Task.ID || found.Task.Prompt != "Do delivery" {
		t.Fatalf("find prepared: %+v, %v", found, err)
	}

	claim := testClaim(admitted.Attempt.ID, 1000, "worker-a")
	got, err := s.ClaimPreparedAttempt(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt.State != task.AttemptDelivering || got.Task.State != task.TaskRunning || got.Attempt.Revision != 2 || got.Task.Revision != 2 {
		t.Fatalf("claim state/revisions: %+v", got)
	}
	if got.Attempt.DeliveryClaimOwner == nil || *got.Attempt.DeliveryClaimOwner != claim.LeaseOwner || got.Attempt.DeliveryClaimExpiresAt == nil || !got.Attempt.DeliveryClaimExpiresAt.Equal(claim.LeaseExpiresAt) || got.Attempt.DeliveryStartedAt == nil || !got.Attempt.DeliveryStartedAt.Equal(claim.Now) {
		t.Fatalf("claim fence/timestamps: %+v", got.Attempt)
	}
	assertDeliveryEvents(t, got, "attempt.delivery_started", "task.running")
	if got.Task.LatestEventCursor != got.TaskEvent.Cursor || got.AttemptEvent.Actor != claim.Actor || got.TaskEvent.Actor != claim.Actor {
		t.Fatalf("claim event ownership/cursor: %+v", got)
	}
	if _, err := s.FindPreparedAttempt(context.Background(), testWorkspaceID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("find after claim = %v", err)
	}
	delivering, err := s.FindDeliveringAttempt(context.Background(), testWorkspaceID())
	if err != nil || delivering.Attempt.ID != got.Attempt.ID || delivering.Task.ID != got.Task.ID {
		t.Fatalf("find delivering attempt: %+v, %v", delivering, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	t.Cleanup(func() { _ = s.Close() })
	inspected, err := s.InspectDeliveryAttempt(context.Background(), admitted.Attempt.ID)
	if err != nil || inspected.Attempt.State != task.AttemptDelivering || inspected.Task.State != task.TaskRunning || inspected.Attempt.DeliveryClaimOwner == nil || *inspected.Attempt.DeliveryClaimOwner != "worker-a" {
		t.Fatalf("restart inspection: %+v, %v", inspected, err)
	}
	delivering, err = s.FindDeliveringAttempt(context.Background(), testWorkspaceID())
	if err != nil || delivering.Attempt.ID != got.Attempt.ID {
		t.Fatalf("restart delivering lookup: %+v, %v", delivering, err)
	}
	page, err := s.ListEvents(context.Background(), testWorkspaceID(), 0, 20)
	if err != nil || len(page.Events) != 4 || page.Events[2].Type != "attempt.delivery_started" || page.Events[3].Type != "task.running" {
		t.Fatalf("restart events: %+v, %v", page, err)
	}
}

func TestFindDeliveringAttemptValidatesWorkspaceAndMissingState(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	if _, err := s.FindDeliveringAttempt(context.Background(), "invalid"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid workspace error = %v", err)
	}
	if _, err := s.FindDeliveringAttempt(context.Background(), testWorkspaceID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing delivering attempt error = %v", err)
	}
}

func TestConcurrentClaimHasOneWinner(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	admitted, err := s.AdmitTask(context.Background(), testAdmission(101, "concurrent-claim", "Claim once"))
	if err != nil {
		t.Fatal(err)
	}

	const workers = 20
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			p := testClaim(admitted.Attempt.ID, 1100+i*2, "worker-concurrent")
			_, err := s.ClaimPreparedAttempt(context.Background(), p)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	wins := 0
	for err := range errs {
		if err == nil {
			wins++
			continue
		}
		if !errors.Is(err, ErrInvalidState) && !errors.Is(err, ErrWorkspaceBusy) {
			t.Errorf("claim loser error = %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("claim winners = %d, want 1", wins)
	}
	assertCounts(t, s, 1, 1, 1, 4)
}

func TestOneEffectingAttemptPerWorkspace(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	first, err := s.AdmitTask(context.Background(), testAdmission(102, "effect-1", "First"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AdmitTask(context.Background(), testAdmission(103, "effect-2", "Second"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimPreparedAttempt(context.Background(), testClaim(first.Attempt.ID, 1200, "worker-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimPreparedAttempt(context.Background(), testClaim(second.Attempt.ID, 1210, "worker-b")); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("second workspace claim = %v", err)
	}

	now := testDeliveryTime().Add(20 * time.Millisecond).UnixMilli()
	_, err = s.db.Exec(`
UPDATE attempts SET state='delivering',delivery_claim_owner='direct',delivery_claim_expires_at=?,
delivery_started_at=?,revision=revision+1,updated_at=? WHERE id=?`, now+1000, now, now, second.Attempt.ID)
	if err == nil {
		t.Fatal("schema accepted a second effecting attempt")
	}
	got, err := s.GetAttempt(context.Background(), second.Attempt.ID)
	if err != nil || got.State != task.AttemptPrepared || got.Revision != 1 {
		t.Fatalf("failed uniqueness update changed second attempt: %+v, %v", got, err)
	}
}

func TestRecordAdmissionFencesOwnerAndRevision(t *testing.T) {
	s, claimed := claimedTestAttempt(t, 104)
	evidence, evidenceHash := testEvidence()
	base := RecordAdmissionParams{
		AttemptID: claimed.Attempt.ID, LeaseOwner: "worker-a", ExpectedAttemptRevision: claimed.Attempt.Revision,
		AttemptEventID: testEventID(1300), TaskEventID: testEventID(1301),
		Now: testDeliveryTime().Add(10 * time.Millisecond), EvidencePayload: evidence, EvidenceSHA256: evidenceHash,
		Actor: testDeliveryActor(),
	}
	wrongOwner := base
	wrongOwner.LeaseOwner = "worker-b"
	if _, err := s.RecordAdmission(context.Background(), wrongOwner); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("wrong owner = %v", err)
	}
	stale := base
	stale.ExpectedAttemptRevision = claimed.Attempt.Revision - 1
	if _, err := s.RecordAdmission(context.Background(), stale); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale revision = %v", err)
	}

	got, err := s.RecordAdmission(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt.State != task.AttemptAdmitted || got.Task.State != task.TaskRunning || got.Attempt.DeliveryPhase != DeliveryPhasePromptStarted || got.Attempt.Revision != claimed.Attempt.Revision+1 || got.Task.Revision != claimed.Task.Revision+1 || got.Attempt.AdmittedAt == nil || !got.Attempt.AdmittedAt.Equal(base.Now) || got.Attempt.DeliveryClaimOwner != nil || got.Attempt.DeliveryClaimExpiresAt != nil {
		t.Fatalf("admission state: %+v", got)
	}
	assertDeliveryEvents(t, got, "attempt.admitted", "task.delivery_admitted")
	assertEvidenceEvent(t, got.AttemptEvent, evidence, evidenceHash, "")
	if _, err := s.RecordAdmission(context.Background(), base); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("stale owner after admission = %v", err)
	}
	assertCounts(t, s, 1, 1, 1, 9)
}

func TestRecoverExpiredDeliveryClaimFencesOldWorkerAndSurvivesRestart(t *testing.T) {
	path := testDBPath(t)
	s, claimed := claimedTestAttemptAtPath(t, path, 105)
	evidence, evidenceHash := testEvidence()
	p := RecoverExpiredDeliveryClaimParams{
		AttemptID: claimed.Attempt.ID, ExpiredLeaseOwner: "worker-a", ExpectedAttemptRevision: claimed.Attempt.Revision,
		RecoveryEventID: testEventID(1400), TaskEventID: testEventID(1401),
		Now: testDeliveryTime().Add(time.Second), Reason: "delivery lease expired before a durable observation",
		EvidencePayload: evidence, EvidenceSHA256: evidenceHash, Actor: testRecoveryActor(),
	}
	notExpired := p
	notExpired.Now = testDeliveryTime().Add(100 * time.Millisecond)
	if _, err := s.RecoverExpiredDeliveryClaim(context.Background(), notExpired); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("early recovery = %v", err)
	}
	got, err := s.RecoverExpiredDeliveryClaim(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt.State != task.AttemptUncertain || got.Task.State != task.TaskUncertain || got.Attempt.DeliveryPhase != DeliveryPhasePromptStarted || got.Attempt.DeliveryClaimOwner != nil || got.Attempt.RecoveryReason == nil || *got.Attempt.RecoveryReason != p.Reason || got.Attempt.Revision != claimed.Attempt.Revision+1 || got.Task.Revision != claimed.Task.Revision+1 {
		t.Fatalf("expired recovery: %+v", got)
	}
	assertDeliveryEvents(t, got, "attempt.delivery_claim_expired", "task.uncertain")
	late := RecordAdmissionParams{
		AttemptID: p.AttemptID, LeaseOwner: p.ExpiredLeaseOwner, ExpectedAttemptRevision: 2,
		AttemptEventID: testEventID(1402), TaskEventID: testEventID(1403), Now: p.Now.Add(time.Millisecond),
		EvidencePayload: evidence, EvidenceSHA256: evidenceHash, Actor: testDeliveryActor(),
	}
	if _, err := s.RecordAdmission(context.Background(), late); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("late stale worker = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	t.Cleanup(func() { _ = s.Close() })
	restarted, err := s.GetAttempt(context.Background(), p.AttemptID)
	if err != nil || restarted.State != task.AttemptUncertain || restarted.DeliveryPhase != DeliveryPhasePromptStarted || restarted.DeliveryClaimOwner != nil || restarted.Revision != got.Attempt.Revision {
		t.Fatalf("recovered restart state: %+v, %v", restarted, err)
	}
}

func TestAmbiguousDeliveryOutcomesAlignTaskAndAttempt(t *testing.T) {
	tests := []struct {
		name         string
		attemptState task.AttemptState
		taskState    task.TaskState
		attemptEvent string
		taskEvent    string
		record       func(*Store, RecordDeliveryUncertainParams) (DeliveryTransition, error)
	}{
		{"uncertain", task.AttemptUncertain, task.TaskUncertain, "attempt.delivery_uncertain", "task.uncertain", func(s *Store, p RecordDeliveryUncertainParams) (DeliveryTransition, error) {
			return s.RecordDeliveryUncertain(context.Background(), p)
		}},
		{"recovery required", task.AttemptRecoveryRequired, task.TaskRecoveryRequired, "attempt.delivery_recovery_required", "task.recovery_required", func(s *Store, p RecordDeliveryUncertainParams) (DeliveryTransition, error) {
			return s.RecordDeliveryRecoveryRequired(context.Background(), p)
		}},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, claimed := claimedTestAttempt(t, 106+i)
			evidence, evidenceHash := testEvidence()
			p := RecordDeliveryUncertainParams{
				AttemptID: claimed.Attempt.ID, LeaseOwner: "worker-a", ExpectedAttemptRevision: claimed.Attempt.Revision,
				AttemptEventID: testEventID(1500 + i*2), TaskEventID: testEventID(1501 + i*2),
				Now: testDeliveryTime().Add(10 * time.Millisecond), Reason: "bounded protocol observation was ambiguous",
				EvidencePayload: evidence, EvidenceSHA256: evidenceHash, Actor: testDeliveryActor(),
			}
			got, err := tt.record(s, p)
			if err != nil {
				t.Fatal(err)
			}
			if got.Attempt.State != tt.attemptState || got.Task.State != tt.taskState || got.Attempt.DeliveryPhase != DeliveryPhasePromptStarted || got.Attempt.DeliveryClaimOwner != nil || got.Attempt.RecoveryReason == nil || *got.Attempt.RecoveryReason != p.Reason {
				t.Fatalf("outcome alignment: %+v", got)
			}
			assertDeliveryEvents(t, got, tt.attemptEvent, tt.taskEvent)
			assertEvidenceEvent(t, got.AttemptEvent, evidence, evidenceHash, p.Reason)
		})
	}
}

func TestDeliveryLateFailureRollsBack(t *testing.T) {
	t.Run("claim second event", func(t *testing.T) {
		s := openTestStore(t, testDBPath(t))
		t.Cleanup(func() { _ = s.Close() })
		createTestWorkspace(t, s)
		admitted, err := s.AdmitTask(context.Background(), testAdmission(108, "rollback-claim", "Rollback"))
		if err != nil {
			t.Fatal(err)
		}
		p := testClaim(admitted.Attempt.ID, 1600, "worker-a")
		p.TaskEventID = admitted.TaskEvent.ID
		if _, err := s.ClaimPreparedAttempt(context.Background(), p); err == nil {
			t.Fatal("claim with duplicate late event succeeded")
		}
		attempt, _ := s.GetAttempt(context.Background(), admitted.Attempt.ID)
		owner, _ := s.GetTask(context.Background(), admitted.Task.ID)
		if attempt.State != task.AttemptPrepared || attempt.Revision != 1 || owner.State != task.TaskQueued || owner.Revision != 1 || owner.LatestEventCursor != admitted.AttemptEvent.Cursor {
			t.Fatalf("partial claim: attempt=%+v task=%+v", attempt, owner)
		}
		assertCounts(t, s, 1, 1, 1, 2)
	})

	t.Run("outcome second event", func(t *testing.T) {
		s, claimed := claimedTestAttempt(t, 109)
		evidence, evidenceHash := testEvidence()
		p := RecordAdmissionParams{
			AttemptID: claimed.Attempt.ID, LeaseOwner: "worker-a", ExpectedAttemptRevision: claimed.Attempt.Revision,
			AttemptEventID: testEventID(1610), TaskEventID: claimed.TaskEvent.ID, Now: testDeliveryTime().Add(10 * time.Millisecond),
			EvidencePayload: evidence, EvidenceSHA256: evidenceHash, Actor: testDeliveryActor(),
		}
		if _, err := s.RecordAdmission(context.Background(), p); err == nil {
			t.Fatal("admission with duplicate late event succeeded")
		}
		attempt, _ := s.GetAttempt(context.Background(), claimed.Attempt.ID)
		owner, _ := s.GetTask(context.Background(), claimed.Task.ID)
		if attempt.State != task.AttemptDelivering || attempt.Revision != claimed.Attempt.Revision || attempt.DeliveryClaimOwner == nil || owner.State != task.TaskRunning || owner.Revision != claimed.Task.Revision || owner.LatestEventCursor != claimed.Task.LatestEventCursor {
			t.Fatalf("partial admission: attempt=%+v task=%+v", attempt, owner)
		}
		assertCounts(t, s, 1, 1, 1, 7)
	})
}

func TestDeliveryRejectsInvalidInputsWithoutWrites(t *testing.T) {
	s, claimed := claimedTestAttempt(t, 110)
	evidence, evidenceHash := testEvidence()
	base := RecordDeliveryUncertainParams{
		AttemptID: claimed.Attempt.ID, LeaseOwner: "worker-a", ExpectedAttemptRevision: claimed.Attempt.Revision,
		AttemptEventID: testEventID(1700), TaskEventID: testEventID(1701), Now: testDeliveryTime().Add(10 * time.Millisecond),
		Reason: "ambiguous", EvidencePayload: evidence, EvidenceSHA256: evidenceHash, Actor: testDeliveryActor(),
	}
	tests := []struct {
		name   string
		mutate func(*RecordDeliveryUncertainParams)
	}{
		{"empty reason", func(p *RecordDeliveryUncertainParams) { p.Reason = "" }},
		{"control reason", func(p *RecordDeliveryUncertainParams) { p.Reason = "bad\nreason" }},
		{"oversize reason", func(p *RecordDeliveryUncertainParams) { p.Reason = strings.Repeat("x", 1001) }},
		{"malformed evidence", func(p *RecordDeliveryUncertainParams) { p.EvidencePayload = []byte(`{"broken"`) }},
		{"non-object evidence", func(p *RecordDeliveryUncertainParams) {
			p.EvidencePayload = []byte(`[]`)
			p.EvidenceSHA256 = sha256.Sum256(p.EvidencePayload)
		}},
		{"wrong evidence hash", func(p *RecordDeliveryUncertainParams) { p.EvidenceSHA256 = [32]byte{} }},
		{"raw prompt evidence", func(p *RecordDeliveryUncertainParams) {
			p.EvidencePayload = []byte(`{"prompt":"secret"}`)
			p.EvidenceSHA256 = sha256.Sum256(p.EvidencePayload)
		}},
		{"raw credential evidence", func(p *RecordDeliveryUncertainParams) {
			p.EvidencePayload = []byte(`{"nested":{"credential":"secret"}}`)
			p.EvidenceSHA256 = sha256.Sum256(p.EvidencePayload)
		}},
		{"raw body evidence", func(p *RecordDeliveryUncertainParams) {
			p.EvidencePayload = []byte(`{"response_body":"secret"}`)
			p.EvidenceSHA256 = sha256.Sum256(p.EvidencePayload)
		}},
		{"oversize evidence", func(p *RecordDeliveryUncertainParams) {
			p.EvidencePayload = []byte(`{"value":"` + strings.Repeat("x", maxDeliveryEvidenceBytes) + `"}`)
			p.EvidenceSHA256 = sha256.Sum256(p.EvidencePayload)
		}},
		{"non-millisecond time", func(p *RecordDeliveryUncertainParams) { p.Now = p.Now.Add(time.Nanosecond) }},
		{"same event IDs", func(p *RecordDeliveryUncertainParams) { p.TaskEventID = p.AttemptEventID }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := base
			tt.mutate(&p)
			if _, err := s.RecordDeliveryUncertain(context.Background(), p); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("invalid input error = %v", err)
			}
		})
	}
	attempt, _ := s.GetAttempt(context.Background(), claimed.Attempt.ID)
	owner, _ := s.GetTask(context.Background(), claimed.Task.ID)
	if attempt.State != task.AttemptDelivering || attempt.Revision != claimed.Attempt.Revision || owner.State != task.TaskRunning || owner.Revision != claimed.Task.Revision {
		t.Fatalf("invalid inputs wrote state: attempt=%+v task=%+v", attempt, owner)
	}
	assertCounts(t, s, 1, 1, 1, 7)
}

func TestClaimRespectsLeaseAndAttemptDeadlines(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	admitted, err := s.AdmitTask(context.Background(), testAdmission(112, "delivery-deadline", "Deadline"))
	if err != nil {
		t.Fatal(err)
	}
	claim := testClaim(admitted.Attempt.ID, 1750, "worker-a")
	claim.LeaseExpiresAt = claim.Now.Add(maxDeliveryLease + time.Millisecond)
	if _, err := s.ClaimPreparedAttempt(context.Background(), claim); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized lease error = %v", err)
	}
	claim = testClaim(admitted.Attempt.ID, 1752, "worker-a")
	claim.LeaseExpiresAt = admitted.Attempt.Deadline.Add(time.Millisecond)
	if _, err := s.ClaimPreparedAttempt(context.Background(), claim); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("lease beyond attempt deadline error = %v", err)
	}
	claim = testClaim(admitted.Attempt.ID, 1754, "worker-a")
	claim.Now = admitted.Attempt.Deadline
	claim.LeaseExpiresAt = claim.Now.Add(time.Millisecond)
	if _, err := s.ClaimPreparedAttempt(context.Background(), claim); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expired attempt claim error = %v", err)
	}
	attempt, err := s.GetAttempt(context.Background(), admitted.Attempt.ID)
	if err != nil || attempt.State != task.AttemptPrepared || attempt.Revision != 1 {
		t.Fatalf("deadline rejection changed attempt: %+v, %v", attempt, err)
	}
	assertCounts(t, s, 1, 1, 1, 2)
}

func TestDeliveryContextAndBusyDeadline(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	admitted, err := s.AdmitTask(context.Background(), testAdmission(111, "delivery-context", "Context"))
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ClaimPreparedAttempt(canceled, testClaim(admitted.Attempt.ID, 1800, "worker-a")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled claim = %v", err)
	}
	lock, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	if _, err := s.ClaimPreparedAttempt(ctx, testClaim(admitted.Attempt.ID, 1802, "worker-a")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy claim = %v", err)
	}
}

func TestDeliverySchemaRejectsInvalidClaimShapeAndEventWorkspace(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	admitted, err := s.AdmitTask(context.Background(), testAdmission(112, "shape", "Shape"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE attempts SET delivery_claim_owner='owner',delivery_claim_expires_at=? WHERE id=?`, testDeliveryTime().Add(time.Second).UnixMilli(), admitted.Attempt.ID); err == nil {
		t.Fatal("prepared attempt accepted claim fields")
	}
	if _, err := s.db.Exec(`UPDATE attempts SET state='delivering',delivery_claim_owner='owner',delivery_claim_expires_at=? WHERE id=?`, testDeliveryTime().Add(time.Second).UnixMilli(), admitted.Attempt.ID); err == nil {
		t.Fatal("delivering attempt accepted no delivery_started_at")
	}

	otherWorkspace := Workspace{
		ID: task.WorkspaceID(testID("wsp_", 500)), Name: "other", State: WorkspaceActive,
		RepositoryPath: "/srv/fern/workspaces/other", InstallationID: 124, RepositoryID: 987654322,
		RepositoryFullName: "owner/other", ImageDigest: "sha256:image", OpenCodeProtocol: "v2",
		RuntimeDesiredState: "running", ReconciliationEpoch: 1, CreatedAt: testTime,
	}
	if err := s.CreateWorkspace(context.Background(), otherWorkspace); err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`
INSERT INTO events(id,workspace_id,task_id,attempt_id,entity_type,entity_id,type,version,occurred_at,actor_snapshot_id,payload)
SELECT ?,?,task_id,id,'attempt',id,'attempt.delivery_started',1,created_at,1,'{}' FROM attempts WHERE id=?`,
		testEventID(1900), otherWorkspace.ID, admitted.Attempt.ID)
	if err == nil {
		t.Fatal("event with mismatched workspace ownership was accepted")
	}
}

func claimedTestAttempt(t *testing.T, n int) (*Store, DeliveryTransition) {
	t.Helper()
	return claimedTestAttemptAtPath(t, testDBPath(t), n)
}

func claimedTestAttemptAtPath(t *testing.T, path string, n int) (*Store, DeliveryTransition) {
	t.Helper()
	s := openTestStore(t, path)
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	admitted, err := s.AdmitTask(context.Background(), testAdmission(n, "claim-helper-"+strconv.Itoa(n), "Deliver"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimPreparedAttempt(context.Background(), testClaim(admitted.Attempt.ID, 2000+n*2, "worker-a"))
	if err != nil {
		t.Fatal(err)
	}
	return s, advanceTestDeliveryToPrompt(t, s, claimed, "worker-a", 12000+n*4)
}

func advanceTestDeliveryToPrompt(t *testing.T, s *Store, claimed DeliveryTransition, owner string, eventN int) DeliveryTransition {
	t.Helper()
	phases := []DeliveryPhase{DeliveryPhaseSessionCreateStarted, DeliveryPhaseSessionReady, DeliveryPhasePromptStarted}
	from := DeliveryPhaseClaimed
	for i, to := range phases {
		advanced, err := s.AdvanceDeliveryPhase(context.Background(), AdvanceDeliveryPhaseParams{
			AttemptID: claimed.Attempt.ID, LeaseOwner: owner, ExpectedAttemptRevision: claimed.Attempt.Revision,
			From: from, To: to, EventID: testEventID(eventN + i), Now: testDeliveryTime().Add(time.Duration(i+1) * time.Millisecond), Actor: testDeliveryActor(),
		})
		if err != nil {
			t.Fatal(err)
		}
		claimed.Task, claimed.Attempt = advanced.Task, advanced.Attempt
		from = to
	}
	return claimed
}

func testClaim(attemptID task.AttemptID, eventN int, owner string) ClaimPreparedAttemptParams {
	now := testDeliveryTime()
	return ClaimPreparedAttemptParams{
		AttemptID: attemptID, LeaseOwner: owner, ClaimEventID: testEventID(eventN), TaskEventID: testEventID(eventN + 1),
		Now: now, LeaseExpiresAt: now.Add(500 * time.Millisecond), Actor: testDeliveryActor(),
	}
}

func testDeliveryTime() time.Time { return testTime.Truncate(time.Millisecond).Add(time.Second) }

func testDeliveryActor() task.ActorSnapshot {
	return task.ActorSnapshot{Type: task.ActorSystem, ID: "delivery-coordinator", DisplayName: "Delivery coordinator", CredentialID: "service-v1", Authentication: "internal", RequestID: "delivery-request"}
}

func testRecoveryActor() task.ActorSnapshot {
	return task.ActorSnapshot{Type: task.ActorRecovery, ID: "startup-reconciler", DisplayName: "Startup reconciler", CredentialID: "service-v1", Authentication: "internal", RequestID: "recovery-request"}
}

func testEvidence() (json.RawMessage, [32]byte) {
	evidence := json.RawMessage(`{ "httpStatus": 200, "messageIdMatched": true }`)
	return evidence, sha256.Sum256(evidence)
}

func assertDeliveryEvents(t *testing.T, got DeliveryTransition, attemptType, taskType string) {
	t.Helper()
	if got.AttemptEvent.Type != attemptType || got.TaskEvent.Type != taskType || got.AttemptEvent.Cursor >= got.TaskEvent.Cursor || got.AttemptEvent.TaskID != got.Task.ID || got.AttemptEvent.AttemptID != got.Attempt.ID || got.TaskEvent.TaskID != got.Task.ID || got.TaskEvent.AttemptID != "" || got.AttemptEvent.WorkspaceID != got.Task.WorkspaceID || got.TaskEvent.WorkspaceID != got.Task.WorkspaceID {
		t.Fatalf("delivery event ordering/ownership: %+v", got)
	}
	if got.Task.LatestEventCursor != got.TaskEvent.Cursor {
		t.Fatalf("latest cursor %d, want %d", got.Task.LatestEventCursor, got.TaskEvent.Cursor)
	}
}

func assertEvidenceEvent(t *testing.T, event Event, evidence json.RawMessage, hash [32]byte, reason string) {
	t.Helper()
	var payload struct {
		Reason         string          `json:"reason"`
		Evidence       json.RawMessage `json:"evidence"`
		EvidenceSHA256 string          `json:"evidenceSha256"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload.Evidence) != string(evidence) || payload.Reason != reason || payload.EvidenceSHA256 != "sha256:"+hex.EncodeToString(hash[:]) {
		t.Fatalf("evidence payload: %s", event.Payload)
	}
	if !strings.Contains(string(event.Payload), `"evidence":`+string(evidence)) {
		t.Fatalf("exact evidence bytes not retained: %s", event.Payload)
	}
}
