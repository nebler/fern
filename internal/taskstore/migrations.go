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
	{version: 1, name: "initial_task_store", sql: initialSchema},
	{version: 2, name: "execution_projection_and_results", sql: executionAndResultSchema},
	{version: 3, name: "verification_and_publication_journals", sql: verificationAndPublicationSchema},
	{version: 4, name: "user_authorized_snapshot_seals", sql: userAuthorizedSealSchema},
	{version: 5, name: "explicit_workspace_github_authority", sql: explicitWorkspaceGitHubAuthoritySchema},
	{version: 6, name: "publication_admission_receipts", sql: publicationAdmissionReceiptSchema},
}

// CurrentSchemaVersion is the schema produced by all migrations in this build.
func CurrentSchemaVersion() int { return len(migrations) }

const publicationAdmissionReceiptSchema = `
ALTER TABLE publications ADD COLUMN admission_receipt_id TEXT
  REFERENCES receipts(id) ON UPDATE RESTRICT ON DELETE RESTRICT;
CREATE UNIQUE INDEX publications_admission_receipt ON publications(admission_receipt_id)
  WHERE admission_receipt_id IS NOT NULL;

CREATE TRIGGER publications_admission_receipt_insert BEFORE INSERT ON publications
BEGIN
  SELECT CASE WHEN NOT EXISTS (
    SELECT 1 FROM receipts r
    JOIN workspaces w ON w.id=NEW.workspace_id
    WHERE r.id=NEW.admission_receipt_id AND r.workspace_id=NEW.workspace_id AND
      r.command_kind='result.publish' AND r.state='accepted' AND r.target_type='task' AND r.target_id=NEW.task_id AND
      r.actor_snapshot_id=NEW.requester_actor_snapshot_id AND r.accepted_at=NEW.created_at AND r.response_status=202 AND
      json_extract(r.response_projection,'$.publicationId')=NEW.id AND
      json_extract(r.response_projection,'$.resultId')=NEW.result_id AND
      json_extract(r.response_projection,'$.verificationId')=NEW.verification_id AND
      w.state='active' AND w.github_authority='github-app-broker'
  ) THEN RAISE(ABORT, 'publication admission has no exact receipt') END;
END;
CREATE TRIGGER publications_admission_receipt_immutable BEFORE UPDATE OF admission_receipt_id ON publications
WHEN NEW.admission_receipt_id IS NOT OLD.admission_receipt_id
BEGIN SELECT RAISE(ABORT, 'publication admission receipt is immutable'); END;
CREATE TRIGGER publications_unreceipted_quarantine BEFORE UPDATE ON publications
WHEN OLD.admission_receipt_id IS NULL
BEGIN SELECT RAISE(ABORT, 'legacy unreceipted publication is quarantined'); END;
`

const explicitWorkspaceGitHubAuthoritySchema = `
ALTER TABLE workspaces ADD COLUMN github_authority TEXT NOT NULL DEFAULT 'github-app-broker'
CHECK(github_authority IN ('workspace-gh','github-app-broker'));
CREATE TRIGGER workspaces_github_authority_insert BEFORE INSERT ON workspaces
BEGIN
  SELECT CASE WHEN NEW.github_authority='workspace-gh' AND NEW.installation_id<>1
    THEN RAISE(ABORT, 'workspace gh legacy installation discriminator differs') END;
END;
CREATE TRIGGER workspaces_github_authority_update BEFORE UPDATE OF github_authority,installation_id ON workspaces
BEGIN
  SELECT CASE WHEN NEW.github_authority='workspace-gh' AND NEW.installation_id<>1
    THEN RAISE(ABORT, 'workspace gh legacy installation discriminator differs') END;
END;
`

// initialSchema carries the delivery lease bound enforced in SQL. The
// attempts_delivery_resume_integrity trigger below caps a resumed claim with
// `NEW.delivery_claim_expires_at > NEW.updated_at + 300000`; the literal
// 300000 is milliseconds and MUST equal taskstore.maxDeliveryLease (5m)
// declared in delivery.go. If one side changes, change both.
const initialSchema = `
CREATE TABLE schema_migrations (
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
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
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
    state TEXT NOT NULL CHECK(state IN ('queued','running','input_required','cancel_requested','uncertain','recovery_required','completed','failed','canceled')),
    terminal_reason TEXT CHECK(terminal_reason IS NULL OR length(CAST(terminal_reason AS BLOB)) BETWEEN 1 AND 1000),
    cancel_epoch INTEGER NOT NULL DEFAULT 0 CHECK(cancel_epoch IN (0,1)),
    cancel_actor_snapshot_id INTEGER REFERENCES actor_snapshots(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    cancel_reason TEXT CHECK(cancel_reason IS NULL OR length(CAST(cancel_reason AS BLOB)) BETWEEN 1 AND 500),
    cancel_requested_at INTEGER CHECK(cancel_requested_at IS NULL OR cancel_requested_at >= 0),
    cancel_receipt_id TEXT,
    cancel_attempt_id TEXT,
    cancel_attempt_event_id TEXT,
    cancel_task_event_id TEXT,
    cancel_effect_disposition TEXT CHECK(cancel_effect_disposition IS NULL OR cancel_effect_disposition IN ('none_prepared','reconcile_delivery','interrupt','none_terminal')),
    current_attempt_id TEXT NOT NULL,
    actor_snapshot_id INTEGER NOT NULL REFERENCES actor_snapshots(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    latest_event_cursor INTEGER NOT NULL DEFAULT 0 CHECK(latest_event_cursor >= 0),
    revision INTEGER NOT NULL CHECK(revision >= 1),
    created_at INTEGER NOT NULL CHECK(created_at >= 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
    CHECK(
        (cancel_epoch = 0 AND cancel_actor_snapshot_id IS NULL AND cancel_reason IS NULL AND cancel_requested_at IS NULL AND
         cancel_receipt_id IS NULL AND cancel_attempt_id IS NULL AND cancel_attempt_event_id IS NULL AND
         cancel_task_event_id IS NULL AND cancel_effect_disposition IS NULL) OR
        (cancel_epoch = 1 AND cancel_actor_snapshot_id IS NOT NULL AND cancel_requested_at IS NOT NULL AND
         cancel_receipt_id IS NOT NULL AND cancel_attempt_id IS NOT NULL AND cancel_attempt_event_id IS NOT NULL AND
         cancel_task_event_id IS NOT NULL AND cancel_effect_disposition IS NOT NULL)
    ),
    CHECK(cancel_epoch <> 0 OR state NOT IN ('cancel_requested','canceled')),
    CHECK(cancel_epoch = 0 OR state IN ('cancel_requested','uncertain','recovery_required','canceled')),
	CHECK(state <> 'canceled' OR terminal_reason = 'cancellation_acknowledged'),
    CHECK(cancel_requested_at IS NULL OR cancel_requested_at >= created_at),
    FOREIGN KEY(workspace_id, repository_id) REFERENCES workspaces(id, repository_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(current_attempt_id, id) REFERENCES attempts(id, task_id) ON UPDATE RESTRICT ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY(latest_event_cursor, id) REFERENCES events(cursor, task_id) ON UPDATE RESTRICT ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY(cancel_receipt_id) REFERENCES receipts(id) ON UPDATE RESTRICT ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY(cancel_attempt_id, id, workspace_id) REFERENCES attempts(id, task_id, workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY(cancel_attempt_event_id) REFERENCES events(id) ON UPDATE RESTRICT ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY(cancel_task_event_id) REFERENCES events(id) ON UPDATE RESTRICT ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
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
    state TEXT NOT NULL CHECK(state IN ('prepared','delivering','admitted','running','input_required','cancel_requested','uncertain','recovery_required','succeeded','failed','canceled','superseded')),
    delivery_phase TEXT NOT NULL CHECK(delivery_phase IN ('none','claimed','session_create_started','session_ready','prompt_started')),
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
    budget_snapshot TEXT NOT NULL CHECK(length(CAST(budget_snapshot AS BLOB)) BETWEEN 1 AND 16384 AND json_valid(budget_snapshot)),
    deadline INTEGER NOT NULL,
    delivery_claim_owner TEXT CHECK(delivery_claim_owner IS NULL OR length(CAST(delivery_claim_owner AS BLOB)) BETWEEN 1 AND 64),
    delivery_claim_expires_at INTEGER,
    delivery_started_at INTEGER,
    admitted_at INTEGER,
    opencode_log_aggregate_id TEXT CHECK(opencode_log_aggregate_id IS NULL OR length(CAST(opencode_log_aggregate_id AS BLOB)) BETWEEN 1 AND 256),
    opencode_log_seq INTEGER NOT NULL DEFAULT 0 CHECK(opencode_log_seq >= 0),
    cancellation_ack_at INTEGER,
    recovery_reason TEXT CHECK(recovery_reason IS NULL OR length(CAST(recovery_reason AS BLOB)) BETWEEN 1 AND 1000),
    terminal_reason TEXT CHECK(terminal_reason IS NULL OR length(CAST(terminal_reason AS BLOB)) BETWEEN 1 AND 1000),
    revision INTEGER NOT NULL CHECK(revision >= 1),
    created_at INTEGER NOT NULL CHECK(created_at >= 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
    CHECK(deadline > created_at),
	CHECK(state <> 'prepared' OR delivery_phase = 'none'),
	CHECK(state <> 'delivering' OR delivery_phase <> 'none'),
	CHECK(state NOT IN ('admitted','running','input_required','succeeded') OR delivery_phase = 'prompt_started'),
	CHECK((delivery_phase = 'none') = (delivery_started_at IS NULL)),
    CHECK((delivery_claim_owner IS NULL) = (delivery_claim_expires_at IS NULL)),
    CHECK(state = 'delivering' OR (delivery_claim_owner IS NULL AND delivery_claim_expires_at IS NULL)),
    CHECK(state <> 'delivering' OR (delivery_claim_owner IS NOT NULL AND delivery_started_at IS NOT NULL AND admitted_at IS NULL)),
    CHECK(state <> 'prepared' OR (delivery_started_at IS NULL AND admitted_at IS NULL)),
    CHECK(state <> 'admitted' OR (delivery_started_at IS NOT NULL AND admitted_at IS NOT NULL)),
    CHECK(admitted_at IS NULL OR state NOT IN ('prepared','delivering')),
    CHECK(delivery_claim_expires_at IS NULL OR delivery_claim_expires_at >= created_at),
    CHECK(delivery_started_at IS NULL OR delivery_started_at >= created_at),
    CHECK(delivery_claim_expires_at IS NULL OR delivery_claim_expires_at > delivery_started_at),
    CHECK(admitted_at IS NULL OR admitted_at >= created_at),
    CHECK((opencode_log_aggregate_id IS NULL AND opencode_log_seq = 0) OR opencode_log_aggregate_id IS NOT NULL),
    CHECK(cancellation_ack_at IS NULL OR cancellation_ack_at >= created_at),
	CHECK((state <> 'canceled' AND cancellation_ack_at IS NULL) OR
	      (state = 'canceled' AND cancellation_ack_at IS NOT NULL AND terminal_reason = 'cancellation_acknowledged')),
    UNIQUE(task_id, sequence),
    UNIQUE(opencode_session_id),
    UNIQUE(opencode_session_id, opencode_message_id),
    FOREIGN KEY(task_id, workspace_id) REFERENCES tasks(id, workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    UNIQUE(id, task_id),
    UNIQUE(id, task_id, workspace_id)
) STRICT;
CREATE INDEX attempts_task_state ON attempts(task_id, state);
CREATE INDEX attempts_delivery_claim ON attempts(delivery_claim_expires_at) WHERE delivery_claim_owner IS NOT NULL;
CREATE UNIQUE INDEX attempts_one_effecting_per_workspace ON attempts(workspace_id)
WHERE state IN ('delivering','admitted','running','input_required','cancel_requested','uncertain','recovery_required');
CREATE TRIGGER attempts_immutable_inputs BEFORE UPDATE ON attempts
WHEN NEW.id <> OLD.id OR NEW.task_id <> OLD.task_id OR NEW.workspace_id <> OLD.workspace_id OR NEW.sequence <> OLD.sequence OR
     NEW.opencode_session_id <> OLD.opencode_session_id OR NEW.opencode_message_id <> OLD.opencode_message_id OR
     NEW.prompt_sha256 <> OLD.prompt_sha256 OR NEW.base_sha <> OLD.base_sha OR
     NEW.image_digest <> OLD.image_digest OR NEW.opencode_protocol <> OLD.opencode_protocol OR
     NEW.execution_contract_version <> OLD.execution_contract_version OR NEW.agent <> OLD.agent OR
     NEW.model_provider <> OLD.model_provider OR NEW.model <> OLD.model OR
     NEW.budget_snapshot <> OLD.budget_snapshot OR NEW.deadline <> OLD.deadline OR NEW.created_at <> OLD.created_at
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
CREATE UNIQUE INDEX events_one_attempt_canceled ON events(attempt_id) WHERE type='attempt.canceled';
CREATE UNIQUE INDEX events_one_task_canceled ON events(task_id) WHERE type='task.canceled';

CREATE TRIGGER attempts_delivery_phase_progression BEFORE UPDATE OF delivery_phase ON attempts
WHEN NEW.delivery_phase <> OLD.delivery_phase
BEGIN
    SELECT CASE WHEN NEW.revision <> OLD.revision + 1 OR NEW.updated_at < OLD.updated_at
        THEN RAISE(ABORT, 'delivery phase update must advance revision') END;
    SELECT CASE WHEN NOT (
        (OLD.delivery_phase='none' AND NEW.delivery_phase='claimed' AND OLD.state='prepared' AND NEW.state='delivering' AND
         EXISTS (SELECT 1 FROM events e WHERE e.attempt_id=OLD.id AND e.type='attempt.delivery_started'
                 AND e.occurred_at=NEW.updated_at AND json_extract(e.payload,'$.phase')='claimed')) OR
        (OLD.state='delivering' AND NEW.state='delivering' AND
         ((OLD.delivery_phase='claimed' AND NEW.delivery_phase='session_create_started') OR
          (OLD.delivery_phase='session_create_started' AND NEW.delivery_phase='session_ready') OR
          (OLD.delivery_phase='session_ready' AND NEW.delivery_phase='prompt_started')) AND
         EXISTS (SELECT 1 FROM events e WHERE e.attempt_id=OLD.id AND e.type='attempt.delivery_phase_advanced'
                 AND e.occurred_at=NEW.updated_at AND json_extract(e.payload,'$.from')=OLD.delivery_phase
                 AND json_extract(e.payload,'$.to')=NEW.delivery_phase))
    ) THEN RAISE(ABORT, 'invalid delivery phase transition') END;
END;

CREATE TRIGGER attempts_delivery_resume_integrity BEFORE UPDATE OF state ON attempts
WHEN OLD.state='uncertain' AND NEW.state='delivering'
BEGIN
    SELECT CASE WHEN OLD.delivery_phase NOT IN ('claimed','session_create_started','session_ready') OR
                          NEW.delivery_phase <> OLD.delivery_phase
        THEN RAISE(ABORT, 'invalid uncertain delivery resume phase') END;
    SELECT CASE WHEN OLD.delivery_claim_owner IS NOT NULL OR OLD.delivery_claim_expires_at IS NOT NULL OR
                          NEW.delivery_claim_owner IS NULL OR NEW.delivery_claim_expires_at IS NULL OR
                          NEW.delivery_started_at IS NOT OLD.delivery_started_at OR NEW.admitted_at IS NOT OLD.admitted_at OR
                          NEW.recovery_reason IS NOT NULL OR NEW.revision <> OLD.revision + 1 OR NEW.updated_at < OLD.updated_at OR
                          NEW.delivery_claim_expires_at <= NEW.updated_at OR NEW.delivery_claim_expires_at > NEW.deadline OR
                          NEW.delivery_claim_expires_at > NEW.updated_at + 300000
        THEN RAISE(ABORT, 'invalid uncertain delivery resume shape') END;
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM events ae
        JOIN events te ON te.workspace_id=ae.workspace_id AND te.task_id=ae.task_id AND
                          te.attempt_id IS NULL AND te.type='task.running' AND
                          te.occurred_at=ae.occurred_at AND te.actor_snapshot_id=ae.actor_snapshot_id AND
                          te.payload=ae.payload AND te.cursor>ae.cursor
        JOIN actor_snapshots actor ON actor.id=ae.actor_snapshot_id AND actor.actor_type='recovery'
        WHERE ae.attempt_id=OLD.id AND ae.type='attempt.delivery_resumed' AND ae.occurred_at=NEW.updated_at
          AND json_extract(ae.payload,'$.attemptId')=OLD.id
          AND json_extract(ae.payload,'$.phase')=OLD.delivery_phase
          AND json_extract(ae.payload,'$.leaseOwner')=NEW.delivery_claim_owner
          AND json_extract(ae.payload,'$.leaseExpiresAtMillis')=NEW.delivery_claim_expires_at
          AND json_extract(ae.payload,'$.expectedAttemptRevision')=OLD.revision
          AND json_extract(ae.payload,'$.expectedTaskRevision')=(SELECT revision FROM tasks WHERE id=OLD.task_id)
          AND EXISTS (SELECT 1 FROM tasks t WHERE t.id=OLD.task_id AND t.workspace_id=OLD.workspace_id
                      AND t.current_attempt_id=OLD.id AND t.state='uncertain')
    ) THEN RAISE(ABORT, 'uncertain delivery resume has no exact event') END;
END;

CREATE TRIGGER receipts_immutable_update BEFORE UPDATE ON receipts
BEGIN SELECT RAISE(ABORT, 'receipts are immutable'); END;
CREATE TRIGGER receipts_immutable_delete BEFORE DELETE ON receipts
BEGIN SELECT RAISE(ABORT, 'receipts are immutable'); END;
CREATE TRIGGER events_immutable_update BEFORE UPDATE ON events
BEGIN SELECT RAISE(ABORT, 'events are immutable'); END;
CREATE TRIGGER events_immutable_delete BEFORE DELETE ON events
BEGIN SELECT RAISE(ABORT, 'events are immutable'); END;

CREATE TRIGGER tasks_cancellation_integrity BEFORE UPDATE ON tasks
WHEN OLD.cancel_epoch = 0 AND NEW.cancel_epoch = 1
BEGIN
    SELECT CASE WHEN OLD.state NOT IN ('queued','running','input_required','uncertain','recovery_required')
        THEN RAISE(ABORT, 'terminal or invalid task cancellation transition') END;
    SELECT CASE WHEN NEW.current_attempt_id <> NEW.cancel_attempt_id
        THEN RAISE(ABORT, 'cancellation attempt is not current') END;
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM attempts a WHERE a.id=NEW.cancel_attempt_id AND a.task_id=NEW.id AND a.workspace_id=NEW.workspace_id AND (
            (NEW.cancel_effect_disposition='none_prepared' AND a.state='prepared') OR
            (NEW.cancel_effect_disposition='reconcile_delivery' AND a.state='delivering') OR
            (NEW.cancel_effect_disposition='interrupt' AND a.state IN ('admitted','running','input_required','uncertain','recovery_required')) OR
            (NEW.cancel_effect_disposition='none_terminal' AND a.state IN ('succeeded','failed','canceled','superseded'))
        )
    ) THEN RAISE(ABORT, 'invalid cancellation effect disposition') END;
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM receipts r
        WHERE r.id=NEW.cancel_receipt_id AND r.workspace_id=NEW.workspace_id AND r.command_kind='task.cancel'
          AND r.target_type='task' AND r.target_id=NEW.id AND r.actor_snapshot_id=NEW.cancel_actor_snapshot_id
          AND r.accepted_at=NEW.cancel_requested_at
    ) THEN RAISE(ABORT, 'invalid cancellation receipt ownership') END;
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM events e
        WHERE e.id=NEW.cancel_attempt_event_id AND e.workspace_id=NEW.workspace_id AND e.task_id=NEW.id
          AND e.attempt_id=NEW.cancel_attempt_id AND e.entity_type='attempt' AND e.entity_id=NEW.cancel_attempt_id
          AND e.type='attempt.cancel_requested' AND e.actor_snapshot_id=NEW.cancel_actor_snapshot_id
          AND e.occurred_at=NEW.cancel_requested_at
    ) THEN RAISE(ABORT, 'invalid cancellation attempt event ownership') END;
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM events e
        WHERE e.id=NEW.cancel_task_event_id AND e.workspace_id=NEW.workspace_id AND e.task_id=NEW.id
          AND e.attempt_id IS NULL AND e.entity_type='task' AND e.entity_id=NEW.id
          AND e.type='task.cancel_requested' AND e.actor_snapshot_id=NEW.cancel_actor_snapshot_id
          AND e.occurred_at=NEW.cancel_requested_at AND e.cursor=NEW.latest_event_cursor
    ) THEN RAISE(ABORT, 'invalid cancellation task event ownership') END;
    SELECT CASE WHEN (SELECT cursor FROM events WHERE id=NEW.cancel_attempt_event_id) >=
                          (SELECT cursor FROM events WHERE id=NEW.cancel_task_event_id)
        THEN RAISE(ABORT, 'invalid cancellation event order') END;
END;

CREATE TRIGGER tasks_cancellation_insert_guard BEFORE INSERT ON tasks
WHEN NEW.cancel_epoch <> 0 OR NEW.state IN ('cancel_requested','canceled')
BEGIN SELECT RAISE(ABORT, 'task cancellation must be recorded by transition'); END;

CREATE TRIGGER tasks_cancellation_immutable BEFORE UPDATE ON tasks
WHEN OLD.cancel_epoch = 1 AND (
    NEW.cancel_epoch <> OLD.cancel_epoch OR
    NEW.cancel_actor_snapshot_id IS NOT OLD.cancel_actor_snapshot_id OR
    NEW.cancel_reason IS NOT OLD.cancel_reason OR
    NEW.cancel_requested_at IS NOT OLD.cancel_requested_at OR
    NEW.cancel_receipt_id IS NOT OLD.cancel_receipt_id OR
    NEW.cancel_attempt_id IS NOT OLD.cancel_attempt_id OR
    NEW.cancel_attempt_event_id IS NOT OLD.cancel_attempt_event_id OR
    NEW.cancel_task_event_id IS NOT OLD.cancel_task_event_id OR
    NEW.cancel_effect_disposition IS NOT OLD.cancel_effect_disposition
)
BEGIN SELECT RAISE(ABORT, 'task cancellation is immutable'); END;

CREATE TRIGGER attempts_cancel_requested_integrity BEFORE UPDATE OF state ON attempts
WHEN NEW.state = 'cancel_requested' AND OLD.state <> 'cancel_requested'
BEGIN
    SELECT CASE WHEN OLD.state NOT IN ('prepared','delivering','admitted','running','input_required','uncertain','recovery_required')
        THEN RAISE(ABORT, 'invalid attempt cancellation transition') END;
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM tasks t WHERE t.id=NEW.task_id AND t.workspace_id=NEW.workspace_id
          AND t.current_attempt_id=NEW.id AND t.cancel_attempt_id=NEW.id
          AND t.cancel_epoch=1 AND t.state='cancel_requested'
    ) THEN RAISE(ABORT, 'attempt cancellation has no owning task fence') END;
END;

CREATE TRIGGER attempts_cancellation_ack_integrity BEFORE UPDATE OF cancellation_ack_at ON attempts
WHEN OLD.cancellation_ack_at IS NULL AND NEW.cancellation_ack_at IS NOT NULL
BEGIN
    SELECT CASE WHEN NEW.state <> 'canceled' OR NEW.terminal_reason <> 'cancellation_acknowledged' OR
                          NEW.cancellation_ack_at <> NEW.updated_at OR NEW.revision <> OLD.revision + 1 OR
                          NEW.delivery_claim_owner IS NOT NULL OR NEW.delivery_claim_expires_at IS NOT NULL OR
                          NOT (OLD.state='cancel_requested' OR
                               (OLD.state IN ('succeeded','failed','canceled','superseded') AND EXISTS (
                                   SELECT 1 FROM tasks t WHERE t.id=OLD.task_id AND
                                   t.cancel_effect_disposition='none_terminal')))
        THEN RAISE(ABORT, 'invalid cancellation acknowledgment shape') END;
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1
        FROM tasks t
        JOIN events ae ON ae.workspace_id=t.workspace_id AND ae.task_id=t.id AND
                          ae.attempt_id=OLD.id AND ae.entity_type='attempt' AND ae.entity_id=OLD.id AND
                          ae.type='attempt.canceled' AND ae.occurred_at=NEW.cancellation_ack_at
        JOIN events te ON te.workspace_id=t.workspace_id AND te.task_id=t.id AND
                          te.attempt_id IS NULL AND te.entity_type='task' AND te.entity_id=t.id AND
                          te.type='task.canceled' AND te.occurred_at=NEW.cancellation_ack_at AND
                          te.actor_snapshot_id=ae.actor_snapshot_id AND te.payload=ae.payload AND te.cursor>ae.cursor
        JOIN actor_snapshots actor ON actor.id=ae.actor_snapshot_id AND actor.actor_type IN ('system','recovery')
        WHERE t.id=OLD.task_id AND t.workspace_id=OLD.workspace_id AND t.current_attempt_id=OLD.id AND
              t.cancel_attempt_id=OLD.id AND t.cancel_epoch=1 AND t.state='cancel_requested' AND
              json_extract(ae.payload,'$.taskId')=t.id AND json_extract(ae.payload,'$.attemptId')=OLD.id AND
              json_extract(ae.payload,'$.cancelEpoch')=1 AND
              json_extract(ae.payload,'$.expectedAttemptRevision')=OLD.revision AND
              json_extract(ae.payload,'$.expectedTaskRevision')=t.revision AND
              json_extract(ae.payload,'$.disposition')=t.cancel_effect_disposition AND
              json_extract(ae.payload,'$.terminalReason')='cancellation_acknowledged' AND
              json_type(ae.payload,'$.evidence')='object' AND
              substr(json_extract(ae.payload,'$.evidenceSha256'),1,7)='sha256:' AND
              length(json_extract(ae.payload,'$.evidenceSha256'))=71 AND
              substr(json_extract(ae.payload,'$.evidenceSha256'),8) NOT GLOB '*[^0-9a-f]*'
    ) THEN RAISE(ABORT, 'cancellation acknowledgment has no exact events') END;
END;

CREATE TRIGGER attempts_cancellation_ack_insert_guard BEFORE INSERT ON attempts
WHEN NEW.cancellation_ack_at IS NOT NULL OR NEW.state='canceled'
BEGIN SELECT RAISE(ABORT, 'attempt cancellation acknowledgment must be recorded by transition'); END;

CREATE TRIGGER tasks_cancellation_ack_integrity BEFORE UPDATE OF state ON tasks
WHEN OLD.state<>'canceled' AND NEW.state='canceled'
BEGIN
    SELECT CASE WHEN OLD.state <> 'cancel_requested' OR OLD.cancel_epoch <> 1 OR NEW.cancel_epoch <> 1 OR
                          NEW.terminal_reason <> 'cancellation_acknowledged' OR
                          NEW.revision <> OLD.revision + 1 OR NEW.updated_at < OLD.updated_at
        THEN RAISE(ABORT, 'invalid canceled task shape') END;
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1
        FROM attempts a
        JOIN events ae ON ae.workspace_id=OLD.workspace_id AND ae.task_id=OLD.id AND
                          ae.attempt_id=a.id AND ae.type='attempt.canceled' AND
                          ae.occurred_at=a.cancellation_ack_at
        JOIN events te ON te.workspace_id=OLD.workspace_id AND te.task_id=OLD.id AND
                          te.attempt_id IS NULL AND te.type='task.canceled' AND
                          te.occurred_at=a.cancellation_ack_at AND te.actor_snapshot_id=ae.actor_snapshot_id AND
                          te.payload=ae.payload AND te.cursor>ae.cursor AND te.cursor=NEW.latest_event_cursor
        JOIN actor_snapshots actor ON actor.id=ae.actor_snapshot_id AND actor.actor_type IN ('system','recovery')
        WHERE a.id=OLD.current_attempt_id AND a.task_id=OLD.id AND a.workspace_id=OLD.workspace_id AND
              a.state='canceled' AND a.cancellation_ack_at=NEW.updated_at AND
              a.terminal_reason='cancellation_acknowledged' AND a.delivery_claim_owner IS NULL AND
              a.delivery_claim_expires_at IS NULL AND
              json_extract(ae.payload,'$.taskId')=OLD.id AND json_extract(ae.payload,'$.attemptId')=a.id AND
              json_extract(ae.payload,'$.cancelEpoch')=OLD.cancel_epoch AND
              json_extract(ae.payload,'$.expectedAttemptRevision')=a.revision-1 AND
              json_extract(ae.payload,'$.expectedTaskRevision')=OLD.revision AND
              json_extract(ae.payload,'$.disposition')=OLD.cancel_effect_disposition AND
              json_extract(ae.payload,'$.terminalReason')=NEW.terminal_reason
    ) THEN RAISE(ABORT, 'canceled task has no exact acknowledged attempt') END;
END;

CREATE TRIGGER attempts_cancellation_ack_immutable BEFORE UPDATE ON attempts
WHEN OLD.cancellation_ack_at IS NOT NULL AND (
    NEW.cancellation_ack_at IS NOT OLD.cancellation_ack_at OR NEW.state <> OLD.state OR
    NEW.terminal_reason IS NOT OLD.terminal_reason
)
BEGIN SELECT RAISE(ABORT, 'attempt cancellation acknowledgment is immutable'); END;

CREATE TRIGGER tasks_canceled_immutable BEFORE UPDATE ON tasks
WHEN OLD.state='canceled' AND (NEW.state <> OLD.state OR NEW.terminal_reason IS NOT OLD.terminal_reason)
BEGIN SELECT RAISE(ABORT, 'canceled task terminal state is immutable'); END;
`

const executionAndResultSchema = `
ALTER TABLE attempts ADD COLUMN sealed_result_id TEXT REFERENCES results(id) ON UPDATE RESTRICT ON DELETE RESTRICT;
ALTER TABLE tasks ADD COLUMN sealed_result_id TEXT REFERENCES results(id) ON UPDATE RESTRICT ON DELETE RESTRICT;
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
    updated_at INTEGER NOT NULL CHECK(updated_at = sealed_at),
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

CREATE TRIGGER attempts_execution_projection_integrity BEFORE UPDATE OF state ON attempts
WHEN OLD.state IN ('admitted','running','input_required') AND NEW.state <> OLD.state AND
     NEW.state IN ('running','input_required','recovery_required','failed','succeeded')
BEGIN
    SELECT CASE WHEN NOT (
        (OLD.state='admitted' AND NEW.state IN ('running','input_required','recovery_required','failed','succeeded')) OR
        (OLD.state='running' AND NEW.state IN ('input_required','recovery_required','failed','succeeded')) OR
        (OLD.state='input_required' AND NEW.state IN ('running','recovery_required','failed'))
    ) THEN RAISE(ABORT, 'invalid execution projection transition') END;
    SELECT CASE WHEN NEW.revision<>OLD.revision+1 OR NEW.updated_at<OLD.updated_at OR
                          NEW.delivery_claim_owner IS NOT NULL OR NEW.delivery_claim_expires_at IS NOT NULL OR
                          NEW.delivery_phase<>'prompt_started' OR NEW.admitted_at IS NULL OR
                          (NEW.state='recovery_required')<>(NEW.recovery_reason IS NOT NULL) OR
                          (NEW.state='failed')<>(NEW.terminal_reason IS NOT NULL)
        THEN RAISE(ABORT, 'invalid execution projection shape') END;
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM tasks t
        JOIN events ae ON ae.workspace_id=t.workspace_id AND ae.task_id=t.id AND ae.attempt_id=OLD.id AND
                          ae.entity_type='attempt' AND ae.entity_id=OLD.id AND ae.type='attempt.'||NEW.state AND
                          ae.occurred_at=NEW.updated_at
        JOIN events te ON te.workspace_id=t.workspace_id AND te.task_id=t.id AND te.attempt_id IS NULL AND
                          te.entity_type='task' AND te.entity_id=t.id AND te.occurred_at=ae.occurred_at AND
                          te.actor_snapshot_id=ae.actor_snapshot_id AND te.payload=ae.payload AND te.cursor>ae.cursor
        JOIN actor_snapshots actor ON actor.id=ae.actor_snapshot_id AND actor.actor_type IN ('system','recovery')
        WHERE t.id=OLD.task_id AND t.workspace_id=OLD.workspace_id AND t.current_attempt_id=OLD.id AND
              t.cancel_epoch=0 AND t.state=CASE OLD.state WHEN 'input_required' THEN 'input_required' ELSE 'running' END AND
              te.type=CASE NEW.state WHEN 'succeeded' THEN 'task.execution_succeeded' ELSE 'task.'||NEW.state END AND
              json_extract(ae.payload,'$.taskId')=t.id AND json_extract(ae.payload,'$.attemptId')=OLD.id AND
              json_extract(ae.payload,'$.expectedAttemptRevision')=OLD.revision AND
              json_extract(ae.payload,'$.expectedTaskRevision')=t.revision AND
              json_extract(ae.payload,'$.from')=OLD.state AND json_extract(ae.payload,'$.to')=NEW.state AND
              json_extract(ae.payload,'$.opencodeSessionId')=OLD.opencode_session_id AND
              json_extract(ae.payload,'$.opencodeMessageId')=OLD.opencode_message_id AND
              json_type(ae.payload,'$.evidence')='object' AND
              length(json_extract(ae.payload,'$.evidenceSha256'))=71 AND
              substr(json_extract(ae.payload,'$.evidenceSha256'),1,7)='sha256:' AND
              substr(json_extract(ae.payload,'$.evidenceSha256'),8) NOT GLOB '*[^0-9a-f]*'
    ) THEN RAISE(ABORT, 'execution projection has no exact events') END;
END;

CREATE TRIGGER tasks_execution_projection_integrity BEFORE UPDATE OF state ON tasks
WHEN OLD.state IN ('running','input_required') AND NEW.state <> OLD.state AND NEW.state IN ('running','input_required','recovery_required','failed') AND
     EXISTS (SELECT 1 FROM events e WHERE e.task_id=OLD.id AND e.attempt_id=OLD.current_attempt_id AND
             e.type='attempt.'||NEW.state AND e.occurred_at=NEW.updated_at)
BEGIN
    SELECT CASE WHEN NEW.cancel_epoch<>0 OR NEW.revision<>OLD.revision+1 OR NEW.updated_at<OLD.updated_at OR
                          (NEW.state='failed')<>(NEW.terminal_reason IS NOT NULL)
        THEN RAISE(ABORT, 'invalid task execution projection shape') END;
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM attempts a
        JOIN events ae ON ae.workspace_id=OLD.workspace_id AND ae.task_id=OLD.id AND ae.attempt_id=a.id AND
                          ae.type='attempt.'||a.state AND ae.occurred_at=NEW.updated_at
        JOIN events te ON te.workspace_id=OLD.workspace_id AND te.task_id=OLD.id AND te.attempt_id IS NULL AND
                          te.type='task.'||NEW.state AND te.occurred_at=ae.occurred_at AND
                          te.actor_snapshot_id=ae.actor_snapshot_id AND te.payload=ae.payload AND
                          te.cursor>ae.cursor AND te.cursor=NEW.latest_event_cursor
        WHERE a.id=OLD.current_attempt_id AND a.task_id=OLD.id AND a.workspace_id=OLD.workspace_id AND
              a.state=CASE NEW.state WHEN 'input_required' THEN 'input_required' WHEN 'recovery_required' THEN 'recovery_required' WHEN 'failed' THEN 'failed' ELSE 'running' END AND
              json_extract(ae.payload,'$.expectedAttemptRevision')=a.revision-1 AND
              json_extract(ae.payload,'$.expectedTaskRevision')=OLD.revision
    ) THEN RAISE(ABORT, 'task execution projection has no exact attempt') END;
END;

CREATE TRIGGER results_insert_integrity BEFORE INSERT ON results
BEGIN
    SELECT CASE WHEN (SELECT count(*) FROM result_manifest m WHERE m.result_id=NEW.id)<>NEW.manifest_entries OR
                          (NEW.manifest_entries>0 AND ((SELECT min(ordinal) FROM result_manifest WHERE result_id=NEW.id)<>0 OR
                           (SELECT max(ordinal) FROM result_manifest WHERE result_id=NEW.id)<>NEW.manifest_entries-1))
        THEN RAISE(ABORT, 'result manifest is incomplete') END;
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM tasks t
        JOIN attempts a ON a.id=t.current_attempt_id AND a.task_id=t.id AND a.workspace_id=t.workspace_id
        JOIN events ae ON ae.id=NEW.sealed_event_id AND ae.workspace_id=t.workspace_id AND ae.task_id=t.id AND
                          ae.attempt_id=a.id AND ae.entity_type='attempt' AND ae.entity_id=a.id AND ae.type='attempt.result_sealed'
        JOIN events te ON te.id=NEW.completed_event_id AND te.workspace_id=t.workspace_id AND te.task_id=t.id AND
                          te.attempt_id IS NULL AND te.entity_type='task' AND te.entity_id=t.id AND te.type='task.completed' AND
                          te.occurred_at=ae.occurred_at AND te.actor_snapshot_id=ae.actor_snapshot_id AND
                          te.payload=ae.payload AND te.cursor>ae.cursor
        JOIN actor_snapshots actor ON actor.id=NEW.creator_actor_snapshot_id AND actor.id=ae.actor_snapshot_id AND
                                      actor.actor_type IN ('system','recovery')
        WHERE t.id=NEW.task_id AND t.workspace_id=NEW.workspace_id AND t.repository_id=NEW.repository_id AND
              t.base_sha=NEW.base_sha AND t.state='running' AND t.cancel_epoch=0 AND t.revision=json_extract(ae.payload,'$.expectedTaskRevision') AND
              a.id=NEW.attempt_id AND a.state='succeeded' AND a.revision=json_extract(ae.payload,'$.expectedAttemptRevision') AND
              a.base_sha=NEW.base_sha AND a.opencode_session_id=NEW.opencode_session_id AND
              a.opencode_message_id=NEW.opencode_message_id AND ae.occurred_at=NEW.sealed_at AND
              json_extract(ae.payload,'$.resultId')=NEW.id AND json_extract(ae.payload,'$.taskId')=NEW.task_id AND
              json_extract(ae.payload,'$.attemptId')=NEW.attempt_id AND json_extract(ae.payload,'$.repositoryId')=NEW.repository_id AND
              json_extract(ae.payload,'$.baseSha')=NEW.base_sha AND json_extract(ae.payload,'$.resultCommit')=NEW.result_commit AND
              json_extract(ae.payload,'$.treeOid')=NEW.tree_oid AND json_extract(ae.payload,'$.outcome')=NEW.outcome AND
              json_extract(ae.payload,'$.clean')=1 AND json_extract(ae.payload,'$.manifestEntries')=NEW.manifest_entries AND
              json_extract(ae.payload,'$.opencodeSessionId')=NEW.opencode_session_id AND
              json_extract(ae.payload,'$.opencodeMessageId')=NEW.opencode_message_id AND
              json_extract(ae.payload,'$.collectedAtMillis')=NEW.collected_at AND
              json_extract(ae.payload,'$.policyVersion')=NEW.policy_version AND
              json_extract(ae.payload,'$.manifestSha256')='sha256:'||lower(hex(NEW.manifest_sha256)) AND
              json_extract(ae.payload,'$.evidenceSha256')='sha256:'||lower(hex(NEW.evidence_sha256))
    ) THEN RAISE(ABORT, 'sealed result has no exact current proof') END;
END;

CREATE TRIGGER attempts_result_seal_integrity BEFORE UPDATE OF sealed_result_id ON attempts
WHEN OLD.sealed_result_id IS NULL AND NEW.sealed_result_id IS NOT NULL
BEGIN
    SELECT CASE WHEN OLD.state<>'succeeded' OR NEW.state<>'succeeded' OR NEW.revision<>OLD.revision+1 OR
                          NEW.updated_at<OLD.updated_at OR NOT EXISTS (
        SELECT 1 FROM results r JOIN events e ON e.id=r.sealed_event_id
        WHERE r.id=NEW.sealed_result_id AND r.task_id=OLD.task_id AND r.attempt_id=OLD.id AND
              r.workspace_id=OLD.workspace_id AND r.sealed_at=NEW.updated_at AND
              json_extract(e.payload,'$.expectedAttemptRevision')=OLD.revision
    ) THEN RAISE(ABORT, 'invalid attempt result seal') END;
END;

CREATE TRIGGER tasks_result_seal_integrity BEFORE UPDATE OF state,sealed_result_id ON tasks
WHEN OLD.state<>'completed' AND NEW.state='completed'
BEGIN
    SELECT CASE WHEN OLD.state<>'running' OR OLD.cancel_epoch<>0 OR NEW.cancel_epoch<>0 OR
                          OLD.sealed_result_id IS NOT NULL OR NEW.sealed_result_id IS NULL OR
                          NEW.terminal_reason IS NOT NULL OR NEW.revision<>OLD.revision+1 OR NEW.updated_at<OLD.updated_at OR
                          NOT EXISTS (
        SELECT 1 FROM results r
        JOIN attempts a ON a.id=OLD.current_attempt_id AND a.task_id=OLD.id AND a.workspace_id=OLD.workspace_id
        JOIN events ae ON ae.id=r.sealed_event_id
        JOIN events te ON te.id=r.completed_event_id
        WHERE r.id=NEW.sealed_result_id AND r.task_id=OLD.id AND r.attempt_id=a.id AND r.workspace_id=OLD.workspace_id AND
              a.state='succeeded' AND a.sealed_result_id=r.id AND r.sealed_at=NEW.updated_at AND
              te.cursor=NEW.latest_event_cursor AND ae.cursor<te.cursor AND
              json_extract(ae.payload,'$.expectedAttemptRevision')=a.revision-1 AND
              json_extract(ae.payload,'$.expectedTaskRevision')=OLD.revision
    ) THEN RAISE(ABORT, 'invalid completed task result seal') END;
END;

CREATE TRIGGER results_immutable_update BEFORE UPDATE ON results
BEGIN SELECT RAISE(ABORT, 'results are immutable'); END;
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
CREATE TRIGGER tasks_completed_insert_guard BEFORE INSERT ON tasks
WHEN NEW.state='completed' OR NEW.sealed_result_id IS NOT NULL
BEGIN SELECT RAISE(ABORT, 'task result must be recorded by seal transition'); END;
`

const verificationAndPublicationSchema = `
CREATE TABLE journal_events (
    id TEXT PRIMARY KEY CHECK(
        length(id)=40 AND substr(id,1,4)='fev_' AND substr(id,13,1)='-' AND
        substr(id,18,1)='-' AND substr(id,19,1)='7' AND substr(id,23,1)='-' AND
        substr(id,24,1) IN ('8','9','a','b') AND substr(id,28,1)='-' AND
        length(replace(substr(id,5),'-',''))=32 AND replace(substr(id,5),'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    task_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    result_id TEXT NOT NULL,
    entity_type TEXT NOT NULL CHECK(entity_type IN ('verification','publication')),
    entity_id TEXT NOT NULL CHECK(length(entity_id)=40),
    type TEXT NOT NULL CHECK(
        (entity_type='verification' AND substr(type,1,13)='verification.') OR
        (entity_type='publication' AND substr(type,1,12)='publication.')
    ),
    from_state TEXT,
    to_state TEXT NOT NULL,
    entity_revision INTEGER NOT NULL CHECK(entity_revision>=1),
    occurred_at INTEGER NOT NULL CHECK(occurred_at>=0),
    actor_snapshot_id INTEGER NOT NULL REFERENCES actor_snapshots(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    evidence_sha256 BLOB NOT NULL CHECK(length(evidence_sha256)=32),
    payload TEXT NOT NULL CHECK(length(CAST(payload AS BLOB))<=65536 AND json_valid(payload)),
    FOREIGN KEY(task_id, workspace_id) REFERENCES tasks(id, workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(attempt_id, task_id, workspace_id) REFERENCES attempts(id, task_id, workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(result_id, task_id, attempt_id) REFERENCES results(id, task_id, attempt_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    CHECK((entity_revision=1 AND from_state IS NULL) OR (entity_revision>1 AND from_state IS NOT NULL)),
    CHECK(json_type(payload,'$.detail')='object' AND json_type(payload,'$.evidence')='object' AND
          json_extract(payload,'$.evidenceSha256')='sha256:'||lower(hex(evidence_sha256))),
    UNIQUE(entity_type,entity_id,entity_revision)
) STRICT;
CREATE INDEX journal_events_entity ON journal_events(entity_type,entity_id,entity_revision);
CREATE TRIGGER journal_events_id_collision BEFORE INSERT ON journal_events
WHEN EXISTS (SELECT 1 FROM events e WHERE e.id=NEW.id)
BEGIN SELECT RAISE(ABORT, 'journal event ID already belongs to task event'); END;
CREATE TRIGGER events_journal_id_collision BEFORE INSERT ON events
WHEN EXISTS (SELECT 1 FROM journal_events e WHERE e.id=NEW.id)
BEGIN SELECT RAISE(ABORT, 'event ID already belongs to journal event'); END;
CREATE TRIGGER journal_events_immutable_update BEFORE UPDATE ON journal_events
BEGIN SELECT RAISE(ABORT, 'journal events are immutable'); END;
CREATE TRIGGER journal_events_immutable_delete BEFORE DELETE ON journal_events
BEGIN SELECT RAISE(ABORT, 'journal events are immutable'); END;

CREATE TABLE verifications (
    id TEXT PRIMARY KEY CHECK(
        length(id)=40 AND substr(id,1,4)='ver_' AND substr(id,13,1)='-' AND
        substr(id,18,1)='-' AND substr(id,19,1)='7' AND substr(id,23,1)='-' AND
        substr(id,24,1) IN ('8','9','a','b') AND substr(id,28,1)='-' AND
        length(replace(substr(id,5),'-',''))=32 AND replace(substr(id,5),'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    result_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('prepared','running','succeeded','failed','recovery_required')),
    policy_name TEXT NOT NULL CHECK(length(CAST(policy_name AS BLOB)) BETWEEN 1 AND 64 AND
      substr(policy_name,1,1) BETWEEN 'a' AND 'z' AND policy_name NOT GLOB '*[^a-z0-9._-]*'),
    policy_sha256 BLOB NOT NULL CHECK(length(policy_sha256)=32),
    verified_commit TEXT NOT NULL CHECK(length(verified_commit)=40 AND verified_commit NOT GLOB '*[^0-9a-f]*'),
    working_directory TEXT NOT NULL CHECK(length(CAST(working_directory AS BLOB))<=4096 AND working_directory NOT GLOB '/*' AND
      working_directory<>'..' AND substr(working_directory,1,3)<>'../' AND instr(working_directory,char(0))=0),
    timeout_millis INTEGER NOT NULL CHECK(timeout_millis BETWEEN 1 AND 3600000),
    output_limit_bytes INTEGER NOT NULL CHECK(output_limit_bytes BETWEEN 1 AND 1048576),
    runner_name TEXT NOT NULL CHECK(length(CAST(runner_name AS BLOB)) BETWEEN 1 AND 128),
    runner_version TEXT NOT NULL CHECK(length(CAST(runner_version AS BLOB)) BETWEEN 1 AND 128),
    image_digest TEXT NOT NULL CHECK(length(CAST(image_digest AS BLOB)) BETWEEN 1 AND 256),
    environment_sha256 BLOB NOT NULL CHECK(length(environment_sha256)=32),
    effect_attempt INTEGER NOT NULL CHECK(effect_attempt IN (0,1)),
    started_at INTEGER,
    ended_at INTEGER,
    outcome TEXT CHECK(outcome IS NULL OR outcome IN ('passed','exit_nonzero','signaled','timeout','canceled','start_failed','integrity_failure','runner_failure')),
    exit_code INTEGER CHECK(exit_code IS NULL OR exit_code BETWEEN 0 AND 255),
    signal TEXT CHECK(signal IS NULL OR length(CAST(signal AS BLOB)) BETWEEN 1 AND 64),
    stdout_byte_count INTEGER CHECK(stdout_byte_count IS NULL OR stdout_byte_count>=0),
    stdout_retained_bytes INTEGER CHECK(stdout_retained_bytes IS NULL OR stdout_retained_bytes>=0),
    stdout_sha256 BLOB CHECK(stdout_sha256 IS NULL OR length(stdout_sha256)=32),
    stdout_truncated INTEGER CHECK(stdout_truncated IS NULL OR stdout_truncated IN (0,1)),
    stderr_byte_count INTEGER CHECK(stderr_byte_count IS NULL OR stderr_byte_count>=0),
    stderr_retained_bytes INTEGER CHECK(stderr_retained_bytes IS NULL OR stderr_retained_bytes>=0),
    stderr_sha256 BLOB CHECK(stderr_sha256 IS NULL OR length(stderr_sha256)=32),
    stderr_truncated INTEGER CHECK(stderr_truncated IS NULL OR stderr_truncated IN (0,1)),
    reason TEXT CHECK(reason IS NULL OR length(CAST(reason AS BLOB)) BETWEEN 1 AND 1000),
    latest_event_id TEXT NOT NULL REFERENCES journal_events(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    revision INTEGER NOT NULL CHECK(revision>=1),
    created_at INTEGER NOT NULL CHECK(created_at>=0),
    updated_at INTEGER NOT NULL CHECK(updated_at>=created_at),
    FOREIGN KEY(task_id, workspace_id) REFERENCES tasks(id, workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(attempt_id, task_id, workspace_id) REFERENCES attempts(id, task_id, workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(result_id, task_id, attempt_id) REFERENCES results(id, task_id, attempt_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    UNIQUE(id,result_id),
    CHECK(
      (state='prepared' AND effect_attempt=0 AND started_at IS NULL AND ended_at IS NULL AND outcome IS NULL AND reason IS NULL) OR
      (state='running' AND effect_attempt=1 AND started_at IS NOT NULL AND ended_at IS NULL AND outcome IS NULL AND reason IS NULL) OR
      (state='succeeded' AND effect_attempt=1 AND started_at IS NOT NULL AND ended_at IS NOT NULL AND outcome='passed' AND exit_code=0 AND signal IS NULL AND reason IS NULL) OR
      (state='failed' AND effect_attempt=1 AND started_at IS NOT NULL AND ended_at IS NOT NULL AND outcome IS NOT NULL AND outcome<>'passed' AND reason IS NOT NULL) OR
      (state='recovery_required' AND reason IS NOT NULL AND ((effect_attempt=0 AND started_at IS NULL AND ended_at IS NULL AND outcome IS NULL) OR
       (effect_attempt=1 AND started_at IS NOT NULL AND ended_at IS NOT NULL AND outcome IS NOT NULL AND outcome<>'passed')))
    ),
    CHECK((ended_at IS NULL OR ended_at>=started_at) AND (started_at IS NULL OR started_at>=created_at)),
    CHECK(outcome IS NULL OR
      (outcome='passed' AND exit_code=0 AND signal IS NULL) OR
      (outcome='exit_nonzero' AND exit_code BETWEEN 1 AND 255 AND signal IS NULL) OR
      (outcome='signaled' AND signal IS NOT NULL) OR
      outcome IN ('timeout','canceled','start_failed','integrity_failure','runner_failure')),
    CHECK((outcome IS NULL)=(stdout_byte_count IS NULL)),
    CHECK((outcome IS NULL)=(stdout_retained_bytes IS NULL)),
    CHECK((outcome IS NULL)=(stdout_sha256 IS NULL)),
    CHECK((outcome IS NULL)=(stdout_truncated IS NULL)),
    CHECK((outcome IS NULL)=(stderr_byte_count IS NULL)),
    CHECK((outcome IS NULL)=(stderr_retained_bytes IS NULL)),
    CHECK((outcome IS NULL)=(stderr_sha256 IS NULL)),
    CHECK((outcome IS NULL)=(stderr_truncated IS NULL)),
    CHECK(outcome IS NULL OR (stdout_retained_bytes<=stdout_byte_count AND stdout_retained_bytes<=output_limit_bytes AND
      ((stdout_truncated=0 AND stdout_retained_bytes=stdout_byte_count) OR (stdout_truncated=1 AND stdout_byte_count>stdout_retained_bytes AND stdout_retained_bytes=output_limit_bytes)) AND
      stderr_retained_bytes<=stderr_byte_count AND stderr_retained_bytes<=output_limit_bytes AND
      ((stderr_truncated=0 AND stderr_retained_bytes=stderr_byte_count) OR (stderr_truncated=1 AND stderr_byte_count>stderr_retained_bytes AND stderr_retained_bytes=output_limit_bytes))))
) STRICT;
CREATE UNIQUE INDEX verifications_one_effecting_per_workspace ON verifications(workspace_id) WHERE state='running';
CREATE INDEX verifications_work ON verifications(workspace_id,state,updated_at,id);

CREATE TRIGGER verifications_insert_integrity BEFORE INSERT ON verifications BEGIN
  SELECT CASE WHEN NEW.state<>'prepared' OR NEW.revision<>1 OR NEW.created_at<>NEW.updated_at OR NOT EXISTS (
    SELECT 1 FROM results r JOIN tasks t ON t.id=r.task_id AND t.workspace_id=r.workspace_id
    JOIN attempts a ON a.id=r.attempt_id AND a.task_id=r.task_id AND a.workspace_id=r.workspace_id
    JOIN journal_events e ON e.id=NEW.latest_event_id
    JOIN actor_snapshots ea ON ea.id=e.actor_snapshot_id AND ea.actor_type IN ('system','recovery')
    WHERE r.id=NEW.result_id AND r.task_id=NEW.task_id AND r.attempt_id=NEW.attempt_id AND r.workspace_id=NEW.workspace_id AND
      r.state='sealed' AND r.result_commit=NEW.verified_commit AND t.current_attempt_id=a.id AND t.sealed_result_id=r.id AND
      t.state='completed' AND t.cancel_epoch=0 AND a.state='succeeded' AND a.sealed_result_id=r.id AND
      e.entity_type='verification' AND e.entity_id=NEW.id AND e.type='verification.prepared' AND e.from_state IS NULL AND
      e.to_state='prepared' AND e.entity_revision=1 AND e.workspace_id=NEW.workspace_id AND e.task_id=NEW.task_id AND
      e.attempt_id=NEW.attempt_id AND e.result_id=NEW.result_id AND e.occurred_at=NEW.created_at AND
      json_extract(e.payload,'$.detail.expectedTaskRevision')=t.revision AND
      json_extract(e.payload,'$.detail.expectedAttemptRevision')=a.revision
  ) THEN RAISE(ABORT, 'verification preparation has no exact current proof') END;
END;
CREATE TRIGGER verifications_update_integrity BEFORE UPDATE ON verifications BEGIN
  SELECT CASE WHEN OLD.state IN ('succeeded','failed','recovery_required') THEN RAISE(ABORT, 'terminal verification is immutable') END;
  SELECT CASE WHEN NEW.id<>OLD.id OR NEW.result_id<>OLD.result_id OR NEW.task_id<>OLD.task_id OR NEW.attempt_id<>OLD.attempt_id OR
    NEW.workspace_id<>OLD.workspace_id OR NEW.policy_name<>OLD.policy_name OR NEW.policy_sha256<>OLD.policy_sha256 OR
    NEW.verified_commit<>OLD.verified_commit OR NEW.working_directory<>OLD.working_directory OR NEW.timeout_millis<>OLD.timeout_millis OR
    NEW.output_limit_bytes<>OLD.output_limit_bytes OR NEW.runner_name<>OLD.runner_name OR NEW.runner_version<>OLD.runner_version OR
    NEW.image_digest<>OLD.image_digest OR NEW.environment_sha256<>OLD.environment_sha256 OR NEW.created_at<>OLD.created_at
    THEN RAISE(ABORT, 'verification tuple is immutable') END;
  SELECT CASE WHEN NEW.revision<>OLD.revision+1 OR NEW.updated_at<OLD.updated_at OR NOT (
    (OLD.state='prepared' AND NEW.state IN ('running','recovery_required')) OR
    (OLD.state='running' AND NEW.state IN ('succeeded','failed','recovery_required'))
  ) THEN RAISE(ABORT, 'invalid verification transition') END;
  SELECT CASE WHEN NOT EXISTS (
    SELECT 1 FROM results r JOIN tasks t ON t.id=r.task_id AND t.workspace_id=r.workspace_id
    JOIN attempts a ON a.id=r.attempt_id AND a.task_id=r.task_id AND a.workspace_id=r.workspace_id
    JOIN journal_events e ON e.id=NEW.latest_event_id
    JOIN actor_snapshots ea ON ea.id=e.actor_snapshot_id AND ea.actor_type IN ('system','recovery')
    WHERE r.id=NEW.result_id AND r.state='sealed' AND r.result_commit=NEW.verified_commit AND t.id=NEW.task_id AND
      t.current_attempt_id=NEW.attempt_id AND t.sealed_result_id=r.id AND t.state='completed' AND t.cancel_epoch=0 AND
      a.id=NEW.attempt_id AND a.state='succeeded' AND a.sealed_result_id=r.id AND
      e.entity_type='verification' AND e.entity_id=NEW.id AND e.from_state=OLD.state AND e.to_state=NEW.state AND
      e.entity_revision=NEW.revision AND e.workspace_id=NEW.workspace_id AND e.task_id=NEW.task_id AND
      e.attempt_id=NEW.attempt_id AND e.result_id=NEW.result_id AND e.occurred_at=NEW.updated_at AND
      json_extract(e.payload,'$.detail.expectedRevision')=OLD.revision AND
      json_extract(e.payload,'$.detail.expectedTaskRevision')=t.revision AND
      json_extract(e.payload,'$.detail.expectedAttemptRevision')=a.revision
  ) THEN RAISE(ABORT, 'verification transition has no exact event or ownership') END;
  SELECT CASE WHEN OLD.effect_attempt=1 AND NEW.effect_attempt<>1 THEN RAISE(ABORT, 'verification effect attempt regressed') END;
  SELECT CASE WHEN OLD.state='prepared' AND NEW.state='running' AND EXISTS (
    SELECT 1 FROM publications p WHERE p.workspace_id=OLD.workspace_id AND p.state='running' AND p.effect_phase IN ('push_started','pr_create_started')
  ) THEN RAISE(ABORT, 'workspace already has an effecting publication') END;
END;
CREATE TRIGGER verifications_delete_guard BEFORE DELETE ON verifications
BEGIN SELECT RAISE(ABORT, 'verifications are durable'); END;

CREATE TABLE publications (
    id TEXT PRIMARY KEY CHECK(
        length(id)=40 AND substr(id,1,4)='pub_' AND substr(id,13,1)='-' AND
        substr(id,18,1)='-' AND substr(id,19,1)='7' AND substr(id,23,1)='-' AND
        substr(id,24,1) IN ('8','9','a','b') AND substr(id,28,1)='-' AND
        length(replace(substr(id,5),'-',''))=32 AND replace(substr(id,5),'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    operation_id TEXT NOT NULL UNIQUE CHECK(
        length(operation_id)=39 AND substr(operation_id,1,3)='op_' AND
        substr(operation_id,12,1)='-' AND substr(operation_id,17,1)='-' AND substr(operation_id,18,1)='7' AND
        substr(operation_id,22,1)='-' AND substr(operation_id,23,1) IN ('8','9','a','b') AND substr(operation_id,27,1)='-' AND
        length(replace(substr(operation_id,4),'-',''))=32 AND replace(substr(operation_id,4),'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    result_id TEXT NOT NULL,
    verification_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('prepared','running','uncertain','recovery_required','published','failed','conflict')),
    effect_phase TEXT NOT NULL CHECK(effect_phase IN ('none','push_started','push_observed','pr_create_started')),
    installation_id INTEGER NOT NULL CHECK(installation_id>0),
    repository_id INTEGER NOT NULL CHECK(repository_id>0),
    repository_full_name TEXT NOT NULL CHECK(length(CAST(repository_full_name AS BLOB)) BETWEEN 3 AND 201),
    base_ref TEXT NOT NULL CHECK(length(CAST(base_ref AS BLOB)) BETWEEN 1 AND 255),
    base_sha TEXT NOT NULL CHECK(length(base_sha)=40 AND base_sha NOT GLOB '*[^0-9a-f]*'),
    result_commit TEXT NOT NULL CHECK(length(result_commit)=40 AND result_commit NOT GLOB '*[^0-9a-f]*'),
    branch TEXT NOT NULL UNIQUE CHECK(length(CAST(branch AS BLOB)) BETWEEN 1 AND 255),
    expected_remote_old_sha TEXT CHECK(expected_remote_old_sha IS NULL OR (length(expected_remote_old_sha)=40 AND expected_remote_old_sha NOT GLOB '*[^0-9a-f]*')),
    broker_policy_version TEXT NOT NULL CHECK(length(CAST(broker_policy_version AS BLOB)) BETWEEN 1 AND 128),
    broker_policy_sha256 BLOB NOT NULL CHECK(length(broker_policy_sha256)=32),
    observed_remote_sha TEXT CHECK(observed_remote_sha IS NULL OR (length(observed_remote_sha)=40 AND observed_remote_sha NOT GLOB '*[^0-9a-f]*')),
    pr_number INTEGER CHECK(pr_number IS NULL OR pr_number>0),
    pr_url TEXT CHECK(pr_url IS NULL OR length(CAST(pr_url AS BLOB)) BETWEEN 1 AND 2048),
    pr_state TEXT CHECK(pr_state IS NULL OR pr_state='open'),
    pr_draft INTEGER CHECK(pr_draft IS NULL OR pr_draft IN (0,1)),
    pr_repository_id INTEGER,
    pr_repository_full_name TEXT,
    pr_base_repository_id INTEGER,
    pr_base_repository_full_name TEXT,
    pr_base_ref TEXT,
    pr_base_sha TEXT,
    pr_head_repository_id INTEGER,
    pr_head_repository_full_name TEXT,
    pr_head_repository_owner TEXT,
    pr_head_repository_name TEXT,
    pr_head_ref TEXT,
    pr_head_sha TEXT,
    reason TEXT CHECK(reason IS NULL OR length(CAST(reason AS BLOB)) BETWEEN 1 AND 1000),
    latest_event_id TEXT NOT NULL REFERENCES journal_events(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    requester_actor_snapshot_id INTEGER NOT NULL REFERENCES actor_snapshots(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    revision INTEGER NOT NULL CHECK(revision>=1),
    created_at INTEGER NOT NULL CHECK(created_at>=0),
    updated_at INTEGER NOT NULL CHECK(updated_at>=created_at),
    FOREIGN KEY(task_id, workspace_id) REFERENCES tasks(id, workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(attempt_id, task_id, workspace_id) REFERENCES attempts(id, task_id, workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(result_id, task_id, attempt_id) REFERENCES results(id, task_id, attempt_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(verification_id,result_id) REFERENCES verifications(id,result_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    CHECK(branch<>base_ref),
    CHECK((effect_phase='none' AND observed_remote_sha IS NULL) OR effect_phase<>'none'),
    CHECK((state IN ('uncertain','recovery_required','failed','conflict'))=(reason IS NOT NULL)),
    CHECK(state<>'prepared' OR effect_phase='none'),
    CHECK(effect_phase<>'none' OR state IN ('prepared','recovery_required','failed','conflict')),
    CHECK((state='published')=(pr_number IS NOT NULL)),
    CHECK((pr_number IS NULL AND pr_url IS NULL AND pr_state IS NULL AND pr_draft IS NULL AND pr_repository_id IS NULL AND
      pr_repository_full_name IS NULL AND pr_base_repository_id IS NULL AND pr_base_repository_full_name IS NULL AND
      pr_base_ref IS NULL AND pr_base_sha IS NULL AND pr_head_repository_id IS NULL AND pr_head_repository_full_name IS NULL AND
      pr_head_repository_owner IS NULL AND pr_head_repository_name IS NULL AND pr_head_ref IS NULL AND pr_head_sha IS NULL) OR
      (pr_number IS NOT NULL AND pr_url IS NOT NULL AND pr_state IS NOT NULL AND pr_draft IS NOT NULL AND pr_repository_id IS NOT NULL AND
      pr_repository_full_name IS NOT NULL AND pr_base_repository_id IS NOT NULL AND pr_base_repository_full_name IS NOT NULL AND
      pr_base_ref IS NOT NULL AND pr_base_sha IS NOT NULL AND pr_head_repository_id IS NOT NULL AND pr_head_repository_full_name IS NOT NULL AND
      pr_head_repository_owner IS NOT NULL AND pr_head_repository_name IS NOT NULL AND pr_head_ref IS NOT NULL AND pr_head_sha IS NOT NULL))
) STRICT;
CREATE UNIQUE INDEX publications_one_per_result ON publications(result_id);
CREATE UNIQUE INDEX publications_one_effecting_per_workspace ON publications(workspace_id) WHERE state='running' AND effect_phase IN ('push_started','pr_create_started');
CREATE INDEX publications_work ON publications(workspace_id,state,effect_phase,updated_at,id);

CREATE TRIGGER publications_insert_integrity BEFORE INSERT ON publications BEGIN
  SELECT CASE WHEN NEW.state<>'prepared' OR NEW.effect_phase<>'none' OR NEW.revision<>1 OR NEW.created_at<>NEW.updated_at OR NOT EXISTS (
    SELECT 1 FROM results r JOIN tasks t ON t.id=r.task_id AND t.workspace_id=r.workspace_id
    JOIN attempts a ON a.id=r.attempt_id AND a.task_id=r.task_id AND a.workspace_id=r.workspace_id
    JOIN workspaces w ON w.id=r.workspace_id AND w.repository_id=r.repository_id
    JOIN verifications v ON v.id=NEW.verification_id AND v.result_id=r.id
    JOIN journal_events e ON e.id=NEW.latest_event_id
    WHERE r.id=NEW.result_id AND r.task_id=NEW.task_id AND r.attempt_id=NEW.attempt_id AND r.workspace_id=NEW.workspace_id AND
      r.state='sealed' AND r.repository_id=NEW.repository_id AND r.base_sha=NEW.base_sha AND r.result_commit=NEW.result_commit AND
      t.current_attempt_id=a.id AND t.sealed_result_id=r.id AND t.state='completed' AND t.cancel_epoch=0 AND
      a.state='succeeded' AND a.sealed_result_id=r.id AND v.state='succeeded' AND v.verified_commit=r.result_commit AND
      w.state='active' AND w.installation_id=NEW.installation_id AND w.repository_full_name=NEW.repository_full_name AND
      NEW.branch='fern/'||w.name||'/'||NEW.operation_id AND
      e.entity_type='publication' AND e.entity_id=NEW.id AND e.type='publication.prepared' AND e.from_state IS NULL AND
      e.to_state='prepared' AND e.entity_revision=1 AND e.workspace_id=NEW.workspace_id AND e.task_id=NEW.task_id AND
      e.attempt_id=NEW.attempt_id AND e.result_id=NEW.result_id AND e.occurred_at=NEW.created_at AND
      e.actor_snapshot_id=NEW.requester_actor_snapshot_id AND
      json_extract(e.payload,'$.detail.expectedTaskRevision')=t.revision AND
      json_extract(e.payload,'$.detail.expectedAttemptRevision')=a.revision
  ) THEN RAISE(ABORT, 'publication preparation has no exact current proof') END;
END;
CREATE TRIGGER publications_update_integrity BEFORE UPDATE ON publications BEGIN
  SELECT CASE WHEN OLD.state IN ('published','recovery_required','failed','conflict') THEN RAISE(ABORT, 'terminal publication is immutable') END;
  SELECT CASE WHEN NEW.id<>OLD.id OR NEW.operation_id<>OLD.operation_id OR NEW.result_id<>OLD.result_id OR
    NEW.verification_id<>OLD.verification_id OR NEW.task_id<>OLD.task_id OR NEW.attempt_id<>OLD.attempt_id OR
    NEW.workspace_id<>OLD.workspace_id OR NEW.installation_id<>OLD.installation_id OR NEW.repository_id<>OLD.repository_id OR
    NEW.repository_full_name<>OLD.repository_full_name OR NEW.base_ref<>OLD.base_ref OR NEW.base_sha<>OLD.base_sha OR
    NEW.result_commit<>OLD.result_commit OR NEW.branch<>OLD.branch OR NEW.expected_remote_old_sha IS NOT OLD.expected_remote_old_sha OR
    NEW.broker_policy_version<>OLD.broker_policy_version OR NEW.broker_policy_sha256<>OLD.broker_policy_sha256 OR
    NEW.requester_actor_snapshot_id<>OLD.requester_actor_snapshot_id OR NEW.created_at<>OLD.created_at
    THEN RAISE(ABORT, 'publication tuple is immutable') END;
  SELECT CASE WHEN NEW.revision<>OLD.revision+1 OR NEW.updated_at<OLD.updated_at OR NOT EXISTS (
    SELECT 1 FROM results r JOIN tasks t ON t.id=r.task_id AND t.workspace_id=r.workspace_id
    JOIN attempts a ON a.id=r.attempt_id AND a.task_id=r.task_id AND a.workspace_id=r.workspace_id
    JOIN verifications v ON v.id=OLD.verification_id AND v.result_id=r.id
    JOIN journal_events e ON e.id=NEW.latest_event_id
    JOIN actor_snapshots ea ON ea.id=e.actor_snapshot_id AND ea.actor_type IN ('system','recovery')
    WHERE r.id=OLD.result_id AND r.state='sealed' AND r.result_commit=OLD.result_commit AND t.id=OLD.task_id AND
      t.current_attempt_id=OLD.attempt_id AND t.sealed_result_id=r.id AND t.state='completed' AND t.cancel_epoch=0 AND
      a.id=OLD.attempt_id AND a.state='succeeded' AND a.sealed_result_id=r.id AND v.state='succeeded' AND v.verified_commit=r.result_commit AND
      e.entity_type='publication' AND e.entity_id=OLD.id AND e.from_state=OLD.state AND e.to_state=NEW.state AND
      e.entity_revision=NEW.revision AND e.workspace_id=OLD.workspace_id AND e.task_id=OLD.task_id AND
      e.attempt_id=OLD.attempt_id AND e.result_id=OLD.result_id AND e.occurred_at=NEW.updated_at AND
      json_extract(e.payload,'$.detail.expectedRevision')=OLD.revision AND
      json_extract(e.payload,'$.detail.expectedTaskRevision')=t.revision AND
      json_extract(e.payload,'$.detail.expectedAttemptRevision')=a.revision
  ) THEN RAISE(ABORT, 'publication transition has no exact event or ownership') END;
  SELECT CASE WHEN NOT (
    (NEW.effect_phase=OLD.effect_phase AND NEW.state IN ('uncertain','recovery_required','failed','conflict','published')) OR
    (OLD.effect_phase='none' AND NEW.effect_phase='push_started' AND NEW.state='running') OR
    (OLD.effect_phase='push_started' AND NEW.effect_phase='push_observed' AND NEW.state='running' AND NEW.observed_remote_sha=OLD.result_commit) OR
    (OLD.effect_phase='push_observed' AND NEW.effect_phase='pr_create_started' AND NEW.state='running')
  ) THEN RAISE(ABORT, 'publication effect phase regressed or skipped') END;
  SELECT CASE WHEN OLD.effect_phase='none' AND NEW.effect_phase='push_started' AND EXISTS (
    SELECT 1 FROM verifications v WHERE v.workspace_id=OLD.workspace_id AND v.state='running'
  ) THEN RAISE(ABORT, 'workspace already has an effecting verification') END;
  SELECT CASE WHEN NEW.state='published' AND NOT (
    OLD.effect_phase IN ('push_observed','pr_create_started') AND NEW.observed_remote_sha=OLD.result_commit AND
    NEW.pr_repository_id=OLD.repository_id AND NEW.pr_repository_full_name=OLD.repository_full_name AND NEW.pr_state='open' AND NEW.pr_draft=1 AND
    NEW.pr_base_repository_id=OLD.repository_id AND NEW.pr_base_repository_full_name=OLD.repository_full_name AND
    NEW.pr_base_ref=OLD.base_ref AND NEW.pr_base_sha=OLD.base_sha AND NEW.pr_head_repository_id=OLD.repository_id AND
    NEW.pr_head_repository_full_name=OLD.repository_full_name AND NEW.pr_head_repository_owner||'/'||NEW.pr_head_repository_name=OLD.repository_full_name AND
    NEW.pr_head_ref=OLD.branch AND NEW.pr_head_sha=OLD.result_commit AND
    NEW.pr_url='https://github.com/'||OLD.repository_full_name||'/pull/'||CAST(NEW.pr_number AS TEXT)
  ) THEN RAISE(ABORT, 'publication completion observation differs') END;
END;
CREATE TRIGGER publications_delete_guard BEFORE DELETE ON publications
BEGIN SELECT RAISE(ABORT, 'publications are durable'); END;
`

// userAuthorizedSealSchema (migration 4) recreates triggers first created by
// earlier migrations. Deltas versus those v3 versions, trigger by trigger:
//
//   - results_insert_integrity: same manifest/proof shape as v3 plus the
//     completion-authority branch. The shared WHERE now also demands neither
//     task nor attempt already sealed (v3 expressed that via t.state='running'
//     /a.state='succeeded', which moved into the branches), requires event
//     $.completionAuthority to equal NEW.completion_authority, keeps v3's
//     running/succeeded states for 'execution_success' (which must carry no
//     seal request or authorizer), and adds a 'user_seal' branch accepting
//     task running/input_required with attempt admitted/running/input_required
//     only when a claimed seal_requests row matches every expected field.
//   - attempts_result_seal_integrity: v3 required succeeded→succeeded; v4
//     branches on r.completion_authority — 'execution_success' keeps
//     succeeded→succeeded, 'user_seal' admits admitted/running/input_required
//     superseding to 'superseded'.
//   - tasks_result_seal_integrity: v3 required OLD.state='running'; v4 moves
//     that into the authority branch ('execution_success': running + attempt
//     succeeded; 'user_seal': running/input_required + attempt superseded).
//   - verifications_insert_integrity: only delta vs v3 is the widened attempt
//     guard: a.state='succeeded' OR (a.state='superseded' AND
//     r.completion_authority='user_seal').
//   - verifications_update_integrity: identical widening inside the transition
//     proof EXISTS clause; everything else matches v3 verbatim.
//   - publications_insert_integrity: same single widening as the verification
//     insert trigger.
//   - publications_update_integrity: same single widening inside the
//     transition proof EXISTS clause; effect-phase and completion-observation
//     checks match v3 verbatim.
const userAuthorizedSealSchema = `
CREATE TABLE seal_requests (
    id TEXT PRIMARY KEY CHECK(
        length(id)=40 AND substr(id,1,4)='slr_' AND substr(id,13,1)='-' AND
        substr(id,18,1)='-' AND substr(id,19,1)='7' AND substr(id,23,1)='-' AND
        substr(id,24,1) IN ('8','9','a','b') AND substr(id,28,1)='-' AND
        length(replace(substr(id,5),'-',''))=32 AND replace(substr(id,5),'-','') NOT GLOB '*[^0-9a-f]*'
    ),
    receipt_id TEXT NOT NULL UNIQUE REFERENCES receipts(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    workspace_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('pending','claimed','completed','rejected')),
    completion_authority TEXT NOT NULL CHECK(completion_authority='user_seal'),
    expected_workspace_revision INTEGER NOT NULL CHECK(expected_workspace_revision>=1),
    expected_task_revision INTEGER NOT NULL CHECK(expected_task_revision>=1),
    expected_attempt_revision INTEGER NOT NULL CHECK(expected_attempt_revision>=1),
    repository_id INTEGER NOT NULL CHECK(repository_id>0),
    base_sha TEXT NOT NULL CHECK(length(base_sha)=40 AND base_sha NOT GLOB '*[^0-9a-f]*'),
    expected_result_commit TEXT NOT NULL CHECK(length(expected_result_commit)=40 AND expected_result_commit NOT GLOB '*[^0-9a-f]*'),
    expected_tree_oid TEXT NOT NULL CHECK(length(expected_tree_oid)=40 AND expected_tree_oid NOT GLOB '*[^0-9a-f]*'),
    expected_outcome TEXT NOT NULL CHECK(expected_outcome IN ('changed','no_changes')),
    expected_manifest_entries INTEGER NOT NULL CHECK(expected_manifest_entries>=0),
    expected_manifest_sha256 BLOB NOT NULL CHECK(length(expected_manifest_sha256)=32),
    expected_worktree_clean INTEGER NOT NULL CHECK(expected_worktree_clean=1),
    idempotency_key TEXT NOT NULL CHECK(length(CAST(idempotency_key AS BLOB)) BETWEEN 1 AND 128),
    request_hash BLOB NOT NULL CHECK(length(request_hash)=32),
    authorizer_actor_snapshot_id INTEGER NOT NULL REFERENCES actor_snapshots(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    result_id TEXT NOT NULL UNIQUE CHECK(
      length(result_id)=40 AND substr(result_id,1,4)='res_' AND substr(result_id,13,1)='-' AND substr(result_id,18,1)='-' AND
      substr(result_id,19,1)='7' AND substr(result_id,23,1)='-' AND substr(result_id,24,1) IN ('8','9','a','b') AND
      substr(result_id,28,1)='-' AND length(replace(substr(result_id,5),'-',''))=32 AND replace(substr(result_id,5),'-','') NOT GLOB '*[^0-9a-f]*'),
    result_event_id TEXT NOT NULL UNIQUE CHECK(
      length(result_event_id)=40 AND substr(result_event_id,1,4)='fev_' AND substr(result_event_id,13,1)='-' AND substr(result_event_id,18,1)='-' AND
      substr(result_event_id,19,1)='7' AND substr(result_event_id,23,1)='-' AND substr(result_event_id,24,1) IN ('8','9','a','b') AND
      substr(result_event_id,28,1)='-' AND length(replace(substr(result_event_id,5),'-',''))=32 AND replace(substr(result_event_id,5),'-','') NOT GLOB '*[^0-9a-f]*'),
    task_event_id TEXT NOT NULL UNIQUE CHECK(
      length(task_event_id)=40 AND substr(task_event_id,1,4)='fev_' AND substr(task_event_id,13,1)='-' AND substr(task_event_id,18,1)='-' AND
      substr(task_event_id,19,1)='7' AND substr(task_event_id,23,1)='-' AND substr(task_event_id,24,1) IN ('8','9','a','b') AND
      substr(task_event_id,28,1)='-' AND length(replace(substr(task_event_id,5),'-',''))=32 AND
      replace(substr(task_event_id,5),'-','') NOT GLOB '*[^0-9a-f]*' AND task_event_id<>result_event_id),
    claim_owner TEXT CHECK(claim_owner IS NULL OR length(CAST(claim_owner AS BLOB)) BETWEEN 1 AND 64),
    claim_expires_at INTEGER,
    claim_revision INTEGER NOT NULL DEFAULT 0 CHECK(claim_revision>=0),
    accepted_at INTEGER NOT NULL CHECK(accepted_at>=0),
    completed_at INTEGER,
    rejected_at INTEGER,
    rejected_reason TEXT CHECK(rejected_reason IS NULL OR length(CAST(rejected_reason AS BLOB)) BETWEEN 1 AND 1000),
    FOREIGN KEY(workspace_id,repository_id) REFERENCES workspaces(id,repository_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(task_id,workspace_id) REFERENCES tasks(id,workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY(attempt_id,task_id,workspace_id) REFERENCES attempts(id,task_id,workspace_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
    CHECK((expected_outcome='no_changes' AND expected_result_commit=base_sha AND expected_manifest_entries=0) OR
          (expected_outcome='changed' AND expected_result_commit<>base_sha AND expected_manifest_entries>0)),
    CHECK(
      (state='pending' AND claim_owner IS NULL AND claim_expires_at IS NULL AND claim_revision=0 AND completed_at IS NULL AND rejected_at IS NULL AND rejected_reason IS NULL) OR
      (state='claimed' AND claim_owner IS NOT NULL AND claim_expires_at IS NOT NULL AND claim_revision>=1 AND completed_at IS NULL AND rejected_at IS NULL AND rejected_reason IS NULL) OR
      (state='completed' AND claim_owner IS NULL AND claim_expires_at IS NULL AND claim_revision>=1 AND completed_at IS NOT NULL AND rejected_at IS NULL AND rejected_reason IS NULL) OR
      (state='rejected' AND claim_owner IS NULL AND claim_expires_at IS NULL AND claim_revision>=1 AND completed_at IS NULL AND rejected_at IS NOT NULL AND rejected_reason IS NOT NULL)
    ),
    CHECK(claim_expires_at IS NULL OR claim_expires_at>accepted_at),
    CHECK(completed_at IS NULL OR completed_at>=accepted_at),
    CHECK(rejected_at IS NULL OR rejected_at>=accepted_at)
) STRICT;
CREATE UNIQUE INDEX seal_requests_one_open_per_task ON seal_requests(task_id) WHERE state IN ('pending','claimed');
CREATE INDEX seal_requests_work ON seal_requests(workspace_id,state,claim_expires_at,accepted_at,id);

ALTER TABLE results ADD COLUMN completion_authority TEXT NOT NULL DEFAULT 'execution_success'
  CHECK(completion_authority IN ('execution_success','user_seal'));
ALTER TABLE results ADD COLUMN seal_request_id TEXT REFERENCES seal_requests(id) ON UPDATE RESTRICT ON DELETE RESTRICT;
ALTER TABLE results ADD COLUMN authorizer_actor_snapshot_id INTEGER REFERENCES actor_snapshots(id) ON UPDATE RESTRICT ON DELETE RESTRICT;

CREATE TRIGGER seal_requests_insert_integrity BEFORE INSERT ON seal_requests BEGIN
  SELECT CASE WHEN NOT EXISTS (
    SELECT 1 FROM receipts r JOIN actor_snapshots a ON a.id=NEW.authorizer_actor_snapshot_id
    JOIN workspaces w ON w.id=NEW.workspace_id AND w.repository_id=NEW.repository_id
    JOIN tasks t ON t.id=NEW.task_id AND t.workspace_id=NEW.workspace_id
    JOIN attempts x ON x.id=NEW.attempt_id AND x.task_id=t.id AND x.workspace_id=t.workspace_id
    WHERE r.id=NEW.receipt_id AND r.workspace_id=NEW.workspace_id AND r.command_kind='task.seal' AND
      r.target_type='task' AND r.target_id=NEW.task_id AND r.idempotency_key=NEW.idempotency_key AND
      r.request_hash=NEW.request_hash AND r.actor_snapshot_id=NEW.authorizer_actor_snapshot_id AND
      r.accepted_at=NEW.accepted_at AND w.state='active' AND w.revision=NEW.expected_workspace_revision AND
      t.current_attempt_id=x.id AND t.cancel_epoch=0 AND t.sealed_result_id IS NULL AND
      t.state IN ('running','input_required') AND t.revision=NEW.expected_task_revision AND
      t.base_sha=NEW.base_sha AND x.base_sha=NEW.base_sha AND x.sealed_result_id IS NULL AND
      x.state IN ('admitted','running','input_required') AND x.revision=NEW.expected_attempt_revision
  ) THEN RAISE(ABORT, 'seal request has no exact current authorization') END;
END;
CREATE TRIGGER seal_requests_immutable_tuple BEFORE UPDATE ON seal_requests WHEN
  NEW.id<>OLD.id OR NEW.receipt_id<>OLD.receipt_id OR NEW.workspace_id<>OLD.workspace_id OR NEW.task_id<>OLD.task_id OR
  NEW.attempt_id<>OLD.attempt_id OR NEW.completion_authority<>OLD.completion_authority OR
  NEW.expected_workspace_revision<>OLD.expected_workspace_revision OR NEW.expected_task_revision<>OLD.expected_task_revision OR
  NEW.expected_attempt_revision<>OLD.expected_attempt_revision OR NEW.repository_id<>OLD.repository_id OR
  NEW.base_sha<>OLD.base_sha OR NEW.expected_result_commit<>OLD.expected_result_commit OR NEW.expected_tree_oid<>OLD.expected_tree_oid OR
  NEW.expected_outcome<>OLD.expected_outcome OR NEW.expected_manifest_entries<>OLD.expected_manifest_entries OR
  NEW.expected_manifest_sha256<>OLD.expected_manifest_sha256 OR NEW.expected_worktree_clean<>OLD.expected_worktree_clean OR
  NEW.idempotency_key<>OLD.idempotency_key OR
  NEW.request_hash<>OLD.request_hash OR NEW.authorizer_actor_snapshot_id<>OLD.authorizer_actor_snapshot_id OR
  NEW.result_id<>OLD.result_id OR NEW.result_event_id<>OLD.result_event_id OR NEW.task_event_id<>OLD.task_event_id OR
  NEW.accepted_at<>OLD.accepted_at
BEGIN SELECT RAISE(ABORT, 'seal request authorization is immutable'); END;
CREATE TRIGGER seal_requests_transition_integrity BEFORE UPDATE ON seal_requests BEGIN
  SELECT CASE WHEN OLD.state IN ('completed','rejected') THEN RAISE(ABORT, 'terminal seal request is immutable') END;
  SELECT CASE WHEN NOT (
    (OLD.state='pending' AND NEW.state='claimed' AND NEW.claim_revision=1 AND NEW.claim_owner IS NOT NULL AND NEW.claim_expires_at IS NOT NULL) OR
    (OLD.state='claimed' AND NEW.state='claimed' AND NEW.claim_revision=OLD.claim_revision+1 AND
      NEW.claim_owner IS NOT NULL AND NEW.claim_expires_at IS NOT NULL AND NEW.claim_expires_at>OLD.claim_expires_at) OR
    (OLD.state='claimed' AND NEW.state='rejected' AND NEW.claim_revision=OLD.claim_revision AND NEW.rejected_at IS NOT NULL AND NEW.rejected_reason IS NOT NULL) OR
    (OLD.state='claimed' AND NEW.state='completed' AND NEW.claim_revision=OLD.claim_revision AND NEW.completed_at IS NOT NULL AND EXISTS (
      SELECT 1 FROM results r JOIN tasks t ON t.id=OLD.task_id JOIN attempts a ON a.id=OLD.attempt_id
      WHERE r.id=OLD.result_id AND r.seal_request_id=OLD.id AND r.completion_authority='user_seal' AND
        t.sealed_result_id=r.id AND t.state='completed' AND a.sealed_result_id=r.id AND a.state='superseded'
    ))
  ) THEN RAISE(ABORT, 'invalid seal request transition') END;
END;
CREATE TRIGGER seal_requests_delete_guard BEFORE DELETE ON seal_requests
BEGIN SELECT RAISE(ABORT, 'seal requests are durable'); END;

DROP TRIGGER results_insert_integrity;
DROP TRIGGER attempts_result_seal_integrity;
DROP TRIGGER tasks_result_seal_integrity;

CREATE TRIGGER results_insert_integrity BEFORE INSERT ON results BEGIN
  SELECT CASE WHEN (SELECT count(*) FROM result_manifest m WHERE m.result_id=NEW.id)<>NEW.manifest_entries OR
    (NEW.manifest_entries>0 AND ((SELECT min(ordinal) FROM result_manifest WHERE result_id=NEW.id)<>0 OR
     (SELECT max(ordinal) FROM result_manifest WHERE result_id=NEW.id)<>NEW.manifest_entries-1))
    THEN RAISE(ABORT, 'result manifest is incomplete') END;
  SELECT CASE WHEN NOT EXISTS (
    SELECT 1 FROM tasks t JOIN attempts a ON a.id=t.current_attempt_id AND a.task_id=t.id AND a.workspace_id=t.workspace_id
    JOIN events ae ON ae.id=NEW.sealed_event_id AND ae.workspace_id=t.workspace_id AND ae.task_id=t.id AND ae.attempt_id=a.id AND ae.type='attempt.result_sealed'
    JOIN events te ON te.id=NEW.completed_event_id AND te.workspace_id=t.workspace_id AND te.task_id=t.id AND te.attempt_id IS NULL AND
      te.type='task.completed' AND te.occurred_at=ae.occurred_at AND te.actor_snapshot_id=ae.actor_snapshot_id AND te.payload=ae.payload AND te.cursor>ae.cursor
    JOIN actor_snapshots creator ON creator.id=NEW.creator_actor_snapshot_id AND creator.id=ae.actor_snapshot_id AND creator.actor_type IN ('system','recovery')
    WHERE t.id=NEW.task_id AND t.workspace_id=NEW.workspace_id AND t.repository_id=NEW.repository_id AND t.base_sha=NEW.base_sha AND
      t.cancel_epoch=0 AND t.sealed_result_id IS NULL AND t.revision=json_extract(ae.payload,'$.expectedTaskRevision') AND
      a.id=NEW.attempt_id AND a.sealed_result_id IS NULL AND a.revision=json_extract(ae.payload,'$.expectedAttemptRevision') AND
      a.base_sha=NEW.base_sha AND a.opencode_session_id=NEW.opencode_session_id AND a.opencode_message_id=NEW.opencode_message_id AND
      ae.occurred_at=NEW.sealed_at AND json_extract(ae.payload,'$.resultId')=NEW.id AND json_extract(ae.payload,'$.taskId')=NEW.task_id AND
      json_extract(ae.payload,'$.attemptId')=NEW.attempt_id AND json_extract(ae.payload,'$.repositoryId')=NEW.repository_id AND
      json_extract(ae.payload,'$.baseSha')=NEW.base_sha AND json_extract(ae.payload,'$.resultCommit')=NEW.result_commit AND
      json_extract(ae.payload,'$.treeOid')=NEW.tree_oid AND json_extract(ae.payload,'$.outcome')=NEW.outcome AND
      json_extract(ae.payload,'$.clean')=1 AND json_extract(ae.payload,'$.manifestEntries')=NEW.manifest_entries AND
      json_extract(ae.payload,'$.opencodeSessionId')=NEW.opencode_session_id AND json_extract(ae.payload,'$.opencodeMessageId')=NEW.opencode_message_id AND
      json_extract(ae.payload,'$.collectedAtMillis')=NEW.collected_at AND json_extract(ae.payload,'$.policyVersion')=NEW.policy_version AND
      json_extract(ae.payload,'$.manifestSha256')='sha256:'||lower(hex(NEW.manifest_sha256)) AND
      json_extract(ae.payload,'$.evidenceSha256')='sha256:'||lower(hex(NEW.evidence_sha256)) AND
      json_extract(ae.payload,'$.completionAuthority')=NEW.completion_authority AND (
        (NEW.completion_authority='execution_success' AND NEW.seal_request_id IS NULL AND NEW.authorizer_actor_snapshot_id IS NULL AND
         t.state='running' AND a.state='succeeded') OR
        (NEW.completion_authority='user_seal' AND t.state IN ('running','input_required') AND a.state IN ('admitted','running','input_required') AND EXISTS (
          SELECT 1 FROM seal_requests q WHERE q.id=NEW.seal_request_id AND q.state='claimed' AND q.completion_authority='user_seal' AND
            q.workspace_id=NEW.workspace_id AND q.task_id=NEW.task_id AND q.attempt_id=NEW.attempt_id AND q.result_id=NEW.id AND
            q.result_event_id=NEW.sealed_event_id AND q.task_event_id=NEW.completed_event_id AND q.authorizer_actor_snapshot_id=NEW.authorizer_actor_snapshot_id AND
            q.expected_task_revision=t.revision AND q.expected_attempt_revision=a.revision AND q.repository_id=NEW.repository_id AND q.base_sha=NEW.base_sha AND
            q.expected_result_commit=NEW.result_commit AND q.expected_tree_oid=NEW.tree_oid AND q.expected_outcome=NEW.outcome AND
            q.expected_manifest_entries=NEW.manifest_entries AND q.expected_manifest_sha256=NEW.manifest_sha256 AND
            q.expected_worktree_clean=NEW.worktree_clean AND
            json_extract(ae.payload,'$.sealRequestId')=q.id
        ))
      )
  ) THEN RAISE(ABORT, 'sealed result has no exact current proof') END;
END;

CREATE TRIGGER attempts_result_seal_integrity BEFORE UPDATE OF sealed_result_id ON attempts
WHEN OLD.sealed_result_id IS NULL AND NEW.sealed_result_id IS NOT NULL BEGIN
  SELECT CASE WHEN NEW.revision<>OLD.revision+1 OR NEW.updated_at<OLD.updated_at OR NOT EXISTS (
    SELECT 1 FROM results r JOIN events e ON e.id=r.sealed_event_id WHERE r.id=NEW.sealed_result_id AND
      r.task_id=OLD.task_id AND r.attempt_id=OLD.id AND r.workspace_id=OLD.workspace_id AND r.sealed_at=NEW.updated_at AND
      json_extract(e.payload,'$.expectedAttemptRevision')=OLD.revision AND (
        (r.completion_authority='execution_success' AND OLD.state='succeeded' AND NEW.state='succeeded') OR
        (r.completion_authority='user_seal' AND OLD.state IN ('admitted','running','input_required') AND NEW.state='superseded')
      )
  ) THEN RAISE(ABORT, 'invalid attempt result seal') END;
END;

CREATE TRIGGER tasks_result_seal_integrity BEFORE UPDATE OF state,sealed_result_id ON tasks
WHEN OLD.state<>'completed' AND NEW.state='completed' BEGIN
  SELECT CASE WHEN OLD.cancel_epoch<>0 OR NEW.cancel_epoch<>0 OR OLD.sealed_result_id IS NOT NULL OR NEW.sealed_result_id IS NULL OR
    NEW.terminal_reason IS NOT NULL OR NEW.revision<>OLD.revision+1 OR NEW.updated_at<OLD.updated_at OR NOT EXISTS (
      SELECT 1 FROM results r JOIN attempts a ON a.id=OLD.current_attempt_id AND a.task_id=OLD.id AND a.workspace_id=OLD.workspace_id
      JOIN events ae ON ae.id=r.sealed_event_id JOIN events te ON te.id=r.completed_event_id
      WHERE r.id=NEW.sealed_result_id AND r.task_id=OLD.id AND r.attempt_id=a.id AND r.workspace_id=OLD.workspace_id AND
        a.sealed_result_id=r.id AND r.sealed_at=NEW.updated_at AND te.cursor=NEW.latest_event_cursor AND ae.cursor<te.cursor AND
        json_extract(ae.payload,'$.expectedAttemptRevision')=a.revision-1 AND json_extract(ae.payload,'$.expectedTaskRevision')=OLD.revision AND (
          (r.completion_authority='execution_success' AND OLD.state='running' AND a.state='succeeded') OR
          (r.completion_authority='user_seal' AND OLD.state IN ('running','input_required') AND a.state='superseded')
        )
    ) THEN RAISE(ABORT, 'invalid completed task result seal') END;
END;

DROP TRIGGER verifications_insert_integrity;
DROP TRIGGER verifications_update_integrity;
CREATE TRIGGER verifications_insert_integrity BEFORE INSERT ON verifications BEGIN
  SELECT CASE WHEN NEW.state<>'prepared' OR NEW.revision<>1 OR NEW.created_at<>NEW.updated_at OR NOT EXISTS (
    SELECT 1 FROM results r JOIN tasks t ON t.id=r.task_id AND t.workspace_id=r.workspace_id
    JOIN attempts a ON a.id=r.attempt_id AND a.task_id=r.task_id AND a.workspace_id=r.workspace_id
    JOIN journal_events e ON e.id=NEW.latest_event_id
    JOIN actor_snapshots ea ON ea.id=e.actor_snapshot_id AND ea.actor_type IN ('system','recovery')
    WHERE r.id=NEW.result_id AND r.task_id=NEW.task_id AND r.attempt_id=NEW.attempt_id AND r.workspace_id=NEW.workspace_id AND
      r.state='sealed' AND r.result_commit=NEW.verified_commit AND t.current_attempt_id=a.id AND t.sealed_result_id=r.id AND
      t.state='completed' AND t.cancel_epoch=0 AND
      (a.state='succeeded' OR (a.state='superseded' AND r.completion_authority='user_seal')) AND a.sealed_result_id=r.id AND
      e.entity_type='verification' AND e.entity_id=NEW.id AND e.type='verification.prepared' AND e.from_state IS NULL AND
      e.to_state='prepared' AND e.entity_revision=1 AND e.workspace_id=NEW.workspace_id AND e.task_id=NEW.task_id AND
      e.attempt_id=NEW.attempt_id AND e.result_id=NEW.result_id AND e.occurred_at=NEW.created_at AND
      json_extract(e.payload,'$.detail.expectedTaskRevision')=t.revision AND
      json_extract(e.payload,'$.detail.expectedAttemptRevision')=a.revision
  ) THEN RAISE(ABORT, 'verification preparation has no exact current proof') END;
END;
CREATE TRIGGER verifications_update_integrity BEFORE UPDATE ON verifications BEGIN
  SELECT CASE WHEN OLD.state IN ('succeeded','failed','recovery_required') THEN RAISE(ABORT, 'terminal verification is immutable') END;
  SELECT CASE WHEN NEW.id<>OLD.id OR NEW.result_id<>OLD.result_id OR NEW.task_id<>OLD.task_id OR NEW.attempt_id<>OLD.attempt_id OR
    NEW.workspace_id<>OLD.workspace_id OR NEW.policy_name<>OLD.policy_name OR NEW.policy_sha256<>OLD.policy_sha256 OR
    NEW.verified_commit<>OLD.verified_commit OR NEW.working_directory<>OLD.working_directory OR NEW.timeout_millis<>OLD.timeout_millis OR
    NEW.output_limit_bytes<>OLD.output_limit_bytes OR NEW.runner_name<>OLD.runner_name OR NEW.runner_version<>OLD.runner_version OR
    NEW.image_digest<>OLD.image_digest OR NEW.environment_sha256<>OLD.environment_sha256 OR NEW.created_at<>OLD.created_at
    THEN RAISE(ABORT, 'verification tuple is immutable') END;
  SELECT CASE WHEN NEW.revision<>OLD.revision+1 OR NEW.updated_at<OLD.updated_at OR NOT (
    (OLD.state='prepared' AND NEW.state IN ('running','recovery_required')) OR
    (OLD.state='running' AND NEW.state IN ('succeeded','failed','recovery_required'))
  ) THEN RAISE(ABORT, 'invalid verification transition') END;
  SELECT CASE WHEN NOT EXISTS (
    SELECT 1 FROM results r JOIN tasks t ON t.id=r.task_id AND t.workspace_id=r.workspace_id
    JOIN attempts a ON a.id=r.attempt_id AND a.task_id=r.task_id AND a.workspace_id=r.workspace_id
    JOIN journal_events e ON e.id=NEW.latest_event_id
    JOIN actor_snapshots ea ON ea.id=e.actor_snapshot_id AND ea.actor_type IN ('system','recovery')
    WHERE r.id=NEW.result_id AND r.state='sealed' AND r.result_commit=NEW.verified_commit AND t.id=NEW.task_id AND
      t.current_attempt_id=NEW.attempt_id AND t.sealed_result_id=r.id AND t.state='completed' AND t.cancel_epoch=0 AND
      a.id=NEW.attempt_id AND (a.state='succeeded' OR (a.state='superseded' AND r.completion_authority='user_seal')) AND
      a.sealed_result_id=r.id AND e.entity_type='verification' AND e.entity_id=NEW.id AND e.from_state=OLD.state AND e.to_state=NEW.state AND
      e.entity_revision=NEW.revision AND e.workspace_id=NEW.workspace_id AND e.task_id=NEW.task_id AND
      e.attempt_id=NEW.attempt_id AND e.result_id=NEW.result_id AND e.occurred_at=NEW.updated_at AND
      json_extract(e.payload,'$.detail.expectedRevision')=OLD.revision AND
      json_extract(e.payload,'$.detail.expectedTaskRevision')=t.revision AND
      json_extract(e.payload,'$.detail.expectedAttemptRevision')=a.revision
  ) THEN RAISE(ABORT, 'verification transition has no exact event or ownership') END;
  SELECT CASE WHEN OLD.effect_attempt=1 AND NEW.effect_attempt<>1 THEN RAISE(ABORT, 'verification effect attempt regressed') END;
  SELECT CASE WHEN OLD.state='prepared' AND NEW.state='running' AND EXISTS (
    SELECT 1 FROM publications p WHERE p.workspace_id=OLD.workspace_id AND p.state='running' AND p.effect_phase IN ('push_started','pr_create_started')
  ) THEN RAISE(ABORT, 'workspace already has an effecting publication') END;
END;

DROP TRIGGER publications_insert_integrity;
DROP TRIGGER publications_update_integrity;
CREATE TRIGGER publications_insert_integrity BEFORE INSERT ON publications BEGIN
  SELECT CASE WHEN NEW.state<>'prepared' OR NEW.effect_phase<>'none' OR NEW.revision<>1 OR NEW.created_at<>NEW.updated_at OR NOT EXISTS (
    SELECT 1 FROM results r JOIN tasks t ON t.id=r.task_id AND t.workspace_id=r.workspace_id
    JOIN attempts a ON a.id=r.attempt_id AND a.task_id=r.task_id AND a.workspace_id=r.workspace_id
    JOIN workspaces w ON w.id=r.workspace_id AND w.repository_id=r.repository_id
    JOIN verifications v ON v.id=NEW.verification_id AND v.result_id=r.id
    JOIN journal_events e ON e.id=NEW.latest_event_id
    WHERE r.id=NEW.result_id AND r.task_id=NEW.task_id AND r.attempt_id=NEW.attempt_id AND r.workspace_id=NEW.workspace_id AND
      r.state='sealed' AND r.repository_id=NEW.repository_id AND r.base_sha=NEW.base_sha AND r.result_commit=NEW.result_commit AND
      t.current_attempt_id=a.id AND t.sealed_result_id=r.id AND t.state='completed' AND t.cancel_epoch=0 AND
      (a.state='succeeded' OR (a.state='superseded' AND r.completion_authority='user_seal')) AND a.sealed_result_id=r.id AND
      v.state='succeeded' AND v.verified_commit=r.result_commit AND w.state='active' AND w.installation_id=NEW.installation_id AND
      w.repository_full_name=NEW.repository_full_name AND NEW.branch='fern/'||w.name||'/'||NEW.operation_id AND
      e.entity_type='publication' AND e.entity_id=NEW.id AND e.type='publication.prepared' AND e.from_state IS NULL AND
      e.to_state='prepared' AND e.entity_revision=1 AND e.workspace_id=NEW.workspace_id AND e.task_id=NEW.task_id AND
      e.attempt_id=NEW.attempt_id AND e.result_id=NEW.result_id AND e.occurred_at=NEW.created_at AND
      e.actor_snapshot_id=NEW.requester_actor_snapshot_id AND
      json_extract(e.payload,'$.detail.expectedTaskRevision')=t.revision AND
      json_extract(e.payload,'$.detail.expectedAttemptRevision')=a.revision
  ) THEN RAISE(ABORT, 'publication preparation has no exact current proof') END;
END;
CREATE TRIGGER publications_update_integrity BEFORE UPDATE ON publications BEGIN
  SELECT CASE WHEN OLD.state IN ('published','recovery_required','failed','conflict') THEN RAISE(ABORT, 'terminal publication is immutable') END;
  SELECT CASE WHEN NEW.id<>OLD.id OR NEW.operation_id<>OLD.operation_id OR NEW.result_id<>OLD.result_id OR
    NEW.verification_id<>OLD.verification_id OR NEW.task_id<>OLD.task_id OR NEW.attempt_id<>OLD.attempt_id OR
    NEW.workspace_id<>OLD.workspace_id OR NEW.installation_id<>OLD.installation_id OR NEW.repository_id<>OLD.repository_id OR
    NEW.repository_full_name<>OLD.repository_full_name OR NEW.base_ref<>OLD.base_ref OR NEW.base_sha<>OLD.base_sha OR
    NEW.result_commit<>OLD.result_commit OR NEW.branch<>OLD.branch OR NEW.expected_remote_old_sha IS NOT OLD.expected_remote_old_sha OR
    NEW.broker_policy_version<>OLD.broker_policy_version OR NEW.broker_policy_sha256<>OLD.broker_policy_sha256 OR
    NEW.requester_actor_snapshot_id<>OLD.requester_actor_snapshot_id OR NEW.created_at<>OLD.created_at
    THEN RAISE(ABORT, 'publication tuple is immutable') END;
  SELECT CASE WHEN NEW.revision<>OLD.revision+1 OR NEW.updated_at<OLD.updated_at OR NOT EXISTS (
    SELECT 1 FROM results r JOIN tasks t ON t.id=r.task_id AND t.workspace_id=r.workspace_id
    JOIN attempts a ON a.id=r.attempt_id AND a.task_id=r.task_id AND a.workspace_id=r.workspace_id
    JOIN verifications v ON v.id=OLD.verification_id AND v.result_id=r.id
    JOIN journal_events e ON e.id=NEW.latest_event_id
    JOIN actor_snapshots ea ON ea.id=e.actor_snapshot_id AND ea.actor_type IN ('system','recovery')
    WHERE r.id=OLD.result_id AND r.state='sealed' AND r.result_commit=OLD.result_commit AND t.id=OLD.task_id AND
      t.current_attempt_id=OLD.attempt_id AND t.sealed_result_id=r.id AND t.state='completed' AND t.cancel_epoch=0 AND
      a.id=OLD.attempt_id AND (a.state='succeeded' OR (a.state='superseded' AND r.completion_authority='user_seal')) AND
      a.sealed_result_id=r.id AND v.state='succeeded' AND v.verified_commit=r.result_commit AND
      e.entity_type='publication' AND e.entity_id=OLD.id AND e.from_state=OLD.state AND e.to_state=NEW.state AND
      e.entity_revision=NEW.revision AND e.workspace_id=OLD.workspace_id AND e.task_id=OLD.task_id AND
      e.attempt_id=OLD.attempt_id AND e.result_id=OLD.result_id AND e.occurred_at=NEW.updated_at AND
      json_extract(e.payload,'$.detail.expectedRevision')=OLD.revision AND
      json_extract(e.payload,'$.detail.expectedTaskRevision')=t.revision AND
      json_extract(e.payload,'$.detail.expectedAttemptRevision')=a.revision
  ) THEN RAISE(ABORT, 'publication transition has no exact event or ownership') END;
  SELECT CASE WHEN NOT (
    (NEW.effect_phase=OLD.effect_phase AND NEW.state IN ('uncertain','recovery_required','failed','conflict','published')) OR
    (OLD.effect_phase='none' AND NEW.effect_phase='push_started' AND NEW.state='running') OR
    (OLD.effect_phase='push_started' AND NEW.effect_phase='push_observed' AND NEW.state='running' AND NEW.observed_remote_sha=OLD.result_commit) OR
    (OLD.effect_phase='push_observed' AND NEW.effect_phase='pr_create_started' AND NEW.state='running')
  ) THEN RAISE(ABORT, 'publication effect phase regressed or skipped') END;
  SELECT CASE WHEN OLD.effect_phase='none' AND NEW.effect_phase='push_started' AND EXISTS (
    SELECT 1 FROM verifications v WHERE v.workspace_id=OLD.workspace_id AND v.state='running'
  ) THEN RAISE(ABORT, 'workspace already has an effecting verification') END;
  SELECT CASE WHEN NEW.state='published' AND NOT (
    OLD.effect_phase IN ('push_observed','pr_create_started') AND NEW.observed_remote_sha=OLD.result_commit AND
    NEW.pr_repository_id=OLD.repository_id AND NEW.pr_repository_full_name=OLD.repository_full_name AND NEW.pr_state='open' AND NEW.pr_draft=1 AND
    NEW.pr_base_repository_id=OLD.repository_id AND NEW.pr_base_repository_full_name=OLD.repository_full_name AND
    NEW.pr_base_ref=OLD.base_ref AND NEW.pr_base_sha=OLD.base_sha AND NEW.pr_head_repository_id=OLD.repository_id AND
    NEW.pr_head_repository_full_name=OLD.repository_full_name AND NEW.pr_head_repository_owner||'/'||NEW.pr_head_repository_name=OLD.repository_full_name AND
    NEW.pr_head_ref=OLD.branch AND NEW.pr_head_sha=OLD.result_commit AND
    NEW.pr_url='https://github.com/'||OLD.repository_full_name||'/pull/'||CAST(NEW.pr_number AS TEXT)
  ) THEN RAISE(ABORT, 'publication completion observation differs') END;
END;
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
	if version < 0 || version > len(migrations) {
		return fmt.Errorf("%w: user_version %d", ErrUnsupportedSchema, version)
	}
	if err := verifyMigrationLedger(ctx, conn, version); err != nil {
		return err
	}
	for _, m := range migrations[version:] {
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
		if gotVersion != seen || gotVersion > len(migrations) {
			return fmt.Errorf("%w: unknown migration %d", ErrUnsupportedSchema, gotVersion)
		}
		expected := migrations[gotVersion-1]
		if name != expected.name || checksum != migrationChecksum(expected) {
			return fmt.Errorf("%w: migration %d", ErrMigrationDrift, gotVersion)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read migration ledger: %w", err)
	}
	if seen != version {
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
