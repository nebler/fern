# Fern Through Seven Independent Lenses

> **Document status:** Supplemental dated review pinned to Fern `7c470d6`, not
> current implementation or operating guidance. See
> [docs/DOCUMENTATION.md](./docs/DOCUMENTATION.md).

**Review date:** 2026-08-15  
**Repository:** `github.com/nebler/fern` at `7c470d6`

## Method And Limits

Seven independent agents reviewed Fern, one for each requested perspective.
These are analytical lenses informed by publicly associated principles and
work. They are not impersonations, quotations, endorsements, or claims about
what any named person would literally say.

Each review inspected the repository rather than relying only on planning
documents. The shared evidence includes the lifecycle manager, reverse proxy,
OpenCode activity watcher, Docker runtime, CLI, tests, architecture documents,
roadmap, and current delivery state. The worktree was already dirty and the
research documents were untracked; this review does not treat those files as
released product behavior.

## Executive Synthesis

- Fern is currently one coherent program: a foreground, single-workspace,
  OpenCode-aware Docker lifecycle proxy.
- Its strongest technical contribution is the stop boundary: close admission,
  wait for held requests, re-inspect runtime state, query all OpenCode sessions,
  and stop only when current evidence says all are idle
  (`internal/workspace/manager.go:251-299`).
- Its strongest product moment is returning through one stable private address
  and finding the retained OpenCode session ready after an ordinary authorized
  request.
- The roadmap risks converting that coherent program into a control-plane
  framework through hooks, previews, artifacts, receipts, webhooks, providers,
  and broader identity concerns.
- Installation and operation are not yet product-grade: the documented path
  builds a development image and runs from source in the foreground
  (`README.md:38-61`).
- Lifecycle truth is richer internally than externally. `fern status` mainly
  reports a Docker snapshot, while generation, watcher state, causal transition,
  and safe recovery remain hidden (`cmd/fern/commands.go:92-117`).
- Wake latency, memory reclaimed, daemon-backed failure behavior, remote attach,
  phone use, reboot recovery, and comparative value against plain OpenCode plus
  Tailscale need reproducible evidence.
- The defensible audience is one trusted independent developer, one trusted
  repository, and one Docker host they already operate. Users satisfied with an
  always-running OpenCode server should not install Fern.
- Fern is presently strongest as an OSS portfolio project and potentially a
  niche community utility. There is no evidence yet for a standalone business.
- The next work should be CI, versioned distribution, one supervised private
  deployment, a canonical client origin, causal status, raw measurements, and
  outside-user validation. Do not build hooks or a platform in the meantime.

## Rob Pike-Informed Lens

**Verdict:** Fern is one coherent program under pressure to become an
accumulating control-plane framework.

### Strongest Aspect

The implementation has a compact responsibility: one process owns one Docker
workspace, one stable ingress point, and one conservative lifecycle boundary.
OpenCode retains conversation, models, history, and UI; Fern composes with the
official client through `fern attach` rather than replacing it
(`cmd/fern/attach.go:32-43`).

The concurrency work is purposeful. Wake operations are service-owned and
coalesced (`internal/workspace/manager.go:141-177`). Watch attempts are canceled
and joined (`internal/watch/controller.go:126-185`). A single supervisor owns
activity history, connection state, and epoch interpretation
(`internal/watch/supervisor.go:142-188`). Unknown state fails safe by leaving
compute running.

### Serious Challenge

Stopping one container already spans interacting Docker, manager, and watcher
state machines. `Manager` combines mutexes, token channels, completion channels,
counters, generations, and flags (`internal/workspace/manager.go:51-72`). The
watcher has a separate epoch from the proxy endpoint generation. This is tested
complexity, but the proof is already larger than the apparent product.

Planning documents then add setup/resume hooks, diagnostics schemas, previews,
artifacts, verification, and webhook ingestion. Those are separate products,
not natural refinements of one lifecycle abstraction.

### Keep, Remove, Defer

**Keep:** request admission, active-session tracking, `seenBusy`, the final
authoritative status query, ownership labels, spec-drift refusal, persisted
pause intent, and native OpenCode attachment.

**Remove or collapse:** duplicate generation identities, the ordinary direct
backend URL, speculative Kubernetes mapping, and public commands that bypass the
normal lifecycle model. `fern resume` starts Docker without restoring the
controller, proxy, watcher, or idle policy (`cmd/fern/commands.go:42-90`).

**Defer:** hooks, previews, artifacts, receipts, webhooks, OIDC, multiple
workspaces, Kubernetes, and provider adapters.

### Falsification Test

Compare one week of plain `opencode serve` plus Tailscale with one week of Fern.
Record manual lifecycle actions, interrupted work, reconnect friction, memory
pressure, and failures. If Fern does not remove a repeated burden, freeze it as
a portfolio artifact.

### Smallest Product

Keep `up`, `attach`, `status`, and `down`. One foreground process owns one
workspace. Ordinary authorized work wakes it. A verified busy-to-idle boundary
allows it to stop. Docker owns containers, Tailscale owns private networking,
Git owns source state, and OpenCode owns interaction.

## Jony Ive-Informed Lens

**Verdict:** Fern's hidden lifecycle is careful and intentional, but its visible
experience gives implementation machinery the same hierarchy as the user's
workspace.

### Strongest Aspect

Wake is subordinate to the user's intent. The user makes an ordinary request;
Fern restores compute, confirms authenticated health, attaches the exact watcher
generation, and only then forwards traffic (`internal/workspace/manager.go:203-239`).
Concurrent callers share one wake. Sleep is similarly respectful: ambiguity
keeps compute running rather than risking unfinished work.

### Serious Challenge

The first success teaches Docker before demonstrating continuity. Users build an
image, copy YAML, run a foreground Go process, and open another terminal. Ready
output gives the transient direct endpoint and stable proxy similar prominence
(`cmd/fern/up.go:161-162`). Wake appears as unexplained delay. Failures collapse
to `503 failed to wake workspace` even when Fern knows the responsible layer
(`internal/proxy/proxy.go:52-70`).

Vocabulary alternates among stop, sleep, pause, resume, wake, and running. The
primary interface should express human states such as `awake`, `asleep`,
`waking`, and `needs attention`; Docker vocabulary belongs in verbose evidence.

### Keep, Remove, Defer

**Keep:** one stable origin, ordinary request-driven wake, quiet conservative
sleep, retained sessions, and the native OpenCode UI.

**Remove:** the dynamic endpoint from normal output and `fern resume` from the
ordinary journey. Avoid surfacing container IDs, watcher epochs, fingerprints,
and event firehoses by default.

**Reveal when relevant:** `Waking demo`, `Ready in 3.0s`, `Asleep, work
preserved`, or `Needs attention, OpenCode exited unexpectedly`, followed by one
safe action.

**Defer:** hooks, dashboards, artifact inboxes, and preview products until the
central return-to-work transition is excellent.

### Falsification Test

Give new users terminal and phone tasks without explaining Fern's architecture.
They should install it, return to a sleeping workspace, identify a failure, and
remove it without using Docker vocabulary. Failure means the reduction is only
cosmetic.

### Smallest Product

Install one binary, receive one canonical private origin, return through native
OpenCode, experience a brief truthful wake transition, continue the exact prior
session, and leave knowing that Fern will sleep only after completed work.

## Mitchell Hashimoto-Informed Lens

**Verdict:** Fern has a strong control-plane kernel but is not yet packaged as a
coherent, durable appliance.

### Strongest Aspect

Fern distinguishes desired state from observed Docker state, checks ownership
and drift, treats endpoints as ephemeral, persists intentional pause separately
from process failure, and pins the OpenCode image inputs
(`internal/runtime/runtime.go:58-78`, `internal/registry/intent.go:20-35`,
`images/opencode/Dockerfile:1-24`). These are solid infrastructure-product
foundations.

### Serious Challenge

The product has no complete installation-to-upgrade model. The default image is
`fern/opencode:dev`; there is no tracked CI workflow, tagged release, installer,
service definition, upgrade policy, or `version` command. `fern up` behaves as a
foreground controller, but its name does not teach that lifetime responsibility.
`fern attach` derives the client URL from the bind address, conflating where the
server listens with the origin a laptop or phone should use
(`cmd/fern/attach.go:47-65`).

### One Teachable Model

Fern should expose five nouns:

| Noun | Meaning |
|---|---|
| Workspace | Stable identity: one name and repository |
| Controller | Long-running Fern process owning proxy, watcher, and admission |
| Compute | Disposable OpenCode container |
| Data | Retained repository and OpenCode session volume |
| Generation | One concrete compute and endpoint incarnation |

The core rule is `workspace identity != compute generation != network origin`.

### Keep, Remove, Defer

**Keep:** one strict versioned config, explicit ownership, fail-safe state, a
small local durable record, and commands whose outcomes are inspectable.

**Build first:** CI; versioned Linux/macOS binaries and image; `version`; a
minimal initialization path; separate `listen` from canonical `origin`; one
supervised Linux deployment; status that distinguishes controller, compute, and
activity; and stable error codes with a next action.

**Remove:** `resume` as a normal command and the implication that Docker
`running` means the Fern controller is ready.

**Defer:** public runtime-provider interfaces, Kubernetes, hooks, previews,
webhooks, credential brokers, multiple workspaces, and receipts.

### Falsification Test

On a fresh host, an unrelated user should install a released binary and image,
reach first attachment, survive reboot, inspect state, upgrade, roll back, and
remove Fern without repository knowledge or maintainer help.

### Smallest Product

A versioned single-workspace appliance with `init`, `up`, `attach`, `status`,
`logs`, `down`, and explicit destructive data removal. Every green state names
its generation and observation time.

## DHH-Informed Lens

**Verdict:** Fern can be a proud one-person self-hosted appliance only if it
defines its independent user sharply and rejects platform ambitions.

### Strongest Aspect

The operating model is appropriately boring: one Go process, one Docker
container, one named volume, one bind mount, host-local state, and no database or
distributed control plane (`ARCHITECTURE.md:9-28`). Ownership and drift are
explicit. Unknown state leaves compute running. Container removal retains user
data (`internal/runtime/docker.go:348-376`).

### Serious Challenge

The phrase “self-hosted workspace” can pull Fern toward teams, hostile code,
multiple repositories, cloud cost claims, identity, audit, and scheduling. The
current system provides none of those and should not gradually acquire them.
Fern stops a container, not the host, so its honest value is reclaiming host
resources and eliminating manual lifecycle work, not reducing a cloud invoice.

Long-term operation also has gaps: no supervised install, release artifacts,
CI, backup/restore procedure, compatibility policy, or reboot-tested runbook.
The retained OpenCode volume is important state, yet backup and restoration are
not product operations.

### Intended User

One independent developer who already owns an always-on Docker host, trusts the
repository, uses one durable checkout, prefers a private network, accepts wake
latency, and values conservative failure behavior.

Fern is not for teams, parallel agents, hostile code, users seeking ephemeral
clean environments, people who do not operate Docker, or organizations needing
SSO, policy, audit, quotas, billing, and support.

### Keep, Remove, Defer

**Keep:** Docker as the product runtime, one workspace, native OpenCode clients,
private publication delegated to Tailscale, and explicit data ownership.

**Reject:** Kubernetes, multi-cloud, multi-workspace scheduling, hosted/BYOC
control planes, Redis/PostgreSQL, generic OIDC, public previews, webhooks,
schedules, custom UI, OpenCode forks, LLM gateway functionality, and branded
verification claims.

**Build for durability:** releases, service supervision, backup/restore,
upgrade/rollback, compatibility tests, and exact recovery instructions.

### Falsification Test

Run Fern for three months on one real host through reboots, OpenCode upgrades,
backups, restore, OOM, and daemon failure. If ordinary operations require
maintainer knowledge or undocumented Docker repair, it is not a durable
self-hosted product.

### Smallest Product

One private OpenCode appliance on a Docker host the user already owns. It has a
stable origin, retained data, conservative sleep, deterministic wake, legible
status, and documented backup, upgrade, rollback, and removal.

## Bret Victor-Informed Lens

**Verdict:** Fern models lifecycle causality internally but exposes snapshots and
implementation logs instead of a comprehensible account of what changed and why.

### Strongest Aspect

The causal model already exists. Runtime observations retain identity, Docker
status, exit, OOM, endpoint, and desired-spec facts
(`internal/runtime/runtime.go:95-108`). Watch observations carry epoch,
connection, status, and invalidation cause (`internal/watch/controller.go:12-29`).
The supervisor interprets busy-to-idle transitions, request invalidation, and
disconnects (`internal/watch/supervisor.go:142-188`).

### Serious Challenge

`fern status` presents a static Docker observation without temporal or watcher
identity. Wake is experienced as latency, not as an operation. The strongest
safety argument disappears into disconnected log lines exactly when users need
to know why Fern stopped or refused to stop.

A generic dashboard or telemetry stack would add clutter without repairing the
model. The missing primitive is a bounded causal lifecycle record.

### Minimal Causal Record

For each lifecycle operation, retain:

- operation ID, workspace, trigger, and timestamps;
- before state and concrete generation;
- only policy-relevant stages;
- after state and generation;
- outcome: succeeded, deferred, failed, or unknown;
- responsible layer and one safe next action.

A successful stop should explain that the current watcher observed busy to all
idle, the deadline elapsed, held requests were zero, the authenticated snapshot
was all idle, and the container stopped. A deferred stop should explain which
evidence was missing and state that compute remained running.

### Keep, Remove, Defer

**Keep:** independent runtime, activity, and controller dimensions. Do not
flatten simultaneous truths into one “healthy” badge.

**Build:** `fern status`, `fern status --history N`, and `fern status --json`
over the same bounded semantics. A failed wake response may include an operation
ID. `Server-Timing` can reveal wake cost without replacing OpenCode's UI.

**Hide:** raw event properties, health retries, full IDs, and Docker internals
unless verbose. Never persist request bodies, model text, credentials, or token
events.

**Reject:** an indefinite audit system, generic event bus, metrics dashboard, or
claims that lifecycle evidence proves task correctness.

### Falsification Test

For running, waking, asleep, deferred-stop, and OOM cases, a user unfamiliar with
the code should identify within ten seconds: current state, cause, evidence
currency, and next action. Old watcher evidence after restart must never appear
current.

### Smallest Product

The existing CLI gains one bounded, generation-aware causal narrative. Users
explore lifecycle decisions over SSH or a phone-sized terminal without a new web
service.

## John Carmack-Informed Lens

**Verdict:** Fern's direct implementation is credible, but its central value and
several safety claims are supported by sparse manual observations rather than
repeatable empirical evidence.

### Strongest Aspect

The code directly addresses real race classes: coalesced wake, request-held
leases, endpoint generation invalidation, watcher epochs, all-session status,
and persisted pause intent. SSE proxy behavior is tested without buffering
(`internal/proxy/proxy_test.go:64-107`). A controlled crash experiment shows
completed session persistence and loss of in-flight provider output
(`DAY-1.md:274-373`).

### Serious Challenge

The reported 2.8-3.1 second wake is a summary without raw stage timings,
distribution, sample count, or machine context (`IMPLEMENTATION.md:109-131`).
There is no checked-in measurement of resident memory reclaimed. Docker tests
exercise a mocked API rather than a real daemon. OOM, host reboot, daemon loss,
Fern death during stop, pending permissions, remote phone access, and repeated
wake cycles lack daemon-backed evidence.

The two-week roadmap combines delivery, deployment, remote use, measurements,
hooks, persistence, diagnostics, and demo work. For part-time effort it is not a
credible slice.

### Keep, Remove, Defer

**Keep:** the watcher state machine and admission boundary; their complexity
corresponds to concrete race and safety requirements.

**Build:** CI and one executable daemon-backed harness that invokes the real
binary and preserves raw timestamps, inspect output, logs, state, and cleanup
results.

**Measure:** stopped-container wake versus running overhead; create versus
resume; 100 wake/stop cycles; host and container memory; streaming disconnect;
permission wait; tool execution; external SIGTERM/SIGKILL; OOM; daemon
interruption; Fern death during stop; reboot; and session checksums.

**Defer:** hooks, broad doctor architecture, previews, receipts, Kubernetes,
multiple workspaces, hosted identity, and gateway features until measurements
identify a reproduced need.

### Falsification Test

If repeated measurements show insignificant reclaimed memory, disruptive p95
wake, manual recovery, unsafe authorized stops, or no advantage over plain
OpenCode, stop positioning lifecycle management as a product benefit.

### Smallest Product

One supervised process, one real lifecycle test harness, raw benchmark artifacts,
and a narrowly worded guarantee: a conservative stop policy after an observed
idle boundary. Do not claim universal safe shutdown or cost scale-to-zero.

## Patrick McKenzie-Informed Lens

**Verdict:** Fern solves a precise but unvalidated operational problem and is
currently more credible as an OSS portfolio project than as a business.

### Strongest Aspect

The immediate job is concrete:

> When my private OpenCode workspace finishes a turn, release its host resources
> without making me inspect session state or manage Docker, then restore the same
> session when I return.

Fern uniquely coordinates OpenCode activity, Docker lifecycle, and request
admission. OpenCode plus Tailscale supplies remote access but does not itself
provide this conservative stop boundary.

### Serious Challenge

The likely audience is narrow: a developer already self-hosting OpenCode on a
constrained Docker machine, using one durable repository, experiencing real
resource contention or manual shutdown work, and willing to operate another
daemon. There is no evidence yet that enough such users have this pain, prefer
Fern, remain active, or will pay.

The current installation path is unsuitable for broad conversion, and economic
claims must remain modest. Stopping the container can reclaim CPU and memory for
other workloads, but the host may remain fully billed. The configured 8 GiB
limit is not evidence that 8 GiB is saved.

### Positioning

Use problem-shaped language:

> Let one self-hosted OpenCode workspace release RAM between turns. Fern watches
> real session state, stops its Docker container only after a confirmed idle
> boundary, and wakes the retained session on the next authorized request.

Immediately qualify it:

> If leaving `opencode serve` running is acceptable, use OpenCode plus Tailscale
> instead.

Likely search language includes `OpenCode stop when idle`, `OpenCode preserve
session after restart`, `OpenCode wake on request`, and `self host OpenCode
Docker`. Avoid trying to create demand for internal terms such as “lifecycle
controller” or “evidence-bound workspace.”

### Keep, Remove, Defer

**Keep:** the precise job, honest exclusions, open-source distribution, and
technical writing around the session-persistence experiment.

**Build:** a versioned release, installation funnel, plain-versus-Fern case
study, raw wake/memory evidence, and a short complete demo.

**Defer:** feature expansion until unrelated users retain Fern specifically for
safe lifecycle behavior. Commercialization requires paid organizational demand,
not stars or compliments.

### Falsification Test

Interview 15 current self-hosted OpenCode users before describing Fern. Seek at
least five independent reports of recurring lifecycle or host-contention pain.
Then target five unrelated successful installs, three four-week retained users,
and 100 reliable wake/stop cycles. Weak retention means maintenance-only.

### Smallest Product

A narrow OSS utility with a searchable problem statement, sub-15-minute install,
reproducible proof, and an explicit alternative. Sponsorship or support may
follow usage; a standalone business is unsupported without repeated paid demand.

## Cross-Lens Agreement

At least five lenses agree on all of the following:

1. **Preserve the lifecycle kernel.** Request admission, current all-session
   status, watcher generation discipline, and durable pause intent are Fern's
   strongest work.
2. **Define one user and one workspace.** The useful scope is a trusted
   independent developer on an existing private Docker host.
3. **Finish the product path before adding capabilities.** CI, releases,
   supervised operation, canonical origin, status, recovery, upgrade, and
   removal outrank hooks and previews.
4. **Do not compete with OpenCode's UI or Tailscale's identity/networking.** Fern
   should compose with them.
5. **Make transitions truthful and legible.** Users need current generation,
   cause, failure ownership, and next action, not raw machinery or a generic
   green badge.
6. **Measure the claimed benefit.** Wake distribution, memory reclamation,
   failure classification, session persistence, remote use, and comparative
   friction require raw reproducible evidence.
7. **Reject platform scope.** Kubernetes, multi-workspace scheduling, hosted
   control planes, generic OIDC, webhooks, artifacts, and LLM gateway features
   are not earned.
8. **Treat business potential honestly.** Fern can become a strong portfolio
   project and perhaps a niche community utility; commercial pursuit requires
   retained external users and paid demand.

## Genuine Disagreements

### How Much CLI Surface To Add

The simplicity lens favors only `up`, `attach`, `status`, and `down`. The
packaging lens supports `init`, `logs`, `version`, and an explicit destructive
data-removal command. This disagreement depends on whether those commands remove
manual installation and recovery knowledge or merely formalize machinery.

**Decision:** add only commands required by a tested fresh-host journey. `version`
and explicit data removal are likely justified; `init` and `logs` should be
validated against simpler documentation and existing tools.

### Whether To Persist Lifecycle History

The causal-state lens recommends a bounded operation ledger. The simplicity and
empirical lenses warn against building a diagnostics subsystem before proving a
need.

**Decision:** persist only the latest bounded transition records needed to answer
cause, generation, outcome, and recovery. Do not create an event platform,
schema ecosystem, dashboard, or indefinite audit log.

### Product Terminology

The reduction lens prefers human terms such as awake/asleep. Infrastructure
lenses value precise distinctions among controller, compute, activity, and
failure.

**Decision:** use human terms in default output and preserve precise component
facts in verbose/JSON output. Never let `asleep` hide a crash or let `ready`
omit its observed generation.

### Whether Hooks Might Ever Belong

Packaging and product documents can imagine setup/resume as appliance readiness.
All seven reviews reject them now. The remaining disagreement is only whether
repeated wake failures could later earn a minimal hook.

**Decision:** require at least three independently reproduced readiness failures
whose smallest solution is a lifecycle hook rather than an image or repository
script.

## Robust Recommendations

These recommendations survive all seven perspectives:

| Priority | Work | Evidence of completion |
|---|---|---|
| P0 | Add CI for format, tests, race, vet, binary build, and image build | Green clean-checkout workflow |
| P0 | Publish versioned binaries, checksums, and a compatible immutable image | Fresh host installs without Go or local image construction |
| P0 | Separate bind address from canonical private client origin | Laptop, tailnet, and phone use one teachable address model |
| P0 | Document and test one supervised Linux deployment | Reboot recovery is deterministic |
| P0 | Run daemon-backed lifecycle and failure tests | Raw artifacts prove wake, stop, persistence, OOM, external exit, and cleanup |
| P0 | Measure wake and memory repeatedly | p50/p95/max, stage breakdown, host context, and raw output are published |
| P1 | Make status current, causal, and actionable | State, generation, observed time, cause, failure owner, and next action are visible |
| P1 | Publish a plain OpenCode plus Tailscale comparison and short demo | Fern's added value is observable rather than asserted |
| P1 | Recruit unrelated self-hosted OpenCode users | At least five installs and three retained four-week users |
| Do not build | Hooks, previews, artifacts, receipts, webhooks, Kubernetes, multi-workspace, hosted control plane | Reconsider only through a separate evidence-backed product decision |

## Falsification Gates

Fern should become maintenance-only if any of these remain true after a focused
validation period:

- fewer than three unrelated users remain monthly active after three months;
- users do not report recurring lifecycle work or host contention;
- measured memory reclamation has no practical effect on the host;
- p95 wake is unacceptable for the return workflow;
- any authorized automatic stop loses completed state or strands a valid tool or
  permission workflow;
- failure recovery repeatedly requires undocumented Docker archaeology;
- plain OpenCode plus Tailscale is preferred after direct comparison;
- OpenCode ships a stable equivalent lifecycle boundary.

Commercialization should not be considered without at least ten active
organizations, three paid pilots, and one repeated organizational problem that
existing alternatives do not adequately solve.

## Final Scope Boundary

**Fern is:** a private, single-workspace OpenCode lifecycle appliance for a
Docker host the user already owns. It provides one stable origin, wakes retained
compute on authorized demand, stops only after a current conservative idle
boundary, and explains the resulting lifecycle state.

**Fern refuses to become:** a remote-development platform, scheduler, sandbox
fleet, identity provider, hosted control plane, preview host, artifact review
system, verification authority, webhook platform, Kubernetes abstraction, or
LLM gateway.

Fern earns the right to exist through one end-to-end experience: an independent
developer leaves a completed OpenCode turn, lets the workspace release resources,
returns later from terminal or phone through the same private origin, resumes the
retained session after a measured reliable wake, and can understand or recover
from any failure without managing Docker directly. External retention, measured
resource benefit, and fewer manual lifecycle actions are the evidence that this
experience is valuable.
