package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

type migration struct {
	version int
	name    string
	sql     string
}

var migrations = []migration{
	{version: 8, name: "run_task_store", sql: initialSchema},
}

// CurrentSchemaVersion is the schema produced by all migrations in this build.
func CurrentSchemaVersion() int { return migrations[0].version }

// initialSchema is the complete pre-release durable schema. There are no
// supported predecessor taskstore schemas; incompatible development databases
// must be deleted and recreated.
const initialSchema = `CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    checksum TEXT NOT NULL CHECK(length(checksum) = 64 AND checksum NOT GLOB '*[^0-9a-f]*')
) STRICT;

CREATE TABLE workspaces (
    id TEXT PRIMARY KEY CHECK(
        length(id) = 40 AND substr(id,1,4) = 'wsp_' AND
        substr(id,13,1) = '-' AND substr(id,18,1) = '-' AND substr(id,19,1) = '7' AND
        substr(id,23,1) = '-' AND substr(id,24,1) IN ('8','9','a','b') AND substr(id,28,1) = '-' AND
        length(replace(substr(id,5),'-','')) = 32 AND
        replace(substr(id,5),'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    name TEXT NOT NULL UNIQUE CHECK(length(CAST(name AS BLOB)) BETWEEN 1 AND 200),
    state TEXT NOT NULL CHECK(state IN ('active','maintenance','recovery_required','disabled')),
    repository_path TEXT NOT NULL UNIQUE CHECK(length(CAST(repository_path AS BLOB)) BETWEEN 1 AND 4096),
    installation_id INTEGER NOT NULL CHECK(installation_id > 0),
    repository_id INTEGER NOT NULL UNIQUE CHECK(repository_id > 0),
    repository_full_name TEXT NOT NULL CHECK(length(CAST(repository_full_name AS BLOB)) BETWEEN 1 AND 512),
    image_digest TEXT NOT NULL CHECK(length(CAST(image_digest AS BLOB)) BETWEEN 1 AND 256),
    opencode_protocol TEXT NOT NULL CHECK(length(CAST(opencode_protocol AS BLOB)) BETWEEN 1 AND 128),
    runtime_desired_state TEXT NOT NULL CHECK(length(CAST(runtime_desired_state AS BLOB)) BETWEEN 1 AND 64),
    reconciliation_epoch INTEGER NOT NULL CHECK(reconciliation_epoch >= 0),
    revision INTEGER NOT NULL CHECK(revision >= 1),
    created_at INTEGER NOT NULL CHECK(created_at >= 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at), github_authority TEXT NOT NULL DEFAULT 'github-app-broker'
CHECK(github_authority='github-app-broker'),
    UNIQUE(id, repository_id)
) STRICT;

CREATE TABLE runs (
    id TEXT PRIMARY KEY CHECK(
        length(id) = 40 AND substr(id,1,4) = 'run_' AND
        substr(id,13,1) = '-' AND substr(id,18,1) = '-' AND substr(id,19,1) = '7' AND
        substr(id,23,1) = '-' AND substr(id,24,1) IN ('8','9','a','b') AND substr(id,28,1) = '-' AND
        length(replace(substr(id,5),'-','')) = 32 AND
        replace(substr(id,5),'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    workspace_id TEXT NOT NULL,
    repository_id INTEGER NOT NULL CHECK(repository_id > 0),
    repository_remote TEXT NOT NULL CHECK(length(CAST(repository_remote AS BLOB)) BETWEEN 1 AND 2048),
    base_oid TEXT NOT NULL CHECK(length(base_oid)=40 AND base_oid NOT GLOB '*[^0-9a-f]*'),
    branch TEXT CHECK(branch IS NULL OR length(CAST(branch AS BLOB)) BETWEEN 1 AND 255),
    prompt TEXT NOT NULL CHECK(length(CAST(prompt AS BLOB)) BETWEEN 1 AND 65536),
    agent TEXT NOT NULL CHECK(length(CAST(agent AS BLOB)) BETWEEN 1 AND 128),
    model_provider TEXT NOT NULL CHECK(length(CAST(model_provider AS BLOB)) BETWEEN 1 AND 128),
    model TEXT NOT NULL CHECK(length(CAST(model AS BLOB)) BETWEEN 1 AND 256),
    deadline INTEGER NOT NULL,
    profile TEXT NOT NULL CHECK(profile='source-39fb919a054190498f6d5b7985bde231f93ad7a6'),
    image_identity TEXT NOT NULL CHECK(length(CAST(image_identity AS BLOB)) BETWEEN 1 AND 256),
    environment_sha256 BLOB NOT NULL CHECK(length(environment_sha256)=32 AND
      lower(hex(environment_sha256))<>'0000000000000000000000000000000000000000000000000000000000000000'),
    resource_spec_version INTEGER NOT NULL CHECK(resource_spec_version=10),
    opencode_session_id TEXT NOT NULL UNIQUE CHECK(
        length(opencode_session_id) = 36 AND substr(opencode_session_id,1,4) = 'ses_' AND
        substr(opencode_session_id,5) NOT GLOB '*[^0-9a-f]*'
    ),
    opencode_message_id TEXT NOT NULL CHECK(
        length(opencode_message_id) = 36 AND substr(opencode_message_id,1,4) = 'msg_' AND
        substr(opencode_message_id,5) NOT GLOB '*[^0-9a-f]*'
    ),
    creator_actor TEXT NOT NULL CHECK(json_valid(creator_actor) AND json_type(creator_actor)='object' AND length(CAST(creator_actor AS BLOB)) <= 2048),
    state TEXT NOT NULL CHECK(state IN ('queued','setting_up','working','needs_you','canceling','uncertain','result_ready','failed','cleanup_required')),
    effect_phase TEXT NOT NULL CHECK(effect_phase IN ('absent','provisioning','prompt_pending','admitted','sealing','cleaning','cleanup_complete')),
    stop_receipt_id INTEGER REFERENCES receipts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    stop_requested_at INTEGER,
    timeout_requested_at INTEGER CHECK(timeout_requested_at IS NULL OR timeout_requested_at BETWEEN created_at AND updated_at),
    observed_container_id TEXT CHECK(observed_container_id IS NULL OR length(CAST(observed_container_id AS BLOB)) BETWEEN 1 AND 128),
    observed_container_started_at TEXT CHECK(observed_container_started_at IS NULL OR length(CAST(observed_container_started_at AS BLOB)) BETWEEN 1 AND 64),
    runtime_epoch INTEGER CHECK(runtime_epoch IS NULL OR runtime_epoch > 0),
    host_port INTEGER CHECK(host_port IS NULL OR host_port BETWEEN 1 AND 65535),
    prompt_request_attempted_at INTEGER CHECK(prompt_request_attempted_at IS NULL OR prompt_request_attempted_at BETWEEN created_at AND updated_at),
    seal_receipt_id INTEGER REFERENCES receipts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    seal_requested_at INTEGER,
    seal_policy_version TEXT CHECK(seal_policy_version IS NULL OR length(CAST(seal_policy_version AS BLOB)) BETWEEN 1 AND 128),
    result_id TEXT UNIQUE CHECK(result_id IS NULL OR (length(result_id)=40 AND substr(result_id,1,4)='res_' AND substr(result_id,19,1)='7' AND
      replace(substr(result_id,5),'-','') NOT GLOB '*[^0-9a-f]*')),
    writer_fence_kind TEXT CHECK(writer_fence_kind IS NULL OR writer_fence_kind IN ('never_created','never_started','runtime_stopped')),
    writer_fence_container_id TEXT,
    writer_fence_started_at TEXT,
    writer_fence_token TEXT CHECK(writer_fence_token IS NULL OR length(CAST(writer_fence_token AS BLOB)) BETWEEN 1 AND 128),
    writer_fence_stopped_at INTEGER,
    last_evidence TEXT CHECK(last_evidence IS NULL OR length(CAST(last_evidence AS BLOB)) BETWEEN 1 AND 4096),
    last_error TEXT CHECK(last_error IS NULL OR length(CAST(last_error AS BLOB)) BETWEEN 1 AND 4096),
    cleanup_proof TEXT CHECK(cleanup_proof IS NULL OR length(CAST(cleanup_proof AS BLOB)) BETWEEN 1 AND 4096),
    revision INTEGER NOT NULL CHECK(revision >= 1),
    created_at INTEGER NOT NULL CHECK(created_at >= 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
    CHECK(deadline > created_at),
    CHECK(
        (state='queued' AND effect_phase='absent') OR
        (state='setting_up' AND effect_phase IN ('provisioning','prompt_pending')) OR
        (state='uncertain' AND effect_phase IN ('prompt_pending','admitted')) OR
        (state IN ('working','needs_you') AND effect_phase='admitted') OR
        (state='canceling' AND effect_phase IN ('sealing','cleaning')) OR
        (state='cleanup_required' AND effect_phase='cleaning') OR
        (state='result_ready' AND effect_phase IN ('cleaning','cleanup_complete')) OR
        (state='failed' AND effect_phase='cleanup_complete')
    ),
    CHECK((stop_receipt_id IS NULL AND stop_requested_at IS NULL) OR
          (stop_receipt_id IS NOT NULL AND stop_requested_at IS NOT NULL AND state IN ('canceling','cleanup_required','failed'))),
    CHECK(state<>'canceling' OR stop_receipt_id IS NOT NULL OR effect_phase='sealing'),
    CHECK((observed_container_id IS NULL AND observed_container_started_at IS NULL AND runtime_epoch IS NULL AND host_port IS NULL) OR
          (observed_container_id IS NOT NULL AND observed_container_started_at IS NOT NULL AND runtime_epoch IS NOT NULL AND host_port IS NOT NULL)),
    CHECK(effect_phase NOT IN ('prompt_pending','admitted') OR (observed_container_id IS NOT NULL AND prompt_request_attempted_at IS NOT NULL)),
    CHECK((seal_receipt_id IS NULL AND seal_requested_at IS NULL AND seal_policy_version IS NULL AND result_id IS NULL) OR
          (seal_receipt_id IS NOT NULL AND seal_requested_at IS NOT NULL AND seal_policy_version IS NOT NULL AND result_id IS NOT NULL)),
    CHECK(effect_phase<>'sealing' OR seal_receipt_id IS NOT NULL),
    CHECK(state<>'result_ready' OR seal_receipt_id IS NOT NULL),
    CHECK(writer_fence_kind IS NULL OR seal_receipt_id IS NOT NULL),
    CHECK((writer_fence_kind IS NULL AND writer_fence_container_id IS NULL AND writer_fence_started_at IS NULL AND writer_fence_token IS NULL AND writer_fence_stopped_at IS NULL) OR
          (writer_fence_kind='never_created' AND writer_fence_container_id IS NULL AND writer_fence_started_at IS NULL AND writer_fence_token IS NULL AND writer_fence_stopped_at IS NULL) OR
          (writer_fence_kind='never_started' AND writer_fence_container_id IS NOT NULL AND writer_fence_started_at IS NULL AND writer_fence_token IS NULL AND writer_fence_stopped_at IS NULL) OR
          (writer_fence_kind='runtime_stopped' AND writer_fence_container_id=observed_container_id AND writer_fence_started_at=observed_container_started_at AND
           writer_fence_token IS NOT NULL AND writer_fence_stopped_at IS NOT NULL)),
    CHECK((effect_phase='cleanup_complete')=(cleanup_proof IS NOT NULL)),
    FOREIGN KEY(workspace_id,repository_id) REFERENCES workspaces(id,repository_id) ON UPDATE RESTRICT ON DELETE RESTRICT
) STRICT;

CREATE INDEX runs_workspace_created ON runs(workspace_id,created_at DESC,id DESC);

CREATE INDEX runs_next ON runs(workspace_id,effect_phase,updated_at,id);

CREATE UNIQUE INDEX runs_workspace_capacity_one ON runs(workspace_id)
  WHERE effect_phase NOT IN ('absent','cleanup_complete');

CREATE TRIGGER runs_immutable_inputs BEFORE UPDATE ON runs
WHEN NEW.id<>OLD.id OR NEW.workspace_id<>OLD.workspace_id OR NEW.repository_id<>OLD.repository_id OR
     NEW.repository_remote<>OLD.repository_remote OR NEW.base_oid<>OLD.base_oid OR NEW.branch IS NOT OLD.branch OR
     NEW.prompt<>OLD.prompt OR NEW.agent<>OLD.agent OR
     NEW.model_provider<>OLD.model_provider OR NEW.model<>OLD.model OR NEW.deadline<>OLD.deadline OR
     NEW.profile<>OLD.profile OR NEW.image_identity<>OLD.image_identity OR NEW.environment_sha256<>OLD.environment_sha256 OR
     NEW.resource_spec_version<>OLD.resource_spec_version OR NEW.opencode_session_id<>OLD.opencode_session_id OR
     NEW.opencode_message_id<>OLD.opencode_message_id OR NEW.creator_actor<>OLD.creator_actor OR NEW.created_at<>OLD.created_at OR
     NEW.revision<>OLD.revision+1 OR NEW.updated_at<OLD.updated_at
BEGIN SELECT RAISE(ABORT, 'run inputs are immutable and revisions advance by one'); END;

CREATE TRIGGER runs_recorded_once BEFORE UPDATE ON runs
WHEN (OLD.stop_receipt_id IS NOT NULL AND (NEW.stop_receipt_id IS NOT OLD.stop_receipt_id OR NEW.stop_requested_at IS NOT OLD.stop_requested_at)) OR
     (OLD.timeout_requested_at IS NOT NULL AND NEW.timeout_requested_at IS NOT OLD.timeout_requested_at) OR
     (OLD.prompt_request_attempted_at IS NOT NULL AND NEW.prompt_request_attempted_at IS NOT OLD.prompt_request_attempted_at) OR
     (OLD.observed_container_id IS NOT NULL AND (NEW.observed_container_id IS NOT OLD.observed_container_id OR
       NEW.observed_container_started_at IS NOT OLD.observed_container_started_at OR NEW.runtime_epoch IS NOT OLD.runtime_epoch OR
       NEW.host_port IS NOT OLD.host_port)) OR
     (OLD.seal_receipt_id IS NOT NULL AND (NEW.seal_receipt_id IS NOT OLD.seal_receipt_id OR NEW.seal_requested_at IS NOT OLD.seal_requested_at OR
       NEW.seal_policy_version IS NOT OLD.seal_policy_version OR NEW.result_id IS NOT OLD.result_id)) OR
     (OLD.writer_fence_kind IS NOT NULL AND (NEW.writer_fence_kind IS NOT OLD.writer_fence_kind OR
       NEW.writer_fence_container_id IS NOT OLD.writer_fence_container_id OR NEW.writer_fence_started_at IS NOT OLD.writer_fence_started_at OR
       NEW.writer_fence_token IS NOT OLD.writer_fence_token OR NEW.writer_fence_stopped_at IS NOT OLD.writer_fence_stopped_at))
BEGIN SELECT RAISE(ABORT, 'recorded run authority is immutable'); END;

CREATE TRIGGER runs_seal_retains_resources BEFORE UPDATE OF effect_phase ON runs
WHEN OLD.effect_phase='sealing' AND NEW.effect_phase<>'sealing' AND NOT EXISTS (
    SELECT 1 FROM results result WHERE result.id=OLD.result_id AND result.run_id=OLD.id AND result.state='sealed')
BEGIN SELECT RAISE(ABORT,'sealed run has no committed result'); END;

CREATE TRIGGER runs_terminal_immutable BEFORE UPDATE ON runs
WHEN OLD.effect_phase='cleanup_complete'
BEGIN SELECT RAISE(ABORT,'terminal run is immutable'); END;

CREATE TRIGGER runs_terminal_after_cleanup BEFORE UPDATE OF effect_phase ON runs
WHEN NEW.effect_phase='cleanup_complete' AND OLD.effect_phase NOT IN ('absent','cleaning')
BEGIN SELECT RAISE(ABORT,'run resources were not cleaned'); END;

CREATE TABLE receipts (
    id INTEGER PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    command_kind TEXT NOT NULL CHECK(command_kind IN ('run.create','run.stop','run.seal')),
    idempotency_key TEXT NOT NULL CHECK(length(CAST(idempotency_key AS BLOB)) BETWEEN 1 AND 128),
    request_hash BLOB NOT NULL CHECK(length(request_hash) = 32),
    actor TEXT NOT NULL CHECK(json_valid(actor) AND json_type(actor)='object' AND length(CAST(actor AS BLOB)) <= 2048),
    accepted_at INTEGER NOT NULL CHECK(accepted_at >= 0),
    api_contract_version TEXT NOT NULL CHECK(length(CAST(api_contract_version AS BLOB)) BETWEEN 1 AND 64),
    run_id TEXT NOT NULL REFERENCES runs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    response_status INTEGER NOT NULL CHECK(response_status BETWEEN 200 AND 299),
    response_projection TEXT NOT NULL CHECK(length(CAST(response_projection AS BLOB)) <= 65536 AND json_valid(response_projection)),
    UNIQUE(workspace_id, command_kind, idempotency_key)
) STRICT;

CREATE INDEX receipts_run ON receipts(run_id);

CREATE TRIGGER receipts_immutable_update BEFORE UPDATE ON receipts
BEGIN SELECT RAISE(ABORT, 'receipts are immutable'); END;

CREATE TRIGGER receipts_immutable_delete BEFORE DELETE ON receipts
BEGIN SELECT RAISE(ABORT, 'receipts are immutable'); END;

CREATE TABLE results (
  id TEXT PRIMARY KEY CHECK(length(id)=40 AND substr(id,1,4)='res_' AND substr(id,13,1)='-' AND substr(id,18,1)='-' AND substr(id,19,1)='7' AND
    substr(id,23,1)='-' AND substr(id,24,1) IN ('8','9','a','b') AND substr(id,28,1)='-' AND replace(substr(id,5),'-','') NOT GLOB '*[^0-9a-f]*'),
  run_id TEXT NOT NULL UNIQUE REFERENCES runs(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  state TEXT NOT NULL CHECK(state IN ('selected','sealed')),
  outcome TEXT NOT NULL CHECK(outcome IN ('changed','no_changes')),
  base_sha TEXT NOT NULL CHECK(length(base_sha)=40 AND base_sha NOT GLOB '*[^0-9a-f]*'),
  result_commit TEXT NOT NULL CHECK(length(result_commit)=40 AND result_commit NOT GLOB '*[^0-9a-f]*'),
  tree_oid TEXT NOT NULL CHECK(length(tree_oid)=40 AND tree_oid NOT GLOB '*[^0-9a-f]*'),
  change_count INTEGER NOT NULL CHECK(change_count>=0),
  changes_sha256 BLOB NOT NULL CHECK(length(changes_sha256)=32),
  manifest_json TEXT NOT NULL CHECK(json_valid(manifest_json) AND json_type(manifest_json)='object' AND length(CAST(manifest_json AS BLOB))<=4194304),
  manifest_sha256 BLOB NOT NULL UNIQUE CHECK(length(manifest_sha256)=32),
  bundle_sha256 BLOB NOT NULL CHECK(length(bundle_sha256)=32),
  bundle_size INTEGER NOT NULL CHECK(bundle_size>=0),
  collected_at INTEGER NOT NULL CHECK(collected_at>=0),
  materialization_sha256 BLOB CHECK(materialization_sha256 IS NULL OR length(materialization_sha256)=32),
  sealed_at INTEGER,
  CHECK((state='selected' AND materialization_sha256 IS NULL AND sealed_at IS NULL) OR
        (state='sealed' AND materialization_sha256 IS NOT NULL AND sealed_at>=collected_at)),
  CHECK((outcome='no_changes' AND result_commit=base_sha AND change_count=0) OR
        (outcome='changed' AND result_commit<>base_sha AND change_count>0))
) STRICT;

CREATE TRIGGER results_seal_once BEFORE UPDATE ON results WHEN OLD.state<>'selected' OR NEW.state<>'sealed' OR
  NEW.id<>OLD.id OR NEW.run_id<>OLD.run_id OR NEW.outcome<>OLD.outcome OR NEW.base_sha<>OLD.base_sha OR
  NEW.result_commit<>OLD.result_commit OR NEW.tree_oid<>OLD.tree_oid OR NEW.change_count<>OLD.change_count OR
  NEW.changes_sha256<>OLD.changes_sha256 OR NEW.manifest_json<>OLD.manifest_json OR NEW.manifest_sha256<>OLD.manifest_sha256 OR
  NEW.bundle_sha256<>OLD.bundle_sha256 OR NEW.bundle_size<>OLD.bundle_size OR NEW.collected_at<>OLD.collected_at
BEGIN SELECT RAISE(ABORT,'result is immutable'); END;

CREATE TRIGGER results_durable BEFORE DELETE ON results BEGIN SELECT RAISE(ABORT,'result is durable'); END;
`

func (s *Store) initialize(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect task database: %w", err)
	}
	defer conn.Close()
	if err := setContextBusyTimeout(ctx, conn); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeoutMS))
	}()
	// Reject unsupported stores before changing persistent journal policy. The
	// version is checked again under the migration lock below.
	preflightVersion, err := readUserVersion(ctx, conn)
	if err != nil {
		return err
	}
	if preflightVersion != 0 && preflightVersion != CurrentSchemaVersion() {
		return fmt.Errorf("%w: user_version %d", ErrUnsupportedSchema, preflightVersion)
	}
	var journal string
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&journal); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("enable WAL: %w", err)
	}
	if journal != "wal" {
		return fmt.Errorf("%w: SQLite refused WAL mode: %s", ErrCorruptStore, journal)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("lock task migrations: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	version, err := readUserVersion(ctx, conn)
	if err != nil {
		return err
	}
	if version != 0 && version != CurrentSchemaVersion() {
		return fmt.Errorf("%w: user_version %d", ErrUnsupportedSchema, version)
	}
	if err := verifyMigrationLedger(ctx, conn, version); err != nil {
		return err
	}
	pending := migrations
	if version == CurrentSchemaVersion() {
		pending = nil
	}
	for _, m := range pending {
		if _, err := conn.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("apply migration %d: %w", m.version, err)
		}
		sum := migrationChecksum(m)
		if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,checksum) VALUES(?,?,?)`, m.version, m.name, sum); err != nil {
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			return fmt.Errorf("set schema version %d: %w", m.version, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit task migrations: %w", err)
	}
	committed = true
	if _, err := conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeoutMS)); err != nil {
		return fmt.Errorf("restore SQLite busy timeout: %w", err)
	}
	if err := s.checkDatabase(ctx, conn); err != nil {
		return err
	}
	return nil
}

func readUserVersion(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (int, error) {
	var version int
	if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

func verifyMigrationLedger(ctx context.Context, conn *sql.Conn, version int) error {
	if version == 0 {
		var count int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`).Scan(&count); err != nil {
			return fmt.Errorf("inspect empty schema: %w", err)
		}
		if count != 0 {
			return fmt.Errorf("%w: objects exist at user_version 0", ErrMigrationDrift)
		}
		return nil
	}
	rows, err := conn.QueryContext(ctx, `SELECT version,name,checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return fmt.Errorf("%w: missing migration ledger: %v", ErrMigrationDrift, err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var gotVersion int
		var name, checksum string
		if err := rows.Scan(&gotVersion, &name, &checksum); err != nil {
			return fmt.Errorf("read migration ledger: %w", err)
		}
		seen++
		if seen > len(migrations) || gotVersion != migrations[seen-1].version {
			return fmt.Errorf("%w: unknown migration %d", ErrUnsupportedSchema, gotVersion)
		}
		expected := migrations[seen-1]
		if name != expected.name || checksum != migrationChecksum(expected) {
			return fmt.Errorf("%w: migration %d", ErrMigrationDrift, gotVersion)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read migration ledger: %w", err)
	}
	if seen != len(migrations) {
		return fmt.Errorf("%w: ledger has %d entries at user_version %d", ErrMigrationDrift, seen, version)
	}
	return nil
}

func migrationChecksum(m migration) string {
	sum := sha256.Sum256([]byte(m.sql))
	return hex.EncodeToString(sum[:])
}

func (s *Store) checkDatabase(ctx context.Context, conn *sql.Conn) error {
	var integrity string
	if err := conn.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("%w: integrity_check: %s", ErrCorruptStore, integrity)
	}
	rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	if rows.Next() {
		_ = rows.Close()
		return fmt.Errorf("%w: foreign key violation", ErrCorruptStore)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	var foreignKeys, busyTimeout, synchronous int
	var journal string
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		return err
	}
	if foreignKeys != 1 || busyTimeout != busyTimeoutMS || synchronous != 2 || journal != "wal" {
		return fmt.Errorf("%w: unsafe SQLite policy fk=%d busy=%d synchronous=%d journal=%s", ErrCorruptStore, foreignKeys, busyTimeout, synchronous, journal)
	}
	return nil
}

func rollback(tx *sql.Tx, errp *error) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) && *errp == nil {
		*errp = err
	}
}
