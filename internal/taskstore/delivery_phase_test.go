package taskstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

func TestDeliveryPhaseProgressionPersistsEveryCrashBoundary(t *testing.T) {
	phases := []DeliveryPhase{
		DeliveryPhaseClaimed,
		DeliveryPhaseSessionCreateStarted,
		DeliveryPhaseSessionReady,
		DeliveryPhasePromptStarted,
	}
	for i, want := range phases {
		t.Run(string(want), func(t *testing.T) {
			path := testDBPath(t)
			s := openTestStore(t, path)
			createTestWorkspace(t, s)
			admission, err := s.AdmitTask(context.Background(), testAdmission(400+i, "phase-restart-"+string(want), "Phase restart"))
			if err != nil {
				t.Fatal(err)
			}
			if admission.Attempt.DeliveryPhase != DeliveryPhaseNone {
				t.Fatalf("prepared phase = %s", admission.Attempt.DeliveryPhase)
			}
			current := claimTestAttemptAtPhase(t, s, admission, want, 20000+i*20)
			wantRevision := int64(2 + i)
			if current.Attempt.Revision != wantRevision || current.Task.Revision != wantRevision || current.Task.LatestEventCursor <= admission.AttemptEvent.Cursor {
				t.Fatalf("phase revisions/cursor: %+v", current)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openTestStore(t, path)
			t.Cleanup(func() { _ = s.Close() })
			restarted, err := s.FindAmbiguousDeliveryAttempt(context.Background(), testWorkspaceID())
			if err != nil || restarted.Attempt.DeliveryPhase != want || restarted.Attempt.Revision != wantRevision || restarted.Task.Revision != wantRevision || restarted.Task.LatestEventCursor != current.Task.LatestEventCursor {
				t.Fatalf("restarted phase: %+v, %v", restarted, err)
			}
		})
	}
}

func TestAdvanceDeliveryPhaseEventsRevisionsAndRejections(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	admission, err := s.AdmitTask(context.Background(), testAdmission(410, "phase-progress", "Progress"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimPreparedAttempt(context.Background(), testClaim(admission.Attempt.ID, 21000, "phase-owner"))
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Attempt.DeliveryPhase != DeliveryPhaseClaimed {
		t.Fatalf("claim phase = %s", claimed.Attempt.DeliveryPhase)
	}
	evidence, evidenceHash := testEvidence()
	if _, err := s.RecordAdmission(context.Background(), RecordAdmissionParams{
		AttemptID: claimed.Attempt.ID, LeaseOwner: "phase-owner", ExpectedAttemptRevision: claimed.Attempt.Revision,
		AttemptEventID: testEventID(21004), TaskEventID: testEventID(21005), Now: testDeliveryTime().Add(time.Millisecond),
		EvidencePayload: evidence, EvidenceSHA256: evidenceHash, Actor: testDeliveryActor(),
	}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("admission before prompt = %v", err)
	}

	base := AdvanceDeliveryPhaseParams{
		AttemptID: claimed.Attempt.ID, LeaseOwner: "phase-owner", ExpectedAttemptRevision: claimed.Attempt.Revision,
		From: DeliveryPhaseClaimed, To: DeliveryPhaseSessionCreateStarted, EventID: testEventID(21002),
		Now: testDeliveryTime().Add(time.Millisecond), Actor: testDeliveryActor(),
	}
	for name, mutate := range map[string]func(*AdvanceDeliveryPhaseParams){
		"skip": func(p *AdvanceDeliveryPhaseParams) { p.To = DeliveryPhaseSessionReady },
		"backward": func(p *AdvanceDeliveryPhaseParams) {
			p.From, p.To = DeliveryPhaseSessionReady, DeliveryPhaseSessionCreateStarted
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := base
			mutate(&p)
			if _, err := s.AdvanceDeliveryPhase(context.Background(), p); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("rejection = %v", err)
			}
		})
	}
	wrongOwner := base
	wrongOwner.LeaseOwner = "other-owner"
	if _, err := s.AdvanceDeliveryPhase(context.Background(), wrongOwner); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("wrong owner = %v", err)
	}
	stale := base
	stale.ExpectedAttemptRevision--
	if _, err := s.AdvanceDeliveryPhase(context.Background(), stale); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale revision = %v", err)
	}

	advanced, err := s.AdvanceDeliveryPhase(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if advanced.Attempt.DeliveryPhase != base.To || advanced.Attempt.Revision != claimed.Attempt.Revision+1 || advanced.Task.Revision != claimed.Task.Revision+1 || advanced.Task.LatestEventCursor != advanced.Event.Cursor || advanced.Event.Type != "attempt.delivery_phase_advanced" || advanced.Event.AttemptID != claimed.Attempt.ID {
		t.Fatalf("phase advance projection: %+v", advanced)
	}
	var payload struct {
		From DeliveryPhase `json:"from"`
		To   DeliveryPhase `json:"to"`
	}
	if err := json.Unmarshal(advanced.Event.Payload, &payload); err != nil || payload.From != base.From || payload.To != base.To {
		t.Fatalf("phase payload = %s, %v", advanced.Event.Payload, err)
	}
	if _, err := s.db.Exec(`UPDATE events SET payload='{}' WHERE id=?`, advanced.Event.ID); err == nil {
		t.Fatal("phase event was mutable")
	}
	if _, err := s.AdvanceDeliveryPhase(context.Background(), base); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("duplicate advance = %v", err)
	}
	duplicateFrom := base
	duplicateFrom.ExpectedAttemptRevision = advanced.Attempt.Revision
	duplicateFrom.EventID = testEventID(21003)
	if _, err := s.AdvanceDeliveryPhase(context.Background(), duplicateFrom); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("duplicate current revision = %v", err)
	}
}

func TestAdvanceDeliveryPhaseRejectsExpiryDeadlineAndRollsBackLateEvent(t *testing.T) {
	t.Run("expired lease", func(t *testing.T) {
		s, claimed := claimedAtInitialPhase(t, 420, DeliveryPhaseClaimed)
		p := testPhaseAdvance(claimed, DeliveryPhaseClaimed, DeliveryPhaseSessionCreateStarted, 21100, testDeliveryTime().Add(500*time.Millisecond))
		if _, err := s.AdvanceDeliveryPhase(context.Background(), p); !errors.Is(err, ErrLeaseConflict) {
			t.Fatalf("expired lease = %v", err)
		}
	})
	t.Run("attempt deadline", func(t *testing.T) {
		s := openTestStore(t, testDBPath(t))
		t.Cleanup(func() { _ = s.Close() })
		createTestWorkspace(t, s)
		p := testAdmission(421, "phase-deadline", "Deadline")
		p.Deadline = testDeliveryTime().Add(400 * time.Millisecond)
		admission, err := s.AdmitTask(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		claim := testClaim(admission.Attempt.ID, 21110, "phase-owner")
		claim.LeaseExpiresAt = p.Deadline
		claimed, err := s.ClaimPreparedAttempt(context.Background(), claim)
		if err != nil {
			t.Fatal(err)
		}
		advance := testPhaseAdvance(claimed, DeliveryPhaseClaimed, DeliveryPhaseSessionCreateStarted, 21112, p.Deadline)
		if _, err := s.AdvanceDeliveryPhase(context.Background(), advance); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("deadline = %v", err)
		}
	})
	t.Run("late event", func(t *testing.T) {
		s, claimed := claimedAtInitialPhase(t, 422, DeliveryPhaseClaimed)
		beforeEvents := eventCount(t, s)
		p := testPhaseAdvance(claimed, DeliveryPhaseClaimed, DeliveryPhaseSessionCreateStarted, 21120, testDeliveryTime().Add(time.Millisecond))
		p.EventID = claimed.TaskEvent.ID
		if _, err := s.AdvanceDeliveryPhase(context.Background(), p); err == nil {
			t.Fatal("duplicate event advance succeeded")
		}
		stored, _ := s.GetAttempt(context.Background(), claimed.Attempt.ID)
		owner, _ := s.GetTask(context.Background(), claimed.Task.ID)
		if stored.DeliveryPhase != DeliveryPhaseClaimed || stored.Revision != claimed.Attempt.Revision || owner.Revision != claimed.Task.Revision || owner.LatestEventCursor != claimed.Task.LatestEventCursor || eventCount(t, s) != beforeEvents {
			t.Fatalf("late-event partial write: attempt=%+v task=%+v", stored, owner)
		}
	})
}

func TestRecoverExpiredDeliveryClaimRetainsEveryPhaseAndFencesStaleOwner(t *testing.T) {
	phases := []DeliveryPhase{DeliveryPhaseClaimed, DeliveryPhaseSessionCreateStarted, DeliveryPhaseSessionReady, DeliveryPhasePromptStarted}
	for i, phase := range phases {
		t.Run(string(phase), func(t *testing.T) {
			s, claimed := claimedAtInitialPhase(t, 430+i, phase)
			evidence, evidenceHash := testEvidence()
			got, err := s.RecoverExpiredDeliveryClaim(context.Background(), RecoverExpiredDeliveryClaimParams{
				AttemptID: claimed.Attempt.ID, ExpiredLeaseOwner: "phase-owner", ExpectedAttemptRevision: claimed.Attempt.Revision,
				RecoveryEventID: testEventID(23000 + i*4), TaskEventID: testEventID(23001 + i*4), Now: testDeliveryTime().Add(time.Second),
				Reason: "worker stopped before durable observation", EvidencePayload: evidence, EvidenceSHA256: evidenceHash, Actor: testRecoveryActor(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.Attempt.State != task.AttemptUncertain || got.Task.State != task.TaskUncertain || got.Attempt.DeliveryPhase != phase || got.Attempt.DeliveryClaimOwner != nil || got.Attempt.Revision != claimed.Attempt.Revision+1 || got.Task.Revision != claimed.Task.Revision+1 {
				t.Fatalf("expired phase recovery: %+v", got)
			}
			late := RecordDeliveryUncertainParams{
				AttemptID: claimed.Attempt.ID, LeaseOwner: "phase-owner", ExpectedAttemptRevision: claimed.Attempt.Revision,
				AttemptEventID: testEventID(23002 + i*4), TaskEventID: testEventID(23003 + i*4), Now: testDeliveryTime().Add(time.Second + time.Millisecond),
				Reason: "late stale owner", EvidencePayload: evidence, EvidenceSHA256: evidenceHash, Actor: testDeliveryActor(),
			}
			if _, err := s.RecordDeliveryUncertain(context.Background(), late); !errors.Is(err, ErrLeaseConflict) {
				t.Fatalf("stale owner = %v", err)
			}
		})
	}
}

func TestResolveUncertainDeliveryOutcomesAndAmbiguousLookup(t *testing.T) {
	t.Run("admitted", func(t *testing.T) {
		s, uncertain := uncertainTestAttempt(t, 440, DeliveryPhasePromptStarted)
		found, err := s.FindAmbiguousDeliveryAttempt(context.Background(), testWorkspaceID())
		if err != nil || found.Attempt.ID != uncertain.Attempt.ID || found.Attempt.State != task.AttemptUncertain {
			t.Fatalf("uncertain lookup: %+v, %v", found, err)
		}
		p := testUncertainResolution(uncertain, ResolveUncertainDeliveryAdmitted, 24000, "")
		got, err := s.ResolveUncertainDelivery(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		if got.Attempt.State != task.AttemptAdmitted || got.Task.State != task.TaskRunning || got.Attempt.DeliveryPhase != DeliveryPhasePromptStarted || got.Attempt.AdmittedAt == nil || !got.Attempt.AdmittedAt.Equal(p.Now) || got.Attempt.RecoveryReason != nil || got.Attempt.Revision != uncertain.Attempt.Revision+1 || got.Task.Revision != uncertain.Task.Revision+1 {
			t.Fatalf("admitted resolution: %+v", got)
		}
		assertResolutionPayload(t, got.AttemptEvent, ResolveUncertainDeliveryAdmitted, DeliveryPhasePromptStarted)
		if _, err := s.ResolveUncertainDelivery(context.Background(), p); !errors.Is(err, ErrStaleRevision) {
			t.Fatalf("repeated resolution = %v", err)
		}
		if _, err := s.FindAmbiguousDeliveryAttempt(context.Background(), testWorkspaceID()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("resolved lookup = %v", err)
		}
	})

	t.Run("recovery required", func(t *testing.T) {
		s, uncertain := uncertainTestAttempt(t, 441, DeliveryPhaseSessionReady)
		p := testUncertainResolution(uncertain, ResolveUncertainDeliveryRecoveryRequired, 24010, "bounded reconciliation exhausted")
		got, err := s.ResolveUncertainDelivery(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		if got.Attempt.State != task.AttemptRecoveryRequired || got.Task.State != task.TaskRecoveryRequired || got.Attempt.DeliveryPhase != DeliveryPhaseSessionReady || got.Attempt.RecoveryReason == nil || *got.Attempt.RecoveryReason != p.Reason || got.Attempt.AdmittedAt != nil {
			t.Fatalf("recovery resolution: %+v", got)
		}
		assertResolutionPayload(t, got.AttemptEvent, ResolveUncertainDeliveryRecoveryRequired, DeliveryPhaseSessionReady)
	})

	t.Run("admission requires prompt started", func(t *testing.T) {
		s, uncertain := uncertainTestAttempt(t, 442, DeliveryPhaseSessionCreateStarted)
		p := testUncertainResolution(uncertain, ResolveUncertainDeliveryAdmitted, 24020, "")
		if _, err := s.ResolveUncertainDelivery(context.Background(), p); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("early admitted resolution = %v", err)
		}
		stored, _ := s.GetAttempt(context.Background(), uncertain.Attempt.ID)
		if stored.State != task.AttemptUncertain || stored.DeliveryPhase != DeliveryPhaseSessionCreateStarted || stored.Revision != uncertain.Attempt.Revision {
			t.Fatalf("invalid resolution wrote: %+v", stored)
		}
	})

	t.Run("exact revisions", func(t *testing.T) {
		s, uncertain := uncertainTestAttempt(t, 443, DeliveryPhasePromptStarted)
		base := testUncertainResolution(uncertain, ResolveUncertainDeliveryAdmitted, 24030, "")
		staleAttempt := base
		staleAttempt.ExpectedAttemptRevision--
		if _, err := s.ResolveUncertainDelivery(context.Background(), staleAttempt); !errors.Is(err, ErrStaleRevision) {
			t.Fatalf("attempt revision = %v", err)
		}
		staleTask := base
		staleTask.ExpectedTaskRevision--
		if _, err := s.ResolveUncertainDelivery(context.Background(), staleTask); !errors.Is(err, ErrStaleRevision) {
			t.Fatalf("task revision = %v", err)
		}
		base.TaskEventID = uncertain.TaskEvent.ID
		if _, err := s.ResolveUncertainDelivery(context.Background(), base); err == nil {
			t.Fatal("late duplicate event succeeded")
		}
		stored, _ := s.GetAttempt(context.Background(), uncertain.Attempt.ID)
		owner, _ := s.GetTask(context.Background(), uncertain.Task.ID)
		if stored.Revision != uncertain.Attempt.Revision || owner.Revision != uncertain.Task.Revision || owner.LatestEventCursor != uncertain.Task.LatestEventCursor {
			t.Fatalf("resolution rollback: attempt=%+v task=%+v", stored, owner)
		}
	})

	t.Run("closed outcome and sanitized evidence", func(t *testing.T) {
		s, uncertain := uncertainTestAttempt(t, 444, DeliveryPhasePromptStarted)
		base := testUncertainResolution(uncertain, ResolveUncertainDeliveryAdmitted, 24040, "")
		invalidOutcome := base
		invalidOutcome.Outcome = "other"
		if _, err := s.ResolveUncertainDelivery(context.Background(), invalidOutcome); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("outcome = %v", err)
		}
		rawEvidence := base
		rawEvidence.EvidencePayload = json.RawMessage(`{"prompt":"secret"}`)
		rawEvidence.EvidenceSHA256 = sha256Sum(rawEvidence.EvidencePayload)
		if _, err := s.ResolveUncertainDelivery(context.Background(), rawEvidence); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("raw evidence = %v", err)
		}
	})
}

func TestFindAmbiguousDeliveryAttemptIsNarrow(t *testing.T) {
	s, delivering := claimedAtInitialPhase(t, 450, DeliveryPhaseSessionReady)
	found, err := s.FindAmbiguousDeliveryAttempt(context.Background(), testWorkspaceID())
	if err != nil || found.Attempt.ID != delivering.Attempt.ID || found.Attempt.State != task.AttemptDelivering {
		t.Fatalf("delivering lookup: %+v, %v", found, err)
	}
	if _, err := s.FindAmbiguousDeliveryAttempt(context.Background(), "bad"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid workspace = %v", err)
	}
}

func TestResolveUncertainDeliveryCancellationRace(t *testing.T) {
	for run := 0; run < 12; run++ {
		s, uncertain := uncertainTestAttempt(t, 460+run, DeliveryPhasePromptStarted)
		resolve := testUncertainResolution(uncertain, ResolveUncertainDeliveryAdmitted, 25000+run*4, "")
		cancel := testCancellation(uncertain.Task.ID, 460+run, "resolve-cancel-race", "stop")
		start := make(chan struct{})
		var resolved DeliveryTransition
		var cancellation Cancellation
		var resolveErr, cancelErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			resolved, resolveErr = s.ResolveUncertainDelivery(context.Background(), resolve)
		}()
		go func() {
			defer wg.Done()
			<-start
			cancellation, cancelErr = s.RequestCancellation(context.Background(), cancel)
		}()
		close(start)
		wg.Wait()
		if cancelErr != nil {
			t.Fatalf("cancellation = %v", cancelErr)
		}
		stored, err := s.GetAttempt(context.Background(), uncertain.Attempt.ID)
		if err != nil || stored.State != task.AttemptCancelRequested || stored.DeliveryPhase != DeliveryPhasePromptStarted {
			t.Fatalf("race final attempt: %+v, %v", stored, err)
		}
		if resolveErr == nil {
			if resolved.Attempt.State != task.AttemptAdmitted || cancellation.Disposition != CancellationEffectInterrupt {
				t.Fatalf("resolution winner: resolved=%+v cancel=%+v", resolved, cancellation)
			}
		} else if !errors.Is(resolveErr, ErrStaleRevision) || cancellation.Disposition != CancellationEffectInterrupt {
			t.Fatalf("cancellation winner: resolve=%v cancel=%+v", resolveErr, cancellation)
		}
		_ = s.Close()
	}
}

func TestAdvanceDeliveryPhaseCancellationRacePreservesCommittedPhase(t *testing.T) {
	for run := 0; run < 12; run++ {
		s, claimed := claimedAtInitialPhase(t, 480+run, DeliveryPhaseSessionCreateStarted)
		advance := testPhaseAdvance(claimed, DeliveryPhaseSessionCreateStarted, DeliveryPhaseSessionReady, 27000+run*4, testDeliveryTime().Add(4*time.Millisecond))
		cancel := testCancellation(claimed.Task.ID, 480+run, "phase-cancel-race", "stop")
		start := make(chan struct{})
		var advanced DeliveryPhaseTransition
		var cancellation Cancellation
		var advanceErr, cancelErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			advanced, advanceErr = s.AdvanceDeliveryPhase(context.Background(), advance)
		}()
		go func() {
			defer wg.Done()
			<-start
			cancellation, cancelErr = s.RequestCancellation(context.Background(), cancel)
		}()
		close(start)
		wg.Wait()
		if cancelErr != nil {
			t.Fatalf("cancellation = %v", cancelErr)
		}
		want := DeliveryPhaseSessionCreateStarted
		if advanceErr == nil {
			want = DeliveryPhaseSessionReady
			if advanced.Attempt.DeliveryPhase != want {
				t.Fatalf("advanced phase = %s", advanced.Attempt.DeliveryPhase)
			}
		} else if !errors.Is(advanceErr, ErrLeaseConflict) {
			t.Fatalf("advance loser = %v", advanceErr)
		}
		if cancellation.Attempt.State != task.AttemptCancelRequested || cancellation.Attempt.DeliveryPhase != want || cancellation.Attempt.DeliveryClaimOwner != nil {
			t.Fatalf("cancellation phase: %+v", cancellation)
		}
		_ = s.Close()
	}
}

func TestCancellationPreservesEveryDeliveryPhase(t *testing.T) {
	phases := []DeliveryPhase{DeliveryPhaseClaimed, DeliveryPhaseSessionCreateStarted, DeliveryPhaseSessionReady, DeliveryPhasePromptStarted}
	for i, phase := range phases {
		t.Run(string(phase), func(t *testing.T) {
			s, claimed := claimedAtInitialPhase(t, 495+i, phase)
			got, err := s.RequestCancellation(context.Background(), testCancellation(claimed.Task.ID, 495+i, "cancel-phase", "stop"))
			if err != nil {
				t.Fatal(err)
			}
			if got.Attempt.State != task.AttemptCancelRequested || got.Attempt.DeliveryPhase != phase || got.Attempt.DeliveryClaimOwner != nil || got.Disposition != CancellationEffectReconcileDelivery {
				t.Fatalf("canceled phase: %+v", got)
			}
		})
	}
}

func TestExpirePreparedAttemptDeadlineAndRollback(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	admission, err := s.AdmitTask(context.Background(), testAdmission(500, "expire-prepared", "Expire"))
	if err != nil {
		t.Fatal(err)
	}
	base := ExpirePreparedAttemptParams{
		AttemptID: admission.Attempt.ID, ExpectedAttemptRevision: admission.Attempt.Revision, ExpectedTaskRevision: admission.Task.Revision,
		AttemptEventID: testEventID(28000), TaskEventID: testEventID(28001), Now: admission.Attempt.Deadline, Actor: testRecoveryActor(),
	}
	early := base
	early.Now = admission.Attempt.Deadline.Add(-time.Millisecond)
	if _, err := s.ExpirePreparedAttempt(context.Background(), early); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("early expiration = %v", err)
	}
	staleAttempt := base
	staleAttempt.ExpectedAttemptRevision++
	if _, err := s.ExpirePreparedAttempt(context.Background(), staleAttempt); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale attempt = %v", err)
	}
	staleTask := base
	staleTask.ExpectedTaskRevision++
	if _, err := s.ExpirePreparedAttempt(context.Background(), staleTask); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale task = %v", err)
	}

	lateEvent := base
	lateEvent.TaskEventID = admission.TaskEvent.ID
	if _, err := s.ExpirePreparedAttempt(context.Background(), lateEvent); err == nil {
		t.Fatal("duplicate event expiration succeeded")
	}
	stored, _ := s.GetAttempt(context.Background(), admission.Attempt.ID)
	if stored.State != task.AttemptPrepared || stored.Revision != admission.Attempt.Revision || eventCount(t, s) != 2 {
		t.Fatalf("expiration rollback: %+v", stored)
	}

	got, err := s.ExpirePreparedAttempt(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt.State != task.AttemptFailed || got.Task.State != task.TaskFailed || got.Attempt.DeliveryPhase != DeliveryPhaseNone || got.Attempt.TerminalReason == nil || *got.Attempt.TerminalReason != PreparedAttemptDeadlineElapsed || got.Task.TerminalReason == nil || *got.Task.TerminalReason != PreparedAttemptDeadlineElapsed || got.Attempt.Revision != 2 || got.Task.Revision != 2 {
		t.Fatalf("expired result: %+v", got)
	}
	assertDeliveryEvents(t, got, "attempt.failed", "task.failed")
	if _, err := s.ExpirePreparedAttempt(context.Background(), base); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("repeated expiration = %v", err)
	}
	if _, err := s.FindPreparedAttempt(context.Background(), testWorkspaceID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired attempt remained schedulable: %v", err)
	}
}

func TestExpirePreparedAttemptRacesClaimAndCancellation(t *testing.T) {
	t.Run("claim", func(t *testing.T) {
		for run := 0; run < 10; run++ {
			s := openTestStore(t, testDBPath(t))
			createTestWorkspace(t, s)
			admission, err := s.AdmitTask(context.Background(), testAdmission(510+run, "expire-claim-race", "Race"))
			if err != nil {
				t.Fatal(err)
			}
			expire := testExpiration(admission, 29000+run*4)
			claim := testClaim(admission.Attempt.ID, 29002+run*4, "race-owner")
			start := make(chan struct{})
			var expireErr, claimErr error
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); <-start; _, expireErr = s.ExpirePreparedAttempt(context.Background(), expire) }()
			go func() { defer wg.Done(); <-start; _, claimErr = s.ClaimPreparedAttempt(context.Background(), claim) }()
			close(start)
			wg.Wait()
			if (expireErr == nil) == (claimErr == nil) {
				t.Fatalf("expire=%v claim=%v", expireErr, claimErr)
			}
			stored, _ := s.GetAttempt(context.Background(), admission.Attempt.ID)
			if expireErr == nil && stored.State != task.AttemptFailed || claimErr == nil && stored.State != task.AttemptDelivering {
				t.Fatalf("race state: %+v", stored)
			}
			_ = s.Close()
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		for run := 0; run < 10; run++ {
			s := openTestStore(t, testDBPath(t))
			createTestWorkspace(t, s)
			admission, err := s.AdmitTask(context.Background(), testAdmission(530+run, "expire-cancel-race", "Race"))
			if err != nil {
				t.Fatal(err)
			}
			expire := testExpiration(admission, 30000+run*4)
			cancel := testCancellation(admission.Task.ID, 530+run, "expire-cancel-race", "stop")
			start := make(chan struct{})
			var expireErr, cancelErr error
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); <-start; _, expireErr = s.ExpirePreparedAttempt(context.Background(), expire) }()
			go func() { defer wg.Done(); <-start; _, cancelErr = s.RequestCancellation(context.Background(), cancel) }()
			close(start)
			wg.Wait()
			if (expireErr == nil) == (cancelErr == nil) {
				t.Fatalf("expire=%v cancel=%v", expireErr, cancelErr)
			}
			stored, _ := s.GetAttempt(context.Background(), admission.Attempt.ID)
			if expireErr == nil && stored.State != task.AttemptFailed || cancelErr == nil && stored.State != task.AttemptCancelRequested {
				t.Fatalf("race state: %+v", stored)
			}
			_ = s.Close()
		}
	})
}

func testExpiration(admission Admission, eventN int) ExpirePreparedAttemptParams {
	return ExpirePreparedAttemptParams{
		AttemptID: admission.Attempt.ID, ExpectedAttemptRevision: admission.Attempt.Revision, ExpectedTaskRevision: admission.Task.Revision,
		AttemptEventID: testEventID(eventN), TaskEventID: testEventID(eventN + 1), Now: admission.Attempt.Deadline, Actor: testRecoveryActor(),
	}
}

func TestDeliveryPhaseSchemaRejectsInvalidRowsAndDirectMutation(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	admission, err := s.AdmitTask(context.Background(), testAdmission(550, "phase-schema", "Schema"))
	if err != nil {
		t.Fatal(err)
	}
	for name, query := range map[string]string{
		"unknown phase":        `UPDATE attempts SET delivery_phase='unknown' WHERE id=?`,
		"prepared claimed":     `UPDATE attempts SET delivery_phase='claimed',delivery_started_at=created_at WHERE id=?`,
		"delivering none":      `UPDATE attempts SET state='delivering',delivery_claim_owner='owner',delivery_claim_expires_at=deadline,delivery_started_at=created_at WHERE id=?`,
		"admitted before send": `UPDATE attempts SET state='admitted',admitted_at=created_at WHERE id=?`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.db.Exec(query, admission.Attempt.ID); err == nil {
				t.Fatal("invalid phase/state row was accepted")
			}
		})
	}
	claimed, err := s.ClaimPreparedAttempt(context.Background(), testClaim(admission.Attempt.ID, 31000, "direct-owner"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE attempts SET delivery_phase='session_create_started',revision=revision+1,updated_at=updated_at+1 WHERE id=?`, admission.Attempt.ID); err == nil {
		t.Fatal("direct phase progression without event was accepted")
	}
	stored, _ := s.GetAttempt(context.Background(), admission.Attempt.ID)
	if stored.DeliveryPhase != DeliveryPhaseClaimed || stored.Revision != claimed.Attempt.Revision {
		t.Fatalf("schema rejection changed attempt: %+v", stored)
	}
	claimed = advanceTestDeliveryToPrompt(t, s, claimed, "direct-owner", 31010)
	evidence, evidenceHash := testEvidence()
	admitted, err := s.RecordAdmission(context.Background(), RecordAdmissionParams{
		AttemptID: claimed.Attempt.ID, LeaseOwner: "direct-owner", ExpectedAttemptRevision: claimed.Attempt.Revision,
		AttemptEventID: testEventID(31020), TaskEventID: testEventID(31021), Now: testDeliveryTime().Add(10 * time.Millisecond),
		EvidencePayload: evidence, EvidenceSHA256: evidenceHash, Actor: testDeliveryActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE attempts SET delivery_phase='session_ready',revision=revision+1 WHERE id=?`, admitted.Attempt.ID); err == nil {
		t.Fatal("phase regressed after admission")
	}
}

func TestNewDeliveryMutationsHonorCanceledContext(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("advance", func(t *testing.T) {
		s, claimed := claimedAtInitialPhase(t, 560, DeliveryPhaseClaimed)
		p := testPhaseAdvance(claimed, DeliveryPhaseClaimed, DeliveryPhaseSessionCreateStarted, 32000, testDeliveryTime().Add(time.Millisecond))
		if _, err := s.AdvanceDeliveryPhase(canceled, p); !errors.Is(err, context.Canceled) {
			t.Fatalf("advance context = %v", err)
		}
	})
	t.Run("resolve", func(t *testing.T) {
		s, uncertain := uncertainTestAttempt(t, 561, DeliveryPhasePromptStarted)
		if _, err := s.ResolveUncertainDelivery(canceled, testUncertainResolution(uncertain, ResolveUncertainDeliveryAdmitted, 32010, "")); !errors.Is(err, context.Canceled) {
			t.Fatalf("resolve context = %v", err)
		}
	})
	t.Run("expire", func(t *testing.T) {
		s := openTestStore(t, testDBPath(t))
		t.Cleanup(func() { _ = s.Close() })
		createTestWorkspace(t, s)
		admission, err := s.AdmitTask(context.Background(), testAdmission(562, "expire-context", "Context"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ExpirePreparedAttempt(canceled, testExpiration(admission, 32020)); !errors.Is(err, context.Canceled) {
			t.Fatalf("expire context = %v", err)
		}
	})
}

func uncertainTestAttempt(t *testing.T, n int, phase DeliveryPhase) (*Store, DeliveryTransition) {
	t.Helper()
	s, claimed := claimedAtInitialPhase(t, n, phase)
	evidence, evidenceHash := testEvidence()
	uncertain, err := s.RecordDeliveryUncertain(context.Background(), RecordDeliveryUncertainParams{
		AttemptID: claimed.Attempt.ID, LeaseOwner: "phase-owner", ExpectedAttemptRevision: claimed.Attempt.Revision,
		AttemptEventID: testEventID(26000 + n*2), TaskEventID: testEventID(26001 + n*2), Now: testDeliveryTime().Add(10 * time.Millisecond),
		Reason: "ambiguous delivery fixture", EvidencePayload: evidence, EvidenceSHA256: evidenceHash, Actor: testDeliveryActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, uncertain
}

func testUncertainResolution(uncertain DeliveryTransition, outcome ResolveUncertainDeliveryOutcome, eventN int, reason string) ResolveUncertainDeliveryParams {
	evidence, evidenceHash := testEvidence()
	return ResolveUncertainDeliveryParams{
		AttemptID: uncertain.Attempt.ID, ExpectedAttemptRevision: uncertain.Attempt.Revision, ExpectedTaskRevision: uncertain.Task.Revision,
		AttemptEventID: testEventID(eventN), TaskEventID: testEventID(eventN + 1), Now: testDeliveryTime().Add(20 * time.Millisecond),
		Outcome: outcome, Reason: reason, EvidencePayload: evidence, EvidenceSHA256: evidenceHash, Actor: testRecoveryActor(),
	}
}

func assertResolutionPayload(t *testing.T, event Event, outcome ResolveUncertainDeliveryOutcome, phase DeliveryPhase) {
	t.Helper()
	var payload struct {
		Outcome ResolveUncertainDeliveryOutcome `json:"outcome"`
		Phase   DeliveryPhase                   `json:"phase"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.Outcome != outcome || payload.Phase != phase {
		t.Fatalf("resolution payload = %s, %v", event.Payload, err)
	}
}

func claimTestAttemptAtPhase(t *testing.T, s *Store, admission Admission, want DeliveryPhase, eventBase int) DeliveryTransition {
	t.Helper()
	claimed, err := s.ClaimPreparedAttempt(context.Background(), testClaim(admission.Attempt.ID, eventBase, "phase-owner"))
	if err != nil {
		t.Fatal(err)
	}
	sequence := []DeliveryPhase{DeliveryPhaseSessionCreateStarted, DeliveryPhaseSessionReady, DeliveryPhasePromptStarted}
	from := DeliveryPhaseClaimed
	for i, to := range sequence {
		if claimed.Attempt.DeliveryPhase == want {
			break
		}
		advanced, err := s.AdvanceDeliveryPhase(context.Background(), testPhaseAdvance(claimed, from, to, eventBase+2+i, testDeliveryTime().Add(time.Duration(i+1)*time.Millisecond)))
		if err != nil {
			t.Fatal(err)
		}
		claimed.Task, claimed.Attempt = advanced.Task, advanced.Attempt
		from = to
	}
	if claimed.Attempt.DeliveryPhase != want {
		t.Fatalf("phase = %s, want %s", claimed.Attempt.DeliveryPhase, want)
	}
	return claimed
}

func claimedAtInitialPhase(t *testing.T, n int, phase DeliveryPhase) (*Store, DeliveryTransition) {
	t.Helper()
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	admission, err := s.AdmitTask(context.Background(), testAdmission(n, "phase-fixture", "Phase fixture"))
	if err != nil {
		t.Fatal(err)
	}
	return s, claimTestAttemptAtPhase(t, s, admission, phase, 22000+n*10)
}

func testPhaseAdvance(claimed DeliveryTransition, from, to DeliveryPhase, eventN int, now time.Time) AdvanceDeliveryPhaseParams {
	return AdvanceDeliveryPhaseParams{
		AttemptID: claimed.Attempt.ID, LeaseOwner: "phase-owner", ExpectedAttemptRevision: claimed.Attempt.Revision,
		From: from, To: to, EventID: testEventID(eventN), Now: now, Actor: testDeliveryActor(),
	}
}

func eventCount(t *testing.T, s *Store) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func sha256Sum(value []byte) [32]byte {
	return sha256.Sum256(value)
}
