# Product Direction

This document is authoritative for Fern's product direction. It describes
planned boundaries, not implemented behavior. [ARCHITECTURE.md](./ARCHITECTURE.md)
and the code remain authoritative for the current system.

## Direction

Fern should become a self-hosted control plane for durable remote coding tasks,
using OpenCode as its first agent runtime.

> Submit work remotely, disconnect, return to the same task, inspect an
> attributable tested result, and publish it safely from a workspace that can
> stop, wake, reboot, and recover.

Fern should not become another model loop or rebuild OpenCode's coding UI.
OpenCode remains authoritative for conversations, tools, permissions,
questions, terminals, files, and diffs. Fern owns the durable journey around
those capabilities.

## Product Boundary

| Concern | Authority |
| --- | --- |
| Task receipt, delivery state, attempts, and cancellation intent | Fern |
| Conversation and tool execution | OpenCode |
| Compute lifecycle and recovery | Fern runtime backend |
| Repository content | Git and the workspace |
| Verification and result provenance | Fern |
| Push and pull request effects | OpenCode through workspace `gh`; optional audited Fern actions |
| Repository authorization | User-authenticated workspace `gh` |
| Infrastructure scheduling | Docker now; other backends later |

The workspace GitHub credential is intentionally available to OpenCode, matching
the Amp workflow. Explicit prompt intent authorizes the agent to use it. Fern
must not claim its own phone actions are an exclusive mutation gate; those
actions can add durable receipts and reconciliation only for effects they own.
The host-brokered GitHub App code remains current implementation, not target
product authority.

The central product model should become:

```text
Workspace
  contains Tasks
    contain Attempts
      map to OpenCode Sessions
      produce Results
      may produce Publications
```

A Fern task is not a duplicate transcript. It is the durable record proving
that Fern accepted an instruction, whether OpenCode received it, what attempt
ran, whether cancellation was requested, and which exact result was verified
or published.

## First Product Outcome

From a supported phone browser, a user can:

1. Pair one device through a private TLS route.
2. Submit one durable, idempotently accepted task and disconnect.
3. Return to its current status and OpenCode session.
4. Answer an approval or question.
5. Inspect the changed files and verification tied to an exact commit.
6. Publish one draft pull request through an effect-narrowed host broker; the
   supported release must replace the current broad host credential with a
   repository-scoped GitHub App installation token.
7. Stop, wake, restart, and restore without losing completed work.

The current field demo validates only a constrained portion of this outcome.
See [FIELD_DEMO.md](./FIELD_DEMO.md) for its exact claim.

## Current Evidence

The 2026-08-22 physical Android Chrome rehearsal materially reduced product
risk. It demonstrated private phone reachability, explicit pairing, use of the
official OpenCode UI, provider-backed edits/tests/local commits, reconnect after
phone lock, Wi-Fi-to-cellular continuity, and continuity across an orderly Fern
restart.

It did not complete release acceptance. Automatic idle pause failed, and
permission/question handling, device revocation, and exact-SHA draft PR
publication were not completed. The pairing harnesses and idle reducer were
subsequently repaired with automated coverage, but physical pause/wake and one
complete integrated evidence bundle remain open. Treat this as proof that the
core phone interaction is viable, not proof of the durable-task or
safe-publication product claims.

## Roadmap

`CP` marks the release critical path. `P` marks work that can proceed in
parallel after the named contract or dependency is stable.

### Phase 0: Restore Field-Demo Truth

| Track | Work | Exit criteria |
| --- | --- | --- |
| CP-0A Pairing gates (implemented) | Browser and lifecycle harnesses cover GET-confirm/POST-consume pairing, replay rejection, and restart survival | Re-run every local gate in `FIELD_DEMO.md` from one recorded integrated commit and image |
| CP-0B Lifecycle (automated gate implemented) | A late request after proven idle restarts the grace timer; the 14-scenario lifecycle harness proves grace timing, fenced pause, wake, and preserved state | Physical completed work reaches paused compute and wakes to the same session |
| CP-0C Runbook (partially implemented) | Paired-device/operator surfaces and revocation expectations are separated; pairing now preserves bounded self-asserted names | Every field step maps to one executable surface and expected result |
| CP-0D Rehearsal | After the GitHub hardening gate, run one bounded provider task, isolated GitHub rehearsal, and physical-phone sequence with retained evidence | All field steps have timestamped pass/fail evidence and cleanup |
| P-0E Browser matrix | Exercise the confirmation flow and unchanged OpenCode UI at phone viewport sizes | Supported browser routes load without auth loops or overflow |

Do not claim release acceptance or run another live publication while these
gates fail. Read-only contract work and isolated implementation can continue in
parallel without changing the frozen field-demo artifact.

### Phase 1: Prove The OpenCode Contract

1. **CP-1A (observed):** The exact pinned image preserves caller-selected
   session/message IDs, response-loss admission, finite ordered messages, and
   exact provider-turn deduplication across container replacement. Conflicting
   retry remains closed; there is no usable durable event cursor.
2. **CP-1B (partially observed):** Interrupt closes provider work and its durable
   evidence survives replacement without resurrecting execution. Before-
   admission and after-completion races remain open without a rollback claim.
3. **P-1C (boundary observed):** Live permission approval and form answers work,
   but pending and answered forms disappear on replacement. Form-backed input
   therefore enters `recovery_required`; pending-permission restart remains open.

Fern should persist deterministic OpenCode IDs and reconcile the pinned V2
contract, not infer deduplication from transcript text.

### Phase 2: Transactional Task Foundation

1. **CP-2A (contract drafted):** `TASK_MODEL.md` defines Task, Attempt, Receipt,
   Event, Approval, Result, Verification, and Publication state machines,
   including `uncertain` and `recovery_required`; it now records the observed
   top-level prompt/inbox/message profile and remaining restart assumptions.
2. **CP-2B:** Replace coarse workflow truth with a versioned SQLite store using
   foreign keys, WAL, bounded payloads, and explicit transaction boundaries.
3. **CP-2C:** Persist task, attempt, idempotency key, prompt hash, and exact
   OpenCode IDs before wake or delivery.
4. **P-2D:** Freeze task list/detail, result, and reconnect-cursor API contracts
   so phone UX can proceed independently.

SQLite is sufficient for the first single-host store. Do not maintain the JSON
workflow store and SQLite as competing task authorities.

### Phase 3: Durable Delivery And Reconciliation

1. **CP-3A:** Wake OpenCode and submit exactly one persisted prompt ID.
2. **CP-3B:** Re-scan finite inbox/message/form projections and persist exact
   projected IDs. Volatile OpenCode events may trigger scans but are not a Fern
   reconnect cursor.
3. **CP-3C:** Reconcile every nonterminal task and ambiguous delivery before
   accepting new effects after startup.
4. **CP-3D:** Persist cancellation intent before interrupt and expose only the
   state supported by upstream evidence.

### Phase 4: Phone Task Journey

1. **CP-4A:** Add a scoped paired-device task inbox with durable receipt,
   current status, and deep links into the authoritative OpenCode session.
2. **CP-4B:** Persist input-required shadows and reconcile answers without
   rebuilding OpenCode's permission/question UI.
3. **P-4C:** Add a transactional notification outbox for input required,
   completion, failure, and publication readiness.
4. **P-4D:** Build mobile delivery adapters only after the outbox semantics are
   stable.

### Phase 5: Attributable Result And Publication

1. **CP-5A:** Seal base SHA, candidate commit, dirty state, changed-file
   manifest, task/attempt/session IDs, and event boundary into one Result.
2. **CP-5B:** Run bounded approved verification and bind command, logs, exit
   status, timestamps, and tool identity to that exact commit.
3. **CP-5C:** Make publication consume a successful Result, not current `HEAD`,
   and require verified commit, pushed branch, and draft PR head to match.
4. **P-5D:** Present mobile-safe changed-file and verification summaries while
   deep-linking full coding details to OpenCode.

### Phase 6: Authorization And Recovery Completion

1. Integrate repository-scoped GitHub App onboarding and short-lived
   installation tokens from the parallel GitHub lane below.
2. Add versioned setup/resume hooks with bounded logs and failure states.
3. Automate backup, fresh-host restore, upgrade, rollback, and old-host fencing.
4. Test paused, idle-running, active-task, and interrupted-publication reboots
   on the target systemd host.
5. Add private previews and bounded CI/review follow-up only after exact PR
   identity is durable.

### Parallel Workstreams

| Workstream | Can start | Dependency and merge point |
| --- | --- | --- |
| Lifecycle and browser acceptance | Now | Independent release gate; must pass before any lifecycle claim |
| OpenCode exact-ID/restart contract harness (profile complete) | Caller IDs, response-loss and provider-turn retry, finite messages, live approvals/forms, restart behavior, interrupt, and missing durable event APIs are recorded for one digest | Pending-permission restart, form recovery policy, and cancellation race extensions gate their respective claims |
| GitHub prototype safety fixes (first tranche implemented) | Recovered branch, origin scheme, worktree/gitlink, URL, and post-create reconciliation tests now pass | Immutable repository identity, base SHA, full PR proof, and browser-control rehearsal still block live mutation |
| GitHub App authentication/onboarding foundation (implemented, not integrated) | RS256 signing, scoped token client, key parsing, private Manifest conversion, and atomic permission-protected credential storage exist | Callback state, installation selection, encrypted backup/rotation, and publisher integration join after shared publication identity is fixed |
| Task domain and admission store (implemented, not integrated) | Pure state machines plus CGO-free SQLite atomically persist task, prepared attempt, exact OpenCode IDs, receipt, and two events before effects | Delivery transitions, coordinator/HTTP wiring, backup/cutover, and form epoch recovery remain |
| Phone inbox UI prototype (implemented, fixture-backed) | Mobile/detail and desktop split views cover durable states with strict links/CSP | Replace fixtures with read-only task APIs after persistence projections freeze |
| Notification outbox | After task event vocabulary freezes | Delivery adapters proceed independently afterward |
| Exact-commit verification runner (implemented, not integrated) | Clean pre/postflight, bounded hashed output, timeout/cancel, and mutation detection are covered | Policy persistence, sandboxing, artifact transactions, and publication eligibility wiring remain |
| Release artifact foundation (implemented) | Deterministic metadata, checksums, packaged deployment assets, schemas, tamper/reproducibility harness, and CI exist | Signing, executable upgrade/rollback, and backup wait for stable SQLite/App state |
| Backup tooling | After SQLite migrations stabilize | Joins fresh-host release gate |
| GitHub onboarding UI | After manual App broker works | Joins first supported repository onboarding |
| Control-plane security | Device-only remote ingress, host-only operator listener, backend credential rejection, canonical auth regeneration, strict external origin/forwarding, active revoke, expiry cancellation, and backend negative probes are implemented | Browser-safe control-only operator surface, CSRF, pinned-image real TLS/WSS, and real SSE/PTY revocation acceptance remain |
| Backend authentication fail-closed gate (implemented) | Startup negative-probes missing and wrong credentials before accepting health | Exact packaged-image smoke must retain the `401` contract |

Parallel execution must preserve one authority per contract:

1. Freeze identifiers and transitions for Workspace, Task, Attempt, Result,
   Verification, Publication, repository ID, base SHA, operation ID, and actor
   before independent implementations consume them.
2. Give one track ownership of SQLite migrations and state transitions. Other
   tracks develop against contract fixtures rather than adding shadow stores.
3. Keep the App broker behind an internal interface so credential work does not
   depend on phone HTML or OpenCode delivery internals.
4. Let phone UI use recorded API fixtures after contracts freeze; do not let UI
   work invent task or publication states.
5. Merge each track only through its contract and fault-injection tests. A green
   package test without the cross-track invariant is not a merge gate.

The critical path is:

```text
field-demo truth
  -> pinned OpenCode exact-ID contract
  -> transactional task model
  -> persist before wake
  -> idempotent delivery and event reconciliation
  -> phone inbox and input-required flow
  -> exact-commit result and verification
  -> publication bound to that result
  -> GitHub App repository authorization
  -> restart, pause/wake, and restore acceptance
  -> one retained phone-to-tested-draft-PR journey
```

The GitHub App lane, phone UI, lifecycle repair, and notification design should
run concurrently where shown, but all join the release gate. A workspace
registry remains deferred until one complete durable task journey works.

### Later: Execution Backends

Docker remains the supported backend for the single-owner, trusted-host
product. Before adding another backend, separate the workspace controller from
Docker-specific status, endpoints, locks, storage, logs, and CLI operations.

Kubernetes becomes useful for a workspace fleet, multi-node scheduling,
distributed reconciliation, or a concrete enterprise deployment. It does not
provide durable task semantics or strong tenant isolation by itself. A shared,
hostile multi-tenant service also needs a sandbox runtime such as gVisor, Kata,
or a microVM boundary, plus external identity, secrets, egress, and audit.

For Grab, Fern should integrate its OpenCode-aware lifecycle and durable task
coordination with a Palana-style Kubernetes platform. Grab's platform should
continue to own pod scheduling, storage, ingress, workload identity, Vault,
egress policy, and audit. Fern should not run its Docker-daemon model inside an
agent pod.

## T3 Code Decision

T3 Code is a useful benchmark for mobile clients, durable command receipts,
event replay, terminals, files, Git views, and multi-provider orchestration. It
is not a drop-in frontend for Fern. Adopting its server would make T3 the thread
and application authority while Fern became its lifecycle supervisor.

Fern may run a time-boxed, version-pinned T3 experiment against a Fern-managed
OpenCode server. The experiment must prove OpenCode V2 compatibility, joint
T3/OpenCode quiescence, crash recovery, Git checkpoint ordering, publication
safety, and persistence. Do not fork T3, reproduce its private RPC contract, or
make it a release dependency before those results exist.

Fern can adopt T3-like interaction contracts without adopting T3's runtime:

- stable task identity;
- durable command receipts;
- monotonic reconnect cursors;
- explicit connection and execution states;
- mobile task and result views;
- deep links into the authoritative coding session.

## Not Now

- A second coding conversation UI.
- Native Fern mobile applications.
- Multiple agent-provider adapters.
- Multi-agent orchestration or a general workflow builder.
- Kubernetes as a requirement for the personal release.
- Direct Firecracker fleet management.
- Hostile multi-tenancy on ordinary shared-kernel Docker containers.

## Success Criteria

Fern is closer to Amp when the task, rather than the HTTP connection or
container, survives disconnects and lifecycle changes. The next release should
be judged by one complete phone-to-tested-PR journey, not by the number of
runtimes, schedulers, or infrastructure features it supports.

## Research Sources

- [Amp Orbs](https://ampcode.com/manual/orbs)
- [T3 Code architecture](https://github.com/pingdotgg/t3code/blob/main/docs/internals/overview.md)
- [T3 Code remote architecture](https://github.com/pingdotgg/t3code/blob/main/docs/internals/remote.md)
- [Kubernetes multi-tenancy](https://kubernetes.io/docs/concepts/security/multi-tenancy/)
- [Kubernetes RuntimeClass](https://kubernetes.io/docs/concepts/containers/runtime-class/)
- [Grab Palana architecture](https://engineering.grab.com/part-2-palana-architecture)
- [Firecracker](https://firecracker-microvm.github.io/)
