package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

func TestMigrationThreeFromVersionTwo(t *testing.T) {
	path := testDBPath(t)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:2] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(version,name,checksum) VALUES(?,?,?)`, migration.version, migration.name, migrationChecksum(migration)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version=2`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	var version, tables int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('journal_events','verifications','publications')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion() || tables != 3 {
		t.Fatalf("migration version=%d tables=%d", version, tables)
	}
}

func prepareJournalVerification(t *testing.T, store *Store, sealed SealedResult, n int) VerificationRecord {
	t.Helper()
	evidence := journalEvidence()
	params := PrepareVerificationParams{
		VerificationID: task.VerificationID(testID("ver_", n)), ResultID: sealed.Result.ID,
		ExpectedTaskRevision: sealed.Task.Revision, ExpectedAttemptRevision: sealed.Attempt.Revision,
		EventID: testEventID(n), PolicyName: "go-test", PolicySHA256: sha256.Sum256([]byte("policy")),
		VerifiedCommit: sealed.Result.ResultCommit, WorkingDirectory: "", Timeout: time.Minute, OutputLimitBytes: 1024,
		RunnerName: "fern-verifier", RunnerVersion: "v1", ImageDigest: "sha256:image",
		EnvironmentSHA256: sha256.Sum256([]byte("environment")), PreparedAt: sealed.Result.UpdatedAt.Add(time.Millisecond),
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: testDeliveryActor(),
	}
	prepared, err := store.PrepareVerification(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.PrepareVerification(context.Background(), params)
	if err != nil || !replay.Replayed {
		t.Fatalf("verification preparation replay: %+v, %v", replay, err)
	}
	return prepared
}

func prepareJournalPublication(t *testing.T, store *Store, sealed SealedResult, verification VerificationRecord, n int) PublicationRecord {
	t.Helper()
	operationID := task.PublicationOperationID(testID("op_", n))
	requestHash := sha256.Sum256([]byte("journal publication" + string(sealed.Result.ID)))
	params := AdmitPublicationParams{PublicationID: task.PublicationID(testID("pub_", n)), OperationID: operationID,
		ReceiptID: testReceiptID(n), EventID: testEventID(n), ResultID: sealed.Result.ID,
		VerificationID: verification.Verification.ID,
		Claim: task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: sealed.Result.WorkspaceID, CommandKind: PublishResultCommand},
			Key: task.IdempotencyKey(testID("publish-", n)), RequestHash: requestHash, Actor: testDeliveryActor()},
		BrokerPolicyVersion: "github-v1", BrokerPolicySHA256: sha256.Sum256([]byte("broker-policy")),
		APIContractVersion: "fern.task.v1", AcceptedAt: verification.Verification.UpdatedAt.Add(time.Millisecond)}
	admitted, err := store.AdmitPublication(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	return PublicationRecord{Publication: admitted.Publication, Event: admitted.Event}
}

func journalEvidence() json.RawMessage { return json.RawMessage(`{"authorityRead":true,"objects":1}`) }
