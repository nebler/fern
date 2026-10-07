package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// schemaVersion is the PRAGMA user_version of schema. There are no supported
// predecessor schemas: Open applies schema to an empty database, opens a
// database already at schemaVersion, and refuses every other version.
// Incompatible development databases must be deleted and recreated.
const schemaVersion = 12

const schema = `CREATE TABLE workspaces (
    id TEXT PRIMARY KEY CHECK(
        length(id) = 40 AND substr(id,1,4) = 'wsp_' AND
        substr(id,13,1) = '-' AND substr(id,18,1) = '-' AND substr(id,19,1) = '7' AND
        substr(id,23,1) = '-' AND substr(id,24,1) IN ('8','9','a','b') AND substr(id,28,1) = '-' AND
        length(replace(substr(id,5),'-','')) = 32 AND
        replace(substr(id,5),'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    name TEXT NOT NULL UNIQUE CHECK(length(CAST(name AS BLOB)) BETWEEN 1 AND 200),
    state TEXT NOT NULL CHECK(state='active'),
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

-- Paired browser devices and the operator credential identifier (package
-- control). Only the SHA-256 of a device bearer token is stored; the device ID
-- is its first 16 hex digits. Times are Unix nanoseconds.
CREATE TABLE devices (
    token_sha256 TEXT PRIMARY KEY CHECK(length(token_sha256)=64 AND token_sha256 NOT GLOB '*[^0-9a-f]*'),
    name TEXT NOT NULL CHECK(length(CAST(name AS BLOB)) BETWEEN 1 AND 80),
    created_at INTEGER NOT NULL,
    last_seen INTEGER NOT NULL,
    expires_at INTEGER NOT NULL CHECK(expires_at > created_at)
) STRICT;

CREATE TABLE operator_credential (
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
    id TEXT NOT NULL
) STRICT;

-- Plugin device authorization (package pluginauth). Only domain-separated
-- SHA-256 digests of the device and user codes are stored. Times are Unix
-- nanoseconds. A credential lives and dies with its authorization.
CREATE TABLE plugin_authorizations (
    id TEXT PRIMARY KEY,
    device_sha256 TEXT NOT NULL UNIQUE CHECK(length(device_sha256)=64 AND device_sha256 NOT GLOB '*[^0-9a-f]*'),
    user_sha256 TEXT NOT NULL CHECK(length(user_sha256)=64 AND user_sha256 NOT GLOB '*[^0-9a-f]*'),
    state TEXT NOT NULL CHECK(state IN ('pending','approved','denied','expired')),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL CHECK(expires_at > created_at),
    last_polled_at INTEGER,
    decided_at INTEGER,
    decided_by TEXT CHECK(decided_by IS NULL OR json_valid(decided_by)),
    CHECK((state='pending') = (decided_at IS NULL))
) STRICT;

CREATE TABLE plugin_credentials (
    id TEXT PRIMARY KEY,
    authorization_id TEXT NOT NULL UNIQUE REFERENCES plugin_authorizations(id) ON DELETE CASCADE,
    state TEXT NOT NULL CHECK(state IN ('active','revoked','expired')),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL CHECK(expires_at > created_at),
    revoked_at INTEGER,
    approved_by TEXT NOT NULL CHECK(json_valid(approved_by)),
    revoked_by TEXT CHECK(revoked_by IS NULL OR json_valid(revoked_by)),
    CHECK((state='revoked') = (revoked_at IS NOT NULL))
) STRICT;

CREATE TABLE plugin_invalid_polls (at INTEGER NOT NULL) STRICT;
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
	// Reject unsupported stores before changing persistent journal policy.
	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version != 0 && version != schemaVersion {
		return fmt.Errorf("%w: user_version %d", ErrUnsupportedSchema, version)
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
	if version == 0 {
		if err := applySchema(ctx, conn); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeoutMS)); err != nil {
		return fmt.Errorf("restore SQLite busy timeout: %w", err)
	}
	return s.checkDatabase(ctx, conn)
}

// applySchema creates the schema and stamps its version in one transaction.
func applySchema(ctx context.Context, conn *sql.Conn) (err error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema creation: %w", err)
	}
	defer rollback(tx, &err)
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema creation: %w", err)
	}
	return nil
}

// checkDatabase validates every opened store, including restored and durable
// ones: SQLite integrity, foreign keys, and the connection policy.
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
