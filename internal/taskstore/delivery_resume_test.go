package taskstore

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

func TestResumeUncertainPrePromptDeliveryEachPhaseAcrossRestart(t *testing.T) {
	tests := []struct {
		phase DeliveryPhase
		next  DeliveryPhase
	}{
		{DeliveryPhaseClaimed, DeliveryPhaseSessionCreateStarted},
		{DeliveryPhaseSessionCreateStarted, DeliveryPhaseSessionReady},
		{DeliveryPhaseSessionReady, DeliveryPhasePromptStarted},
	}
	for i, tt := range tests {
		t.Run(string(tt.phase), func(t *testing.T) {
			path := testDBPath(t)
			s, uncertain := uncertainPrePromptTestAttemptAtPath(t, path, 600+i, tt.phase)
			startedAt := *uncertain.Attempt.DeliveryStartedAt
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openTestStore(t, path)
			t.Cleanup(func() { _ = s.Close() })

			p := testDeliveryResume(uncertain, 33000+i*10)
			got, err := s.ResumeUncertainPrePromptDelivery(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			if got.Attempt.State != task.AttemptDelivering || got.Task.State != task.TaskRunning || got.Attempt.DeliveryPhase != tt.phase || got.Attempt.DeliveryClaimOwner == nil || *got.Attempt.DeliveryClaimOwner != p.LeaseOwner || got.Attempt.DeliveryClaimExpiresAt == nil || !got.Attempt.DeliveryClaimExpiresAt.Equal(p.LeaseExpiresAt) || got.Attempt.DeliveryStartedAt == nil || !got.Attempt.DeliveryStartedAt.Equal(startedAt) || got.Attempt.RecoveryReason != nil || got.Attempt.Revision != uncertain.Attempt.Revision+1 || got.Task.Revision != uncertain.Task.Revision+1 {
				t.Fatalf("resumed delivery: %+v", got)
			}
			assertDeliveryEvents(t, got, "attempt.delivery_resumed", "task.running")
			assertResumePayload(t, got.AttemptEvent, p)

			advanced, err := s.AdvanceDeliveryPhase(context.Background(), AdvanceDeliveryPhaseParams{
				AttemptID: got.Attempt.ID, LeaseOwner: p.LeaseOwner, ExpectedAttemptRevision: got.Attempt.Revision,
				From: tt.phase, To: tt.next, EventID: testEventID(33002 + i*10), Now: p.Now.Add(time.Millisecond), Actor: testDeliveryActor(),
			})
			if err != nil || advanced.Attempt.DeliveryPhase != tt.next {
				t.Fatalf("continue monotonic delivery: %+v, %v", advanced, err)
			}
		})
	}
}

func TestResumeUncertainPrePromptDeliveryRejectionsAndRollback(t *testing.T) {
	t.Run("prompt started", func(t *testing.T) {
		s, uncertain := uncertainPrePromptTestAttempt(t, 610, DeliveryPhasePromptStarted)
		p := testDeliveryResume(uncertain, 33100)
		p.ExpectedPhase = DeliveryPhasePromptStarted
		if _, err := s.ResumeUncertainPrePromptDelivery(context.Background(), p); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("prompt phase = %v", err)
		}
	})

	t.Run("none and mismatch", func(t *testing.T) {
		s := openTestStore(t, testDBPath(t))
		t.Cleanup(func() { _ = s.Close() })
		createTestWorkspace(t, s)
		admission, err := s.AdmitTask(context.Background(), testAdmission(611, "resume-none", "None"))
		if err != nil {
			t.Fatal(err)
		}
		now := testDeliveryTime().UnixMilli()
		if _, err := s.db.Exec(`UPDATE tasks SET state='uncertain',revision=revision+1,updated_at=? WHERE id=?`, now, admission.Task.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE attempts SET state='uncertain',revision=revision+1,updated_at=? WHERE id=?`, now, admission.Attempt.ID); err != nil {
			t.Fatal(err)
		}
		owner, _ := s.GetTask(context.Background(), admission.Task.ID)
		attempt, _ := s.GetAttempt(context.Background(), admission.Attempt.ID)
		fixture := DeliveryTransition{Task: owner, Attempt: attempt}
		p := testDeliveryResume(fixture, 33110)
		p.ExpectedPhase = DeliveryPhaseClaimed
		if _, err := s.ResumeUncertainPrePromptDelivery(context.Background(), p); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("none phase = %v", err)
		}
	})

	t.Run("phase mismatch", func(t *testing.T) {
		s, uncertain := uncertainPrePromptTestAttempt(t, 612, DeliveryPhaseSessionCreateStarted)
		p := testDeliveryResume(uncertain, 33120)
		p.ExpectedPhase = DeliveryPhaseSessionReady
		if _, err := s.ResumeUncertainPrePromptDelivery(context.Background(), p); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("phase mismatch = %v", err)
		}
	})

	t.Run("owner revisions actor evidence", func(t *testing.T) {
		s, uncertain := uncertainPrePromptTestAttempt(t, 613, DeliveryPhaseSessionReady)
		base := testDeliveryResume(uncertain, 33130)
		cases := []struct {
			name   string
			mutate func(*ResumeUncertainPrePromptDeliveryParams)
			want   error
		}{
			{"owner", func(p *ResumeUncertainPrePromptDeliveryParams) { p.LeaseOwner = "" }, ErrInvalidInput},
			{"attempt revision", func(p *ResumeUncertainPrePromptDeliveryParams) { p.ExpectedAttemptRevision-- }, ErrStaleRevision},
			{"task revision", func(p *ResumeUncertainPrePromptDeliveryParams) { p.ExpectedTaskRevision-- }, ErrStaleRevision},
			{"actor", func(p *ResumeUncertainPrePromptDeliveryParams) { p.Actor = testDeliveryActor() }, ErrInvalidInput},
			{"evidence", func(p *ResumeUncertainPrePromptDeliveryParams) { p.EvidenceSHA256 = [32]byte{} }, ErrInvalidInput},
			{"sensitive evidence", func(p *ResumeUncertainPrePromptDeliveryParams) {
				p.EvidencePayload = json.RawMessage(`{"credential":"secret"}`)
				p.EvidenceSHA256 = sha256Sum(p.EvidencePayload)
			}, ErrInvalidInput},
		}
		for _, tt := range cases {
			t.Run(tt.name, func(t *testing.T) {
				p := base
				tt.mutate(&p)
				if _, err := s.ResumeUncertainPrePromptDelivery(context.Background(), p); !errors.Is(err, tt.want) {
					t.Fatalf("rejection = %v", err)
				}
			})
		}
	})

	t.Run("deadline and lease", func(t *testing.T) {
		s, uncertain := uncertainPrePromptTestAttempt(t, 614, DeliveryPhaseClaimed)
		deadline := testDeliveryResume(uncertain, 33140)
		deadline.Now = uncertain.Attempt.Deadline
		deadline.LeaseExpiresAt = deadline.Now.Add(time.Millisecond)
		if _, err := s.ResumeUncertainPrePromptDelivery(context.Background(), deadline); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("deadline = %v", err)
		}
		tooLong := testDeliveryResume(uncertain, 33142)
		tooLong.LeaseExpiresAt = tooLong.Now.Add(maxDeliveryLease + time.Millisecond)
		if _, err := s.ResumeUncertainPrePromptDelivery(context.Background(), tooLong); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("long lease = %v", err)
		}
		pastDeadline := testDeliveryResume(uncertain, 33144)
		pastDeadline.LeaseExpiresAt = uncertain.Attempt.Deadline.Add(time.Millisecond)
		if _, err := s.ResumeUncertainPrePromptDelivery(context.Background(), pastDeadline); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("lease past deadline = %v", err)
		}
	})

	t.Run("late event rollback", func(t *testing.T) {
		s, uncertain := uncertainPrePromptTestAttempt(t, 615, DeliveryPhaseSessionReady)
		beforeEvents := eventCount(t, s)
		p := testDeliveryResume(uncertain, 33150)
		p.TaskEventID = uncertain.TaskEvent.ID
		if _, err := s.ResumeUncertainPrePromptDelivery(context.Background(), p); err == nil {
			t.Fatal("duplicate event resume succeeded")
		}
		stored, _ := s.GetAttempt(context.Background(), uncertain.Attempt.ID)
		owner, _ := s.GetTask(context.Background(), uncertain.Task.ID)
		if stored.State != task.AttemptUncertain || stored.DeliveryClaimOwner != nil || stored.Revision != uncertain.Attempt.Revision || owner.State != task.TaskUncertain || owner.Revision != uncertain.Task.Revision || eventCount(t, s) != beforeEvents {
			t.Fatalf("resume rollback: attempt=%+v task=%+v", stored, owner)
		}
	})
}

func TestResumeUncertainPrePromptDeliveryRaces(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		for run := 0; run < 10; run++ {
			s, uncertain := uncertainPrePromptTestAttempt(t, 620+run, DeliveryPhaseSessionReady)
			resume := testDeliveryResume(uncertain, 34000+run*4)
			cancel := testCancellation(uncertain.Task.ID, 620+run, "resume-cancel-race", "stop")
			start := make(chan struct{})
			var resumed DeliveryTransition
			var cancellation Cancellation
			var resumeErr, cancelErr error
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				resumed, resumeErr = s.ResumeUncertainPrePromptDelivery(context.Background(), resume)
			}()
			go func() {
				defer wg.Done()
				<-start
				cancellation, cancelErr = s.RequestCancellation(context.Background(), cancel)
			}()
			close(start)
			wg.Wait()
			if cancelErr != nil {
				t.Fatalf("cancel = %v", cancelErr)
			}
			if resumeErr == nil {
				if resumed.Attempt.State != task.AttemptDelivering || cancellation.Disposition != CancellationEffectReconcileDelivery {
					t.Fatalf("resume winner: resumed=%+v cancel=%+v", resumed, cancellation)
				}
			} else if !errors.Is(resumeErr, ErrStaleRevision) || cancellation.Disposition != CancellationEffectInterrupt {
				t.Fatalf("cancel winner: resume=%v cancel=%+v", resumeErr, cancellation)
			}
			if cancellation.Attempt.State != task.AttemptCancelRequested || cancellation.Attempt.DeliveryPhase != DeliveryPhaseSessionReady || cancellation.Attempt.DeliveryClaimOwner != nil {
				t.Fatalf("race final cancellation: %+v", cancellation)
			}
			_ = s.Close()
		}
	})

	t.Run("resolution", func(t *testing.T) {
		for run := 0; run < 10; run++ {
			s, uncertain := uncertainPrePromptTestAttempt(t, 640+run, DeliveryPhaseSessionCreateStarted)
			resume := testDeliveryResume(uncertain, 35000+run*4)
			resolve := testUncertainResolution(uncertain, ResolveUncertainDeliveryRecoveryRequired, 35002+run*4, "reconciliation refused resume")
			start := make(chan struct{})
			var resumeErr, resolveErr error
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, resumeErr = s.ResumeUncertainPrePromptDelivery(context.Background(), resume)
			}()
			go func() {
				defer wg.Done()
				<-start
				_, resolveErr = s.ResolveUncertainDelivery(context.Background(), resolve)
			}()
			close(start)
			wg.Wait()
			if (resumeErr == nil) == (resolveErr == nil) {
				t.Fatalf("resume=%v resolve=%v", resumeErr, resolveErr)
			}
			if resumeErr != nil && !errors.Is(resumeErr, ErrStaleRevision) || resolveErr != nil && !errors.Is(resolveErr, ErrStaleRevision) {
				t.Fatalf("race losers: resume=%v resolve=%v", resumeErr, resolveErr)
			}
			stored, _ := s.GetAttempt(context.Background(), uncertain.Attempt.ID)
			if stored.DeliveryPhase != DeliveryPhaseSessionCreateStarted || stored.State != task.AttemptDelivering && stored.State != task.AttemptRecoveryRequired {
				t.Fatalf("resolution race state: %+v", stored)
			}
			_ = s.Close()
		}
	})

	t.Run("concurrent resumes", func(t *testing.T) {
		s, uncertain := uncertainPrePromptTestAttempt(t, 660, DeliveryPhaseClaimed)
		p := testDeliveryResume(uncertain, 36000)
		start := make(chan struct{})
		errs := make(chan error, 12)
		var wg sync.WaitGroup
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := s.ResumeUncertainPrePromptDelivery(context.Background(), p)
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
			} else if !errors.Is(err, ErrStaleRevision) {
				t.Errorf("resume loser = %v", err)
			}
		}
		if wins != 1 {
			t.Fatalf("resume winners = %d", wins)
		}
	})
}

func TestResumeUncertainPrePromptDeliverySchemaAndContext(t *testing.T) {
	t.Run("direct SQL", func(t *testing.T) {
		s, uncertain := uncertainPrePromptTestAttempt(t, 670, DeliveryPhaseSessionReady)
		expires := testDeliveryTime().Add(time.Second).UnixMilli()
		now := testDeliveryTime().Add(20 * time.Millisecond).UnixMilli()
		if _, err := s.db.Exec(`UPDATE attempts SET delivery_claim_owner='stale',delivery_claim_expires_at=? WHERE id=?`, expires, uncertain.Attempt.ID); err == nil {
			t.Fatal("uncertain attempt accepted an existing claim")
		}
		if _, err := s.db.Exec(`UPDATE attempts SET state='delivering',delivery_claim_owner='direct',delivery_claim_expires_at=?,revision=revision+1,updated_at=? WHERE id=?`, expires, now, uncertain.Attempt.ID); err == nil {
			t.Fatal("direct uncertain resume without event succeeded")
		}
		stored, _ := s.GetAttempt(context.Background(), uncertain.Attempt.ID)
		if stored.State != task.AttemptUncertain || stored.DeliveryClaimOwner != nil {
			t.Fatalf("direct SQL changed attempt: %+v", stored)
		}
	})

	t.Run("prompt SQL", func(t *testing.T) {
		s, uncertain := uncertainPrePromptTestAttempt(t, 671, DeliveryPhasePromptStarted)
		expires := testDeliveryTime().Add(time.Second).UnixMilli()
		now := testDeliveryTime().Add(20 * time.Millisecond).UnixMilli()
		if _, err := s.db.Exec(`UPDATE attempts SET state='delivering',delivery_claim_owner='direct',delivery_claim_expires_at=?,revision=revision+1,updated_at=? WHERE id=?`, expires, now, uncertain.Attempt.ID); err == nil {
			t.Fatal("prompt-started uncertain resume succeeded")
		}
	})

	t.Run("context", func(t *testing.T) {
		s, uncertain := uncertainPrePromptTestAttempt(t, 672, DeliveryPhaseClaimed)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := s.ResumeUncertainPrePromptDelivery(ctx, testDeliveryResume(uncertain, 36100)); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled resume = %v", err)
		}
	})
}

func uncertainPrePromptTestAttempt(t *testing.T, n int, phase DeliveryPhase) (*Store, DeliveryTransition) {
	t.Helper()
	return uncertainPrePromptTestAttemptAtPath(t, testDBPath(t), n, phase)
}

func uncertainPrePromptTestAttemptAtPath(t *testing.T, path string, n int, phase DeliveryPhase) (*Store, DeliveryTransition) {
	t.Helper()
	s := openTestStore(t, path)
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	admission, err := s.AdmitTask(context.Background(), testAdmission(n, "resume-fixture", "Resume fixture"))
	if err != nil {
		t.Fatal(err)
	}
	claimed := claimTestAttemptAtPhase(t, s, admission, phase, 37000+n*10)
	evidence, evidenceHash := testEvidence()
	uncertain, err := s.RecordDeliveryUncertain(context.Background(), RecordDeliveryUncertainParams{
		AttemptID: claimed.Attempt.ID, LeaseOwner: "phase-owner", ExpectedAttemptRevision: claimed.Attempt.Revision,
		AttemptEventID: testEventID(38000 + n*2), TaskEventID: testEventID(38001 + n*2), Now: testDeliveryTime().Add(10 * time.Millisecond),
		Reason: "restart requires read-only reconciliation", EvidencePayload: evidence, EvidenceSHA256: evidenceHash, Actor: testRecoveryActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, uncertain
}

func testDeliveryResume(uncertain DeliveryTransition, eventN int) ResumeUncertainPrePromptDeliveryParams {
	evidence, evidenceHash := testEvidence()
	now := testDeliveryTime().Add(20 * time.Millisecond)
	return ResumeUncertainPrePromptDeliveryParams{
		AttemptID: uncertain.Attempt.ID, ExpectedAttemptRevision: uncertain.Attempt.Revision, ExpectedTaskRevision: uncertain.Task.Revision,
		ExpectedPhase: uncertain.Attempt.DeliveryPhase, LeaseOwner: "resumed-owner", LeaseExpiresAt: now.Add(time.Second),
		AttemptEventID: testEventID(eventN), TaskEventID: testEventID(eventN + 1), Now: now,
		EvidencePayload: evidence, EvidenceSHA256: evidenceHash, Actor: testRecoveryActor(),
	}
}

func assertResumePayload(t *testing.T, event Event, p ResumeUncertainPrePromptDeliveryParams) {
	t.Helper()
	var payload struct {
		AttemptID            task.AttemptID  `json:"attemptId"`
		AttemptRevision      int64           `json:"expectedAttemptRevision"`
		TaskRevision         int64           `json:"expectedTaskRevision"`
		Phase                DeliveryPhase   `json:"phase"`
		LeaseOwner           string          `json:"leaseOwner"`
		LeaseExpiresAtMillis int64           `json:"leaseExpiresAtMillis"`
		Evidence             json.RawMessage `json:"evidence"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.AttemptID != p.AttemptID || payload.AttemptRevision != p.ExpectedAttemptRevision || payload.TaskRevision != p.ExpectedTaskRevision || payload.Phase != p.ExpectedPhase || payload.LeaseOwner != p.LeaseOwner || payload.LeaseExpiresAtMillis != p.LeaseExpiresAt.UnixMilli() || string(payload.Evidence) != string(p.EvidencePayload) {
		t.Fatalf("resume payload = %s, %v", event.Payload, err)
	}
}
