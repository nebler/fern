package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

func TestMigrationSixQuarantinesPendingUnreceiptedPublications(t *testing.T) {
	for index, state := range []PublicationState{PublicationPrepared, PublicationRunning, PublicationUncertain} {
		t.Run(string(state), func(t *testing.T) {
			path, publicationID, workspaceID := pendingVersionFivePublication(t, 47000+index*100, state)

			before, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			var version, receiptColumn int
			if err := before.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			rows, err := before.Query(`PRAGMA table_info(publications)`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var cid, notNull, primaryKey int
				var name, kind string
				var defaultValue any
				if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
					t.Fatal(err)
				}
				if name == "admission_receipt_id" {
					receiptColumn++
				}
			}
			if err := errors.Join(rows.Err(), rows.Close(), before.Close()); err != nil {
				t.Fatal(err)
			}
			if version != 5 || receiptColumn != 0 {
				t.Fatalf("fixture schema version=%d receipt columns=%d", version, receiptColumn)
			}

			store := openTestStore(t, path)
			t.Cleanup(func() { _ = store.Close() })
			publication, err := store.GetPublication(context.Background(), publicationID)
			if err != nil || publication.State != state || publication.AdmissionReceiptID != "" {
				t.Fatalf("migrated publication = %+v, error = %v", publication, err)
			}
			if _, err := store.FindPublicationWork(context.Background(), workspaceID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("legacy publication remained executable: %v", err)
			}
			if _, err := store.db.Exec(`UPDATE publications SET updated_at=updated_at+1 WHERE id=?`, publicationID); err == nil {
				t.Fatal("legacy publication was not mutation-fenced")
			}
			var receipts int
			if err := store.db.QueryRow(`SELECT count(*) FROM receipts WHERE command_kind=?`, PublishResultCommand).Scan(&receipts); err != nil || receipts != 0 {
				t.Fatalf("migration synthesized publication receipts: count=%d error=%v", receipts, err)
			}
		})
	}
}

func pendingVersionFivePublication(t *testing.T, n int, state PublicationState) (string, task.PublicationID, task.WorkspaceID) {
	t.Helper()
	store, sealed, verified := publicationAdmissionFixture(t, n)
	params := testPublicationAdmission(sealed, verified, n+30, task.IdempotencyKey("pre-v6-publication"))
	admitted, err := store.AdmitPublication(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	record := PublicationRecord{Publication: admitted.Publication, Event: admitted.Event}
	if state != PublicationPrepared {
		record = advanceJournalPublication(t, store, sealed, record, PublicationPhaseNone, PublicationPhasePushStarted, "", n+40)
	}
	if state == PublicationUncertain {
		evidence := journalEvidence()
		record, err = store.RecoverPublication(context.Background(), RecoverPublicationParams{
			PublicationID: record.Publication.ID, ExpectedRevision: record.Publication.Revision,
			ExpectedTaskRevision: sealed.Task.Revision, ExpectedAttemptRevision: sealed.Attempt.Revision,
			EventID: testEventID(n + 41), State: PublicationUncertain, Reason: "pre_v6_effect_disposition_unknown",
			RecoveredAt: record.Publication.UpdatedAt.Add(time.Millisecond), EvidencePayload: evidence,
			EvidenceSHA256: digestBytes(evidence), Actor: testDeliveryActor(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	if _, err := store.db.Exec(`DROP TRIGGER publications_admission_receipt_immutable;
DROP TRIGGER publications_admission_receipt_insert;
DROP TRIGGER publications_unreceipted_quarantine;
DROP INDEX publications_admission_receipt;
DROP TRIGGER receipts_immutable_delete;
ALTER TABLE publications DROP COLUMN admission_receipt_id;
DELETE FROM receipts WHERE id=?;
CREATE TRIGGER receipts_immutable_delete BEFORE DELETE ON receipts
BEGIN SELECT RAISE(ABORT, 'receipts are immutable'); END;
DELETE FROM schema_migrations WHERE version=6;
PRAGMA user_version=5`, admitted.Receipt.ID); err != nil {
		t.Fatal(err)
	}
	path := store.Path()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path, record.Publication.ID, sealed.Result.WorkspaceID
}

func digestBytes(value []byte) [32]byte {
	return sha256.Sum256(value)
}
