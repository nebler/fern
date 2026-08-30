# OpenCode Background Mode TODO

**Status:** active product experiment

**Updated:** 2026-08-30
**Scope:** one trusted owner, one always-on private Linux host, configured trusted
repositories, and at most two concurrent disposable attempts

## Product Promise

> Submit work to an always-on private host, leave, inspect or steer the exact
> native OpenCode session from another device, and retain an exact recoverable
> Git result after the runtime and checkout disappear.

This checklist owns the active implementation sequence. Code and
[Architecture](../docs/ARCHITECTURE.md) remain authoritative for shipped
behavior. [Roadmap](../docs/ROADMAP.md) owns promotion and later-work gates.
The [Background Mode Goal Design](../docs/BACKGROUND_MODE.md) defines the target
components, data model, Go APIs, concurrency patterns, graphics, and demo.

## Current Baseline

- [x] One persistent OpenCode V2 workspace can stop, wake, and survive Fern
  restart.
- [x] Durable task admission commits Fern and exact OpenCode identities before
  delivery.
- [x] Cancellation fences new Fern-owned effects.
- [x] Explicit user sealing binds a clean Git result to one task and attempt.
- [x] Exact-result host verification and optional GitHub App publication exist.
- [x] The private browser and phone flow has been demonstrated by the owner.
- [x] Upgrade the repository and release fixture from Go 1.24 to Go 1.27.
- [ ] The current implementation still has one persistent checkout, one
  OpenCode state volume, and one effecting task at a time.
- [ ] Exact Git object retention after checkout deletion is not implemented.

The Background Mode lane must be added beside the persistent
`workspace.Manager`. Do not convert that manager into a generic scheduler or
change its retention semantics during the experiment.

## Phase 1: Prove The Native Difference

Complete this comparison before productizing a second execution lane.

- [ ] Pin the exact OpenHands Agent Canvas, Agent Server, OpenCode, container,
  and API schema identities used for comparison.
- [ ] Run one supported OpenHands ACP agent as a control.
- [ ] Run OpenCode through OpenHands custom ACP using the same repository, model,
  task class, and host.
- [ ] Compare Canvas and official OpenCode for conversation fidelity, settings,
  skills, plugins, model selection, permissions, questions, files, terminal,
  diffs, steering, and restart behavior.
- [ ] Start two independent tasks and verify their checkout, state, credential,
  cache, process, and port boundaries.
- [ ] Disconnect the initiating client for at least ten minutes, reconnect from
  another device, and measure time to truthful current state.
- [ ] Restart Canvas and Agent Server separately and record what survives.
- [ ] Delete the runtime/workspace and determine whether the exact useful Git
  result remains independently reconstructable.
- [ ] Record every capability for which native OpenCode is materially better,
  equally good, or worse.

**Stop:** do not build the Fern lane if OpenHands custom ACP is satisfactory for
real owner work and the official OpenCode UI adds no repeatedly used value.

## Phase 2: Pin The OpenCode Contract

The production pin is not a proxy for a newer V2 build. Select one exact
candidate and retain source/build identity with every observation.

- [ ] Verify server startup, health, authentication, and deterministic task URL
  routing.
- [ ] Verify exact caller-selected session and prompt IDs.
- [ ] Verify prompt admission and read-only reconciliation after a lost response.
- [ ] Verify inbox, message, event/log, question, form, permission, terminal, and
  interrupt behavior.
- [ ] Verify process restart and complete container replacement.
- [ ] Characterize `/api/session/{sessionID}/wait`; treat unavailable, volatile,
  or process-local behavior as non-authoritative.
- [ ] Prove that Fern can reopen the same authoritative session in the official
  UI rather than creating a second session.
- [ ] Record every process-epoch fact that disappears and project uncertainty or
  `recovery_required` when loss is positively detected.
- [ ] Keep explicit user sealing unless a restart-safe positive terminal result
  is proven twice under the collection fence.

**Stop:** reject the candidate if exact identity, restart reconciliation, or
official UI attachment cannot be made reliable without replaying mutations.

## Phase 3: Disposable Native Prototype

Build throwaway product-test code first. Keep it local to one Docker daemon and
one configured repository.

### Durable Identity

- [ ] Add an environment/attempt record that commits before Docker or Git I/O:
  task ID, attempt generation, repository identity, exact base commit, prompt
  digest, image digest, checkout identity, state-volume identity, container
  identity, port/route identity, and exact OpenCode session/prompt IDs.
- [ ] Give every provisioning, prompt-delivery, stop, export, and cleanup effect
  an explicit started phase before external I/O.
- [ ] Make all reconciliation exact-ID reads; never allocate or replay because a
  response was silent.
- [ ] Fence every writer with the current attempt generation.

### Environment Provider

- [ ] Create a separate disposable Docker provider rather than extending
  `workspace.Manager` into multi-workspace scheduling.
- [ ] Resolve and verify the configured repository and exact base commit before
  provisioning.
- [ ] Create a full private clone per attempt. Do not use linked worktrees or a
  shared writable Git common directory.
- [ ] Create a distinct OpenCode state volume, container, host port, and runtime
  credential per attempt.
- [ ] Pin the OpenCode image by digest and run as the existing unprivileged UID.
- [ ] Bound CPU, memory, PIDs, disk admission, wall time, and retained logs.
- [ ] Start one attempt serially before enabling concurrency.
- [ ] Add a deterministic authenticated route from immutable task ID to that
  attempt's official OpenCode UI.
- [ ] Remove the route before deleting or replacing the exact runtime.

### Observation And Control

- [ ] Project only conservative states: `queued`, `setting_up`, `working`,
  `needs_you`, `canceling`, `uncertain`, `result_ready`, `failed`, and
  `cleanup_required`.
- [ ] Do not map container health, process exit, stream EOF, idle, or an empty
  inbox to task success.
- [ ] Deep-link to the official OpenCode UI for inspection, steering,
  permissions, questions, terminal, files, and diffs.
- [ ] Make explicit stop durable before interrupting OpenCode or stopping the
  container.
- [ ] Keep `canceling` until exact writer inactivity is positively established.
- [ ] On Fern restart, enumerate owned environments, attest identity, restore
  routes and observers, and never resend an admitted prompt.
- [ ] On unknown or mismatched identity, quarantine the environment and require
  recovery instead of adopting or deleting it.

### Exact Result Retention

- [ ] Acquire an exclusive stop/seal fence before inspecting the repository.
- [ ] Require the configured repository identity, exact base, supported object
  format, and a clean worktree after creating the selected result commit.
- [ ] Produce an immutable manifest containing base commit, result commit, tree,
  changed paths/modes/blob IDs/sizes, image digest, attempt generation, OpenCode
  IDs, terminal reason, and artifact digest.
- [ ] Export a self-contained Git bundle that contains every object needed to
  materialize the selected result from an empty clone.
- [ ] Ingest the bundle and manifest atomically into host-owned retained storage
  before marking the result ready.
- [ ] Validate the bundle with `git bundle verify` and materialize it into a
  clean independent checkout.
- [ ] Run configured verification against the materialized exact commit, never
  against the agent checkout.
- [ ] Delete the container, state volume, and checkout, then repeat clean
  materialization and compare the exact commit/tree/manifest.
- [ ] Preserve partial useful work under an honest interrupted or failed result;
  never label it completed automatically.

## Phase 4: Fault Gates

Automate these before treating the prototype as a durable lane.

- [ ] Fern exits after environment intent commits but before Docker create.
- [ ] Docker create succeeds but its response is lost.
- [ ] OpenCode session creation succeeds but its response is lost.
- [ ] Prompt admission succeeds but its response is lost.
- [ ] Fern restarts while the prompt is running.
- [ ] OpenCode restarts while a question or permission is pending.
- [ ] Container exits normally, fails, and is OOM-killed.
- [ ] Cancellation races prompt delivery, model work, result sealing, and export.
- [ ] A stale attempt tries to observe, seal, export, clean, verify, or publish.
- [ ] Git bundle export is interrupted before and after host ingestion.
- [ ] Cleanup is interrupted after each resource deletion.
- [ ] Disk exhaustion occurs during clone, OpenCode state growth, and artifact
  ingestion.
- [ ] Two attempts start from the same base and edit different files without any
  writable checkout, OpenCode state, or route collision.
- [ ] Runtime and checkout deletion still yields 100% clean reconstruction for
  every accepted result.

## Phase 5: Productize Only After Prototype Acceptance

- [ ] Implement serial durable provisioning and reconciliation first.
- [ ] Add bounded queueing and explicit capacity/disk admission.
- [ ] Add concurrency of two only after serial fault gates pass.
- [ ] Add host-owned content-addressed artifact storage with retention and
  garbage-collection reconciliation.
- [ ] Adapt current verification to materialize exact attempt artifacts.
- [ ] Adapt App publication to push only the verified materialized result; keep
  workspace-`gh` outside Fern's brokered-effect guarantees.
- [ ] Add one transactional notification outbox and one destination only after
  background tasks demonstrate an attention need.
- [ ] Add retention views and cleanup controls without building a replacement
  conversation UI.
- [ ] Add metrics for queue time, provisioning, prompt admission, reconnect,
  native takeover, active duration, export, verification, cleanup, retained
  bytes, recovery, and laptop-side repair.
- [ ] Extend backup/restore to include environment authority and retained result
  artifacts, then prove replacement-host materialization.

## Dogfood Gate

Run at least six real owner tasks over two weeks after the disposable prototype
works. Include two concurrent pairs and at least one forced runtime failure.

Record for every task:

```text
date and repository
task class and exact base
runtime: OpenHands | Fern persistent | Fern disposable
submission and inspection devices
background duration
reconnect and native-UI-open latency
why the native UI was opened
question, permission, steering, or cancellation performed
truthful state after reconnect/restart
laptop or SSH repair required
useful result produced
manual seal acceptable
bundle verified and reconstructed after deletion
verification and draft-PR outcome
duplicate prompt, result, or publication
setup/repair time and estimated time saved
failure notes
```

Continue only if:

- [ ] At least two genuine background-worthy tasks recur per week.
- [ ] At least 60% produce useful work without laptop-side repair.
- [ ] The official OpenCode UI is used meaningfully for at least 25% of tasks and
  at least weekly.
- [ ] Native OpenCode preserves at least two repeatedly used capabilities that
  OpenHands Canvas does not.
- [ ] Every accepted result reconstructs after runtime and checkout deletion.
- [ ] Fern restart causes no accepted prompt loss or replay.
- [ ] Two same-repository attempts never share writable state.
- [ ] Setup and repair cost less time than delegation saves.
- [ ] The owner prefers the complete journey to OpenHands ACP and one managed
  cloud-agent alternative.

Stop or narrow if OpenHands is equivalent, native UI attachment is rarely used,
manual sealing makes the workflow unattractive, ordinary pushed branches are
always sufficient, or Kubernetes/new UI/generic-agent scope is needed to make
the first useful demonstration.

## Explicitly Not Now

- Kubernetes, k3s, or Agent Sandbox.
- Remote runner pools or hosted multi-tenancy.
- Generic ACP or multiple-agent abstractions.
- A replacement OpenCode conversation UI.
- A native mobile application.
- Fern Gateway, provider routing, or model accounting.
- Fern Labs, schedules, workflow automation, or previews.
- Automatic success inferred from inactivity.

Kubernetes becomes eligible only after a concrete second-node,
customer-cluster, workload-identity, NetworkPolicy, RuntimeClass, or measured
capacity requirement. Gateway becomes eligible only for credential custody,
budgets, accounting, routing, fallback, or an explicit portfolio objective.

## Definition Of Done

Background Mode is ready for one external installer only when one signed Fern
release can be installed on clean Ubuntu, run two isolated attempts, survive
Fern and OpenCode restarts without mutation replay, reopen each exact official
OpenCode session, stop and fence each writer, retain and independently
reconstruct every accepted Git result after deletion, verify and optionally
publish the exact selected commit, enforce bounded cleanup, restore on a
replacement host, and pass the dogfood gate above.
