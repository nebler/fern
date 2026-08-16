# Deep Research Prompt: Fern, Amp, OpenCode, Remote Coding, and Product Direction

> **Document status:** Reproducibility artifact for the 2026-08-15 research.
> Its repository context is intentionally frozen and V1-centric; it is not a
> current implementation description. Start new reviews from
> [docs/DOCUMENTATION.md](./docs/DOCUMENTATION.md).

## Role

Act as a principal product engineer, developer-tools researcher, distributed
systems architect, and applied-AI product designer. Your job is not to validate
the existing fern strategy. Your job is to challenge it using current evidence,
identify incorrect assumptions, and recommend the smallest defensible product
direction.

Use information available as of the date you perform the research. Put that
date at the top of the report. Treat August 2026 claims in this repository as
historical snapshots that may already be stale.

## Repository Context

The repository is `github.com/nebler/fern`.

Fern is currently intended to be a self-hosted lifecycle controller for one
durable OpenCode workspace. It runs on the host, controls an OpenCode V1 Docker
container, watches OpenCode activity over SSE, exposes a stable reverse proxy,
stops the container only at a safe idle boundary, and wakes it on an ordinary
request.

The current implementation is deliberately narrower than Amp Orbs or a hosted
remote-development platform. It does not currently provide VM scale-to-zero,
multiple workspaces, per-task isolation, a hosted control plane, application
previews, setup/resume hooks, artifact review, verification receipts, generic
OIDC, Redis coordination, Kubernetes, or an LLM egress gateway.

Read these repository files before drawing conclusions:

- `README.md`
- `ARCHITECTURE.md`
- `CODEBASE_GUIDE.md`
- `IMPLEMENTATION.md`
- `DAY-1.md`
- `AMP_PRODUCT_RESEARCH.md`
- `ROADMAP.md`
- `images/opencode/Dockerfile`
- `cmd/fern/attach.go`
- `internal/workspace/manager.go`
- `internal/proxy/proxy.go`
- `internal/watch/`
- `internal/runtime/docker.go`

Inspect the current Git history, worktree, CI configuration, tests, and release
artifacts. Do not infer implemented behavior from planning documents when the
code can answer the question.

## Core Research Question

What should fern become, if anything, given:

- Amp's current Orb, runner, portal, setup/resume, artifact, verification,
  identity, scheduling, webhook, and multiplayer model;
- OpenCode's current supported and experimental workspace extension surfaces;
- the present remote and background coding-agent market;
- fern's actual implementation and realistic development capacity;
- the requirements and signals of Grab's Backend AI / GrabGPT AI Gateway role;
- the standard of product design required to make fern useful rather than just
  technically interesting?

The answer must distinguish among:

1. features that make fern easier to use;
2. features that genuinely differentiate fern;
3. features that merely copy Amp or another competitor;
4. features that improve interview or portfolio evidence;
5. features that would require operating a company rather than maintaining an
   open-source project;
6. features that should not be built.

## Mandatory Research Passes

Perform at least six distinct research passes. Report the methodology and
results of each pass separately before synthesizing them.

### Pass One: Amp, From First-Party Sources

Reconstruct Amp's current product and architecture using Amp's own manual,
product pages, announcements, notes, API or plugin documentation, and public
statements from its team.

Verify at least:

- whether execution location is selected when a thread is created or can be
  changed later;
- the current executor model for local execution, Orbs, and runners;
- project and workspace defaults;
- Orb sizes, pricing, pause timing, wake behavior, and billing semantics;
- whether `.agents/setup` is cached or snapshotted and for how long;
- `.agents/resume` timeout and failure behavior;
- portal declaration, supervision, authentication, wake, and routing behavior;
- artifact, screenshot, video, review, and proof workflows;
- webhooks, delivery guarantees, idempotency, rate limits, and signature
  verification responsibilities;
- schedules, workload identity, secrets, mobile access, multiplayer, and Puck;
- any current feature that binds verification evidence to exact source or
  environment state and invalidates it after later changes.

Do not infer architecture from marketing language when documentation provides
a narrower claim. Distinguish product behavior from speculation about Amp's
private implementation.

### Pass Two: OpenCode, From Docs and Pinned Source

Research OpenCode V1 and V2 separately. Fern currently pins V1 `1.18.16`, while
V2 is a different and potentially changing product surface.

For V1, verify:

- `serve`, `attach`, authentication, SSE, persistence, and web behavior;
- plugin context and event capabilities;
- whether plugins can influence session placement;
- known attach and remote-server limitations relevant to fern.

For V2, use the official V2 documentation as the primary source. Verify:

- background service discovery and lifecycle;
- client and HTTP API behavior;
- authentication for explicit remote servers;
- location, session move, worktree, PTY, shell, and filesystem APIs;
- the V2 plugin capability model;
- whether a documented third-party workspace provider, execution target,
  runner, adaptor, or sandbox registration API exists;
- migration implications for fern.

Then inspect OpenCode source pinned to an exact commit. Investigate internal or
experimental workspace, location, routing, adaptor, session migration, Docker,
remote-server, and cloud-sandbox work. Cite files and commits.

Search relevant issues and pull requests, including requests for workspace
provider abstractions or runner frameworks. A closed issue, source comment, or
maintainer post is roadmap evidence, not a stable API contract.

Conclude which integration surfaces are:

| Classification | Meaning |
|---|---|
| Supported | Publicly documented for the applicable version |
| Beta | Publicly documented but explicitly unstable |
| Experimental | Shipped or visible but not a dependable external contract |
| Internal | Source implementation not offered to third parties |
| Planned | Stated direction without shipped support |
| Speculative | Inference without sufficient evidence |

### Pass Three: Remote Coding Market and Product Economics

Map the current market for remote, background, hosted, and self-hosted coding
agents. Include at least:

- Amp Orbs;
- OpenCode's own remote workspace direction;
- Cursor background agents;
- GitHub Copilot coding agent;
- OpenAI Codex cloud or its current successor;
- Google Jules;
- Devin / Cognition;
- Factory;
- E2B;
- Daytona;
- Modal;
- Cloudflare Sandbox;
- Vercel Sandbox;
- Coder;
- Ona / Gitpod;
- Northflank;
- relevant self-hosted OpenCode projects such as Netclode or current
  equivalents;
- any newer competitor that materially changes the analysis.

For each relevant product, determine:

- unit of work: session, thread, task, workspace, VM, container, or repository;
- environment isolation model;
- pause, stop, snapshot, resume, and billing behavior;
- client surfaces: CLI, web, desktop, phone, Slack, API;
- preview and artifact UX;
- setup and environment contract;
- proof and verification workflow;
- self-hosting and BYOC availability;
- pricing and meaningful minimum spend;
- security and enterprise controls;
- openness and extensibility;
- likely overlap with fern.

Cross-check vendor pricing and BYOC claims against current first-party pricing
or documentation. Date every volatile figure. Treat vendor benchmarks and
analyst estimates as claims, not independent facts.

Answer whether there is a credible underserved segment for:

> A private, single-tenant, self-hosted, wake-on-request OpenCode workspace that
> runs on infrastructure the user already owns.

Estimate whether this segment supports:

- a polished open-source portfolio project;
- a useful community project;
- paid support or sponsorship;
- a small sustainable business;
- an enterprise open-core company;
- a managed multi-tenant cloud.

Do not treat theoretical willingness to pay as market validation.

### Pass Four: Product Design and User Experience

Evaluate fern as a product, not only as infrastructure.

Use principles from excellent developer tools and applied-AI systems:

- progressive disclosure;
- sensible defaults;
- deterministic operations;
- legible system state;
- direct manipulation where appropriate;
- low-cost recovery;
- interruption and return-to-work design;
- evidence before trust;
- secure defaults;
- explicit ownership boundaries;
- graceful failure;
- minimal setup;
- fast time to first success;
- compatibility with phone-sized and low-attention interactions;
- avoiding model-visible tools for deterministic control-plane actions;
- avoiding features that add navigation without adding leverage.

Analyze the complete journey:

```text
discover fern
  -> install it
  -> connect a repository
  -> configure credentials
  -> start the workspace
  -> attach locally or remotely
  -> send a task
  -> disconnect
  -> return from a phone
  -> inspect state and evidence
  -> recover from failure
  -> let the workspace sleep
  -> upgrade or remove fern
```

For every step, identify:

- the user's intent;
- required knowledge;
- current friction;
- hidden or ambiguous state;
- failure modes;
- security risks;
- the smallest useful product improvement;
- what should remain delegated to OpenCode, Docker, Tailscale, or Git.

Evaluate whether these proposed features form a coherent product:

- `fern attach` and an explicit remote URL;
- `.fern/setup`;
- `.fern/resume`;
- `fern doctor --json`;
- one wake-aware private preview service;
- a bounded artifact inbox;
- concise return-to-work status;
- state-bound verification receipts with freshness;
- signed GitHub event ingestion.

For each feature, answer:

- Which observed user problem does it solve?
- Is the problem already solved by OpenCode, Tailscale, Docker, or Amp?
- Does fern uniquely have the state or authority needed to solve it?
- What is the minimum coherent version?
- What complexity and attack surface does it add?
- What evidence would justify building the next version?
- What should be explicitly excluded?

Develop at least three alternative product concepts, not merely three feature
lists. One must be the deliberately minimal option. Examples include:

- fern as only a lifecycle and attach wrapper;
- fern as a wake-ready, inspectable self-hosted workspace;
- fern as an evidence-bound workspace that independently verifies freshness.

Assess each concept for clarity, distinctiveness, usefulness, implementation
cost, operational burden, and resilience to OpenCode shipping remote workspaces.

### Pass Five: Product Taste and Developer Experience References

Amp is not the only product-design reference. Study a small set of companies
and individuals known for strong developer experience, clear product opinions,
or unusually effective infrastructure abstractions. Do not copy their visual
style or repeat reputation-based praise. Identify concrete interaction and
system-design choices that can be observed in current products.

Research at least these references:

#### Tailscale

Study how Tailscale turns difficult networking and identity configuration into
a small number of legible actions. Examine:

- identity-based access instead of exposing network topology;
- installation and first-connection flow;
- device naming and discovery;
- secure defaults;
- status, diagnostics, and recovery;
- Tailscale Serve as a private publication primitive;
- how much implementation detail is hidden versus inspectable.

Ask what fern can learn about making wake, attachment, identity, and private
access feel unsurprising.

#### Vercel

Study Vercel's deployment workflow rather than its marketing aesthetic:

- zero-configuration first success;
- repository connection;
- preview URLs as a result artifact;
- production versus preview environments;
- CLI-to-web continuity;
- logs, build failures, and remediation;
- defaults that work while preserving an escape hatch;
- where convenience creates platform coupling or hidden complexity.

Ask whether fern can return a private, wake-aware result as directly as Vercel
returns a preview deployment, without becoming a hosting platform.

#### Cloudflare

Study how Cloudflare exposes globally distributed infrastructure through
developer primitives such as Wrangler, Workers, Durable Objects, Tunnels, and
Sandbox. Examine:

- local-to-remote workflow continuity;
- composable primitives;
- configuration and secret handling;
- observability and debugging;
- secure publication;
- product sprawl and conceptual overload as counterexamples;
- how new primitives remain understandable when the platform becomes broad.

Ask which control-plane facts fern should expose and which infrastructure
details should remain behind one deterministic operation.

#### Convex

Study Convex as an opinionated developer-platform and backend-DX reference:

- the path from installation to the first working application;
- local development that stays connected to deployed state;
- generated types and end-to-end type safety;
- colocating schema, functions, queries, mutations, and application logic;
- reactive behavior that removes explicit synchronization work;
- dashboard, logs, data inspection, and function observability;
- deployment and environment separation;
- errors that teach the product model rather than expose implementation noise;
- strong constraints that eliminate configuration choices;
- where the abstraction becomes limiting or creates platform dependence.

Ask what fern can learn from Convex about making durable state, lifecycle
transitions, diagnostics, and remote execution feel like one coherent model
instead of a collection of Docker commands.

Also ask which Convex lessons should not transfer. Fern should not build a
dashboard, hosted database, reactive application framework, or proprietary
control plane merely to imitate Convex's polish.

#### Theo Browne and the T3 Ecosystem

Use Theo/T3 as a product-opinion and communication reference, not as proof of
large-scale infrastructure reliability. Evaluate:

- strong defaults for a clearly named audience;
- willingness to exclude use cases;
- transparent tradeoff communication;
- reducing choice during first setup;
- teaching users the underlying model;
- community feedback loops;
- the risks of personality-led prioritization, trend sensitivity, and
  anecdotal validation.

Ask whether fern can state an equally clear opinion about who it is for and who
should use plain OpenCode instead.

For every reference, produce this table:

| Reference | Concrete design choice | User problem solved | What fern should borrow | What fern should avoid | Evidence |
|---|---|---|---|---|---|

Then synthesize no more than seven product principles for fern. Principles must
be operational enough to reject a feature proposal. Avoid generic statements
such as "make it simple" or "focus on users."

At minimum, test these candidate principles:

1. One command should produce one inspectable outcome.
2. Hide routine orchestration, never hide failure state.
3. Use secure private defaults and make public exposure explicit.
4. Preserve OpenCode's native interface instead of building a competing UI.
5. Every green status must identify the state and generation it describes.
6. The first successful workflow should require no control-plane vocabulary.
7. Advanced configuration must be earned by a reproduced need.

### Pass Six: Independent Product And Engineering Lenses

Run seven independent reviews, one for each person below. Use each person's
publicly demonstrated principles and body of work as an analytical lens. Do not
impersonate them, invent quotations, imply endorsement, or substitute reputation
for evidence. Each review must inspect fern's actual code and product journey,
reach its own conclusion before reading the other reviews, cite repository
evidence, and state what its lens would build, remove, measure, and refuse.

| Lens | Perspective to apply | Primary challenge to fern |
|---|---|---|
| Rob Pike | Conceptual simplicity, concurrency discipline, and Unix-like composition | Is fern genuinely one coherent program, or an accumulating control-plane framework? Could half the states and abstractions be deleted? |
| Jony Ive | Reduction, hierarchy, and the felt quality of transitions | Does wake and sleep feel calm and intentional, or does fern expose machinery because engineers find it interesting? |
| Mitchell Hashimoto | Infrastructure-tool packaging and progression from CLI to a durable product | Can installation, configuration, state, diagnostics, and extension seams form one teachable model? |
| DHH | Opinionated self-hosting for a sharply defined independent user | Can fern proudly reject cloud and platform complexity and be operated by one person for years? |
| Bret Victor | Making invisible system state visible and explorable | Can users understand lifecycle transitions causally rather than reading snapshots and logs? |
| John Carmack | Empirical performance, direct implementation, and intolerance of unsupported abstractions | Are wake latency, memory savings, and failure behavior measured, or is the roadmap mostly conceptual architecture? |
| Patrick McKenzie | Positioning, distribution, and economic honesty | Who searches for this, why would they install it today, and is the pain commercially meaningful? |

For every lens, return:

- a one-sentence verdict;
- the strongest aspect of fern under that lens;
- the most serious challenge;
- concrete repository evidence with file and line references;
- what to keep, remove, and defer;
- one experiment or user test that could falsify the recommendation;
- the smallest coherent fern product that survives the critique.

After the independent reviews, synthesize:

- points on which at least five lenses agree;
- genuine disagreements and the assumptions causing them;
- recommendations that are robust across lenses;
- recommendations that depend on taste rather than evidence;
- one final scope boundary stated as what fern is and what it refuses to become.

## Grab Role Cross-Check

Find the current first-party job description for the relevant Grab Backend AI
or GrabGPT AI Gateway position. If the original role is no longer available,
say so and use an archived copy or the nearest current first-party role with
clear qualification.

Research Grab's current first-party engineering material on:

- GrabGPT AI Gateway;
- agent platforms;
- Palana or its current successor;
- model routing;
- streaming;
- cancellation;
- authentication and authorization;
- quotas and rate limiting;
- observability;
- cost attribution;
- governance;
- deployment and reliability.

Build a strict evidence matrix:

| Job requirement | Fern evidence today | Evidence after roadmap | Still missing | Honest interview framing |
|---|---|---|---|---|

Do not claim that fern is an LLM gateway. Do not call portfolio code production
experience unless there is evidence it was deployed and operated under real
usage. Distinguish:

- directly demonstrated skill;
- transferable systems skill;
- design-only knowledge;
- missing experience.

Determine which two weeks of work produce the highest interview signal. Compare
feature work against CI, deployment, dogfooding, measurement, documentation,
release engineering, and a short demo.

## Claims That Must Be Challenged

Attempt to falsify each of these claims:

1. Fern is meaningfully better than plain `opencode serve` plus Tailscale.
2. Automatic container stop/wake creates enough value without stopping the host
   VM or eliminating infrastructure cost.
3. `fern attach` is sufficiently seamless for local, tailnet, and phone use.
4. Setup/resume hooks are more valuable than previews or notifications.
5. State-bound verification is not already fully solved by Amp, Cursor,
   ProofShot, agent-receipts, AI Integrity Receipts, or another product.
6. Fern has independent lifecycle state that makes verification freshness more
   trustworthy than an agent-authored report.
7. OpenCode's roadmap does not make fern redundant.
8. A one-workspace limitation is a useful focus rather than a fatal product
   limitation.
9. A self-hosted privacy-oriented user segment exists and has this exact pain.
10. The proposed two-week roadmap is feasible for one developer with a
    full-time job.
11. Building hooks and diagnostics produces more value than finishing CI,
    deployment, measurement, and a demo.
12. Fern is a stronger Grab interview artifact than a smaller project directly
    implementing model routing, quotas, or token accounting.

For each claim, return one of:

- supported;
- partly supported;
- unsupported;
- contradicted;
- unknown.

Include the strongest evidence against the conclusion you ultimately recommend.

## Evidence Standards

Use this source priority:

1. current first-party documentation;
2. versioned API references and release notes;
3. source code pinned to exact commits;
4. first-party engineering blogs and public talks;
5. issue and pull-request discussions;
6. reproducible local experiments;
7. credible third-party analysis;
8. social posts and community reports.

Every important factual claim must have a citation. Link to exact documentation
sections, source files, commits, issues, pricing pages, or job descriptions when
possible.

Apply these rules:

- Separate observed behavior from inferred architecture.
- Separate current capability from announced direction.
- Separate V1 and V2 OpenCode behavior.
- Date pricing, product, and roadmap claims.
- Report conflicting sources rather than silently choosing one.
- Do not infer absence from one failed keyword search.
- Search competitors and documentation using multiple synonymous terms.
- Phrase negative findings as "not found in reviewed public documentation,"
  not proof that a private feature does not exist.
- Verify important findings through at least two independent searches or one
  first-party source plus pinned source inspection.
- Mark vendor claims and adoption numbers as self-reported.
- Do not use DeepWiki or generated summaries as final authority when source is
  available.

## Required Experiments

If repository and runtime access are available, perform or specify reproducible
experiments for:

- plain remote OpenCode versus fern startup and reconnect friction;
- authenticated `fern attach` behavior;
- a paused workspace waking from an ordinary phone HTTP request;
- at least ten authenticated wake measurements;
- memory before and after container stop;
- persisted OpenCode session recovery after stop and recreation;
- behavior when the client disconnects during and after a model turn;
- failure classification after external exit or OOM;
- current CI commands from a clean checkout;
- whether OpenCode V2 can replace the V1 attach path without private APIs.

Do not fabricate experiment results. If an experiment cannot be run, provide an
exact procedure, expected observations, and what decision each possible result
would change.

## Required Deliverables

Produce one report with the following structure.

### 1. Executive Decision

State in no more than ten bullets:

- what fern is today;
- whether it solves a real problem;
- whether it should remain an OSS portfolio project, become a community tool,
  or be pursued as a business;
- the recommended product concept;
- the next two weeks of work;
- the most important thing not to build.

### 2. Corrections To Existing Research

List every material factual error, stale statement, unsupported inference, or
missing caveat in `AMP_PRODUCT_RESEARCH.md` and `ROADMAP.md`. Include file and
line references.

### 3. Amp Cross-Check

Provide a current capability map and explain what fern can learn without
copying Amp's company-sized control plane.

### 4. OpenCode Integration Assessment

Provide a V1/V2 matrix, classify each integration surface, and recommend one of:

- keep the V1 attach wrapper;
- migrate to a documented V2 service/client seam;
- contribute an upstream adaptor;
- maintain a fork;
- stop investing because first-party functionality supersedes fern.

### 5. Market Map

Compare the most relevant competitors. Avoid a feature table so wide that it
obscures the decision. Group products by customer and operating model.

### 6. User Journey And Product Critique

Identify the five highest-friction moments, the strongest current moment, and
the smallest coherent end-to-end experience worth shipping.

Include a product-taste scorecard comparing fern with Amp, Tailscale, Vercel,
Cloudflare, Convex, and the T3 design lens. Score only dimensions supported by
concrete observations, not brand reputation.

### 7. Product Concepts

Compare at least three distinct product concepts using a weighted decision
matrix. Explain the weights.

### 8. Differentiation Test

Evaluate the strongest proposed differentiator, especially evidence-bound
workspace freshness. State the exact novelty claim fern can defend and the
claims it cannot defend.

### 9. Grab Evidence Matrix

Map implemented fern behavior to the current role and clearly list remaining
gaps.

### 10. Revised Two-Week Roadmap

Return a plan feasible for one developer with a full-time job. Each item must
include:

- user outcome;
- implementation boundary;
- verification method;
- estimated focused hours;
- dependency;
- stop condition;
- what is explicitly deferred.

Rank all work as `P0`, `P1`, `P2`, or `Do not build`.

### 11. Decision Triggers

Define measurable evidence that would cause fern to:

- continue with the current direction;
- pivot toward previews;
- pivot toward verification;
- integrate with OpenCode V2;
- become maintenance-only;
- be abandoned;
- be considered for commercialization.

### 12. Source Appendix

Group sources as first-party documentation, pinned source, issues and pull
requests, market/pricing, job-role material, and third-party analysis. Include
access dates.

### 13. Fern Product Principles

Return no more than seven specific principles derived from the product-taste
research. For each principle, include one roadmap choice it supports and one
feature it rules out.

### 14. Independent Lens Review

Include the seven independent reviews from Pass Six, clearly labeled as
analytical lenses rather than simulated statements by the named people. End the
section with the cross-lens synthesis, disagreements, falsification tests, and
the final scope boundary. Do not flatten materially different recommendations
into artificial consensus.

## Quality Bar

The report should read like a high-quality architecture decision record and
product strategy review, not a market-research content piece.

Be direct. Prefer a smaller validated product over an expansive speculative
platform. Reward restraint, interoperability, reversibility, and evidence.
Penalize duplicated platform functionality, unstable dependencies, hidden
operational burden, and features selected mainly to match job-description
keywords.

The final recommendation must answer:

> If fern had to earn the right to exist through one excellent end-to-end user
> experience, what exactly would that experience be, why is fern the correct
> layer to provide it, and what evidence would prove users value it?
