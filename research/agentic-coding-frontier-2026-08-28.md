# Agentic Coding Frontier And Fern Direction

Research date: 2026-08-28

Repository baseline: `harden/production-readiness` at
`ab945b5a00db3a310b3fcc30fe8bc99669598b6f`

Status: strategy research. Code and maintained architecture documents remain
authoritative for implemented behavior.

## Executive Decision

The agent loop, chat UI, remote control, worktrees, background execution, and
basic multi-agent fan-out are now widely shipped or announced capabilities.
Fern should not try to win by adding another conversation UI, generic model
Gateway, evaluation dashboard, or agent-to-agent chat protocol.

The market is moving from coding assistants toward **software factories**:
unattended jobs in complete disposable environments, started from existing work
surfaces, run in parallel, checked by deterministic and agent reviewers, and
returned as ordinary pull requests. Cursor, Codex, Claude Code, Copilot, Jules,
Warp, Amp, and newer OSS control planes are rapidly commoditizing the execution
and supervision layers. Ramp Inspect and Stripe Minions demonstrate the same
shape inside large engineering organizations.

Fern should therefore not become a broad runtime-agnostic runner. Its strongest
credible wedge is the part those factories still describe weakly:

> **Fern is the transaction and evidence layer for agent-authored changes. It
> binds one admitted intent to one exact Git result, host verification, human
> authority, runtime identity, and journaled App-broker effect, and it does not
> turn an ambiguous timeout or restart into Fern authority to resubmit the
> prompt or repeat a push or PR mutation.**

This is not an exactly-once provider or tool-execution claim. Workspace-`gh`
effects remain outside Fern's publication journal, verification is optional in
the current product, and a future portable record must state which capabilities
were actually composed.

The current OpenCode appliance remains Fern's reference implementation and first
product. The immediate move is to finish its phone-to-verified-draft-PR loop,
deploy it physically, and make its hidden chronology and exact-result guarantees
visible. The next strategic proof is a portable Agent Change Record, offline
verifier, and one observed external decision using an existing Fern task. A
GitHub Check or second runtime adapter follows only if that consumer workflow
requires it. Building a general runner platform does not.

OIDC is not the next-level feature. It becomes useful only when Fern has a second
worker, a second trust principal, or an external service that needs federated
workload identity. Gateway remains a narrow custody and measurement subsystem.
Labs remains conditional. Fleets remain bounded independent jobs, if they are
needed at all.

The honest answer to "is Fern too niche?" is **yes, in its current packaging**.
One owner, one OpenCode profile, one workspace, manual completion, no
notification loop, and an unproven physical deployment is a narrow appliance.
The answer is not to imitate every platform feature. It is to finish the current
loop, extract the unusual correctness kernel into one independently consumable
artifact, and stop if external users do not use that artifact in a real
decision.

## Where The Scene Is Heading

Five shifts are now visible across both major vendors and internal engineering
platforms.

### From Chat To Durable Work

The unit is moving from a chat turn to a durable goal, task, run, subscription,
or routine. Work continues after the initiating client disconnects and can be
resumed, steered, canceled, scheduled, or triggered by repository events.

### From Local Checkout To Prepared Environment

Leading systems provision an isolated worktree, VM, devbox, or customer
runner with the repository, dependencies, tools, policy, and organization
context already available. Ramp and Stripe show that environment quality and
company-specific tools matter at least as much as the agent loop.

### From One Agent To Bounded Parallelism

Parallel worktrees, candidate races, subagents, and independent workers are
shipping broadly. Large communicating swarms remain expensive and unreliable.
The strongest production pattern is isolated jobs plus centralized verification
and integration, not agents freely editing one shared checkout.

### From Generation To Review Throughput

As more changes are generated, review, CI, conflict resolution, and evidence
become the bottleneck. Deterministic checks, screenshots, telemetry, critic
agents, bounded CI repair, and exact artifacts increasingly define the handoff.

### From User Tokens To Workload Policy

Enterprise platforms are adding short-lived credentials, OIDC, customer-hosted
workers, egress controls, audit logs, and separate read/write boundaries. These
are scale and trust-boundary features, not automatic product differentiation for
a single-owner host.

## What Is Insufficient As Standalone Differentiation

Fern should treat the following widely shipped or announced capabilities as
insufficient standalone wedges:

| Capability | Current evidence |
| --- | --- |
| Phone/browser remote supervision | Codex, Claude Code, Cursor, Copilot, Warp and Amp surfaces |
| Background task to branch or PR | Every major hosted coding-agent product reviewed |
| Worktrees and isolated parallel runs | Codex, Claude Code, Cursor, Copilot, Jules, xAI and OSS control planes |
| Schedules, triggers, and persistent goals | Codex, Claude Code, Cursor, Copilot, Jules and xAI |
| Structured headless or agent APIs | Codex, Claude Code, Cursor, Copilot and Jules |
| Environment snapshots and warm starts | Cursor Builds, Jules, Codex cloud, Ramp Inspect and Stripe Minions |
| Customer-hosted execution | Claude self-hosted environments, Cursor BYOM and Copilot runners |
| OIDC/workload identity | Codex, Cursor and the GitHub Actions ecosystem |
| Basic multi-agent fan-out | Amp, Claude Code, Copilot `/fleet`, Cursor, Warp and xAI |
| Multi-harness control UI | T3 Code, Warp and several OSS control planes |
| Generic model routing and metering | LiteLLM, Portkey, Cloudflare AI Gateway and agentgateway |
| General evaluation infrastructure | Stet, RepoAgentBench, Harbor and Inspect AI |

The implication is not that Fern must abandon remote control. It is that remote
control is the delivery surface, not the strategic claim.

## Market Map

The products differ in distribution and emphasis, but their trajectories now
overlap heavily.

| Product | Direction | Important implication for Fern |
| --- | --- | --- |
| OpenAI Codex | Local, SSH/connected-host, and cloud execution converging into one app with worktrees, subagents, automations, review, mobile control, hooks, SDKs, and enterprise workload identity | Parallel remote work and OIDC are already platform features; a Fern clone loses on distribution |
| Anthropic Claude Code | Composable local/headless agent plus remote control, hosted routines, customer-hosted runners, experimental teams, and multi-agent review | Its structured headless contract is a useful adapter candidate; its team limitations argue against swarms |
| Cursor | Durable cloud agents, resumable run events, goals/subagents, versioned environment builds, BYOM pools, OIDC, audit, artifacts, and CursorBench | The strongest direct threat to a general Fern control plane |
| GitHub Copilot | Agent work embedded in repository identity, Actions runners, issues, PRs, CI, policy, metrics, budgets, and many dispatch surfaces | GitHub already owns the repository and effect authority; Fern must add stricter cross-agent evidence, not another GitHub task UI |
| Google Jules / Gemini CLI | Hosted short-lived Jules VMs with snapshots, schedules, parallel candidates, critics, CI repair, and API activities; separate open-source local Gemini runtime | Another complete task-to-PR path; also shows that hosted and local runtimes can remain separate products |
| xAI Grok | Local-first/open agent harness plus goals, dashboards, workflows, large fan-out, and always-on cloud computers | Strong evidence that raw agent scale will be marketed aggressively; weaker public evidence that 100+ agent coding swarms are economical |
| Pi | Minimal local harness with JSON/RPC/SDK modes, branchable JSONL sessions, broad providers, deep TypeScript extension points, and an experimental transport-neutral remote protocol | A strong future runtime component, not a managed control plane or evidence authority |
| Warp | Terminal-native agent platform expanding into durable orchestration across local/cloud workers, parent-child runs, factories, and ordered lifecycle/message events | A generalized orchestration surface already exists; Fern should not build a workflow builder |
| Amp | Opinionated coding agent with reusable skills/custom agents, isolated subagents, agent-to-agent messaging, and remote Orbs | Agent composition and remote sessions are not open whitespace |
| T3 Code | Provider-neutral control surface around independent agent threads and durable event-sourced state | A better UI over multiple harnesses is already being attempted; its rejected fleet PR also shows orchestration is not required for a useful control surface |
| OpenCode | Open-source agent runtime and server increasingly used as a substrate by products such as Ramp Inspect | OpenCode remains replaceable compute; Fern must validate newer batch/server contracts rather than treating its old pin as the market boundary |
| Ramp Inspect | Internal OpenCode-based background agent with fresh full-stack VMs, company tools, many entry surfaces, candidate races, and verification-rich PR handoff | The environment and review loop create adoption; OpenCode itself is only one component |
| Stripe Minions | One-shot unattended jobs from Slack to human-reviewed, CI-passing PRs in isolated prepared devboxes | Production value is concentrated in separable work, environment quality, bounded loops, and review evidence |

The public capability lists above are not equivalent to independently verified
reliability. Some newer fleet, goal, BYOM, and self-hosted surfaces remain beta,
preview, experimental, or vendor-claimed. The strategic conclusion needs only
the weaker fact: enough credible alternatives ship each generic feature that
Fern cannot use it as a standalone wedge.

## Major Vendors In Detail

### OpenAI Codex

OpenAI is converging local desktop work, SSH/connected-host execution, and cloud
containers into one Codex surface. Worktrees, parallel chats, subagents, review,
automations, hooks, SDK/app-server interfaces, and mobile supervision make the
unit a durable delegated task rather than one terminal conversation. Cloud
environments add setup scripts and controlled internet; enterprise controls add
RBAC, analytics, compliance events, and beta OIDC/SPIFFE workload identity.

The strategic direction is distribution plus governed agent operations. Fern
cannot beat that by adding remote dispatch or OIDC. Codex's structured batch
events may be useful input to a Fern conformance test and change record.

### Anthropic Claude Code

Claude Code is separating several execution modes cleanly. Remote Control keeps
execution on the user's host while syncing supervision through Anthropic;
hosted cloud sessions and routines continue without that host; public-beta
self-hosted runners execute inside customer networks while Anthropic retains
queueing, transcripts, control-plane, and inference authority. Headless mode and
the Agent SDK expose structured output, resume identity, cost, schemas, and
terminal status.

Agent teams and multi-agent Code Review show Anthropic moving toward coordinated
specialists and independent review, but teams remain experimental with explicit
resumption and coordination limitations. Claude's headless contract is one of
the strongest second-adapter candidates; its own caveats reinforce Fern's choice
not to normalize every runtime into false common semantics.

### Cursor

Cursor is the closest broad commercial version of the control plane Fern might
otherwise try to become. Cloud Agents separate durable agents from individual
runs, expose terminal states, cancellation, usage, artifacts, and resumable
events, and now add long-lived goals and subagents on separate VMs. Versioned
Builds retain environment provenance and fallback; BYOM pools can claim,
hibernate, and restore customer workers. OIDC, service accounts, hooks, egress
policy, audit, signed commits, and CursorBench cover enterprise and evaluation
layers.

This is why a general Fern runner is a bad strategic bet. Fern's possible value
is a stricter record independent of Cursor, not a smaller Cursor.

### GitHub Copilot

GitHub is embedding agent work directly into the system that already owns
repository identity, issues, Actions environments, branches, checks, PRs,
rulesets, CODEOWNERS, budgets, and audit. Cloud-agent tasks can start from many
surfaces and automations; organizations can select hosted or self-hosted runners;
custom agents, hooks, skills, MCP policy, memory, metrics, and OIDC-backed
external access complete the platform shape.

Fern should assume GitHub wins generic repository workflow integration. A Fern
GitHub Check is useful only if it communicates exact cross-runtime authority and
ambiguity facts GitHub's native task records do not already establish.

### Google Jules And Gemini CLI

Jules runs tasks in short-lived hosted VMs with reusable validated snapshots,
plans, immutable activities, schedules, suggested tasks, repository memory,
parallel candidates, critics, PR-comment response, and automatic CI repair.
Gemini CLI remains a separate local open-source runtime with its own sandbox and
orchestration work. That separation is a useful precedent for keeping Fern's
interactive OpenCode path and any batch adapter semantically distinct.

### xAI Grok

xAI is pursuing the most aggressive scale story: a local-first/open Grok Build
harness, a dashboard for parallel sessions, durable `/goal` work, workflows that
fan out to many agents, and an early always-on Grok Bot with memory and routines.
The shipped surfaces establish intent to build an agent labor system. Public
launch material provides weaker evidence for governance and for the economics of
large coding swarms than Cursor, GitHub, or enterprise factory accounts do.

## Pi, Amp, Warp, T3 Code, And OpenCode In Detail

### Pi

"Pi" most plausibly means the open-source `earendil-works/pi` coding agent. It is
a deliberately small, embeddable local harness, not a hosted agent platform. It
ships interactive, JSON, RPC, and SDK modes; branchable JSONL session trees;
broad model-provider support; packages, skills, and TypeScript extensions that
can replace tools or add permissions, subagents, checkpoints, MCP, SSH, or an
external sandbox. It has no built-in permission boundary and normally runs with
the invoking user's authority.

Pi's 2026 experimental remote-session work is strategically interesting: a
transport-neutral CBOR protocol, snapshots, event projection, and client APIs
could make it easier to embed beneath another control plane. It still does not
provide first-party hosted workers, a publication broker, or Fern's durable
task/result/effect authority. For Fern, Pi is an adapter candidate, not a product
direction to copy.

### Amp

Amp has moved from a coding client toward a distributed durable agent system.
Its authoritative unit is a thread that can run locally, on a user runner, or in
a hosted persistent Orb; active threads can be controlled remotely, Orbs sleep
and wake with their environments, agent-to-agent delegation crosses runners,
and Orb OIDC can carry user/project/workspace/thread claims. Amp also has the
web, mobile, CLI, native, plugin, skill, MCP, and webhook breadth Fern lacks.

This directly commoditizes Fern's original "start remotely, disconnect, return
later" thesis. Public Amp material does not document Fern-equivalent exact Git
result authorization, host verification of that same commit, or write-ahead
publication reconciliation after lost responses.

### Warp

Warp is becoming a configurable automation and software-factory platform. It
combines terminal and CLI agents with cloud or customer-hosted workers, reusable
environments, APIs, triggers, multiple harnesses, nested orchestration, and
ordered run lineage. Factories add a durable work item above specialist triage,
specification, implementation, and advisory review runs.

Even self-hosted Warp execution remains part of a split control plane, and its
Factories surface was still Early Access at the research date. Those caveats do
not create a reason for Fern to compete on orchestration breadth. They strengthen
the case for an independent exact-result and policy record that can sit beside a
runner rather than replacing it.

### T3 Code

T3 Code is the strongest correction to any claim that Fern uniquely provides
durable local/remote agent control. Its server drives Codex, Claude Code, Cursor,
Grok, and OpenCode; native mobile, desktop, web, relay, Tailscale, and SSH clients
connect to it. Commands are serialized, accepted command receipts are durable
and idempotent, events and projections commit transactionally, and Git
checkpoints bracket turns.

The remaining semantic difference matters: a provider turn reaching a terminal
state and a Git checkpoint support status, diff, and revert, but do not by
themselves prove host verification of one user-authorized immutable result or
bind that result to a write-ahead publication effect. Fern should not chase T3's
client breadth. T3 could eventually be a producer of Fern-style records if its
runtime and checkpoint contracts can supply the required exact facts.

### OpenCode

OpenCode is becoming a broad open runtime and client/server platform with tools,
custom agents, subagents, plugins, skills, MCP, ACP, SDKs, many providers, and
commercial Zen/Go/enterprise layers. It is also a moving contract. Stable V1
`v1.18.25` exposes durable prompt admission, a persisted inbox, paginated session
history, and per-session replay, but `session.wait` remained unavailable on
2026-08-28. OpenCode 2 remains a separate beta with intentionally changing
plugin, server/client, and terminal contracts.

Fern's pinned `0.0.0-next-17444` V2 profile is neither current stable V1 nor a
stable OpenCode 2 contract. Fern should black-box current V1 server replay and a
pinned `opencode run` batch path without assuming an automatic upgrade. OpenCode
is the first execution substrate, not the durable authority of a future portable
Fern record.

## What Fleets Actually Mean

"Fleet" currently covers four different products:

1. local subagents that return summaries to one lead;
2. coordinated teams with a task list or messages;
3. durable remote runs scheduled into separate workspaces;
4. research swarms with recursive decomposition and custom merge machinery.

The demonstrated value is uneven. Parallel research has produced strong gains,
but multi-agent studies also show large regressions, error amplification, and
coordination cost. Coding demonstrations from Anthropic and Cursor required
specialized schedulers, version-control machinery, extensive tests, and high
token spend. Claude teams remain experimental; Warp Factories remain early;
Copilot `/fleet` warns about separable work and shared-write risks.

The useful numbers are cautionary rather than a mandate. Anthropic reported a
90.2% internal-eval gain for breadth-first research but roughly 15 times the chat
tokens. A 260-configuration agent-scaling study measured outcomes from +80.8% to
-70.0% versus a single agent and much higher error amplification without
centralized verification. Cursor's large coding swarms required custom
version-control and merge agents; published runs cost roughly $1,339 to $10,565,
and an earlier design accumulated more than 70,000 conflicts. These prove that
parallelism can work when decomposition and verification are engineered. They do
not prove that a general coding swarm is the next feature for Fern.

Fern should build fleet-compatible identities, budgets, artifacts, cancellation,
and recovery, not a fleet scheduler. If real work later justifies concurrency,
the first form should be a small fixed number of independent children, each with
its own checkout and result, followed by centralized verification. No peer
messaging, recursive delegation, shared writable repository, or automatic merge.

## Ramp Means Inspect

The likely "Ramp" reference is Ramp, the finance company, and its internal
background coding agent **Inspect**. No credible current commercial coding agent
named simply Ramp was found.

Ramp's official account is unusually useful evidence because Inspect is built on
OpenCode but goes far beyond an OpenCode session:

- every session starts in a fresh Modal VM containing Ramp's full development
  environment;
- repository/environment images are rebuilt and snapshotted frequently;
- sessions can resume from post-work snapshots;
- agents can use company-specific GitHub, Buildkite, Sentry, Datadog,
  LaunchDarkly, Braintrust, browser, terminal, and internal-tool context;
- users can launch several prompts/models concurrently and choose a result;
- tests, telemetry, screenshots, live previews, and ordinary human-reviewed PRs
  form the handoff;
- users can start work from web, Slack, mobile, voice, a Chrome extension, or
  hosted VS Code.

Ramp reported roughly 30% of merged frontend/backend PRs shortly after launch;
later public accounts report more than half and then over 60%. These numbers show
adoption, not causal productivity: public evidence does not establish hours
saved, defect rates, review load, or comparable task complexity. Linear's Ramp
case study explicitly says the bottleneck moved from writing code to reviewing
it.

The lesson for Fern is not to copy Ramp's Modal fleet, multiplayer UI, or
enterprise integration count. The lesson is that the useful product is the
complete job environment and verified handoff, not a more durable chat thread.
Fern's exact result, verifier, and publication receipts are well placed for that
handoff. Fern should certify and govern that handoff across runtimes rather than
trying to reproduce Ramp's internal platform at personal-project scale.

## What Would Actually Move The Needle

A Fern direction moves the needle only if it:

1. creates a recurring user job rather than an infrastructure checkbox;
2. broadens beyond one pinned OpenCode workspace without discarding Fern's
   correctness advantages;
3. produces a compelling end-to-end result in roughly four weeks;
4. remains useful on one machine without Redis, Kubernetes, OIDC, or a hosted
   control plane;
5. has a credible distinction from vendor cloud agents and multi-harness UIs;
6. can be killed by evidence if users prefer a simpler existing product.

Under that test, publishing and polishing the current release is necessary but
not a strategy. Gateway credential custody is useful but not a product wedge.
Labs is a later comparison mode, not the foundation. Sophisticated fleets are a
later scheduler shape, not the first build.

## Why Fern Has A Real Starting Point

Fern's useful assets are not generic task tables. The current code already
connects several difficult authority boundaries:

- task, attempt, receipt, actor, base SHA, and exact OpenCode IDs commit before
  wake or delivery;
- ambiguous delivery reuses exact IDs and refuses speculative prompt mutation;
- cancellation, lifecycle, and repository operations are fenced against stale
  workers and mutable state;
- user sealing binds one exact clean commit and manifest without falsely calling
  the agent successful;
- host verification checks the same exact commit before and after the
  verification command;
- GitHub publication commits mutation intent before push/PR effects and uses
  exact reads after response loss instead of blind retry;
- release, compatibility, backup, and restore machinery can anchor software and
  recovery provenance. A host signing identity and key lifecycle would be new
  work.

That chain is more defensible than remote wake or task persistence alone. It is
also largely invisible in the current product. The embedded page cannot request
the already-implemented App publication, has no useful evidence timeline, and
the physical Ubuntu/phone/replacement-host journey has not occurred. Fern has a
correctness kernel wrapped in an incomplete workflow.

## The Whitespace Is Narrow And Unproven

The proposed record sits between existing categories rather than owning an empty
market:

| Existing category | What it proves or records | What remains for Fern to test |
| --- | --- | --- |
| Agent transcripts and observability | Messages, tools, timing, tokens and runtime events | Whether those events bind the exact commit verified and published after failures |
| GitHub checks and CI | One commit passed named checks | Which admitted task, actor, runtime and effect chronology belong to it |
| SLSA, in-toto, Sigstore and build attestations | Artifact build provenance and signed statements | A coding-agent change transaction and human/effect authority before the build |
| AgentDiff-style attribution | Claimed agent/model authorship for lines or commits | Exact task/result verification and conservative external-effect recovery |
| Durable workflow engines | Persisted steps, retries, schedules and signals | Domain-specific proof of Git, agent and publication ambiguity |
| Vendor agent records | Rich evidence inside one provider's platform | Cross-runtime policy and verification independent of that provider |

This is not yet a validated business. A better-distributed control plane could
add similar exports, and platform teams may decide that CI plus vendor audit logs
are enough. The moat would have to become a strict evidence contract, offline
verifier, runtime conformance corpus, policy integrations, and a reputation for
never treating missing evidence as success. The post-acceptance prototype is
designed to find out whether anyone wants that, not to assume it.

## OSS Source Audit

A source-level audit pinned Warren, Orbit, Deputies, AgentRouter, agentserver,
Codex Router, and `agentdiff` on 2026-08-28. None implemented the complete chain.
The [detailed matrix and file-level evidence](./agent-change-record-competitor-audit-2026-08-28.md)
are retained separately.

```text
admitted authority
  -> exact base/result/tree
  -> verification of that same result
  -> write-ahead publication
  -> ambiguity reconciliation
  -> signed portable export
  -> offline policy verification
```

The important correction is that the individual primitives are not novel.

| System | Strongest adjacent implementation | Missing from the complete chain |
| --- | --- | --- |
| Warren | Exact base pin, default-branch protection, head/base-idempotent PR find/create | Dedupe and finalize intent are process-local; no durable publication journal, exact-result verifier, or signed export |
| Orbit | Exact Git/tree/test checkpoints, durable write-ahead merge operations, exact-tip gates, durable SHA-bound receipts, partial-outcome handling | No concrete PR creation, GitHub Check/required-merge wiring, or portable signed offline-verifiable export located |
| Deputies | Durable integration-delivery dedupe and concrete Git/PR tools | No commit-bound intent, exact-result verification, publication reconciliation, or signed result export located |
| AgentRouter | Durable idempotency, canonical action digests, approval bound to action, ordered policy/execution events | Repository output is a binary diff/status rather than an immutable commit/tree seal; no publication transaction or signed result export located |
| agentserver | Signed canonical run manifests, digest-bound approvals, immutable checkpoint finalization, explicit ambiguous checkpoint recovery | Checkpoints attest runtime objects rather than an exact repository commit/tree publication chain |
| Codex Router | Durable idempotency and startup reconciliation with `needs_attention` | Tests are not bound to the recorded Git state; push/merge authority is inferred from approval text |
| agentdiff | Signed Git-native authorship traces, offline signature verification, and installable PR policy workflow | Explicitly authorship, not execution quality, same-result verification, or publication transaction proof |

Orbit is the closest transaction competitor. `agentdiff` most clearly narrows
the positioning: Fern must not claim to invent cross-agent provenance, signed
records, offline verification, or CI policy. The plausible whitespace is their
composition around one exact repository change and its effect chronology.

The resulting product language should be:

> **Host-attested change transaction with exact-result verification and
> conservative publication recovery.**

It should not be "AI authorship provenance," "independent proof the model did
the work," or "the first durable agent control plane."

## Recommended Product Shape

Fern should separate three layers rather than expand one appliance into a broad
platform.

### Reference Product: Verified Remote Handoff

Keep the current OpenCode workspace for conversations, terminals, phone access,
manual exact-snapshot authorization, lifecycle continuity, and receipt-backed
publication. Finish the missing user loop:

```text
submit
  -> inspect or answer in OpenCode
  -> authorize one exact snapshot
  -> host verification
  -> request one draft PR
  -> inspect chronology and exact evidence
```

The App publication API already exists, but the phone UI cannot initiate it.
The durable chronology is also compressed into terse status strings. Exposing
those two existing capabilities has higher near-term value than a new backend.

### Reusable Product: Agent Change Record

Define the minimum portable record that binds:

- admitted task, actor, authority, and exact base commit;
- runtime/adapter and release identity, with explicit trust claims;
- exact result commit and required Git objects;
- host-owned verification policy, output digests, and result;
- user-seal receipts, plus approval receipts only when a durable approval
  contract is implemented;
- publication intent, mutation boundaries, exact reads, and final effect;
- redacted chronology, explicit omissions, and any unresolved ambiguity;
- host signature and manifest digest.

Ship an offline verifier. Add a GitHub Check projection only when the first
consumer needs merge policy. Call the record
**host-attested, tamper-evident evidence**, not proof that a model executed the
recorded prompt. A compromised Fern host or signing key remains authoritative
inside the current trust boundary.

The first target consumer is a platform or security engineer deciding whether
an agent-authored PR may merge. The recurring job is to apply one repository
policy to exact changes produced by different agent systems without trusting a
transcript or provider dashboard as proof of the tested and published commit.

The schema needs two explicit producer modes:

- **Fern-wrapped:** Fern admits exact intent and authority before execution and
  can make only the claims supported by its configured adapter and effect path.
- **Imported producer claim:** another runtime supplies post-hoc chronology or
  artifacts. Fern may verify integrity and the Git result, but must not relabel
  those claims as Fern-admitted input, terminal, cancellation, or effect facts.

Every section is capability-typed as present, `not_configured`, `unavailable`,
or `unresolved`. The record is strategically useful only if a second runtime can
produce useful typed facts and another machine can verify them. OpenCode is the
first producer, not the schema.

### Compatibility Product: Runtime Conformance

Use Fern's existing black-box contract style to test exact runtime behavior:
caller-selected IDs, input binding, terminal classification, cancellation,
response loss, process restart, environment replacement, and unknown events.
Add one structured batch candidate such as Claude headless, `codex exec --json`,
or a newer pinned `opencode run` profile.

This does not require Fern to own the user's fleet. A minimal adapter can ingest
an exact Git result and capability-typed producer claims from an existing
runner. Full input, terminal, and cancellation authority is required only when
Fern wraps that runtime before execution. Imported mode may leave those fields
`unavailable`; it must never promote a provider claim into Fern authority.

## How Later Features Fit

- **Headless jobs:** a reference producer for the change record, not a new
  general control-plane category.
- **Fleets:** several independent records and results with isolated workers.
  Start only after serial portability works; avoid peer-agent authority.
- **Labs:** repeated records across configurations, only if the no-build pilot
  changes a real decision and current evaluation tools are insufficient.
- **Gateway:** optional credential custody, budgets, and provider-attempt facts.
  It strengthens a record but is not required for quality-only verification.
- **OIDC/workload identity:** standards-based authentication for a second worker
  or external resource. Bind its subject and audience to an exact Fern attempt.
- **Mobile:** a triage and authority surface for inspect, seal, cancel, publish,
  and important notifications, not another full coding editor.

## Direction Scorecard

| Direction | User value | Fit with implemented Fern | Crowding | Decision |
| --- | --- | --- | --- | --- |
| Finish verified phone-to-PR handoff | Immediate for current users | Very high | Medium | Do now |
| Portable Agent Change Record and verifier | Unknown pending observed merge/rollout/audit use | High | Medium, but exact cross-boundary contract remains unusual | Strategic proof |
| Runtime durability/conformance kit | Useful to agent/platform maintainers | High | Low-medium | Distribution and credibility path |
| General self-hosted agent runner | Real | Medium | Very high | Do not pursue as category |
| Multi-harness desktop/mobile UI | Real | Low | Very high | Do not build |
| General LLM Gateway | Low as standalone Fern job | Medium | Very high | Narrow subsystem only |
| Standalone Labs platform | Conditional | Medium | High | No-build pilot first |
| Complex communicating swarm | Unproven for this user | Low | High | Do not build |
| OIDC, Redis, PostgreSQL, Kubernetes | Trigger-dependent | Low today | Commodity infrastructure | Not now |

## Ordered Execution Plan

### Step 1: Finish And Prove Current Fern

Do not change the strategic product boundary before the existing claim survives
real operation:

- publish one signed, attested release;
- deploy it to the planned Ubuntu host;
- complete phone TLS/WSS, reboot, revocation, ACL denial, provider-funded task,
  verified draft PR, backup, and replacement-host restore;
- add the missing publication action to the embedded task page with persisted
  idempotency state;
- expose a bounded task chronology and exact result/verification/publication
  summary;
- dogfood it for two weeks and record every laptop escape and recovery action.

This is polish in the strategic sense, but it is also mandatory credibility.
Fern cannot sell reliability while its primary physical journey is unproven.

### Step 2: Four-Week Post-Acceptance Prototype

After physical acceptance, test the evidence-layer thesis without building a
platform. This is a separate four-week prototype after release and dogfood, not
a claim that the entire sequence fits into four calendar weeks from today.

**Week 1: Competitive and consumer test**

- use the pinned OSS source audit as the competitive baseline and refresh only
  projects whose relevant implementation materially changed;
- show a concrete proposed record to at least five platform, security, or
  developer-productivity engineers;
- identify one merge, rollout, compliance, or incident decision the record would
  actually affect.

**Week 2: Export and offline verify**

- export one existing Fern task, result, verification, publication chronology,
  release identity, and required Git objects;
- sign a canonical manifest with an explicitly prototype-scoped key and verify
  integrity on a clean second machine;
- state trust and omissions directly in verifier output.

**Week 3: External use and failure proof**

- inject lost responses after push or PR-create start and show restart
  reconciliation without duplicate mutation;
- show exact failures for a changed commit, missing object, invalid signature,
  failed verification, and unresolved ambiguity;
- complete one external installation and observe whether the record affects a
  real merge, rollout, compliance, or incident decision.

**Week 4: One consumer-driven extension**

- if merge policy is the demonstrated job, project the record into one GitHub
  Check; otherwise do not build the Check;
- if cross-runtime input is the demonstrated job, black-box one structured batch
  runtime and import capability-typed facts without weakening Fern's terminal
  semantics; otherwise defer the second adapter;
- record additional teams' installation intent only as a continuation signal,
  not validation.

### Step 3: Choose From Evidence

Proceed with the portable layer only after at least one external installation
and one observed merge, rollout, compliance, or incident decision where the
record matters. Interest from three teams is a useful continuation signal, not
validation. If users value only the integrated Fern appliance, keep the record
internal and continue as a focused personal tool. If neither loop recurs, stop
expanding Fern after the operational proof.

## Comparison With Current Fern Documents

The maintained documents are technically candid, but this market review changes
their emphasis.

### Keep

- `docs/ARCHITECTURE.md`, `docs/SECURITY.md`, and `docs/TASK_MODEL.md` correctly
  describe a single-owner trusted workspace and do not pretend Docker is hostile
  multi-tenant isolation.
- `docs/ROADMAP.md` correctly requires a signed release, physical deployment,
  replacement-host restore, dogfood, and a no-build Labs gate before expansion.
- The Gateway G0 contract-first work remains valid if credential custody or exact
  provider measurement is needed.
- The refusal to infer completion from idle, disconnect, process death, or an
  empty inbox should not be weakened for product convenience.

### Change After The Proof

- `README.md` leads with sleeping remote workspaces and says coding agents stop
  when the initiating laptop closes unless the user rents someone else's
  sandbox. That is no longer a credible market contrast given connected-host,
  BYOM, self-hosted runner, and hosted background-agent products.
- `docs/REMOTE_PRODUCT.md` calls Fern a self-hosted control plane. Keep that as
  the implemented reference product, but do not use the broad control-plane
  category as the long-term wedge.
- `docs/ROADMAP.md` currently places portable evidence after Gateway and Labs.
  Keep final schema stabilization late, but move a deliberately provisional
  one-task export and offline verifier forward as the strategic demand test.
- Labs should remain conditional and should consume the same record if built. It
  should not become Fern's identity merely because evaluation is adjacent.
- The README and product surface should eventually lead with exact verified
  handoff and safe ambiguous-effect recovery, but only after a physical demo and
  external demand proof make those claims legible.

No maintained architecture claim needs to change immediately. The research
recommendation should be tested before replacing the current product direction.

## Product Versus Portfolio

The sequence differs depending on the objective:

| Objective | Next work |
| --- | --- |
| Make Fern a useful product | Publish, physically deploy, finish the phone-to-verified-PR handoff, dogfood, then test one provisional change record in an observed external decision |
| Demonstrate fit for the Grab backend-AI role | Apply now; after the baseline proof, build the narrow Gateway G0/G1 streamed-provider and fault/reconciliation demonstration described in the existing roadmap |

Gateway G0/G1 is a strong portfolio artifact because it demonstrates streaming,
credential custody, scoped identity, metering, cancellation, and ambiguity. It
is still not the best unsupported claim about what Fern users need next. Do not
let a portfolio exercise silently become the product architecture.

## OIDC Decision

Do not build OIDC next. Build it only when at least one of these is true:

- Fern enrolls a second worker machine that must authenticate without a shared
  long-lived secret;
- more than one human or service submits jobs under different policy;
- an agent needs short-lived access to an external cloud service that accepts
  federated workload identity;
- an external team requires SSO/workload identity before a real pilot.

Until then, Fern's existing device identity, task actor snapshots, GitHub App
broker, and a future hashed scoped worker/run token are the smaller correct
tools. A toy identity provider would be portfolio theater.

## Kill Criteria

Abandon the portable evidence direction if:

- source audit finds two credible systems already exporting and independently
  verifying the same intent/result/verification/effect chain;
- no external team completes installation against a real repository after
  qualified demand testing;
- no observed merge, rollout, compliance, or incident decision changes or gains
  useful confidence from the record;
- a demonstrated consumer requires Fern-wrapped cross-runtime execution and the
  selected runtime cannot provide exact enough input, terminal, cancellation,
  and result contracts without fragile scraping or invented success;
- users consider ordinary GitHub checks and vendor logs sufficient;
- portability requires weakening Fern's exact identity, cancellation, result,
  verification, or effect fences.

Independently, stop expanding the appliance if two weeks of real use does not
establish a recurring remote verified-handoff job. If either thesis fails,
retain the signed release and fault demonstrations as a strong engineering
artifact rather than adding enterprise-shaped infrastructure to a niche tool.

## Research Limitations

- Shipping interfaces and official documentation support the market-direction
  claims; they do not prove vendor reliability or customer return on investment.
- Ramp, Stripe, Devin, and other adoption figures are company or vendor reports,
  not controlled productivity studies.
- Several 2026 surfaces are beta, preview, experimental, early access, or rapidly
  changing. Recheck exact contracts before integration.
- The OSS source audit records `not located` separately from explicit absence;
  failure to locate a capability is not proof that no implementation exists.
- Fern Gateway, Labs, portable evidence, a second runtime adapter, fleets, OIDC,
  and generic automatic completion are not implemented.

## Primary Sources

Sources were accessed on 2026-08-28 unless a publication date is shown.

### Software Factories And Enterprise Practice

- [Ramp, "Why We Built Our Own Background Agent"](https://builders.ramp.com/post/why-we-built-our-background-agent), 2026-01-12.
- [Modal, "How Ramp Built a Full-Context Background Coding Agent"](https://modal.com/blog/how-ramp-built-a-full-context-background-coding-agent-on-modal), 2026-02-19.
- [Stripe, "Minions: Stripe's One-Shot, End-to-End Coding Agents"](https://stripe.dev/blog/minions-stripes-one-shot-end-to-end-coding-agents), 2026-02-09.
- [Stripe, Minions Part 2](https://stripe.dev/blog/minions-stripes-one-shot-end-to-end-coding-agents-part-2), 2026-02-19.
- [GitHub, "Security Architecture of GitHub Agentic Workflows"](https://github.blog/ai-and-ml/generative-ai/under-the-hood-security-architecture-of-github-agentic-workflows/), 2026-03-09.
- [Ramp, "You're Spending Too Much on AI. You're Also Using Too Little"](https://builders.ramp.com/post/ai-spend-value), 2026-06-17.

### Major Agent Platforms

- [OpenAI, "Introducing the Codex App"](https://openai.com/index/introducing-the-codex-app/), 2026-02-02.
- [Codex cloud environments](https://developers.openai.com/codex/environments/cloud-environment), [worktrees](https://developers.openai.com/codex/environments/git-worktrees), [remote connections](https://developers.openai.com/codex/remote-connections), and [workload identity](https://developers.openai.com/codex/enterprise/workload-identity).
- [Claude Code Remote Control](https://code.claude.com/docs/en/remote-control), [scheduled tasks](https://code.claude.com/docs/en/scheduled-tasks), [self-hosted environments](https://code.claude.com/docs/en/self-hosted-environments), [headless mode](https://code.claude.com/docs/en/headless), and [agent teams](https://code.claude.com/docs/en/agent-teams).
- [Cursor cloud agent and harness changes](https://cursor.com/changelog/08-19-26), 2026-08-19; [Cursor Builds](https://cursor.com/changelog/08-13-26), 2026-08-13; [BYOM](https://cursor.com/docs/cloud-agent/bring-your-own-machine); and [CursorBench](https://cursor.com/evals).
- [GitHub Copilot cloud agent](https://docs.github.com/en/copilot/concepts/agents/cloud-agent/about-cloud-agent); [comment-triggered automations](https://github.blog/changelog/2026-08-03-trigger-copilot-automations-with-comments), 2026-08-03; and [agent metrics](https://github.blog/changelog/2026-08-07-copilot-usage-metrics-api-adds-agent-app-activity), 2026-08-07.
- [Jules API and immutable activities](https://jules.google/docs/changelog/2026-01-26-4/), 2026-01-26; [CI fixer](https://jules.google/docs/changelog/2026-02-19/), 2026-02-19; and [Jules API](https://jules.google/docs/api/reference/overview/).
- [xAI Agent Dashboard](https://x.ai/news/agent-dashboard), 2026-06-15; [`/goal`](https://x.ai/news/introducing-goal), 2026-06-22; [Grok Build open source](https://x.ai/news/grok-build-open-source), 2026-07-15; [Workflows](https://x.ai/news/workflows), 2026-07-23; and [Grok Bot](https://x.ai/news/introducing-grok-bot), 2026-08-11.

### Composition, Fleets, And Independent Runtimes

- [Pi repository](https://github.com/earendil-works/pi), [coding-agent README](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/README.md), [`v0.84.0` remote protocol release](https://github.com/earendil-works/pi/releases/tag/v0.84.0), and [`v0.84.3` containerization](https://github.com/earendil-works/pi/blob/v0.84.3/packages/coding-agent/docs/containerization.md).
- [Amp, "Agents for the Agent"](https://ampcode.com/notes/agents-for-the-agent), 2025-06-10; [custom agents](https://ampcode.com/news/custom-agents), 2026-06-19; and [agent-to-agent](https://ampcode.com/news/from-agent-to-agent), 2026-07-17.
- [Amp rebuild](https://ampcode.com/news/neo), 2026-05-06; [remote control](https://ampcode.com/news/agents-everywhere), 2026-06-04; [Orbs](https://ampcode.com/news/agents-in-orbs), 2026-06-30; [user runners](https://ampcode.com/news/agents-anywhere), 2026-07-08; and [Orb OIDC](https://ampcode.com/news/secrets-of-the-orb), 2026-07-14.
- [Warp orchestration documentation](https://docs.warp.dev/platform/orchestration/), updated 2026-08-27.
- [Warp self-hosting](https://docs.warp.dev/platform/self-hosting/), [Factories](https://docs.warp.dev/factories/), and [2026 changelog](https://docs.warp.dev/changelog/2026/).
- [T3 Code `v0.0.35` README](https://github.com/pingdotgg/t3code/blob/v0.0.35/README.md), [architecture](https://github.com/pingdotgg/t3code/blob/v0.0.35/docs/internals/overview.md), [remote model](https://github.com/pingdotgg/t3code/blob/v0.0.35/docs/internals/remote.md), and [authentication](https://github.com/pingdotgg/t3code/blob/v0.0.35/docs/internals/environment-auth.md), 2026-08-27.
- [OpenCode documentation](https://opencode.ai/docs/), [stable `v1.18.25`](https://github.com/anomalyco/opencode/releases/tag/v1.18.25), [server API](https://opencode.ai/docs/server/), and [V2 migration](https://opencode.ai/v2/docs/migrate-v1), 2026-08-28.
- [GitHub Copilot `/fleet`](https://github.blog/ai-and-ml/github-copilot/run-multiple-agents-at-once-with-fleet-in-copilot-cli/), 2026-04-01, updated 2026-04-14.
- [T3 Code proposed provider-neutral fleet orchestration](https://github.com/pingdotgg/t3code/pull/5632), closed unmerged 2026-08-23.
- [OpenAI Symphony specification](https://github.com/openai/symphony/blob/main/SPEC.md).
- [Anthropic, "How We Built Our Multi-Agent Research System"](https://www.anthropic.com/engineering/multi-agent-research-system), 2025-06-13.
- [Anthropic, "Building a C Compiler with a Team of Parallel Claudes"](https://www.anthropic.com/engineering/building-c-compiler), 2026-02-05.
- [Cursor, "Agent Swarm Model Economics"](https://cursor.com/blog/agent-swarm-model-economics), 2026-07-20.
- [Towards a Science of Scaling Agent Systems](https://arxiv.org/html/2512.08296), v3 2026-04-08; [MAST](https://arxiv.org/html/2503.13657), v3 2025-10-26; and [Co-Coder](https://arxiv.org/html/2606.00953), v1 2026-05-31.

### Direct Threats And Adjacent Evidence

- Source-audit pins: [Warren `fe10715`](https://github.com/jayminwest/warren/tree/fe1071562ac957aacba39beba850ef00e10d879a), [Orbit `aca9757`](https://github.com/jianghailong-xy/orbit/tree/aca97577e5e57cb2d6d39e503a035115e1cedd9c), [Deputies `60c7e18`](https://github.com/sidpalas/deputies/tree/60c7e186187839a52d56c23d57cf9e22fe9cd5b4), [AgentRouter `8c7e339`](https://github.com/perixtar/AgentRouter/tree/8c7e339f36593d4daf03003a7ca24f7e380e8ed6), [agentserver `3411a15`](https://github.com/agentserver/agentserver/tree/3411a155375dfe8a1843b7f702ae8f5eaed3438a), [Codex Router `56356ec`](https://github.com/rixzkiye/codex-router/tree/56356ec55e36d3360e6e13ea75634e5124b28d78), and [`agentdiff` `f9ffbd2`](https://github.com/codeprakhar25/agentdiff/tree/f9ffbd2b742826b27de7584e104da455d1635f64).
- [agentdiff](https://github.com/codeprakhar25/agentdiff) is a direct adjacent example of cross-agent, Git-native signed authorship records.
- [SLSA provenance](https://slsa.dev/provenance/v1), [in-toto](https://in-toto.io/), and [Sigstore](https://www.sigstore.dev/) are established supply-chain attestation alternatives and potential envelope formats rather than evidence that Fern needs a novel cryptographic container.
