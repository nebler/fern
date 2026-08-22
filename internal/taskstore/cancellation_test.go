package taskstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nebler/fern/internal/task"
)

func TestPreparedCancellationRestartAndReplay(t *testing.T) {
	path := testDBPath(t)
	s := openTestStore(t, path)
	createTestWorkspace(t, s)
	admitted, err := s.AdmitTask(context.Background(), testAdmission(200, "cancel-prepared-task", "Wait here"))
	if err != nil {
		t.Fatal(err)
	}
	p := testCancellation(admitted.Task.ID, 200, "cancel-prepared", "user changed direction")
	first, err := s.RequestCancellation(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	assertCancellation(t, first, CancellationEffectNonePrepared, task.AttemptCancelRequested, 2, 2)
	if first.Replayed || first.Task.CancelEpoch != 1 || first.Task.CancellationActor == nil || *first.Task.CancellationActor != p.Claim.Actor ||
		first.Task.CancellationReason == nil || *first.Task.CancellationReason != p.Reason || first.Task.CancellationRequestedAt == nil || !first.Task.CancellationRequestedAt.Equal(p.Now) {
		t.Fatalf("prepared cancellation facts: %+v", first)
	}
	if _, err := s.ClaimPreparedAttempt(context.Background(), testClaim(admitted.Attempt.ID, 4100, "late-worker")); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("claim after cancellation = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	t.Cleanup(func() { _ = s.Close() })
	inspected, err := s.InspectCancellation(context.Background(), admitted.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertCancellation(t, inspected, CancellationEffectNonePrepared, task.AttemptCancelRequested, 2, 2)

	retry := p
	retry.ReceiptID = testReceiptID(999)
	retry.AttemptEventID = testEventID(4198)
	retry.TaskEventID = testEventID(4199)
	retry.Now = p.Now.Add(time.Second)
	retry.Claim.Actor.RequestID = "retry-request"
	replay, err := s.RequestCancellation(context.Background(), retry)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || replay.Receipt.ID != first.Receipt.ID || replay.AttemptEvent.ID != first.AttemptEvent.ID ||
		replay.TaskEvent.ID != first.TaskEvent.ID || replay.Disposition != first.Disposition || replay.Task.CancelEpoch != 1 || replay.Task.Revision != 2 || replay.Attempt.Revision != 2 {
		t.Fatalf("cancellation replay differs: %+v", replay)
	}
	assertCounts(t, s, 1, 1, 2, 4)
}

func TestFindPendingCancellationSupportsRestartScheduling(t *testing.T) {
	t.Parallel()
	store := openTestStore(t, testDBPath(t))
	defer store.Close()
	createTestWorkspace(t, store)
	first, err := store.AdmitTask(context.Background(), testAdmission(70, "find-pending", "wait"))
	if err != nil {
		t.Fatal(err)
	}
	request := testCancellation(first.Task.ID, 700, "find-cancel", "stop")
	want, err := store.RequestCancellation(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.FindPendingCancellation(context.Background(), first.Task.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Task.ID != want.Task.ID || got.Attempt.ID != want.Attempt.ID || got.Disposition != want.Disposition {
		t.Fatalf("pending cancellation = %+v", got)
	}
	if _, err := store.FindPendingCancellation(context.Background(), task.WorkspaceID("bad")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid workspace error = %v", err)
	}

	evidence := json.RawMessage(`{"stage":"none_prepared","effect":"none"}`)
	ack, err := store.AcknowledgeCancellation(context.Background(), AcknowledgeCancellationParams{
		TaskID: got.Task.ID, AttemptID: got.Attempt.ID, CancelEpoch: 1,
		ExpectedAttemptRevision: got.Attempt.Revision, ExpectedTaskRevision: got.Task.Revision,
		AttemptEventID: testEventID(702), TaskEventID: testEventID(703),
		Now: testDeliveryTime().Add(3 * time.Second), Disposition: got.Disposition,
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	})
	if err != nil || ack.Task.State != task.TaskCanceled {
		t.Fatalf("acknowledgment = %+v, %v", ack, err)
	}
	if _, err := store.FindPendingCancellation(context.Background(), first.Task.WorkspaceID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("closed lookup error = %v", err)
	}
}

func TestDeliveringCancellationFencesClaimAndLateOutcome(t *testing.T) {
	s, claimed := claimedTestAttempt(t, 201)
	p := testCancellation(claimed.Task.ID, 201, "cancel-delivering", "stop delivery")
	got, err := s.RequestCancellation(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	assertCancellation(t, got, CancellationEffectReconcileDelivery, task.AttemptCancelRequested, claimed.Attempt.Revision+1, claimed.Task.Revision+1)
	if got.Attempt.DeliveryClaimOwner != nil || got.Attempt.DeliveryClaimExpiresAt != nil || got.Attempt.DeliveryStartedAt == nil {
		t.Fatalf("delivery claim not fenced: %+v", got.Attempt)
	}
	evidence, evidenceHash := testEvidence()
	_, err = s.RecordAdmission(context.Background(), RecordAdmissionParams{
		AttemptID: claimed.Attempt.ID, LeaseOwner: "worker-a", ExpectedAttemptRevision: claimed.Attempt.Revision,
		AttemptEventID: testEventID(4210), TaskEventID: testEventID(4211), Now: p.Now.Add(time.Millisecond),
		EvidencePayload: evidence, EvidenceSHA256: evidenceHash, Actor: testDeliveryActor(),
	})
	if !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("late admission outcome = %v", err)
	}
	if _, err := s.FindDeliveringAttempt(context.Background(), testWorkspaceID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delivering lookup after fence = %v", err)
	}
	assertCounts(t, s, 1, 1, 2, 9)
}

func TestCancellationDispositionForAdmittedAndRunningAttempts(t *testing.T) {
	for i, state := range []task.AttemptState{task.AttemptAdmitted, task.AttemptRunning, task.AttemptInputRequired, task.AttemptUncertain, task.AttemptRecoveryRequired} {
		t.Run(string(state), func(t *testing.T) {
			s := openTestStore(t, testDBPath(t))
			t.Cleanup(func() { _ = s.Close() })
			createTestWorkspace(t, s)
			admission, err := s.AdmitTask(context.Background(), testAdmission(210+i, "cancel-state-"+string(state), "State"))
			if err != nil {
				t.Fatal(err)
			}
			setCancellationFixtureState(t, s, admission, state)
			beforeAttempt, _ := s.GetAttempt(context.Background(), admission.Attempt.ID)
			beforeTask, _ := s.GetTask(context.Background(), admission.Task.ID)
			got, err := s.RequestCancellation(context.Background(), testCancellation(admission.Task.ID, 210+i, "cancel-"+string(state), "stop"))
			if err != nil {
				t.Fatal(err)
			}
			assertCancellation(t, got, CancellationEffectInterrupt, task.AttemptCancelRequested, beforeAttempt.Revision+1, beforeTask.Revision+1)
		})
	}
}

func TestConcurrentCancellationAndAdmissionHaveOneWinner(t *testing.T) {
	for run := 0; run < 20; run++ {
		s, claimed := claimedTestAttempt(t, 230+run)
		evidence, evidenceHash := testEvidence()
		cancel := testCancellation(claimed.Task.ID, 230+run, "cancel-race-"+string(rune('a'+run)), "race")
		outcome := RecordAdmissionParams{
			AttemptID: claimed.Attempt.ID, LeaseOwner: "worker-a", ExpectedAttemptRevision: claimed.Attempt.Revision,
			AttemptEventID: testEventID(5000 + run*4), TaskEventID: testEventID(5001 + run*4), Now: cancel.Now,
			EvidencePayload: evidence, EvidenceSHA256: evidenceHash, Actor: testDeliveryActor(),
		}
		start := make(chan struct{})
		var cancelResult Cancellation
		var cancelErr, outcomeErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			cancelResult, cancelErr = s.RequestCancellation(context.Background(), cancel)
		}()
		go func() { defer wg.Done(); <-start; _, outcomeErr = s.RecordAdmission(context.Background(), outcome) }()
		close(start)
		wg.Wait()
		if cancelErr != nil {
			t.Fatalf("cancellation must accept after either race winner: %v", cancelErr)
		}
		if outcomeErr == nil {
			if cancelResult.Disposition != CancellationEffectInterrupt {
				t.Fatalf("admission won but disposition = %s", cancelResult.Disposition)
			}
		} else {
			if !errors.Is(outcomeErr, ErrLeaseConflict) || cancelResult.Disposition != CancellationEffectReconcileDelivery {
				t.Fatalf("cancellation winner: disposition=%s outcome=%v", cancelResult.Disposition, outcomeErr)
			}
		}
		stored, _ := s.GetAttempt(context.Background(), claimed.Attempt.ID)
		if stored.State != task.AttemptCancelRequested || stored.DeliveryClaimOwner != nil {
			t.Fatalf("race final attempt: %+v", stored)
		}
		_ = s.Close()
	}
}

func TestConcurrentSameKeyCancellationCreatesOneSemanticCancellation(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	admission, err := s.AdmitTask(context.Background(), testAdmission(260, "same-cancel-task", "Cancel once"))
	if err != nil {
		t.Fatal(err)
	}
	p := testCancellation(admission.Task.ID, 260, "same-cancel", "once")
	const workers = 16
	start := make(chan struct{})
	results := make(chan Cancellation, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := s.RequestCancellation(context.Background(), p)
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
		t.Errorf("same-key cancellation: %v", err)
	}
	firstUses := 0
	for result := range results {
		if !result.Replayed {
			firstUses++
		}
		if result.Receipt.ID != p.ReceiptID || result.Task.CancelEpoch != 1 {
			t.Errorf("wrong cancellation: %+v", result)
		}
	}
	if firstUses != 1 {
		t.Fatalf("first uses = %d", firstUses)
	}
	assertCounts(t, s, 1, 1, 2, 4)
}

func TestCancellationIdempotencyAndAlreadyRequestedPolicies(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	admission, err := s.AdmitTask(context.Background(), testAdmission(270, "cancel-policy-task", "Policy"))
	if err != nil {
		t.Fatal(err)
	}
	p := testCancellation(admission.Task.ID, 270, "owned-cancel-key", "original reason")
	accepted, err := s.RequestCancellation(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}

	changedHash := p
	changedHash.Claim.RequestHash = task.RequestHash(sha256.Sum256([]byte("changed")))
	_, err = s.RequestCancellation(context.Background(), changedHash)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.ReceiptID != accepted.Receipt.ID {
		t.Fatalf("hash conflict = %v", err)
	}
	changedActor := p
	changedActor.Claim.Actor.ID = "other-device"
	_, err = s.RequestCancellation(context.Background(), changedActor)
	if !errors.Is(err, ErrIdempotencyOwnerMismatch) {
		t.Fatalf("actor conflict = %v", err)
	}
	differentKey := testCancellation(admission.Task.ID, 271, "different-cancel-key", "another cancellation")
	_, err = s.RequestCancellation(context.Background(), differentKey)
	var already *CancellationAlreadyRequestedError
	if !errors.As(err, &already) || already.ReceiptID != accepted.Receipt.ID {
		t.Fatalf("different key = %v", err)
	}
	stored, _ := s.GetTask(context.Background(), admission.Task.ID)
	if stored.CancelEpoch != 1 || stored.Revision != 2 {
		t.Fatalf("duplicate changed cancellation: %+v", stored)
	}
	assertCounts(t, s, 1, 1, 2, 4)
}

func TestTerminalTaskCancellationHasNoWritesOrAuthority(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	admission, err := s.AdmitTask(context.Background(), testAdmission(280, "terminal-task", "Terminal"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE attempts SET state='failed',revision=revision+1 WHERE id=?`, admission.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE tasks SET state='failed',revision=revision+1 WHERE id=?`, admission.Task.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.RequestCancellation(context.Background(), testCancellation(admission.Task.ID, 280, "cancel-terminal", "too late"))
	var terminal *TerminalTaskError
	if !errors.As(err, &terminal) || terminal.State != task.TaskFailed {
		t.Fatalf("terminal error = %v", err)
	}
	stored, _ := s.GetTask(context.Background(), admission.Task.ID)
	if stored.State != task.TaskFailed || stored.CancelEpoch != 0 || stored.Revision != 2 {
		t.Fatalf("terminal task changed: %+v", stored)
	}
	assertCounts(t, s, 1, 1, 1, 2)
}

func TestTerminalCurrentAttemptCreatesFenceWithoutExternalAuthority(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	admission, err := s.AdmitTask(context.Background(), testAdmission(281, "terminal-attempt-task", "Await result"))
	if err != nil {
		t.Fatal(err)
	}
	setCancellationFixtureState(t, s, admission, task.AttemptAdmitted)
	beforeAttempt, _ := s.GetAttempt(context.Background(), admission.Attempt.ID)
	beforeTask, _ := s.GetTask(context.Background(), admission.Task.ID)
	recordFixtureExecution(t, s, admission.Attempt.ID, ExecutionSucceeded, 8700)
	p := testCancellation(admission.Task.ID, 281, "cancel-terminal-attempt", "do not seal result")
	got, err := s.RequestCancellation(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	assertCancellation(t, got, CancellationEffectNoneTerminal, task.AttemptSucceeded, beforeAttempt.Revision+1, beforeTask.Revision+2)
	if got.Attempt.UpdatedAt.Equal(p.Now) {
		t.Fatal("terminal attempt was rewritten by cancellation")
	}
}

func TestCancellationRollsBackOnDuplicateReceiptOrLateEvent(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(Admission, *RequestCancellationParams)
	}{
		{"receipt", func(a Admission, p *RequestCancellationParams) { p.ReceiptID = a.Receipt.ID }},
		{"attempt event", func(a Admission, p *RequestCancellationParams) { p.AttemptEventID = a.TaskEvent.ID }},
		{"task event", func(a Admission, p *RequestCancellationParams) { p.TaskEventID = a.TaskEvent.ID }},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t, testDBPath(t))
			t.Cleanup(func() { _ = s.Close() })
			createTestWorkspace(t, s)
			a, err := s.AdmitTask(context.Background(), testAdmission(290+i, "rollback-task-"+tt.name, "Rollback"))
			if err != nil {
				t.Fatal(err)
			}
			p := testCancellation(a.Task.ID, 290+i, "rollback-cancel-"+tt.name, "rollback")
			tt.mutate(a, &p)
			if _, err := s.RequestCancellation(context.Background(), p); err == nil {
				t.Fatal("duplicate cancellation value succeeded")
			}
			owner, _ := s.GetTask(context.Background(), a.Task.ID)
			attempt, _ := s.GetAttempt(context.Background(), a.Attempt.ID)
			if owner.State != task.TaskQueued || owner.CancelEpoch != 0 || owner.Revision != 1 || attempt.State != task.AttemptPrepared || attempt.Revision != 1 {
				t.Fatalf("partial cancellation: task=%+v attempt=%+v", owner, attempt)
			}
			assertCounts(t, s, 1, 1, 1, 2)
		})
	}
}

func TestCancellationValidationAndRedaction(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	a, err := s.AdmitTask(context.Background(), testAdmission(300, "validation-task", "Secret prompt"))
	if err != nil {
		t.Fatal(err)
	}
	base := testCancellation(a.Task.ID, 300, "validation-cancel", strings.Repeat("x", maxCancellationReasonBytes))
	invalidUTF8 := string([]byte{0xff})
	tests := []struct {
		name   string
		mutate func(*RequestCancellationParams)
	}{
		{"task ID", func(p *RequestCancellationParams) { p.TaskID = "tsk_bad" }},
		{"receipt ID", func(p *RequestCancellationParams) { p.ReceiptID = "rcp_bad" }},
		{"attempt event ID", func(p *RequestCancellationParams) { p.AttemptEventID = "fev_bad" }},
		{"task event ID", func(p *RequestCancellationParams) { p.TaskEventID = p.AttemptEventID }},
		{"command", func(p *RequestCancellationParams) { p.Claim.Scope.CommandKind = "other" }},
		{"reason over max", func(p *RequestCancellationParams) { p.Reason += "x" }},
		{"reason control", func(p *RequestCancellationParams) { p.Reason = "secret\nreason" }},
		{"reason UTF-8", func(p *RequestCancellationParams) { p.Reason = invalidUTF8 }},
		{"timestamp precision", func(p *RequestCancellationParams) { p.Now = p.Now.Add(time.Nanosecond) }},
		{"timestamp zero", func(p *RequestCancellationParams) { p.Now = time.Time{} }},
		{"API version", func(p *RequestCancellationParams) { p.APIContractVersion = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := base
			tt.mutate(&p)
			_, err := s.RequestCancellation(context.Background(), p)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("invalid error = %v", err)
			}
			if strings.Contains(err.Error(), "Secret prompt") || strings.Contains(err.Error(), "secret\nreason") {
				t.Fatalf("error leaked content: %v", err)
			}
		})
	}
	if !utf8.ValidString("用户取消") {
		t.Fatal("test setup")
	}
	unicodeReason := base
	unicodeReason.Reason = "用户取消"
	unicodeReason.Claim.RequestHash = task.RequestHash(sha256.Sum256([]byte(unicodeReason.Reason)))
	got, err := s.RequestCancellation(context.Background(), unicodeReason)
	if err != nil || got.Task.CancellationReason == nil || *got.Task.CancellationReason != unicodeReason.Reason {
		t.Fatalf("unicode reason: %+v, %v", got, err)
	}
}

func TestCancellationSchemaRejectsInvalidRowsAndMutation(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	a, err := s.AdmitTask(context.Background(), testAdmission(310, "schema-task", "Schema"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE tasks SET state='cancel_requested',cancel_epoch=1 WHERE id=?`, a.Task.ID); err == nil {
		t.Fatal("schema accepted incomplete cancellation")
	}
	if _, err := s.db.Exec(`UPDATE tasks SET cancel_epoch=2 WHERE id=?`, a.Task.ID); err == nil {
		t.Fatal("schema accepted cancel epoch outside signed semantic range")
	}
	if _, err := s.db.Exec(`UPDATE attempts SET state='cancel_requested' WHERE id=?`, a.Attempt.ID); err == nil {
		t.Fatal("schema accepted unfenced attempt cancellation")
	}
	got, err := s.RequestCancellation(context.Background(), testCancellation(a.Task.ID, 310, "schema-cancel", "immutable"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE tasks SET cancel_reason='changed' WHERE id=?`, a.Task.ID); err == nil {
		t.Fatal("schema allowed cancellation mutation")
	}
	if _, err := s.db.Exec(`UPDATE tasks SET cancel_epoch=0 WHERE id=?`, a.Task.ID); err == nil {
		t.Fatal("schema allowed cancellation removal")
	}
	stored, _ := s.InspectCancellation(context.Background(), a.Task.ID)
	if stored.Receipt.ID != got.Receipt.ID || stored.Task.CancelEpoch != 1 {
		t.Fatalf("schema failures changed cancellation: %+v", stored)
	}
}

func TestCancellationContextCancellationAndBusyDeadline(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	a, err := s.AdmitTask(context.Background(), testAdmission(320, "context-task", "Context"))
	if err != nil {
		t.Fatal(err)
	}
	p := testCancellation(a.Task.ID, 320, "context-cancel", "stop")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.RequestCancellation(canceled, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request = %v", err)
	}
	lock, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	if _, err := s.RequestCancellation(ctx, p); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy cancellation = %v", err)
	}
}

func testCancellation(taskID task.TaskID, n int, key, reason string) RequestCancellationParams {
	hash := sha256.Sum256([]byte(CancelTaskCommand + "\n" + string(taskID) + "\n" + reason))
	return RequestCancellationParams{
		TaskID: taskID, ReceiptID: testReceiptID(1000 + n), AttemptEventID: testEventID(3000 + n*2), TaskEventID: testEventID(3001 + n*2),
		Claim: task.IdempotencyClaim{
			Scope: task.IdempotencyScope{WorkspaceID: testWorkspaceID(), CommandKind: CancelTaskCommand}, Key: task.IdempotencyKey(key), RequestHash: task.RequestHash(hash),
			Actor: task.ActorSnapshot{Type: task.ActorDevice, ID: "device-1", DisplayName: "Phone", CredentialID: "credential-1", Authentication: "fern_device_cookie", RequestID: "cancel-request"},
		},
		Reason: reason, Now: testDeliveryTime().Add(2 * time.Second), APIContractVersion: "v1",
	}
}

func assertCancellation(t *testing.T, got Cancellation, disposition CancellationEffectDisposition, attemptState task.AttemptState, attemptRevision, taskRevision int64) {
	t.Helper()
	if got.Disposition != disposition || got.Task.CancellationEffect != disposition || got.Task.State != task.TaskCancelRequested || got.Attempt.State != attemptState ||
		got.Task.CancelEpoch != 1 || got.Attempt.Revision != attemptRevision || got.Task.Revision != taskRevision || got.AttemptEvent.Type != "attempt.cancel_requested" ||
		got.TaskEvent.Type != "task.cancel_requested" || got.AttemptEvent.Cursor >= got.TaskEvent.Cursor || got.Task.LatestEventCursor != got.TaskEvent.Cursor ||
		got.AttemptEvent.AttemptID != got.Attempt.ID || got.TaskEvent.AttemptID != "" || got.Receipt.CommandKind != CancelTaskCommand || got.Receipt.TargetID != got.Task.ID {
		t.Fatalf("cancellation mismatch: %+v", got)
	}
}

func setCancellationFixtureState(t *testing.T, s *Store, admission Admission, state task.AttemptState) {
	t.Helper()
	claimed, err := s.ClaimPreparedAttempt(context.Background(), testClaim(admission.Attempt.ID, 8000+int(admission.Attempt.Sequence), "fixture-worker"))
	if err != nil {
		t.Fatal(err)
	}
	claimed = advanceTestDeliveryToPrompt(t, s, claimed, "fixture-worker", 8300+int(admission.Attempt.Sequence)*10)
	if state == task.AttemptAdmitted || state == task.AttemptRunning || state == task.AttemptInputRequired {
		evidence, evidenceHash := testEvidence()
		_, err = s.RecordAdmission(context.Background(), RecordAdmissionParams{
			AttemptID: admission.Attempt.ID, LeaseOwner: "fixture-worker", ExpectedAttemptRevision: claimed.Attempt.Revision,
			AttemptEventID: testEventID(8100 + int(admission.Attempt.Sequence)), TaskEventID: testEventID(8200 + int(admission.Attempt.Sequence)),
			Now: testDeliveryTime().Add(10 * time.Millisecond), EvidencePayload: evidence, EvidenceSHA256: evidenceHash, Actor: testDeliveryActor(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if state != task.AttemptAdmitted {
			outcome := ExecutionRunning
			if state == task.AttemptInputRequired {
				outcome = ExecutionInputRequired
			}
			recordFixtureExecution(t, s, admission.Attempt.ID, outcome, 8700)
		}
		return
	}
	evidence, evidenceHash := testEvidence()
	p := RecordDeliveryUncertainParams{
		AttemptID: admission.Attempt.ID, LeaseOwner: "fixture-worker", ExpectedAttemptRevision: claimed.Attempt.Revision,
		AttemptEventID: testEventID(8500 + int(admission.Attempt.Sequence)), TaskEventID: testEventID(8600 + int(admission.Attempt.Sequence)),
		Now: testDeliveryTime().Add(10 * time.Millisecond), Reason: "fixture ambiguity", EvidencePayload: evidence, EvidenceSHA256: evidenceHash, Actor: testDeliveryActor(),
	}
	if state == task.AttemptUncertain {
		_, err = s.RecordDeliveryUncertain(context.Background(), p)
	} else {
		_, err = s.RecordDeliveryRecoveryRequired(context.Background(), p)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func recordFixtureExecution(t *testing.T, s *Store, attemptID task.AttemptID, outcome ExecutionProjectionOutcome, eventN int) ExecutionProjection {
	t.Helper()
	attempt, err := s.GetAttempt(context.Background(), attemptID)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.GetTask(context.Background(), attempt.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, evidenceHash := testEvidence()
	projection, err := s.RecordExecutionProjection(context.Background(), RecordExecutionProjectionParams{
		TaskID: owner.ID, AttemptID: attempt.ID, ExpectedAttemptRevision: attempt.Revision, ExpectedTaskRevision: owner.Revision,
		ExpectedState: attempt.State, OpenCodeSessionID: attempt.OpenCodeSessionID, OpenCodeMessageID: attempt.OpenCodeMessageID,
		Outcome: outcome, AttemptEventID: testEventID(eventN), TaskEventID: testEventID(eventN + 1),
		ObservedAt: testDeliveryTime().Add(1500 * time.Millisecond), EvidencePayload: evidence, EvidenceSHA256: evidenceHash,
		Actor: testDeliveryActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return projection
}
