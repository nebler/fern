# Product Direction

This document records Fern's product thesis, possible future directions, and
the evidence required before expanding scope. It is not a description of
implemented behavior; [ARCHITECTURE.md](./ARCHITECTURE.md) remains the current
system contract.

## Current Product

Fern is a private, self-hosted control plane for one durable OpenCode workspace.
It provides authenticated remote access, conservative idle shutdown, durable
repository and session storage, and lifecycle recovery on infrastructure the
user owns.

The current product claim must be proven before Fern adopts a broader platform
role:

> A user can start real coding work from another device, disconnect, let compute
> stop, return to the same completed session state, and recover after a host
> restart.

The next acceptance milestone is therefore a provider-backed deployment and
phone rehearsal, not a new orchestration or isolation layer.

## Thesis

Most remote coding agents retain the local coding session as their primary
metaphor: one working directory, shell, conversation, and machine. This is a
useful transitional design because models understand terminals and repositories,
but newer infrastructure primitives allow the control plane to treat placement,
parallelism, triggers, and execution history as independent concerns.

The defensible form of the thesis is not that shells or working directories
should disappear. It is:

> Keep a conventional local-feeling environment inside the sandbox, but invert
> control over where the agent runs, how many branches run, what wakes them, and
> how their progress survives failure.

This yields five possible primitives:

1. **Placement:** run the same bounded agent task locally or on a remote runtime.
2. **Fork:** branch an identical workspace and execution state into competing
   approaches.
3. **Event:** wake an addressable agent because a request, webhook, schedule, or
   system condition occurred.
4. **Durable step:** journal model calls, tools, approvals, and outputs so a task
   survives process and infrastructure failure.
5. **Immutable state:** represent workspace and execution versions as values
   that can be inspected, compared, retained, or collected.

No mainstream developer-facing product has yet proven that replacing the
session with one of these primitives is a better general interaction model.
Fork-first and event-first systems remain hypotheses to test, not assumptions on
which to rebuild Fern.

## Market Lessons

The relevant products increasingly separate four concerns:

- the agent loop and its reasoning state;
- the machine or sandbox in which tools execute;
- conversation storage and client streaming;
- durable orchestration, triggers, and artifacts.

Cursor has publicly described moving its cloud-agent loop into Temporal and
decoupling loop, machine, and conversation state. Amp exposes remote runners,
Orbs, durable threads, schedules, webhooks, portals, and agent-to-agent work.
Grab's Palana treats isolation, identity, secret mediation, network policy,
auditability, persistence, and idle shutdown as platform responsibilities.
T3 Code demonstrates an open, local-first, multi-harness control surface. Claude
Code and Codex also support remote control of user-owned machines, while Claude
and Copilot can place work on customer-operated runners. Remote control and
bring-your-own compute are therefore no longer sufficient differentiation.

Fern should not infer from this convergence that it needs every layer. It should
choose a narrow user problem and preserve clean seams around concerns it may add
later.

## Recommended Wedge: Portable Verified Missions

Fern should not combine two agent loops. It should own the durable boundary
between existing loops and the environments in which they work.

The proposed unit is a **portable verified mission**:

```text
immutable input -> attempts -> agent turns -> approval -> verification
                -> artifacts -> publication receipt -> retention
```

A mission envelope records:

- repository identity, base commit, starting patch, and worktree identity;
- the bounded task, policy, budget, verifier, and publication rules;
- ordered attempts, each with a harness, placement, version, and operation ID;
- normalized lifecycle events and explicit terminal outcomes;
- Git checkpoints, test evidence, artifacts, approvals, and external effects;
- links between the input, resulting commit, evidence, and publication action.

The envelope is portable; an active process is not. Harness-specific transcript
exports may be attached as useful context, but they are optional and lossy. Fern
must not claim that it can transfer provider-side inference, process memory,
permission waiters, sockets, or arbitrary external effects between harnesses.

This fills a narrower gap than a generic agent UI or workflow engine. MCP
standardizes tool access, Git standardizes source history, and agent protocols
standardize parts of client interaction. None of them supplies a vendor-neutral
record for task intent, attempts, approvals, verification, artifacts, and
publication across agent runtimes.

### First Capability: Verified Hibernation

The first implementation should remain OpenCode-only and compound Fern's
existing lifecycle work. At an authenticated quiescent boundary, Fern can emit
an immutable continuity receipt containing:

- workspace ownership and checkpoint generation;
- OpenCode protocol/version and container image digest;
- desired-spec fingerprint and final activity observations;
- Git `HEAD`, index, tracked-diff, and untracked-state fingerprints;
- durable session-volume identity and integrity result;
- stop intent, final compute state, timestamp, and prior-receipt link.

On wake, Fern classifies continuity as exact, external repository drift, runtime
drift, or invalid checkpoint. This turns "the workspace can safely disappear"
from an assumption into an inspectable product contract. A restorable volume
generation can follow after receipts are cheap and reliable; automatic archival
of ignored files should be avoided because they often contain secrets or large
build outputs.

### Distinctive Demonstration

After one OpenCode mission works, add exactly one second harness adapter. Run the
same mission from the same Git boundary in isolated worktrees, enforce comparable
budgets, run one external verifier, and retain a commit-bound proof bundle for
both outcomes. The demonstration is not "two agents in one chat." It is:

> One self-hosted mission can sleep, resume, retry under another harness, or fork
> into competing attempts without losing its identity, evidence, or policy.

This is meaningfully ahead of remote chat while remaining honest about the
coarse boundaries available today. A later event inbox or Temporal workflow can
drive the same mission without changing its data model.

### Why Not Host Git

Amp-hosted repositories remove setup friction, but Git hosting itself is not the
open problem. Fern should remain compatible with GitHub, GitLab, Forgejo, and
plain remotes rather than become another forge. Its system of record should be
the mission and its evidence, with Git as the portable source-state boundary.

### Adjacent Wedges

Research found three useful capabilities that fit the mission model but should
not become independent products yet:

| Capability | Value | Place in the mission |
| --- | --- | --- |
| Private proof vault | Keeps logs, screenshots, tests, and provenance off public artifact URLs | Artifact manifest and commit-bound evidence |
| Fork quarantine | Diagnoses untrusted fork changes without exposing write credentials | Restricted attempt followed by an approved promotion |
| Durable event relay | Authenticates, deduplicates, retries, and replays external triggers | Mission inbox and attempt creation |

The unifying primitive is an inspectable transition from untrusted intent to a
verified, approved Git effect. Building all three surfaces before Mission V0
would recreate a platform rather than test the wedge.

## What Generalizes

Several existing Fern decisions remain useful under a broader control-plane
model:

| Fern concept | General value |
| --- | --- |
| Desired `runtime.Spec` versus observed state | Reconciliation across any placement backend |
| Stable workspace identity | Addressability, ownership, and later branch identity |
| Stop compute while retaining durable state | Scale to zero independent of the sandbox technology |
| Coalesced wake and request admission | Wake-on-event with concurrency control |
| Endpoint generations and stale rejection | Safe replacement or relocation of a backend |
| Conservative idle verification | Fail-safe lifecycle policy |
| Ownership checks before mutation | Required control-plane discipline |

The current Docker labels, host file locks, one configured workspace, and
protocol-specific polling are local implementations rather than universal
abstractions. They should be generalized only when a second concrete use case
requires it.

## Possible Designs

### Parallel Branches

The unit is a branch set. Starting from one state, run several approaches,
evaluate them with tests or another explicit predicate, retain the useful
results, and collect the rest.

```text
snapshot -> fork N -> run N bounded agents -> verify -> compare -> retain
```

This is technically distinctive but expensive. It requires isolated workspaces,
clear cost controls, deterministic verification, branch comparison, and a real
task where parallel search outperforms one stronger sequential agent.

### Event-Driven Agent

The unit is a trigger and one bounded response. A stable agent address wakes on
CI failure, an issue, a schedule, a message, or another service event, performs
work, records an outcome, and returns to zero compute.

Fern's stable proxy and scale-to-zero lifecycle are adjacent to this design, but
event ingestion also requires authentication, deduplication, delivery state,
retry semantics, and an outcome interface.

### Durable Mission

The unit is a task composed of explicit steps. Provisioning, agent turns,
verification, human approval, publication, and cleanup are journaled and can
survive worker failure.

Temporal is a plausible implementation after Fern has a useful mission model.
It cannot make an opaque, active OpenCode turn exactly resumable. Exact step
recovery requires the agent loop to expose model calls, tool calls, permission
waiters, operation IDs, and attempt-aware event streams. With OpenCode as an
opaque runtime, Fern can first provide safe recovery at turn and Git checkpoint
boundaries.

### Verified Hibernation

The unit is a quiescent continuity boundary. Fern records why stopping was safe,
which durable and repository state existed at that point, and whether the next
wake observes the same state. This is the nearest-term subset of portable
verified missions because Fern already owns wake, admission, idle verification,
and stop decisions.

### Content-Addressed Execution

The unit is an immutable workspace and execution version. Each action creates a
new value, making branches cheap and history inspectable. This is conceptually
clean and useful for research, but whole-environment merge semantics and storage
costs are not yet proven product requirements for Fern.

### Placement-Abstracted Runtime

The conventional agent remains inside a shell-oriented environment, while the
control plane externalizes placement, branch count, and wake source. This is the
least speculative long-term design because it retains model-compatible local
metaphors and creates room for local Docker, remote sandboxes, and future agent
runtimes.

## Product Gaps Before Expansion

Fern does not yet have:

- provider-backed phone and reboot evidence;
- a task or mission contract;
- non-interactive prompt submission and terminal task outcomes;
- per-task Git worktrees, branches, artifacts, and retention;
- append-only mission events, attempt IDs, or idempotency rules;
- durable approvals, cancellation, retries, or schedules;
- provider credential mediation or default-deny egress;
- per-agent identity, multi-tenant isolation, or policy enforcement;
- task-level tracing, evaluations, cost accounting, or outcome metrics;
- multiple runtime or placement implementations.

Wake latency is not the limiting product gap. A reliable three-second wake is
already reasonable for a personal remote environment. Snapshotting, warm pools,
Kubernetes, gVisor, and Firecracker become relevant only after latency, fleet
scale, or hostile tenancy is demonstrated.

## Decision Gates

### Foundation

Fern builds on OpenCode. It will not migrate to another harness or start a
clean-sheet agent loop unless all of these are true:

1. A validated user need requires an interaction OpenCode cannot support.
2. The limitation is confirmed in the current protocol and implementation.
3. A composed prototype can demonstrate the alternative within four weeks.
4. The prototype has an explicit success metric and stopping condition.

### Durable Missions

Temporal becomes justified only after a mission can be driven end to end without
it. The first mission must create isolated Git state, submit one task, observe a
terminal result, run verification, preserve evidence, and stop compute. Temporal
then earns its place by surviving worker failure, approval delays, retries, and
deployment without repeating completed side effects.

The first implementation should use Fern-owned records and the pinned V1 HTTP
contract: create a Session, submit an asynchronous prompt, treat SSE as a wake
hint, reconcile terminal state from status and persisted messages, then verify
outside OpenCode. Current-source V2 has stronger durable admission and
per-Session history, but those features are not established for Fern's pinned V2
beta and must be smoke-tested before use.

### Harness Neutrality

Do not introduce a generic adapter interface before one OpenCode mission works.
A second adapter earns the abstraction only if it can preserve the same mission
identity, Git input, outcome vocabulary, verifier, and evidence contract. Exact
conversation equivalence is not required and must not be implied.

### Parallel Search

Forking continues only if a timeboxed prototype beats a strong sequential run on
a real task under a fixed token, time, and compute budget. A visually impressive
fan-out without a better verified outcome is not a product.

### Multi-Tenant Hosting

Fern must not host another user's untrusted repository until it has a threat
model and enforced identity, secret mediation, egress policy, audit history, and
an isolation boundary appropriate to hostile code. Docker plus a trusted service
account is not that boundary.

## Delivery Order

1. Deploy and record the current provider-backed phone flow.
2. Rehearse idle wake, host reboot, backup, and restore.
3. Publish measured results and the current architecture.
4. Emit and verify a continuity receipt at the existing safe-stop boundary.
5. Define one bounded OpenCode mission if asynchronous delegation is a real need
   observed during use.
6. Persist mission attempts, Git checkpoints, terminal outcomes, verification,
   and evidence independently of OpenCode's session database.
7. Add authenticated event submission only after direct mission submission is
   reliable.
8. Run one timeboxed best-of-three worktree experiment against a sequential
   baseline.
9. Add one second harness adapter only if the mission envelope preserves useful
   meaning across both runtimes.
10. Add Temporal only when worker failure, delayed approval, or retries expose a
    concrete durability failure in the working mission.

## Research Conclusions

Five independent research tracks examined Cursor, Amp, T3 Code, Claude Code,
Codex, Copilot, OpenCode, agent-branching systems, and Grab's published platform
work. They support these conclusions:

- Remote/mobile control, worktrees, schedules, webhooks, and customer-operated
  workers are already crowded capabilities.
- No reviewed product exposes a vendor-neutral durable mission that carries task
  identity, attempts, policy, verification, and publication evidence across
  harnesses and placements.
- T3 Code already supplies a strong open multi-harness local control surface;
  Fern should not compete by building another chat UI.
- Branching research proves that fast filesystem and VM forks are becoming
  practical, but cannot clone provider state or roll back arbitrary network
  effects. Most systems remain substrate, beta, or research.
- OpenCode V1 can support coarse whole-turn missions. Its Session fork copies
  conversation state, not a workspace. Host-created Git worktrees and fresh
  Sessions are the safe parallel boundary.
- Current OpenCode V2 source has event-sourced Session state and durable prompt
  admission, but execution remains process-local. `Location` is not a shipped
  remote-placement abstraction, and the Durable Object in the source tree is a
  legacy sharing service rather than agent orchestration.
- OpenCode export/import transfers projected conversation data, not live
  execution state. Fern can promise turn-boundary recovery, not mid-turn replay.
- External effects are the hard boundary. Durable retries require idempotency,
  reconciliation, or human approval when an effect may have happened before a
  failure.

## Open Questions

- Does a continuity receipt make a real recovery or audit problem easier for a
  user, or is ordinary backup sufficient?
- Does a portable mission remain useful when transcript and tool semantics vary
  substantially between two harnesses?
- Does best-of-three improve verified outcomes under a fixed cost budget, rather
  than merely reduce wall-clock time?
- Which external effects need first-class adapters instead of a human approval
  gate?
- Can private, commit-bound proof measurably improve review speed or trust?

## Sources

- [Cursor: What we've learned building cloud agents](https://cursor.com/blog/cloud-agent-lessons)
- [Cursor Cloud Agents](https://cursor.com/docs/cloud-agent)
- [Amp manual](https://ampcode.com/manual)
- [Amp: Agents Everywhere](https://ampcode.com/news/agents-everywhere)
- [T3 Code repository](https://github.com/pingdotgg/t3code)
- [Claude Code Remote Control](https://code.claude.com/docs/en/remote-control)
- [OpenAI Codex App Server](https://developers.openai.com/codex/app-server)
- [GitHub Copilot cloud agent](https://docs.github.com/en/copilot/concepts/agents/cloud-agent/about-cloud-agent)
- [Fly.io: Code and Let Live](https://fly.io/blog/code-and-let-live/)
- [Grab: Palana architecture](https://engineering.grab.com/part-2-palana-architecture)
- [Grab: Agent platform](https://engineering.grab.com/how-grab-builds-and-runs-ai-agents-at-scale)
- [Fork, Explore, Commit](https://arxiv.org/abs/2602.08199)
- [Shepherd](https://github.com/shepherd-agents/shepherd)
- [ConTree documentation](https://docs.tokenfactory.nebius.com/sandboxes/overview)
- [Cloudflare Agents](https://developers.cloudflare.com/agents/)
- [Temporal Go SDK](https://docs.temporal.io/develop/go)

Claims about unshipped OpenCode placement work, vendor substrates, benchmark
numbers, and reported Temporal adoption by products without a primary source
must remain labeled as unverified until independently confirmed.
