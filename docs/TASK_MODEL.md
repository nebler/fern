# Durable Task Model

## Status And Scope

This document is the contract foundation for Fern-owned durable remote tasks.
It is normative for the task store, coordinator, phone API, result service,
verification runner, and publication broker. Implemented portions include task
admission, delivery, cancellation, conservative post-admission execution
observation, execution projection, immutable result sealing, verification,
receipt-backed App publication admission, and publication reconciliation. The
observer
can prove running and live input states but cannot infer generic terminal success
from this pinned OpenCode profile. `internal/taskresult` implements exact
read-only Git collection. `internal/taskresultcoord` has both an external-
observer sealing path and a production-composed user-authorized snapshot path;
the latter records an idempotent exact preview, supersedes the attempt, and
completes the task without claiming OpenCode success. No production observer can
currently provide generic success proof. Future approval behavior is explicitly
distinguished from the implemented HTTP surface below.
The legacy control plane still has coarse
`Workflow` and `Publication` records outside this task-store boundary.

[REMOTE_PRODUCT.md](./REMOTE_PRODUCT.md) remains authoritative for product
direction. OpenCode remains authoritative for conversations, tools,
permissions, questions, forms, terminals, files, diffs, and its own persistence.
Git and the workspace are authoritative for repository content. GitHub is
authoritative for remote repositories, refs, and pull requests. Fern owns the
durable receipt, delivery shadow, attempts, cancellation intent, projected
events, result provenance, verification, and publication journal described
here.

The publication journal specifies only Fern-owned effects. In mutually
exclusive `workspace-gh` mode, OpenCode has an authenticated workspace `gh` and
may publish when explicitly requested in the prompt; those direct effects
cannot be exclusively authorized or completely observed by Fern. GitHub App
invariants are normative only for `github-app-broker` mode.

Normative language uses **MUST**, **MUST NOT**, **SHOULD**, and **MAY**. Sections
marked **HARNESS ASSUMPTION** are not established facts. They MUST be proved
against the exact image digest built from `images/opencode/Dockerfile` before a
delivery worker or release claim depends on them.

## Authority And Invariants

| Concern | Authority | Fern record |
| --- | --- | --- |
| Workspace configuration and lifecycle intent | Fern | `Workspace` |
| Repository identity | GitHub numeric repository ID selected through configured App or workspace-`gh` authority | `Workspace.repository_id` |
| Base content | Git commit object | `Task.base_sha` |
| Task admission, idempotency, cancellation intent | Fern SQLite | `Receipt`, `Task`, `Event` |
| Conversation and agent execution | OpenCode | exact IDs on `Attempt`; no durable approval row is currently composed |
| Delivery reconciliation | OpenCode durable session log and finite projections, shadowed by Fern | `Attempt`, `Event` |
| Working content | Git objects and worktree | sealed by `Result` |
| Test claim | Fern verification runner | `Verification` |
| Fern-owned push and draft pull request effect | Fern publication broker and GitHub | `Publication` |
| Prompt-authorized direct GitHub effect | OpenCode with workspace `gh` and GitHub | OpenCode/session evidence only |
| Actor attribution | Fern authentication boundary | immutable actor snapshot on every command and event |

The following invariants are unconditional:

1. A task, first attempt, receipt, prompt hash, base SHA, actor, OpenCode session
   ID, and OpenCode message ID MUST commit in one SQLite transaction before Fern
   wakes compute or sends any OpenCode request.
2. Fern MUST never deduplicate by prompt text, transcript text, title, timestamp,
   or current `HEAD`. It uses the command idempotency key and exact persisted
   OpenCode IDs.
3. A retry MUST reuse the persisted OpenCode IDs. It MUST NOT allocate a second
   session or message to escape an ambiguous response.
4. `repository_id`, `base_sha`, and the task's workspace are immutable after
   admission. Repository name and base branch name are display metadata, not
   identity.
5. A `Result` seals one exact commit and a clean-worktree observation. A
   `Verification` tests that same commit. A `Publication` pushes that same
   commit. No stage consumes mutable current `HEAD` as authority.
6. Durable cancellation intent fences all new Fern-owned effects before Fern
   asks OpenCode to interrupt. Cancellation does not claim rollback of provider
   charges, completed tools, filesystem changes, commits, pushes, or pull
   requests.
7. No external request is made while a SQLite write transaction is open.
   Persist intent, commit, perform the effect, observe authoritative state, then
   commit the observation.
8. Every state change appends one immutable Fern `Event` in the same transaction
   as the changed entity. Client-visible state without a corresponding event is
   invalid.
9. `uncertain` means an external effect may have happened and automated
   reconciliation is still safe. `recovery_required` means Fern cannot prove a
   unique safe state or an invariant failed; no new effect may run until an
   explicit recovery action restores proof.
10. Fern provides at-least-once worker execution with idempotent reconciliation.
    It MUST NOT claim exactly-once provider, tool, Git, or GitHub execution.
11. Every mutable App publication MUST reference the exact accepted
    `result.publish` receipt that authorized it. A legacy publication without
    that receipt is quarantined and grants no worker authority.

## Identifiers

### Fern IDs

Fern generates IDs on the host using a cryptographically random UUIDv7. The
wire form is a lowercase prefix, underscore, and canonical lowercase UUID with
hyphens. IDs are immutable, globally unique within an appliance, opaque to
clients, and at most 64 bytes.

| Entity | Format | Example |
| --- | --- | --- |
| Workspace | `wsp_<uuidv7>` | `wsp_0198d34d-5e40-7c5a-8e3f-6bfad471ae12` |
| Task | `tsk_<uuidv7>` | `tsk_0198d34d-6a50-75fb-b1f2-b4a14d70ec55` |
| Attempt | `att_<uuidv7>` | `att_0198d34d-6a50-75fb-b1f2-b4a14d70ec56` |
| Receipt | `rcp_<uuidv7>` | `rcp_0198d34d-6a50-75fb-b1f2-b4a14d70ec57` |
| Event | `fev_<uuidv7>` | `fev_0198d34d-6a50-75fb-b1f2-b4a14d70ec58` |
| Approval | `apr_<uuidv7>` | `apr_0198d34d-6a50-75fb-b1f2-b4a14d70ec59` |
| Result | `res_<uuidv7>` | `res_0198d34d-6a50-75fb-b1f2-b4a14d70ec5a` |
| Verification | `ver_<uuidv7>` | `ver_0198d34d-6a50-75fb-b1f2-b4a14d70ec5b` |
| Publication | `pub_<uuidv7>` | `pub_0198d34d-6a50-75fb-b1f2-b4a14d70ec5c` |
| Publication operation | `op_<uuidv7>` | `op_0198d34d-6a50-75fb-b1f2-b4a14d70ec5d` |

The Fern event reconnect cursor is a separate SQLite-assigned positive signed
64-bit integer. It is encoded as a base-10 JSON string so JavaScript clients do not
lose precision. It is monotonically increasing for the database, is never
reused, and need not be contiguous within a workspace.

GitHub `installation_id`, `repository_id`, and pull request number are encoded
as base-10 strings on the wire. Git object IDs are lowercase hexadecimal. The
first supported Git object format is SHA-1, so `base_sha`, `result_commit`,
`verified_commit`, and `remote_sha` are exactly 40 lowercase hex characters.
Unsupported object formats fail preflight rather than being truncated or
translated.

### Exact OpenCode IDs

Each `Attempt` stores these exact, case-sensitive, opaque strings:

- `opencode_session_id`, beginning `ses` as required by the pinned schema;
- `opencode_message_id`, beginning `msg_` as required by the pinned schema;
- nullable reserved `opencode_log_aggregate_id` and `opencode_log_seq` fields,
  which remain unset for the pinned profile because it has no durable log
  cursor.

Fern generates the session and message IDs once using 128 bits of randomness,
encoded as lowercase hex after `ses_` and `msg_`, respectively. For example,
`ses_8c41c724db7b4bb5974d11977ee67c92` and
`msg_26fc9098cfa84ca6a272d97b712e797f`. They are persisted before wake and sent in
the OpenCode request body. Fern compares returned and projected IDs byte for
byte. An upstream response containing another ID is a protocol conflict and
moves the attempt to `recovery_required`.

OpenCode event, permission, question, and form IDs are upstream-assigned and
stored byte for byte. Current advertised prefixes are `evt_`, `per`, `que`, and
`frm_`; Fern does not synthesize them.

**OBSERVED CONTRACT OC-ID-1:** The pinned `0.0.0-next-17444` server accepts and
returns caller-selected session IDs in `POST /api/session` and caller-selected
message IDs in `POST /api/session/{sessionID}/prompt`.

**OBSERVED CONTRACT OC-ID-2:** Reusing a message ID with the same observed wire
payload returns the same admission without adding another inbox item. Reusing it
with different text returns `409 ConflictError`. The harness proved this after
dropping the first HTTP response and again after container replacement. Prompt
mutation is still never retried by Fern because `resume` is not persisted or
idempotency-bound; restart recovery uses exact read-only reconciliation.

## Authoritative Entities

All timestamps are UTC RFC 3339 with nanoseconds on the API and integer Unix
milliseconds in SQLite. Input timestamps are never accepted from clients.
Every mutable row has `revision`, `created_at`, and `updated_at`. Revisions start
at 1 and increment in the same transaction as each state change.
GitHub numeric identities are encoded as decimal strings on the API and must fit
a positive signed 64-bit SQLite `INTEGER` before admission.

### Workspace

`Workspace` is the Fern lifecycle and repository binding. Required fields are:

- `id`, stable configured `name`, and `state`;
- canonical host repository path, never exposed as a client-selectable effect
  parameter;
- GitHub `installation_id`, numeric `repository_id`, and display-only
  `repository_full_name`;
- approved image digest and OpenCode protocol version;
- current runtime desired state and reconciliation epoch;
- optional active task lease and recovery reason.

One workspace binds one repository. Changing numeric repository identity creates
a new workspace. A rename or transfer updates only display metadata after the
same numeric ID is revalidated.

### Task

`Task` is the durable user intent. Required fields are:

- `id`, `workspace_id`, `title`, encrypted or access-controlled prompt body,
  and `prompt_sha256` over the exact UTF-8 prompt bytes;
- immutable `repository_id`, display base ref, exact `base_sha`, and repository
  object format;
- `state`, `cancel_epoch`, optional cancellation reason, current attempt ID,
  terminal reason, and actor snapshot;
- latest Fern event cursor and timestamps.

The task prompt is bounded to 64 KiB and title to 200 UTF-8 bytes. The first
release permits one effecting task at a time per workspace. Queue order is the
acceptance event cursor, not wall-clock time.

### Attempt

`Attempt` is one bounded execution against a task. Required fields are:

- `id`, `task_id`, positive `sequence`, and `state`;
- closed monotonic `delivery_phase` (`none`, `claimed`,
  `session_create_started`, `session_ready`, or `prompt_started`), recording the
  last delivery effect boundary durably entered;
- exact OpenCode IDs described above;
- immutable prompt hash, base SHA, image digest, execution contract version,
  agent/model selection snapshot, deadline, and budget snapshot;
- delivery claim owner and expiry, delivery timestamps, latest completed local
  projection scan, cancellation acknowledgment, and terminal reason.

`UNIQUE(task_id, sequence)`, `UNIQUE(opencode_session_id)`, and
`UNIQUE(opencode_session_id, opencode_message_id)` are required. A replacement
attempt has new OpenCode IDs and a higher sequence, but remains bound to the
task's repository ID and base SHA. It is created only while the task remains
nonterminal, by an explicit retry command after the previous attempt is terminal
or recovery authorizes replacement.

### Receipt

`Receipt` is immutable proof that Fern accepted a mutating command. It contains:

- `id`, `workspace_id`, command kind, idempotency key, canonical request hash,
  actor snapshot, accepted timestamp, and API contract version;
- target entity type and ID;
- the accepted response status and immutable response projection.

Receipts have the single state `accepted` and never transition. Validation or
authorization failures produce an audit event but no acceptance receipt.

### Event

`Event` is an immutable Fern projection or audit fact. It contains:

- `id`, decimal `cursor`, `workspace_id`, optional task/attempt/entity IDs,
  stable event `type`, schema `version`, server timestamp, actor snapshot, and a
  bounded JSON payload;
- optional exact upstream object ID, object kind, and bounded semantic payload
  hash for facts projected from the selected OpenCode profile.

Payloads are at most 64 KiB. Logs and artifacts are referenced by digest rather
than embedded. Unknown OpenCode events may be retained as bounded raw evidence,
but MUST NOT advance task state until the adapter understands them. Dedupe
constraints include `UNIQUE(attempt_id, upstream_kind, upstream_object_id)` when
an upstream object ID exists. The same object ID with incompatible bytes enters
`recovery_required`; there is no upstream aggregate sequence in this profile.

### Approval (Target Contract)

`Approval` is the target Fern shadow of one OpenCode permission, question, or
form. The current schema has no approvals table and the current HTTP API has no
decision route. It would contain:

- `id`, `task_id`, `attempt_id`, kind (`permission`, `question`, or `form`),
  exact upstream request ID, exact session ID, and state;
- bounded display payload, payload hash, context hash, source message/tool IDs,
  and the local projection scan and timestamp at which it was observed;
- decision, deciding actor snapshot, decision receipt, delivery attempts,
  upstream acknowledgment evidence, expiry, and terminal reason.

The context hash covers task ID, attempt ID, session ID, upstream request ID,
kind, normalized request payload, source message/tool identity, task cancel
epoch, and result commit if one exists. A decision is invalid after any covered
value changes. Fern presents a summary and deep-link to OpenCode; it does not
replace OpenCode's approval UI or invent options.

### Result

`Result` is an immutable provenance envelope assembled after OpenCode execution.
It contains:

- `id`, `task_id`, `attempt_id`, state and outcome (`changed` or `no_changes`);
- numeric `repository_id`, exact `base_sha`, exact `result_commit`, commit tree
  OID, and observed clean-worktree flag;
- sorted changed-file manifest with path bytes safely encoded, change kind,
  mode, blob OIDs, sizes, and manifest SHA-256;
- exact OpenCode session/message IDs plus a digest of the bounded projected
  evidence set used to explain the result;
- creator actor, collection timestamps, policy version, and failure evidence.

The sealed fields never change. A later commit produces a new result. A task has
at most one publication-eligible sealed result, selected explicitly if multiple
attempts produced candidates.

### Verification

`Verification` is one bounded command run against one sealed result. It
contains:

- `id`, `result_id`, state, policy check name, argv array, sanitized working
  directory, timeout, runner identity/version, image and environment digests;
- exact `verified_commit`, start/end timestamps, exit status or signal;
- stdout/stderr full byte counts and SHA-256 digests plus bounded retained-byte
  counts and truncation flags. The task store contains no output bytes.

Shell command strings are not accepted from mobile clients. Commands come from
the approved workspace verification policy. A successful verification requires
exit status zero and `verified_commit == Result.result_commit` before and after
execution.

### Publication

`Publication` is the durable journal for one exact GitHub branch and draft pull
request operation. It contains:

- `id`, immutable `operation_id`, `result_id`, state, requesting actor and
  receipt;
- `installation_id`, numeric `repository_id`, display repository name, base ref,
  exact `base_sha`, exact `result_commit`, and branch;
- expected remote old SHA, observed remote SHA, numeric pull request number,
  URL, draft state, observed base ref/SHA and head ref/SHA;
- broker lease, attempt count, last error, reconciliation evidence, and
  timestamps.

`operation_id` is the idempotency identity passed to the GitHub App broker. The
branch is exactly `fern/<workspace-name>/<operation_id>` and MUST differ from
the base ref. Repository URL, refspec, credential, path, and force options are
never accepted from API callers.

## State Machines

Unlisted transitions are invalid and return `409 invalid_transition`. Terminal
states do not regress. A stale worker update guarded by an old revision or lease
is ignored and audited.

### Workspace

| State | Allowed next states | Meaning |
| --- | --- | --- |
| `active` | `maintenance`, `recovery_required`, `disabled` | New work may be accepted subject to queue policy. |
| `maintenance` | `active`, `recovery_required`, `disabled` | No new effects; migrations or operator maintenance are running. |
| `recovery_required` | `active`, `maintenance`, `disabled` | Repository, runtime, database, or external authority cannot be reconciled automatically. |
| `disabled` | `active`, `maintenance` | No task or publication effects run. |

### Receipt And Event

A `Receipt` is created directly in `accepted` and has no transition. An `Event`
is append-only and has no mutable state. Corrections are later events; neither
record is updated or deleted to rewrite history.

### Task

| State | Allowed next states | Meaning |
| --- | --- | --- |
| `queued` | `running`, `cancel_requested`, `uncertain`, `recovery_required`, `failed` | Accepted durably; no execution is yet proven. |
| `running` | `input_required`, `cancel_requested`, `completed`, `failed`, `uncertain`, `recovery_required` | Current attempt is admitted or executing. |
| `input_required` | `running`, `cancel_requested`, `completed`, `failed`, `uncertain`, `recovery_required` | At least one current approval is pending; explicit user sealing may complete the task without answering it. |
| `cancel_requested` | `canceled`, `uncertain`, `recovery_required` | Durable Fern effect fence is active; upstream stop is not yet proven. |
| `uncertain` | `queued`, `running`, `input_required`, `cancel_requested`, `completed`, `failed`, `canceled`, `recovery_required` | An ambiguous effect is being reconciled. |
| `recovery_required` | `queued`, `running`, `cancel_requested`, `failed`, `canceled` | Explicit recovery action and evidence are required. |
| `completed` | none | Execution completed and a sealed result, including `no_changes`, exists. |
| `failed` | none | The task ended without a publishable result. Explicit retry creates a new task; it does not reopen this state. |
| `canceled` | none | Cancellation fence is durable and upstream execution is absent or acknowledged; prior side effects may remain. |

### Attempt

| State | Allowed next states |
| --- | --- |
| `prepared` | `delivering`, `cancel_requested`, `recovery_required`, `failed` |
| `delivering` | `admitted`, `uncertain`, `cancel_requested`, `recovery_required`, `failed` |
| `admitted` | `running`, `input_required`, `cancel_requested`, `succeeded`, `failed`, `uncertain`, `recovery_required`, `superseded` |
| `running` | `input_required`, `cancel_requested`, `succeeded`, `failed`, `uncertain`, `recovery_required`, `superseded` |
| `input_required` | `running`, `cancel_requested`, `failed`, `uncertain`, `recovery_required`, `superseded` |
| `cancel_requested` | `canceled`, `uncertain`, `recovery_required` |
| `uncertain` | `prepared`, `delivering`, `admitted`, `running`, `input_required`, `cancel_requested`, `succeeded`, `failed`, `canceled`, `recovery_required` |
| `recovery_required` | `prepared`, `admitted`, `running`, `cancel_requested`, `failed`, `canceled`, `superseded` |
| `succeeded`, `failed`, `canceled`, `superseded` | none |

`admitted` means the exact message is durably present upstream, not merely that
Fern received HTTP 200. `succeeded` means the OpenCode execution terminal event
was reconciled; task completion still requires result sealing.

Delivery phase is independent evidence within this state machine. A `prepared`
attempt has phase `none`; claiming it sets `claimed`. While `delivering`, phase
advances exactly one edge at a time:
`claimed -> session_create_started -> session_ready -> prompt_started`. Each
edge is an immutable attempt event and advances the owning task's latest event
cursor and both row revisions in one transaction before the named external
effect. A delivering attempt always has a non-`none` phase and a live claim.
Admission is valid only from `prompt_started`; admitted and later execution
states retain `prompt_started`. Ambiguity, recovery-required, failure, and
cancellation preserve the last phase, including `none` when prepared work is
canceled or expires before delivery.

### Approval (Target Contract, Not Schema 9)

No approval/question/form table, delivery coordinator, or answer API is composed
in the implemented schema-10 service. This state machine describes a future
contract only.

| State | Allowed next states |
| --- | --- |
| `pending` | `decision_recorded`, `expired`, `canceled`, `recovery_required` |
| `decision_recorded` | `delivering`, `canceled`, `recovery_required` |
| `delivering` | `applied`, `rejected`, `uncertain`, `recovery_required` |
| `uncertain` | `delivering`, `applied`, `rejected`, `canceled`, `recovery_required` |
| `recovery_required` | `delivering`, `rejected`, `canceled` |
| `applied`, `rejected`, `expired`, `canceled` | none |

`rejected` means the decision was invalid or refused, not the OpenCode
permission reply value `reject`. A valid user choice of `reject` can reach
`applied` after OpenCode acknowledges it.

### Result

Schema 6 inserts an immutable Result directly in `sealed`; collection,
uncertainty, and recovery are coordinator/request phases rather than mutable
Result rows. A future disposable-artifact workflow may add a separate export
journal, but it must not mutate a committed sealed Result.

| State | Allowed next states |
| --- | --- |
| `sealed` | none |

### Verification

| State | Allowed next states |
| --- | --- |
| `prepared` | `running`, `recovery_required` |
| `running` | `succeeded`, `failed`, `recovery_required` |
| `succeeded`, `failed`, `recovery_required` | none |

Migration 3 introduced one effecting verification attempt. Entering `running`
atomically changes `effect_attempt` from zero to one; it can never change again.
A process loss or ambiguous runner observation therefore enters
`recovery_required`, not `prepared` or `running`, and the command is never
automatically rerun.

### Publication

| State | Allowed next states |
| --- | --- |
| `prepared` | `running`, `recovery_required`, `failed`, `conflict` |
| `running` | `running`, `uncertain`, `published`, `recovery_required`, `failed`, `conflict` |
| `uncertain` | `running`, `published`, `recovery_required`, `failed`, `conflict` |
| `published`, `recovery_required`, `failed`, `conflict` | none |

`conflict` is terminal and visible when authoritative remote state differs from
the immutable tuple. Fern never force-pushes or overwrites the conflict.

Publication state is paired with a monotonic effect phase:
`none -> push_started -> push_observed -> pr_create_started`. The broker commits
`push_started` before push and `pr_create_started` before pull-request creation.
`push_observed` records the exact result commit read from the remote ref. A
phase never regresses or skips an edge. After either mutation-start phase, a
coordinator performs authoritative reads; it never assumes that retrying the
mutation is safe. An exact already-existing pull request may complete from
`push_observed` without entering `pr_create_started`.

## Idempotency And Conflict Rules

Every mutating Fern API requires `Idempotency-Key`. It is 1 to 128 printable
ASCII bytes and MUST NOT contain whitespace at either end. Scope is
`(workspace_id, command_kind, idempotency_key)`. The first accepted command owns
the key and stores its actor.

Each command defines a bounded, closed canonical projection. The task API
computes `request_hash = SHA-256(command_kind || "\n" || json.Marshal(projection))`
after strict decoding into a server-owned Go struct. Struct field order fixes the
wire order, strings use `encoding/json` escaping, and omitted cancellation reason
is normalized to the empty string. Task submission projects only `title`,
`prompt`, and `baseRef`; cancellation projects the path task ID and `reason`.
This is a known-schema convention, not generic JSON canonicalization and not an
RFC 8785 claim. Authentication headers, trace headers, generated IDs, actor,
timestamps, and transport details are excluded. Prompt bytes remain part of the
submission projection; `prompt_sha256` is also stored separately.

| Retry condition | Result |
| --- | --- |
| Same scope, key, request hash, and actor authority | Return the original receipt and target IDs. Set `Idempotency-Replayed: true`. Do not repeat the command transaction. |
| Same scope and key, different request hash | `409 idempotency_conflict`; disclose no existing receipt, target, actor, or request content. |
| Same scope and key from a different actor | `403 idempotency_owner_mismatch`; disclose no target details unless that actor can already read the target. |
| Same key in another workspace or command kind | Independent command. |
| Missing or malformed key | `400 invalid_idempotency_key`; no receipt. |

Workers use entity IDs plus durable leases, not client idempotency keys, to
deduplicate execution. A lease includes owner UUID, expiry, and entity revision.
Taking over an expired lease first commits a recovery event. A stale owner
cannot commit because updates compare owner and revision.

OpenCode delivery reconciliation for an ambiguous response is ordered:

1. Read the exact session ID.
2. Read the exact message ID projection when the pinned profile exposes it.
3. List the session inbox and match the exact message ID.
4. Scan finite ascending message pages to exhaustion, rejecting repeated
   cursors, duplicate IDs with incompatible bytes, or non-ascending order.
5. If the ID is present, verify semantic payload hash and mark `admitted` or its
   later observed state.
6. Retry with the same ID and payload only when the exact-image contract proves
   that the relevant protocol epoch cannot duplicate a provider turn. Until
   restart-safe retry is proved, absence after restart is not proof of
   non-admission.
7. If presence, absence, or payload equality cannot be proved, remain
   `uncertain`; after the bounded reconciliation deadline enter
   `recovery_required`.

No retry submits another ID. A `409` is not automatically success; Fern must
read and compare the exact existing object.

## Pinned OpenCode Adapter Contract

The pinned image and checked-in black-box harness expose the following V2
operations. Profile differences are called out below rather than hidden behind
automatic fallback:

```http
POST /api/session
Content-Type: application/json

{
  "id": "ses_8c41c724db7b4bb5974d11977ee67c92",
  "title": "Fix signup",
  "location": { "directory": "/home/user/workspace" }
}
```

Success is expected as `200 {"data":{"id":"ses_...", ...}}`. Fern then sends:

```http
POST /api/session/ses_8c41c724db7b4bb5974d11977ee67c92/prompt
Content-Type: application/json

{
  "id": "msg_26fc9098cfa84ca6a272d97b712e797f",
  "text": "Fix signup and run the approved checks.",
  "resume": true
}
```

The checked-in black-box harness proves this top-level `text` form and expects
`200 {"data":{"id":"msg_...", ...}}` with the exact message ID. A `409
ConflictError` is reconciled through the finite inbox/message projections below.
Fern MUST bind this shape to the recorded image digest; it MUST NOT silently
fall back to a nested `prompt` shape or mix fields across protocol versions.

The adapter uses these finite and durable surfaces:

- `GET /api/session/{sessionID}` for exact session existence;
- `GET /api/session/{sessionID}/inbox` for exact pending message reconciliation;
- `GET /api/session/{sessionID}/message?limit=<bounded>&order=asc&cursor=<opaque>`
  for finite, ordered, duplicate-free message projection and restart recovery;
- `POST /api/session/{sessionID}/interrupt?continue=false` for active
  interruption;
- `DELETE /api/session/{sessionID}/inbox/{messageID}` only for a proven
  undelivered item;
- session-scoped permission and form list/state/reply routes. Model questions in
  this image use `/form`; legacy `/question` is not the question authority.

This pinned image does not expose `/api/session/{sessionID}/history` or
`/api/session/{sessionID}/event`. Its experimental log returns only
`log.synced`, including for a future `after` value, and therefore is not a
durable replay source. `opencode_log_aggregate_id` and `opencode_log_seq` remain
nullable reserved fields; no transition may depend on them for this profile.
Fern instead stores exact projected object IDs and re-scans bounded finite
projections after reconnect. If a complete synchronized scan cannot establish a
unique compatible state, the attempt enters `recovery_required`.

`GET /api/event` is explicitly volatile. It may wake the reconciler, improve
latency, and support lifecycle observation, but it is never Fern's reconnect
cursor and never proves complete history.

**OBSERVED CONTRACT OC-WIRE-1:** The top-level `text` prompt, inbox projection,
and cursor-paginated message projection above are the selected profile for image
digest `sha256:73688cd6f96ce3b236bb1c2d25607b03566a4ee92f0fedabeb06fd1a3e643c6c`,
which reports `0.0.0-next-17444`.

**OBSERVED CONTRACT OC-LOG-1:** There is no usable durable event/log cursor in
this profile. Global `/api/event` is volatile and ignores `Last-Event-ID`.

**OBSERVED CONTRACT OC-DELIVERY-1:** A prompt is visible in the durable inbox
after its HTTP response is dropped; an exact retry returns the same admission
and does not add another inbox item. A conflicting retry fails closed.

**OBSERVED CONTRACT OC-DELIVERY-2:** Caller-selected session/message identity,
ordered finite messages, exact retry deduplication, and conflicting retry
behavior survive OpenCode container replacement. A response-lost executing
prompt completes exactly one fake-provider turn; exact retry after replacement
creates no second inbox item, message, or provider turn.

**OBSERVED CONTRACT OC-APPROVAL-1:** Permission `ask` requests can be listed,
read, and replied once while the owning process is live; a duplicate reply is
not accepted. Model questions use forms, and an in-process answer resumes once.
Pending and answered form state both disappear after container replacement. A
pending question can remain durably represented as a `running` tool while the
session is inactive, so any form-backed nonterminal attempt observed across an
OpenCode epoch change enters `recovery_required`; Fern must not recreate or
auto-answer the form. Pending synthetic permissions also disappear after both a
same-container process restart and container replacement while their durable
session remains.

**OBSERVED CONTRACT OC-CANCEL-1:** Interrupt returns `204`, closes provider work,
removes active ownership, and records `Step interrupted`; this evidence survives
container replacement without resurrecting execution. An undelivered
`resume:false` inbox item survives process restart, can be deleted without a
message/provider projection, remains absent after replacement, and permits exact
ID reuse. Interrupt before admission and after completed provider work is an
idle `204` no-op; concurrent decision/interruption races remain open.

The checked-in exact-image harness proves caller IDs, response-loss admission,
finite message pagination, volatile event behavior, live permission/form
behavior, provider-turn deduplication, and interrupt evidence across container
replacement without a paid provider. It also establishes that form state is
process-local and unsafe to reconstruct, pending permissions are process-epoch
state, and undelivered inbox deletion is restart-stable. Concurrent permission
decision and interruption races remain open. The harness must retain request,
response, database, process-kill, and restart evidence for each release claim.

## Transaction Boundaries

SQLite is configured with foreign keys enabled, WAL mode, bounded busy timeout,
and a supported synchronous setting selected by the durability test. Every
transition service owns its SQL; HTTP handlers, mobile code, OpenCode adapters,
and broker implementations do not issue ad hoc state updates.

### Task Admission

One `BEGIN IMMEDIATE` transaction MUST:

1. Authenticate and snapshot the actor; validate workspace state and queue
   capacity.
2. Claim the scoped idempotency key or resolve replay/conflict.
3. Validate the configured numeric repository ID and exact requested base SHA
   against already-fetched authoritative repository metadata.
4. Insert `Task`, sequence-1 `Attempt`, exact OpenCode IDs, and `Receipt`.
5. Append `task.accepted` and `attempt.prepared` events.
6. Commit.

Only after commit may the coordinator request a wake. Wake failure changes the
attempt in a later transaction; it does not erase acceptance.

### Delivery

The worker transactionally claims the attempt, sets phase `claimed`, and changes
`prepared` to `delivering`, then commits. Before session creation, after exact
session readiness, and before prompt submission, separate narrow transactions
advance the phase to `session_create_started`, `session_ready`, and
`prompt_started`. Session reads/creation and prompt submission occur outside
SQLite. A later transaction stores returned evidence and transitions to
`admitted`, `uncertain`, or `recovery_required` with an event. Admission requires
`prompt_started`. Deterministic local delivery errors enter `recovery_required`;
the retained phase records whether session creation or prompt submission may
have begun. HTTP caller disconnect cancels only waiting for the receipt
response; it does not cancel the accepted task or service-owned worker.

An expired delivering claim transitions to `uncertain` without changing phase;
the stale owner is fenced by owner and revision checks. Startup and delivery
reconciliation locate only the workspace's current `delivering` or `uncertain`
attempt. Resolving `uncertain` to `admitted` requires `prompt_started`; resolving
to `recovery_required` preserves any non-`none` phase. Both outcomes require
exact attempt and task revisions and ordered attempt/task events. A queued
`prepared` attempt at or after its deadline instead transitions atomically to
attempt/task `failed` with reason `deadline_elapsed`; it is never claimed or
woken merely to record expiration.

One recovery-only restart-liveness transition may move the current task and
attempt from `uncertain` back to `running` and `delivering` with a fresh bounded
lease. It requires exact task and attempt revisions, exact expected phase,
ordered `attempt.delivery_resumed` and `task.running` events, and bounded
sanitized evidence from read-only reconciliation. It preserves
`delivery_started_at` and the phase, and is allowed only for `claimed`,
`session_create_started`, or `session_ready`, before the attempt deadline. It is
never allowed for `none` or `prompt_started`.

The coordinator's proof obligation depends on the retained phase. `claimed`
needs no external existence proof because no session mutation was durably
started. `session_create_started` requires reading and proving the exact session
ID exists with compatible immutable identity before resume; absence or conflict
enters `recovery_required`. `session_ready` likewise requires an exact compatible
session. After resume, the worker continues only along the remaining monotonic
phase edges. In particular, it never retries session creation from
`session_create_started` or `session_ready`; it advances to `session_ready` after
the proof, then toward `prompt_started`.

### Projection Reconciliation

Each bounded OpenCode inbox/message/form scan is fully decoded and validated
before a short transaction. That transaction inserts deduplicated Fern facts,
updates approval and attempt/task shadows, records the completed local scan, and
commits atomically. Re-observing an exact object is idempotent; incompatible
bytes for the same identity, malformed pagination, or an unknown
state-changing object enters `recovery_required`. Volatile OpenCode events may
schedule a scan but never advance durable Fern state by themselves.

The implemented post-admission task-store boundary is deliberately closed:

- `FindExecutionAttempt(workspaceID)` discovers only the current exact
  `admitted`, `running`, or `input_required` attempt whose task shadow agrees and
  whose `cancel_epoch` is zero. `InspectExecutionAttempt(attemptID)` performs the
  same eligibility check by ID. These are discovery reads, not effect authority.
- `RecordExecutionProjection(params)` is the only execution projection write.
  It requires exact task and attempt IDs and revisions, expected source state,
  exact persisted OpenCode session and message IDs, two fresh event IDs, an
  exact-millisecond observation time, bounded sanitized JSON evidence and its
  SHA-256, and a `system` or `recovery` actor.
- Its closed outcomes are `running`, `input_required`, `recovery_required`,
  `failed`, and `succeeded`. It permits only the Attempt state-machine edges
  listed above. `succeeded` leaves the task `running` until a result is sealed;
  `failed` terminates the task; `recovery_required` fences new effects.
- The transaction appends the attempt event before the paired task event,
  requires byte-identical payload and actor on both, updates both exact
  revisions, and advances `Task.latest_event_cursor` to the task event. The
  payload binds task ID, attempt ID, both expected revisions, source and target
  states, exact OpenCode IDs, evidence bytes, and evidence digest.
- An exact retry including IDs, revisions, state, event IDs, timestamp, actor,
  evidence bytes, and digest returns the committed projection without a write.
  A changed retry, stale revision, replacement attempt, cancellation race, or
  OpenCode identity mismatch is fenced.

Before calling `RecordExecutionProjection`, the coordinator MUST fully decode
and validate one finite authoritative OpenCode scan outside SQLite. It MUST prove
that every state-changing object belongs to the persisted exact session and
message, that pagination was complete and compatible, and that the requested
outcome follows from that proof. A volatile event, timeout, process death,
partial scan, unknown object, conflicting duplicate, or missing durable form
state is not proof. The coordinator MUST use `recovery_required` when a unique
safe projection cannot be established. The task store intentionally persists no
approval, question, or form state in this tranche; it MUST NOT reconstruct or
invent that state. A form-backed state that cannot be durably proved across an
OpenCode epoch therefore projects only to `recovery_required`.

### Approval Decision

One transaction validates pending state and context hash, claims idempotency,
stores the actor's decision, inserts its receipt, changes the approval to
`decision_recorded`, and appends an event. Upstream reply happens after commit.
Its observation is committed separately. A client disconnect never retracts a
recorded decision.

### Cancellation

One transaction increments `Task.cancel_epoch`, stores actor and reason, changes
the task and current attempt to `cancel_requested`, cancels pending approvals,
fences unstarted verification/publication work, creates the receipt, and appends
events. Only then may Fern delete a proven undelivered inbox item or interrupt
OpenCode. Acknowledgment and any late completion are separate observations.

The coordinator closes that observation only through the task store's
`AcknowledgeCancellation` API. The command requires the exact task ID, current
attempt ID, cancellation epoch `1`, current task and attempt revisions, two new
event IDs, an exact-millisecond observation time, the persisted effect
disposition, bounded sanitized JSON evidence and its SHA-256, and a `system` or
`recovery` actor snapshot. It has no external-effect authority. In one SQLite
transaction it verifies that all identity, revision, state, epoch, and
disposition values still match; appends `attempt.canceled` before
`task.canceled`; sets `Attempt.cancellation_ack_at`; sets both terminal reasons
to `cancellation_acknowledged`; and closes the exact attempt and task as
`canceled`. It preserves the original cancellation actor, reason, requested
time, receipt, event IDs, effect disposition, and cancel epoch.

An exact retry of the acknowledgment tuple, including event IDs, actor,
timestamp, revisions, disposition, evidence bytes, and evidence digest, returns
the already committed transition without another write. A changed retry is a
conflict, and a stale revision, replacement/current-attempt mismatch, changed
cancel epoch, or concurrent state transition is fenced. The migration-1 schema
requires the ordered proof events, matching system/recovery actor, matching
event payloads, current ownership, revision increments, acknowledgment time,
terminal reason, and persisted disposition before either acknowledgment update
can commit. Acknowledgment time and terminal cancellation state are immutable.

For the initial single-attempt store, a task has one semantic cancellation:
`cancel_epoch` changes from 0 to 1 exactly once. An exact idempotency replay
returns the original receipt, events, epoch, and effect disposition. A different
idempotency key after that fence returns `409 cancellation_already_requested`
and does not create another receipt or event. A terminal task returns
`409 task_already_terminal` without a cancellation receipt or interrupt
authority.

### Result And Verification

Result collection holds the workspace effect lease, captures repository facts,
and revalidates them immediately before sealing in one transaction. The
transaction inserts the immutable result and manifest, completes the task, and
either retains a proven successful attempt or supersedes the active attempt for
a user-authorized snapshot. Repository reads occur outside the transaction, but
the workspace lease prevents lifecycle, task, and publication mutation during
capture.

Migration 4 added the production user-authorized branch. An eligible running or
input-required task is previewed by `POST .../seal-preview` under
`AcquirePaused`; the exact commit,
tree, outcome, manifest count/digest, clean flag, and workspace/task/attempt
revisions are then submitted with an idempotency key. `RequestSeal` atomically
stores the receipt, authorizer snapshot, stable result/event IDs, and immutable
expected tuple. The authorized coordinator claims that row, recollects the exact
snapshot under a retained fence, and `SealAuthorizedResult` inserts the result,
marks the attempt `superseded`, completes the task, and completes the request.
This authority means "accept this repository snapshot," not "OpenCode
succeeded."

The asynchronous authorized coordinator reacquires `AcquirePaused`, reselects
the exact ownership tuple, and recollects the approved snapshot while compute
remains unable to mutate it. This user-authorized fence is deliberately distinct
from the external-observer path's `AcquireQuiesced`: only an authoritative
observer needs running compute for two success observations before stopping it.

The implemented result-store surface is:

- `FindSucceededUnsealedAttempt(workspaceID)` discovers only the current
  `succeeded` attempt whose task is still `running`, has `cancel_epoch == 0`, and
  has no sealed result. This read grants no Git or OpenCode effect authority.
- `SealResult(params)` requires a fresh Result ID, exact task and attempt IDs and
  revisions, two fresh event IDs, the task's numeric repository ID and base SHA,
  exact result commit and tree OIDs, `clean == true`, outcome, canonical
  manifest and digest, exact persisted OpenCode IDs, bounded sanitized evidence
  and digest, policy version, collection and seal times, and a `system` or
  `recovery` actor.
- One transaction appends `attempt.result_sealed` before `task.completed`,
  inserts all manifest rows and the immutable `sealed` Result, binds that Result
  ID to the exact successful attempt and task, advances both revisions, advances
  the task event cursor, and changes the task to `completed`. Deferred ownership
  constraints prevent a standalone Result from committing without both links.
- An exact retry of the complete seal tuple returns the existing Result and
  events without writing. Another Result ID, changed tuple, stale revision,
  replacement attempt, or committed cancellation loses the race. Sealed Result
  rows, manifest rows, result links, and completed task state are immutable.

For this API, the manifest is a JSON array in supplied order using the exact Go
field projection `pathBase64`, `changeKind`, `oldMode`, `newMode`, `oldBlobOid`,
`newBlobOid`, `oldSize`, and `newSize`; nullable fields are encoded as `null`.
`manifest_sha256` is SHA-256 over `json.Marshal` of that array (`[]` for no
entries). `pathBase64` is canonical padded RFC 4648 base64 of Git path bytes.
Decoded paths MUST be nonempty, relative, NUL-free, contain no empty, `.` or
`..` component, and be strictly increasing by raw path bytes. The closed change
kinds are `added`, `modified`, and `deleted`; a rename is represented by a
delete/add pair. Modes are `100644`, `100755`, or `120000`. Presence of old/new
mode, blob OID, and size MUST agree with the change kind.

The coordinator owns all facts unavailable to SQLite. Before `SealResult`, while
holding the workspace effect lease, it MUST prove that the checkout still binds
the configured numeric repository, `base_sha` and `result_commit` are commits,
the result is equal to or descends from the base under policy, `tree_oid` is the
exact result commit tree, the index/worktree is clean with no untracked files,
and every manifest entry was computed from exact Git objects for
`base_sha..result_commit`. It MUST also collect the bounded evidence set from the
exact OpenCode session/message and compute its digest. It then passes the exact
current revisions returned by discovery. `SealResult` performs no repository or
OpenCode read and MUST never be called using mutable current `HEAD` as authority.

The external-observer variant of `internal/taskresultcoord` implements this
caller boundary without inventing the
missing OpenCode fact. `RunOnce` selects only an already-durable succeeded
attempt, requires an injected observer to return the same bounded exact-session
success evidence on both `AcquireQuiesced` observations, reselects the identical
task/attempt tuple under the retained fence, collects Git, and calls
`SealResult` once before release. It exposes no polling or success-projection
method. `fern up` does not construct that observer variant because the pinned
profile has no production observer satisfying its contract; it does construct
the user-authorized variant above.

Verification request admission is one transaction. The runner claims and
commits `running`, then executes outside SQLite against the exact commit. Its
completion transaction checks the result is unchanged, stores artifacts and
digests, and records `succeeded`, `failed`, or an ambiguity state.

Migration 3 introduced the following task-store boundary:

- `PrepareVerification` binds a fresh verification ID to the immutable sealed
  result, exact successful current attempt/task ownership, exact policy name
  and SHA-256, exact verified commit, sanitized relative working directory,
  timeout/output limit, runner identity/version, image digest, and environment
  digest. It requires current task and attempt revisions and `cancel_epoch ==
  0`.
- `FindPreparedVerification`, `FindRunningVerification`, `GetVerification`, and
  `InspectVerification` are discovery and exact-read APIs. They grant no
  command authority.
- `FindResultAwaitingVerification(workspaceID)` returns the exact sealed Result,
  current Task, and current Attempt, including current task and attempt
  revisions, only when no Verification in any state exists for that Result.
  It grants no command authority.
- `AdvanceVerification` is the sole command-start fence. It requires expected
  verification/task/attempt revisions and commits `running` and
  `effect_attempt == 1` with its immutable event before execution.
- `CompleteVerification` records only `succeeded` or `failed`. It stores full
  stdout/stderr byte counts and SHA-256 digests, retained byte counts and
  truncation flags, but never stores output bytes. Success requires outcome
  `passed`, exit zero, no signal, and the sealed result commit.
- `RecoverVerification` terminally records `recovery_required`; it cannot
  authorize or represent a command retry. A started attempt includes bounded
  output accounting and an explicit non-success outcome.

Every API revalidates the immutable result and exact current task/attempt links
inside the write transaction. Every accepted insert or transition has one
immutable `journal_events` row with the same entity revision, identities,
timestamp, actor, bounded sanitized evidence object, and evidence digest. An
exact retry returns the committed row and event; changed event IDs, evidence,
tuple values, timestamps, actors, or stale revisions fail closed.

`internal/verification` now implements the isolated execution boundary: a
host-owned shell-free policy, exact clean-commit pre/postflight, explicit
environment, timeout/process-group cancellation, and independently bounded,
fully counted and SHA-256-hashed stdout/stderr. Repository mutation or inability
to repeat the exact proof is an integrity failure and is never repaired. Its
Policy exposes a detached canonical snapshot, a stable SHA-256 over all policy
fields, and a stable environment SHA-256. Runner exposes immutable name,
version, image, and exact merged-environment digest metadata. Runner creation
rejects a script, symlinked, non-regular, mutable, or unsafe-owned Git
executable. Policy creation applies the same native-binary, parent traversal,
ownership, mode, size, and content-digest checks to the configured check
executable; approved host directories remain part of the trusted host
operator boundary.

`internal/taskverification` implements the host coordinator. For each workspace
it first terminally recovers a discovered `running` Verification, then executes
a matching `prepared` Verification, then prepares and executes one Result found
by `FindResultAwaitingVerification`. It copies the constructor-supplied Policy
and Runner snapshots into preparation, commits `AdvanceVerification` before its
single `Runner.Run` call, and never reruns a started record. Prepared execution
first acquires `workspace.Manager.AcquirePaused`, reads ownership under that
fence, and retains it through the command and completion/recovery transaction.
Snapshot/result
integrity conflicts, command-start failures, preflight failures, postflight
integrity failures, and ambiguous start or completion observations after the
advance fence become `recovery_required`. Definite exit, signal, timeout, and
cancellation observations become failed Verifications; only exact exit zero
becomes succeeded. Output bytes remain process-local: SQLite and journal
evidence receive only full counts, retained counts, truncation flags, and
SHA-256 digests. The coordinator's evidence is a bounded static-schema JSON
object and contains no command output or error text.

When `tasks.verification` is configured, `fern up` constructs the host-owned
Policy and Runner snapshots, binds the repository path, workspace ID, deadline,
workspace image identity, and system/recovery actors, and runs one coordinator
loop under the service context. Omitting that block grants no verification
effect authority. Sandboxing beyond the existing process and repository
integrity boundary, artifact storage, and operator recovery remain separate
obligations.

### Publication

Publication request admission atomically validates actor authority, idempotency,
sealed result, required successful verification, and cancellation fence; then
inserts the immutable publication tuple, receipt, and event. The broker never
derives this tuple from current checkout state.

Before the first GitHub mutation, the admission transaction MUST persist
`prepared` with
operation ID, repository ID, base ref and SHA, branch, result commit, expected
remote old SHA, actor, and broker policy version. Push and pull request calls
occur outside SQLite. Every ambiguous response transitions to `uncertain` and
permits read reconciliation only. Completion is committed only after an
authoritative query proves the exact repository/branch/base/head/draft tuple.

Migration 3 introduced `PreparePublication`, `AdvancePublication`,
`CompletePublication`, and `RecoverPublication`, plus
`FindPreparedPublication`, `FindPublicationWork`,
`FindUncertainPublication`, `GetPublication`, and `InspectPublication`.
`FindPublicationWork` returns one consistent `PublicationWork` snapshot with
the current `Task`, `Attempt`, sealed `Result`, successful `Verification`,
`Publication`, and its exact latest journal event. Publication workers use the
revisions and tuples in that snapshot; they do not reconstruct publication
authority from a checkout or from separate mutable reads.
Preparation is the last admission point: before any mutation it binds the
sealed result and successful verification to the exact installation,
repository ID/full name, base ref/SHA, result commit, operation ID, generated
branch, expected old remote SHA, requesting actor, and broker policy digest.
All writes require expected publication/task/attempt revisions and revalidate
current ownership and cancellation.

The coordinator MUST treat phase authority narrowly. `push_started` authorizes
one push call only after that phase commits. A lost response is reconciled by
reading the exact branch; the coordinator records `push_observed` only when its
SHA equals `result_commit`. `pr_create_started` similarly authorizes one create
call only after commit. A lost response is reconciled by querying exact
repository and branch identities. `CompletePublication` accepts only an exact
open draft PR observation whose repository, URL, numeric PR number, base
repository/ref/SHA, head repository/ref/SHA, remote SHA, and branch all match
the immutable tuple. `uncertain` permits read-only reconciliation;
`recovery_required`, `failed`, and `conflict` are terminal and grant no mutation
authority.

`internal/taskpublication` exposes four phase-scoped broker operations:
`ReconcileBranch` is read-only; `PushOnce` invokes at most one push and then
reads the exact branch; `ReconcilePullRequest` is read-only exact discovery plus
an exact-number read; and `CreatePullRequestOnce` invokes at most one create,
then performs exact discovery and an exact-number read. The legacy
`PublishOrReconcile` composition is not a coordinator authority boundary.
`internal/taskpublicationcoord` consumes only receipt-backed prepared
publications.
It discovers work before pausing, acquires
`workspace.Manager.AcquirePaused`, re-reads identical publication, task,
attempt, result, and verification IDs/revisions, and retains the fence through
every broker call and store transition. This prevents workspace code from
changing local Git configuration around a credential-bearing push.
It commits every started phase before calling its corresponding one-shot
operation. A phase present when a pass starts grants read-only reconciliation:
in particular, startup at `push_started` never pushes and startup at
`pr_create_started` never creates. Transient or ambiguous observations remain
`uncertain` and read-only; exact contradictions become `conflict` or
`recovery_required`. No timeout or absent observation grants a mutation retry.
`fern up` constructs the repository-scoped broker, inspects journal
reconciliation before serving, and then starts one publication loop under the
service context. `POST /fern/api/v1/results/{resultId}/publications` is the only
production admission boundary. `AdmitPublication` atomically validates the
authenticated actor, idempotency claim, active App authority, current ownership
and cancellation fence, sealed changed result, exact successful verification,
and absence of another publication before deriving the tuple and inserting its
receipt, actor, journal event, and prepared publication.

Migration 6 added `publications.admission_receipt_id`. Existing rows upgraded
from schema 5 retain null rather than receiving synthetic authorization. SQL
triggers reject updates to those rows and worker discovery requires a non-null
receipt, so pre-admission publications are quarantined by construction.

## Startup Reconciliation

Startup acquires the workspace lease, reconciles Docker without waking planned
dormant compute, opens and migrates SQLite with integrity/checksum validation,
and constructs enabled coordinators before serving. Each coordinator recovers
through its durable store boundary:

- expired delivery claims become uncertain before exact read reconciliation;
- cancellation remains fenced until exact upstream proof permits
  acknowledgment;
- claimed user seals are reselected and recollected under `AcquirePaused`;
- a `running` verification becomes terminal `recovery_required` and is never
  rerun;
- publication phases already at `push_started` or `pr_create_started` authorize
  reads only, never another mutation;
- schema-6 publication discovery excludes migrated rows without an admission
  receipt.

Missing App credentials keep process liveness available for onboarding while
the fixed GitHub task dependency reports blocked and task routes remain omitted.
No timeout clears `recovery_required` or grants mutation authority.

## Reconnect Cursor

Phone clients consume Fern events, not the OpenCode volatile stream. The mounted
contract is:

```http
GET /fern/api/v1/events?after=1842&limit=100
```

```json
{
  "events": [
    {
      "id": "fev_0198d34d-6a50-75fb-b1f2-b4a14d70ec58",
      "cursor": "1843",
      "type": "attempt.admitted",
      "version": 1,
      "taskId": "tsk_0198d34d-6a50-75fb-b1f2-b4a14d70ec55",
      "attemptId": "att_0198d34d-6a50-75fb-b1f2-b4a14d70ec56",
      "occurredAt": "2026-08-22T18:57:20.584Z"
    }
  ],
  "nextCursor": "1843",
  "watermark": "1901",
  "caughtUp": false
}
```

`after` is exclusive. `limit` defaults to 100 and is at most 500. Results are
ascending. `watermark` is captured when the query starts; `caughtUp` is true
when the page reaches that watermark. An empty page returns `nextCursor` equal
to `after`. Omitting `after` means `0` for initial synchronization.

Actor and payload are intentionally omitted because arbitrary durable payloads
are not proven client-safe. The current page API does not implement retention or
a `cursor_expired` response; task list/detail snapshots are the bounded current
state projection.

## Cancellation Semantics

Cancellation is ordered by Fern's committed cancellation event cursor:

- **Before OpenCode admission:** after the cancellation transaction, Fern does
  not create or wake a session. If an ambiguous delivery exists, it reconciles
  exact IDs; a proven undelivered inbox item is deleted. The task becomes
  `canceled` only after absence or durable inbox cancellation is proved.
- **During execution:** Fern persists `cancel_requested`, fences result
  selection, verification, and publication, then calls interrupt with
  `continue=false` when the exact session is active. The UI continues to say
  "cancel requested" until exact prompt reconciliation and the active-session
  projection prove no current execution.
- **After Fern recorded `completed`:** cancel returns
  `409 task_already_terminal`; the result remains valid. There is no separate
  publication-cancellation API.
- **Completion races an unseen cancellation:** if cancellation commits before
  Fern commits task completion, the cancellation fence wins for new Fern
  effects. Late upstream success is retained as evidence, but the task becomes
  canceled or recovery-required and no result is automatically selected for
  publication.
- **Across restart:** `cancel_requested` is durable. Startup reconciles it before
  admitting any queued work or publication. It never assumes process death
  stopped the provider or tools.

No API state means "all cost stopped" unless the pinned adapter and provider
evidence establish that stronger fact. The terminal `canceled` claim means no
known execution remains and no new Fern-owned effect will start; completed side
effects are itemized.

The cancellation receipt stores one immutable coordinator disposition. A
`prepared` attempt requires no external effect and cannot later be claimed. A
  `delivering` attempt has its delivery claim fenced, retains its last delivery
  phase, and requires exact delivery reconciliation; the old owner cannot
  advance phase or record a late outcome. An `admitted`,
`running`, `input_required`, `uncertain`, or `recovery_required` attempt permits
an interrupt only when coordinator policy and current evidence allow it. A
terminal current attempt grants no external effect authority. These values are
instructions to reconcile after commit, never proof that an effect was already
performed.

The coordinator MUST satisfy the following closed proof obligation before it
calls `AcknowledgeCancellation`; its acknowledgment disposition MUST equal the
immutable receipt disposition:

| Persisted disposition | Required closed proof |
| --- | --- |
| `none_prepared` | The exact attempt was fenced while still prepared, retained delivery phase `none`, has no delivery claim, and no OpenCode session/message effect was durably started. No external call is made. |
| `reconcile_delivery` | The coordinator follows the retained delivery phase. Before prompt start, it proves the exact session is matching or absent and makes no prompt call. After `prompt_started`, it reconciles the exact session/message/inbox tuple. A proven undelivered item receives at most one delete followed by exact absence reads; an admitted item follows the active-session check described below. |
| `interrupt` | The coordinator reconciles the persisted exact session/message/inbox tuple, lists active sessions, and targets only that exact session. If active, it performs at most one `continue=false` interrupt and requires that session to be absent from a second active-session projection before acknowledgment. If already inactive, interruption is a no-op. This proves no currently projected execution, not durable terminal success. |
| `none_terminal` | The current exact attempt was already terminal when cancellation intent committed, so the coordinator acknowledges without an external call. The task fence still wins over later result selection. |

Evidence MUST be a JSON object of at most 16 KiB and MUST contain only sanitized
identifiers, states, counts, booleans, bounded error classes, and digests. It
MUST NOT contain prompts, credentials, authorization values, cookies, tokens,
or raw request/response bodies. Its SHA-256 covers the exact evidence bytes
passed to the store and both cancellation-completion events retain those bytes
and digest. A timeout, transport error, incomplete page scan, conflicting
object, uncertain delete/interrupt response, active execution, or inability to
prove exact identity is not completion: the coordinator leaves
`cancel_requested` durable and continues bounded reconciliation or enters
`recovery_required`; it MUST NOT acknowledge by inference from process death or
elapsed time.

## Repository, Result, And Publication Proof

Task admission requires all of the following:

1. Workspace configuration selects either `github-app-broker` or
   `workspace-gh` authority and names an immutable numeric repository ID and
   canonical full name. App mode also names its installation.
2. The selected authority resolves the exact remote base and validates the
   configured repository identity. Host-side Git validates the checkout and
   never treats writable `origin` as authorization.
3. `base_sha` exists as a commit, is the exact selected remote base at admission,
   uses the supported object format, and is persisted before work.
4. The initial tree is clean with no untracked files, unsupported submodules,
   unsafe alternates/replace refs/grafts, hook execution, or path escape.

A result can be sealed only when:

- `result.repository_id == task.repository_id`;
- `result.base_sha == task.base_sha`;
- `result_commit` exists locally as a commit and is equal to or a descendant of
  `base_sha` under the approved history policy;
- the worktree and index are clean and contain no untracked files;
- the manifest is computed from exact Git objects for
  `base_sha..result_commit`, not filesystem modification times;
- `no_changes` uses `result_commit == base_sha` and an empty manifest;
- the bounded OpenCode evidence projection was collected for the exact attempt
  session and its object IDs/payload hashes match the sealed evidence digest.

A verification is publication-eligible only when it succeeded under the current
verification policy and
`verified_commit == result.result_commit`. A publication is eligible only when:

```text
Publication.repository_id == Workspace.repository_id == Task.repository_id
Publication.base_sha      == Task.base_sha == Result.base_sha
Publication.result_commit == Result.result_commit == Verification.verified_commit
Publication.remote_sha    == Publication.result_commit
PullRequest.repository_id == Publication.repository_id
PullRequest.repository_full_name == Publication.repository_full_name
PullRequest.number is a positive persisted GitHub PR number
PullRequest.url == canonical URL for repository_full_name and number
PullRequest.state == open
PullRequest.base_repository_id/full_name == Publication repository identity
PullRequest.base_ref       == Publication.base_ref
PullRequest.base_sha       == Publication.base_sha
PullRequest.head_repository_id/full_name/owner/name == Publication repository identity
PullRequest.head_ref       == Publication.branch
PullRequest.head_sha       == Publication.result_commit
PullRequest.is_draft       == true
```

Any mismatch is `conflict` or `recovery_required`; it is never repaired with a
force push. Base branch movement after task admission does not rewrite
`base_sha`. Policy may require a new task or explicit rebase attempt, preserving
the old result as historical evidence.

## Actor Attribution And Authorization

An actor snapshot has this exact shape:

```json
{
  "type": "device",
  "id": "device-id-or-service-principal",
  "displayName": "Noah's phone",
  "credentialId": "credential-version-id",
  "authentication": "fern_device_cookie",
  "requestId": "server-generated-request-id"
}
```

Allowed actor types are `device`, `operator`, `system`, `opencode`,
`github_app`, and `recovery`. Actor identity comes only from authenticated
server context, never a request body or forwarded client header. Background
events retain the initiating actor where applicable and also record the system
worker actor that performed the transition.

Current mounted capability is:

| Capability | Device | Operator | System/recovery | GitHub App |
| --- | --- | --- | --- | --- |
| List/read own workspace tasks and results | yes | yes | yes | no |
| Submit and cancel a task | yes | yes | reconciliation only | no |
| Answer a current approval | no | no | no durable approval API | no |
| Request verification | no | no | configured policy automation only | no |
| Request eligible App publication | yes | yes | reconcile existing only | no |
| Push/query PR | no | no | call broker only | repository-scoped effect only |

The current shared operator Basic credential can attribute an action only to a
named credential version, not a natural person. The API MUST state that
limitation. Supported multi-operator attribution requires distinct principals;
Fern MUST NOT infer a person from device name, IP address, or Git commit author.

## Fern HTTP API V1

The implemented surface includes task list/submit/read, cancellation, user-seal
preview/request, App publication admission, and task event reads:

```text
GET  /fern/api/v1/tasks?limit=<n>
POST /fern/api/v1/tasks
GET  /fern/api/v1/tasks/{taskId}
POST /fern/api/v1/tasks/{taskId}/cancel
POST /fern/api/v1/tasks/{taskId}/seal-preview
POST /fern/api/v1/tasks/{taskId}/seal
POST /fern/api/v1/results/{resultId}/publications
GET  /fern/api/v1/events?after=<cursor>&limit=<n>
```

Approval and standalone verification endpoints later in this section are target
contracts, not mounted routes. App publication admission is mounted when that
authority mode and task policy are active.

All endpoints are under `/fern/api/v1`, require private TLS and an actor resolved
only from server-owned request context, reject unknown, duplicate, and
case-aliased JSON fields, reject trailing JSON data and invalid UTF-8, and accept
exactly `Content-Type: application/json` on POSTs. The transport bound permits a
64 KiB prompt after worst-case JSON string escaping; semantic field bounds are
applied after decoding. Same-origin enforcement belongs to the outer proxy. IDs
are path parameters, never body-overridable. Workspace, repository, base SHA,
agent/model, budget, deadline, API/execution versions, timestamps, and actor are
server-generated or server-configured.

Errors use:

```json
{
  "error": {
    "code": "idempotency_conflict",
    "message": "The idempotency key was already used for different input.",
    "requestId": "req_..."
  }
}
```

`message` is safe for display and contains no prompt, token, host path, or
credential. Stable behavior is keyed by `code`. State-changing responses include
`ETag: "<revision>"` where useful.

### Submit Task

```http
POST /fern/api/v1/tasks
Idempotency-Key: phone-command-01892
```

```json
{
  "title": "Fix signup",
  "prompt": "Fix signup and run the approved checks.",
  "baseRef": "main"
}
```

First acceptance and an idempotent replay both return `202` and the same stable
acceptance projection. A replay additionally sets `Idempotency-Replayed: true`;
it does not wake workers again. The response contains the bounded receipt and a
stable queued task acceptance projection: task/workspace IDs, title, repository
ID, base ref/SHA, current attempt ID, zero cancellation epoch, latest acceptance
event cursor, revision 1, and receipt acceptance timestamps. It contains no
prompt or OpenCode transcript. The acceptance projection does not advance when
workers later mutate the task, so a replay remains byte-stable.

Repository or base mismatch is `409 repository_mismatch` or
`409 base_moved`; Fern returns the observed display identity/SHA only when the
actor may read that workspace.

### List And Read Tasks

```http
GET /fern/api/v1/tasks?limit=50
GET /fern/api/v1/tasks/{taskId}
```

The list response is `{"tasks":[...]}` with at most the requested number of
current snapshots; there is no list cursor. List and detail use the same closed
snapshot: task, exact current attempt with same-origin `openCodePath`, latest
seal request or null, sealed result or null, ordered verification summaries,
and current publication/PR summary or null. It omits prompts, OpenCode
transcripts, event payloads, raw evidence, command output, credentials, and
diffs.

### Cancel Task

```http
POST /fern/api/v1/tasks/{taskId}/cancel
Idempotency-Key: cancel-01892

{"reason":"User requested cancellation"}
```

Returns `202` with the safe receipt plus task ID, current attempt ID, cancel
epoch, and effect disposition. A terminal task returns `409
task_already_terminal`. `reason` is optional and bounded to 500 UTF-8 bytes. An
empty body is equivalent to `{}`; a POST still requires the exact JSON content
type. Cancellation allocates its receipt and event IDs before calling the atomic
store operation, wakes only after a fresh commit, and uses the same stable-`202`
replay behavior as submission.

### Read Events

```http
GET /fern/api/v1/events?after=7&limit=100
```

`after` is an optional canonical non-negative decimal cursor and `limit` is 1 to
500 (default 100). Unknown or repeated query parameters are rejected. The page
contains event ID, decimal-string cursor, task/attempt IDs, type, version, and
server timestamp plus `nextCursor`, `watermark`, and `caughtUp`. Actor snapshots
and event payloads are omitted because this API has no per-event proof that an
arbitrary durable payload is client-safe.

### Read And Decide Approval

```http
GET /fern/api/v1/tasks/{taskId}/approvals
GET /fern/api/v1/approvals/{approvalId}
POST /fern/api/v1/approvals/{approvalId}/decision
Idempotency-Key: approval-01892
```

Permission decision:

```json
{
  "expectedContextHash": "sha256:...",
  "decision": { "reply": "once", "message": null }
}
```

Question decision:

```json
{
  "expectedContextHash": "sha256:...",
  "decision": { "answers": [["Option label"], ["Free-form answer"]] }
}
```

Form decision:

```json
{
  "expectedContextHash": "sha256:...",
  "decision": { "answer": { "fieldKey": "value" } }
}
```

The decision variant MUST match approval kind and the exact options/schema
captured from OpenCode. Returns `202` with receipt and approval. Stale context is
`409 stale_approval`; a settled approval is `409 approval_already_terminal`.

### Read Result And Request Verification (Target Contract)

```http
GET /fern/api/v1/results/{resultId}
POST /fern/api/v1/results/{resultId}/verifications
Idempotency-Key: verify-01892

{"check":"required"}
```

Result response includes immutable repository/base/commit values, outcome,
manifest digest and paginated changed-file summaries, exact attempt/OpenCode
IDs, projected-evidence digest, and verification/publication summaries. Verification
acceptance returns `202` with receipt and verification. The server resolves
`check` to policy-owned argv; no command or environment is accepted from the
client.

### Request And Read Publication

```http
POST /fern/api/v1/results/{resultId}/publications
Idempotency-Key: publish-01892
```

```json
{
  "expectedVerificationId": "ver_0198d34d-6a50-75fb-b1f2-b4a14d70ec5b"
}
```

The body is a closed object and the actor cannot provide title, body, repository,
base, branch, commit, operation ID, path, remote, policy, or credential. Fern
derives the complete tuple from configured App authority, the result, and the
named successful verification. Returns `202` with:

```json
{
  "receipt": {
    "id": "rcp_0198d34d-6a50-75fb-b1f2-b4a14d70ec5e",
    "kind": "result.publish",
    "state": "accepted",
    "acceptedAt": "2026-08-22T19:10:00.000Z",
    "targetId": "pub_0198d34d-6a50-75fb-b1f2-b4a14d70ec5c"
  },
  "publication": {
    "id": "pub_0198d34d-6a50-75fb-b1f2-b4a14d70ec5c",
    "resultId": "res_0198d34d-6a50-75fb-b1f2-b4a14d70ec5a",
    "verificationId": "ver_0198d34d-6a50-75fb-b1f2-b4a14d70ec5b",
    "state": "prepared",
    "createdAt": "2026-08-22T19:10:00.000Z"
  }
}
```

An exact same-key/body/actor replay returns the same projection and
`Idempotency-Replayed: true`. Current publication state and the exact draft PR
summary are read through task list/detail snapshots; there is no standalone
publication read or cancellation route.

## GitHub App Broker Boundary

The GitHub App lane may implement credentials, onboarding, and API calls in
parallel behind this internal interface:

```text
PreparePublication(
  operation_id,
  installation_id,
  repository_id,
  base_ref,
  base_sha,
  result_commit,
  branch,
  expected_remote_old_sha,
  title,
  body,
  actor_id,
  deadline
) -> PreparedObservation

PublishOrReconcile(PreparedObservation) -> PublicationObservation
```

The broker receives immutable values; it does not read Task or Result tables and
does not choose them. It returns observations, never direct database mutations.
The host transport uses the repository-scoped installation token directly; it
does not expose or inherit an operator `gh` credential. For Git HTTPS push, the
token is supplied by a one-use askpass executable from a mode-0700 temporary
directory through a mode-0600 credential file. The credential file is removed
on first password read and the complete directory is removed when Git exits.
Subprocess evidence retains only bounded byte counts and SHA-256 digests, never
argv, environment, output bytes, or credentials.
The coordinator alone transitions `Publication`. Broker implementation MUST:

- resolve authorization by numeric installation and repository IDs;
- mint a fresh repository-scoped installation token in host memory;
- disable hooks and untrusted Git configuration;
- use exact SHA refspecs and expected remote old SHA, never force;
- query the exact remote branch after ambiguous push;
- find/query the exact repository, head, base, and draft PR before create and
  after ambiguous create;
- return numeric PR identity and exact observed head/base SHA;
- never expose a token to SQLite, logs, command arguments, repository config,
  Docker, OpenCode, or API clients.

This frozen input/output boundary lets App JWT, token storage, onboarding, and
GitHub fixtures proceed without depending on mobile HTML, task delivery, or
OpenCode internals.

## Mobile UI Boundary

The production phone task page owns presentation, polling, accessibility, and
deep links. Before task submission it stores one exact body and idempotency key
under `fern.pending-task-submission.v1` in `localStorage`, reuses both after a
lost response or reload, and removes them only after acceptance. It refuses to
send if durable browser storage is unavailable. Cancellation and sealing use a
fresh key for each explicit invocation; other clients must retain their keys to
obtain the backend replay guarantee. Mobile work MUST NOT:

- add task, attempt, approval, result, verification, or publication states;
- infer completion from absence of live events;
- consume OpenCode `/api/event` as a durable cursor;
- submit repository paths, remotes, branches, commits, verification commands,
  credentials, actor IDs, or OpenCode IDs;
- render `cancel_requested` as `canceled`, `uncertain` as failed, or
  `recovery_required` as automatically retrying;
- reproduce transcript, tools, permissions, questions, forms, terminals, files,
  or diffs already owned by OpenCode.

The UI can ship task inbox/detail, pending-input summaries, exact commit and
verification summaries, publication status, reconnect behavior, and links to
the official OpenCode session independently of the delivery worker. Fixture
changes require a contract change owned by the task-model/migration lane.

## Migration Authority

`internal/taskstore` is the sole owner of task SQLite schema migrations and
state transitions. Migrations use `PRAGMA user_version`, run under the workspace
lease and an exclusive migration lock, and are transactional. A binary supports
an explicit contiguous schema range and refuses unknown newer versions.
Migrations do not call OpenCode, GitHub, Docker, providers, or verification
commands.

`internal/taskstore` implements schema 10 with CGO-free SQLite, private path
checks, a checksum-pinned migration ledger, foreign keys, WAL with
`synchronous=FULL`, receipt-backed task and publication admission, fenced
coordinator journals, exact OpenCode IDs, results, verification, and
publication records. Background Run intent includes the exact disposable
environment digest; schema-8 rows migrate conservatively as the empty explicit
environment and retain resource-spec version 8 for cleanup-only attestation.
Admission and replay are wired through the production task
API. Migration 6 quarantines unresolved unreceipted legacy publication rows so
they grant no worker authority. Migration 8 similarly terminalizes unqualified
schema-7 Background Runs before adding exact effect claims and cleanup proof.

The JSON control store remains a separate compatibility authority for legacy
control-plane records; SQLite is authoritative for Fern tasks and their
receipts/effects. They do not both own the same task entity. Offline backup and
restore preserve both stores, task SQLite/WAL state, OpenCode data, Git objects,
managed volumes, configuration, and the appliance epoch under one manifest.
Rollback means restoring the verified pre-upgrade bytes; older binaries must
not open a migrated schema-10 database. See [Deployment](./DEPLOYMENT.md) and the
`integration/upgrade` harness.

## Fault-Injection Acceptance

Each implemented tranche is accepted only when tests capture durable database
rows, Fern events/cursors, exact upstream IDs, authoritative Git/GitHub
observations, and user-visible state. Package tests alone are insufficient.

### Admission And Delivery

1. Kill Fern after admission commit and before wake. Restart returns the same
   receipt/task/attempt/OpenCode IDs and delivers at most one exact message.
2. Drop the task-submit HTTP response and retry the same idempotency key. One
   receipt, task, and attempt exist; retry returns the same IDs.
3. Reuse the key with one changed prompt byte. Fern returns
   `idempotency_conflict` and neither prompt is disclosed.
4. Lose the OpenCode session-create response. Exact GET and retry behavior prove
   one session with the persisted ID.
5. Lose the prompt response before, during, and after upstream durable commit.
   Reconciliation proves absence or one exact inbox/message; no second ID or
   provider turn appears.
6. Reuse exact OpenCode IDs with a changed payload. The adapter fails closed and
   enters `recovery_required` without delivering either as a Fern success.
7. Disconnect the phone after `202`. Work continues under the service-owned
   worker, and reconnect shows the same task.
8. Run two workers and expire one lease mid-delivery. Revision/lease fencing
   permits one committer and exact-ID reconciliation prevents duplicate work.

### Durable Log And Reconnect

9. Disconnect volatile `/api/event` during execution. Reconnection and finite
   projections may recover positive current activity or input-required state,
   but never infer terminal success from the gap.
10. Kill OpenCode or replace the container while process-epoch permission,
    question, or form input is pending. Fern enters `recovery_required` rather
    than reconstructing or auto-answering the missing object.
11. Return malformed, unauthorized, incomplete, or contradictory finite
    projections. Fern keeps compute running and records no positive transition
    that the projections do not prove.
12. Disconnect between a finite scan and event subscription. A newly observed
    positive state may be projected, but the volatile stream grants no durable
    cursor and no absence-based transition.
13. Restart Fern after committing Fern events but before responding to the phone.
    The phone's exclusive Fern cursor returns each persisted event once in
    order; duplicate retrieval is harmless.
14. Request any valid old Fern cursor. Because retention and cursor expiry are
    not implemented, the API pages persisted events up to its captured
    watermark; snapshots remain the bounded current-state projection.

### Target Approval And Implemented Cancellation

Items 15-17 are future approval acceptance tests; no production approval table
or decision route exists. Items 18-20 apply to current cancellation.

15. Kill Fern after decision commit and before OpenCode reply. Restart delivers
    or reconciles the exact pending request once and preserves deciding actor.
16. Lose permission, question, and form reply responses. Exact upstream request
    state/event evidence resolves `applied` or `uncertain`; no alternate answer
    is sent.
17. Cancel or change context before an old approval decision. Context hash
    rejects it visibly and no upstream reply occurs.
18. Cancel before session creation, while message is undelivered, during a
    provider turn, immediately before terminal execution event, and after Fern
    completion. Each follows the documented fence and terminal ordering.
19. Kill Fern after cancellation commit but before interrupt. Startup processes
    cancellation before any new effect and does not report `canceled` without
    evidence.
20. Continue provider cost or a side-effecting tool after interrupt. UI remains
    `cancel_requested` or enters recovery; it never falsely claims rollback or
    stopped cost.

### Result And Verification

21. Crash after a tool writes but before Fern observes it. Recovery compares Git
    objects/worktree and enters `uncertain` or seals only after synchronized
    OpenCode evidence; it never claims the old tree.
22. Leave staged, modified, or untracked files; retarget origin; add replace
    refs, unsafe alternates, submodules, or unsupported object format. Result or
    publication preflight fails closed.
23. Mutate `HEAD`, index, worktree, or a Git object between collection phases.
    Lease/revalidation prevents sealing mismatched evidence.
24. Kill verification on timeout, signal, process crash, host reboot, and disk
    full. Logs are bounded/digested, state is recoverable, and no success is
    recorded without exact commit and zero exit.
25. Verify commit A, then change to commit B. Publication of B is rejected until
    a Result and required Verification both name B.

### Publication

26. Kill after publication `prepared` commit and before push. Startup uses the same
    immutable tuple and operation ID.
27. Lose push response before and after GitHub updates the ref. Exact
    `ls-remote` reconciliation records one matching ref or a conflict; no force
    or second branch is used.
28. Lose draft-PR create response. Exact repository/head/base query finds one
    draft PR before any retry.
29. Return a PR with wrong repository ID, base, head, commit, draft state, or
    number. Publication never reaches `published`.
30. Human-push a different commit, move/delete the Fern branch, close/merge the
    PR, or move the base. Fern records `conflict` and never overwrites remote
    state.
31. Race task cancellation against result selection and publication admission.
    A committed task cancellation prevents new publication admission. Once a
    publication mutation has started, ambiguous or finished effects are
    reconciled and shown, not repeated, deleted, or mislabeled canceled.
32. Revoke/rotate the installation during each stage. Fresh credentials are
    required; local result remains intact and publication blocks or reconciles
    read-only.

### Startup, Migration, And Recovery

33. Power-cut at every SQLite transaction boundary and external-effect boundary.
    Restart reaches one listed state, retains accepted receipts, and performs no
    unjournaled mutation.
34. Start from paused, idle-running, active-attempt, input-required,
    cancel-requested, verifying, and each publication state. Reconciliation
    completes before mutating APIs open and paused compute does not wake without
    need.
35. Corrupt SQLite pages, foreign keys, WAL, lifecycle intent, migration ledger,
    or backup manifest. Fern fails closed and preserves evidence for repair.
36. Fill disk during receipt/event commit, projection reconciliation, artifact
    write, and migration. No external effect starts unless its intent transaction
    was durable; partial artifacts are not successful records.
37. Crash at every schema migration and legacy-publication quarantine boundary.
    The migration transaction either commits one valid ledger/schema version or
    rolls back; an unresolved unreceipted publication never gains authority.
38. Restore onto a fresh host with expired credentials and a new appliance
    epoch. State remains inspectable, old-host effects are fenced, and all
    nonterminal operations re-preflight authorization and exact identities.

## Release Gates

The current release gates are:

- `OC-WIRE`, `OC-ID`, `OC-LOG`, `OC-DELIVERY`, and `OC-CANCEL` observations must
  continue to pass against the exact pinned image digest. The release does not
  reinterpret the missing durable log as a success signal or claim provider
  rollback after interruption.
- Generic automatic success remains blocked until an authoritative observer can
  prove it. User-authorized sealing is a separate authority and records the
  attempt as `superseded`, not `succeeded`.
- Approval mutation remains future work. It cannot ship until permission,
  question, and form decisions are restart-safe and lost-response reconcilable;
  current process-epoch objects do not meet that gate.
- App publication requires numeric repository authorization, exact base SHA,
  one sealed changed Result, successful exact-commit Verification, a matching
  admission receipt, and post-mutation exact branch/PR proof. Workspace-`gh`
  effects remain explicitly outside Fern's receipt journal.
- Schema and state vocabulary retain one owner. API and UI code consume typed
  task-store transitions rather than introducing shadow stores or states.
