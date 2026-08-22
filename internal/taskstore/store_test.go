package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

var testTime = time.Date(2026, 8, 22, 18, 57, 11, 565123000, time.UTC)

func TestAdmissionReplaySurvivesRestart(t *testing.T) {
	path := testDBPath(t)
	s := openTestStore(t, path)
	createTestWorkspace(t, s)
	p := testAdmission(1, "command-1", "Fix signup")

	first, err := s.AdmitTask(context.Background(), p)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if first.Replayed || first.Task.State != task.TaskQueued || first.Attempt.State != task.AttemptPrepared || first.TaskEvent.Cursor <= 0 || first.AttemptEvent.Cursor <= first.TaskEvent.Cursor {
		t.Fatalf("unexpected first admission: %+v", first)
	}
	if got := sha256.Sum256([]byte(p.Prompt)); got != first.Task.PromptSHA256 {
		t.Fatal("prompt digest differs")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openTestStore(t, path)
	t.Cleanup(func() { _ = s.Close() })
	p.TaskID = testTaskID(99)
	p.AttemptID = testAttemptID(99)
	p.ReceiptID = testReceiptID(99)
	p.TaskEventID = testEventID(198)
	p.AttemptEventID = testEventID(199)
	p.OpenCodeSessionID = testSessionID(99)
	p.OpenCodeMessageID = testMessageID(99)
	p.AcceptedAt = p.AcceptedAt.Add(time.Hour)
	p.Deadline = p.Deadline.Add(time.Hour)
	p.Claim.Actor.RequestID = "req-retry"
	replay, err := s.AdmitTask(context.Background(), p)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Replayed || replay.Task.ID != first.Task.ID || replay.Attempt.ID != first.Attempt.ID || replay.Receipt.ID != first.Receipt.ID || replay.TaskEvent.ID != first.TaskEvent.ID || replay.AttemptEvent.ID != first.AttemptEvent.ID {
		t.Fatalf("replay did not return originals: %+v", replay)
	}
	if !replay.Task.CreatedAt.Equal(testTime.Truncate(time.Millisecond)) || replay.Receipt.Actor.RequestID != "req-1" {
		t.Fatalf("stored clock or actor changed: %+v", replay)
	}

	gotTask, err := s.GetTask(context.Background(), first.Task.ID)
	if err != nil || gotTask.Prompt != p.Prompt || gotTask.CurrentAttemptID != first.Attempt.ID || gotTask.LatestEventCursor != first.AttemptEvent.Cursor {
		t.Fatalf("get task: %+v, %v", gotTask, err)
	}
	gotAttempt, err := s.GetAttempt(context.Background(), first.Attempt.ID)
	if err != nil || gotAttempt.OpenCodeSessionID != first.Attempt.OpenCodeSessionID || gotAttempt.OpenCodeMessageID != first.Attempt.OpenCodeMessageID || gotAttempt.ImageDigest != "sha256:image" || gotAttempt.OpenCodeProtocol != "v2" || gotAttempt.PromptSHA256 != first.Task.PromptSHA256 || gotAttempt.BaseSHA != first.Task.BaseSHA {
		t.Fatalf("get attempt: %+v, %v", gotAttempt, err)
	}
	gotReceipt, err := s.GetReceipt(context.Background(), first.Receipt.ID)
	if err != nil || gotReceipt.TargetID != first.Task.ID {
		t.Fatalf("get receipt: %+v, %v", gotReceipt, err)
	}
	var projection struct {
		TaskID    task.TaskID    `json:"taskId"`
		AttemptID task.AttemptID `json:"attemptId"`
	}
	if err := json.Unmarshal(gotReceipt.ResponseProjection, &projection); err != nil || projection.TaskID != first.Task.ID || projection.AttemptID != first.Attempt.ID {
		t.Fatalf("receipt projection: %+v, %v", projection, err)
	}
	page, err := s.ListEvents(context.Background(), testWorkspaceID(), 0, 10)
	if err != nil || len(page.Events) != 2 || !page.CaughtUp || page.Events[0].ID != first.TaskEvent.ID || page.Events[1].ID != first.AttemptEvent.ID || page.Events[1].AttemptID != first.Attempt.ID || page.NextCursor != first.AttemptEvent.Cursor {
		t.Fatalf("list events: %+v, %v", page, err)
	}
	firstPage, err := s.ListEvents(context.Background(), testWorkspaceID(), 0, 1)
	if err != nil || len(firstPage.Events) != 1 || firstPage.CaughtUp || firstPage.NextCursor != first.TaskEvent.Cursor || firstPage.Watermark != first.AttemptEvent.Cursor {
		t.Fatalf("first event page: %+v, %v", firstPage, err)
	}
	secondPage, err := s.ListEvents(context.Background(), testWorkspaceID(), firstPage.NextCursor, 1)
	if err != nil || len(secondPage.Events) != 1 || !secondPage.CaughtUp || secondPage.Events[0].ID != first.AttemptEvent.ID {
		t.Fatalf("second event page: %+v, %v", secondPage, err)
	}
	assertCounts(t, s, 1, 1, 1, 2)
}

func TestConcurrentSameKeyAdmissionCreatesOneSet(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	p := testAdmission(2, "same-key", "Do one thing")

	const workers = 16
	start := make(chan struct{})
	results := make(chan Admission, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := s.AdmitTask(context.Background(), p)
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Errorf("concurrent admission: %v", err)
	}
	firstUses := 0
	for result := range results {
		if !result.Replayed {
			firstUses++
		}
		if result.Task.ID != p.TaskID || result.Attempt.ID != p.AttemptID || result.Receipt.ID != p.ReceiptID || result.TaskEvent.ID != p.TaskEventID || result.AttemptEvent.ID != p.AttemptEventID {
			t.Errorf("wrong durable IDs: %+v", result)
		}
	}
	if firstUses != 1 {
		t.Fatalf("first-use count = %d, want 1", firstUses)
	}
	assertCounts(t, s, 1, 1, 1, 2)
}

func TestAdmissionConflictsHaveNoWrites(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	p := testAdmission(3, "owned-key", "Original")
	accepted, err := s.AdmitTask(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}

	changed := testAdmission(4, "owned-key", "Changed")
	_, err = s.AdmitTask(context.Background(), changed)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.ReceiptID != accepted.Receipt.ID || conflict.TargetID != accepted.Task.ID {
		t.Fatalf("hash conflict = %v", err)
	}

	otherActor := p
	otherActor.TaskID, otherActor.AttemptID, otherActor.ReceiptID = testTaskID(5), testAttemptID(5), testReceiptID(5)
	otherActor.TaskEventID, otherActor.AttemptEventID = testEventID(10), testEventID(11)
	otherActor.Claim.Actor.ID = "another-device"
	_, err = s.AdmitTask(context.Background(), otherActor)
	if !errors.Is(err, ErrIdempotencyOwnerMismatch) {
		t.Fatalf("actor conflict = %v", err)
	}
	assertCounts(t, s, 1, 1, 1, 2)
}

func TestAdmissionRollsBackOnLateEventFailure(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	first := testAdmission(6, "first", "First")
	if _, err := s.AdmitTask(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := testAdmission(7, "second", "Second")
	second.AttemptEventID = first.TaskEventID // Fails on the second event insert.
	if _, err := s.AdmitTask(context.Background(), second); err == nil {
		t.Fatal("expected duplicate event failure")
	}
	if _, err := s.GetTask(context.Background(), second.TaskID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partially written task: %v", err)
	}
	if _, err := s.GetReceipt(context.Background(), second.ReceiptID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partially written receipt: %v", err)
	}
	if _, err := s.GetAttempt(context.Background(), second.AttemptID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partially written attempt: %v", err)
	}
	assertCounts(t, s, 1, 1, 1, 2)
}

func TestAttemptIdentityAndSequenceConstraints(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	first := testAdmission(30, "first-attempt", "First")
	if _, err := s.AdmitTask(context.Background(), first); err != nil {
		t.Fatal(err)
	}

	sameSession := testAdmission(31, "same-session", "Second")
	sameSession.OpenCodeSessionID = first.OpenCodeSessionID
	if _, err := s.AdmitTask(context.Background(), sameSession); err == nil {
		t.Fatal("duplicate OpenCode session was accepted")
	}
	if _, err := s.GetTask(context.Background(), sameSession.TaskID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("duplicate-session task was partially written: %v", err)
	}

	sameMessage := testAdmission(32, "same-message", "Third")
	sameMessage.OpenCodeMessageID = first.OpenCodeMessageID
	if _, err := s.AdmitTask(context.Background(), sameMessage); err != nil {
		t.Fatalf("message ID in another session should be independent: %v", err)
	}

	_, err := s.db.Exec(`INSERT INTO attempts(
id,task_id,workspace_id,sequence,state,opencode_session_id,opencode_message_id,prompt_sha256,base_sha,
image_digest,opencode_protocol,execution_contract_version,agent,model_provider,model,
budget_snapshot,deadline,revision,created_at,updated_at)
SELECT ?,task_id,workspace_id,sequence,state,?,?,prompt_sha256,base_sha,image_digest,opencode_protocol,
execution_contract_version,agent,model_provider,model,budget_snapshot,deadline,revision,created_at,updated_at
FROM attempts WHERE id=?`, testAttemptID(33), testSessionID(33), testMessageID(33), first.AttemptID)
	if err == nil {
		t.Fatal("duplicate task attempt sequence was accepted")
	}
	if _, err := s.db.Exec(`UPDATE attempts SET base_sha=? WHERE id=?`, "89abcdef0123456789abcdef0123456789abcdef", first.AttemptID); err == nil {
		t.Fatal("immutable attempt base SHA was updated")
	}
	assertCounts(t, s, 2, 2, 2, 4)
}

func TestDeferredTaskAttemptAndEventOwnership(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	first, err := s.AdmitTask(context.Background(), testAdmission(40, "owner-1", "First"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AdmitTask(context.Background(), testAdmission(41, "owner-2", "Second"))
	if err != nil {
		t.Fatal(err)
	}

	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `UPDATE tasks SET current_attempt_id=? WHERE id=?`, second.Attempt.ID, first.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `COMMIT`); err == nil {
		t.Fatal("task linked to another task's attempt")
	}
	_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)

	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `UPDATE tasks SET latest_event_cursor=? WHERE id=?`, second.AttemptEvent.Cursor, first.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `COMMIT`); err == nil {
		t.Fatal("task linked to another task's event")
	}
	_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)

	_, err = s.db.Exec(`INSERT INTO events(
id,workspace_id,task_id,attempt_id,entity_type,entity_id,type,version,occurred_at,actor_snapshot_id,payload)
SELECT ?,workspace_id,?,?, 'attempt',?, 'attempt.prepared',1,occurred_at,actor_snapshot_id,'{}'
FROM events WHERE id=?`, testEventID(100), first.Task.ID, second.Attempt.ID, second.Attempt.ID, first.TaskEvent.ID)
	if err == nil {
		t.Fatal("attempt event with mismatched task ownership was accepted")
	}

	got, err := s.GetTask(context.Background(), first.Task.ID)
	if err != nil || got.CurrentAttemptID != first.Attempt.ID || got.LatestEventCursor != first.AttemptEvent.Cursor {
		t.Fatalf("failed deferred transaction changed task: %+v, %v", got, err)
	}
}

func TestAdmissionRejectsMalformedAttemptInputs(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)

	tests := []struct {
		name   string
		mutate func(*AdmitTaskParams)
	}{
		{"attempt ID", func(p *AdmitTaskParams) { p.AttemptID = "att_bad" }},
		{"attempt event ID", func(p *AdmitTaskParams) { p.AttemptEventID = p.TaskEventID }},
		{"session prefix", func(p *AdmitTaskParams) { p.OpenCodeSessionID = "ses_bad" }},
		{"session uppercase", func(p *AdmitTaskParams) {
			p.OpenCodeSessionID = task.OpenCodeSessionID("ses_ABCDEF0123456789abcdef0123456789")
		}},
		{"message prefix", func(p *AdmitTaskParams) { p.OpenCodeMessageID = "msg_bad" }},
		{"title control", func(p *AdmitTaskParams) { p.Title = "bad\ntitle" }},
		{"execution contract", func(p *AdmitTaskParams) { p.ExecutionContractVersion = "" }},
		{"agent", func(p *AdmitTaskParams) { p.Agent = "" }},
		{"provider", func(p *AdmitTaskParams) { p.ModelProvider = "provider\nunsafe" }},
		{"model", func(p *AdmitTaskParams) { p.Model = string(make([]byte, 257)) }},
		{"budget", func(p *AdmitTaskParams) { p.BudgetSnapshot = []byte(`{"broken"`) }},
		{"deadline", func(p *AdmitTaskParams) { p.Deadline = p.AcceptedAt }},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testAdmission(60+i, "invalid-"+fmt.Sprint(i), "Invalid")
			tt.mutate(&p)
			if _, err := s.AdmitTask(context.Background(), p); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("admission error = %v", err)
			}
		})
	}
	assertCounts(t, s, 0, 0, 0, 0)
}

func TestMigrationDriftAndUnknownVersionFailClosed(t *testing.T) {
	t.Run("checksum drift", func(t *testing.T) {
		path := testDBPath(t)
		s := openTestStore(t, path)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		raw := openRaw(t, path)
		if _, err := raw.Exec(`UPDATE schema_migrations SET checksum=? WHERE version=1`, fmt.Sprintf("%064d", 1)); err != nil {
			t.Fatal(err)
		}
		_ = raw.Close()
		_, err := Open(context.Background(), path)
		if !errors.Is(err, ErrMigrationDrift) {
			t.Fatalf("open drifted schema = %v", err)
		}
	})

	t.Run("unknown newer version", func(t *testing.T) {
		path := testDBPath(t)
		s := openTestStore(t, path)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		raw := openRaw(t, path)
		if _, err := raw.Exec(`PRAGMA user_version=4`); err != nil {
			t.Fatal(err)
		}
		_ = raw.Close()
		_, err := Open(context.Background(), path)
		if !errors.Is(err, ErrUnsupportedSchema) {
			t.Fatalf("open newer schema = %v", err)
		}
	})
}

func TestSQLitePoliciesAndForeignKeys(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	for range 12 { // Force checks across pooled connections.
		var fk, syncMode, busy int
		var journal string
		if err := s.db.QueryRow(`SELECT (SELECT * FROM pragma_foreign_keys), (SELECT * FROM pragma_synchronous), (SELECT * FROM pragma_busy_timeout), (SELECT * FROM pragma_journal_mode)`).Scan(&fk, &syncMode, &busy, &journal); err != nil {
			t.Fatal(err)
		}
		if fk != 1 || syncMode != 2 || busy != busyTimeoutMS || journal != "wal" {
			t.Fatalf("unsafe policy: fk=%d sync=%d busy=%d journal=%s", fk, syncMode, busy, journal)
		}
	}
	_, err := s.db.Exec(`INSERT INTO receipts(
id,workspace_id,command_kind,state,idempotency_key,request_hash,actor_snapshot_id,accepted_at,
api_contract_version,target_type,target_id,response_status,response_projection)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, testReceiptID(20), testWorkspaceID(), "task.submit", "accepted", "fk", make([]byte, 32), 999, 1, "v1", "task", testTaskID(20), 202, `{}`)
	if err == nil {
		t.Fatal("foreign key violation was accepted")
	}
}

func TestOpenRejectsExistingForeignKeyViolation(t *testing.T) {
	path := testDBPath(t)
	s := openTestStore(t, path)
	createTestWorkspace(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := openRaw(t, path) // SQLite defaults foreign_keys off for this connection.
	if _, err := raw.Exec(`INSERT INTO actor_snapshots(
actor_type,actor_id,display_name,credential_id,authentication,request_id)
VALUES('device','device-1','Phone','credential-1','fern_device_cookie','req-corrupt')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO receipts(
id,workspace_id,command_kind,state,idempotency_key,request_hash,actor_snapshot_id,accepted_at,
api_contract_version,target_type,target_id,response_status,response_projection)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, testReceiptID(21), testWorkspaceID(), "task.submit", "accepted", "broken-fk", make([]byte, 32), 1, 1, "v1", "task", testTaskID(21), 202, `{}`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), path); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("open store with foreign key violation = %v", err)
	}
}

func TestOpenRejectsUnsafePathsAndPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission and symlink semantics")
	}
	t.Run("directory permissions", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := Open(context.Background(), filepath.Join(dir, "tasks.db"))
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("open public directory = %v", err)
		}
	})
	t.Run("file permissions", func(t *testing.T) {
		dir := privateDir(t)
		path := filepath.Join(dir, "tasks.db")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Open(context.Background(), path)
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("open public file = %v", err)
		}
	})
	t.Run("database symlink", func(t *testing.T) {
		dir := privateDir(t)
		target := filepath.Join(dir, "target.db")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "tasks.db")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		_, err := Open(context.Background(), link)
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("open symlink = %v", err)
		}
	})
	t.Run("directory symlink", func(t *testing.T) {
		root := privateDir(t)
		realDir := filepath.Join(root, "real")
		if err := os.Mkdir(realDir, 0o700); err != nil {
			t.Fatal(err)
		}
		linkDir := filepath.Join(root, "link")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Fatal(err)
		}
		_, err := Open(context.Background(), filepath.Join(linkDir, "tasks.db"))
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("open under symlink = %v", err)
		}
	})
}

func TestCanceledContextsStopOperations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, testDBPath(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("open canceled context = %v", err)
	}

	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	if _, err := s.AdmitTask(ctx, testAdmission(8, "canceled", "Canceled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("admit canceled context = %v", err)
	}
	if _, err := s.ListEvents(ctx, testWorkspaceID(), 0, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("list canceled context = %v", err)
	}
}

func TestAdmissionHonorsDeadlineWhileDatabaseIsBusy(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	lock, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = s.AdmitTask(ctx, testAdmission(9, "deadline", "Deadline"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy admission = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("deadline took %v despite context cancellation", elapsed)
	}
	assertCounts(t, s, 0, 0, 0, 0)
}

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

func testDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(privateDir(t), "tasks.db")
}

func privateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func createTestWorkspace(t *testing.T, s *Store) {
	t.Helper()
	err := s.CreateWorkspace(context.Background(), Workspace{
		ID: testWorkspaceID(), Name: "demo", State: WorkspaceActive,
		RepositoryPath: "/srv/fern/workspaces/demo", InstallationID: 123, RepositoryID: 987654321,
		RepositoryFullName: "owner/repository", ImageDigest: "sha256:image", OpenCodeProtocol: "v2",
		RuntimeDesiredState: "running", ReconciliationEpoch: 1, CreatedAt: testTime,
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
}

func testAdmission(n int, key, prompt string) AdmitTaskParams {
	hash := sha256.Sum256([]byte("task.submit\n" + prompt))
	return AdmitTaskParams{
		TaskID: testTaskID(n), AttemptID: testAttemptID(n), ReceiptID: testReceiptID(n),
		TaskEventID: testEventID(n * 2), AttemptEventID: testEventID(n*2 + 1),
		OpenCodeSessionID: testSessionID(n), OpenCodeMessageID: testMessageID(n),
		Claim: task.IdempotencyClaim{
			Scope: task.IdempotencyScope{WorkspaceID: testWorkspaceID(), CommandKind: SubmitTaskCommand},
			Key:   task.IdempotencyKey(key), RequestHash: task.RequestHash(hash),
			Actor: task.ActorSnapshot{Type: task.ActorDevice, ID: "device-1", DisplayName: "Phone", CredentialID: "credential-1", Authentication: "fern_device_cookie", RequestID: "req-1"},
		},
		Title: "Task title", Prompt: prompt, RepositoryID: 987654321, BaseRef: "main",
		BaseSHA: task.GitOID("0123456789abcdef0123456789abcdef01234567"), ObjectFormat: "sha1",
		ExecutionContractVersion: "exec-v1", Agent: "build", ModelProvider: "provider", Model: "model-1",
		BudgetSnapshot: []byte(`{"maxTokens":4096}`), Deadline: testTime.Add(time.Hour),
		APIContractVersion: "v1", AcceptedAt: testTime,
	}
}

func testWorkspaceID() task.WorkspaceID  { return task.WorkspaceID(testID("wsp_", 0)) }
func testTaskID(n int) task.TaskID       { return task.TaskID(testID("tsk_", n)) }
func testAttemptID(n int) task.AttemptID { return task.AttemptID(testID("att_", n)) }
func testReceiptID(n int) task.ReceiptID { return task.ReceiptID(testID("rcp_", n)) }
func testEventID(n int) task.EventID     { return task.EventID(testID("fev_", n)) }

func testSessionID(n int) task.OpenCodeSessionID {
	return task.OpenCodeSessionID(fmt.Sprintf("ses_%032x", n+1))
}

func testMessageID(n int) task.OpenCodeMessageID {
	return task.OpenCodeMessageID(fmt.Sprintf("msg_%032x", n+1))
}

func testID(prefix string, n int) string {
	return fmt.Sprintf("%s0198d34d-6a50-75fb-b1f2-%012x", prefix, n+1)
}

func assertCounts(t *testing.T, s *Store, tasks, attempts, receipts, events int) {
	t.Helper()
	for table, want := range map[string]int{"tasks": tasks, "attempts": attempts, "receipts": receipts, "events": events} {
		var got int
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s count = %d, want %d", table, got, want)
		}
	}
}

func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	return db
}
