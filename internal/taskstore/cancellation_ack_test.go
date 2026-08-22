package taskstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

func TestAcknowledgeCancellationClosesAllDispositions(t *testing.T) {
	tests := []struct {
		name        string
		disposition CancellationEffectDisposition
		prepare     func(*testing.T, *Store, Admission)
		actor       task.ActorSnapshot
	}{
		{"none prepared", CancellationEffectNonePrepared, nil, testDeliveryActor()},
		{"reconciled delivery", CancellationEffectReconcileDelivery, func(t *testing.T, s *Store, a Admission) {
			if _, err := s.ClaimPreparedAttempt(context.Background(), testClaim(a.Attempt.ID, 9100, "cancel-ack-worker")); err != nil {
				t.Fatal(err)
			}
		}, testRecoveryActor()},
		{"interrupt", CancellationEffectInterrupt, func(t *testing.T, s *Store, a Admission) {
			setCancellationFixtureState(t, s, a, task.AttemptAdmitted)
		}, testDeliveryActor()},
		{"terminal no-op", CancellationEffectNoneTerminal, func(t *testing.T, s *Store, a Admission) {
			setCancellationFixtureState(t, s, a, task.AttemptAdmitted)
			recordFixtureExecution(t, s, a.Attempt.ID, ExecutionSucceeded, 9000)
		}, testRecoveryActor()},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t, testDBPath(t))
			t.Cleanup(func() { _ = s.Close() })
			createTestWorkspace(t, s)
			a, err := s.AdmitTask(context.Background(), testAdmission(600+i, "ack-task-"+tt.name, "Cancel safely"))
			if err != nil {
				t.Fatal(err)
			}
			if tt.prepare != nil {
				tt.prepare(t, s, a)
			}
			requested, err := s.RequestCancellation(context.Background(), testCancellation(a.Task.ID, 600+i, "ack-cancel-"+tt.name, "stop"))
			if err != nil {
				t.Fatal(err)
			}
			if requested.Disposition != tt.disposition {
				t.Fatalf("request disposition = %s, want %s", requested.Disposition, tt.disposition)
			}
			p := testCancellationAcknowledgment(requested, 9200+i*2, tt.actor)
			got, err := s.AcknowledgeCancellation(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			assertCancellationAcknowledgment(t, got, p)
			if got.Task.CancellationActor == nil || *got.Task.CancellationActor != requested.Receipt.Actor ||
				got.Task.CancellationReason == nil || requested.Task.CancellationReason == nil || *got.Task.CancellationReason != *requested.Task.CancellationReason ||
				got.Task.CancellationRequestedAt == nil || !got.Task.CancellationRequestedAt.Equal(*requested.Task.CancellationRequestedAt) ||
				got.Task.CancellationReceiptID != requested.Receipt.ID {
				t.Fatalf("immutable cancellation facts changed: %+v", got.Task)
			}

			replay, err := s.AcknowledgeCancellation(context.Background(), p)
			if err != nil || !replay.Replayed || replay.AttemptEvent.ID != got.AttemptEvent.ID || replay.TaskEvent.ID != got.TaskEvent.ID || replay.Task.Revision != got.Task.Revision || replay.Attempt.Revision != got.Attempt.Revision {
				t.Fatalf("exact acknowledgment replay: %+v, %v", replay, err)
			}
		})
	}
}

func TestAcknowledgeCancellationRejectsMismatchStaleAndChangedReplay(t *testing.T) {
	newCanceled := func(t *testing.T, n int) (*Store, Cancellation, AcknowledgeCancellationParams) {
		t.Helper()
		s := openTestStore(t, testDBPath(t))
		t.Cleanup(func() { _ = s.Close() })
		createTestWorkspace(t, s)
		a, err := s.AdmitTask(context.Background(), testAdmission(n, "ack-conflict-task", "Conflict"))
		if err != nil {
			t.Fatal(err)
		}
		requested, err := s.RequestCancellation(context.Background(), testCancellation(a.Task.ID, n, "ack-conflict-cancel", "stop"))
		if err != nil {
			t.Fatal(err)
		}
		return s, requested, testCancellationAcknowledgment(requested, 9300+n, testDeliveryActor())
	}

	t.Run("effect mismatch", func(t *testing.T) {
		s, _, p := newCanceled(t, 610)
		p.Disposition = CancellationEffectInterrupt
		if _, err := s.AcknowledgeCancellation(context.Background(), p); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("effect mismatch = %v", err)
		}
		if eventCount(t, s) != 4 {
			t.Fatal("effect mismatch wrote events")
		}
	})
	t.Run("stale attempt", func(t *testing.T) {
		s, _, p := newCanceled(t, 611)
		p.ExpectedAttemptRevision--
		if _, err := s.AcknowledgeCancellation(context.Background(), p); !errors.Is(err, ErrStaleRevision) {
			t.Fatalf("stale attempt = %v", err)
		}
	})
	t.Run("stale task", func(t *testing.T) {
		s, _, p := newCanceled(t, 612)
		p.ExpectedTaskRevision--
		if _, err := s.AcknowledgeCancellation(context.Background(), p); !errors.Is(err, ErrStaleRevision) {
			t.Fatalf("stale task = %v", err)
		}
	})
	t.Run("changed replay", func(t *testing.T) {
		s, _, p := newCanceled(t, 613)
		if _, err := s.AcknowledgeCancellation(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		changed := p
		changed.EvidencePayload = json.RawMessage(`{"closed":"different"}`)
		changed.EvidenceSHA256 = sha256.Sum256(changed.EvidencePayload)
		if _, err := s.AcknowledgeCancellation(context.Background(), changed); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("changed replay = %v", err)
		}
		if eventCount(t, s) != 6 {
			t.Fatal("changed replay wrote events")
		}
	})
	t.Run("wrong attempt", func(t *testing.T) {
		s, _, p := newCanceled(t, 614)
		p.AttemptID = testAttemptID(9999)
		if _, err := s.AcknowledgeCancellation(context.Background(), p); !errors.Is(err, ErrNotFound) {
			t.Fatalf("wrong attempt = %v", err)
		}
	})
}

func TestAcknowledgeCancellationValidationAndSanitizedEvidence(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	a, err := s.AdmitTask(context.Background(), testAdmission(620, "ack-validation-task", "Secret prompt"))
	if err != nil {
		t.Fatal(err)
	}
	requested, err := s.RequestCancellation(context.Background(), testCancellation(a.Task.ID, 620, "ack-validation-cancel", "stop"))
	if err != nil {
		t.Fatal(err)
	}
	base := testCancellationAcknowledgment(requested, 9500, testDeliveryActor())
	tests := []struct {
		name   string
		mutate func(*AcknowledgeCancellationParams)
	}{
		{"task ID", func(p *AcknowledgeCancellationParams) { p.TaskID = "tsk_bad" }},
		{"attempt ID", func(p *AcknowledgeCancellationParams) { p.AttemptID = "att_bad" }},
		{"duplicate events", func(p *AcknowledgeCancellationParams) { p.TaskEventID = p.AttemptEventID }},
		{"epoch", func(p *AcknowledgeCancellationParams) { p.CancelEpoch = 0 }},
		{"attempt revision", func(p *AcknowledgeCancellationParams) { p.ExpectedAttemptRevision = 0 }},
		{"task revision", func(p *AcknowledgeCancellationParams) { p.ExpectedTaskRevision = 0 }},
		{"disposition", func(p *AcknowledgeCancellationParams) { p.Disposition = "open" }},
		{"time precision", func(p *AcknowledgeCancellationParams) { p.Now = p.Now.Add(time.Nanosecond) }},
		{"device actor", func(p *AcknowledgeCancellationParams) { p.Actor.Type = task.ActorDevice }},
		{"malformed evidence", func(p *AcknowledgeCancellationParams) { p.EvidencePayload = []byte(`{"broken"`) }},
		{"array evidence", func(p *AcknowledgeCancellationParams) {
			p.EvidencePayload = []byte(`[]`)
			p.EvidenceSHA256 = sha256.Sum256(p.EvidencePayload)
		}},
		{"wrong evidence hash", func(p *AcknowledgeCancellationParams) { p.EvidenceSHA256 = [32]byte{} }},
		{"sensitive evidence", func(p *AcknowledgeCancellationParams) {
			p.EvidencePayload = []byte(`{"nested":{"credential":"secret"}}`)
			p.EvidenceSHA256 = sha256.Sum256(p.EvidencePayload)
		}},
		{"oversize evidence", func(p *AcknowledgeCancellationParams) {
			p.EvidencePayload = []byte(`{"value":"` + strings.Repeat("x", maxDeliveryEvidenceBytes) + `"}`)
			p.EvidenceSHA256 = sha256.Sum256(p.EvidencePayload)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := base
			tt.mutate(&p)
			if _, err := s.AcknowledgeCancellation(context.Background(), p); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
	if eventCount(t, s) != 4 {
		t.Fatal("invalid acknowledgments wrote events")
	}
}

func TestAcknowledgeCancellationRollbackAndSchemaEnforcement(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	a, err := s.AdmitTask(context.Background(), testAdmission(630, "ack-schema-task", "Schema"))
	if err != nil {
		t.Fatal(err)
	}
	requested, err := s.RequestCancellation(context.Background(), testCancellation(a.Task.ID, 630, "ack-schema-cancel", "immutable"))
	if err != nil {
		t.Fatal(err)
	}
	ackMS := requested.Task.UpdatedAt.Add(time.Millisecond).UnixMilli()
	if _, err := s.db.Exec(`UPDATE attempts SET state='canceled',cancellation_ack_at=?,terminal_reason=?,revision=revision+1,updated_at=? WHERE id=?`, ackMS, CancellationTerminalReason, ackMS, a.Attempt.ID); err == nil {
		t.Fatal("schema accepted acknowledgment without proof events")
	}
	if _, err := s.db.Exec(`UPDATE tasks SET state='canceled',terminal_reason=?,revision=revision+1,updated_at=? WHERE id=?`, CancellationTerminalReason, ackMS, a.Task.ID); err == nil {
		t.Fatal("schema accepted task cancellation without acknowledged attempt")
	}

	p := testCancellationAcknowledgment(requested, 9600, testDeliveryActor())
	p.TaskEventID = a.TaskEvent.ID
	if _, err := s.AcknowledgeCancellation(context.Background(), p); err == nil {
		t.Fatal("duplicate late event did not roll back")
	}
	storedTask, _ := s.GetTask(context.Background(), a.Task.ID)
	storedAttempt, _ := s.GetAttempt(context.Background(), a.Attempt.ID)
	if storedTask.State != task.TaskCancelRequested || storedAttempt.State != task.AttemptCancelRequested || storedAttempt.CancellationAckAt != nil || eventCount(t, s) != 4 {
		t.Fatalf("partial acknowledgment: task=%+v attempt=%+v", storedTask, storedAttempt)
	}

	p = testCancellationAcknowledgment(requested, 9610, testDeliveryActor())
	got, err := s.AcknowledgeCancellation(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE attempts SET cancellation_ack_at=cancellation_ack_at+1 WHERE id=?`, a.Attempt.ID); err == nil {
		t.Fatal("schema allowed acknowledgment time mutation")
	}
	if _, err := s.db.Exec(`UPDATE attempts SET state='failed' WHERE id=?`, a.Attempt.ID); err == nil {
		t.Fatal("schema allowed acknowledged attempt regression")
	}
	if _, err := s.db.Exec(`UPDATE tasks SET state='failed' WHERE id=?`, a.Task.ID); err == nil {
		t.Fatal("schema allowed canceled task regression")
	}
	storedTask, _ = s.GetTask(context.Background(), a.Task.ID)
	if storedTask.Revision != got.Task.Revision || storedTask.State != task.TaskCanceled {
		t.Fatalf("direct SQL failures changed task: %+v", storedTask)
	}
}

func TestAcknowledgeCancellationContextRaceAndBusyDeadline(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	a, err := s.AdmitTask(context.Background(), testAdmission(640, "ack-race-task", "Race"))
	if err != nil {
		t.Fatal(err)
	}
	requested, err := s.RequestCancellation(context.Background(), testCancellation(a.Task.ID, 640, "ack-race-cancel", "stop"))
	if err != nil {
		t.Fatal(err)
	}
	p := testCancellationAcknowledgment(requested, 9700, testRecoveryActor())
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.AcknowledgeCancellation(canceled, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context = %v", err)
	}

	lock, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	if _, err := s.AcknowledgeCancellation(ctx, p); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy acknowledgment = %v", err)
	}
	stop()
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}

	const workers = 12
	start := make(chan struct{})
	results := make(chan CancellationAcknowledgment, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := s.AcknowledgeCancellation(context.Background(), p)
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
		t.Errorf("concurrent acknowledgment: %v", err)
	}
	first := 0
	for result := range results {
		if !result.Replayed {
			first++
		}
	}
	if first != 1 || eventCount(t, s) != 6 {
		t.Fatalf("first acknowledgments=%d events=%d", first, eventCount(t, s))
	}
}

func testCancellationAcknowledgment(c Cancellation, eventN int, actor task.ActorSnapshot) AcknowledgeCancellationParams {
	evidence := json.RawMessage(`{ "closed": true, "authorityActive": false }`)
	return AcknowledgeCancellationParams{
		TaskID: c.Task.ID, AttemptID: c.Attempt.ID, CancelEpoch: 1,
		ExpectedAttemptRevision: c.Attempt.Revision, ExpectedTaskRevision: c.Task.Revision,
		AttemptEventID: testEventID(eventN), TaskEventID: testEventID(eventN + 1),
		Now: c.Task.UpdatedAt.Add(time.Millisecond), Disposition: c.Disposition,
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: actor,
	}
}

func assertCancellationAcknowledgment(t *testing.T, got CancellationAcknowledgment, p AcknowledgeCancellationParams) {
	t.Helper()
	if got.Replayed || got.Disposition != p.Disposition || got.Task.State != task.TaskCanceled || got.Attempt.State != task.AttemptCanceled ||
		got.Task.TerminalReason == nil || *got.Task.TerminalReason != CancellationTerminalReason ||
		got.Attempt.TerminalReason == nil || *got.Attempt.TerminalReason != CancellationTerminalReason ||
		got.Attempt.CancellationAckAt == nil || !got.Attempt.CancellationAckAt.Equal(p.Now) ||
		got.Task.Revision != p.ExpectedTaskRevision+1 || got.Attempt.Revision != p.ExpectedAttemptRevision+1 ||
		got.AttemptEvent.ID != p.AttemptEventID || got.AttemptEvent.Type != "attempt.canceled" ||
		got.TaskEvent.ID != p.TaskEventID || got.TaskEvent.Type != "task.canceled" ||
		got.AttemptEvent.Cursor >= got.TaskEvent.Cursor || got.Task.LatestEventCursor != got.TaskEvent.Cursor ||
		got.AttemptEvent.Actor != p.Actor || got.TaskEvent.Actor != p.Actor {
		t.Fatalf("cancellation acknowledgment mismatch: %+v", got)
	}
	var payload struct {
		TaskID                  task.TaskID                   `json:"taskId"`
		AttemptID               task.AttemptID                `json:"attemptId"`
		CancelEpoch             uint64                        `json:"cancelEpoch"`
		ExpectedAttemptRevision int64                         `json:"expectedAttemptRevision"`
		ExpectedTaskRevision    int64                         `json:"expectedTaskRevision"`
		Disposition             CancellationEffectDisposition `json:"disposition"`
		TerminalReason          string                        `json:"terminalReason"`
		Evidence                json.RawMessage               `json:"evidence"`
		EvidenceSHA256          string                        `json:"evidenceSha256"`
	}
	if err := json.Unmarshal(got.AttemptEvent.Payload, &payload); err != nil || payload.TaskID != p.TaskID || payload.AttemptID != p.AttemptID ||
		payload.CancelEpoch != 1 || payload.ExpectedAttemptRevision != p.ExpectedAttemptRevision || payload.ExpectedTaskRevision != p.ExpectedTaskRevision ||
		payload.Disposition != p.Disposition || payload.TerminalReason != CancellationTerminalReason || string(payload.Evidence) != string(p.EvidencePayload) ||
		payload.EvidenceSHA256 != "sha256:"+hex.EncodeToString(p.EvidenceSHA256[:]) || string(got.AttemptEvent.Payload) != string(got.TaskEvent.Payload) {
		t.Fatalf("cancellation acknowledgment payload: %+v, %s", payload, got.AttemptEvent.Payload)
	}
}
