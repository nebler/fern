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
| Production-wired | Configuration, two ingress policies, pairing and device CSRF, wake/stop/freeze, Docker ownership, workspace `gh`, task listing/admission/delivery/cancellation, conservative execution observation, explicit user-authorized snapshot sealing, optional verification coordination, GitHub App onboarding, repository-scoped publication reconciliation |
| Implemented but authority-gated | Store-level execution success/failure, observer-authorized terminal result sealing, durable publication preparation and transport |
| Not implemented end to end | Generic OpenCode terminal-result proof, durable remote approval answers, idempotent phone publication admission, notifications, GitHub installation-selection UI, automated credential rotation and online transactional backup |

The ordinary durable task path currently reaches this boundary:

```text
phone submit
    |
    v
durable admission -> exact OpenCode delivery -> running/input_required
                                               |       |
                                               |       +-> cancellation/recovery
                                               |
                                               +-> explicit snapshot preview
                                                        |
                                                        v
                                               idempotent user seal
                                                        |
                                                        v
                                               immutable result -> optional verification
```

Verification and durable publication coordinators are real and wired. They do
not make upstream facts true: a user seal authorizes one exact clean repository
snapshot but does not assert that OpenCode completed successfully. Verification
requires that sealed result. Brokered publication additionally requires an
explicitly prepared publication, and no production API currently prepares one.

The root blocker is the pinned OpenCode profile. It has no durable generic
terminal-success object and no durable event cursor. An inactive session, empty
inbox, idle status, or missing process-epoch form is not proof that work
succeeded. Fern fails closed rather than turning absence into success; explicit
user snapshot authorization is a separate completion authority.

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
| App-broker publication mutation authority | A committed durable publication phase |
| Workspace-`gh` mutation authority | The authenticated workspace credential plus explicit user/prompt intent; outside Fern's broker journal |
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
- with task services, one user-authorized result-sealing coordinator;
- optionally, one verification coordinator and one App-broker publication coordinator;
- optionally, one GitHub App onboarding handler.

GitHub authority is explicit and durable. `mode: github-app-broker` requires a
positive installation ID and constructs the App task publisher. `mode:
workspace-gh` forbids an installation ID, constructs the bounded managed-`gh`
executor for task base resolution, mounts a dedicated persistent `gh` config
volume, and does not construct the App publisher. Push and pull-request commands
in workspace-`gh` mode are ordinary user/agent actions outside Fern's brokered
publication journal. The legacy host publisher package remains for diagnostics
and compatibility tests but is not constructed by `fern up`.

### 4.1 Startup Order

`fern up` performs these operations in order:

1. Parse flags and the optional protected environment file.
2. Load defaults, strict YAML, environment references, and explicit overrides.
3. Merge only approved environment-file values into the workspace environment.
4. Validate configuration and parse the memory limit.
5. Bind both loopback listeners before any Docker side effect.
6. Create the shared signal and errgroup context.
7. Acquire the exclusive workspace lease under `$HOME/.fern/locks`.
8. Open the local Docker client, pause-intent state, and legacy control store.
9. Construct the stream controller and workspace manager.
10. Reconcile existing Docker state without waking absent or paused compute.
11. Construct GitHub App onboarding only for App-broker mode when a remote
    origin exists and credentials do not.
12. Construct task services when the task policy and either explicit GitHub
    authority mode exist.
13. Validate App authority eagerly, or construct the managed workspace-`gh`
    resolver, then persist the explicit durable workspace authority binding.
    Workspace-`gh` performs its first live credential/repository check later at
    task admission.
14. Construct user-seal services, delivery and execution coordinators, and any
    configured verification or App-publication coordinator.
15. Construct the two policy-separated proxy handlers.
16. Drain available App-publication journal work before serving HTTP.
17. Start the supervisor, task loops, and both HTTP servers in one errgroup.

Binding listeners first makes an invalid or occupied address a side-effect-free
failure. The exclusive lease prevents a second lifecycle writer. Errors after
manager startup close the task store and manager before returning.

If task policy is configured but App credentials are absent, Fern can still
start the operator onboarding surface when `proxy.remoteOrigin` is configured.
Task services remain disabled until the operator completes onboarding and
restarts Fern. Missing credentials without an onboarding path fail startup.

### 4.2 Shutdown Order

A signal or service error cancels the shared context. Fern then:

1. Gives both HTTP servers a bounded graceful shutdown window.
2. Force-closes tracked connections if graceful shutdown fails.
3. Waits for errgroup-owned servers and task coordinators.
4. On an actual process signal, records a bounded Docker shutdown intent.
5. Closes the workspace manager and waits for admitted requests, pauses, wakes,
   and rollback ownership.
6. Stops the OpenCode activity stream.
7. Closes the SQLite store and Docker client through deferred cleanup.

The manager closes before Docker because it owns wake and rollback goroutines.
If bounded manager shutdown times out, Fern deliberately leaves the Docker
client open until process exit rather than closing it underneath still-owned
goroutines.
Stopping Fern does not intentionally stop OpenCode. A short-lived shutdown
intent lets Docker stop the exact container shortly afterward without that exit
being misclassified as an unexplained crash.

## 5. Configuration And Secret Flow

Configuration types and defaults are defined in `internal/config/config.go`;
strict loading, environment expansion, YAML decoding, and validation are split
across `load.go`, `env.go`, `yamlnode.go`, and `validate.go`.

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
| Idle suspend mechanism | `stop` (`freeze` uses the cgroup freezer) |
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
duration, turn budget, GitHub authority mode, numeric repository ID, and exact
canonical repository full name. App-broker mode also requires a positive
installation ID; workspace-`gh` mode forbids one and supports only `github.com`.

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
- task listing, submission, reads, cancellation, snapshot sealing, and event reads.

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
Cross-site and same-site sibling requests fail closed. Cookie-authenticated
device mutations additionally require a short-lived HMAC token bound to the
device credential, method, and normalized route. Explicit Basic-auth mutations
remain token-exempt and rely on their separate ingress and origin policy.

### 6.5 Pairing And Revocation

Pairing codes contain 256 random bits and expire after five minutes. GET only
previews; POST atomically consumes the code and creates a 30-day grant. Fern
caps outstanding codes, per-code attempts, global failures, issuance, and
successful exchanges. Limiter state is persisted with bounded strict JSON so a
restart does not reset abuse controls. Fern stores only code/token digests.

Device cookies use the `__Host-fern_device` namespace and are `Secure`,
`HttpOnly`, `SameSite=Strict`, path `/`, and domainless. A successful
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
- writable OpenCode volume at `/home/user/.local/share/opencode`;
- in workspace-`gh` mode, a separate writable volume at
  `/home/user/.config/gh` with Fern-managed `GH_CONFIG_DIR`.

The root filesystem is writable. These controls reduce accidental privilege but
do not make untrusted repository code a host-security sandbox.

The image runs:

```text
opencode2 serve --hostname 0.0.0.0 --port 4096
```

The supported OpenCode version is `0.0.0-next-17444`. The characterized
development image identity is
`sha256:73688cd6f96ce3b236bb1c2d25607b03566a4ee92f0fedabeb06fd1a3e643c6c`;
the contract harness verifies both version and digest. The image also pins
GitHub CLI `2.98.0`; workspace-`gh` execution uses only
`/usr/local/bin/gh` through Fern's attested, bounded Docker exec path.

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

It stores device grants, a stable random operator credential identifier, coarse
workflows, and legacy publications. A sibling auxiliary file persists bounded
pairing limiter state. The store
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
4. `user_authorized_snapshot_seals`
5. `explicit_workspace_github_authority`

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
| Seal request | Idempotent user authority for one exact previewed repository snapshot |
| Verification | One exact policy/runner invocation journal |
| Publication | One exact branch and draft-PR effect journal |

Reads discover work. They do not grant effect authority. Every write rechecks
current identity, state, revision, cancellation, and ownership transactionally.

## 11. Task API And UI

Remote paired devices can use:

```text
GET  /fern/api/v1/tasks?limit=<n>
POST /fern/api/v1/tasks
GET  /fern/api/v1/tasks/{taskId}
POST /fern/api/v1/tasks/{taskId}/cancel
GET  /fern/api/v1/tasks/{taskId}/seal-preview
POST /fern/api/v1/tasks/{taskId}/seal
GET  /fern/api/v1/events?after=<cursor>&limit=<n>
```

Submission, cancellation, and sealing require `Idempotency-Key`. Request bodies
and responses are bounded and strict. The event route returns cursor-paginated
task/attempt JSON; it is not OpenCode's volatile SSE stream and does not include
the separate verification/publication journal.

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

The phone UI supports server-backed task listing, submission, refresh,
cancellation, exact-session links, snapshot preview, and explicit sealing. A
seal authorizes one exact clean repository snapshot and does not claim OpenCode
success. The current UI generates a fresh idempotency key for each invocation;
it does not retain an in-flight key across a lost response, so retrying manually
can create a second semantic submission despite the backend replay contract.

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

`internal/taskresultcoord` supports two deliberately separate completion
authorities. Neither polls for or infers success.

### 14.1 User-Authorized Snapshot Sealing

This path is production-composed when task services are enabled:

1. `GET .../seal-preview` selects an eligible running or input-required task,
   acquires `AcquirePaused`, rechecks ownership, and collects one exact clean Git
   snapshot.
2. `POST .../seal` requires the exact preview plus an idempotency key, repeats
   collection under the paused fence, and atomically stores a receipt and
   immutable `seal_requests` row.
3. The result coordinator claims that row, acquires `AcquireQuiesced`, rechecks
   every revision and expected object/digest, recollects the exact snapshot, and
   calls `SealAuthorizedResult` once.
4. The final transaction inserts the result and manifest, changes the attempt
   to `superseded` rather than `succeeded`, completes the task, emits result/task
   events, and completes the seal request.

This is explicit authority for repository state, not evidence that OpenCode
finished. There is a current lifecycle defect in the composition: preview and
authorization leave compute paused, while the asynchronous coordinator's
`AcquireQuiesced` requires it already running. A seal can therefore remain
claimed until unrelated upstream traffic wakes the workspace and the lease is
retried. The API/UI/store path is wired, but end-to-end liveness needs a
real-manager integration fix and test.

### 14.2 Observer-Authorized Execution Success

The second path accepts only an already-durable succeeded, unsealed attempt and
an injected authoritative observer. It requires two valid matching success
observations inside `AcquireQuiesced`, reselects the complete identity, collects
Git evidence, and calls `SealResult` once while retaining the fence through
bounded commit cleanup. Observer evidence must be bounded JSON with an exact
SHA-256 and policy version; sensitive evidence keys are rejected.

No production OpenCode observer supplies this generic terminal-success
authority, so `fern up` constructs only the user-authorized variant. In both
paths the sealing transaction performs no Git or OpenCode reads; its caller must
supply those facts while retaining the relevant fence through commit.

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

Once valid credentials exist, setup is not mounted. Automated replacement and
rotation remain disabled. The offline host backup flow can externalize the
credential file, but it is not an online credential-lifecycle system.

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

App-broker task startup validates installation ID, numeric repository ID, and
exact canonical full name through GitHub before persisting a new workspace binding.
Every repository/PR REST operation requests a fresh scoped token and validates
permissions and expiry. Clients refuse redirects and bounded-response or
pagination violations.

Repository discovery primitives exist, but onboarding does not yet provide a
selection UI or durable selection update. Operators configure installation and
repository identity explicitly.

### 16.4 Chosen Product Direction

The chosen workflow, recorded in `docs/GITHUB_INTEGRATION.md`, is Amp-style
authenticated `gh` inside the workspace with explicit user-invoked push and
draft-PR actions. Its substrate is implemented: the image checksum-pins GitHub
CLI 2.98.0, workspace-`gh` mode mounts a dedicated persistent config volume,
and Fern can execute bounded, image-attested `gh api` reads for task base-ref
resolution. Users authenticate and run ordinary `gh`/Git commands in the
trusted workspace; Fern does not wrap those mutations in its App publication
journal.

In the chosen mode, prompt intent is the authorization and OpenCode may invoke
`gh` directly. A Fern phone action cannot be an exclusive publication gate; it
can only provide additional durable audit and reconciliation for effects Fern
itself performs.

Moving authority into the workspace changes the credential boundary rather
than merely changing the phone UI. The workspace can use whatever repository
scope its stored `gh` credential has, and direct mutations have no Fern receipt
or lost-response reconciliation. Fern's managed base resolver still validates
numeric repository identity and exact full name. Unlike App mode, the first
live workspace-`gh` repository check happens when a task resolves its base,
after a new durable workspace binding may already have been created; correcting
a mistaken immutable binding currently requires operator state recovery.

## 17. Durable GitHub App Publication

Durable publication consumes only an exact sealed result and successful
verification tuple. It never publishes mutable current `HEAD`.

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

The legacy host-credential subsystem remains implemented and tested but is not
constructed by `fern up`; production leaves `proxy.Controls.Publications` nil,
so effecting routes reject use. The retained package uses:

- JSON `control.Workflow` and `control.Publication` records;
- the host user's broad `gh` credential;
- `workspace.Manager.AcquirePaused`;
- now-disabled operator `/fern/control` publication routes.

It persists an exact preparation before mutation and re-reads one draft pull
request before success. Checkout `origin` is a consistency diagnostic, not
repository authority. This path is prototype-only because its credential may be
account-wide.

`fern github publish --dry-run` retains diagnostics. Standalone mutation is
rejected because it would bypass the service-owned durable coordinator.

No GitHub authority mode enables this legacy publisher. It does not share a
state machine or credential source with App publication.

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
| Lost user-seal response | Same idempotency key/hash returns the durable seal request |
| Authorized snapshot changes before sealing | Reject that seal request; never substitute a new snapshot |
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
- Browser mutations enforce exact origin and Fetch Metadata; device-cookie
  mutations also require route-bound CSRF tokens.
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
- The operator listener is not a supported OpenCode browser origin.
- Real TLS/WSS and physical-phone PTY revocation are not accepted yet.
- App and workspace-`gh` credentials are unencrypted and have no automated
  rotation or online transactional backup.
- Workspace-`gh` credentials may have broader authority than App mode and are
  intentionally available to trusted workspace code.
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

The offline host backup utility archives Fern state, configuration, repository
state, and named OpenCode/workspace-`gh` volumes with checksums, separates
credential custody, and exercises staged activation and rollback. This is a
deterministic backup/restore foundation, not an online cross-store snapshot or a
physical fresh-host acceptance result.

It does not establish:

- a complete fresh target-host install;
- physical reboot and fresh-host restore;
- real private-edge TLS/WSS behavior;
- a provider-funded generic terminal task;
- schema upgrade from every shipped release;
- online transactional backup across SQLite, App secrets, and Docker volumes;
- executable rollback;
- artifact signing or provenance authenticity;
- tailnet ACL denial from an independent principal.

## 24. Known Product Gaps

The following are architectural gaps, not hidden implementation assumptions:

1. **User-seal lifecycle liveness:** preview and authorization pause compute,
   while asynchronous completion currently requires an already-running target.
   The claimed request can stall until another upstream request wakes compute.
   Preview is also a side-effecting GET, which is unsafe for prefetch/retry
   semantics.
2. **Generic terminal result:** the pinned OpenCode profile cannot durably prove
   generic terminal success/failure. Result sealing cannot be triggered by idle,
   inactivity, empty inbox, or missing process-epoch input.
3. **Current broker publication admission:** no idempotent phone command
   prepares a durable publication, even after successful verification.
4. **Client replay and status:** the phone UI does not retain idempotency keys
   across lost responses, and task reads/events do not expose coherent seal,
   verification, or publication status.
5. **Durable approvals:** no approval table, phone approval API, or restart-safe
   option contract exists.
6. **Workspace-`gh` binding validation:** a new immutable durable binding is
   persisted before the first live credential/repository check, so correcting a
   mistaken binding requires operator state recovery.
7. **GitHub selection:** installation discovery primitives exist, but selection
   UI and durable configuration update do not.
8. **Credential lifecycle:** offline backup foundations exist, but neither
   workspace-`gh` nor App credentials have automated rotation, revocation, and
   replacement-recovery contracts.
9. **Unhealthy-start classification:** rollback after a failed health/observer
   attach commits an ordinary pause intent, so a never-healthy start can later
   appear intentionally dormant rather than failed.
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
| Configuration | `internal/config/config.go`, `load.go`, `env.go`, `yamlnode.go`, `validate.go` |
| Remote/operator proxy | `internal/proxy/proxy.go`, `internal/proxy/gateway.go` |
| Pairing, CSRF, and device auth | `internal/proxy/pairing.go`, `internal/proxy/browser_security.go`, `internal/control/store.go` |
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
| Result sealing and user authorization | `internal/taskresultcoord/`, `internal/taskstore/seal_request.go`, `internal/taskstore/authorized_result.go` |
| Verification runner | `internal/verification/` |
| Verification coordinator | `internal/taskverification/` |
| GitHub App | `internal/githubapp/` |
| Workspace GitHub CLI authority | `internal/workspacegithub/`, `internal/runtime/exec.go` |
| Shared evidence/ref/JSON validation | `internal/evidence/`, `internal/gitref/`, `internal/jsoncanon/` |
| Durable publication transport | `internal/taskpublication/` |
| Durable publication coordinator | `internal/taskpublicationcoord/` |
| Legacy publication | `internal/publication/` |
| OpenCode contract tests | `integration/opencode-contract/` |
| Lifecycle evidence | `integration/lifecycle/` |
| Release evidence | `integration/release/` |
| Host backup/restore | `scripts/fern-host-backup.py`, `integration/release/run.sh` |
| Deployment | `deploy/systemd/`, `docs/DEPLOYMENT.md` |

`docs/TASK_MODEL.md` is the normative task-state and transaction contract.
`docs/GITHUB_INTEGRATION.md` owns GitHub details. `docs/SECURITY.md` owns the
security gap register.
