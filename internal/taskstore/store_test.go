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

	"github.com/nebler/fern/internal/domain"
)

var testTime = time.Date(2026, 8, 22, 18, 57, 11, 565123000, time.UTC)

func TestAdmissionReplaySurvivesRestart(t *testing.T) {
	path := testDBPath(t)
	s := openTestStore(t, path)
	createTestWorkspace(t, s)
	p := testAdmission(1, "command-1", "Fix signup")

	first, err := s.AdmitBackgroundRun(context.Background(), p)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if first.Replayed || first.Run.State != domain.Queued || first.Run.EffectPhase != domain.Absent {
		t.Fatalf("unexpected first admission: %+v", first)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openTestStore(t, path)
	t.Cleanup(func() { _ = s.Close() })
	p.AcceptedAt = p.AcceptedAt.Add(time.Hour)
	p.Deadline = p.Deadline.Add(time.Hour)
	p.Claim.Actor.RequestID = "req-retry"
	replay, err := s.AdmitBackgroundRun(context.Background(), p)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Replayed || replay.Run.RunID != first.Run.RunID || replay.Receipt.ID != first.Receipt.ID {
		t.Fatalf("replay did not return originals: %+v", replay)
	}
	if !replay.Run.CreatedAt.Equal(testTime.Truncate(time.Millisecond)) || !replay.Run.Deadline.Equal(first.Run.Deadline) ||
		replay.Receipt.Actor.RequestID != "req-1" {
		t.Fatalf("stored clock or actor changed: %+v", replay)
	}

	var prompt string
	if err := s.db.QueryRow(`SELECT prompt FROM runs WHERE id=?`, first.Run.RunID).Scan(&prompt); err != nil || prompt != p.Prompt {
		t.Fatalf("stored prompt = %q, %v", prompt, err)
	}
	gotReceipt, _, err := s.FindReceiptByIdempotency(context.Background(), testWorkspaceID(), CreateBackgroundRunCommand, p.Claim.Key)
	if err != nil || gotReceipt.RunID != first.Run.RunID {
		t.Fatalf("get receipt: %+v, %v", gotReceipt, err)
	}
	var projection struct {
		RunID domain.RunID `json:"run_id"`
	}
	if err := json.Unmarshal(gotReceipt.ResponseProjection, &projection); err != nil || projection.RunID != first.Run.RunID {
		t.Fatalf("receipt projection: %+v, %v", projection, err)
	}
	assertCounts(t, s, 1, 1)
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
			result, err := s.AdmitBackgroundRun(context.Background(), p)
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
	receipts := map[int64]bool{}
	for result := range results {
		if !result.Replayed {
			firstUses++
		}
		receipts[result.Receipt.ID] = true
		if result.Run.RunID != p.RunID {
			t.Errorf("wrong durable run: %+v", result)
		}
	}
	if firstUses != 1 || len(receipts) != 1 {
		t.Fatalf("first-use count = %d, receipts = %d, want 1", firstUses, len(receipts))
	}
	assertCounts(t, s, 1, 1)
}

func TestAdmissionConflictsHaveNoWrites(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	p := testAdmission(3, "owned-key", "Original")
	accepted, err := s.AdmitBackgroundRun(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}

	changed := testAdmission(4, "owned-key", "Changed")
	_, err = s.AdmitBackgroundRun(context.Background(), changed)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.ReceiptID != accepted.Receipt.ID || conflict.RunID != accepted.Run.RunID {
		t.Fatalf("hash conflict = %v", err)
	}

	otherActor := p
	otherActor.Claim.Actor.ID = "pc_other"
	otherActor.Claim.Actor.CredentialID = "pc_other"
	_, err = s.AdmitBackgroundRun(context.Background(), otherActor)
	if !errors.Is(err, ErrIdempotencyOwnerMismatch) {
		t.Fatalf("actor conflict = %v", err)
	}
	assertCounts(t, s, 1, 1)
}

func TestAdmissionRollsBackOnLateInsertFailure(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	first := testAdmission(6, "first", "First")
	if _, err := s.AdmitBackgroundRun(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := testAdmission(7, "second", "Second")
	second.OpenCodeSessionID = first.OpenCodeSessionID // Fails on the late run insert.
	if _, err := s.AdmitBackgroundRun(context.Background(), second); err == nil {
		t.Fatal("expected duplicate session failure")
	}
	if _, err := readRun(context.Background(), s.db, testWorkspaceID(), second.RunID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partially written run: %v", err)
	}
	if _, found, err := s.FindReceiptByIdempotency(context.Background(), testWorkspaceID(), CreateBackgroundRunCommand, second.Claim.Key); err != nil || found {
		t.Fatalf("partially written receipt: %v", err)
	}
	assertCounts(t, s, 1, 1)
}

func TestRunSessionIdentityAndInputConstraints(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)
	first := testAdmission(30, "first-run", "First")
	if _, err := s.AdmitBackgroundRun(context.Background(), first); err != nil {
		t.Fatal(err)
	}

	sameSession := testAdmission(31, "same-session", "Second")
	sameSession.OpenCodeSessionID = first.OpenCodeSessionID
	if _, err := s.AdmitBackgroundRun(context.Background(), sameSession); err == nil {
		t.Fatal("duplicate OpenCode session was accepted")
	}

	sameMessage := testAdmission(32, "same-message", "Third")
	sameMessage.OpenCodeMessageID = first.OpenCodeMessageID
	if _, err := s.AdmitBackgroundRun(context.Background(), sameMessage); err != nil {
		t.Fatalf("message ID in another session should be independent: %v", err)
	}

	for column, value := range map[string]any{"base_oid": "89abcdef0123456789abcdef0123456789abcdef", "prompt": "Changed", "deadline": 1} {
		if _, err := s.db.Exec(`UPDATE runs SET `+column+`=?,revision=revision+1 WHERE id=?`, value, first.RunID); err == nil {
			t.Fatalf("immutable run %s was updated", column)
		}
	}
	assertCounts(t, s, 2, 2)
}

func TestSchemaRejectsMalformedRunInputs(t *testing.T) {
	s := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = s.Close() })
	createTestWorkspace(t, s)

	tests := []struct {
		name   string
		mutate func(*AdmitBackgroundRunParams)
	}{
		{"run ID", func(p *AdmitBackgroundRunParams) { p.RunID = "run_bad" }},
		{"session prefix", func(p *AdmitBackgroundRunParams) { p.OpenCodeSessionID = "ses_bad" }},
		{"session uppercase", func(p *AdmitBackgroundRunParams) {
			p.OpenCodeSessionID = domain.OpenCodeSessionID("ses_ABCDEF0123456789abcdef0123456789")
		}},
		{"message prefix", func(p *AdmitBackgroundRunParams) { p.OpenCodeMessageID = "msg_bad" }},
		{"agent", func(p *AdmitBackgroundRunParams) { p.Agent = "" }},
		{"model", func(p *AdmitBackgroundRunParams) { p.Model = string(make([]byte, 257)) }},
		{"deadline", func(p *AdmitBackgroundRunParams) { p.Deadline = p.AcceptedAt }},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testAdmission(60+i, "invalid-"+fmt.Sprint(i), "Invalid")
			tt.mutate(&p)
			// Fern generates these values; schema CHECKs are the backstop.
			if _, err := s.AdmitBackgroundRun(context.Background(), p); err == nil {
				t.Fatal("malformed admission was accepted")
			}
		})
	}
	assertCounts(t, s, 0, 0)
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
workspace_id,command_kind,idempotency_key,request_hash,actor,accepted_at,
api_contract_version,run_id,response_status,response_projection)
VALUES(?,?,?,?,?,?,?,?,?,?)`, testWorkspaceID(), CreateBackgroundRunCommand, "fk", make([]byte, 32), `{}`, 1, "v1", testRunID(20), 202, `{}`)
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
	if _, err := raw.Exec(`INSERT INTO receipts(
workspace_id,command_kind,idempotency_key,request_hash,actor,accepted_at,
api_contract_version,run_id,response_status,response_projection)
VALUES(?,?,?,?,?,?,?,?,?,?)`, testWorkspaceID(), CreateBackgroundRunCommand, "broken-fk", make([]byte, 32), `{}`, 1, "v1", testRunID(21), 202, `{}`); err != nil {
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
	tests := []struct {
		name string
		path func(t *testing.T) string // prepares an unsafe database path
	}{
		{"directory permissions", func(t *testing.T) string {
			dir := t.TempDir()
			must(t, os.Chmod(dir, 0o755))
			return filepath.Join(dir, "tasks.db")
		}},
		{"file permissions", func(t *testing.T) string {
			path := filepath.Join(privateDir(t), "tasks.db")
			must(t, os.WriteFile(path, nil, 0o644))
			return path
		}},
		{"database symlink", func(t *testing.T) string {
			dir := privateDir(t)
			target, link := filepath.Join(dir, "target.db"), filepath.Join(dir, "tasks.db")
			must(t, os.WriteFile(target, nil, 0o600))
			must(t, os.Symlink(target, link))
			return link
		}},
		{"directory symlink", func(t *testing.T) string {
			root := privateDir(t)
			realDir, linkDir := filepath.Join(root, "real"), filepath.Join(root, "link")
			must(t, os.Mkdir(realDir, 0o700))
			must(t, os.Symlink(realDir, linkDir))
			return filepath.Join(linkDir, "tasks.db")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Open(context.Background(), test.path(t)); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("open = %v, want ErrUnsafePath", err)
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err := s.AdmitBackgroundRun(ctx, testAdmission(8, "canceled", "Canceled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("admit canceled context = %v", err)
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
	_, err = s.AdmitBackgroundRun(ctx, testAdmission(9, "deadline", "Deadline"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy admission = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("deadline took %v despite context cancellation", elapsed)
	}
	assertCounts(t, s, 0, 0)
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
		RepositoryPath: "/srv/fern/workspaces/demo", GitHubAuthority: GitHubAuthorityAppBroker, InstallationID: 123, RepositoryID: 987654321,
		RepositoryFullName: "owner/repository", ImageDigest: "sha256:image", OpenCodeProtocol: "v2",
		RuntimeDesiredState: "running", ReconciliationEpoch: 1, CreatedAt: testTime,
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
}

func testAdmission(n int, key, prompt string) AdmitBackgroundRunParams {
	hash := sha256.Sum256([]byte(key + "\n" + prompt))
	params := AdmitBackgroundRunParams{
		RunID: testRunID(n), OpenCodeSessionID: testSessionID(n), OpenCodeMessageID: testMessageID(n),
		Claim: domain.IdempotencyClaim{
			Scope: domain.IdempotencyScope{WorkspaceID: testWorkspaceID(), CommandKind: CreateBackgroundRunCommand},
			Key:   domain.IdempotencyKey(key), RequestHash: domain.RequestHash(hash),
			Actor: domain.ActorSnapshot{Type: domain.ActorOpenCode, ID: "pc_owner", DisplayName: "OpenCode", CredentialID: "pc_owner", Authentication: "fern_plugin_bearer", RequestID: "req-1"},
		},
		Prompt: prompt, RepositoryID: 987654321, RepositoryRemote: "https://github.com/owner/repository",
		BaseSHA: domain.GitOID("0123456789abcdef0123456789abcdef01234567"), Branch: "main",
		Profile: domain.SourceProfile, EnvironmentSHA256: sha256.Sum256([]byte("{}")),
		ImageIdentity: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Agent:         "build", ModelProvider: "provider", Model: "model-1",
		Deadline:           testTime.Add(time.Hour),
		APIContractVersion: "v1", AcceptedAt: testTime,
	}
	return params
}

func testWorkspaceID() domain.WorkspaceID { return domain.WorkspaceID(testID("wsp_", 0)) }
func testRunID(n int) domain.RunID        { return domain.RunID(testID("run_", n)) }

func testSessionID(n int) domain.OpenCodeSessionID {
	return domain.OpenCodeSessionID(fmt.Sprintf("ses_%032x", n+1))
}

func testMessageID(n int) domain.OpenCodeMessageID {
	return domain.OpenCodeMessageID(fmt.Sprintf("msg_%032x", n+1))
}

func testResultID(n int) domain.ResultID { return domain.ResultID(testID("res_", n)) }

func testID(prefix string, n int) string {
	return fmt.Sprintf("%s0198d34d-6a50-75fb-b1f2-%012x", prefix, n+1)
}

func assertCounts(t *testing.T, s *Store, runs, receipts int) {
	t.Helper()
	for table, want := range map[string]int{"runs": runs, "receipts": receipts} {
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
