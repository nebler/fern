# Fern Architecture

This document describes the implemented Fern system, its authority boundaries,
durable state machines, recovery rules, and known gaps. It is written from the
production composition root and checked-in tests. Where this document and code
diverge, code and tests are authoritative.

## 1. Product Scope And Current Status

Fern is a single-host, single-owner Go service that supervises one durable
OpenCode workspace in local Docker. It gives a paired phone access to the
official OpenCode UI and to Fern's durable task API while keeping operator
controls and backend credentials on a separate loopback surface.

Fern does not replace or fork the OpenCode UI. OpenCode remains authoritative
for sessions, prompts, tools, files, diffs, terminals, provider interaction,
permissions, forms, and its client protocol. Fern owns the host lifecycle,
admission, durable command intent, repository and GitHub authority, verification
policy, publication journals, and recovery decisions.

The implementation has three distinct status classes:

| Class | Capabilities |
| --- | --- |
| Production-wired | Configuration, two ingress policies, pairing, wake/pause, Docker ownership, task admission, delivery, cancellation, conservative execution observation, optional verification coordination, GitHub App onboarding, repository-scoped publication reconciliation |
| Implemented but authority-gated | Store-level execution success/failure, explicit quiesced result coordinator, read-only Git result collection, immutable result sealing, durable publication preparation |
| Not implemented end to end | Generic OpenCode terminal-result proof, durable remote approval answers, idempotent phone publication admission, notifications, GitHub installation-selection UI, credential backup/rotation |

The ordinary durable task path currently reaches this boundary:

```text
phone submit
    |
    v
durable admission -> exact OpenCode delivery -> running/input_required
                                               |             |
                                               |             +-> cancellation/recovery
                                               |
                                               +-> generic terminal success is not provable
```

Verification and durable publication coordinators are real and wired. They do
not make upstream facts true: verification requires a sealed result, and
publication requires an explicitly prepared publication. Ordinary production
traffic currently creates neither.

The root blocker is the pinned OpenCode profile. It has no durable generic
terminal-success object and no durable event cursor. An inactive session, empty
inbox, idle status, or missing process-epoch form is not proof that work
succeeded. Fern fails closed rather than turning absence into success.

## 2. Authority Model

Fern treats authority as a set of narrow, non-interchangeable sources:

| Concern | Authority |
| --- | --- |
| Desired workspace configuration | Strict Fern configuration |
| Container identity and observed process state | Docker, revalidated by Fern |
| OpenCode UI, session and tool behavior | OpenCode |
| Durable task intent and audit history | Fern SQLite task store |
| Legacy devices, workflows and publications | Fern JSON control store |
| Repository objects, ancestry and clean state | Host Git object database and index |
| Repository identity, refs and pull requests | GitHub numeric identities and authoritative API reads |
| Verification command and environment | Host-owned Fern policy |
| Verification outcome | One fenced Fern runner invocation |
| Publication mutation authority | A committed durable publication phase |
| Device access | Digest-backed Fern device grant |
| Operator access | Loopback listener plus Fern/OpenCode Basic credentials |

Several values are consistency evidence but never authorization:

- A writable checkout `origin` cannot select a GitHub repository.
- A Docker image tag cannot substitute for the inspected immutable image ID.
- Browser `Host`, `Forwarded`, and `X-Forwarded-*` input cannot select the
  effective public authority.
- OpenCode inactivity cannot select a successful task outcome.
- Current checkout `HEAD` cannot replace an immutable result tuple.
- A request body cannot select actor identity, model policy, verification
  command, installation, or repository.

## 3. Topology And Trust Boundaries

```text
private TLS edge / Tailscale Serve                 local operator and CLI
                 |                                          |
                 v                                          v
       remote/device listener                     operator listener
       numeric loopback only                       numeric loopback only
       device-cookie policy                        Basic-auth policy
                 |                                          |
                 +------------ Fern router -----------------+
                                      |
                          request admission and wake
                                      |
                           dynamic loopback Docker port
                                      |
                            pinned OpenCode V2 server
                           UID/GID 1001:1001, port 4096
                               |                 |
                    repository bind mount   OpenCode data volume
```

The remote listener is the only supported target for private HTTPS exposure.
The operator listener defaults to `127.0.0.1:8081` and must remain host-local.
Fern does not infer listener policy from source IP or forwarding headers.

The supported trust model assumes one trusted owner, host, repository, image,
Docker daemon, and tailnet. Docker access is effectively root-equivalent.
Repository code and OpenCode are trusted to execute in the workspace boundary
and may receive explicitly forwarded provider credentials. Fern is not a
hostile multi-tenant sandbox.

## 4. Process Composition

`cmd/fern/up.go` is the production composition root. One `fern up` process owns:

- one host-local workspace lease;
- one Docker client and one workspace manager;
- one OpenCode activity stream controller and idle supervisor;
- one remote HTTP server and one operator HTTP server;
- one legacy JSON control store;
- optionally, one SQLite task store and task coordinator set;
- optionally, one legacy host-credential publication coordinator;
- optionally, one GitHub App onboarding handler.

GitHub authority is explicit and durable. `mode: github-app-broker` requires a
positive installation ID and constructs the App task publisher. `mode:
workspace-gh` forbids an installation ID, constructs the bounded managed-`gh`
executor for task base resolution, and does not construct the App publisher.
The legacy host publisher remains separate from both task authority modes.

### 4.1 Startup Order

`fern up` performs these operations in order:

1. Parse flags and the optional protected environment file.
2. Load defaults, strict YAML, environment references, and explicit overrides.
3. Merge only approved environment-file values into the workspace environment.
4. Validate configuration and parse the memory limit.
5. Bind both loopback listeners before any Docker side effect.
6. Create the shared signal and errgroup context.
7. Acquire the exclusive workspace lease under `$HOME/.fern/locks`.
8. Select the legacy publisher only when no explicit task GitHub authority is configured.
9. Open the local Docker client, pause-intent state, and legacy control store.
10. Construct the stream controller and workspace manager.
11. Reconcile existing Docker state without waking absent or paused compute.
12. Construct GitHub App onboarding only for App-broker mode when a remote
    origin exists and credentials do not.
13. Construct task services when the task policy and either explicit GitHub
    authority mode exist.
14. Validate App authority eagerly, or construct the managed workspace-`gh`
    resolver for live token-free validation at task admission, before persisting
    the explicit durable workspace authority binding.
15. Construct the two policy-separated proxy handlers.
16. Reconcile interrupted legacy publications.
17. Drain available durable publication journal work before serving HTTP.
18. Start the supervisor, task loops, and both HTTP servers in one errgroup.

Binding listeners first makes an invalid or occupied address a side-effect-free
failure. The exclusive lease prevents a second lifecycle writer. Errors after
manager startup close publication workers and the manager before returning.

If task policy is configured but App credentials are absent, Fern can still
start the operator onboarding surface when `proxy.remoteOrigin` is configured.
Task services remain disabled until the operator completes onboarding and
restarts Fern. Missing credentials without an onboarding path fail startup.

### 4.2 Shutdown Order

A signal or service error cancels the shared context. Fern then:

1. Gives both HTTP servers a bounded graceful shutdown window.
2. Force-closes tracked connections if graceful shutdown fails.
3. Waits for errgroup-owned servers and task coordinators.
4. Closes the legacy publication coordinator.
5. On an actual process signal, records a bounded Docker shutdown intent.
6. Closes the workspace manager and waits for admitted requests, pauses, wakes,
   and rollback ownership.
7. Stops the OpenCode activity stream.
8. Closes the SQLite store and Docker client through deferred cleanup.

The manager closes before Docker because it owns wake and rollback goroutines.
If bounded manager shutdown times out, Fern deliberately leaves the Docker
client open until process exit rather than closing it underneath still-owned
goroutines.
Stopping Fern does not intentionally stop OpenCode. A short-lived shutdown
intent lets Docker stop the exact container shortly afterward without that exit
being misclassified as an unexplained crash.

## 5. Configuration And Secret Flow

Configuration is defined in `internal/config/config.go`.

### 5.1 Precedence And Parsing

```text
explicit CLI flag > YAML > defaults
```

YAML uses known-field checking and accepts one document. Repository paths in
YAML resolve relative to the configuration file. The `-repo` override resolves
relative to the caller's working directory.

Default values include:

| Setting | Default |
| --- | --- |
| Workspace | `demo` |
| Image | `fern/opencode:dev` |
| Memory | `8Gi` |
| Idle timeout | `10m` |
| Remote listener | `127.0.0.1:8080` |
| Operator listener | `127.0.0.1:8081` |

Both listeners must be distinct numeric loopback addresses. A configured
`proxy.remoteOrigin` must be one canonical HTTPS root with no userinfo, path,
query, fragment, explicit default port, trailing-dot hostname, or trailing
slash.

### 5.2 Credentials

OpenCode Basic uses username `opencode` and `OPENCODE_PASSWORD`. Fern operator
Basic uses username `fern` and `FERN_CONTROL_PASSWORD`. The control password
must be at least 32 characters and cannot equal any forwarded workspace value.

Only these environment-file values are automatically eligible for forwarding:

- `ANTHROPIC_API_KEY`
- `OPENAI_API_KEY`
- `OPENCODE_PASSWORD`

Fern rejects host-only credential names and aliases that reference or expand to
them, including:

- `FERN_CONTROL_PASSWORD`
- `FERN_GITHUB_TOKEN`
- `GH_TOKEN`
- `GITHUB_TOKEN`

The environment file must be a real regular file, not a symlink, with no access
beyond owner and optional group read. Expanded workspace environment is part of
the desired Docker specification. Secret rotation therefore requires explicit
container recreation while preserving the OpenCode volume.

### 5.3 Task Policy

Task mode requires an explicit agent, provider, model ID, attempt timeout, lease
duration, turn budget, GitHub installation ID, numeric repository ID, and exact
canonical repository full name.

Optional verification policy contains:

- a bounded lowercase check name;
- a shell-free argument array;
- an absolute clean executable path;
- a repository-relative working directory;
- an explicit timeout;
- a bounded exact environment map;
- an output byte cap.

The check executable must be an immutable native host binary, not a script or
symlink. Its ownership, parent directories, mode, size, and SHA-256 are checked,
and its resolved path must be outside the writable workspace repository.
Reserved runner environment keys cannot be overridden. No verification command
is inferred from repository content.

## 6. Ingress, Authentication And Routing

Production constructs `proxy.NewHandlers`, which returns distinct remote and
operator policies. Fern-owned routes are handled before workspace admission and
do not wake compute.

### 6.1 Remote Listener

The remote listener accepts without a device cookie only:

- `/fern/pair` for pairing preview and consumption;
- `/fern/github/app/callback`, authorized by one-use callback state.

With a valid paired-device grant it accepts:

- the official OpenCode UI and API;
- `/fern/` and Fern readiness;
- the task UI;
- task submission, reads, cancellation, and event reads.

It rejects Fern Basic, OpenCode Basic, pairing issuance, operator controls,
device administration, legacy workflows, and legacy publication. Credentials
are rejected before wake.

### 6.2 Operator Listener

Fern routes require `fern:<control password>`. Upstream OpenCode routes require
`opencode:<OpenCode password>`. Pairing issuance and GitHub App setup exist only
on this surface.

The operator listener also supports the official OpenCode CLI. It must not be
used as a supported OpenCode browser origin after an operator has entered Fern
Basic credentials; doing so would recreate a same-origin confused-deputy risk.

### 6.3 Credential Translation

After successful authentication Fern:

1. strips incoming `Authorization` and Fern device cookies;
2. creates a server-owned actor snapshot for Fern task commands;
3. generates canonical backend OpenCode Basic for upstream traffic;
4. prevents upstream OpenCode from setting Fern's current or reserved future
   device-cookie names.

Actor identity cannot come from client headers or JSON.

### 6.4 Forwarding Authority

Fern removes `Forwarded` and all case-insensitive `X-Forwarded-*` input. It sets
the upstream host, scheme, and effective port from trusted listener
configuration. It does not generate `X-Forwarded-For`. Browser `Origin` remains
available for mutation checks.

Fern-owned browser mutations use exact-origin and Fetch Metadata checks.
Cross-site and same-site sibling requests fail closed. Per-form CSRF tokens are
not implemented.

### 6.5 Pairing And Revocation

Pairing codes contain 256 random bits and expire after five minutes. GET only
previews; POST atomically consumes the code and creates a 30-day grant. Fern
caps outstanding codes and stores only a SHA-256 device-token digest.

Device cookies are `Secure`, `HttpOnly`, and `SameSite=Strict`. A successful
revocation is persisted before active request contexts for that device are
canceled. Active request contexts also inherit the grant expiry deadline, so a
stream cannot outlive its durable grant.

The standard reverse proxy closes upgraded connections abruptly on context
cancellation. Real OpenCode PTY/WebSocket close behavior remains an acceptance
gap.

## 7. Request Admission, Wake And Endpoint Identity

The proxy classifies upstream requests conservatively:

| Intent | Typical routes | Wakes | Holds admission | Invalidates idle evidence |
| --- | --- | --- | --- | --- |
| Observe | OpenCode lifecycle SSE | No | No | No |
| Read | exact health and activity reads | Yes | Yes | No |
| Work | UI, unknown, mutation, upload, upgrade | Yes | Yes | Yes |

Unknown routes default to work so future OpenCode endpoints cannot accidentally
bypass wake or idle invalidation.

`workspace.Manager` separates endpoint synchronization, lifecycle
serialization, and request admission. It:

- coalesces concurrent wake requests;
- does not let one canceled waiter cancel a wake needed by others;
- serializes wake, pause, and quiesce operations;
- publishes only an attested endpoint and immutable image ID;
- assigns monotonically increasing endpoint generations;
- invalidates only the exact failed generation.

A cached endpoint avoids a Docker inspection on every request. Transport
failure invalidates by endpoint and generation. A reachable upstream HTTP error
does not.

## 8. Activity And Idle Shutdown

The OpenCode event stream is a volatile scheduling signal, not a durable task
log. The stream controller assigns an epoch to each backend generation and
serializes connected, disconnected, status, malformed-status, and work-request
observations through the supervisor.

An epoch becomes pause-eligible only after observing busy/retry activity and a
subsequent drain of all tracked sessions to idle. Disconnects, malformed or
unknown states, relevant work requests, and stale epochs invalidate eligibility.
Starting Fern against an already idle process does not arm the timer.

When the timer expires, the manager performs an authoritative pause barrier:

1. Refuse pause while a held request exists.
2. Close admission to new held requests.
3. Serialize with wake and lifecycle operations.
4. Reinspect Docker and the current endpoint.
5. Perform a complete authenticated activity pass.
6. Repeat the complete pass while admission remains closed.
7. Stop only if both passes are entirely idle.

Each activity pass checks sessions, shells, PTYs, pending permissions, forms,
and questions. Unknown or unavailable observations defer pause. These reads are
sequential rather than atomic; the second pass catches activity that begins
during the first pass.

### 8.1 Quiesced Result Fence

`workspace.Manager.AcquireQuiesced` is stronger than ordinary pause. It:

- closes request admission;
- verifies one exact running endpoint and image;
- invokes a caller-owned exact observation twice;
- performs an all-idle pass after each observation;
- stops compute;
- retains lifecycle and repository exclusion until explicit release.

Any result coordinator must hold this fence through Git collection and the
`SealResult` transaction. Idle checks alone cannot serve as terminal success
evidence.

## 9. Docker Runtime And Ownership

Fern supports the default local Docker endpoint or an absolute Unix socket.
Ordinary TCP and SSH Docker endpoints are rejected because the design relies on
host-local bind mounts, locks, loopback publication, and pause-intent state.

### 9.1 Fixed Container Policy

The workspace container uses:

- UID/GID `1001:1001`;
- dynamic loopback publication of container port 4096;
- configured memory;
- two-CPU quota;
- 512 PID limit;
- Docker init;
- all capabilities dropped;
- `no-new-privileges`;
- no added devices;
- no restart policy;
- writable repository bind at `/home/user/workspace`;
- writable OpenCode volume at `/home/user/.local/share/opencode`.

The root filesystem is writable. These controls reduce accidental privilege but
do not make untrusted repository code a host-security sandbox.

The image runs:

```text
opencode2 serve --hostname 0.0.0.0 --port 4096
```

The supported OpenCode version is `0.0.0-next-17444`. The characterized
development image identity is
`sha256:73688cd6f96ce3b236bb1c2d25607b03566a4ee92f0fedabeb06fd1a3e643c6c`;
the contract harness verifies both version and digest.

### 9.2 Ownership And Drift

Fern labels every managed container and volume. Before reuse it validates:

- management and workspace labels;
- the actual immutable Docker image ID;
- configured image reference;
- user, resources, init, restart, and security settings;
- exact bind and volume mounts;
- loopback port binding;
- required environment values;
- desired-spec fingerprint.

Drift is rejected rather than silently adopted. Container names, image tags,
and labels do not replace immutable container/image inspection.

### 9.3 Runtime States

| State | Meaning |
| --- | --- |
| `absent` | No owned container exists |
| `provisioning` | Create, reconcile, or stop outcome is unresolved |
| `running` | OpenCode is running and not externally frozen |
| `paused` | An exact committed Fern intent explains stopped/frozen compute |
| `failed` | Exit, death, OOM, or stop lacks valid Fern intent |

Fern writes a pending, exact-container pause intent before stopping and commits
it only after Docker confirms success. Ambiguous stops remain unresolved. An
external clean exit is failure unless a valid pause or shutdown intent explains
it.

### 9.4 Backend Health

An endpoint is not published until Fern proves:

1. missing credentials receive `401`;
2. intentionally wrong credentials receive `401`;
3. configured credentials receive a healthy response;
4. the OpenCode activity stream connects.

The negative probes prevent Fern from exposing a backend that silently disabled
authentication.

## 10. Durable Storage

Fern intentionally has two persistence systems with different compatibility
histories.

### 10.1 Legacy JSON Control Store

Location:

```text
$HOME/.fern/control/<workspace-hash>.json
```

It stores device grants, coarse workflows, and legacy publications. The store
uses a private real directory, a private singly linked regular file, strict and
bounded JSON, atomic temporary-file replacement, file and directory sync, a
versioned schema, and a process-local mutex.

This store is not the durable task journal.

### 10.2 SQLite Task Store

Location:

```text
$HOME/.fern/tasks/<workspace>.db
```

The store requires a private current-user-owned directory and current-user-owned
regular `0600` database. It rejects symlinks and unsafe types. SQLite uses WAL,
foreign keys, `synchronous=FULL`, a bounded busy timeout, and immediate write
transactions. Startup verifies integrity, migration names, and migration SQL
checksums.

Current migrations are:

1. `initial_task_store`
2. `execution_projection_and_results`
3. `verification_and_publication_journals`

Constraints and triggers enforce ownership, immutable tuples, event-backed
transitions, revision increments, one effecting verification per workspace,
publication phase monotonicity, terminal immutability, and direct-SQL tamper
rejection.

### 10.3 Durable Entities

The task store contains:

| Entity | Purpose |
| --- | --- |
| Workspace | Immutable repository, installation, image, protocol, and runtime binding |
| Task | User intent, base tuple, cancellation fence, current attempt, sealed result |
| Attempt | Delivery/execution identity, OpenCode IDs, policy snapshot, state and revision |
| Receipt | Idempotency scope, request hash, actor, target, response projection |
| Actor snapshot | Immutable authenticated principal at command time |
| Event | Ordered task/attempt audit projection |
| Result | Immutable Git and OpenCode evidence tuple |
| Manifest entry | Canonical changed-path/object evidence |
| Verification | One exact policy/runner invocation journal |
| Publication | One exact branch and draft-PR effect journal |

Reads discover work. They do not grant effect authority. Every write rechecks
current identity, state, revision, cancellation, and ownership transactionally.

## 11. Task API And UI

Remote paired devices can use:

```text
POST /fern/api/v1/tasks
GET  /fern/api/v1/tasks/{taskId}
POST /fern/api/v1/tasks/{taskId}/cancel
GET  /fern/api/v1/events?after=<cursor>&limit=<n>
```

Submission and cancellation require `Idempotency-Key`. Request bodies and
responses are bounded and strict. The event route returns cursor-paginated JSON;
it is not OpenCode's volatile SSE stream.

Submission resolves the requested base ref through the configured numeric
GitHub repository before admission. The client cannot choose image, object
format, OpenCode protocol, execution contract, agent, model provider, model,
budget, installation, repository, or verification policy.

One admission transaction creates:

- task and first attempt;
- command receipt;
- actor snapshot;
- caller-selected exact OpenCode session/message IDs generated by Fern;
- `task.accepted` and `attempt.prepared` events.

Wake notification occurs only after commit. A lost response can be replayed by
the same idempotency key and request hash without creating a second task.

The phone UI supports submission, exact reads, refresh, and cancellation. It
stores up to 50 submitted task IDs in browser `localStorage`. There is no
server-side task-list endpoint, so task discovery is device-local rather than a
complete workspace queue browser.

## 12. Task And Attempt State Machines

Task states are:

```text
queued
running
input_required
cancel_requested
uncertain
recovery_required
completed
failed
canceled
```

Attempt states are:

```text
prepared
delivering
admitted
running
input_required
cancel_requested
uncertain
recovery_required
succeeded
failed
canceled
superseded
```

The store supports `succeeded` and `failed`; the production OpenCode observer
currently has no generic evidence that authorizes either terminal projection.

### 12.1 Delivery Phases

Delivery has a separate monotonic effect phase:

```text
none
  -> claimed
  -> session_create_started
  -> session_ready
  -> prompt_started
```

The coordinator handles cancellation first, uncertain reconciliation second,
and fresh prepared work last. It commits each phase before the corresponding
OpenCode effect.

Delivery performs these steps:

1. Claim one exact attempt under a bounded lease.
2. Commit session-create start.
3. Create or read-reconcile the exact caller-selected session ID.
4. Commit session readiness only after exact title, directory, agent, model,
   protocol, and image compatibility.
5. Commit prompt start.
6. Submit the exact message ID and exact prompt envelope once.
7. Read-reconcile inbox/history before recording admission.

The prompt text maximum is exactly 65,536 bytes. Fern's JSON envelope has its
own independent bound. Fern never retries a prompt with mutated bytes.

Expired claims become uncertain before takeover. Pre-prompt ambiguity may
resume only from exact proof. Once prompt start is durable, Fern never assumes a
safe mutation retry; it reconciles exact session, inbox, message, and prompt
identity. Identity conflicts become `recovery_required`.

### 12.2 Execution Observation

The execution coordinator can positively prove:

- exact session and prompt compatibility;
- prompt promotion into message history;
- active execution;
- live pending permissions or forms;
- protocol, image, session, or message conflicts;
- attempt deadline expiry.

It projects `running`, `input_required`, durable cancellation request, or
`recovery_required`. It does not treat an inactive session, empty inbox, idle
status, or missing form as success or failure.

Permissions and forms are process-epoch state in the pinned profile. They may
disappear across OpenCode process restart while durable tool state remains
running. Disappearance therefore cannot imply acceptance, rejection, or
terminal completion.

### 12.3 Cancellation

Cancellation first commits one exact fence and disposition:

| Disposition | Meaning |
| --- | --- |
| `none_prepared` | No OpenCode effect was started |
| `reconcile_delivery` | Delivery identity must be read-reconciled |
| `interrupt` | Exact admitted/running session may be interrupted |
| `none_terminal` | Attempt is already terminal |

Only after commit may the coordinator acknowledge unstarted work, reconcile
delivery, delete one proven-undelivered inbox item, or interrupt the exact
session. Completion records ordered attempt/task events.

Cancellation does not claim to roll back provider cost, tool effects, files,
commits, pushes, or pull requests.

### 12.4 Durable Approvals

The domain model and OpenCode adapter know permission/form concepts, but the
SQLite task schema has no durable approvals table and the phone API has no
approval answer route. Current production behavior records only
`input_required`. Fern does not reconstruct vanished process-epoch options.

## 13. OpenCode Contract

The pinned profile is characterized by `integration/opencode-contract` rather
than assumed from generic OpenCode documentation.

Proven properties include:

- caller-selected session IDs are accepted and durable;
- caller-selected message IDs are preserved;
- exact same-ID/same-body retry is stable;
- same-ID/different-body retry conflicts;
- durable sessions/messages survive container replacement;
- an exact 65,536-byte prompt is admitted;
- response-lost prompt execution does not duplicate on exact retry;
- permission/form/question behavior is process-epoch scoped as characterized;
- interrupt behavior is exact-session scoped.

Blocked properties include:

- durable generic terminal success/failure;
- durable event replay from `Last-Event-ID`;
- useful generic log replay after a cursor;
- durable permission/form state across process replacement.

These blocked properties are architectural constraints. They are not converted
into heuristics in the task store.

## 14. Result Collection And Sealing

`internal/taskresult` is a read-only Git collector. It accepts an exact
repository/base/OpenCode evidence tuple from a coordinator and proves:

- repository and `.git` are real nonsymlink directories;
- SHA-1 object format;
- no shallow history, grafts, alternates, replace refs, unsafe config,
  submodules, gitlinks, or linked-worktree ambiguity;
- base and result are commits;
- result equals or descends from base under policy;
- exact result tree;
- exact clean index/worktree with no untracked files;
- bounded sorted raw-diff manifest from full object IDs;
- allowed regular, executable, and symlink blob modes;
- exact old/new blob sizes;
- canonical manifest JSON and SHA-256;
- repeated final state, tree, and manifest stability.

Paths are stored as canonical padded RFC 4648 base64 so arbitrary Git path bytes
remain representable. Rename detection is disabled; a rename is a delete/add
pair.

`internal/taskresultcoord` is the explicit authority boundary between a durable
succeeded attempt and the collector. It does not poll, record success, or expose
any success-projection method. One `RunOnce` call:

1. Selects an already-durable succeeded, unsealed attempt.
2. Passes the complete task/attempt revision and OpenCode session/message
   identity to an injected authoritative observer.
3. Requires two valid, matching sanitized success observations inside
   `AcquireQuiesced`.
4. Re-selects and compares the complete identity under the retained fence.
5. Collects the exact Git result using the observed evidence and policy digest.
6. Generates one stable result/event ID set and calls `SealResult` once.
7. Retains the fence until that transaction returns, including through bounded
   commit cleanup after caller cancellation.

Observer errors are classified without rendering raw OpenCode output. Evidence
must be a bounded JSON object with an exact SHA-256 and policy version; the
collector additionally rejects sensitive evidence keys.

`taskstore.SealResult` atomically:

1. rechecks the exact successful current attempt and task revisions;
2. rechecks repository/base and OpenCode session/message identity;
3. inserts the immutable result and manifest;
4. binds the result to task and attempt;
5. advances the task to `completed`;
6. emits ordered `attempt.result_sealed` and `task.completed` events.

The transaction performs no Git or OpenCode read. Its caller must supply those
facts while retaining `AcquireQuiesced` through commit.

No production OpenCode observer currently supplies generic terminal-success
authority, and `fern up` intentionally does not construct this coordinator.
Result collection therefore remains unreachable from ordinary inactive/idle
observations even though the exact host-side sealing entry point exists.

## 15. Verification

Verification is optional and begins only from an immutable sealed result. Its
command comes from host configuration, never from the phone or repository.

`internal/verification` constructs canonical policy and runner snapshots:

- check name and argv;
- working directory and timeout;
- output limit;
- policy SHA-256;
- exact merged-environment SHA-256;
- runner name and version;
- bound workspace image identity.

Fern inserts no shell and uses no ambient process environment. It resolves native
check and Git executables with symlink-refusing `openat` traversal, rejects
unsafe writable parents and files, and binds metadata plus SHA-256 into policy
and runner snapshots. Each invocation reopens and verifies one descriptor, then
executes that descriptor on Linux or a private descriptor-sourced copy on
Darwin. Apple's non-relocatable `/usr/bin/git` shim is rejected; Fern selects
the real Command Line Tools or Xcode Git binary. The persisted runner version
includes the complete runner aggregate digest. Unsupported platforms fail
closed. Repository code and operator-supplied policy remain in the trusted host
boundary.

Every invocation runs in a dedicated process group. Fern tears down that group
after the leader exits, including on success, bounds inherited-output draining,
and performs repository postflight only afterward. This contains ordinary
background descendants, but it is not a hostile-process sandbox: a deliberately
malicious descendant can create a new session to escape process-group teardown.

Before advancing a prepared verification, the coordinator acquires
`workspace.Manager.AcquirePaused`, re-reads task and attempt ownership under that
fence, and retains it through command execution and the terminal store write.
Workspace compute therefore cannot change and restore the checkout around the
runner's observations.

The runner proves exact clean commit state before and after the command. It
executes in a process group with a deadline and captures stdout/stderr through
independently bounded evidence writers. SQLite stores total byte counts,
retained-byte counts, truncation flags, and full SHA-256 digests, but no output
bytes.

Durable verification states are:

```text
prepared -> running -> succeeded | failed | recovery_required
```

`internal/taskverification` commits `running` before one runner invocation.
After a process restart, a discovered `running` record is never rerun; it becomes
`recovery_required`. Start ambiguity, postflight integrity failure, snapshot
mismatch, and ambiguous completion also fail closed. Definite timeout,
cancellation, signal, and nonzero exit become failed. Only exact exit zero with
clean postflight becomes succeeded.

When `tasks.verification` is configured, `fern up` runs one workspace-scoped
verification loop. Without the block, no verification effect is authorized.

## 16. GitHub App Boundary

### 16.1 Onboarding

App setup is operator-authenticated on loopback. The callback is accepted only
on the exact configured private HTTPS origin and is authorized by one-use state,
not by an ambient browser credential.

The generated App is private and requests:

| Permission | Level |
| --- | --- |
| Metadata | Read |
| Contents | Write |
| Pull requests | Write |

No webhook events are requested.

Onboarding state has 256 random bits and a ten-minute lifetime. Fern persists a
digest and non-secret local flow binding, not raw state, code, or claim secrets.
A pending callback can recover after Fern restart. Claim is durably committed
before Manifest conversion. A callback claimed before a crash never regains
exchange authority; ambiguous conversion is quarantined for operator review.

The proxy strips ambient `Authorization` and `Cookie` before callback dispatch.
After successful conversion the browser redirects to the exact validated
loopback setup origin plus its persisted relative return path. The control page
is never exposed on remote ingress.

Once valid credentials exist, setup is not mounted. Replacement, backup, and
rotation remain intentionally disabled until their recovery contract exists.

### 16.2 Credential Storage

App credentials live under:

```text
$HOME/.fern/github-app
```

The store requires a private `0700` directory and `0600` regular credential
file, rejects symlinks and unsafe ownership/type, validates credentials on load,
and uses atomic replacement plus directory sync. This is filesystem permission
protection, not encryption at rest.

### 16.3 Tokens And Repository Authority

Fern signs short-lived RS256 App JWTs. Two installation credential types are
separate and non-interchangeable:

- installation-wide discovery tokens for repository enumeration primitives;
- repository-scoped installation tokens for one configured numeric repository.

Task startup validates installation ID, numeric repository ID, and exact
canonical full name through GitHub before persisting a new workspace binding.
Every repository/PR REST operation requests a fresh scoped token and validates
permissions and expiry. Clients refuse redirects and bounded-response or
pagination violations.

Repository discovery primitives exist, but onboarding does not yet provide a
selection UI or durable selection update. Operators configure installation and
repository identity explicitly.

### 16.4 Chosen Product Direction

The target product workflow, recorded in `docs/GITHUB_INTEGRATION.md`, is now
Amp-style authenticated `gh` inside the workspace with explicit user-invoked
push and draft-PR actions. This section and Section 17 continue to describe the
current host-brokered GitHub App implementation. They are not claims that the
chosen workspace-`gh` workflow is implemented.

In the chosen mode, prompt intent is the authorization and OpenCode may invoke
`gh` directly. A Fern phone action cannot be an exclusive publication gate; it
can only provide additional durable audit and reconciliation for effects Fern
itself performs.

Moving authority into the workspace changes the credential boundary rather
than merely changing the phone UI. It requires a pinned `gh` binary, durable
authentication and revocation, explicit command authorization, repository and
scope policy, mutation reconciliation, and updated image/security/release
evidence. Automatic OpenCode terminal-result proof is not a prerequisite for an
explicit user-authorized snapshot and draft-PR action.

## 17. Durable GitHub App Publication

Durable publication consumes only an exact successful result and verification
tuple. It never publishes mutable current `HEAD`.

The immutable publication tuple binds:

- installation ID;
- numeric repository ID and exact full name;
- base ref and base SHA;
- result commit;
- successful verification commit;
- operation ID;
- deterministic branch `fern/<workspace>/<operation-id>`;
- expected old remote SHA;
- requesting actor;
- broker policy digest.

The transport uses one private askpass file per push. Installation credentials
do not appear in argv, durable evidence, returned errors, or logs. Git receives
an exact commit refspec and an exact `--force-with-lease` expected-old value;
there is no unconditional force or blind overwrite.

The GitHub REST client revalidates repository identity, branch SHA, and complete
pull-request base/head repository, ref, SHA, state, draft flag, number, and URL.
Fork-head observations conflict rather than being accepted as the configured
target.

Publication effect phases are:

```text
none
  -> push_started
  -> push_observed
  -> pr_create_started
  -> published
```

`internal/taskpublicationcoord` obeys these rules:

1. Discover work, acquire `workspace.Manager.AcquirePaused`, and re-read the
   identical publication/task/attempt/result/verification revisions under the
   retained fence.
2. Keep that fence through every publisher call and store transition so mutable
   checkout Git configuration cannot redirect credential-bearing Git.
3. Commit `push_started` before one push invocation.
4. After restart in `push_started`, perform only branch reads; never push again.
5. Commit `push_observed` only for the exact result SHA.
6. Read-reconcile an existing exact draft PR before creation.
7. Commit `pr_create_started` before one PR create invocation.
8. After restart in `pr_create_started`, perform only PR reads; never create
   again.
9. Complete only from one exact authoritative branch/PR observation.

Lost responses do not authorize mutation retries. Temporary read ambiguity
becomes `uncertain`; contradictions become `conflict` or
`recovery_required`. Startup drains available journal reconciliation before HTTP
serving, then one workspace-scoped loop continues.

The missing boundary is preparation. `taskstore.PreparePublication` exists, but
no production API creates its initial record. A phone publication command still
needs a transactional idempotency receipt and explicit user authority before it
can safely call that method.

## 18. Legacy Host-Credential Publication

The legacy operator path is a separate subsystem. It uses:

- JSON `control.Workflow` and `control.Publication` records;
- the host user's broad `gh` credential;
- `workspace.Manager.AcquirePaused`;
- operator-only `/fern/control` routes.

It persists an exact preparation before mutation and re-reads one draft pull
request before success. Checkout `origin` is a consistency diagnostic, not
repository authority. This path is prototype-only because its credential may be
account-wide.

`fern github publish --dry-run` retains diagnostics. Standalone mutation is
rejected because it would bypass the service-owned durable coordinator.

App installation mode disables this entire legacy publisher. The two systems do
not share a state machine or credential source.

## 19. Recovery Rules

Fern uses four broad recovery classes:

| Class | Rule |
| --- | --- |
| Read-only replay | Repeat bounded authoritative reads freely |
| Idempotent exact replay | Repeat only when the external API contract proves same-ID/same-bytes stability |
| Journal-fenced one-shot effect | Commit start first; after ambiguity reconcile but never mutate again |
| Unprovable outcome | Enter `uncertain` or `recovery_required`; require operator/new authority |

Examples:

| Condition | Recovery |
| --- | --- |
| Lost task admission response | Same idempotency key/hash returns committed receipt |
| Lost exact OpenCode prompt response | Exact ID/body read-reconciliation; no changed retry |
| Expired delivery claim | Persist uncertain before takeover |
| Fern restart during verification | Mark running verification recovery-required; do not rerun |
| Lost push response | Read exact branch; do not push again |
| Lost PR-create response | Discover and re-read exact PR; do not create again |
| Missing process-epoch form after restart | Do not infer answer or success |
| OpenCode inactive with empty inbox | Leave execution unresolved |
| External clean Docker exit | Classify failed unless exact Fern intent explains it |
| OOM/dead container | Classify failed |
| Desired-state drift | Refuse reuse until explicit recreation |

## 20. Security Properties And Limits

### 20.1 Implemented Properties

- Both application listeners are distinct numeric loopback addresses.
- Remote ingress rejects backend and operator Basic before wake.
- Operator controls are absent from remote routing policy.
- Backend health includes negative authentication probes.
- Device grants are digest-backed, expiring, revocable, and request-attributed.
- Incoming forwarding authority and credentials are stripped and regenerated.
- Browser mutations enforce exact origin and Fetch Metadata.
- Host state paths enforce ownership, type, link, and mode constraints.
- Docker resources require exact labels, immutable IDs, and desired-state proof.
- Task commands persist immutable actor snapshots and idempotency receipts.
- Prompt mutation retry, verification rerun, push retry, and PR-create retry are
  prohibited after their respective ambiguity fences.
- Verification commands are host policy, shell-free, bounded, and commit-bound.
- Repository identity comes from numeric GitHub authority, never checkout
  remotes.
- App publication uses repository-scoped, short-lived credentials.
- Request, response, output, evidence, manifest, and pagination sizes are
  bounded.

### 20.2 Explicit Limits

- Fern is not a hostile multi-user or multi-tenant sandbox.
- Docker-group access and the trusted host remain privileged.
- The workspace root filesystem is writable.
- Provider credentials intentionally exposed to OpenCode are in its trust
  boundary.
- Tailscale supplies private reachability and ACL policy, not Fern user identity.
- Operator HTML has no per-form CSRF token.
- The device cookie is not yet in the `__Host-` namespace.
- Pairing has an outstanding-code cap but no general issuance rate limiter.
- The operator listener is not a supported OpenCode browser origin.
- Real TLS/WSS and physical-phone PTY revocation are not accepted yet.
- App credentials are unencrypted and have no Fern-managed backup or rotation.
- The legacy host-`gh` publisher has broader authority than App mode.
- Verification process isolation does not protect the host from malicious
  operator-approved checks or trusted repository code.

`docs/SECURITY.md` is the detailed security gap record.

## 21. Commands And Operational Surfaces

| Command | Role | Workspace lease/effect |
| --- | --- | --- |
| `fern init` | Generate configuration and protected secrets | No workspace lease |
| `fern doctor` | Validate Docker, proxy, GitHub, Tailscale, and phone route | Diagnostic reads |
| `fern up` | Run lifecycle, proxy, onboarding, and task services | Exclusive lifecycle writer |
| `fern attach` | Launch official OpenCode terminal client through operator proxy | Read/admission path |
| `fern down` | Remove compute and clear pause intent | Exclusive lifecycle writer |
| `fern status` | Inspect classified Docker state | Read-only |
| `fern logs` | Stream Docker logs | Read-only |
| `fern debug events` | Direct backend health/SSE diagnostic | Diagnostic bypass |
| `fern debug wake` | Print the phase waterfall (admission, lifecycle token, Docker mutation, health probe, observer attach) for one wake through the operator listener | Operator-only diagnostic; requires the control credential and a running `fern up` |
| `fern github publish --dry-run` | Legacy preparation diagnostics | No mutation |
| `fern version` | Print embedded release identity | No workspace lease |

`down`, `status`, and `logs` load only workspace identity so incident cleanup
remains possible when unrelated configuration is invalid.

## 22. Observability And Evidence

Fern logs service state and bounded error classes through structured logging.
Sensitive payloads, prompts, credentials, subprocess output, and GitHub token
values are not logged as coordinator evidence.

Durable task evidence uses:

- immutable actor snapshots;
- ordered task/attempt event cursors;
- bounded sanitized JSON evidence;
- SHA-256 evidence digests;
- exact expected revisions;
- immutable external identity tuples;
- complete output counts and digests without output bytes.

OpenCode's volatile event stream is never presented as a durable audit log.
GitHub mutation success is never accepted solely from a mutation response when
an authoritative read can prove the final tuple.

## 23. CI, Release And Deployment

CI performs:

- `gofmt` checks;
- all Go tests;
- CGO-free task-store tests;
- full race tests;
- `go vet`;
- binary build;
- deployment static checks;
- reproducible release harness;
- release checksum verification;
- workspace image UID/GID and OpenCode version checks;
- real OpenCode smoke testing;
- real Docker lifecycle testing;
- Chromium and WebKit mobile browser rehearsals.

Release builds require a clean Git tree. They embed version and commit, use
`CGO_ENABLED=0`, target Linux amd64 and arm64, use `-trimpath` and no build ID,
and emit metadata plus SHA-256 checksums. The clean-copy release harness builds
twice and compares artifacts.

The checked-in systemd unit runs as `fern`, uses Docker group access, loads
`/etc/fern/fern.env`, applies `UMask=0077`, enables service hardening, restarts on
failure, and allows bounded shutdown. Both Fern listeners remain loopback.
Tailscale Serve must expose only the remote listener.

Checked-in deterministic evidence covers lifecycle transitions, simulated
orderly Docker shutdown, archive/checksum/restore, mobile viewport behavior,
device revocation, reproducible release output, and static deployment policy.

It does not establish:

- a complete fresh target-host install;
- physical reboot and fresh-host restore;
- real private-edge TLS/WSS behavior;
- a provider-funded generic terminal task;
- schema upgrade from every shipped release;
- transactional backup of all SQLite and App secrets;
- executable rollback;
- artifact signing or provenance authenticity;
- tailnet ACL denial from an independent principal.

## 24. Known Product Gaps

The following are architectural gaps, not hidden implementation assumptions:

1. **Workspace `gh` target:** the chosen Amp-style workflow is not implemented.
   The image has no `gh`, no workspace GitHub authentication lifecycle exists,
   and the current configuration rejects GitHub token variables from the
   workspace.
2. **Explicit snapshot/publication command:** there is no idempotent paired-phone
   push or draft-PR command. Its semantics, actor receipt, repository/scope
   checks, and lost-response reconciliation still need to be defined.
3. **User-authorized result sealing:** the collector can seal only an already
   succeeded attempt through an authoritative observer. There is no separate
   path for a user to quiesce and seal the current clean committed state without
   asserting that OpenCode completed successfully.
4. **Generic terminal result:** the pinned OpenCode profile cannot durably prove
   generic terminal success/failure. Result sealing cannot be triggered by idle,
   inactivity, empty inbox, or missing process-epoch input.
5. **Result coordinator reachability:** the explicit coordinator exists, but no
   production observer can provide its externally authorized success evidence.
6. **Current broker publication admission:** no idempotent phone command
   prepares a durable publication, even after successful verification.
7. **Durable approvals:** no approval table, phone approval API, or restart-safe
   option contract exists.
8. **GitHub selection:** installation discovery primitives exist, but selection
   UI and durable configuration update do not.
9. **Credential lifecycle:** neither target workspace-`gh` credentials nor the
   current App credentials have a complete backup, rotation, revocation, and
   replacement-recovery contract.
10. **Automatic post-onboarding activation:** Fern requires restart after first
   credential creation.
11. **Notifications and review continuation:** PR/CI polling, phone notification,
   and continued review sessions are absent.
12. **Physical acceptance:** phone sleep/wake, real PTY/SSE revocation, private
   TLS/WSS, reboot, restore, and ACL-negative tests require operator-controlled
   environments.

## 25. Source Map

| Area | Primary source |
| --- | --- |
| Composition and shutdown | `cmd/fern/up.go` |
| Task/App construction | `cmd/fern/tasks.go` |
| Configuration | `internal/config/config.go` |
| Remote/operator proxy | `internal/proxy/proxy.go`, `internal/proxy/gateway.go` |
| Pairing and device auth | `internal/proxy/pairing.go`, `internal/control/store.go` |
| Lifecycle manager | `internal/workspace/manager.go` |
| Activity observation | `internal/watch/` |
| Docker runtime | `internal/runtime/` |
| Task API and UI | `internal/taskapi/`, `internal/proxy/task_ui.go` |
| Task state and tuples | `internal/task/` |
| SQLite store | `internal/taskstore/` |
| OpenCode adapter | `internal/opencodeapi/` |
| Delivery | `internal/taskdelivery/` |
| Execution observation | `internal/taskexecution/` |
| Result collection | `internal/taskresult/` |
| Result sealing coordinator | `internal/taskresultcoord/` |
| Verification runner | `internal/verification/` |
| Verification coordinator | `internal/taskverification/` |
| GitHub App | `internal/githubapp/` |
| Durable publication transport | `internal/taskpublication/` |
| Durable publication coordinator | `internal/taskpublicationcoord/` |
| Legacy publication | `internal/publication/` |
| OpenCode contract tests | `integration/opencode-contract/` |
| Lifecycle evidence | `integration/lifecycle/` |
| Release evidence | `integration/release/` |
| Deployment | `deploy/systemd/`, `docs/DEPLOYMENT.md` |

`docs/TASK_MODEL.md` is the normative task-state and transaction contract.
`docs/GITHUB_INTEGRATION.md` owns GitHub details. `docs/SECURITY.md` owns the
security gap register.
