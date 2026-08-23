package taskstore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

func TestExecutionProjectionDiscoveryTransitionsAndReplay(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	a, err := s.AdmitTask(context.Background(), testAdmission(700, "execution", "Project execution"))
	if err != nil {
		t.Fatal(err)
	}
	setCancellationFixtureState(t, s, a, task.AttemptAdmitted)

	work, err := s.FindExecutionAttempt(context.Background(), testWorkspaceID())
	if err != nil || work.Attempt.State != task.AttemptAdmitted || work.Task.State != task.TaskRunning {
		t.Fatalf("find admitted execution: %+v, %v", work, err)
	}
	p := testExecutionProjection(work, ExecutionRunning, 11000, work.Attempt.UpdatedAt.Add(time.Millisecond))
	running, err := s.RecordExecutionProjection(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if running.Replayed || running.Attempt.State != task.AttemptRunning || running.Task.State != task.TaskRunning ||
		running.Attempt.Revision != p.ExpectedAttemptRevision+1 || running.Task.Revision != p.ExpectedTaskRevision+1 ||
		running.AttemptEvent.Cursor >= running.TaskEvent.Cursor || running.Task.LatestEventCursor != running.TaskEvent.Cursor {
		t.Fatalf("running projection: %+v", running)
	}
	replay, err := s.RecordExecutionProjection(context.Background(), p)
	if err != nil || !replay.Replayed || replay.AttemptEvent.ID != running.AttemptEvent.ID {
		t.Fatalf("running replay: %+v, %v", replay, err)
	}

	inputParams := testExecutionProjection(DeliveryWork{Task: running.Task, Attempt: running.Attempt}, ExecutionInputRequired, 11002, p.ObservedAt.Add(time.Millisecond))
	input, err := s.RecordExecutionProjection(context.Background(), inputParams)
	if err != nil {
		t.Fatal(err)
	}
	if input.Attempt.State != task.AttemptInputRequired || input.Task.State != task.TaskInputRequired {
		t.Fatalf("input projection: %+v", input)
	}
	work, err = s.FindExecutionAttempt(context.Background(), testWorkspaceID())
	if err != nil || work.Attempt.State != task.AttemptInputRequired {
		t.Fatalf("find input execution: %+v, %v", work, err)
	}
	resumeParams := testExecutionProjection(work, ExecutionRunning, 11004, inputParams.ObservedAt.Add(time.Millisecond))
	resumed, err := s.RecordExecutionProjection(context.Background(), resumeParams)
	if err != nil || resumed.Attempt.State != task.AttemptRunning || resumed.Task.State != task.TaskRunning {
		t.Fatalf("resume projection: %+v, %v", resumed, err)
	}

	changed := p
	changed.EvidencePayload = json.RawMessage(`{"active":false}`)
	changed.EvidenceSHA256 = sha256.Sum256(changed.EvidencePayload)
	if _, err := s.RecordExecutionProjection(context.Background(), changed); !errors.Is(err, ErrInvalidState) && !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("changed old replay = %v", err)
	}
	var approvals int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('approvals','forms')`).Scan(&approvals); err != nil || approvals != 0 {
		t.Fatalf("invented approval persistence: count=%d err=%v", approvals, err)
	}
}

func TestExecutionProjectionTerminalAndRecoveryOutcomes(t *testing.T) {
	for i, outcome := range []ExecutionProjectionOutcome{ExecutionRecoveryRequired, ExecutionFailed, ExecutionSucceeded} {
		t.Run(string(outcome), func(t *testing.T) {
			s := openTestStore(t, testDBPath(t))
			t.Cleanup(func() { _ = s.Close() })
			createTestWorkspace(t, s)
			a, err := s.AdmitTask(context.Background(), testAdmission(710+i, "execution-"+string(outcome), "Outcome"))
			if err != nil {
				t.Fatal(err)
			}
			setCancellationFixtureState(t, s, a, task.AttemptAdmitted)
			work, _ := s.FindExecutionAttempt(context.Background(), testWorkspaceID())
			p := testExecutionProjection(work, outcome, 11100+i*2, work.Attempt.UpdatedAt.Add(time.Millisecond))
			if outcome == ExecutionRecoveryRequired || outcome == ExecutionFailed {
				p.Reason = "bounded_projection_failure"
			}
			got, err := s.RecordExecutionProjection(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			switch outcome {
			case ExecutionRecoveryRequired:
				if got.Attempt.RecoveryReason == nil || got.Task.State != task.TaskRecoveryRequired {
					t.Fatalf("recovery projection: %+v", got)
				}
			case ExecutionFailed:
				if got.Attempt.TerminalReason == nil || got.Task.TerminalReason == nil || got.Task.State != task.TaskFailed {
					t.Fatalf("failure projection: %+v", got)
				}
			case ExecutionSucceeded:
				if got.Attempt.State != task.AttemptSucceeded || got.Task.State != task.TaskRunning {
					t.Fatalf("success awaits seal: %+v", got)
				}
				unsealed, err := s.FindSucceededUnsealedAttempt(context.Background(), testWorkspaceID())
				if err != nil || unsealed.Attempt.ID != got.Attempt.ID {
					t.Fatalf("find unsealed: %+v, %v", unsealed, err)
				}
			}
		})
	}
}

func TestExecutionProjectionCancellationRace(t *testing.T) {
	for run := 0; run < 12; run++ {
		s := openTestStore(t, testDBPath(t))
		createTestWorkspace(t, s)
		a, err := s.AdmitTask(context.Background(), testAdmission(790+run, "execution-cancel-race", "Race"))
		if err != nil {
			t.Fatal(err)
		}
		setCancellationFixtureState(t, s, a, task.AttemptAdmitted)
		work, _ := s.FindExecutionAttempt(context.Background(), testWorkspaceID())
		projection := testExecutionProjection(work, ExecutionSucceeded, 12100+run*4, work.Attempt.UpdatedAt.Add(time.Millisecond))
		cancel := testCancellation(work.Task.ID, 790+run, "execution-race-cancel", "stop")
		start := make(chan struct{})
		var projected ExecutionProjection
		var canceled Cancellation
		var projectionErr, cancelErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			projected, projectionErr = s.RecordExecutionProjection(context.Background(), projection)
		}()
		go func() {
			defer wg.Done()
			<-start
			canceled, cancelErr = s.RequestCancellation(context.Background(), cancel)
		}()
		close(start)
		wg.Wait()
		if cancelErr != nil {
			t.Fatalf("cancellation must fence either winner: %v", cancelErr)
		}
		if projectionErr == nil {
			if projected.Attempt.State != task.AttemptSucceeded || canceled.Disposition != CancellationEffectNoneTerminal {
				t.Fatalf("projection winner: projection=%+v cancellation=%+v", projected, canceled)
			}
		} else if !errors.Is(projectionErr, ErrStaleRevision) && !errors.Is(projectionErr, ErrInvalidState) {
			t.Fatalf("cancellation winner projection error = %v", projectionErr)
		}
		stored, _ := s.GetTask(context.Background(), work.Task.ID)
		if stored.State != task.TaskCancelRequested || stored.CancelEpoch != 1 || stored.SealedResultID != "" {
			t.Fatalf("race final task: %+v", stored)
		}
		_ = s.Close()
	}
}

func TestExecutionProjectionValidationAndDirectSQLFence(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	a, _ := s.AdmitTask(context.Background(), testAdmission(720, "execution-validation", "Validation"))
	setCancellationFixtureState(t, s, a, task.AttemptAdmitted)
	work, _ := s.FindExecutionAttempt(context.Background(), testWorkspaceID())
	base := testExecutionProjection(work, ExecutionRunning, 11200, work.Attempt.UpdatedAt.Add(time.Millisecond))
	tests := []struct {
		name   string
		mutate func(*RecordExecutionProjectionParams)
	}{
		{"task", func(p *RecordExecutionProjectionParams) { p.TaskID = "tsk_bad" }},
		{"session", func(p *RecordExecutionProjectionParams) { p.OpenCodeSessionID = testSessionID(999) }},
		{"revision", func(p *RecordExecutionProjectionParams) { p.ExpectedTaskRevision = 0 }},
		{"actor", func(p *RecordExecutionProjectionParams) { p.Actor.Type = task.ActorDevice }},
		{"timestamp", func(p *RecordExecutionProjectionParams) { p.ObservedAt = p.ObservedAt.Add(time.Nanosecond) }},
		{"reason", func(p *RecordExecutionProjectionParams) { p.Reason = "not allowed" }},
		{"sensitive", func(p *RecordExecutionProjectionParams) {
			p.EvidencePayload = json.RawMessage(`{"authorization":"secret"}`)
			p.EvidenceSHA256 = sha256.Sum256(p.EvidencePayload)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := base
			tt.mutate(&p)
			if _, err := s.RecordExecutionProjection(context.Background(), p); !errors.Is(err, ErrInvalidInput) && tt.name != "session" {
				t.Fatalf("validation error = %v", err)
			} else if tt.name == "session" && !errors.Is(err, ErrInvalidState) {
				t.Fatalf("identity error = %v", err)
			}
		})
	}
	if _, err := s.db.Exec(`UPDATE attempts SET state='running',revision=revision+1,updated_at=updated_at+1 WHERE id=?`, work.Attempt.ID); err == nil {
		t.Fatal("direct SQL execution transition bypassed proof events")
	}
}

func TestSealNoChangesResultReplayAndImmutability(t *testing.T) {
	s, success := succeededTestAttempt(t, 730)
	p := testSealResult(success, 11300)
	sealed, err := s.SealResult(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.Replayed || sealed.Result.State != task.ResultSealed || sealed.Result.Outcome != task.ResultNoChanges ||
		sealed.Task.State != task.TaskCompleted || sealed.Task.SealedResultID != p.ResultID ||
		sealed.Attempt.State != task.AttemptSucceeded || sealed.Attempt.SealedResultID != p.ResultID ||
		sealed.Task.Revision != p.ExpectedTaskRevision+1 || sealed.Attempt.Revision != p.ExpectedAttemptRevision+1 ||
		sealed.ResultEvent.Cursor >= sealed.TaskEvent.Cursor || sealed.Task.LatestEventCursor != sealed.TaskEvent.Cursor {
		t.Fatalf("sealed result: %+v", sealed)
	}
	replay, err := s.SealResult(context.Background(), p)
	if err != nil || !replay.Replayed || replay.Result.ID != sealed.Result.ID {
		t.Fatalf("seal replay: %+v, %v", replay, err)
	}
	stored, err := s.GetResult(context.Background(), p.ResultID)
	manifest, manifestErr := s.GetResultManifest(context.Background(), p.ResultID)
	if err != nil || manifestErr != nil || stored.ResultCommit != p.BaseSHA || len(manifest) != 0 {
		t.Fatalf("result reads: %+v manifest=%+v errors=%v,%v", stored, manifest, err, manifestErr)
	}
	if _, err := s.db.Exec(`UPDATE results SET policy_version='changed' WHERE id=?`, p.ResultID); err == nil {
		t.Fatal("result mutation succeeded")
	}
	if _, err := s.db.Exec(`DELETE FROM results WHERE id=?`, p.ResultID); err == nil {
		t.Fatal("result deletion succeeded")
	}
	if _, err := s.db.Exec(`UPDATE tasks SET state='failed' WHERE id=?`, p.TaskID); err == nil {
		t.Fatal("completed task regressed")
	}
}

func TestSealChangedResultCanonicalManifest(t *testing.T) {
	s, success := succeededTestAttempt(t, 740)
	p := testSealResult(success, 11400)
	p.Outcome = task.ResultChanged
	p.ResultCommit = task.GitOID("1111111111111111111111111111111111111111")
	mode, blob, size := "100644", "2222222222222222222222222222222222222222", int64(12)
	p.Manifest = []ManifestEntry{{
		PathBase64: base64.StdEncoding.EncodeToString([]byte("internal/file.go")), ChangeKind: "added",
		NewMode: &mode, NewBlobOID: &blob, NewSize: &size,
	}}
	p.ManifestSHA256 = manifestDigest(p.Manifest)
	sealed, err := s.SealResult(context.Background(), p)
	if err != nil || len(sealed.Manifest) != 1 || sealed.Result.ResultCommit != p.ResultCommit {
		t.Fatalf("changed seal: %+v, %v", sealed, err)
	}
	if _, err := s.db.Exec(`UPDATE result_manifest SET path_base64=? WHERE result_id=?`, base64.StdEncoding.EncodeToString([]byte("other")), p.ResultID); err == nil {
		t.Fatal("manifest mutation succeeded")
	}
}

func TestSealResultCancellationAndConcurrentRaces(t *testing.T) {
	t.Run("cancellation wins", func(t *testing.T) {
		s, success := succeededTestAttempt(t, 750)
		p := testSealResult(success, 11500)
		if _, err := s.RequestCancellation(context.Background(), testCancellation(success.Task.ID, 750, "seal-cancel", "stop")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SealResult(context.Background(), p); !errors.Is(err, ErrStaleRevision) && !errors.Is(err, ErrInvalidState) {
			t.Fatalf("seal after cancellation = %v", err)
		}
		if _, err := s.GetResult(context.Background(), p.ResultID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("canceled race wrote result: %v", err)
		}
	})

	t.Run("same proof", func(t *testing.T) {
		s, success := succeededTestAttempt(t, 751)
		p := testSealResult(success, 11510)
		const workers = 12
		start := make(chan struct{})
		results := make(chan SealedResult, workers)
		errs := make(chan error, workers)
		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got, err := s.SealResult(context.Background(), p)
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
			t.Errorf("concurrent seal: %v", err)
		}
		first := 0
		for result := range results {
			if !result.Replayed {
				first++
			}
		}
		if first != 1 {
			t.Fatalf("first seals = %d", first)
		}
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM results`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("result count=%d err=%v", count, err)
		}
	})
}

func TestSealResultRejectsTupleManifestAndDirectSQLBypass(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SealResultParams)
	}{
		{"dirty", func(p *SealResultParams) { p.WorktreeClean = false }},
		{"repository", func(p *SealResultParams) { p.RepositoryID++ }},
		{"base", func(p *SealResultParams) { p.BaseSHA = "3333333333333333333333333333333333333333" }},
		{"no changes commit", func(p *SealResultParams) { p.ResultCommit = "4444444444444444444444444444444444444444" }},
		{"manifest digest", func(p *SealResultParams) { p.ManifestSHA256 = [32]byte{} }},
		{"evidence digest", func(p *SealResultParams) { p.EvidenceSHA256 = [32]byte{} }},
		{"identity", func(p *SealResultParams) { p.OpenCodeMessageID = testMessageID(999) }},
		{"actor", func(p *SealResultParams) { p.Actor.Type = task.ActorDevice }},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, success := succeededTestAttempt(t, 760+i)
			p := testSealResult(success, 11600+i*2)
			tt.mutate(&p)
			if _, err := s.SealResult(context.Background(), p); err == nil {
				t.Fatal("invalid seal succeeded")
			}
			var count int
			if err := s.db.QueryRow(`SELECT count(*) FROM results`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("invalid seal wrote result count=%d err=%v", count, err)
			}
		})
	}

	s, success := succeededTestAttempt(t, 780)
	if _, err := s.db.Exec(`UPDATE tasks SET state='completed',sealed_result_id=? WHERE id=?`, testResultID(999), success.Task.ID); err == nil {
		t.Fatal("direct SQL task completion bypassed result proof")
	}
}

func TestExecutionResultMigrationFromVersionOne(t *testing.T) {
	path := testDBPath(t)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	raw := openRaw(t, path)
	if _, err := raw.Exec(initialSchema); err != nil {
		t.Fatalf("install v1 schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_migrations(version,name,checksum) VALUES(1,?,?)`, migrations[0].name, migrationChecksum(migrations[0])); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, path)
	t.Cleanup(func() { _ = s.Close() })
	var version, resultTables, sealedColumns int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('results','result_manifest')`).Scan(&resultTables); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM pragma_table_info('tasks') WHERE name='sealed_result_id') + (SELECT count(*) FROM pragma_table_info('attempts') WHERE name='sealed_result_id')`).Scan(&sealedColumns); err != nil {
		t.Fatal(err)
	}
	if version != 5 || resultTables != 2 || sealedColumns != 2 {
		t.Fatalf("migration projection version=%d tables=%d columns=%d", version, resultTables, sealedColumns)
	}
}

func testExecutionProjection(work DeliveryWork, outcome ExecutionProjectionOutcome, eventN int, at time.Time) RecordExecutionProjectionParams {
	evidence := json.RawMessage(`{"scanComplete":true,"active":true,"objects":2}`)
	return RecordExecutionProjectionParams{
		TaskID: work.Task.ID, AttemptID: work.Attempt.ID, ExpectedAttemptRevision: work.Attempt.Revision,
		ExpectedTaskRevision: work.Task.Revision, ExpectedState: work.Attempt.State,
		OpenCodeSessionID: work.Attempt.OpenCodeSessionID, OpenCodeMessageID: work.Attempt.OpenCodeMessageID,
		Outcome: outcome, AttemptEventID: testEventID(eventN), TaskEventID: testEventID(eventN + 1), ObservedAt: at,
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	}
}

func succeededTestAttempt(t *testing.T, n int) (*Store, ExecutionProjection) {
	t.Helper()
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	a, err := s.AdmitTask(context.Background(), testAdmission(n, "result-task", "Seal result"))
	if err != nil {
		t.Fatal(err)
	}
	setCancellationFixtureState(t, s, a, task.AttemptAdmitted)
	return s, recordFixtureExecution(t, s, a.Attempt.ID, ExecutionSucceeded, 12000)
}

func testSealResult(success ExecutionProjection, eventN int) SealResultParams {
	evidence := json.RawMessage(`{"scanComplete":true,"terminal":"succeeded","objects":4}`)
	manifest := make([]ManifestEntry, 0)
	return SealResultParams{
		ResultID: testResultID(eventN), TaskID: success.Task.ID, AttemptID: success.Attempt.ID,
		ExpectedAttemptRevision: success.Attempt.Revision, ExpectedTaskRevision: success.Task.Revision,
		ResultEventID: testEventID(eventN), TaskEventID: testEventID(eventN + 1), RepositoryID: success.Task.RepositoryID,
		BaseSHA: success.Task.BaseSHA, ResultCommit: success.Task.BaseSHA, TreeOID: task.GitOID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Outcome: task.ResultNoChanges, WorktreeClean: true, Manifest: manifest, ManifestSHA256: manifestDigest(manifest),
		OpenCodeSessionID: success.Attempt.OpenCodeSessionID, OpenCodeMessageID: success.Attempt.OpenCodeMessageID,
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), PolicyVersion: "result-v1",
		CollectedAt: success.Attempt.UpdatedAt.Add(time.Millisecond), SealedAt: success.Attempt.UpdatedAt.Add(2 * time.Millisecond),
		Actor: testDeliveryActor(),
	}
}

func manifestDigest(entries []ManifestEntry) [32]byte {
	encoded, _ := json.Marshal(entries)
	return sha256.Sum256(encoded)
}

func testResultID(n int) task.ResultID { return task.ResultID(testID("res_", n)) }
