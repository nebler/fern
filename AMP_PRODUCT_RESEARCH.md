# Fern Product Direction: Lessons from Amp, Applied AI, and Grab

Date: 2026-08-15

> **Document status:** Dated product-strategy research, not current feature or
> assurance documentation. It predates checked-in CI, supervised deployment,
> the lifecycle matrix, loopback-only exposure, and V2 compatibility. Use
> [docs/DOCUMENTATION.md](./docs/DOCUMENTATION.md) for current authorities.

## Executive Summary

Fern is not primarily a remote-access product. Running `opencode serve` on an
always-on machine and connecting through Tailscale already provides remote
access, persistent OpenCode sessions, and work that continues after the client
disconnects.

Fern's implemented value is narrower:

> Fern is an external lifecycle controller and stable ingress for one durable
> AI development workspace. It determines when the workspace is safe to stop,
> releases its compute resources, and wakes it transparently on the next
> request.

Amp Orbs provide much more than remote execution or sleep/wake. Amp wraps the
entire asynchronous software task:

```text
intent
  -> prepared isolated environment
  -> durable thread and execution
  -> files, terminal, and live services
  -> verification artifacts
  -> review from web, CLI, mobile, or Slack
  -> event-driven continuation
```

Fern currently wraps this smaller path:

```text
request
  -> safe wake
  -> OpenCode
  -> safe sleep
```

The highest-leverage direction is not to copy Puck, add Redis, or force an LLM
gateway into fern. It is to make the workspace wake **ready to work and ready
to prove its work**, rather than merely waking an OpenCode process.

The recommended product sequence is:

1. Keep the already-shipped `fern attach` wrapper as the supported OpenCode V1
   seam; do not fork OpenCode or generate project configuration unnecessarily.
2. Add CI, deploy fern on an always-on host, and compare it directly with plain
   remote `opencode serve` from a phone.
3. Add deterministic setup, resume, and machine-readable preflight.
4. Add one private, wake-aware preview service and a bounded artifact inbox.
5. Add lifecycle accounting and a concise return-to-work status surface.
6. Add fern's differentiated feature: state-bound verification receipts whose
   freshness is controlled by fern rather than asserted by the agent.
7. Add signed event ingestion only when a real personal workflow needs it.

This direction demonstrates applied-AI product thinking: model reliability is
partly a property of the environment, tools, feedback, and proof contract
around the model.

## The Critical Baseline: Remote OpenCode Versus Fern

For the basic phone use case, fern and plain remote OpenCode are currently
close.

| Capability | Remote `opencode serve` | Fern today |
|---|---:|---:|
| Access from another device | Yes | Yes, with external networking |
| Continue after phone disconnect | Yes | Yes |
| Persistent OpenCode sessions | Yes, if configured | Yes |
| Stable public/private address | Yes, if configured | Yes, through fern ingress |
| Automatic idle stop | No | Yes |
| Request-triggered wake | No | Yes |
| Activity-aware stop boundary | No | Yes |
| Distinguish crash from intentional pause | Manual | Yes |
| Verify runtime ownership and spec drift | No | Yes |
| Deterministic repository setup | Manual | Not yet |
| Restore development services after wake | Manual | Not yet |
| Private application previews | Manual | Not yet |
| Structured proof of completed work | No | Not yet |

The honest distinction is:

> Remote OpenCode provides remote access. Fern gives the remote process an
> activity-aware, request-driven lifecycle.

Fern currently stops a Docker container, not its host. A stopped container
releases its CPU and memory, but an underlying cloud VM may continue billing.
The phrase "paused costs nothing" is therefore not equivalent to Amp's billing
claim. On an already-running home server or shared host, fern returns resources
to the host; it does not yet stop billed infrastructure.

Fern is worth using when automatic suspension, memory reclamation, reduced
process exposure, safe turn boundaries, and external lifecycle control matter.
If an always-running OpenCode process is acceptable, plain OpenCode plus
Tailscale is simpler.

## OpenCode Integration Boundary

Fern should integrate at OpenCode's documented client/server boundary. It
should not try to relocate a live session through a plugin, MCP server, or an
undocumented workspace adaptor.

The implemented OpenCode V1 path is:

```text
fern attach
  -> opencode attach <stable-fern-proxy>
  -> request-driven wake
  -> opencode serve inside Docker
```

`fern attach` already loads the proxy address and server credentials from fern
configuration, passes credentials through the child environment, and starts
the official OpenCode TUI. This is the minimum useful seamless integration and
does not require fern to generate or own `opencode.json`.

### Version Scope

Fern's image and empirical lifecycle research are pinned to OpenCode V1
`1.18.16`. Statements about `opencode serve`, `opencode attach`, `/event`, and
the V1 plugin context describe that tested target.

OpenCode V2 is publicly documented as a beta and differs materially:

- the beta binary is `opencode2`;
- the OpenAPI document is served at `/openapi.json`;
- `@opencode-ai/client` is the generated network client;
- `Service.discover`, `Service.ensure`, and `Service.stop` manage a local
  background service;
- the V2 plugin API uses `Plugin.define` and a capability-based context rather
  than the V1 plugin shape;
- sessions have explicit locations and the public API includes location,
  worktree, session-move, shell, filesystem, and PTY operations.

The V2 public documentation reviewed on 2026-08-15 does not expose a
third-party workspace-provider registration API. Its service and location
model may eventually provide a better fern seam, but the API and plugin system
are explicitly beta. Fern should track V2 without coupling its implementation to
its private control-plane internals.

Use three evidence levels when discussing OpenCode:

| Evidence | What it supports |
|---|---|
| Published V1/V2 documentation | Supported behavior for that version |
| Source pinned to an exact commit | Implemented behavior, possibly internal |
| Issues, changelogs, and maintainer posts | Directional roadmap signal only |

OpenCode's workspace roadmap may commoditize remote placement and make fern's
current attach orchestration less important. It does not automatically replace
fern's independently owned idle policy, request-driven ingress, fail-safe
activity epochs, Docker ownership checks, self-hosted operation, diagnostics,
or future state-bound verification.

## Research Method

This document uses two distinct research passes.

### Pass One: Amp's Product And Workflow

The first pass reviewed Amp's first-party product pages, announcements, notes,
and current Owner's Manual. It focused on what Amp users can do, what state Amp
owns, and where the product removes operational friction.

### Pass Two: The Verification Gap

The second pass searched Amp's current manual for documented proof receipts,
evidence freshness, state-bound test results, and attestations. It then compared
the gap with adjacent verification and provenance projects.

This supports a limited claim:

> No state-bound verification receipt or evidence-freshness feature was found
> in Amp's public documentation reviewed on 2026-08-15.

It does not prove that Amp has no private, experimental, or undocumented
version of such a feature.

The second pass also found that receipts and evidence tooling already exist in
the broader ecosystem. Fern should not claim to invent them. The differentiated
idea is integrating evidence freshness with fern's independently owned
workspace lifecycle and exact runtime generation.

## What Amp Orbs Actually Provide

### The Thread Is The Unit Of Work

In Amp, the user starts a thread for a task. An orb is created for that thread.
The thread combines:

- the task and conversation;
- a fresh repository checkout;
- an isolated machine;
- agent execution history;
- files and diffs;
- terminal access;
- preview services;
- review and artifacts;
- durable continuation.

The user thinks about the work rather than the machine. Fern currently owns one
long-lived workspace, so the user still has to think about that workspace as a
machine and shared checkout.

### Fresh Environments Remove Coordination Work

Amp positions fresh per-thread environments as the main difference from a
homemade remote agent. They remove:

- dirty worktree concerns;
- process and port collisions;
- local memory and disk pressure;
- uncertainty about which task owns a checkout;
- cleanup work after a failed task.

This enables parallel work and longer verification runs. Fern deliberately does
not provide per-task isolation or multi-workspace scheduling.

### One Thread Across Every Client

Amp exposes the same durable thread through web, mobile, CLI/TUI, local runners,
orbs, and integrations such as Slack. Closing a client is safe because task and
execution state do not belong to that client.

Fern delegates conversation, mobile UX, and task history to OpenCode. Fern does
not currently own an inbox, notification system, task index, or result view.

### The Environment Is Legible To The Agent

Amp's strongest applied-AI design appears in its repository environment:

- `.agents/setup` prepares fresh compute;
- `.agents/resume` performs fast wake-time repair;
- idempotent scripts reuse healthy services and repair wedged ones;
- `.amp/dev-ports.json` exposes shared port facts;
- `/__dev/preflight` reports missing requirements as structured data;
- development login endpoints avoid agent-hostile OAuth and 2FA flows;
- logs and browser output live at documented locations;
- `AGENTS.md` documents build, test, verification, and recovery procedures;
- screenshots and recordings have a known artifact directory.

The principle is:

> Do not make an agent infer environment state that the platform already knows.

This can improve agent reliability without changing the model.

### Portals Turn Results Into Something Inspectable

Amp Portals expose declared HTTP services through authenticated HTTPS. Portal
requests wake paused orbs. Services receive managed `PORT` and `PUBLIC_URL`
values and can be supervised across pause/resume.

This changes the result from "the agent says it changed the app" into a live
application the user can inspect. Amp also supports review comments directly
from portal surfaces.

### Amp Encourages Evidence, But The Agent Produces It

Amp's published workflow strongly encourages agents to return screenshots,
videos, test matrices, browser runs, and other evidence. Thorsten Ball's
"irrefutable proof" framing is important because reviewing behavior is often
faster and more useful than starting with a large diff.

The documented proof remains primarily agent-driven:

- the user asks for proof;
- repository guidance tells the agent how to verify;
- the agent executes checks;
- the agent returns artifacts or links.

This is a major UX improvement, but it leaves room for a control-plane-owned
freshness contract described later in this document.

### Events Exist Independently Of Compute

Amp webhooks use a durable URL and queue while the orb sleeps. The current
manual describes:

- persistence before HTTP 202;
- at-least-once delivery;
- handler-level idempotency using event IDs;
- request and queue limits;
- bounded handler execution;
- cancellation and retries;
- endpoint pause, deletion, and revocation;
- explicit selection of headers made available to the handler;
- untrusted payload handling.

One correction to earlier research: Amp does not universally verify each
provider's signature at the platform layer. Plugins request the required
headers and must verify the provider signature. Amp can generate a GitHub
plugin that performs that verification.

### Schedules Resume Context

Amp schedules wake a thread with its saved prompt, context, and history. This
supports recurring investigations and monitoring without keeping compute awake.

### Workload Identity Replaces Static Secrets

Amp orbs can mint short-lived OIDC tokens carrying workspace, project, user,
and thread claims. External systems can grant access from those claims instead
of receiving long-lived credentials in the orb.

This is valuable at Amp's multi-user and multi-project scale. A generic OIDC
issuer is premature for fern's current one-user topology. Tailnet identity is a
more proportional first boundary.

## What Puck Adds

Puck is a natural-language interface over Amp's control plane. It can:

- locate past threads;
- identify work by repository or problem;
- create and configure projects;
- start an agent in an appropriate orb or runner;
- monitor, archive, and manage threads;
- fan work out across agents;
- collect and summarize results.

Puck has leverage because Amp has many projects, threads, agents, machines, and
possible actions. It reduces control-plane navigation and coordination.

Fern has one user, one workspace, one repository, and one OpenCode server. A
natural-language router with one destination has little to decide. Copying Puck
now would add an agent without adding leverage.

The useful part of Puck to copy is the return-to-work summary. Fern should make
these questions cheap to answer:

```text
What was happening?
Is work still running?
What changed?
What checks ran?
Does that evidence still apply to the current files?
What can I open from my phone?
Why did the last attempt fail?
```

This does not require a meta-agent.

## Complete Capability Comparison

| Capability | Remote OpenCode | Fern today | Amp Orbs |
|---|---:|---:|---:|
| Remote agent | Yes | Yes, with networking | Yes |
| Continue after client closes | Yes | Yes | Yes |
| Automatic idle stop | No | Yes | Yes |
| Request-triggered wake | No | Yes | Yes |
| Explicit activity-safe pause policy | No | Yes | Not publicly detailed |
| Fresh environment per task | No | No | Yes |
| Parallel isolated tasks | Manual | No | Yes |
| Durable task index | OpenCode sessions | OpenCode sessions | Yes |
| Web/mobile control surface | OpenCode-dependent | OpenCode-dependent | Yes |
| Deterministic setup | Manual | No | Yes |
| Wake-time repair | Manual | No | Yes |
| Agent-readable preflight | No | No | Yes |
| Supervised development services | Manual | No | Yes |
| Authenticated previews | Manual | No | Yes |
| Artifact and review surface | Manual | No | Yes |
| Durable event triggers | No | No | Yes |
| Schedules | No | No | Yes |
| Workload identity | No | No | Yes |
| Meta-agent control plane | No | No | Puck |
| State-bound evidence freshness | No | No | Not found in public docs |

## Applied-AI Product Principles To Copy

### Make State Explicit

Bad UX asks the model to discover whether dependencies, services, credentials,
and ports are correct. Better UX exposes those facts directly:

```json
{
  "ready": false,
  "checks": {
    "dependencies": "ready",
    "database": "ready",
    "devServer": "stopped",
    "authentication": "missing test user"
  },
  "repair": "run .fern/resume"
}
```

### Give The Agent Idempotent Operations

The model should call a high-level operation rather than improvise process
management:

```text
fern doctor --json
fern service ensure web
fern service logs web
```

### Make Completion Evidence-First

A useful result should contain:

- changed files or commit;
- checks that actually ran;
- exit status and relevant output;
- screenshots, recordings, or benchmark data where applicable;
- a private preview URL;
- remaining uncertainty.

### Make Re-Entry Cheap

Returning from a phone should show state, elapsed time, latest activity,
verification, preview, and failure reason without requiring the user to
reconstruct events from logs.

### Keep Trust Boundaries Explicit

Issue bodies, logs, comments, and webhook payloads are data. Verified sender,
repository, event type, and delivery identity are separate trusted metadata.

### Let Users Express Intent

The target experience is:

> Start the app, verify the change, and show it to me.

Not:

> Find a port, start a process, configure a tunnel, inspect logs, and send a URL.

## Recommended Feature Set: The Fern Workspace Contract

The lowest-cost coherent feature set is a wake-ready workspace contract:

```text
.fern/setup
.fern/resume
.fern/services.yaml
.fern/artifacts/
fern doctor
fern services
```

### Setup

`.fern/setup` runs after fresh compute creation. It installs dependencies,
prepares generated files, and performs one-time repository setup.

Required semantics:

- explicit timeout;
- captured output;
- no endpoint publication before success;
- rollback or clear failure state;
- execution through a separate runtime capability rather than widening every
  lifecycle interface.

### Resume

`.fern/resume` runs only after a stopped-to-running transition, never on an
ordinary request served from the running endpoint cache.

It must be fast, bounded, and idempotent. The first fern version should fail
clearly on timeout rather than leave an unknown hook running in the background.

### Doctor And Runtime Metadata

`fern doctor --json` should report configuration, Docker, repository, volume,
runtime, watcher, service, remote-access, and hook readiness.

Fern should also expose stable machine-readable runtime metadata rather than
requiring the agent to parse human log text.

### One Private Preview Service

Start with one declared HTTP service:

```yaml
services:
  web:
    command: pnpm dev
    port: 3000
    health: /
```

Fern should:

- start or repair the service;
- capture service logs;
- check health;
- expose one stable private route;
- wake the workspace before forwarding;
- restart the service after resume;
- preserve the route across backend endpoint changes;
- keep access tailnet-only initially.

Do not initially build dependency graphs, arbitrary subdomains, public Funnel
access, cross-service CORS rewriting, or a generic process supervisor.

### Artifact Inbox

Reserve `.fern/artifacts/` for screenshots, recordings, test reports, and
benchmarks. Fern may provide an authenticated index and downloads.

Security requirements include path traversal prevention, safe symlink handling,
bounded file and total sizes, explicit MIME handling, and no exposure of the
rest of the repository.

### Target User Experience

The user asks from a phone:

> Add dark mode. Run the tests and show me that it works.

The environment gives the agent deterministic setup, repair, service, preflight,
and artifact primitives. The result can be:

```text
Implemented dark mode.

Tests: 42 passed
Preview: https://fern-host/.../web
Artifacts:
- before.png
- after.png
- mobile.png
```

The workspace later sleeps. This is substantially different from merely
running OpenCode remotely.

## Proposed Differentiator: Evidence-Bound Workspaces

### The Documented Gap

Amp strongly encourages agents to provide proof and gives them excellent tools
for producing screenshots, videos, test results, and previews. The reviewed
public documentation does not describe an independent control-plane receipt
that binds those claims to the exact code and environment state, or marks the
evidence stale after later edits.

The problem is subtle:

```text
tests pass
  -> agent edits another file
  -> old test result is still shown
  -> user sees "tests pass"
```

The test result was once true but is no longer fresh for the current workspace.

### The Idea

Fern can produce a structured verification receipt bound to:

- workspace and request generation;
- Git commit and dirty-diff hash;
- desired runtime spec fingerprint;
- exact command and arguments;
- command start/end time and exit code;
- selected output summary and full log hash;
- service health results;
- artifact names, sizes, and content hashes;
- explicit `PASS`, `FAIL`, or `UNKNOWN` outcomes.

If relevant files, runtime spec, command declaration, or artifacts change, fern
marks the receipt stale rather than continuing to display a green result.

Example:

```json
{
  "version": 1,
  "workspace": "demo",
  "generation": 17,
  "source": {
    "head": "7c470d6",
    "diffSha256": "sha256:..."
  },
  "runtimeSpec": "sha256:...",
  "checks": [
    {
      "name": "go-test",
      "argv": ["go", "test", "./..."],
      "exitCode": 0,
      "result": "PASS",
      "logSha256": "sha256:..."
    }
  ],
  "artifacts": [
    {
      "path": ".fern/artifacts/mobile.png",
      "sha256": "sha256:..."
    }
  ],
  "freshness": "EXACT",
  "limitations": [
    "Passing tests prove only the declared suite passed."
  ]
}
```

### Why Fern Is A Good Place For It

Fern already independently owns:

- the safe busy-to-idle boundary;
- workspace generation identity;
- desired runtime fingerprint;
- Docker execution ownership;
- request admission;
- durable lifecycle state;
- the moment before compute is stopped.

That allows fern to separate agent claims from observed evidence. The agent may
request verification, but the lifecycle controller records what actually ran.

### Honest Novelty Claim

Receipts themselves are not new. The second research pass found adjacent work:

- AI Integrity Receipts records content-addressed provenance for declared
  AI-assisted commits;
- ProofShot bundles browser video, screenshots, console output, and server logs;
- agent-receipts compares agent completion claims with transcript and filesystem
  evidence;
- other evidence-plane projects bind command results to source snapshots;
- Cursor has publicly discussed an internal verification architecture combining
  CI, behavioral artifacts, risk, and review agents.

Fern's defensible differentiation would be:

> Evidence freshness integrated directly with a safe scale-to-zero workspace
> lifecycle, where every receipt is bound to the exact code, runtime spec, and
> workspace generation that produced it.

Do not claim that fern invented receipts, attestations, or proof-carrying work.

### Staged Delivery

#### Version One: Manual Proof

Add an explicit `fern prove` command that runs repository-declared checks and
writes a local JSON receipt. Do not add signatures, a policy engine, or automatic
agent continuation.

#### Version Two: Freshness

Recompute source and artifact hashes when showing status. Display `EXACT`,
`STALE`, or `UNKNOWN`. Never silently present stale evidence as current.

#### Version Three: Lifecycle Integration

Allow an opt-in repository policy to request verification after a completed
busy-to-idle cycle. Cancel or invalidate verification when new work begins.
The workspace may still sleep after failed verification; fern records `FAIL`
rather than keeping compute alive indefinitely.

#### Version Four: Return-To-Work View

Show the latest receipt, preview, artifacts, and limitations from the phone.

### What Not To Build Initially

- cryptographic signatures;
- transparency logs;
- a universal test-command detector;
- natural-language claim extraction;
- auto-merge;
- a scalar trust score;
- automatic test weakening detection;
- generalized compliance exports;
- an LLM judge of correctness.

The first useful primitive is freshness-bound deterministic evidence.

## Other Features And Their Priority

| Rank | Feature | Cost | UX impact | Applied-AI signal | Grab relevance |
|---:|---|---:|---:|---:|---:|
| 1 | CI and Docker integration workflow | Low | Medium | Medium | Very high |
| 2 | Real tailnet deployment and phone dogfooding | Low-medium | Very high | High | High |
| 3 | Setup, resume, and doctor | Low-medium | High | Very high | Medium-high |
| 4 | One wake-aware preview service | Medium | Very high | Very high | High |
| 5 | Artifact inbox and lifecycle status | Low-medium | Very high | Very high | High |
| 6 | State-bound verification receipts | Medium | High | Very high | High |
| 7 | Signed GitHub wake | Medium | High | High | High |
| 8 | Tailnet identity and audit | Medium | Medium | Medium | High |
| 9 | Schedules | Low | Medium | Medium | Medium |
| 10 | Credential egress proxy | Medium | Low initially | Medium | Very high |
| 11 | Multiple workspaces | Very high | Very high | High | High |
| 12 | Puck-style meta-agent | Extremely high | Low at fern's scale | Low | Low |

## Why Redis, Generic OIDC, And An LLM Proxy Are Not First

### Redis

Redis solves shared coordination among processes or replicas. Fern has one
controller and one workspace. In-memory ownership plus `flock` is the more
appropriate primitive. Redis becomes justified by multiple replicas, shared
rate limits, or a durable work queue, not by a job-description keyword.

### Generic OIDC

OIDC is valuable when multiple users, workloads, or public services need
federated identity. Fern's initial phone deployment can remain private to a
tailnet. Tailnet identity and actor attribution are more proportional first
steps.

### LLM Egress Proxy

Fern's current ingress proxy solves stable routing and lifecycle admission:

```text
user -> fern -> OpenCode
```

An LLM credential proxy is a separate egress boundary:

```text
OpenCode -> fern -> model provider
```

It becomes coherent only if fern explicitly expands into secure agent egress
and secret containment. It should not be presented as a small extension of the
existing proxy.

## Grab Position Alignment

The advertised role is specifically for the GrabGPT AI Gateway. Palana is useful
context but is not the exact job description.

### Strong Existing Evidence

- Go backend design;
- reverse proxy behavior;
- unbuffered SSE streaming;
- context cancellation;
- concurrent request admission and wake coalescing;
- health and readiness semantics;
- failure classification and rollback;
- configuration and ownership drift detection;
- race testing and technical documentation;
- a thin `fern attach` integration over OpenCode's supported V1 server boundary.

### Natural Evidence From This Roadmap

- setup/resume/preflight demonstrates production wrapper and platform UX;
- private previews deepen proxy routing and compatibility work;
- lifecycle receipts demonstrate observability and cost/resource accountability;
- verification freshness demonstrates governance and reliable evidence;
- signed webhooks demonstrate authenticated event ingestion and idempotency;
- CI and real Docker tests demonstrate shift-left quality.

### Gaps Fern Should Not Pretend To Fill

- provider-native API translation;
- multi-provider model routing;
- distributed Redis rate limiting;
- token-level cost attribution;
- PostgreSQL chargeback pipelines;
- Kubernetes and cloud deployment;
- Python and LangGraph.

A correct interview statement is:

> Fern is not an LLM gateway. It gave me direct experience with several of the
> same critical-path primitives: streaming proxy behavior, cancellation,
> authentication boundaries, endpoint health, concurrent admission, failure
> recovery, observability, and safe configuration changes.

## CV Assessment

### Current Engineering Signal: 8/10

Fern already demonstrates production-oriented Go engineering: nontrivial
concurrency, explicit state modeling, fail-safe lifecycle policy, reverse
proxying, SSE, recovery, ownership checks, and race-oriented testing. This is
stronger than a typical LLM wrapper or CRUD portfolio project, but it should
not be described as production-operated until real deployment evidence exists.

### Current Product Proof: 5/10

The repository does not yet demonstrate regular remote use, a public demo,
phone access, screenshots, a deployed preview, or operational measurements.

### Direct GrabGPT Gateway Match: 6/10

The low-level backend and proxy primitives transfer well, but fern does not
implement provider routing, distributed quotas, token accounting, or
chargeback.

### Transferable Senior Backend Signal: 8/10

The strongest signal is engineering judgment: explicit ownership, failure-safe
unknown states, cancellation, rollback, desired versus observed state, and
resistance to unnecessary dependencies.

### CV Readiness Today: 6.5-7/10

Before featuring it prominently:

- add checked-in CI;
- add a short demo;
- document an actual Tailscale deployment;
- publish repeated wake/failure measurements;
- provide an installation or release path;
- update architecture documents after implementation changes;
- curate raw experiment outputs and internal planning notes;
- avoid claims of zero infrastructure cost.

With setup/resume, preview, proof artifacts, CI, and a real phone demo, fern can
be an 8.5/10 CV project without Redis, Kubernetes, generic OIDC, or an LLM
gateway.

### Example CV Bullets

Use only bullets supported by shipped behavior and measured results:

- Built and tested a Go control plane for a persistent remote AI development
  workspace, safely suspending compute after verified idle boundaries and
  resuming transparently on the next request.
- Designed a wake-aware reverse proxy with unbuffered SSE streaming, concurrent
  wake coalescing, cancellation propagation, endpoint-generation invalidation,
  and graceful shutdown.
- Preserved repository and OpenCode session state across compute stop and
  recreation while distinguishing intentional pause, external exit, OOM,
  ownership drift, and configuration drift.
- Measured authenticated request-triggered resume at approximately 2.8-3.1
  seconds and validated lifecycle behavior with race tests, Docker integration
  tests, and failure injection.

After the workspace contract ships, a further bullet can be:

- Added deterministic setup and resume hooks, machine-readable preflight,
  wake-aware private application previews, and state-bound verification
  receipts for agent-produced changes.

## Delivery Plan

### Stage Zero: Validate The Need

1. Keep `fern attach` as the implemented V1 seam and test its authentication
   behavior end to end.
2. Run plain remote `opencode serve` through Tailscale for several days.
3. Run fern under the same conditions.
4. Compare operational friction, memory use, reconnects, failures, and wake
   delay.
5. Keep fern's product claims limited to differences observed in practice.

### Stage One: Make The Repository Credible

1. Add CI for format, tests, race tests, vet, build, and image build.
2. Update stale architecture documentation.
3. Add a reproducible private deployment guide.
4. Provide a simple install or release path.
5. Document OpenCode V1 as the tested target and V2 as a beta compatibility
   track rather than silently mixing their APIs.

### Stage Two: Wake Ready

1. Add `.fern/setup`.
2. Add `.fern/resume`.
3. Capture bounded hook logs.
4. Add `fern doctor --json`.
5. Expose runtime metadata.

### Stage Three: Show The Work

1. Add one declared HTTP service.
2. Add a stable private wake-aware route.
3. Add health and service logs.
4. Add a bounded artifact inbox.
5. Test the preview from a phone over cellular.

### Stage Four: Bind The Evidence

1. Add manual `fern prove`.
2. Bind receipts to Git and runtime state.
3. Mark old evidence stale.
4. Show the latest result and limitations remotely.
5. Consider opt-in idle-cycle verification only after manual proof is useful.

### Stage Five: Publish

Record a 60-90 second demo:

```text
phone opens fern status
workspace is paused
send a real task
workspace wakes
agent modifies and verifies the application
open the private live preview
inspect the evidence receipt and artifacts
workspace returns to paused
```

This demonstrates more than remote connectivity. It demonstrates a workspace
that wakes ready, produces inspectable work, records what was actually proven,
and releases compute safely.

## Sources

### Amp First-Party Sources

- [Agents in Orbs](https://ampcode.com/news/agents-in-orbs)
- [What Are Orbs?](https://ampcode.com/what-are-orbs)
- [Amp Orbs Manual](https://ampcode.com/manual/orbs)
- [Putting an Agent in an Orb](https://ampcode.com/notes/putting-an-agent-in-an-orb)
- [What I Want to Tell You About Orbs](https://ampcode.com/notes/what-i-want-to-tell-you-about-orbs)
- [Agents, Everywhere](https://ampcode.com/news/agents-everywhere)
- [Agents, Anywhere](https://ampcode.com/news/agents-anywhere)
- [From Agent to Agent](https://ampcode.com/news/from-agent-to-agent)
- [Meet Puck](https://ampcode.com/news/meet-puck)
- [Right on Schedule](https://ampcode.com/news/schedule)
- [Multiplayer](https://ampcode.com/news/multiplayer)
- [Event Driven Orbs](https://ampcode.com/news/event-driven-orbs)
- [Secrets of the Orb](https://ampcode.com/news/secrets-of-the-orb)
- [Portals into Orbs](https://ampcode.com/news/portals)
- [Amp Owner's Manual](https://ampcode.com/manual)

### Grab First-Party Sources

- [Senior Software Engineer, Backend (AI)](https://www.grab.careers/en/jobs/744000137791699/senior-software-engineer-backend-ai/)
- [Grab AI Gateway](https://engineering.grab.com/grab-ai-gateway)
- [Agent Platform Part 1](https://engineering.grab.com/how-grab-builds-and-runs-ai-agents-at-scale)
- [Palana Part 1](https://engineering.grab.com/palana-part-1-secure-platform-for-ai-agents)
- [Palana Part 2](https://engineering.grab.com/part-2-palana-architecture)

### Adjacent Evidence Research

These projects and reports demonstrate that evidence and receipts are an active
area, not an entirely new category:

- [AI Integrity Receipts](https://github.com/invariant-systems-ai/aiir)
- [ProofShot](https://github.com/AmElmo/proofshot)
- [agent-receipts](https://github.com/0xelitesystem/agent-receipts)
- [Inside Cursor's agent factory](https://arize.com/blog/inside-cursors-agent-factory-how-it-verifies-ai-written-code/)

Third-party sources describe their own claims and should not be treated as
independent validation of adoption, completeness, or production quality.

### OpenCode Sources

- [OpenCode V2 documentation](https://opencode.ai/v2/docs/)
- [OpenCode V2 plugins](https://opencode.ai/v2/docs/build/plugins)
- [OpenCode V2 client](https://opencode.ai/v2/docs/build/client)
- [OpenCode V2 API](https://opencode.ai/v2/docs/api)
- [Fern's pinned OpenCode V1 research](./DAY-1.md)

## Final Position

Amp's advantage over a remote agent is that it integrates the complete
asynchronous task, environment, proof, review, and continuation loop.

Fern should not attempt to reproduce Amp's fleet, Puck, multiplayer, or plugin
ecosystem. It should make one workspace exceptionally legible, recoverable, and
verifiable. Its durable integration seam is OpenCode's documented server
boundary, not a private adaptor or model-visible lifecycle tool.

The strongest product statement is:

> Fern turns an always-on machine into an agent-ready remote workspace that
> sleeps safely when idle, wakes into a known-good development environment, and
> returns state-bound evidence and private previews instead of unsupported
> completion claims.
