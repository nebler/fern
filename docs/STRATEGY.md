# Fern Strategy And Direction Map

Last reviewed: 2026-08-30

Repository baseline: `harden/production-readiness` at
`ab945b5a00db3a310b3fcc30fe8bc99669598b6f`

Status: maintained strategy and decision context. This document contains both
implemented facts and explicitly labeled hypotheses. It is not itself an
implementation claim.

## Purpose And Authority

This is the starting document for a person or agent asking:

- What is Fern right now?
- Which properties are real and tested?
- Which product directions are still credible?
- What has been rejected or deferred, and why?
- Which doubts must be resolved before a direction is promoted?
- How do phone use, desktop use, ephemeral workers, Fern Gateway, Fern Labs,
  T3 Code, and Kubernetes fit together without becoming unrelated products?

Use the following authority order when documents differ:

1. Code and tests are authoritative for behavior.
2. [Architecture](./ARCHITECTURE.md) is authoritative for the implemented
   production composition.
3. [Durable Task Model](./TASK_MODEL.md) is normative for Fern-owned task,
   result, verification, and publication semantics.
4. [Product Direction](./REMOTE_PRODUCT.md) owns the accepted product boundary.
5. [Fern Roadmap](./ROADMAP.md) owns committed sequencing and acceptance
   criteria.
6. This document owns the broader strategy map, alternatives, doubts, and
   decision history.
7. Files in [`research/`](../research/) and [`product-docs/`](../product-docs/)
   are point-in-time evidence, not current contracts.

An agent must not convert a candidate direction in this document into a claim
that Fern ships it. When strategy is promoted into committed work, update
`REMOTE_PRODUCT.md` and `ROADMAP.md` in the same change.

## Status Labels

| Label | Meaning |
| --- | --- |
| **Implemented** | Present in the current composition and supported by code/tests. |
| **Partial** | Some required primitives exist, but the user journey or contract is incomplete. |
| **Candidate** | A credible direction that has not passed its product gate. |
| **Conditional** | Build only after the stated evidence or demand gate passes. |
| **Rejected now** | Deliberately excluded from the current sequence; may be reconsidered only with new evidence. |
| **External** | Supplied by another runtime, provider, or infrastructure dependency. |

## One-Page Brief

### Fern Today

Fern is a single-owner, single-host Go control plane around one durable OpenCode
workspace in local Docker.

It lets a paired phone or desktop browser submit a durable coding task, wake the
workspace after task state commits, return to the same OpenCode session, cancel
Fern-owned future effects, authorize one exact clean Git result, verify that
same result under host policy, and optionally publish that same result through a
receipt-backed GitHub App transaction.

Fern is not currently:

- a cloud-agent fleet;
- a generic multi-harness manager;
- a disposable per-task runner;
- a model gateway;
- a benchmark service;
- a Kubernetes application;
- a hostile multi-tenant sandbox;
- a native mobile application;
- a complete unattended coding agent.

### Recommended North-Star Hypothesis

The only product hypothesis currently authorized for testing is:

> OpenCode Background Mode runs real OpenCode sessions on an always-on private
> host, reopens each exact native session from another device, and retains an
> exact recoverable Git result after disposable compute is removed.

This is a bounded falsification experiment, not a Fern 2.0 product verdict. The
existing single-workspace appliance remains the implemented product.

### Immediate Commitment

The active sequence is:

1. Complete and validate the Go 1.27 upgrade.
2. Compare OpenHands custom ACP with the official OpenCode UI on the same tasks.
3. Pin a current OpenCode V2 candidate and test its restart-safe contract.
4. If native OpenCode is materially better, build the two-attempt disposable
   Docker prototype in the Background Mode TODO.
5. Dogfood at least six real tasks over two weeks and retain failures.

The owner has demonstrated the phone flow. Release, Ubuntu reboot,
replacement-host, and other physical acceptance evidence remain operational
gates, but another phone demo is not the next product experiment.

### Strategic Recommendation

If the Background Mode gate passes, prefer this order:

1. Productize the serial disposable local-Docker lifecycle.
2. Add concurrency of two and retained artifact storage.
3. Adapt exact-result verification and publication.
4. Add one notification destination only for observed blockers.
5. Add an outbound runner, Gateway, hosted sandbox, Kubernetes backend, or Labs
   only when its specific trigger recurs.

The [Background Mode TODO](../todo/opencode-background-mode.md) owns the active
implementation checklist.

## Product Objectives

Fern should optimize for all of the following, rather than selecting features
only because they are fashionable:

| Objective | Why it matters |
| --- | --- |
| Useful personal workflow | The owner should be able to delegate real work and finish useful changes. |
| Phone and desktop continuity | The phone handles timely small decisions; desktop handles evidence-heavy triage. |
| Honest reliability | Restarts, disconnects, duplicates, and uncertain external effects must remain explicit. |
| Customer-controlled compute | Repositories and tools can remain on an owner or customer machine. |
| Replaceable compute | A worker may be destroyed without destroying Fern's durable authority or exact result. |
| Go/backend depth | The project should exercise APIs, storage, queues, leases, streaming, reconciliation, and operations. |
| Relevant infrastructure learning | Gateway, workload identity, customer-cloud workers, and Kubernetes should appear only behind real boundaries. |
| Grab relevance | Work should demonstrate streaming gateways, identity, capacity, cost, recovery, and operational judgment without pretending to have Grab's scale. |
| Small correct changes | Fern should reuse existing runtimes and sandbox substrates instead of rebuilding them. |

## Current Implemented Reality

### Topology

```text
phone or laptop
      |
      | private HTTPS / WSS through Tailscale Serve
      v
Fern remote listener
      |
      | paired device, CSRF, server-owned actor
      v
Fern router and workspace admission
      |
      | coalesced stop/freeze/wake
      v
local Docker -> pinned OpenCode -> one host repository
      |
      +-> Fern SQLite task/result/publication state
      +-> host verification
      +-> optional GitHub App broker
```

### Implemented Strengths

- One Go binary supervises one local Docker workspace.
- Stop, freeze, wake, health probing, endpoint attestation, ownership checks,
  OOM/failure classification, and conservative idle handling are implemented.
- The remote listener and host-only operator listener have separate authority.
- Pairing creates restart-safe device grants; revocation cancels active requests.
- Task, attempt, actor, receipt, base commit, and exact OpenCode IDs commit before
  wake or delivery.
- Lost-response retries reuse exact IDs instead of allocating duplicate sessions
  or prompts.
- Cancellation durably fences new Fern-owned effects before interrupting the
  runtime.
- Fern projects positive `running` and `input_required` evidence but refuses to
  infer generic success from inactivity or disconnects.
- A user can preview and seal one exact clean Git snapshot.
- Host verification is bound to the same exact result commit.
- GitHub App publication records mutation phases before push or PR creation and
  reconciles uncertain responses through exact GitHub reads.
- Backup, restore, rollback, encrypted GitHub credential export/import/rotation,
  release provenance, and compatibility harnesses exist.

### Important Invariants

Any future runtime or product direction must preserve these rules:

1. Persist intent before external I/O.
2. Use immutable task, run, repository, base, result, and effect identities.
3. Never use prompt text, timestamps, current `HEAD`, or inactivity as authority.
4. Reconcile ambiguous effects instead of blindly retrying them.
5. Treat cancellation as a fence, not a claim that previous effects rolled back.
6. Verify and publish the exact sealed result, not mutable workspace state.
7. Append an event in the same transaction as every client-visible state change.
8. Export a disposable worker's exact result before deleting its filesystem.
9. Do not call ordinary Docker hostile multi-tenant isolation.
10. Do not claim exactly-once execution where an external system supplies only
    at-least-once calls and reads.

### Current Gaps

| Gap | Status and consequence |
| --- | --- |
| Generic completion | The pinned OpenCode profile has no restart-safe terminal-success fact. Manual user sealing is the production path. |
| Publication UX | App publication backend exists, but the embedded task page does not initiate it. |
| Durable answers | Fern records `input_required` but has no generic durable approval/answer table or phone answer API. |
| Notifications | No transactional notification outbox, push, ntfy, or email adapter. |
| GitHub triggers | No issue comment, label, assignment, or webhook intake. |
| Preview portal | No task-bound authenticated proxy for a running application. |
| Disposable execution | Current workspace and OpenCode state are deliberately persistent. |
| Multiple workers | One workspace and one effecting attempt at a time. |
| Multiple harnesses | The production composition uses one pinned OpenCode profile. |
| Gateway | No provider proxy, scoped model token, usage/cost ledger, translation, or fallback. |
| Labs | No experiment, case, arm, run, evaluator, report, or disposable-run provider. |
| Kubernetes | No Kubernetes dependency or Agent Sandbox adapter. |
| Physical acceptance | Real Ubuntu reboot, external TLS/WSS, replacement restore, and physical phone evidence remain operator work. |

### Current Trust Boundary

Fern trusts one owner, host, repository, image, Docker daemon, and tailnet.
Provider-backed inference may send repository context to an external model
provider. Provider keys may currently enter the trusted workspace. Docker access
is root-equivalent. None of this is a hostile or multi-tenant sandbox claim.

## Owner Questions And Doubts

These questions came from the strategy conversation and should remain visible.
They are product constraints, not objections to hide.

| Question or doubt | Current answer | Evidence that could change the answer |
| --- | --- | --- |
| There are too many ideas. What is Fern actually becoming? | Fern remains the durable control and exact change-handoff product. Compute, Gateway, desktop/phone surfaces, and Labs are supporting layers or modes, not independent products. | Repeated external use showing one supporting mode is the actual recurring job. |
| Would the attention/capability primitive be useful on desktop? | Yes for several concurrent or heterogeneous runs. Desktop is for triage, evidence, policy, and audit; phone is for timely bounded decisions. It is weak for one user running one Claude or Codex session. | Dogfood frequency of cross-runtime blockers and laptop escapes. |
| Is the opportunity just a box or Mac mini? | No. A personal box is valuable for Xcode, hardware, browser state, local tools, and unreproducible environments. Disposable Linux cloud workers are better for ordinary parallel background tasks. Serious use is likely hybrid. | Measured task placement and repeated requirements for Mac-only or local state. |
| Do users actually want ephemeral cloud machines? | Yes for bounded delegated work. Cursor says hosted runtime covers over 80% of customers; Amp reports over 85% of its own commits from Orbs; Stripe reports high-volume disposable devbox use. Customer-cloud and personal runtimes remain important segments. | Independent usage data or Fern dogfood that overwhelmingly prefers one persistent host. |
| Should Fern become a fleet product? | No current evidence supports that. Managed and customer-hosted runner pools, outbound polling, leases, replacement, draining, and stale-write rejection are already occupied. A two-worker Fern mode remains useful as a later capacity feature and portfolio demonstration. | Repeated Fern demand for concurrent placement plus external users choosing Fern-operated runners over managed agents. |
| Is Fern Labs benchmark-as-a-service? | Not initially. It is a conditional private regression-qualification mode for model, harness, prompt, tool, image, or policy changes. A public or hosted benchmark service is rejected now. | A no-build pilot that changes repeated rollout decisions and external users willing to maintain cases. |
| Can Labs comparisons survive network and provider noise? | Only with interleaved repetitions, distributions, explicit provider-attempt records, and modest claims. Small latency differences may be too noisy or expensive to establish. | Pilot variance smaller than decision-relevant differences. |
| Are the best models already good enough? | Often on easy tasks. Labs has value only for real failures, difficult repository constraints, policy, cost, intervention, and reliability. A ceiling effect is a kill signal. | Pilot where all arms solve nearly every case and produce equivalent decisions. |
| Is code quality mostly taste? | Some of it is. Deterministic tests can prove correctness and policy, not architectural taste. Human blind review, merge/revert outcomes, and repository-specific rubrics remain necessary. | Stable reviewer agreement and evidence that a bounded rubric predicts accepted changes. |
| Is ephemeral compute itself a product? | No strong evidence. Hosted sandbox APIs are crowded. Fern's plausible value is durable agent-job semantics above replaceable compute. | Design partners asking specifically for Fern-operated sandbox capacity rather than control/recovery. |
| Why pursue Docker/Kubernetes if the product does not require them? | Disposable Docker is useful for real task isolation and repeatability. Kubernetes is justified later for customer clusters, multiple workers, warm pools, workload identity, or stronger runtimes. It is also valuable learning only when attached to an honest backend. | A second host, a customer cluster, hostile workload requirement, or measured capacity problem. |
| Does building for Grab distort Fern? | It can. Gateway G0/G1, streaming cancellation, identity, and fault handling are useful Fern boundaries and strong role evidence. Redis, PostgreSQL, EKS, and OIDC should not enter the personal product without measured need. | A separate explicit portfolio profile may justify them without changing the default product. |

## Market Facts That Constrain Strategy

### Commodity Capabilities

The following are useful but no longer credible standalone differentiation:

- remote or background coding agents;
- GitHub issue or comment to PR;
- mobile supervision;
- customer-owned workers or BYOC;
- multiple harnesses;
- schedules and webhook triggers;
- authenticated application previews;
- scoped GitHub credentials;
- draft PR generation;
- generic hosted Linux sandboxes;
- Firecracker, fast starts, snapshots, custom images, and scale-to-zero.

Products covering substantial portions include Cursor, Claude Code, Codex,
Warp, Amp, GitHub Copilot, Ona, OpenHands, Coder, T3 Code, Orbit, Warren,
Deputies, Omnara, Pushary, E2B, Daytona, Modal, Vercel Sandbox, Cloudflare
Sandbox, Morph, Blaxel, Runloop, Docker Sandboxes, and Kubernetes Agent
Sandbox.

Fern must not claim an empty category merely because no reviewed product
documents every desired feature under one contract.

### Deployment Segments

| Segment | Likely default | Why |
| --- | --- | --- |
| Individual attended work | Existing laptop or personal machine | Immediate access to current state, tools, credentials, and GUI. |
| Individual delegated work | Managed ephemeral cloud | Continues while devices sleep and supports parallel tasks without maintenance. |
| Small team | Managed cloud first | Lowest operational burden and easiest shared environment. |
| Platform-mature team | Customer-cloud workers | Private services, caches, internal identity, observability, and existing devboxes. |
| Regulated enterprise | Customer VPC/on-prem, often vendor-managed | Data locality, policy, audit, and network requirements. |
| Native Apple work | Personal or customer-controlled Mac | Xcode, simulators, signing, Keychain, and Apple tooling. |
| Untrusted repository | Fresh microVM or hardened sandbox | A personal host and ordinary shared Docker are inappropriate. |

The strategic conclusion is hybrid placement, not cloud-only or box-only.

### Remaining Technical Openings

1. **VM state is not agent state.** A filesystem or memory snapshot does not
   reconcile a push, PR, model charge, message, credential use, deployment, or
   child task outside the machine.
2. **Exception semantics remain fragmented.** Questions, worker loss, quota,
   CI, previews, and publication ambiguity usually live in separate products.
3. **Customer-cloud operation remains costly.** Lightweight outbound runners
   without mandatory Kubernetes are useful, but enterprise support can dominate
   the software.
4. **Governance is not portable.** Commands, paths, egress, MCP tools,
   credentials, budgets, and approvals use provider-specific policy models.
5. **Sandbox semantics are not portable.** Stop, pause, suspend, snapshot, fork,
   restore, and destroy mean different things across providers.

These are plausible openings, not proof of willingness to pay.

## Coherent Product Shape

Fern can contain several components without turning into several unrelated
products:

| Component | Responsibility | Current status |
| --- | --- | --- |
| Fern control plane | Durable task intent, placement, cancellation, exact outcomes, exceptions, verification, and publication recovery | **Partial/implemented core** |
| Fern runner | Persistent or disposable agent execution on owner/customer compute | **Persistent local only** |
| Desktop workbench | Queue triage, evidence-heavy review, policy, worker state, and audit | **Candidate** |
| Phone surface | Capture work, receive urgent attention, make bounded decisions, and deep-link to authoritative context | **Partial browser surface** |
| Fern Gateway | Scoped model access, host-held provider keys, streaming, provider-attempt and cost facts | **Conditional, unimplemented** |
| Fern Labs | Repository-specific qualification using disposable runners | **Conditional, unimplemented** |
| Runtime adapters | OpenCode first; optional T3, Codex, or another pinned producer later | **OpenCode only** |
| Compute providers | Local Docker first; outbound runner, hosted sandbox, or Agent Sandbox later | **Local persistent Docker only** |

The components share durable identity and recovery semantics. They should not
share state machines where their authority differs.

### Production Task Versus Labs Experiment

| Concern | Production task | Labs experiment |
| --- | --- | --- |
| Goal | Produce one useful verified change and optionally publish it | Compare versioned configurations against repository cases |
| Input | User intent against an exact repository base | Immutable case x arm definition |
| Completion | Authoritative runtime contract or explicit user seal | Authoritative batch result, fixed horizon, or explicit manual label |
| Human intervention | Normal product event | Recorded variable that affects comparability |
| Evaluation | Host verification for the exact result | Visible/hidden case evaluator and hard-failure gates |
| Output | Exact result, verification, branch, draft PR | Row-level records and aggregate comparison |
| Publication | Optional, receipt-backed | None by default |
| Repetition | Usually one bounded attempt | Multiple interleaved runs when variance matters |

They may reuse a disposable-run provider, artifact format, Gateway, and
verification primitives. They must not reuse one ambiguous success state.

## Direction Portfolio

Scores are qualitative and relative: `High`, `Medium`, or `Low`. They are not
market forecasts.

| Direction | User value | Fern fit | Backend learning | Crowding | Current decision |
| --- | --- | --- | --- | --- | --- |
| Finish and operate current appliance | High | High | High | Medium | **Committed** |
| Desktop/phone attention and action plane | High after multiple runs | Medium | High | Medium-high | **Candidate after dogfood** |
| Disposable per-task local Docker | High for isolation and repeatability | High | High | High infrastructure crowding | **Preferred next compute primitive** |
| Outbound customer/personal cloud runner | High for hybrid execution | Medium-high | Very high | High | **Conditional after local provider** |
| Fern Fleet as a product | Unproven | Low | Very high | Very high | **Rejected now; portfolio/capacity mode only** |
| Fern Gateway G0/G1 | Medium product value, high security value | High | Very high | Very high | **Conditional on a concrete custody/accounting need** |
| Hosted sandbox adapter | Medium | Medium | Medium-high | Very high | **Conditional** |
| Kubernetes Agent Sandbox backend | Low initially, high for fleets | Medium | Very high | Medium substrate crowding | **Later backend** |
| Fern Labs private qualification | Conditional | High semantic fit | High | High | **No-build pilot first** |
| T3 or second harness adapter | Medium | Medium | Medium-high | High | **Contract spike first** |
| Generic hosted sandbox API | Low Fern-specific value | Low | Very high | Extreme | **Rejected now** |
| Broad benchmark-as-a-service | Unproven | Low-medium | High | High | **Rejected now** |
| Native Fern mobile app | Unproven over browser/T3 | Low | Medium | High | **Rejected now** |
| General multi-agent workflow builder | Unclear | Low | High | Extreme | **Rejected now** |

## Direction Details

### A. Finish And Operate The Current Appliance

**Job:** submit work remotely to a private persistent workspace and safely hand
off one exact result.

**Advantages:** closest to complete, validates years of correctness work, useful
personally, produces real operational evidence, and reveals which later problem
actually recurs.

**Disadvantages:** one repository, one owner, one host, one harness profile, and
manual sealing limit breadth. The category is crowded and deployment remains an
operator responsibility.

**Gate:** physical acceptance plus two weeks of ordinary use.

**Kill or narrow if:** the owner does not repeatedly delegate work, the laptop is
required for most tasks, or ordinary vendor tools solve the complete journey
with less operational burden.

### B. Durable Attention And Action Plane

**Job:** make every blocked or authority-requiring event a durable object that
can be safely resolved from desktop or phone.

Candidate types:

```text
input.question
capability.request
worker.offline
worker.stalled
budget.exhausted
quota.rate_limited
auth.expired
ci.failed
publication.ready
publication.indeterminate
preview.feedback
```

The desktop is an exception workbench. The phone is a bounded decision surface.
Every answer is version-bound, actor-bound, expiring, idempotent, and reconciled
with the authoritative source before resolution.

**Advantages:** survives runtime and device loss, spans coding and delivery, fits
Fern's receipt semantics, and creates a human operations layer for remote jobs.

**Disadvantages:** Omnara, Pushary, T3 Code, Paseo, Orbit, GitHub, and vendor
clients cover important portions. Adapters are fragile, approvals may become
less frequent, and unsafe generic cards can omit destination context.

**Gate:** dogfood must produce repeated blockers that cannot be safely handled
by a deep link to OpenCode or GitHub.

**Minimum proof:** one question or publication item must survive Fern restart,
duplicate notification, stale answer, runtime reconnect, and lost action
response without duplicating the effect.

### C. Disposable Per-Task Docker

**Job:** run a bounded task from an exact base in a fresh environment, export the
exact result, verify it independently, and destroy all run state.

```text
immutable run spec
  -> fresh full clone and agent data volume
  -> pinned container
  -> bounded agent execution
  -> stop and fence writers
  -> export Git bundle or equivalent artifact
  -> independent verification
  -> destroy run state
```

**Advantages:** useful for production tasks and Labs, limits cross-task state,
supports parallelism later, teaches durable lifecycle handling, and makes
compute replaceable.

**Disadvantages:** ordinary Docker is not hostile isolation; cloning and setup
can dominate time; cleanup and disk pressure become operational concerns; the
current pinned OpenCode server lacks generic authoritative completion.

**Architecture rule:** create a separate disposable provider. Do not generalize
the persistent `workspace.Manager` into a fleet scheduler or weaken its retained
state semantics.

**Gate:** first prove a pinned batch contract such as a tested newer
`opencode run` or `codex exec --json`, or explicitly ship a manual/horizon-sealed
research mode.

### D. Outbound Remote Runner And Hybrid Placement

**Job:** run Fern tasks on a replaceable owner/customer VM without exposing an
inbound worker port or moving durable task authority into that worker.

Minimum runner responsibilities:

- enroll and obtain a short-lived runner identity;
- advertise compatible image, runtime, capacity, and platform;
- poll outward for lease-bound work;
- heartbeat with a fencing generation;
- start one disposable local worker;
- propagate cancellation;
- upload exact result artifacts before cleanup;
- drain for upgrades;
- reconcile restart and lost-response state.

**Advantages:** strong Go and distributed-systems project, supports a sleeping
personal machine, keeps execution near private services, and creates a path to
customer-cloud deployment without requiring Kubernetes.

**Disadvantages:** BYOC is crowded; customer IAM, proxies, registries, upgrades,
and diagnostics create a large support surface. One remote runner is not a fleet
business.

Cursor, Claude, GitHub Actions/ARC, Warp, Ona, Orbit, and Deputies already
establish that worker pools, outbound polling, claims, lease renewal, draining,
replacement, and stale-write rejection are occupied capabilities. Fern must not
position those mechanics as market whitespace.

A lease epoch fences only Fern-controlled state that validates the epoch. It
cannot stop a stale worker from calling GitHub, a provider, an MCP server, or an
arbitrary API directly. A replacement worker starts a new attempt from an exact
accepted checkpoint or the original base; it does not resume arbitrary process
memory, an in-flight model response, or an interrupted shell command.

The honest later demonstration is narrower:

```text
attempt A loses its lease
  -> attempt B claims a higher epoch
  -> stale A cannot checkpoint, finalize, or select a winner
  -> B starts from an accepted checkpoint or exact base
  -> one immutable winner is selected
  -> brokered publication converges to one visible PR
```

Provider calls, computation, and unbrokered tool effects may repeat. Workers
must have isolated filesystems and no direct GitHub write credentials for the
publication claim to hold. `workspace-gh` is outside that guarantee.

**Gate:** repeated need for a second host, unavailable personal machine, private
network access, or parallel capacity.

**Product decision:** do not market `Fern Fleet` as Fern's wedge. Use one or two
remote workers as a hybrid execution feature or a clearly scoped portfolio
experiment after local disposable execution works.

### E. Fern Gateway

**Job:** keep provider credentials outside the workspace and record the exact
model-request facts Fern needs for policy, budgets, recovery, and Labs.

Initial boundary:

- one observed OpenCode request shape;
- one upstream provider;
- unbuffered SSE forwarding;
- disconnect and cancellation propagation;
- one hashed, expiring, revocable Fern credential;
- one model allowlist and budget;
- one logical request and one row per provider attempt;
- nullable usage and cost under a versioned price;
- no fallback after output becomes visible.

**Advantages:** meaningful credential reduction, direct Grab relevance, and a
natural place for run-scoped identity and attempt accounting.

**Disadvantages:** gateways are crowded, provider contracts churn, and broad
translation/catalog work can consume the project without improving Fern.

**Gate:** follow G0/G1 in `ROADMAP.md`. Stop at two providers unless real Fern
traffic justifies more.

### F. Hosted Sandbox Adapter

**Job:** dispatch compatible Fern tasks to an established provider when local or
customer compute is unavailable or when stronger isolation is required.

Plausible substrates include E2B, Modal, Vercel Sandbox, Cloudflare Sandbox,
Runloop, Blaxel, or another provider selected through a contract spike.

**Advantages:** gains elastic capacity and hardened execution without operating
microVM infrastructure.

**Disadvantages:** lifecycle semantics are provider-specific; snapshots cannot
reconcile external effects; source and metadata cross another trust boundary;
provider portability can collapse into a weak lowest-common denominator.

**Gate:** one real task class must need hosted overflow or stronger isolation.
Support one provider deeply before adding another.

### G. Kubernetes Agent Sandbox Backend

**Job:** allocate disposable Fern workers from customer Kubernetes using an
existing sandbox substrate.

Potential mapping:

| Fern concept | Agent Sandbox concept |
| --- | --- |
| Runtime profile | `SandboxTemplate` |
| Run allocation | `SandboxClaim` |
| Allocated worker | `Sandbox` |
| Warm capacity | `SandboxWarmPool` |
| Stronger isolation | gVisor/Kata `RuntimeClass` |

Fern still owns task identity, completion authority, leases, exact artifacts,
exceptions, evaluation, and publication. Kubernetes readiness is not agent
success.

The current upstream release at this review is Agent Sandbox `v1.0.0`, which
serves the `v1beta1` API. Router, portable-backend, and some lifecycle features
remain roadmap work and must not be described as shipped solely because the
CRDs exist.

**Advantages:** real experience with CRDs, watches, resource versions,
finalizers, quotas, workload identity, NetworkPolicy, warm pools, and orphan
cleanup. It is a credible enterprise backend.

**Disadvantages:** substantial operations, no initial personal-user value, and
Kubernetes alone is not hostile isolation. A custom Fern sandbox operator would
duplicate upstream work.

**Gate:** multiple workers, customer cluster demand, warm-pool latency, or a
security requirement that local Docker cannot satisfy.

### H. Fern Labs

**Job:** qualify an agent-stack change against reusable repository-specific
cases.

Labs asks:

> Did this model, harness, prompt, tool, image, Gateway route, or policy change
> improve the tasks we care about without unacceptable regressions?

It does not initially ask which model is universally best.

**Advantages:** reuses disposable execution, exact result identity, failure
classification, Gateway records, and verification. It can prevent a bad rollout
or justify a cheaper configuration.

**Disadvantages:** case authoring and hidden evaluators are expensive; provider
and network variance require repetitions; strong models can saturate easy
cases; architectural taste is not mechanically decidable; existing evaluation
tools may be sufficient.

**Gate:** run the no-build pilot in `ROADMAP.md`. Build `fern eval` only if at
least 8 useful cases can be derived from 20 changes within four hours, cases
replay after 30 days, audited evaluators do not falsely accept, and the result
changes a real rollout decision.

**Initial implementation if promoted:** one owner, one repository, two arms,
serial fresh Docker runs, deterministic evaluator, exact Git artifacts, row JSON
and Markdown report, no automatic publication, no public service.

### I. T3 Or Another Harness Adapter

**Job:** let Fern consume a broader native/mobile agent surface without
rebuilding conversations, terminals, diffs, worktrees, or harness drivers.

**Advantages:** T3 already supplies native clients and Claude Code, Codex,
Cursor, Grok, and OpenCode drivers. Fern could focus on durable external jobs,
exact result handoff, exceptions, and publication.

**Disadvantages:** T3's external contracts are private and unstable; accepted
provider intents and approvals have restart limitations; linked worktrees
conflict with Fern's current repository proof boundary.

OpenCode V2 is likewise a digest-pinned beta integration with an experimental
HTTP API, not a generally stable protocol. Every accepted adapter version needs
its own failure contract.

**Gate:** a SHA-pinned spike must bootstrap, observe, deep-link, cancel, restart,
and reconcile two isolated threads. Initially treat T3 as an imported producer,
not Fern's authoritative runtime.

### J. Generic Sandbox API

**Decision:** rejected now.

The market already competes on startup latency, snapshots, custom images,
previews, regions, capacity, utilization, abuse handling, and price. Fern has no
demonstrated advantage in those dimensions. Wrapping ordinary Docker as a
public secure sandbox would be false.

Use an existing sandbox backend if Fern needs hostile or elastic execution.

### K. Broad Benchmark-As-A-Service

**Decision:** rejected now.

Public benchmark rankings are not Fern's strongest job. Network variance, model
ceiling effects, expensive repetitions, difficult private case construction,
and subjective code quality weaken a hosted leaderboard business.

Labs may become private continuous qualification only after the no-build gate.

## Desktop And Phone Product Model

### Desktop

Desktop should be an operational workbench, not a larger phone app:

- morning sweep of overnight jobs;
- grouping by project, worker, runtime, and root cause;
- side-by-side logs, diff, tests, preview, and prior attempts;
- retry from a proven checkpoint;
- worker and capacity health;
- policy authoring and recurring-exception analysis;
- exact audit of who granted which action against which artifact revision.

Suggested information architecture:

```text
Needs me | Assigned | Waiting | Resolved | Workers

work item and exception list
  -> current exact artifact and evidence
  -> event and effect chronology
  -> exact allowed actions
  -> authoritative source deep link
```

### Phone

Phone should optimize for scalar decisions:

- capture a task with text, dictation, screenshot OCR, or an attachment;
- receive one actionable notification;
- answer a bounded question;
- deny, cancel, retry, or wake;
- grant one exact short-lived capability;
- inspect a small preview or diff;
- open GitHub, OpenCode, or T3 when richer context is required.

Do not encourage unsafe approval by hiding context. Sensitive grants and
externally visible effects should require passkey or biometric step-up if a
future authentication design supports it.

### Exact Action Vocabulary

Avoid a generic `Publish` action. Prefer explicit effects:

```text
push branch
open draft PR
mark PR ready
merge PR
create preview deployment
promote deployment
post external comment
```

Every action must be bound to the exact resource and artifact revision.

## Disposable Execution Architecture Rules

The first disposable provider must remain separate from the current persistent
workspace lifecycle.

### Required Run Phases

```text
admitted
  -> provision_started
  -> ready
  -> execution_started
  -> stopped_and_fenced
  -> result_export_started
  -> result_exported
  -> verification_started
  -> verified / rejected
  -> cleanup_started
  -> cleaned / cleanup_required
```

Commit each effect boundary before the corresponding external operation. A
heartbeat proves liveness, never completion. Worker loss after a potentially
mutating operation produces `uncertain` or `recovery_required`, not an automatic
fresh attempt.

### Artifact Boundary

Before deletion, preserve enough content to prove and reproduce the result:

- base commit;
- result commit and tree;
- changed-object manifest and digest;
- Git bundle, patch plus required objects, or equivalent content-addressed
  artifact;
- image digest and run specification digest;
- terminal reason;
- bounded logs and evidence;
- intervention and external-effect records.

Hidden Labs evaluators must never be mounted into an agent worker. Production
verification and Labs evaluation remain distinct policies.

### Security Levels

| Level | Honest claim |
| --- | --- |
| Persistent local Docker | Trusted owner/repository convenience and lifecycle boundary |
| Fresh local Docker | Better task freshness and cleanup, still trusted repository code |
| Dedicated VM per trust domain | Stronger host separation, still requires network and credential policy |
| Agent Sandbox with gVisor/Kata | Stronger workload isolation when configured correctly |
| Managed microVM provider | Provider-supplied hostile workload boundary subject to its contract |

Capabilities, short-lived credentials, and egress rules reduce authority. They
do not turn a weak compute boundary into strong isolation.

## Validation And Kill Gates

| Hypothesis | Cheapest test | Pass condition | Kill or narrow condition |
| --- | --- | --- | --- |
| Current Fern is useful | Two-week real dogfood | Repeated phone/desktop delegation and completed useful changes | Most tasks require laptop repair or vendor tool replacement is consistently easier |
| Phone capture matters | Pocket Brief shortcut or fragment prefill | Repeated task creation away from desktop | Capture is rarely used or correction costs exceed typing |
| Attention plane matters | Log every blocker before building generic schema | Recurrent blocker resolved safely outside source app | Fewer than five useful exceptions or deep links solve them adequately |
| Disposable tasks matter | One serial Docker/OpenCode task | Freshness, native takeover, or exact retention changes real workflow | Setup cost dominates, native UI is rarely used, or the persistent workspace is preferred |
| Remote runner matters | One owner-operated VM | Tasks continue while personal devices sleep and recur | One persistent Fern VM already satisfies all demand |
| Fleet mode matters | Two isolated workers after one remote runner is useful | Repeated capacity or placement need beyond one worker | Runner mechanics exist only for a demo or managed agents remain easier |
| Hosted overflow matters | Contract spike with one provider | A real task needs elasticity or stronger isolation | Local/customer runner handles all useful tasks |
| Kubernetes matters | Trigger-driven backend spike after Docker acceptance | A second node, customer cluster, workload identity, NetworkPolicy, RuntimeClass, or measured capacity need exists | It adds operations without solving a repeated accepted task need |
| Gateway matters | G0 fake-provider contract and G1 one-provider path | Provider key leaves workspace and stream/cost records aid operation | It becomes provider-catalog work without Fern traffic |
| Labs matters | No-build 20-change/8-case pilot | Report changes or prevents a rollout decision | Ceiling effects, noise, evaluator cost, or existing tools make it redundant |
| T3 matters | Exact-SHA contract spike | Native client/multi-harness value exceeds adapter fragility | Restart ambiguity or proof-boundary mismatch remains dominant |

## Recommended Sequence

### Product Sequence

1. Validate Go 1.27 and the current baseline.
2. Run the pinned OpenHands/OpenCode comparison and stop if it is equivalent.
3. Characterize one newer pinned OpenCode V2 server-per-attempt contract.
4. Implement one serial disposable Docker task from exact source through native
   UI attachment, trusted Git-bundle export, runtime deletion, and clean
   reconstruction.
5. Pass restart, lost-response, stale-generation, cancellation, export, disk,
   and cleanup fault gates.
6. Add bounded local concurrency of two, artifact-backed verification and
   publication, and one notification destination only after serial acceptance.
7. Dogfood six real tasks over two weeks and apply the written kill criteria.
8. Add a remote runner, Gateway, Labs, T3, or Kubernetes only after its separate
   trigger is observed.

### Labs Sequence

1. Use existing tools for the no-build pilot.
2. Select difficult historical tasks, not easy model-saturated examples.
3. Interleave repeated arms and record variance.
4. Use deterministic gates plus blind human review where taste matters.
5. Build serial `fern eval` only if the report changes a real decision.
6. Reuse the disposable provider without conflating experiment and production
   completion semantics.

### Grab Portfolio Sequence

1. Use the existing Go proxy, receipts, cancellation, recovery, release, and
   operations work as the baseline story.
2. Complete Gateway G0/G1 with one provider, SSE cancellation, scoped tokens,
   and provider-attempt records.
3. Complete the serial Agent Sandbox path with UID/generation reconciliation,
   PVC/result boundaries, trusted Jobs, NetworkPolicy, and failure injection.
4. Build the outbound runner only after the local Kubernetes product works, to
   demonstrate leases, heartbeats, fencing, compatibility, and recovery.
5. Add Redis, PostgreSQL, OIDC, or two replicas only in a measured hosted profile,
   never as decorative default dependencies.

## Decision Register

| Decision | Status | Reason |
| --- | --- | --- |
| Preserve Fern core and exact change handoff | **Go** | It is implemented differentiation and the foundation for every direction. |
| Deploy and dogfood before expansion | **Go** | Current value and physical operation are still unproven. |
| Treat phone and desktop as complementary surfaces | **Go** | Phone and desktop serve different decision sizes. |
| Build a generic attention SaaS now | **No-go** | Direct competitors exist and Fern has not observed enough recurring exceptions. |
| Test OpenCode Background Mode | **Go as a bounded experiment** | Compare OpenHands first, then use per-attempt Docker, native OpenCode routing, manual sealing, retained Git artifacts, and explicit kill gates. |
| Build an outbound customer runner | **Conditional go** | Strong hybrid path, but BYOC support burden is high. |
| Build or market Fern Fleet as the product | **No-go** | Fleet mechanics are occupied and demand for owner-operated small fleets is unproven. |
| Build a two-worker fenced-attempt demo later | **Conditional go** | Strong portfolio evidence if it makes only Fern-controlled-state and convergent-publication claims. |
| Build Fern Gateway G0/G1 | **Go when roadmap gate is met** | Credential custody and Grab relevance are real; broad gateway scope is not. |
| Build a universal model gateway | **No-go** | Crowded and distracts from Fern-specific semantics. |
| Use Kubernetes Agent Sandbox for isolated attempts | **Conditional** | Reconsider only for a second node, customer cluster, workload identity, NetworkPolicy, RuntimeClass, or measured capacity need. |
| Build a Fern Kubernetes sandbox operator | **No-go** | Duplicates upstream and does not supply Fern task semantics. |
| Keep Fern Labs as private qualification mode | **Conditional go** | Useful only if a no-build pilot changes decisions. |
| Build benchmark-as-a-service | **No-go** | Demand, case economics, variance, and differentiation are unproven. |
| Build a hosted sandbox API | **No-go** | Fern lacks a compute-layer advantage and hostile multi-tenancy boundary. |
| Keep a Mac/Mac mini execution profile | **Conditional go** | Valuable for Apple and hardware work, not a complete product. |
| Integrate T3 as an imported producer | **Conditional go** | High UI/harness leverage if a pinned contract survives failure testing. |
| Build native Fern mobile clients | **No-go now** | Existing browser and possible T3 integration must fail first. |
| Build general agent-to-agent chat | **No-go** | Use tasks, Git artifacts, events, and bounded child work instead. |

## Open Questions

The following questions remain unresolved and should drive experiments rather
than speculative implementation:

1. Does the current phone-to-result journey recur after the novelty wears off?
2. Which tasks need a persistent environment, which can use a fresh Linux
   worker, and which structurally require a Mac?
3. How often does a run need human input, and can that input survive runtime
   restart without fabricating authority?
4. Does exact publication reconciliation change a real merge, audit, or incident
   decision compared with GitHub plus vendor logs?
5. Can a pinned batch contract establish exact input, cancellation, terminal
   reason, and Git result after restart?
6. Is one remote runner enough, or does a real queue produce a placement and
   capacity problem?
7. Can provider credentials be removed from the workspace without breaking the
   pinned OpenCode contract?
8. Does a private evaluation suite produce decision-relevant differences after
   accounting for provider and run variance?
9. Who would pay for Fern: an individual, a small trusted team, or an internal
   agent-platform group?
10. Is the strongest external artifact the product itself, the Gateway, the
    runner protocol, or the failure-semantics conformance suite?
11. Does a portable Agent Change Record change a real decision, or do GitHub,
    CI, and vendor logs already provide sufficient context?

## Rules For Future Agents

Before proposing or implementing a new direction:

1. Read this document, `ARCHITECTURE.md`, `REMOTE_PRODUCT.md`, `ROADMAP.md`, and
   the relevant normative contract.
2. State whether each capability is implemented, partial, candidate,
   conditional, external, or rejected now.
3. Name the authority for completion, credentials, repository identity, result,
   verification, and external effects.
4. Do not infer current behavior from a newer OpenCode, T3, provider, or
   Kubernetes release without an exact-version contract test.
5. Prefer one vertical slice and one provider/runtime over an abstraction with no
   accepted implementation.
6. Preserve existing dirty worktree changes and never rewrite historical
   research as current truth.
7. Add a falsifiable gate and kill criterion to every new product hypothesis.
8. Update this decision register when evidence changes a conclusion.
9. Update `REMOTE_PRODUCT.md` and `ROADMAP.md` when a candidate becomes committed
   work.
10. Keep claims honest: documented competitor behavior is not proof of quality,
    and a missing public contract is not proof that a capability does not exist.

## Primary Internal References

- [Fern Architecture](./ARCHITECTURE.md)
- [Product Direction](./REMOTE_PRODUCT.md)
- [Fern Roadmap](./ROADMAP.md)
- [Durable Task Model](./TASK_MODEL.md)
- [Security Boundary](./SECURITY.md)
- [Phone Field Demo](./FIELD_DEMO.md)
- [Fern Strategy Audit](../research/fern-strategy-audit-2026-08-28.md)
- [Fern Homebase Category Report](../research/fern-homebase-category-report-2026-08-28.md)
- [Agentic Coding Frontier](../research/agentic-coding-frontier-2026-08-28.md)
- [Agent Change Record Competitor Audit](../research/agent-change-record-competitor-audit-2026-08-28.md)
- [Bleeding-Edge Directions](../research/fern-bleeding-edge-directions-2026-08-29.md)
- [Defensible Remote-Agent Wedge](../research/fern-defensible-remote-agent-wedge-2026-08-30.md)
- [Remote-Agent Wedge Findings](../research/fern-remote-agent-wedge-findings-2026-08-30.md)
- [Independent Wedge Audit](../research/fern-defensible-remote-agent-wedge-independent-audit-2026-08-30.md)

## Selected External References

Market and runtime choices:

- [Cursor: choose a Cloud Agent runtime](https://cursor.com/docs/cloud-agent/self-hosted-guides/choose-runtime)
- [Amp: Pave the road](https://ampcode.com/notes/pave-the-road)
- [Stripe Minions](https://stripe.dev/blog/minions-stripes-one-shot-end-to-end-coding-agents-part-2)
- [Claude Code self-hosted environments](https://code.claude.com/docs/en/self-hosted-environments)
- [T3 Code](https://github.com/pingdotgg/t3code)

Compute substrates:

- [Kubernetes Agent Sandbox](https://agent-sandbox.sigs.k8s.io/docs/)
- [Docker Sandboxes](https://docs.docker.com/ai/sandboxes/)
- [E2B Sandbox](https://www.e2b.dev/docs/sandbox)
- [Modal Sandboxes](https://modal.com/docs/guide/sandboxes)
- [Vercel Sandbox](https://vercel.com/docs/sandbox)
- [Cloudflare Sandbox](https://developers.cloudflare.com/sandbox/)

Grab relevance:

- [Grab AI Gateway](https://engineering.grab.com/grab-ai-gateway)
- [Palana Part 1](https://engineering.grab.com/palana-part-1-secure-platform-for-ai-agents)
- [Palana Part 2](https://engineering.grab.com/part-2-palana-architecture)
- [Grab Agent Platform](https://engineering.grab.com/how-grab-builds-and-runs-ai-agents-at-scale)
- [Grab Bench](https://engineering.grab.com/grab-bench-evaluating-ai)

## Maintenance Trigger

Update this document when any of the following occurs:

- physical acceptance or the dogfood period completes;
- a recurring task, blocker, or laptop escape is measured;
- a disposable or remote runner is implemented;
- Gateway G0/G1 changes credential or measurement claims;
- the Labs no-build gate passes or fails;
- a second runtime or compute backend is accepted;
- a competitor removes a claimed opening;
- the owner changes the primary objective among personal utility, product
  validation, open-source distribution, or Grab portfolio evidence.

Record the evidence and change the decision register. Do not preserve a favored
direction merely because implementation has started.
