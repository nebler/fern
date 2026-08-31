# OpenCode Background Mode: Current TODO

**Status:** active product experiment

**Updated:** 2026-08-31
**Scope:** one trusted owner, one private Fern host, configured trusted
repositories, and at most two concurrent Background Runs

## Product Promise

> Start a Background Run from OpenCode, leave the initiating device, reopen or
> steer the exact remote OpenCode session elsewhere, and retain the exact Git
> result after its disposable runtime is deleted.

This is the executable checklist for current work. The
[Roadmap](../docs/ROADMAP.md) owns sequencing and later gates. The
[Background Mode design](../docs/BACKGROUND_MODE.md) owns the proposed data
model, lifecycle, artifact, routing, and fault semantics. Code and
[Architecture](../docs/ARCHITECTURE.md) remain authoritative for implemented
behavior.

## Product Language

Use these terms in user-facing interfaces:

- **Background Run:** one remote unit of work started by the user.
- **OpenCode session:** the exact live native session for that run.
- **Result:** the selected Git commit and retained reconstruction artifact.

Keep task rows, attempts, generations, environments, routes, and export journals
as implementation details. Two simultaneous pieces of work are two Background
Runs, each with one current attempt. A retry or replacement creates another
internal attempt generation; it does not silently replace the session a user has
already opened.

## Decisions Already Made

- [x] OpenCode remains the first and only execution harness for this experiment.
- [x] The primary launch surface is an OpenCode TUI plugin with `/fern`.
- [x] Fern, not the plugin or initiating OpenCode process, owns durable run
  admission and lifecycle.
- [x] A run uses a fresh full clone, distinct OpenCode state, and one
  authoritative remote OpenCode session.
- [x] The legacy `experimental_workspace` plugin API is not a product
  dependency.
- [x] The current embedded V2 workspace-provider API is not the local-to-remote
  handoff mechanism; it may be revisited only for a deliberate always-on remote
  OpenCode home-server mode.
- [x] MCP and skills are optional launch conveniences, not the primary effectful
  confirmation or authentication boundary.
- [x] Fern exposes a harness-neutral run boundary, but does not implement a
  generic runtime framework before a second harness proves demand.
- [x] Dirty local worktrees are rejected initially. Fern does not silently copy
  local files or imply that a local conversation moved.
- [x] Results use retained Git objects and must survive container, OpenCode
  volume, and checkout deletion.

## Current Baseline

- [x] One persistent OpenCode V2 workspace can stop, wake, and survive Fern
  restart.
- [x] Durable task admission commits Fern and exact OpenCode identities before
  delivery.
- [x] Cancellation fences new Fern-owned effects.
- [x] Explicit sealing binds a clean Git result to one persistent-lane task and
  attempt.
- [x] Exact-result host verification and optional GitHub App publication exist
  for the persistent lane.
- [x] The owner completed the private browser and phone demonstration for the
  persistent workspace.
- [x] The repository and release fixture use Go 1.27.
- [x] The serial lane creates isolated per-run clones, volumes, containers,
  runtime credentials, OpenCode sessions, and a fixed private route origin.
- [x] Scoped plugin credentials and harness-neutral create, read, list, stop,
  and open server operations exist.
- [ ] Capacity remains one; multiple concurrent OpenCode workspaces do not
  exist.
- [ ] Exact Git object retention after checkout deletion does not exist.
- [x] A minimal compiled `/fern` plugin package is pack- and loader-qualified
  against OpenCode `1.18.16`; npm publication remains pending.
- [ ] The retained-result server operation does not exist.

Do not convert the persistent `workspace.Manager` into a scheduler or change its
retention behavior. Add Background Runs as a separate disposable lane.

## Immediate Next Todos

### Retained Exact Result

- [ ] Add the durable schema and migration for seal intent, writer-fence proof,
  export journal, retained artifact identity, reconstruction state, and result
  projection.
- [ ] Add an explicit idempotent seal operation; do not infer completion from
  idle state, silence, EOF, process exit, or container exit.
- [ ] Positively prove the exact current writer inactive before reading Git
  state, and reject stale attempt generations throughout collection.
- [ ] Select or create one clean result commit rooted at the exact admitted base
  without trusting agent-authored metadata as authority.
- [ ] Build a self-contained Git bundle and immutable manifest containing the
  exact repository, base, result commit, tree, changed object identities,
  execution identity, and artifact digest.
- [ ] Atomically ingest the bundle and manifest into a private host-owned
  content-addressed store with bounded files, restrictive modes, fsync, and
  digest verification.
- [ ] Verify the bundle and independently materialize the exact result before
  committing `result_ready` or deleting the container, volume, and clone.
- [ ] Implement the scoped idempotent `/result` projection and reconstruction
  path without exposing host paths, credentials, or mutable URLs.
- [ ] Make verification and optional publication consume only the independently
  materialized retained result.
- [ ] Add crash and response-loss tests before and after every seal, export,
  ingestion, result commit, cleanup, reconstruction, and publication boundary.

### Capacity Two

- [ ] Replace the serial fixed origin with a bounded configured private HTTPS
  port set, while preserving one immutable listener/origin per active run.
- [ ] Admit at most two active runs and queue excess work deterministically under
  explicit CPU, memory, PID, disk-admission, and route capacity.
- [ ] Prove two same-repository runs share no writable Git, OpenCode, credential,
  endpoint, route, terminal, event, result, or cleanup state.
- [ ] Restart Fern and independently reconstruct both exact runtime and route
  bindings without replaying either prompt.

### Installed Journey

- [x] Package the pinned `/fern` plugin with confirmation, scoped authorization,
  create, runs, open, stop, and result actions over the existing run API. The
  result action remains unavailable until retained-result authority lands.
- [ ] Qualify actual Tailscale multi-port private TLS, browser cookie acceptance,
  SSE, WSS, revocation, mobile sleep/wake, and cross-device native session use.
- [ ] Promote the qualified OpenCode source image to an immutable registry
  digest before external distribution.
- [ ] Dogfood six real runs, including two concurrent pairs and one forced
  runtime failure, before applying the product kill gates.

## Parallel Execution Plan

Use isolated branches or worktrees for every lane. One integrator owns shared
state-machine files, compatibility fixtures, merge order, final documentation,
and release verification. Lane agents do not update this checklist directly.

| Lane | Scope | Exclusive ownership | Start gate | Merge gate |
| --- | --- | --- | --- | --- |
| A: result authority | Seal command, schema, claims, lifecycle, receipts, result projection | `internal/taskstore`, migration and compatibility fixtures | Immediate | Contract tests and upgrade checks pass |
| B: artifact engine | Git commit selection, manifest, bundle, CAS ingestion, verification, materialization | New Background Run result package and its unit tests | Result contract types fixed | Deterministic artifact and reconstruction tests pass |
| C: plugin | `/fern` setup, confirmation, create, runs, open, stop, and later result UX | Plugin package, fake backend, plugin tests | Immediate | Existing run API contract tests pass |
| D: qualification | Runtime failures, cleanup interruption, private TLS/device evidence, release gates | Integration harnesses and deployment evidence | Immediate | Real harness and negative gates pass |
| Integrator | Coordinator wiring, API composition, conflict resolution, docs, final gates | `internal/backgroundruncoord`, `internal/runapi`, `cmd/fern`, shared docs | Lanes A and B mergeable | Full unit, race, upgrade, release, and real Docker suites pass |

### Merge Order

1. Freeze and merge the retained-result contract and schema authority.
2. Merge the artifact engine after it implements that exact contract.
3. Wire seal, collection, retention, cleanup, reconstruction, and `/result` in
   the coordinator and run API.
4. Merge result fault injection and compatibility evidence.
5. Merge the plugin after `/result` is stable; create, runs, open, and stop may
   land earlier if independently useful.
6. Complete physical private-TLS qualification and registry-digest promotion.
7. Begin capacity-two scheduling and bounded per-run origins only after every
   serial retained-result fault gate passes.

### Conflict Rules

- [ ] Only Lane A edits the active taskstore schema and migration checksums until
  its merge gate passes.
- [ ] Only the integrator edits coordinator phase dispatch while result authority
  and artifact effects are being joined.
- [ ] Capacity-two work does not modify the serial lifecycle before retained
  result reconstruction is proven after runtime deletion.
- [ ] Every lane rebases on the last merge gate before handoff and includes exact
  verification commands and residual risks.
- [ ] A Git-clean merge is insufficient: lifecycle ordering, idempotency,
  ownership, credential redaction, and crash recovery must be reviewed again at
  integration.

## Work Order

Serial disposable execution, restart recovery, and private native-session
routing are locally implemented. Complete the remaining milestones in this
order:

1. Seal, retain, delete, and reconstruct one serial run's exact Git result.
2. Pass the remaining restart, lost-response, cancellation, export, disk, and
   cleanup fault gates.
3. Run two isolated disposable OpenCode workspaces concurrently.
4. Complete the installed `/fern` and physical cross-device journey.
5. Dogfood six real runs and apply the product kill gates.

## 1. `/fern` And First-User Onboarding

The first release has no Fern account sign-up. One trusted owner first operates
a private host:

```text
fern init --repo /path/to/repository
fern up --config fern.yaml
fern doctor --phone
```

The intended OpenCode journey is:

```text
opencode2 plugin @fern/opencode@<pinned-version>
opencode2 /path/to/repository
/fern Fix the cancellation race and add a regression test
```

### Plugin Spike

- [x] Pin one documented OpenCode TUI plugin API and compatible OpenCode version.
- [x] Package a minimal compiled `@fern/opencode` TUI plugin and qualify its
  packed archive with the official OpenCode `1.18.16` loader.
- [x] Register `/fern` and native run, runs, open, stop, and result actions.
- [ ] Let `/fern <instruction>` launch directly and `/fern` without arguments
  open a native setup/run dialog.
- [x] Keep read, open, stop, and result actions useful with both OpenCode's local
  service and an explicit `--server` connection. Create intentionally remains
  local-service-only until repository identity is server-authoritative.
- [x] Use a fake Fern backend first so client UX does not depend on Docker work.

### Authentication

- [x] Ask for the private Fern HTTPS origin on first use.
- [x] Display a short-lived verification URL and user code.
- [x] Require approval through an existing trusted operator or paired-device
  channel.
- [x] Issue a revocable credential scoped only to run create, read, stop, open,
  and result access.
- [x] Keep the credential out of `opencode.json`, prompts, repository files,
  logs, and plugin display state.
- [ ] Add host compatibility and readiness checks before setup succeeds.
- [x] Support explicit disconnect and server-side credential revocation.

### Repository Confirmation

- [x] Read the canonical Git remote, exact `HEAD`, branch display name, and dirty
  state without asking the model.
- [ ] Match the canonical remote to a repository explicitly configured on Fern.
- [ ] Prove the exact base commit is reachable from Fern's configured remote.
- [x] Reject dirty, unborn, ambiguous, unconfigured, or unreachable repository
  state with an actionable message.
- [x] Do not upload a patch, archive, or local untracked file in the first slice.

### Launch Confirmation

- [x] Show host, repository, exact base OID, clean state, pinned OpenCode profile,
  and complete instruction before allocating anything.
- [x] Generate an idempotency key before submission.
- [x] Show success only after Fern returns a committed Background Run ID.
- [x] Store only safe local correlation data; never cache an endpoint as
  authority.
- [x] Resolve the current authoritative OpenCode endpoint from Fern on every
  `open`.
- [ ] Kill the TUI immediately after acceptance and prove the run remains
  readable and stoppable.

Example confirmation:

```text
Run on Fern?

Host          fern-home
Repository    owner/repository
Base          bcd397b...
Working tree  clean
Runtime       OpenCode <pinned profile>
Prompt        Fix the cancellation race and add a regression test

[ Run in background ]  [ Cancel ]
```

## 2. Product And OpenCode Contract Gates

### Native Value

- [ ] Pin the exact OpenHands Agent Canvas, Agent Server, custom ACP adapter,
  OpenCode, image, and API identities used for comparison.
- [ ] Run the same real owner task through OpenHands custom ACP and official
  OpenCode.
- [ ] Compare conversation fidelity, configuration, skills, plugins, models,
  permissions, questions, files, terminal, diffs, steering, reconnect, and
  restart behavior.
- [ ] Record which native OpenCode capabilities are repeatedly useful rather
  than merely available.

**Stop:** do not build the disposable lane if OpenHands is satisfactory and the
official OpenCode experience adds no repeatedly used value.

### Pinned OpenCode Contract

- [x] Select one exact newer OpenCode V2 source commit, package version, image
  digest, and API schema.
- [x] Verify startup, health, authentication, and caller-selected session and
  prompt IDs.
- [x] Verify prompt admission and exact read-only reconciliation after response
  loss.
- [ ] Verify event/history, questions, forms, permissions, terminals,
  interruption, and deep links.
- [x] Verify the same authoritative session reopens instead of creating a
  replacement.
- [x] Characterize process and container restart, including every volatile fact
  that disappears.
- [x] Treat session wait or completion APIs as non-authoritative unless they pass
  repeated restart tests.
- [x] Keep explicit user sealing unless a restart-safe positive terminal result
  is proven under the collection fence.

**Stop:** reject the candidate if Fern cannot reconcile exact identities or open
the official session without replaying mutations.

## 3. One Serial Disposable Background Run

### Durable Intent

- [x] Commit run ID, internal attempt generation, repository identity, exact
  base, instruction digest, image digest, clone/volume/container identities, and
  OpenCode session/prompt IDs before external effects.
- [ ] Give provision, prompt delivery, stop, export, and cleanup explicit started
  phases before I/O.
- [x] Reconcile only by exact identifiers after restart or response loss.
- [x] Fence every writer and observer with the current attempt generation.

### Disposable Environment

- [x] Add a separate Docker provider for Background Runs.
- [x] Create one private full clone; do not use a linked worktree or shared
  writable Git object directory.
- [x] Create a distinct OpenCode state volume, container, runtime credential,
  and endpoint identity.
- [x] Pin the image by digest and run with the existing unprivileged UID/GID.
- [x] Bound CPU, memory, PIDs, disk admission, wall time, and retained logs.
- [x] Publish one fixed private HTTPS port origin while only one run can execute.
- [x] Remove access before deleting or replacing the exact runtime.
- [x] Re-attest the exact container process epoch and published port before each
  proxied request and again after opening a new upstream connection.

### Admission And Takeover

- [x] Create or adopt the preselected OpenCode session ID exactly once.
- [x] Admit the preselected prompt ID exactly once.
- [x] Never resend a prompt because a request timed out or Fern restarted.
- [x] Project only conservative run states: `queued`, `setting_up`, `working`,
  `needs_you`, `canceling`, `uncertain`, `result_ready`, `failed`, and
  `cleanup_required`.
- [x] Do not infer success from idle, EOF, process exit, container health, or an
  empty inbox.
- [x] Resolve the exact official OpenCode session deep link and preserve its
  root-relative UI, API, SSE, and WSS behavior through the paired route.
- [ ] Physically qualify inspection, questions, permissions, steering,
  terminals, files, and diffs through the installed private HTTPS route.
- [ ] Disconnect every initiating client for at least ten minutes and reopen the
  same session from another device.
- [x] Make stop durable before interruption and keep `canceling` until exact
  writer inactivity is positively established.

## 4. Retain The Exact Result

- [ ] Acquire an exclusive stop/seal fence before reading the repository.
- [ ] Select or create one clean result commit based on the exact admitted base.
- [ ] Create an immutable manifest containing repository, base, result commit,
  tree, changed paths/modes/blob IDs/sizes, image digest, attempt generation,
  OpenCode IDs, terminal reason, and artifact digest.
- [ ] Export a self-contained Git bundle with every object required for clean
  reconstruction.
- [ ] Ingest the bundle and manifest atomically into host-owned retained storage
  before reporting `result_ready`.
- [ ] Run `git bundle verify` and materialize an independent clean checkout.
- [ ] Run verification against the materialized commit, never the agent checkout.
- [ ] Delete the container, OpenCode volume, and clone.
- [ ] Materialize again from retained artifacts and compare the exact commit,
  tree, and manifest.
- [ ] Preserve useful interrupted work only with an explicit partial-result
  label; never call it completed automatically.

## 5. Fault Gates For The Serial Run

- [ ] Fern exits before and after each external mutation starts.
- [x] Docker create succeeds but its response is lost.
- [x] OpenCode session creation succeeds but its response is lost.
- [x] Prompt admission succeeds but its response is lost.
- [x] Fern restarts while OpenCode is working or waiting for input.
- [x] OpenCode restarts while a permission or question is pending.
- [x] A same-container process replacement is denied before coordinator
  reconciliation and cannot inherit the routed Basic credential.
- [ ] The container exits normally, fails, and is OOM-killed.
- [ ] Cancellation races provisioning, admission, model work, seal, and export.
- [x] A stale attempt tries to observe, stop, seal, export, verify, clean, or
  publish.
- [ ] Export is interrupted before and after host artifact ingestion.
- [ ] Cleanup is interrupted after each individual resource deletion.
- [ ] Disk exhaustion occurs during clone, OpenCode state growth, and artifact
  ingestion.
- [ ] Every accepted result reconstructs after runtime deletion.

## 6. Two Concurrent OpenCode Workspaces

Concurrency is introduced only after the serial lane passes its fault gates. The
user sees two Background Runs; each run owns one current disposable OpenCode
workspace and session.

### Isolation

- [ ] Admit at most two active runs with explicit CPU, memory, PID, and disk
  capacity checks.
- [ ] Queue excess runs deterministically instead of overcommitting the host.
- [ ] Give each run a distinct full clone, OpenCode state volume, container,
  runtime credential, endpoint/port, session ID, prompt ID, and generation.
- [ ] Prohibit writable repository, Git common-directory, OpenCode database,
  credential, cache, terminal, or child-process sharing.
- [ ] Start two runs from the same repository and base commit and prove their
  changes remain independent.

### Endpoint Mapping

- [ ] Extend the serial fixed private origin to bounded per-run endpoint mapping
  only when capacity two is introduced.
- [ ] Map each durable run to its exact current attempt generation and OpenCode
  endpoint.
- [ ] Re-resolve mapping on every open and reject stale generation links.
- [ ] Preserve the official UI, API, SSE, and WSS behavior through private TLS.
- [ ] Prove two tabs or devices can remain attached to different runs without
  session, cookie, event, terminal, or port crossover.
- [ ] Remove a run's mapping before stopping or deleting its runtime.
- [ ] Never let a replacement attempt inherit an old endpoint grant.

### Independent Lifecycle

- [ ] Kill and restart run A without interrupting run B.
- [ ] Stop or seal run A while run B continues writing.
- [ ] Restart Fern and reconstruct both exact mappings without prompt replay.
- [ ] Delete one runtime and reconstruct its result while the other remains live.
- [ ] Seal and retain both results without artifact or publication mix-up.

## 7. Harness-Neutral Fern Boundary

Keep these operations independent of the launching client:

```text
create background run
read/list background runs
stop background run
resolve live session
read/materialize retained result
```

The create request contains repository identity, exact base commit, instruction,
idempotency key, and requested execution profile. The response contains a
durable run ID. Live session locators and retained results are resolved from that
ID.

- [x] Expose scoped create, read, list, stop, and open semantics without a
  harness-specific runtime framework.
- [ ] Add retained-result read and materialization semantics to complete the
  harness-neutral boundary.
- [ ] Make the human Fern CLI and OpenCode plugin use the same API semantics.
- [ ] Allow a future harness plugin or MCP server to call the same durable API.
- [ ] Keep authentication, repository authorization, capacity, lifecycle,
  generations, writer fencing, and artifacts Fern-owned.
- [ ] Keep OpenCode session IDs, event recovery, permissions, questions,
  interruption, and deep-link handling in the pinned OpenCode profile.
- [ ] Do not claim that conversations or live tool state transfer across
  harnesses.
- [ ] Do not add a second execution harness during this experiment.

A second harness becomes eligible only after real demand and must independently
prove exact admission, continuation after client exit, restart-safe observation,
authoritative attachment or logs, positive interruption, writer inactivity, and
the same retained Git result contract. Extract a shared runtime interface only
from two working implementations.

## 8. Dogfood Gate

Run at least six real Background Runs over two weeks after the disposable lane
works. Include two concurrent pairs and one forced runtime failure.

Record for each run:

```text
repository and exact base
instruction and run class
submission and inspection devices
background duration
reconnect and native-session-open latency
question, permission, steering, stop, or seal actions
truthful state after reconnect and restart
laptop or SSH repair required
useful result produced
bundle verification and reconstruction after deletion
verification and draft-PR outcome
duplicate prompt, result, or publication
setup/repair time and estimated time saved
failure notes
```

Continue only if:

- [ ] At least two genuinely background-worthy runs recur per week.
- [ ] At least 60% produce useful work without laptop-side repair.
- [ ] The official OpenCode UI is used meaningfully at least weekly and for 25%
  or more of runs.
- [ ] Native OpenCode preserves at least two repeatedly used capabilities that
  OpenHands Canvas does not.
- [ ] Every accepted result reconstructs after runtime and clone deletion.
- [ ] Fern restart causes no accepted prompt loss or replay.
- [ ] Two same-repository runs never share writable state or cross routes.
- [ ] Setup and repair cost less time than delegation saves.
- [ ] The owner prefers the complete journey to OpenHands ACP and one managed
  cloud-agent alternative.

Stop or narrow if native attachment is rarely useful, manual sealing makes the
workflow unattractive, pushed branches are always sufficient, or a generic
agent platform is required to make the first demonstration useful.

## Explicitly Not Now

- Kubernetes, k3s, Agent Sandbox, or remote worker pools.
- Hosted multi-tenancy, public sign-up, organizations, billing, or RBAC.
- A replacement OpenCode conversation, terminal, file, or diff UI.
- OpenCode workspace-provider integration as the launch mechanism.
- A second execution harness or generic ACP runtime layer.
- Automatic session migration or dirty-worktree transfer.
- Automatic success inferred from inactivity or disconnection.
- Fern Gateway, model routing, accounting, Labs, schedules, or previews.
- More than two concurrent Background Runs.

## Definition Of Done

The experiment is ready for one external installer only when:

- `/fern` installs, pairs, confirms, and durably starts a run without exposing a
  long-lived credential in OpenCode configuration;
- the initiating OpenCode process and device can disappear immediately after
  acceptance;
- another device opens the exact authoritative remote OpenCode session;
- two isolated runs can execute concurrently without writable or route sharing;
- Fern and OpenCode restart without prompt replay or false completion;
- stopping proves the exact writer is inactive;
- every accepted result reconstructs from retained artifacts after deleting its
  container, OpenCode volume, and clone;
- verification and optional publication consume only the materialized exact
  result;
- normal use and cleanup require no Docker, filesystem, or database repair; and
- the dogfood gates pass.
