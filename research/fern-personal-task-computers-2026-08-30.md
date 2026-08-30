# Fern Personal Task Computers

**Date:** 2026-08-30

**Status:** substrate research supporting the bounded Docker-first experiment
**Scope:** one trusted owner, one self-hosted machine, several concurrent coding
tasks, and repositories the owner has explicitly trusted

## Current Direction

The selected product test is the Docker-first sequence below: one fresh clone,
one OpenCode server, and one state volume per attempt, with native UI attachment
and retained Git artifacts. Kubernetes is deferred until a real multi-node,
customer-cluster, workload-identity, network-policy, or stronger-runtime need
appears. The detailed k3s design remains available as a
[conditional backend proposal](../docs/TARGET_ARCHITECTURE.md).

This document is implementation research. It does not describe the implemented
system or override the maintained roadmap and Background Mode TODO.

## Recommendation

Build a Docker-first personal task computer experiment as a second execution lane
beside Fern's current persistent interactive workspace.

The product promise is:

> Send several coding tasks to hardware you own, close the laptop, and return to
> retained results, actionable questions, previews where configured, and draft
> pull requests.

Under that recommendation, the first release would not use Kubernetes or replace
OpenCode's UI. It would combine existing, well-understood primitives:

| Concern | Primitive |
| --- | --- |
| Durable control state | Fern and SQLite |
| Process and resource isolation | Docker Engine |
| Source and result identity | Git commits, trees, and bundles |
| Agent execution | One pinned OpenCode server per active attempt |
| Remote private access | Tailscale Serve |
| Rich interactive inspection | The harness's existing UI, when required |
| Review and delivery | GitHub draft pull requests |
| Attention delivery | One outbound notification adapter |
| Large retained output | Host-local content-addressed files |

In that recommendation, Kubernetes became appropriate only after a second host,
hostile trust domain, multi-user operation, placement, autoscaling, or stronger
network identity became a measured requirement. It still concluded that Fern
should consume Kubernetes Agent Sandbox rather than build a scheduler, CRD,
operator, or sandbox runtime.

## Why This Still Has Product Value

OpenHands, Cursor, Codex, Jules, Amp, and similar systems validate remote coding
tasks. They also raise the minimum bar. "Open an agent UI on another machine" is
not enough.

Fern's useful job is narrower:

1. A task is the unit of isolation, retention, cancellation, and delivery.
2. Several tasks can run against the same repository without sharing a mutable
   checkout.
3. The control plane survives browser disconnects and Fern restarts.
4. A completed task becomes a reviewable Git result or draft PR, not merely a
   transcript.
5. The owner can run the system on an existing Linux machine or home server
   without operating a cluster.

OpenHands Agent Canvas has multiple conversations, agents, tools, workspaces,
and automations. Its current open-source Helm deployment is nevertheless a
single-replica, shared-volume, trusted deployment rather than one isolated
Kubernetes workload per conversation. OpenHands Cloud and Enterprise add a
different outer control plane. This makes OpenHands an important baseline and a
source of patterns, but not a library Fern must embed to prove task-isolated
self-hosting.

## Product Boundary

### Build now

- one fresh checkout and one deterministic Docker resource set per attempt;
- a bounded queue with two or three concurrent attempts;
- durable provisioning, execution, cancellation, export, verification,
  publication, retention, and cleanup phases;
- exact base commit and image digest;
- bounded structured logs and terminal reason;
- a retained Git result even after compute exits;
- completion and input-required notification records;
- draft-PR delivery through Fern's existing GitHub App broker;
- a task list and task detail page optimized for phone and desktop;
- restart reconciliation using SQLite plus Docker inspection.

### Preserve beside it

- the existing persistent OpenCode workspace;
- the official OpenCode UI and its ownership of interactive sessions, forms,
  permissions, files, terminal, and diffs;
- Fern's current paired-device ingress and operator boundary;
- user-authorized sealing for the interactive lane.

### Defer

- Kubernetes and Agent Sandbox;
- multi-user accounts, organizations, and RBAC;
- hostile public-repository execution;
- automatic framework detection and arbitrary application previews;
- warm pools, VM snapshots, and distributed caches;
- Postgres, Redis, NATS, and Temporal;
- a general model gateway and general egress proxy;
- multiple agent harnesses in the first release;
- native mobile applications;
- a generic hosted sandbox API.

## Two Execution Lanes

Trying to force interactive OpenCode sessions and unattended processes through
one lifecycle contract would weaken both.

```text
Persistent home-workspace lane
  Fern proxy -> singleton workspace.Manager -> OpenCode server
  Completion authority: explicit owner seal

Disposable task lane
  Fern coordinator -> task checkout -> attempt OpenCode server -> exported result
  Completion authority: characterized session/result contract
```

The current persistent manager should remain unchanged during the first task
experiment. The disposable runner should be a separate package and coordinator.
Only extract a shared `EnvironmentProvider` after a second accepted environment
implementation proves the abstraction.

## Target Architecture

```text
Phone or desktop
       |
       | paired HTTPS over Tailscale Serve
       v
+------------------------- Fern --------------------------------+
| task API and UI                                               |
| SQLite task/attempt/effect/notification records               |
| disposable-attempt reconciliation coordinator                 |
| Git result verifier and GitHub publication reconciler         |
+-------------------------+-------------------------------------+
                          |
                          | Docker Engine API
                          v
             +------------+-------------+
             |                          |
     attempt A container        attempt B container
     checkout A only            checkout B only
     OpenCode server A          OpenCode server B
     bounded CPU/RAM/PIDs        bounded CPU/RAM/PIDs
             |                          |
             +------------+-------------+
                          |
                          v
        host-owned attempt directories and artifact store
             Git result | logs | test output | screenshots
                          |
                          v
                  verification -> draft PR
```

Docker is the compute supervisor. It is not the task source of truth. SQLite is
the source of truth, and reconciliation compares durable intent with Docker's
observed labels, container ID, image ID, state, and exit information.

## Attempt Resource Layout

Use deterministic names derived from an immutable attempt ID:

```text
container: fern-attempt-<id>
data:      fern-attempt-<id>-agent
checkout:  $FERN_STATE/tasks/<task-id>/attempts/<attempt-id>/repo
artifacts: $FERN_STATE/artifacts/sha256/<prefix>/<digest>
```

Every managed Docker object should carry labels for:

- Fern ownership;
- task ID and attempt ID;
- execution-contract version;
- immutable specification digest;
- repository identity and base commit;
- requested image digest.

Labels support adoption and drift detection. They do not replace database
records or runtime inspection.

## Execution Contract Comes First

The main technical gate is not Docker provisioning. It is proving what one
specific agent/session contract means when it stops active work.

The spike must bind all of these facts to one attempt:

- exact prompt digest;
- exact base commit and checkout;
- exact harness version and configuration;
- process/container identity;
- structured session or message identity where available;
- cancellation intent and acknowledgement;
- exit code and terminal reason;
- pending question or approval state;
- final repository state;
- behavior after Fern or Docker restart.

The spike must test:

1. successful change;
2. valid no-change completion;
3. model or provider failure;
4. tool failure;
5. timeout;
6. cancellation during model streaming and tool execution;
7. an approval or question that cannot be answered unattended;
8. Fern restart while the container remains active;
9. Docker restart while the task is active;
10. server loss or session error after repository mutation.

Idle state, an empty inbox, a disconnected event stream, and a stopped process
are not independently sufficient proof of success.

### Harness choice

Research pins used for the initial behavior matrix:

| Runtime | Candidate pin | License |
| --- | --- | --- |
| OpenCode | `v1.18.25`, commit `cb7d8b2f5e44876ef98b661dc10590c915af3a9f` | MIT |
| Codex CLI | `0.151.0`, commit `78c290807ce710180111df227df3b7a4fe845452` | Apache-2.0 |
| Claude Code | `@anthropic-ai/claude-code@2.1.251` | Proprietary commercial terms |
| ACP | `schema-v1.21.0`, commit `272bf799f35a258c6a4107a0410ed361e83683d3` | Apache-2.0 |
| OpenHands Agent Server | `v1.44.1`, commit `9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a` | MIT |

Pin the package/container and verify its reported version. Never parse a newer
unknown event stream best-effort in a production attempt.

Start with one pinned candidate, selected by black-box behavior rather than
feature count. The recommended first production candidate is an OpenCode
`v1.18.25` server per active attempt, submitted through its HTTP API and observed
through status, messages, and SSE. The server and its state directory survive a
Fern restart, unlike an in-process `opencode run` pipe. Fern must observe prompt
admission, active work, a subsequent idle transition, drained assistant parts,
and absence of session errors; initial idle, stream EOF, and server loss are not
completion.

Use `opencode run --format json` only as a contract comparison, smoke tool, or
future process adapter. In attach mode its CLI event loop is not a sufficient
durable audit channel. `codex exec --json` and Claude Code headless
`stream-json` are reasonable later process-per-attempt adapters, but Fern cannot
reattach their stdout pipes after its own crash. Recovery must inspect native
persisted session/thread state and start an explicit continuation rather than
silently replaying the original prompt.

OpenHands Agent Server is a later agent-backend option, not the first task
dependency. It would add a Python service, a second conversation authority, and
OpenHands-specific event and persistence semantics before Fern has validated
the user job.

### OpenCode attempt contract

Run the server inside the attempt container with task-specific credentials and
state, then use:

```text
GET  /global/health
POST /session
POST /session/{id}/prompt_async
GET  /session/status
GET  /session/{id}/message
POST /session/{id}/abort
GET  /event
```

Persist the exact OpenCode version, server/container identity, session ID,
prompt digest, last recovered message/event identity, and cancellation state.
SSE accelerates live updates but is not durable replay in stable V1; finite API
reads and repository facts are the recovery authority. A server crash during a
turn becomes runner-lost or recovery-required, not success and not automatic
prompt replay.

OpenCode V2 has a more suitable durable prompt, history, replay cursor, wait,
interrupt, and inbox API, but it remains beta. It deserves a pinned OpenAPI-hash
spike, not an unpinned production dependency.

ACP provides useful normalized terms for prompt turns, streamed updates,
permission decisions, cancellation, and stop reasons. It does not provide
daemon discovery, process supervision, network reconnection, mandatory session
persistence, or in-flight recovery. Use its vocabulary when useful, but do not
claim durability merely because an agent speaks ACP.

## Lifecycle And Effect Boundaries

Use explicit durable phases:

```text
admitted
  -> provision_prepared
  -> provision_started
  -> ready
  -> execution_prepared
  -> execution_started
  -> input_required | execution_terminal | cancellation_uncertain
  -> stop_prepared
  -> stopped_and_fenced
  -> export_prepared
  -> result_exported
  -> verification_started
  -> verified | rejected
  -> publication_prepared
  -> published | publication_recovery_required
  -> retention_active
  -> cleanup_prepared
  -> cleaned | cleanup_required
```

Persist the prepared phase before every external mutation. After a crash:

- inspect before creating a container with a deterministic name;
- inspect before stopping or deleting;
- use recorded process/container identity to reject stale observations;
- reconcile GitHub reads after an ambiguous push or PR creation;
- never interpret a missing resource as proof that an unrecorded mutation did
  not happen;
- never launch a replacement attempt automatically after a potentially
  mutating provider, GitHub, or tool call unless duplicate effects are safe.

## Repository And Result Design

### Checkout creation

For the first product, prefer a full private checkout under Fern's state root
over `git worktree`.

`git worktree` is fast but shares repository metadata, and mounting its `.git`
indirection into a container expands the filesystem coupling between attempts.
A full local clone is slower but easier to inspect, retain, back up, and delete.
Do not use local hardlinks across a trust boundary. Optimize cloning only after
measuring it.

The trusted host bootstrap should:

1. resolve the base ref to an exact remote commit;
2. create an attempt directory with owner-only permissions;
3. clone or copy Git objects without exposing host credentials to the worker;
4. check out the exact base commit;
5. set deterministic local Git identity and disable hooks for host-owned Git
   operations;
6. record the observed HEAD before starting compute;
7. bind-mount only that checkout into the task container.

### Project environment recipe

Keep environment setup explicit and versioned. A minimal project recipe should
name a pinned image, setup command, verification command, resource defaults, and
optional preview configuration. Hash the normalized recipe into the attempt
specification and show setup as a separate phase with its own logs.

```yaml
version: 1
image: ghcr.io/example/project-agent@sha256:...
setup: ["./script/fern-setup"]
verify: ["./script/fern-verify"]
resources:
  memory: 8Gi
  cpus: 2
```

The recipe is trusted repository code and runs inside the attempt environment,
never in Fern's host process. Start with a pinned general-purpose Fern image and
an optional setup command. Add a policy-filtered Dev Container adapter only if
several real repositories already maintain `devcontainer.json` and duplicate
setup becomes the dominant friction.

Treat language and build caches as disposable accelerators. Share no writable
checkout or agent state. Introduce repository/image/lockfile-scoped caches only
after measuring cold setup, and make cache loss affect latency rather than
correctness or result recovery.

### Result capture

Stop the container before trusted collection. Then capture both successful and
failed work:

- exact base commit;
- resulting tree and commit, if clean and committed;
- deterministic manifest and digest;
- patch or Git bundle containing required objects;
- bounded logs and structured terminal record;
- changed-path policy result;
- image and execution specification digests.

The final portable boundary should be a Git bundle or equivalent content
artifact. The first local implementation can retain the host checkout until
publication completes, but result records must not imply that commit IDs alone
preserve object bytes.

If the harness leaves a dirty tree, Fern must choose and document one policy.
The recommended disposable-task policy is a trusted post-stop snapshot commit produced
with hooks disabled and a fixed Fern identity, while retaining whether the agent
itself committed. This preserves useful work without pretending the agent
reported success.

## Data Model Delta

The current Attempt model requires OpenCode session and message IDs and the
database enforces one effecting attempt per workspace. Because the first
isolated runner is also OpenCode, keep those IDs rather than prematurely
generalizing every agent field. Add immutable `execution_mode` to task and
attempt, and add a one-to-one isolated-run journal containing:

```text
execution_mode              home_workspace | isolated_task
execution_contract_version
runner_kind                 opencode_server
runtime_kind                docker
environment_id
environment_generation
provider_resource_id
requested_image_digest
observed_image_id
specification_digest
process_started_at
process_exit_code
process_exited_at
terminal_classification
retention_until
```

The global unique effecting-attempt index should remain for
`home_workspace` attempts and exclude `isolated_task` attempts. The isolated-run
journal should enforce one environment per attempt and one current owner per
environment. If Fern later accepts Codex, Claude, ACP, or OpenHands as production
backends, perform a separate evidence-driven migration that makes native runtime
identities conditional. Do not add nullable generic fields only for hypothetical
adapters.

Large artifacts do not belong in SQLite. Add an artifact table containing:

```text
id, attempt_id, kind, sha256, size, media_type, storage_key, created_at
```

Keep task state, artifact metadata, hashes, and publication phases in SQLite.
Store bytes in an atomic host-local content-addressed directory. Add an
`ArtifactStore` interface only when a second backend such as S3 is actually
implemented.

### Initial code shape

Keep the new code narrow:

```text
internal/attemptrunner   concrete disposable Docker/OpenCode lifecycle
internal/taskisolated    claims and reconciles isolated-run phases
internal/taskartifact    local immutable artifact ingestion/materialization
```

The existing `taskdelivery` and `taskexecution` contracts provide OpenCode API
behavior to reuse or extract, but the isolated coordinator must resolve its own
attempt target and must never receive the singleton `workspace.Manager`.
Result, verification, and publication should receive an attempt-scoped
repository/materialized artifact instead of the global configured path.

## Concurrency

Parallelism should be bounded, not inferred from how many goroutines can be
started.

Initial policy:

- configurable global maximum, default `2`;
- configurable per-repository maximum, default `2`;
- FIFO admission with explicit queued state;
- CPU, memory, PIDs, execution deadline, and output limits per attempt;
- disk-space admission before provisioning;
- no shared writable checkout;
- no automatic retry after ambiguous external effects;
- a new attempt ID for every explicit retry.

SQLite remains sufficient because one Fern process owns scheduling. Claim work
in short transactions, perform external work outside transactions, and commit
observations with revision or lease checks.

## User Experience

The best experience is not a smaller IDE. It is a calm work queue connected to
the existing specialist surfaces.

### First-run setup

Provide an opinionated setup path:

```text
fern init
  -> check Docker
  -> select repository
  -> validate one agent profile
  -> configure GitHub App publication or choose local-only results
  -> configure Tailscale Serve
  -> optionally configure one notification destination
  -> run a real disposable smoke task

fern doctor
  -> report exact failing dependency and remediation
```

Do not ask the owner to understand containers, volumes, provider adapters,
artifact stores, or retention schemas during normal setup.

### Task creation

Default form:

- repository;
- task title or prompt;
- base branch;
- optional "open draft PR when checks pass" toggle.

Keep image, model, budget, setup command, retention, and resource limits under
advanced project defaults.

### Task list

Use product states rather than internal phases:

```text
Queued | Starting | Working | Needs you | Checking | Ready | Failed | Canceled
```

Each card should answer:

- what is being changed;
- where it is running;
- how long it has run;
- whether it needs attention;
- what useful output exists;
- what the next safe action is.

Keep `uncertain`, lease owner, resource UID, execution phase, and reconciliation
detail in an expandable diagnostics panel.

Represent state on separate axes rather than forcing every concern into one
ever-growing enum:

```text
execution: Queued | Setting up | Working | Verifying | Stopped
attention: None | Needs input | Needs approval
outcome:   Review ready | No changes | Failed | Canceled | Expired
retention: Active | Archived | Deleting
```

The primary badge can be derived from these fields. The underlying facts remain
available for recovery and filtering. Prefer `Review ready` over `Success`; code
still requires human responsibility.

### Task detail

Desktop priority:

- current phase and concise chronology;
- bounded live logs;
- changed files and diff summary;
- verification output;
- artifacts and preview links;
- exact base and result commits;
- cancel, retry as new attempt, retain, delete, and open draft PR;
- deep link to the native agent UI when an interactive session exists.

Include a trust panel that states where code executes, where state is stored,
what leaves the host, which credential names are available, current network
policy, exact base commit, result identity, and cleanup deadline. Do not replace
these facts with a generic security label.

Phone priority:

- task capture;
- one actionable attention item;
- cancel or retry;
- answer a bounded question only when the harness contract supports it;
- small diff, screenshot, or verification summary;
- open GitHub or the agent UI for richer review.

### Notifications

Start with one adapter and a transactional outbox. A notification is a derived,
retryable delivery of a durable event, never the only record of that event.

Useful notification kinds:

- needs input;
- completed with result;
- verification failed;
- task failed or timed out;
- draft PR created;
- cleanup or recovery requires operator action.

`ntfy` is a reasonable personal-product first target because it has hosted and
self-hosted modes, mobile clients, simple HTTP publication, actions, and topic
authentication. A generic webhook can follow. Browser push should not be the
first implementation because service workers, VAPID keys, notification
permissions, and iOS installation behavior add a separate product surface.

### Previews

Do not promise automatic previews for arbitrary repositories. Introduce an
explicit project recipe only after task-to-PR works:

```yaml
preview:
  command: ["npm", "run", "dev", "--", "--host", "0.0.0.0"]
  port: 3000
  readiness_path: /
```

Run previews as a separate retained process/container derived from the stopped
result, not as an untracked child of a completed task process. Route previews
through Fern with task authorization, explicit service identity, bounded
lifetime, and no direct public container port.

## Libraries And Projects

### Use now

| Project | Use |
| --- | --- |
| Docker Engine and supported Moby Go `client`/`api` modules | Dynamic containers, limits, logs, wait, inspect, stop, remove, volumes, events, and labels. Migrate Fern's older `github.com/docker/docker` module when upgrading the Engine integration |
| System Git | Exact refs, checkout, manifests, bundles, and publication; preserve Fern's sanitized command boundary |
| `modernc.org/sqlite` | Durable single-process control state; already used by Fern |
| Tailscale Serve | Private TLS and reachability; keep identity and task authorization in Fern |
| Existing Fern proxy | Paired-device policy, credential stripping, wake/routing policy, and streaming |
| Existing GitHub App code | Host-owned credentials and reconciled draft-PR publication |
| `ntfy` HTTP API | First optional notification destination; no Go client dependency is required |
| Litestream and restic | Off-host SQLite replication plus independent artifact/configuration backups; neither participates in live task authority |

### Evaluate after the core path works

| Project | Potential use | Reason to defer |
| --- | --- | --- |
| Dev Container CLI/spec | Repository-defined setup portability | Broad feature surface, Docker orchestration overlap, and untrusted lifecycle hooks |
| Coder `envbuilder` | Build dev-container environments in Kubernetes | Most useful once Kubernetes and repository-defined images are accepted requirements |
| BuildKit/Buildx | Reproducible custom images and registry cache | Prebuilt pinned images are sufficient for the first tasks |
| OpenHands Agent Server | Additional native/ACP agent backend | Adds Python operations and another conversation authority |
| OpenTelemetry Go and Collector | Cross-phase traces and metrics | Structured logs are enough until the path is stable; add before distributed operation |
| S3-compatible storage | Durable off-host artifacts | Local content-addressed storage is simpler for one machine |
| Caddy | Public single-VM TLS when Tailscale is unsuitable | Do not operate both ingress options by default |

### Do not compose into the first product

| Project | Decision |
| --- | --- |
| Docker Compose | Static application composition does not replace dynamic per-attempt reconciliation |
| Daytona, Coder, DevPod | Useful products and references, but adopting one makes Fern a thin workflow wrapper around another workspace control plane |
| Dagger | Adds a pipeline SDK and engine without solving task authority or agent completion |
| Bazel REAPI/build farms | Far beyond measured build-cache needs |
| NATS or Temporal | Duplicate delivery/state semantics Fern already owns for one process |
| Vault, Envoy, OPA, Cilium | Palana-scale controls with disproportionate personal-product operations |
| Firecracker or Kata | Require a VM-capable Linux deployment and substantially more image/network operations |
| OpenHands Canvas components | Coupled to Agent Server events, settings, routes, and state; use Canvas as a benchmark, not Fern's UI kit |

## Docker Security Profile

The first version is for trusted owner-selected repositories. State this clearly.
Still apply inexpensive containment:

- unprivileged container user;
- drop all Linux capabilities;
- `no-new-privileges`;
- Docker's default seccomp profile;
- no Docker socket, host PID/network namespace, host devices, or Kubernetes
  credentials;
- mount only the attempt checkout and task-specific data;
- CPU, memory, PIDs, output, disk, and time limits;
- host-owned GitHub publication credentials;
- image digest recording and observed image ID validation;
- task-specific backend credential rather than one global browser credential;
- stop compute before trusted result collection;
- redact known credentials from retained structured logs;
- reconcile deletion rather than relying on an in-memory timer.

Use Docker's rotating `local` log driver on the host. Fern should still capture
bounded attempt logs as immutable artifacts; Docker logs are a recovery source,
not the retained product record. Subscribe to Docker events only as a wake hint
and perform periodic list/inspect reconciliation because the event stream is
not a durable queue.

Provider keys currently enter the trusted workspace. A later narrow Fern Gateway
can keep model credentials on the host and issue a task-scoped Fern token. This
is valuable defense in depth, but it should not block the initial task-value
test. Do not claim Palana-style proxy-only secrets or default-deny egress until
all bypass paths have been removed and tested.

## What To Copy From Palana

Palana is a proprietary Kubernetes platform operating hundreds of agents. Fern
should copy its durable ideas, not its component count.

| Palana principle | Personal Fern interpretation |
| --- | --- |
| Agent is an acting workload | Treat every task process as separately identified and bounded |
| Control plane remains outside agent | Fern owns stop, cancellation, retention, credentials, and publication from the host |
| Stop compute, keep state | Stop the task container while retaining checkout, agent state, result, and artifacts |
| Isolation is the unit of trust | One checkout and resource set per attempt; no writable sharing |
| Credential use differs from credential reading | Keep GitHub publication credentials host-side; add model mediation later |
| Egress is a control point | Record current open egress honestly; add policy only with a concrete threat model |
| Self-service beats secure friction | `fern init`, templates/defaults, and one-click task submission hide infrastructure |
| Observe lifecycle externally | Reconcile Docker and Git facts rather than trusting agent self-report |

Fern does not need Palana's namespace per agent for one trusted owner. A Docker
resource set plus an attempt ID is the analogous operational boundary, not an
equivalent hostile-workload security boundary.

## When Kubernetes Becomes Correct

Do not migrate because the product now has "multiple tasks." Docker can run
several isolated task containers on one host.

Adopt Kubernetes only when one or more of these recur:

- tasks must be placed across multiple machines;
- a failed node must be replaced automatically;
- capacity must scale beyond one host;
- different tasks require Linux, GPU, or other node classes;
- users or repositories cross trust boundaries;
- network policy and workload identity are product requirements;
- warm pools materially improve measured startup latency;
- control-plane replicas require Postgres-backed claims.

Then use:

```text
Fern -> Kubernetes Agent Sandbox core -> Sandbox per attempt
     -> gVisor RuntimeClass initially
     -> PVC for task state
     -> trusted verifier Job after Sandbox stop
     -> managed Postgres and object storage only when replicas require them
```

Fern remains task authority. Agent Sandbox owns environment lifecycle.
Kubernetes schedules pods. None of them should silently become authority for
task completion or GitHub publication.

## Backup And Host Recovery

Do not treat Docker layers or active task directories as the primary backup
format.

Recommended personal deployment:

- Litestream continuously replicates SQLite to an off-host S3-compatible target;
- a daily SQLite online-backup snapshot provides an independently restorable
  checkpoint;
- restic backs up the artifact CAS, configuration, audit records, and daily
  database snapshot;
- active disposable checkouts remain recoverable execution state, not promised
  durable output until result export completes;
- a quarterly clean-host restore verifies the actual recovery path.

After restore, Fern reconciles database records with managed Docker resources.
A recorded running attempt with no matching container becomes interrupted or
recovery-required; it is not silently declared failed or automatically replayed
after possible external effects.

## OpenHands Lessons

Useful patterns to adopt:

- append-only typed events plus separately persisted current state;
- agent/runtime separation behind an HTTP and WebSocket boundary;
- backend registry rather than baking one deployment into UI concepts;
- conversation pause, resume, fork, and retention as explicit actions;
- responsive browser UX and deep links to specialist views;
- plugins/MCP for optional capabilities;
- local, Docker, and remote workspaces behind tested implementations.

Patterns not to copy initially:

- a broad agent framework;
- file, terminal, editor, desktop, browser, settings, secrets, profiles, and
  automation APIs in Fern;
- all-in-one single-container packaging that obscures task isolation;
- frontend state coupled to one agent server's event schemas;
- shared API keys as a user authorization system;
- a large automation server before task completion is reliable.

## Current Fern Delta

The main coupling points in the current tree are:

| Coupling | Current location |
| --- | --- |
| One process-level workspace manager | `cmd/fern/up.go:226-271` |
| Manager explicitly owns exactly one immutable runtime spec | `internal/workspace/manager.go:95-164` |
| Runtime spec carries one host repository path | `internal/runtime/runtime.go:85-95` |
| Container and volume identity derive from one workspace name | `internal/runtime/provision.go:51-77`, `internal/runtime/provision.go:147-163` |
| Proxy target resolution has no task/environment identity | `internal/proxy/proxy.go:21-27`, `internal/proxy/proxy.go:205-276` |
| Attempts require OpenCode IDs at admission | `internal/taskstore/migrations.go:182-211` |
| Database permits one effecting attempt per workspace | `internal/taskstore/migrations.go:253-254` |
| Delivery and observation consume the singleton manager | `cmd/fern/tasks.go:166-193` |
| Result, verification, and publication use one configured host path | `cmd/fern/tasks.go:334-350`, `cmd/fern/tasks.go:369-421` |
| UI deep-links every task to an OpenCode session on one backend | `internal/proxy/task_ui.go:24-43` |

This is why the first implementation should add an attempt-scoped runner beside
the singleton manager rather than parameterize every current lifecycle path at
once.

### Reuse

- task IDs, actor snapshots, receipts, events, revisions, and idempotent
  admission;
- cancellation intent, conservative uncertainty, and replacement-attempt
  semantics;
- Docker ownership labels, drift detection, limits, health checks, and
  intentional-stop classification;
- paired-device authentication and control/backend credential separation;
- exact Git result manifest and sealing semantics;
- host verification;
- GitHub App token custody and publication reconciliation;
- retry and component-health infrastructure.

### Change

- add isolated-task execution mode and a one-to-one isolated-run journal;
- replace the one-effecting-attempt-per-workspace constraint for isolated tasks;
- add attempt-scoped checkout, environment, artifact, and retention records;
- add a disposable Docker coordinator separate from `workspace.Manager`;
- make result collection accept an attempt repository rather than one global
  configured path;
- make verification and publication materialize the selected attempt result;
- add a transactional notification outbox;
- add task detail and diagnostics projections;
- route interactive and preview services by task/environment identity only when
  those capabilities are implemented.

### Missing today

- authoritative unattended completion;
- one environment per task;
- concurrent effecting attempts;
- portable result bytes;
- artifact API and storage;
- notifications;
- retention and deletion reconciliation;
- application preview lifecycle;
- durable approval answers;
- per-task provider credentials or mediated model access;
- default-deny egress;
- physical acceptance on the intended always-on host.

## Implementation Sequence

### G0: Contract spike, 3 to 5 days

- pin one OpenCode server profile;
- run the ten contract cases listed above;
- write captured fixtures and a behavior matrix;
- reject the harness if restart, cancellation, or terminal state cannot be
  bounded honestly.

Exit: one documented completion authority or an explicit decision to retain
manual sealing.

### G1: Serial disposable task, 2 to 3 weeks

- one host-owned fresh checkout;
- one deterministic Docker container;
- stop/fence and capture result;
- retain logs and checkout;
- recover correctly after Fern restart;
- no changes to the persistent interactive manager.

Exit: one real repository task survives restart and produces a reviewable exact
result.

### G2: Parallel task-to-PR, 3 to 5 weeks

- bounded queue and concurrent attempts;
- schema constraints for environment ownership;
- artifact metadata and Git bundle;
- existing verification and publication adapted to attempt materialization;
- cancellation, timeout, retry-as-new-attempt, and cleanup reconciliation;
- one completion notification adapter.

Exit: three tasks against one repository run concurrently and produce distinct,
correctly based draft PRs without checkout interference.

### G3: Product UX, 2 to 3 weeks

- first-run `init`/`doctor` flow;
- task detail page and concise chronology;
- diff/test/artifact projections;
- retention controls and disk-pressure feedback;
- mobile notification deep links.
- Litestream/restic setup and one clean-host recovery drill.

Exit: the normal path requires no Docker or database knowledge.

### G4: Conditional richer attach and preview work, 2 to 4 weeks

- authenticated routing to the already per-task OpenCode server only if users
  repeatedly need live attach beyond Fern's task detail;
- explicit preview recipe and isolated preview process only if review is blocked
  without it;
- durable question answering only after the selected harness exposes a safe
  context-bound contract.

An aggressive functional prototype is four to six full-time weeks. A reliable
task-to-PR MVP with schema upgrade, restart/cancellation fault injection,
artifact-backed verification, cleanup, and recovery is more honestly eight to
twelve weeks. Rich previews and generalized interactive routing follow only
after dogfood.

## Expected Implementation Size

The repository audit gives this order-of-magnitude estimate:

| Area | Production LOC | Test/fixture LOC | Risk |
| --- | ---: | ---: | --- |
| Migration, models, scans, and transitions | 1,400-2,000 | 1,400-2,000 | Very high |
| Attempt runner and local Docker implementation | 1,150-1,700 | 1,150-1,700 | High |
| Coordinator and restart reconciliation | 900-1,300 | 1,000-1,500 | Very high |
| Artifact store and materializer | 500-800 | 600-900 | High |
| Verification/publication adaptation | 450-750 | 500-800 | High |
| Config, API, UI, startup, and observability | 650-1,050 | 750-1,200 | Medium |
| Integration and release fixtures | - | 800-1,200 | High |
| **Total** | **5,050-7,600** | **6,200-9,300** | **High** |

Parallel container creation is not the expensive part. Most of this work exists
to prevent false success, duplicate starts, premature cancellation
acknowledgement, artifact loss, interactive-workspace interference, and unsafe
cleanup after crashes.

## Acceptance Tests

The MVP is not complete until it passes these on the intended host:

1. Three tasks from the same base run simultaneously in separate checkouts.
2. One task succeeds, one fails after modifying files, and one is canceled.
3. All useful work is retained with the correct terminal classification.
4. Fern restarts during provisioning, model streaming, result export, and PR
   creation without duplicating an unsafe effect.
5. Docker restarts during an active attempt and Fern reconciles the observed
   container state.
6. A stale coordinator cannot overwrite a replacement attempt.
7. Result collection cannot race a live writer.
8. GitHub publication reconciles an ambiguous push and PR response.
9. Retention cleanup survives interruption and never deletes an active attempt.
10. A phone receives one actionable completion/failure notification and opens
    the authenticated task detail.
11. Rebooting the host resumes reconciliation without manual database repair.
12. Disk exhaustion, image pull failure, provider failure, and OOM produce
    bounded, useful states.

## Product Gates

Continue after the MVP only if real dogfood shows:

- parallel isolation is used repeatedly rather than demonstrated once;
- at least half of submitted tasks reach a useful result without laptop repair;
- draft-PR or retained-result delivery saves a recurring manual handoff;
- setup and environment drift do not dominate task time;
- users return through notifications rather than babysitting logs;
- local self-hosting remains preferable to a managed cloud agent for the target
  users.

Narrow or stop if most work requires interactive repair, users prefer one
persistent workspace, GitHub Actions plus an agent CLI is equivalent, or
environment setup consumes more effort than parallelism saves.

## Sources

Primary sources should be rechecked and pinned during implementation:

- [OpenHands repository](https://github.com/OpenHands/OpenHands)
- [OpenHands Software Agent SDK](https://github.com/OpenHands/software-agent-sdk)
- [OpenHands Agent Canvas architecture](https://docs.openhands.dev/openhands/usage/agent-canvas/architecture)
- [OpenCode CLI](https://opencode.ai/docs/cli/)
- [OpenCode server API](https://opencode.ai/docs/server/)
- [OpenCode V2 API](https://opencode.ai/v2/docs/api)
- [Codex non-interactive mode](https://developers.openai.com/codex/non-interactive)
- [Claude Code headless mode](https://docs.anthropic.com/en/docs/claude-code/headless)
- [Agent Client Protocol](https://agentclientprotocol.com/protocol/overview)
- [Kubernetes Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox)
- [Docker Engine API](https://docs.docker.com/reference/api/engine/)
- [Development Containers specification](https://containers.dev/implementors/spec/)
- [Coder envbuilder](https://github.com/coder/envbuilder)
- [ntfy documentation](https://docs.ntfy.sh/)
- [Litestream](https://litestream.io/)
- [restic](https://restic.net/)
- [Palana Part 1](https://engineering.grab.com/palana-part-1-secure-platform-for-ai-agents)
- [Palana Part 2](https://engineering.grab.com/part-2-palana-architecture)
- [Grab Agent Platform Part 1](https://engineering.grab.com/how-grab-builds-and-runs-ai-agents-at-scale)
- [Fern strategy](../docs/STRATEGY.md)
- [Fern architecture](../docs/ARCHITECTURE.md)
- [Fern task model](../docs/TASK_MODEL.md)
