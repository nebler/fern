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
	{version: 5, name: "retained_result_task_store", sql: initialSchema},
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

CREATE TABLE actor_snapshots (
    id INTEGER PRIMARY KEY,
    actor_type TEXT NOT NULL CHECK(actor_type IN ('device','operator','system','opencode','github_app','recovery')),
    actor_id TEXT NOT NULL CHECK(length(CAST(actor_id AS BLOB)) BETWEEN 1 AND 256),
    display_name TEXT NOT NULL CHECK(length(CAST(display_name AS BLOB)) <= 200),
    credential_id TEXT NOT NULL CHECK(length(CAST(credential_id AS BLOB)) BETWEEN 1 AND 256),
    authentication TEXT NOT NULL CHECK(length(CAST(authentication AS BLOB)) BETWEEN 1 AND 128),
    request_id TEXT NOT NULL CHECK(length(CAST(request_id AS BLOB)) BETWEEN 1 AND 128),
    UNIQUE(actor_type, actor_id, display_name, credential_id, authentication, request_id)
) STRICT;

CREATE TRIGGER actor_snapshots_immutable_update BEFORE UPDATE ON actor_snapshots
BEGIN SELECT RAISE(ABORT, 'actor snapshots are immutable'); END;

CREATE TRIGGER actor_snapshots_immutable_delete BEFORE DELETE ON actor_snapshots
BEGIN SELECT RAISE(ABORT, 'actor snapshots are immutable'); END;

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

CREATE TABLE tasks (
    id TEXT PRIMARY KEY CHECK(
        length(id) = 40 AND substr(id,1,4) = 'tsk_' AND
        substr(id,13,1) = '-' AND substr(id,18,1) = '-' AND substr(id,19,1) = '7' AND
        substr(id,23,1) = '-' AND substr(id,24,1) IN ('8','9','a','b') AND substr(id,28,1) = '-' AND
        length(replace(substr(id,5),'-','')) = 32 AND
        replace(substr(id,5),'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    workspace_id TEXT NOT NULL,
    title TEXT NOT NULL CHECK(length(CAST(title AS BLOB)) BETWEEN 1 AND 200),
    prompt TEXT NOT NULL CHECK(length(CAST(prompt AS BLOB)) BETWEEN 1 AND 65536),
    prompt_sha256 BLOB NOT NULL CHECK(length(prompt_sha256) = 32),
    repository_id INTEGER NOT NULL CHECK(repository_id > 0),
    base_ref TEXT NOT NULL CHECK(length(CAST(base_ref AS BLOB)) BETWEEN 1 AND 255),
    base_sha TEXT NOT NULL CHECK(length(base_sha) = 40 AND base_sha NOT GLOB '*[^0-9a-f]*'),
    object_format TEXT NOT NULL CHECK(object_format = 'sha1'),
    state TEXT NOT NULL CHECK(state IN ('queued','failed','completed')),
    terminal_reason TEXT CHECK(terminal_reason IS NULL OR length(CAST(terminal_reason AS BLOB)) BETWEEN 1 AND 1000),
    current_attempt_id TEXT NOT NULL,
    actor_snapshot_id INTEGER NOT NULL REFERENCES actor_snapshots(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    latest_event_cursor INTEGER NOT NULL DEFAULT 0 CHECK(latest_event_cursor >= 0),
    revision INTEGER NOT NULL CHECK(revision >= 1),
    created_at INTEGER NOT NULL CHECK(created_at >= 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at), sealed_result_id TEXT REFERENCES results(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(workspace_id, repository_id) REFERENCES workspaces(id, repository_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(current_attempt_id, id) REFERENCES attempts(id, task_id) ON UPDATE RESTRICT ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY(latest_event_cursor, id) REFERENCES events(cursor, task_id) ON UPDATE RESTRICT ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
    UNIQUE(id, workspace_id)
) STRICT;

CREATE INDEX tasks_workspace_created ON tasks(workspace_id, created_at, id);

CREATE INDEX tasks_workspace_state ON tasks(workspace_id, state);

CREATE TABLE attempts (
    id TEXT PRIMARY KEY CHECK(
        length(id) = 40 AND substr(id,1,4) = 'att_' AND
        substr(id,13,1) = '-' AND substr(id,18,1) = '-' AND substr(id,19,1) = '7' AND
        substr(id,23,1) = '-' AND substr(id,24,1) IN ('8','9','a','b') AND substr(id,28,1) = '-' AND
        length(replace(substr(id,5),'-','')) = 32 AND
        replace(substr(id,5),'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    task_id TEXT NOT NULL REFERENCES tasks(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    workspace_id TEXT NOT NULL,
    sequence INTEGER NOT NULL CHECK(sequence > 0),
    state TEXT NOT NULL CHECK(state IN ('prepared','failed','superseded')),
    opencode_session_id TEXT NOT NULL CHECK(
        length(opencode_session_id) = 36 AND substr(opencode_session_id,1,4) = 'ses_' AND
        substr(opencode_session_id,5) NOT GLOB '*[^0-9a-f]*'
    ),
    opencode_message_id TEXT NOT NULL CHECK(
        length(opencode_message_id) = 36 AND substr(opencode_message_id,1,4) = 'msg_' AND
        substr(opencode_message_id,5) NOT GLOB '*[^0-9a-f]*'
    ),
    prompt_sha256 BLOB NOT NULL CHECK(length(prompt_sha256) = 32),
    base_sha TEXT NOT NULL CHECK(length(base_sha) = 40 AND base_sha NOT GLOB '*[^0-9a-f]*'),
    image_digest TEXT NOT NULL CHECK(length(CAST(image_digest AS BLOB)) BETWEEN 1 AND 256),
    opencode_protocol TEXT NOT NULL CHECK(length(CAST(opencode_protocol AS BLOB)) BETWEEN 1 AND 128),
    execution_contract_version TEXT NOT NULL CHECK(length(CAST(execution_contract_version AS BLOB)) BETWEEN 1 AND 128),
    agent TEXT NOT NULL CHECK(length(CAST(agent AS BLOB)) BETWEEN 1 AND 128),
    model_provider TEXT NOT NULL CHECK(length(CAST(model_provider AS BLOB)) BETWEEN 1 AND 128),
    model TEXT NOT NULL CHECK(length(CAST(model AS BLOB)) BETWEEN 1 AND 256),
    deadline INTEGER NOT NULL,
    terminal_reason TEXT CHECK(terminal_reason IS NULL OR length(CAST(terminal_reason AS BLOB)) BETWEEN 1 AND 1000),
    revision INTEGER NOT NULL CHECK(revision >= 1),
    created_at INTEGER NOT NULL CHECK(created_at >= 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at), sealed_result_id TEXT REFERENCES results(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    CHECK(deadline > created_at),
    UNIQUE(task_id, sequence),
    UNIQUE(opencode_session_id),
    UNIQUE(opencode_session_id, opencode_message_id),
    FOREIGN KEY(task_id, workspace_id) REFERENCES tasks(id, workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    UNIQUE(id, task_id),
    UNIQUE(id, task_id, workspace_id)
) STRICT;

CREATE INDEX attempts_task_state ON attempts(task_id, state);

CREATE TRIGGER attempts_immutable_inputs BEFORE UPDATE ON attempts
WHEN NEW.id <> OLD.id OR NEW.task_id <> OLD.task_id OR NEW.workspace_id <> OLD.workspace_id OR NEW.sequence <> OLD.sequence OR
     NEW.opencode_session_id <> OLD.opencode_session_id OR NEW.opencode_message_id <> OLD.opencode_message_id OR
     NEW.prompt_sha256 <> OLD.prompt_sha256 OR NEW.base_sha <> OLD.base_sha OR
     NEW.image_digest <> OLD.image_digest OR NEW.opencode_protocol <> OLD.opencode_protocol OR
     NEW.execution_contract_version <> OLD.execution_contract_version OR NEW.agent <> OLD.agent OR
     NEW.model_provider <> OLD.model_provider OR NEW.model <> OLD.model OR
     NEW.deadline <> OLD.deadline OR NEW.created_at <> OLD.created_at
BEGIN SELECT RAISE(ABORT, 'attempt execution inputs are immutable'); END;

CREATE TABLE receipts (
    id TEXT PRIMARY KEY CHECK(
        length(id) = 40 AND substr(id,1,4) = 'rcp_' AND
        substr(id,13,1) = '-' AND substr(id,18,1) = '-' AND substr(id,19,1) = '7' AND
        substr(id,23,1) = '-' AND substr(id,24,1) IN ('8','9','a','b') AND substr(id,28,1) = '-' AND
        length(replace(substr(id,5),'-','')) = 32 AND
        replace(substr(id,5),'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    command_kind TEXT NOT NULL CHECK(length(CAST(command_kind AS BLOB)) BETWEEN 1 AND 128),
    state TEXT NOT NULL CHECK(state = 'accepted'),
    idempotency_key TEXT NOT NULL CHECK(length(CAST(idempotency_key AS BLOB)) BETWEEN 1 AND 128),
    request_hash BLOB NOT NULL CHECK(length(request_hash) = 32),
    actor_snapshot_id INTEGER NOT NULL REFERENCES actor_snapshots(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    accepted_at INTEGER NOT NULL CHECK(accepted_at >= 0),
    api_contract_version TEXT NOT NULL CHECK(length(CAST(api_contract_version AS BLOB)) BETWEEN 1 AND 64),
    target_type TEXT NOT NULL CHECK(target_type = 'task'),
    target_id TEXT NOT NULL REFERENCES tasks(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    response_status INTEGER NOT NULL CHECK(response_status BETWEEN 200 AND 299),
    response_projection TEXT NOT NULL CHECK(length(CAST(response_projection AS BLOB)) <= 65536 AND json_valid(response_projection)),
    UNIQUE(workspace_id, command_kind, idempotency_key)
) STRICT;

CREATE INDEX receipts_target ON receipts(target_type, target_id);

CREATE TABLE events (
    cursor INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE CHECK(
        length(id) = 40 AND substr(id,1,4) = 'fev_' AND
        substr(id,13,1) = '-' AND substr(id,18,1) = '-' AND substr(id,19,1) = '7' AND
        substr(id,23,1) = '-' AND substr(id,24,1) IN ('8','9','a','b') AND substr(id,28,1) = '-' AND
        length(replace(substr(id,5),'-','')) = 32 AND
        replace(substr(id,5),'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    task_id TEXT REFERENCES tasks(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    attempt_id TEXT,
    entity_type TEXT NOT NULL CHECK(entity_type IN ('task','attempt')),
    entity_id TEXT NOT NULL CHECK(length(CAST(entity_id AS BLOB)) BETWEEN 1 AND 64),
    type TEXT NOT NULL CHECK(length(CAST(type AS BLOB)) BETWEEN 1 AND 128),
    version INTEGER NOT NULL CHECK(version >= 1),
    occurred_at INTEGER NOT NULL CHECK(occurred_at >= 0),
    actor_snapshot_id INTEGER NOT NULL REFERENCES actor_snapshots(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    payload TEXT NOT NULL CHECK(length(CAST(payload AS BLOB)) <= 65536 AND json_valid(payload)),
    FOREIGN KEY(task_id, workspace_id) REFERENCES tasks(id, workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(attempt_id, task_id, workspace_id) REFERENCES attempts(id, task_id, workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    CHECK(
        (entity_type = 'task' AND task_id IS NOT NULL AND attempt_id IS NULL AND entity_id = task_id) OR
        (entity_type = 'attempt' AND task_id IS NOT NULL AND attempt_id IS NOT NULL AND entity_id = attempt_id)
    ),
    CHECK(type <> 'task.accepted' OR (entity_type = 'task' AND attempt_id IS NULL)),
    CHECK(type <> 'attempt.prepared' OR (entity_type = 'attempt' AND attempt_id IS NOT NULL)),
    CHECK((substr(type,1,5) = 'task.' AND entity_type = 'task') OR
          (substr(type,1,8) = 'attempt.' AND entity_type = 'attempt')),
    UNIQUE(cursor, task_id)
) STRICT;

CREATE INDEX events_workspace_cursor ON events(workspace_id, cursor);

CREATE INDEX events_task_cursor ON events(task_id, cursor) WHERE task_id IS NOT NULL;

CREATE INDEX events_attempt_cursor ON events(attempt_id, cursor) WHERE attempt_id IS NOT NULL;

CREATE TRIGGER receipts_immutable_update BEFORE UPDATE ON receipts
BEGIN SELECT RAISE(ABORT, 'receipts are immutable'); END;

CREATE TRIGGER receipts_immutable_delete BEFORE DELETE ON receipts
BEGIN SELECT RAISE(ABORT, 'receipts are immutable'); END;

CREATE TRIGGER events_immutable_update BEFORE UPDATE ON events
BEGIN SELECT RAISE(ABORT, 'events are immutable'); END;

CREATE TRIGGER events_immutable_delete BEFORE DELETE ON events
BEGIN SELECT RAISE(ABORT, 'events are immutable'); END;

CREATE UNIQUE INDEX attempts_result_ownership ON attempts(id, sealed_result_id);

CREATE UNIQUE INDEX tasks_result_ownership ON tasks(id, sealed_result_id);

CREATE TABLE results (
    id TEXT PRIMARY KEY CHECK(
        length(id) = 40 AND substr(id,1,4) = 'res_' AND
        substr(id,13,1) = '-' AND substr(id,18,1) = '-' AND substr(id,19,1) = '7' AND
        substr(id,23,1) = '-' AND substr(id,24,1) IN ('8','9','a','b') AND substr(id,28,1) = '-' AND
        length(replace(substr(id,5),'-','')) = 32 AND
        replace(substr(id,5),'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    task_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state = 'sealed'),
    outcome TEXT NOT NULL CHECK(outcome IN ('changed','no_changes')),
    repository_id INTEGER NOT NULL CHECK(repository_id > 0),
    base_sha TEXT NOT NULL CHECK(length(base_sha) = 40 AND base_sha NOT GLOB '*[^0-9a-f]*'),
    result_commit TEXT NOT NULL CHECK(length(result_commit) = 40 AND result_commit NOT GLOB '*[^0-9a-f]*'),
    tree_oid TEXT NOT NULL CHECK(length(tree_oid) = 40 AND tree_oid NOT GLOB '*[^0-9a-f]*'),
    worktree_clean INTEGER NOT NULL CHECK(worktree_clean = 1),
    manifest_entries INTEGER NOT NULL CHECK(manifest_entries >= 0),
    manifest_sha256 BLOB NOT NULL CHECK(length(manifest_sha256) = 32),
    opencode_session_id TEXT NOT NULL CHECK(
        length(opencode_session_id) = 36 AND substr(opencode_session_id,1,4) = 'ses_' AND
        substr(opencode_session_id,5) NOT GLOB '*[^0-9a-f]*'
    ),
    opencode_message_id TEXT NOT NULL CHECK(
        length(opencode_message_id) = 36 AND substr(opencode_message_id,1,4) = 'msg_' AND
        substr(opencode_message_id,5) NOT GLOB '*[^0-9a-f]*'
    ),
    evidence_sha256 BLOB NOT NULL CHECK(length(evidence_sha256) = 32),
    policy_version TEXT NOT NULL CHECK(length(CAST(policy_version AS BLOB)) BETWEEN 1 AND 128),
    collected_at INTEGER NOT NULL CHECK(collected_at >= 0),
    sealed_at INTEGER NOT NULL CHECK(sealed_at >= collected_at),
    creator_actor_snapshot_id INTEGER NOT NULL REFERENCES actor_snapshots(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    sealed_event_id TEXT NOT NULL REFERENCES events(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    completed_event_id TEXT NOT NULL REFERENCES events(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    revision INTEGER NOT NULL CHECK(revision = 1),
    created_at INTEGER NOT NULL CHECK(created_at = sealed_at),
    updated_at INTEGER NOT NULL CHECK(updated_at = sealed_at), completion_authority TEXT NOT NULL DEFAULT 'user_seal'
  CHECK(completion_authority='user_seal'), source_kind TEXT NOT NULL DEFAULT 'retained_artifact'
  CHECK(source_kind='retained_artifact'), retained_artifact_id TEXT NOT NULL, artifact_export_id TEXT NOT NULL, materialization_id TEXT NOT NULL,
    CHECK((outcome='no_changes' AND result_commit=base_sha AND manifest_entries=0) OR
          (outcome='changed' AND result_commit<>base_sha AND manifest_entries>0)),
    FOREIGN KEY(task_id, workspace_id) REFERENCES tasks(id, workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(attempt_id, task_id, workspace_id) REFERENCES attempts(id, task_id, workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(workspace_id, repository_id) REFERENCES workspaces(id, repository_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(task_id, id) REFERENCES tasks(id, sealed_result_id) ON UPDATE RESTRICT ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY(attempt_id, id) REFERENCES attempts(id, sealed_result_id) ON UPDATE RESTRICT ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
    UNIQUE(task_id),
    UNIQUE(attempt_id),
    UNIQUE(id, task_id, attempt_id)
) STRICT;

CREATE TABLE result_manifest (
    result_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
    path_base64 TEXT NOT NULL CHECK(length(CAST(path_base64 AS BLOB)) BETWEEN 4 AND 5464),
    change_kind TEXT NOT NULL CHECK(change_kind IN ('added','modified','deleted')),
    old_mode TEXT CHECK(old_mode IS NULL OR old_mode IN ('100644','100755','120000')),
    new_mode TEXT CHECK(new_mode IS NULL OR new_mode IN ('100644','100755','120000')),
    old_blob_oid TEXT CHECK(old_blob_oid IS NULL OR (length(old_blob_oid)=40 AND old_blob_oid NOT GLOB '*[^0-9a-f]*')),
    new_blob_oid TEXT CHECK(new_blob_oid IS NULL OR (length(new_blob_oid)=40 AND new_blob_oid NOT GLOB '*[^0-9a-f]*')),
    old_size INTEGER CHECK(old_size IS NULL OR old_size >= 0),
    new_size INTEGER CHECK(new_size IS NULL OR new_size >= 0),
    CHECK(
        (change_kind='added' AND old_mode IS NULL AND old_blob_oid IS NULL AND old_size IS NULL AND
                             new_mode IS NOT NULL AND new_blob_oid IS NOT NULL AND new_size IS NOT NULL) OR
        (change_kind='deleted' AND new_mode IS NULL AND new_blob_oid IS NULL AND new_size IS NULL AND
                               old_mode IS NOT NULL AND old_blob_oid IS NOT NULL AND old_size IS NOT NULL) OR
        (change_kind='modified' AND old_mode IS NOT NULL AND old_blob_oid IS NOT NULL AND old_size IS NOT NULL AND
                                new_mode IS NOT NULL AND new_blob_oid IS NOT NULL AND new_size IS NOT NULL)
    ),
    PRIMARY KEY(result_id, ordinal),
    UNIQUE(result_id, path_base64),
    FOREIGN KEY(result_id) REFERENCES results(id) ON UPDATE RESTRICT ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED
) STRICT;

CREATE INDEX results_workspace_sealed ON results(workspace_id, sealed_at, id);

CREATE TRIGGER results_immutable_delete BEFORE DELETE ON results
BEGIN SELECT RAISE(ABORT, 'results are immutable'); END;

CREATE TRIGGER result_manifest_immutable_update BEFORE UPDATE ON result_manifest
BEGIN SELECT RAISE(ABORT, 'result manifest is immutable'); END;

CREATE TRIGGER result_manifest_immutable_delete BEFORE DELETE ON result_manifest
BEGIN SELECT RAISE(ABORT, 'result manifest is immutable'); END;

CREATE TRIGGER attempts_result_seal_immutable BEFORE UPDATE ON attempts
WHEN OLD.sealed_result_id IS NOT NULL AND NEW.sealed_result_id IS NOT OLD.sealed_result_id
BEGIN SELECT RAISE(ABORT, 'attempt result seal is immutable'); END;

CREATE TRIGGER tasks_completed_immutable BEFORE UPDATE ON tasks
WHEN OLD.state='completed' AND (NEW.state<>OLD.state OR NEW.sealed_result_id IS NOT OLD.sealed_result_id OR NEW.terminal_reason IS NOT OLD.terminal_reason)
BEGIN SELECT RAISE(ABORT, 'completed task is immutable'); END;

CREATE TABLE background_runs (
    task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    attempt_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL,
    generation INTEGER NOT NULL CHECK(generation > 0),
    writer_generation INTEGER NOT NULL DEFAULT 1 CHECK(writer_generation = 1),
    repository_id INTEGER NOT NULL CHECK(repository_id > 0),
    repository_remote TEXT NOT NULL CHECK(length(CAST(repository_remote AS BLOB)) BETWEEN 1 AND 2048),
    base_oid TEXT NOT NULL CHECK(length(base_oid)=40 AND base_oid NOT GLOB '*[^0-9a-f]*'),
    branch TEXT CHECK(branch IS NULL OR length(CAST(branch AS BLOB)) BETWEEN 1 AND 255),
    instruction_sha256 BLOB NOT NULL CHECK(length(instruction_sha256)=32),
    profile TEXT NOT NULL CHECK(profile='source-39fb919a054190498f6d5b7985bde231f93ad7a6'),
    profile_sha256 BLOB NOT NULL CHECK(
      (profile='source-39fb919a054190498f6d5b7985bde231f93ad7a6' AND lower(hex(profile_sha256))='2c879131c70fa0f5414261aa0d196dd3de2d590d88365ed7c7bd31d20d6cd2ab')
    ),
    image_identity TEXT NOT NULL CHECK(length(CAST(image_identity AS BLOB)) BETWEEN 1 AND 256),
    clone_identity TEXT NOT NULL UNIQUE CHECK(length(CAST(clone_identity AS BLOB)) BETWEEN 1 AND 256),
    volume_identity TEXT NOT NULL UNIQUE CHECK(length(CAST(volume_identity AS BLOB)) BETWEEN 1 AND 256),
    container_identity TEXT NOT NULL UNIQUE CHECK(length(CAST(container_identity AS BLOB)) BETWEEN 1 AND 256),
    endpoint_identity TEXT NOT NULL UNIQUE CHECK(length(CAST(endpoint_identity AS BLOB)) BETWEEN 1 AND 256),
    opencode_session_id TEXT NOT NULL UNIQUE,
    opencode_message_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('queued','setting_up','working','needs_you','canceling','uncertain','result_ready','failed','cleanup_required')),
    effect_phase TEXT NOT NULL CHECK(effect_phase IN (
      'absent','provision_intent','clone_observed','volume_observed','container_observed','health_observed','ready',
      'session_observed','prompt_intent','prompt_admitted','stop_intent','writer_inactive','route_removed',
      'container_removed','volume_removed','clone_removed','cleanup_complete','pre_effect_failed'
    )),
    cancel_epoch INTEGER NOT NULL DEFAULT 0 CHECK(cancel_epoch IN (0,1)),
    stop_receipt_id TEXT REFERENCES receipts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    stop_actor_snapshot_id INTEGER REFERENCES actor_snapshots(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    stop_requested_at INTEGER,
    creator_actor_snapshot_id INTEGER NOT NULL REFERENCES actor_snapshots(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    clone_evidence TEXT CHECK(clone_evidence IS NULL OR length(CAST(clone_evidence AS BLOB)) BETWEEN 1 AND 4096),
    volume_evidence TEXT CHECK(volume_evidence IS NULL OR length(CAST(volume_evidence AS BLOB)) BETWEEN 1 AND 4096),
    observed_container_id TEXT CHECK(observed_container_id IS NULL OR length(CAST(observed_container_id AS BLOB)) BETWEEN 1 AND 128),
    observed_container_started_at TEXT CHECK(observed_container_started_at IS NULL OR length(CAST(observed_container_started_at AS BLOB)) BETWEEN 1 AND 64),
    runtime_epoch INTEGER CHECK(runtime_epoch IS NULL OR runtime_epoch > 0),
    host_port INTEGER CHECK(host_port IS NULL OR host_port BETWEEN 1 AND 65535),
    health_evidence TEXT CHECK(health_evidence IS NULL OR length(CAST(health_evidence AS BLOB)) BETWEEN 1 AND 4096),
    ready_evidence TEXT CHECK(ready_evidence IS NULL OR length(CAST(ready_evidence AS BLOB)) BETWEEN 1 AND 4096),
    session_evidence TEXT CHECK(session_evidence IS NULL OR length(CAST(session_evidence AS BLOB)) BETWEEN 1 AND 4096),
    prompt_evidence TEXT CHECK(prompt_evidence IS NULL OR length(CAST(prompt_evidence AS BLOB)) BETWEEN 1 AND 4096),
    writer_inactive_evidence TEXT CHECK(writer_inactive_evidence IS NULL OR length(CAST(writer_inactive_evidence AS BLOB)) BETWEEN 1 AND 4096),
    route_removed_evidence TEXT CHECK(route_removed_evidence IS NULL OR length(CAST(route_removed_evidence AS BLOB)) BETWEEN 1 AND 4096),
    container_removed_evidence TEXT CHECK(container_removed_evidence IS NULL OR length(CAST(container_removed_evidence AS BLOB)) BETWEEN 1 AND 4096),
    volume_removed_evidence TEXT CHECK(volume_removed_evidence IS NULL OR length(CAST(volume_removed_evidence AS BLOB)) BETWEEN 1 AND 4096),
    clone_removed_evidence TEXT CHECK(clone_removed_evidence IS NULL OR length(CAST(clone_removed_evidence AS BLOB)) BETWEEN 1 AND 4096),
    last_evidence TEXT CHECK(last_evidence IS NULL OR length(CAST(last_evidence AS BLOB)) BETWEEN 1 AND 4096),
    last_error TEXT CHECK(last_error IS NULL OR length(CAST(last_error AS BLOB)) BETWEEN 1 AND 4096),
    provision_intent_at INTEGER,
    clone_observed_at INTEGER,
    volume_observed_at INTEGER,
    container_observed_at INTEGER,
    health_observed_at INTEGER,
    ready_at INTEGER,
    session_observed_at INTEGER,
    prompt_intent_at INTEGER,
    prompt_admitted_at INTEGER,
    stop_intent_at INTEGER,
    writer_inactive_at INTEGER,
    route_removed_at INTEGER,
    container_removed_at INTEGER,
    volume_removed_at INTEGER,
    clone_removed_at INTEGER,
    cleanup_completed_at INTEGER,
    cleanup_proof TEXT CHECK(cleanup_proof IS NULL OR length(CAST(cleanup_proof AS BLOB)) BETWEEN 1 AND 4096),
    absence_proof TEXT CHECK(absence_proof IS NULL OR length(CAST(absence_proof AS BLOB)) BETWEEN 1 AND 4096),
    revision INTEGER NOT NULL CHECK(revision >= 1),
    created_at INTEGER NOT NULL CHECK(created_at >= 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at), prompt_request_attempted_at INTEGER
  CHECK(prompt_request_attempted_at IS NULL OR prompt_request_attempted_at BETWEEN created_at AND updated_at), timeout_requested_at INTEGER
  CHECK(timeout_requested_at IS NULL OR timeout_requested_at BETWEEN created_at AND updated_at), timeout_actor_snapshot_id INTEGER
  REFERENCES actor_snapshots(id) ON UPDATE RESTRICT ON DELETE RESTRICT, environment_sha256 BLOB NOT NULL
  CHECK(length(environment_sha256)=32 AND
    lower(hex(environment_sha256))<>'0000000000000000000000000000000000000000000000000000000000000000'), resource_spec_version INTEGER NOT NULL
  CHECK(resource_spec_version=10), background_seal_request_id TEXT, artifact_export_id TEXT, retained_artifact_id TEXT, materialization_id TEXT, retained_result_id TEXT, result_authority_phase TEXT
  CHECK(result_authority_phase IS NULL OR result_authority_phase IN
    ('seal_intent','writer_inactive','exporting','artifact_committed','cleanup')),
    CHECK((cancel_epoch=0 AND stop_receipt_id IS NULL AND stop_actor_snapshot_id IS NULL AND stop_requested_at IS NULL AND state<>'canceling') OR
          (cancel_epoch=1 AND stop_receipt_id IS NOT NULL AND stop_actor_snapshot_id IS NOT NULL AND stop_requested_at IS NOT NULL AND
           state IN ('canceling','uncertain','result_ready','failed','cleanup_required'))),
    CHECK(
        (state='queued' AND effect_phase='absent') OR
        (state='setting_up' AND effect_phase IN ('provision_intent','clone_observed','volume_observed','container_observed','health_observed','ready','session_observed')) OR
        (state IN ('working','needs_you') AND effect_phase='prompt_admitted') OR
        (state='uncertain' AND effect_phase IN ('provision_intent','clone_observed','volume_observed','container_observed','health_observed','ready','session_observed','prompt_intent','prompt_admitted','stop_intent')) OR
        (state IN ('canceling','cleanup_required') AND effect_phase IN ('stop_intent','writer_inactive','route_removed','container_removed','volume_removed','clone_removed')) OR
        (state='result_ready' AND effect_phase IN ('prompt_admitted','stop_intent','writer_inactive','route_removed','container_removed','volume_removed','clone_removed','cleanup_complete')) OR
        (state='failed' AND effect_phase IN ('pre_effect_failed','cleanup_complete'))
    ),
    CHECK((observed_container_id IS NULL AND observed_container_started_at IS NULL AND runtime_epoch IS NULL AND host_port IS NULL AND container_observed_at IS NULL) OR
          (observed_container_id IS NOT NULL AND observed_container_started_at IS NOT NULL AND runtime_epoch IS NOT NULL AND host_port IS NOT NULL AND container_observed_at IS NOT NULL)),
    CHECK(effect_phase NOT IN ('clone_observed','volume_observed','container_observed','health_observed','ready','session_observed','prompt_intent','prompt_admitted') OR
          (clone_observed_at IS NOT NULL AND clone_evidence IS NOT NULL)),
    CHECK(effect_phase NOT IN ('volume_observed','container_observed','health_observed','ready','session_observed','prompt_intent','prompt_admitted') OR
          (volume_observed_at IS NOT NULL AND volume_evidence IS NOT NULL)),
    CHECK(effect_phase NOT IN ('container_observed','health_observed','ready','session_observed','prompt_intent','prompt_admitted') OR observed_container_id IS NOT NULL),
    CHECK(effect_phase NOT IN ('health_observed','ready','session_observed','prompt_intent','prompt_admitted') OR
          (health_observed_at IS NOT NULL AND health_evidence IS NOT NULL)),
    CHECK(effect_phase NOT IN ('ready','session_observed','prompt_intent','prompt_admitted') OR (ready_at IS NOT NULL AND ready_evidence IS NOT NULL)),
    CHECK(effect_phase NOT IN ('session_observed','prompt_intent','prompt_admitted') OR (session_observed_at IS NOT NULL AND session_evidence IS NOT NULL)),
    CHECK(effect_phase NOT IN ('prompt_intent','prompt_admitted') OR prompt_intent_at IS NOT NULL),
    CHECK(effect_phase<>'prompt_admitted' OR (prompt_admitted_at IS NOT NULL AND prompt_evidence IS NOT NULL)),
    CHECK(effect_phase NOT IN ('stop_intent','writer_inactive','route_removed','container_removed','volume_removed','clone_removed','cleanup_complete') OR stop_intent_at IS NOT NULL),
    CHECK(effect_phase NOT IN ('writer_inactive','route_removed','container_removed','volume_removed','clone_removed','cleanup_complete') OR
          (writer_inactive_at IS NOT NULL AND writer_inactive_evidence IS NOT NULL)),
    CHECK(effect_phase NOT IN ('route_removed','container_removed','volume_removed','clone_removed','cleanup_complete') OR
          (route_removed_at IS NOT NULL AND route_removed_evidence IS NOT NULL)),
    CHECK(effect_phase NOT IN ('container_removed','volume_removed','clone_removed','cleanup_complete') OR
          (container_removed_at IS NOT NULL AND container_removed_evidence IS NOT NULL)),
    CHECK(effect_phase NOT IN ('volume_removed','clone_removed','cleanup_complete') OR
          (volume_removed_at IS NOT NULL AND volume_removed_evidence IS NOT NULL)),
    CHECK(effect_phase NOT IN ('clone_removed','cleanup_complete') OR (clone_removed_at IS NOT NULL AND clone_removed_evidence IS NOT NULL)),
    CHECK((cleanup_completed_at IS NULL AND cleanup_proof IS NULL) OR
          (cleanup_completed_at IS NOT NULL AND cleanup_proof IS NOT NULL AND effect_phase='cleanup_complete')),
    CHECK((effect_phase='pre_effect_failed')=(absence_proof IS NOT NULL)),
    CHECK((provision_intent_at IS NULL OR provision_intent_at BETWEEN created_at AND updated_at) AND
          (clone_observed_at IS NULL OR clone_observed_at BETWEEN created_at AND updated_at) AND
          (volume_observed_at IS NULL OR volume_observed_at BETWEEN created_at AND updated_at) AND
          (container_observed_at IS NULL OR container_observed_at BETWEEN created_at AND updated_at) AND
          (health_observed_at IS NULL OR health_observed_at BETWEEN created_at AND updated_at) AND
          (ready_at IS NULL OR ready_at BETWEEN created_at AND updated_at) AND
          (session_observed_at IS NULL OR session_observed_at BETWEEN created_at AND updated_at) AND
          (prompt_intent_at IS NULL OR prompt_intent_at BETWEEN created_at AND updated_at) AND
          (prompt_admitted_at IS NULL OR prompt_admitted_at BETWEEN created_at AND updated_at) AND
          (stop_intent_at IS NULL OR stop_intent_at BETWEEN created_at AND updated_at) AND
          (writer_inactive_at IS NULL OR writer_inactive_at BETWEEN created_at AND updated_at) AND
          (route_removed_at IS NULL OR route_removed_at BETWEEN created_at AND updated_at) AND
          (container_removed_at IS NULL OR container_removed_at BETWEEN created_at AND updated_at) AND
          (volume_removed_at IS NULL OR volume_removed_at BETWEEN created_at AND updated_at) AND
          (clone_removed_at IS NULL OR clone_removed_at BETWEEN created_at AND updated_at) AND
          (cleanup_completed_at IS NULL OR cleanup_completed_at BETWEEN created_at AND updated_at)),
    FOREIGN KEY(attempt_id,task_id,workspace_id) REFERENCES attempts(id,task_id,workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(task_id,workspace_id) REFERENCES tasks(id,workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(workspace_id,repository_id) REFERENCES workspaces(id,repository_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    UNIQUE(attempt_id),
    UNIQUE(task_id,generation)
) STRICT;

CREATE INDEX background_runs_actor_list ON background_runs(creator_actor_snapshot_id,created_at DESC,task_id DESC);

CREATE INDEX background_runs_next ON background_runs(workspace_id,state,updated_at,task_id);

CREATE UNIQUE INDEX background_runs_workspace_capacity_one ON background_runs(workspace_id)
  WHERE profile='source-39fb919a054190498f6d5b7985bde231f93ad7a6' AND
    effect_phase NOT IN ('absent','cleanup_complete','pre_effect_failed');

CREATE TRIGGER background_runs_immutable_inputs BEFORE UPDATE ON background_runs
WHEN NEW.task_id<>OLD.task_id OR NEW.attempt_id<>OLD.attempt_id OR NEW.workspace_id<>OLD.workspace_id OR
     NEW.generation<>OLD.generation OR NEW.writer_generation<>OLD.writer_generation OR NEW.repository_id<>OLD.repository_id OR NEW.repository_remote<>OLD.repository_remote OR
     NEW.base_oid<>OLD.base_oid OR NEW.branch IS NOT OLD.branch OR NEW.instruction_sha256<>OLD.instruction_sha256 OR
     NEW.profile<>OLD.profile OR NEW.profile_sha256<>OLD.profile_sha256 OR NEW.image_identity<>OLD.image_identity OR
     NEW.clone_identity<>OLD.clone_identity OR NEW.volume_identity<>OLD.volume_identity OR
     NEW.container_identity<>OLD.container_identity OR NEW.endpoint_identity<>OLD.endpoint_identity OR
     NEW.opencode_session_id<>OLD.opencode_session_id OR NEW.opencode_message_id<>OLD.opencode_message_id OR
     NEW.creator_actor_snapshot_id<>OLD.creator_actor_snapshot_id OR NEW.created_at<>OLD.created_at
BEGIN SELECT RAISE(ABORT, 'background run inputs are immutable'); END;

CREATE TRIGGER background_runs_revision BEFORE UPDATE ON background_runs
WHEN NEW.revision<>OLD.revision+1 OR NEW.updated_at<OLD.updated_at
BEGIN SELECT RAISE(ABORT, 'invalid background run revision'); END;

CREATE TRIGGER background_runs_stop_fields_immutable BEFORE UPDATE ON background_runs
WHEN OLD.cancel_epoch=1 AND (NEW.cancel_epoch<>OLD.cancel_epoch OR NEW.stop_receipt_id IS NOT OLD.stop_receipt_id OR
  NEW.stop_actor_snapshot_id IS NOT OLD.stop_actor_snapshot_id OR NEW.stop_requested_at IS NOT OLD.stop_requested_at)
BEGIN SELECT RAISE(ABORT, 'background run stop fields are immutable'); END;

CREATE TRIGGER background_runs_environment_immutable BEFORE UPDATE ON background_runs
WHEN NEW.environment_sha256 IS NOT OLD.environment_sha256 OR NEW.resource_spec_version IS NOT OLD.resource_spec_version
BEGIN SELECT RAISE(ABORT, 'background run environment identity is immutable'); END;

CREATE TRIGGER background_runs_prompt_attempt_immutable BEFORE UPDATE ON background_runs
WHEN OLD.prompt_request_attempted_at IS NOT NULL AND NEW.prompt_request_attempted_at IS NOT OLD.prompt_request_attempted_at
BEGIN SELECT RAISE(ABORT, 'background run prompt attempt is immutable'); END;

CREATE TRIGGER background_runs_prompt_admission_requires_attempt BEFORE UPDATE ON background_runs
WHEN NEW.effect_phase='prompt_admitted' AND NEW.prompt_request_attempted_at IS NULL
BEGIN SELECT RAISE(ABORT, 'background run prompt admission has no request attempt'); END;

CREATE TRIGGER background_runs_timeout_immutable BEFORE UPDATE ON background_runs
WHEN OLD.timeout_requested_at IS NOT NULL AND
  (NEW.timeout_requested_at IS NOT OLD.timeout_requested_at OR NEW.timeout_actor_snapshot_id IS NOT OLD.timeout_actor_snapshot_id)
BEGIN SELECT RAISE(ABORT, 'background run timeout is immutable'); END;

CREATE TABLE background_run_seal_requests (
  id TEXT PRIMARY KEY CHECK(length(id)=40 AND substr(id,1,4)='slr_' AND substr(id,13,1)='-' AND substr(id,18,1)='-' AND substr(id,19,1)='7' AND substr(id,23,1)='-' AND substr(id,24,1) IN ('8','9','a','b') AND substr(id,28,1)='-' AND replace(substr(id,5),'-','') NOT GLOB '*[^0-9a-f]*'),
  receipt_id TEXT NOT NULL UNIQUE REFERENCES receipts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  workspace_id TEXT NOT NULL,
  task_id TEXT NOT NULL UNIQUE,
  attempt_id TEXT NOT NULL UNIQUE,
  generation INTEGER NOT NULL CHECK(generation>0),
  expected_run_revision INTEGER NOT NULL CHECK(expected_run_revision>0),
  expected_task_revision INTEGER NOT NULL CHECK(expected_task_revision>0),
  expected_attempt_revision INTEGER NOT NULL CHECK(expected_attempt_revision>0),
  idempotency_key TEXT NOT NULL CHECK(length(CAST(idempotency_key AS BLOB)) BETWEEN 1 AND 128),
  request_hash BLOB NOT NULL CHECK(length(request_hash)=32),
  owner_actor_snapshot_id INTEGER NOT NULL REFERENCES actor_snapshots(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  export_id TEXT NOT NULL UNIQUE CHECK(length(export_id)=40 AND substr(export_id,1,4)='exp_' AND substr(export_id,19,1)='7' AND substr(export_id,24,1) IN ('8','9','a','b') AND replace(substr(export_id,5),'-','') NOT GLOB '*[^0-9a-f]*'),
  artifact_id TEXT NOT NULL UNIQUE CHECK(length(artifact_id)=40 AND substr(artifact_id,1,4)='art_' AND substr(artifact_id,19,1)='7' AND substr(artifact_id,24,1) IN ('8','9','a','b') AND replace(substr(artifact_id,5),'-','') NOT GLOB '*[^0-9a-f]*'),
  materialization_id TEXT NOT NULL UNIQUE CHECK(length(materialization_id)=40 AND substr(materialization_id,1,4)='mat_' AND substr(materialization_id,19,1)='7' AND substr(materialization_id,24,1) IN ('8','9','a','b') AND replace(substr(materialization_id,5),'-','') NOT GLOB '*[^0-9a-f]*'),
  result_id TEXT NOT NULL UNIQUE CHECK(length(result_id)=40 AND substr(result_id,1,4)='res_' AND substr(result_id,19,1)='7' AND substr(result_id,24,1) IN ('8','9','a','b') AND replace(substr(result_id,5),'-','') NOT GLOB '*[^0-9a-f]*'),
  result_event_id TEXT NOT NULL UNIQUE CHECK(length(result_event_id)=40 AND substr(result_event_id,1,4)='fev_' AND substr(result_event_id,19,1)='7' AND replace(substr(result_event_id,5),'-','') NOT GLOB '*[^0-9a-f]*'),
  task_event_id TEXT NOT NULL UNIQUE CHECK(length(task_event_id)=40 AND substr(task_event_id,1,4)='fev_' AND substr(task_event_id,19,1)='7' AND replace(substr(task_event_id,5),'-','') NOT GLOB '*[^0-9a-f]*' AND task_event_id<>result_event_id),
  commit_epoch_seconds INTEGER NOT NULL CHECK(commit_epoch_seconds>=0),
  policy_version TEXT NOT NULL CHECK(length(CAST(policy_version AS BLOB)) BETWEEN 1 AND 128),
  accepted_at INTEGER NOT NULL CHECK(accepted_at>=0),
  FOREIGN KEY(task_id,workspace_id) REFERENCES tasks(id,workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  FOREIGN KEY(attempt_id,task_id,workspace_id) REFERENCES attempts(id,task_id,workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  UNIQUE(task_id,generation)
) STRICT;

CREATE TABLE background_run_exports (
  id TEXT PRIMARY KEY CHECK(length(id)=40 AND substr(id,1,4)='exp_' AND substr(id,19,1)='7' AND substr(id,24,1) IN ('8','9','a','b') AND replace(substr(id,5),'-','') NOT GLOB '*[^0-9a-f]*'),
  seal_request_id TEXT NOT NULL UNIQUE REFERENCES background_run_seal_requests(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  workspace_id TEXT NOT NULL,
  task_id TEXT NOT NULL UNIQUE,
  attempt_id TEXT NOT NULL UNIQUE,
  generation INTEGER NOT NULL CHECK(generation>0),
  artifact_id TEXT NOT NULL UNIQUE CHECK(length(artifact_id)=40 AND substr(artifact_id,1,4)='art_' AND substr(artifact_id,19,1)='7'),
  materialization_id TEXT NOT NULL UNIQUE CHECK(length(materialization_id)=40 AND substr(materialization_id,1,4)='mat_' AND substr(materialization_id,19,1)='7'),
  result_id TEXT NOT NULL UNIQUE CHECK(length(result_id)=40 AND substr(result_id,1,4)='res_' AND substr(result_id,19,1)='7'),
  state TEXT NOT NULL CHECK(state IN ('prepared','running','recovery_required','completed')),
  phase TEXT NOT NULL CHECK(phase IN ('prepared','snapshot_started','snapshot_selected','bundle_write_started','bundle_verified','cas_install_started','cas_installed','materialize_started','materialized','completed')),
  repository_id INTEGER NOT NULL CHECK(repository_id>0),
  base_sha TEXT NOT NULL CHECK(length(base_sha)=40 AND base_sha NOT GLOB '*[^0-9a-f]*'),
  opencode_session_id TEXT NOT NULL CHECK(length(opencode_session_id)=36 AND substr(opencode_session_id,1,4)='ses_' AND substr(opencode_session_id,5) NOT GLOB '*[^0-9a-f]*'),
  opencode_message_id TEXT NOT NULL CHECK(length(opencode_message_id)=36 AND substr(opencode_message_id,1,4)='msg_' AND substr(opencode_message_id,5) NOT GLOB '*[^0-9a-f]*'),
  result_commit TEXT CHECK(result_commit IS NULL OR (length(result_commit)=40 AND result_commit NOT GLOB '*[^0-9a-f]*')),
  tree_oid TEXT CHECK(tree_oid IS NULL OR (length(tree_oid)=40 AND tree_oid NOT GLOB '*[^0-9a-f]*')),
  outcome TEXT CHECK(outcome IS NULL OR outcome IN ('changed','no_changes')),
  result_manifest_json TEXT CHECK(result_manifest_json IS NULL OR (json_valid(result_manifest_json) AND length(CAST(result_manifest_json AS BLOB))<=4194304)),
  result_manifest_entries INTEGER CHECK(result_manifest_entries IS NULL OR result_manifest_entries>=0),
  result_manifest_sha256 BLOB CHECK(result_manifest_sha256 IS NULL OR length(result_manifest_sha256)=32),
  artifact_manifest_json TEXT CHECK(artifact_manifest_json IS NULL OR (json_valid(artifact_manifest_json) AND json_type(artifact_manifest_json)='object' AND length(CAST(artifact_manifest_json AS BLOB))<=4194304)),
  artifact_manifest_sha256 BLOB CHECK(artifact_manifest_sha256 IS NULL OR length(artifact_manifest_sha256)=32),
  cas_locator TEXT CHECK(cas_locator IS NULL OR (length(cas_locator)=71 AND substr(cas_locator,1,7)='sha256:' AND substr(cas_locator,8) NOT GLOB '*[^0-9a-f]*')),
  bundle_sha256 BLOB CHECK(bundle_sha256 IS NULL OR length(bundle_sha256)=32),
  bundle_size INTEGER CHECK(bundle_size IS NULL OR bundle_size>=0),
  collected_at INTEGER,
  recovery_reason TEXT CHECK(recovery_reason IS NULL OR length(CAST(recovery_reason AS BLOB)) BETWEEN 1 AND 1000),
  revision INTEGER NOT NULL CHECK(revision>=1),
  created_at INTEGER NOT NULL CHECK(created_at>=0),
  updated_at INTEGER NOT NULL CHECK(updated_at>=created_at),
  CHECK((phase IN ('prepared','snapshot_started')) OR
        (result_commit IS NOT NULL AND tree_oid IS NOT NULL AND outcome IS NOT NULL AND result_manifest_json IS NOT NULL AND
         result_manifest_entries IS NOT NULL AND result_manifest_sha256 IS NOT NULL AND artifact_manifest_json IS NOT NULL AND
         artifact_manifest_sha256 IS NOT NULL AND cas_locator='sha256:'||lower(hex(artifact_manifest_sha256)) AND collected_at IS NOT NULL)),
  CHECK(phase NOT IN ('bundle_verified','cas_install_started','cas_installed','materialize_started','materialized','completed') OR
        (bundle_sha256 IS NOT NULL AND bundle_size IS NOT NULL)),
  FOREIGN KEY(task_id,workspace_id) REFERENCES tasks(id,workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  FOREIGN KEY(attempt_id,task_id,workspace_id) REFERENCES attempts(id,task_id,workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  FOREIGN KEY(workspace_id,repository_id) REFERENCES workspaces(id,repository_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  UNIQUE(task_id,generation)
) STRICT;

CREATE TABLE artifact_materializations (
  id TEXT PRIMARY KEY CHECK(length(id)=40 AND substr(id,1,4)='mat_' AND substr(id,19,1)='7' AND replace(substr(id,5),'-','') NOT GLOB '*[^0-9a-f]*'),
  seal_request_id TEXT NOT NULL UNIQUE REFERENCES background_run_seal_requests(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  export_id TEXT NOT NULL UNIQUE REFERENCES background_run_exports(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  artifact_id TEXT NOT NULL UNIQUE CHECK(length(artifact_id)=40 AND substr(artifact_id,1,4)='art_' AND substr(artifact_id,19,1)='7'),
  result_id TEXT NOT NULL UNIQUE CHECK(length(result_id)=40 AND substr(result_id,1,4)='res_' AND substr(result_id,19,1)='7'),
  state TEXT NOT NULL CHECK(state IN ('prepared','ready','recovery_required')),
  result_commit TEXT,
  tree_oid TEXT,
  proof_sha256 BLOB CHECK(proof_sha256 IS NULL OR length(proof_sha256)=32),
  recovery_reason TEXT CHECK(recovery_reason IS NULL OR length(CAST(recovery_reason AS BLOB)) BETWEEN 1 AND 1000),
  revision INTEGER NOT NULL CHECK(revision>=1),
  created_at INTEGER NOT NULL CHECK(created_at>=0),
  updated_at INTEGER NOT NULL CHECK(updated_at>=created_at),
  CHECK((state='prepared' AND result_commit IS NULL AND tree_oid IS NULL AND proof_sha256 IS NULL AND recovery_reason IS NULL) OR
        (state='ready' AND length(result_commit)=40 AND length(tree_oid)=40 AND proof_sha256 IS NOT NULL AND recovery_reason IS NULL) OR
        (state='recovery_required' AND recovery_reason IS NOT NULL))
) STRICT;

CREATE TABLE background_run_writer_fences (
  seal_request_id TEXT PRIMARY KEY REFERENCES background_run_seal_requests(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  export_id TEXT NOT NULL UNIQUE REFERENCES background_run_exports(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  task_id TEXT NOT NULL UNIQUE,
  attempt_id TEXT NOT NULL UNIQUE,
  generation INTEGER NOT NULL CHECK(generation>0),
  kind TEXT NOT NULL CHECK(kind IN ('never_created','never_started','runtime_stopped')),
  container_id TEXT,
  container_started_at TEXT,
  runtime_epoch INTEGER,
	  runtime_token TEXT,
  stopped_at INTEGER,
  proof_sha256 BLOB NOT NULL CHECK(length(proof_sha256)=32),
  recorded_at INTEGER NOT NULL CHECK(recorded_at>=0),
  CHECK((kind='never_created' AND container_id IS NULL AND container_started_at IS NULL AND runtime_epoch IS NULL AND runtime_token IS NULL AND stopped_at IS NULL) OR
		(kind='never_started' AND container_id IS NOT NULL AND container_started_at IS NULL AND runtime_epoch IS NULL AND runtime_token IS NULL AND stopped_at IS NULL) OR
		(kind='runtime_stopped' AND container_id IS NOT NULL AND container_started_at IS NOT NULL AND runtime_epoch>0 AND runtime_token IS NOT NULL AND stopped_at IS NOT NULL))
) STRICT;

CREATE TABLE retained_artifacts (
  id TEXT PRIMARY KEY CHECK(length(id)=40 AND substr(id,1,4)='art_' AND substr(id,19,1)='7' AND replace(substr(id,5),'-','') NOT GLOB '*[^0-9a-f]*'),
  seal_request_id TEXT NOT NULL UNIQUE REFERENCES background_run_seal_requests(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  export_id TEXT NOT NULL UNIQUE REFERENCES background_run_exports(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  materialization_id TEXT NOT NULL UNIQUE REFERENCES artifact_materializations(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  result_id TEXT NOT NULL UNIQUE,
  workspace_id TEXT NOT NULL,
  task_id TEXT NOT NULL UNIQUE,
  attempt_id TEXT NOT NULL UNIQUE,
  generation INTEGER NOT NULL CHECK(generation>0),
  manifest_json TEXT NOT NULL CHECK(json_valid(manifest_json) AND json_type(manifest_json)='object' AND length(CAST(manifest_json AS BLOB))<=4194304),
  manifest_sha256 BLOB NOT NULL UNIQUE CHECK(length(manifest_sha256)=32),
  changes_sha256 BLOB NOT NULL CHECK(length(changes_sha256)=32),
  cas_locator TEXT NOT NULL CHECK(length(cas_locator)=71 AND cas_locator='sha256:'||lower(hex(manifest_sha256))),
  bundle_sha256 BLOB NOT NULL CHECK(length(bundle_sha256)=32),
  bundle_size INTEGER NOT NULL CHECK(bundle_size>=0),
  base_sha TEXT NOT NULL CHECK(length(base_sha)=40 AND base_sha NOT GLOB '*[^0-9a-f]*'),
  result_commit TEXT NOT NULL,
  tree_oid TEXT NOT NULL,
  opencode_session_id TEXT NOT NULL CHECK(length(opencode_session_id)=36 AND substr(opencode_session_id,1,4)='ses_'),
  opencode_message_id TEXT NOT NULL CHECK(length(opencode_message_id)=36 AND substr(opencode_message_id,1,4)='msg_'),
  committed_at INTEGER NOT NULL CHECK(committed_at>=0),
  FOREIGN KEY(task_id,workspace_id) REFERENCES tasks(id,workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  FOREIGN KEY(attempt_id,task_id,workspace_id) REFERENCES attempts(id,task_id,workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  UNIQUE(task_id,generation)
) STRICT;

CREATE TRIGGER background_run_seal_requests_immutable_update BEFORE UPDATE ON background_run_seal_requests BEGIN SELECT RAISE(ABORT,'background run seal request is immutable'); END;

CREATE TRIGGER background_run_seal_requests_immutable_delete BEFORE DELETE ON background_run_seal_requests BEGIN SELECT RAISE(ABORT,'background run seal request is durable'); END;

CREATE TRIGGER background_run_writer_fences_immutable_update BEFORE UPDATE ON background_run_writer_fences BEGIN SELECT RAISE(ABORT,'writer fence is immutable'); END;

CREATE TRIGGER background_run_writer_fences_immutable_delete BEFORE DELETE ON background_run_writer_fences BEGIN SELECT RAISE(ABORT,'writer fence is durable'); END;

CREATE TRIGGER retained_artifacts_immutable_update BEFORE UPDATE ON retained_artifacts BEGIN SELECT RAISE(ABORT,'retained artifact is immutable'); END;

CREATE TRIGGER retained_artifacts_immutable_delete BEFORE DELETE ON retained_artifacts BEGIN SELECT RAISE(ABORT,'retained artifact is durable'); END;

CREATE TRIGGER background_run_exports_immutable_tuple BEFORE UPDATE ON background_run_exports WHEN
  NEW.id<>OLD.id OR NEW.seal_request_id<>OLD.seal_request_id OR NEW.workspace_id<>OLD.workspace_id OR NEW.task_id<>OLD.task_id OR
  NEW.attempt_id<>OLD.attempt_id OR NEW.generation<>OLD.generation OR NEW.artifact_id<>OLD.artifact_id OR
  NEW.materialization_id<>OLD.materialization_id OR NEW.result_id<>OLD.result_id OR NEW.repository_id<>OLD.repository_id OR
  NEW.base_sha<>OLD.base_sha OR NEW.opencode_session_id<>OLD.opencode_session_id OR NEW.opencode_message_id<>OLD.opencode_message_id OR NEW.created_at<>OLD.created_at OR
  (OLD.result_commit IS NOT NULL AND (NEW.result_commit IS NOT OLD.result_commit OR NEW.tree_oid IS NOT OLD.tree_oid OR NEW.outcome IS NOT OLD.outcome OR
   NEW.result_manifest_json IS NOT OLD.result_manifest_json OR NEW.result_manifest_entries IS NOT OLD.result_manifest_entries OR
   NEW.result_manifest_sha256 IS NOT OLD.result_manifest_sha256 OR NEW.artifact_manifest_json IS NOT OLD.artifact_manifest_json OR
   NEW.artifact_manifest_sha256 IS NOT OLD.artifact_manifest_sha256 OR NEW.cas_locator IS NOT OLD.cas_locator OR NEW.collected_at IS NOT OLD.collected_at)) OR
  (OLD.bundle_sha256 IS NOT NULL AND (NEW.bundle_sha256 IS NOT OLD.bundle_sha256 OR NEW.bundle_size IS NOT OLD.bundle_size))
BEGIN SELECT RAISE(ABORT,'background export tuple is immutable'); END;

CREATE TRIGGER background_run_exports_revision BEFORE UPDATE ON background_run_exports WHEN
  NEW.revision<>OLD.revision+1 OR NEW.updated_at<OLD.updated_at
BEGIN SELECT RAISE(ABORT,'invalid background export revision'); END;

CREATE TRIGGER background_run_exports_terminal BEFORE UPDATE ON background_run_exports WHEN OLD.state='completed'
BEGIN SELECT RAISE(ABORT,'completed background export is immutable'); END;

CREATE TRIGGER background_run_exports_delete BEFORE DELETE ON background_run_exports BEGIN SELECT RAISE(ABORT,'background export is durable'); END;

CREATE TRIGGER artifact_materializations_transition BEFORE UPDATE ON artifact_materializations BEGIN
  SELECT CASE WHEN OLD.state<>'prepared' OR NEW.revision<>OLD.revision+1 OR NEW.updated_at<OLD.updated_at OR
    NEW.id<>OLD.id OR NEW.seal_request_id<>OLD.seal_request_id OR NEW.export_id<>OLD.export_id OR NEW.artifact_id<>OLD.artifact_id OR
    NEW.result_id<>OLD.result_id OR NEW.created_at<>OLD.created_at OR NEW.state NOT IN ('ready','recovery_required')
    THEN RAISE(ABORT,'invalid artifact materialization transition') END;
END;

CREATE TRIGGER artifact_materializations_delete BEFORE DELETE ON artifact_materializations BEGIN SELECT RAISE(ABORT,'artifact materialization is durable'); END;

CREATE TRIGGER artifact_manifests_safe_insert BEFORE INSERT ON retained_artifacts WHEN EXISTS (
  SELECT 1 FROM json_tree(NEW.manifest_json) WHERE
	 lower(COALESCE(key,'')) IN ('host_path','remote_url','prompt','environment','credential','credentials','cookie','cookies','authorization','actor_auth','opencode_output','raw_output'))
BEGIN SELECT RAISE(ABORT,'artifact manifest contains forbidden authority'); END;

CREATE TRIGGER background_runs_retained_tuple_immutable BEFORE UPDATE ON background_runs WHEN OLD.background_seal_request_id IS NOT NULL AND (
  NEW.background_seal_request_id IS NOT OLD.background_seal_request_id OR NEW.artifact_export_id IS NOT OLD.artifact_export_id OR
  NEW.retained_artifact_id IS NOT OLD.retained_artifact_id OR NEW.materialization_id IS NOT OLD.materialization_id OR
  NEW.retained_result_id IS NOT OLD.retained_result_id)
BEGIN SELECT RAISE(ABORT,'background retained tuple is immutable'); END;

CREATE TRIGGER background_runs_retained_cleanup_gate BEFORE UPDATE OF effect_phase ON background_runs
WHEN OLD.background_seal_request_id IS NOT NULL AND OLD.state='result_ready' AND OLD.effect_phase='writer_inactive' AND NEW.effect_phase='route_removed' AND
  (OLD.result_authority_phase<>'cleanup' OR NOT EXISTS (
    SELECT 1 FROM results result JOIN retained_artifacts artifact ON artifact.id=OLD.retained_artifact_id
    JOIN background_run_exports export ON export.id=OLD.artifact_export_id
    JOIN artifact_materializations materialization ON materialization.id=OLD.materialization_id
    WHERE result.id=OLD.retained_result_id AND result.source_kind='retained_artifact' AND artifact.result_id=result.id AND
      export.state='completed' AND export.phase='completed' AND export.result_id=result.id AND materialization.state='ready' AND materialization.result_id=result.id))
BEGIN SELECT RAISE(ABORT,'retained cleanup has no exact committed tuple'); END;

CREATE TRIGGER background_runs_phase_timestamps_immutable BEFORE UPDATE ON background_runs WHEN
 (OLD.provision_intent_at IS NOT NULL AND NEW.provision_intent_at IS NOT OLD.provision_intent_at) OR
 (OLD.clone_observed_at IS NOT NULL AND NEW.clone_observed_at IS NOT OLD.clone_observed_at) OR
 (OLD.volume_observed_at IS NOT NULL AND NEW.volume_observed_at IS NOT OLD.volume_observed_at) OR
 (OLD.container_observed_at IS NOT NULL AND NEW.container_observed_at IS NOT OLD.container_observed_at) OR
 (OLD.health_observed_at IS NOT NULL AND NEW.health_observed_at IS NOT OLD.health_observed_at) OR
 (OLD.ready_at IS NOT NULL AND NEW.ready_at IS NOT OLD.ready_at) OR
 (OLD.session_observed_at IS NOT NULL AND NEW.session_observed_at IS NOT OLD.session_observed_at) OR
 (OLD.prompt_intent_at IS NOT NULL AND NEW.prompt_intent_at IS NOT OLD.prompt_intent_at) OR
 (OLD.prompt_admitted_at IS NOT NULL AND NEW.prompt_admitted_at IS NOT OLD.prompt_admitted_at) OR
 (OLD.stop_intent_at IS NOT NULL AND NEW.stop_intent_at IS NOT OLD.stop_intent_at) OR
 (OLD.writer_inactive_at IS NOT NULL AND NEW.writer_inactive_at IS NOT OLD.writer_inactive_at) OR
 (OLD.route_removed_at IS NOT NULL AND NEW.route_removed_at IS NOT OLD.route_removed_at) OR
 (OLD.container_removed_at IS NOT NULL AND NEW.container_removed_at IS NOT OLD.container_removed_at) OR
 (OLD.volume_removed_at IS NOT NULL AND NEW.volume_removed_at IS NOT OLD.volume_removed_at) OR
 (OLD.clone_removed_at IS NOT NULL AND NEW.clone_removed_at IS NOT OLD.clone_removed_at) OR
 (OLD.cleanup_completed_at IS NOT NULL AND NEW.cleanup_completed_at IS NOT OLD.cleanup_completed_at)
BEGIN SELECT RAISE(ABORT,'background run phase timestamp is immutable'); END;

CREATE TRIGGER background_runs_observation_immutable BEFORE UPDATE ON background_runs WHEN
 (OLD.observed_container_id IS NOT NULL AND (NEW.observed_container_id IS NOT OLD.observed_container_id OR NEW.observed_container_started_at IS NOT OLD.observed_container_started_at OR NEW.runtime_epoch IS NOT OLD.runtime_epoch OR NEW.host_port IS NOT OLD.host_port)) OR
 (OLD.clone_evidence IS NOT NULL AND NEW.clone_evidence IS NOT OLD.clone_evidence) OR (OLD.volume_evidence IS NOT NULL AND NEW.volume_evidence IS NOT OLD.volume_evidence) OR
 (OLD.health_evidence IS NOT NULL AND NEW.health_evidence IS NOT OLD.health_evidence) OR (OLD.ready_evidence IS NOT NULL AND NEW.ready_evidence IS NOT OLD.ready_evidence) OR
 (OLD.session_evidence IS NOT NULL AND NEW.session_evidence IS NOT OLD.session_evidence) OR (OLD.prompt_evidence IS NOT NULL AND NEW.prompt_evidence IS NOT OLD.prompt_evidence) OR
 (OLD.writer_inactive_evidence IS NOT NULL AND NEW.writer_inactive_evidence IS NOT OLD.writer_inactive_evidence) OR
 (OLD.route_removed_evidence IS NOT NULL AND NEW.route_removed_evidence IS NOT OLD.route_removed_evidence) OR
 (OLD.container_removed_evidence IS NOT NULL AND NEW.container_removed_evidence IS NOT OLD.container_removed_evidence) OR
 (OLD.volume_removed_evidence IS NOT NULL AND NEW.volume_removed_evidence IS NOT OLD.volume_removed_evidence) OR
 (OLD.clone_removed_evidence IS NOT NULL AND NEW.clone_removed_evidence IS NOT OLD.clone_removed_evidence) OR
 (OLD.absence_proof IS NOT NULL AND NEW.absence_proof IS NOT OLD.absence_proof) OR
 (OLD.cleanup_proof IS NOT NULL AND (NEW.cleanup_proof IS NOT OLD.cleanup_proof OR NEW.cleanup_completed_at IS NOT OLD.cleanup_completed_at))
BEGIN SELECT RAISE(ABORT,'background run resource proof is immutable'); END;

CREATE TRIGGER background_runs_terminal_immutable BEFORE UPDATE ON background_runs
WHEN OLD.state='failed' OR (OLD.state='result_ready' AND OLD.effect_phase='cleanup_complete')
BEGIN SELECT RAISE(ABORT,'terminal background run is immutable'); END;

CREATE TRIGGER results_immutable_update BEFORE UPDATE ON results BEGIN SELECT RAISE(ABORT,'results are immutable'); END;

CREATE INDEX results_retained_artifact ON results(retained_artifact_id) WHERE retained_artifact_id IS NOT NULL;
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
