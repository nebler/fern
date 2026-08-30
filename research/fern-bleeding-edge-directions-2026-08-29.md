# Fern Beyond Remote Agents: Bleeding-Edge Directions

Research date: 2026-08-29

Repository baseline: `harden/production-readiness` at
`ab945b5a00db3a310b3fcc30fe8bc99669598b6f`

Status: frontier research and strategic hypothesis. This document is not an
implementation claim or committed roadmap. Code and maintained documents under
`docs/` remain authoritative.

## Scope

This research asks:

> What could Fern build on a small scale that points toward the next generation
> of coding-agent infrastructure, creates an immediate "wow" demonstration,
> improves actual software creation, and combines cloud execution, distributed
> systems, and compiler/build-system ideas?

Explicit non-goals:

- another agent dashboard;
- generic remote chat or phone approval;
- verification or evidence as the product pitch;
- benchmark-as-a-service;
- AST context selection or repository summarization;
- a generic fleet scheduler;
- a generic sandbox or microVM API;
- a universal model gateway;
- another durable workflow runtime;
- a broad multi-agent chat framework;
- speculative infrastructure with no useful software-building loop.

Tests, exact identities, and effect controls still matter as implementation
mechanisms. They are not the proposed user value.

## Decisive Recommendation

The strongest immediate frontier bet is:

> **Fern Futures: branch a software project into several live implementation
> futures from one warm point, share their common build computation, send each
> action to the right machine, and promote the future the user wants.**

The underlying technical primitive is **Branchable Build Memory**:

- a content-addressed source and artifact graph;
- immutable parent/child branch lineage;
- typed build, test, run, and preview actions;
- exact action coalescing and cache reuse across candidates;
- safe reuse of compiler and language-server disk state where supported;
- optional filesystem or memory snapshots as accelerators;
- branch-local outward effects held until one future is promoted;
- one selected result handed into Fern's existing result/publication path.

The pitch is not "Fern runs three agents." Parallel agents are becoming normal.
The pitch is:

> **From one prompt: three live implementations, one warm setup, one shared
> compiler graph, and one choice of which future becomes real.**

The strongest longer-term alternative is a **Distributed Migration Compiler**:
lower one high-level API/schema/framework migration into compatibility-ordered
changes across packages, repositories, and deployment phases. It is potentially
more valuable to enterprises but farther from Fern and less immediately visual.

## Why This Direction Now

Several independent trends are converging.

### Parallel Generation Is Becoming Commodity

OpenAI Codex, Cursor, Replit, Claude, Amp, Warp, GitHub, Jules, and Devin expose
parallel or isolated agent work. Generic fan-out, worktrees, subagents, and run
dashboards are no longer a strong product position.

The remaining hard problems are:

- choosing where to branch;
- avoiding repeated setup and compilation across branches;
- giving branches genuinely different continuations rather than correlated
  copies;
- controlling cost and review load;
- representing non-repository state honestly;
- preventing losing branches from creating external effects;
- turning one selected future into the normal development flow.

### Test-Time Compute Works, But Selection And Cost Lag

Research such as Large Language Monkeys, SWE-Search, CodeMonkeys, SWE-Replay,
Scaling Test-Time Compute for Agentic Coding, and AlphaEvolve shows that more
trajectories often increase the chance that a useful candidate exists.

The consistent limitations are:

- candidate selection recovers only part of the oracle gain;
- full search can be dramatically more expensive;
- repository tests are incomplete or flaky;
- correlated agents repeat the same mistake;
- restarting every candidate discards useful intermediate work;
- reviewing many complete branches can cost more than generating them.

Fern should therefore not begin with a swarm or MCTS engine. It should make two
or three branches cheap enough, different enough, and concrete enough for a
person to choose between them.

### Sandboxes Can Fork, But They Do Not Supply The Product

Morph, E2B, Daytona, AgentENV, forkd, Mitos, Firecracker, CRIU, and selected
Kubernetes Agent Sandbox backends can preserve or branch some combination of
filesystem and memory state.

Those substrates do not decide:

- which software state is a valid branchpoint;
- what compiler/build work can be reused;
- how branch-specific identities and credentials are created;
- how to compare live implementations;
- which outward effects must remain buffered;
- how one branch is promoted into the repository and delivery flow.

Memory snapshots are optional accelerators. They must not become the portable
Fern contract.

### Build Systems Already Contain The Right Ideas

Bazel REAPI, Bazel Skyframe, Buck2 DICE, BuildBarn, BuildGrid, Nix, incremental
compilers, and language servers already demonstrate:

- Merkle input trees;
- content-addressed outputs;
- immutable action identities;
- dependency-driven invalidation;
- exact action-result reuse;
- persistent compiler workers;
- deferred materialization;
- platform-aware scheduling;
- incremental diagnostics.

They generally optimize one build graph or checkout. They do not expose a
user-facing branch lineage where several coding-agent futures inherit common
computation and diverge only where their source changes.

### Top Companies Are Moving Toward Compilers For Change

Public signals include:

- Cursor describes a swarm architecture as a probabilistic compiler that lowers
  a specification into work, propagates compiler failures through dependents,
  and required custom planning, review, conflict handling, and version control.
- Stripe AutoJDK continuously computes upgrade eligibility from the build graph,
  uses agents for repair, and performed large-scale language migrations.
- Anthropic's managed-agent architecture separates session, harness, and
  sandbox so those layers can be replaced independently.
- Anthropic's long-running-agent work favors fresh sessions, Git checkpoints,
  explicit progress state, and bounded shifts over immortal context.
- DeepMind AlphaEvolve combines generation, evolutionary search, and objective
  evaluators to discover production algorithm improvements.
- Replit and Codex expose isolated parallel project copies but not a public
  branchable compiler-state contract.
- Depot and Dagger are turning build/CI operations into typed, agent-callable
  actions over uncommitted work.

These sources do not prove private roadmaps. They make the likely frontier
visible: more inference-time branching, more replaceable execution, more typed
actions, and more build-state reuse.

## Lead Bet: Fern Futures

### User Job

When a change has several plausible implementations, do not make the user pick
an approach from prose or wait for one long agent attempt to fail.

From one running project state:

1. Create a safe branchpoint.
2. Try two or three materially different continuations.
3. Reuse shared setup, dependencies, compiler actions, and unaffected tests.
4. Run each implementation and show its actual behavior.
5. Let the user compare previews, performance, API shape, or code.
6. Promote one future and discard the rest.

This is especially compelling for:

- UI and interaction choices;
- performance implementations;
- bug fixes with competing diagnoses;
- API design alternatives;
- migration strategies;
- cross-platform application behavior;
- changes where explaining the desired result is harder than choosing it.

### Product Pitch

> **Fern lets you branch software futures. Ask for a change, explore several
> live implementations from the same warm project, and promote the one you want.**

This is not a benchmark. The alternatives are produced for the user's current
work and become directly usable software.

### Wow Demo

```text
phone or desktop: "Redesign checkout so confirmation feels instant"
                         |
                  exact branchpoint
               /          |          \
       optimistic UI   queued flow   inline receipt
            |               |              |
       live preview     live preview    live preview
       42 s build       28 s build      31 s build
               \          |          /
                   choose future B
                         |
                  one normal branch/PR
```

The screen should show a living branch graph and real previews, not three
terminal panes.

The technical reveal is a second view:

```text
shared setup and dependency work        100% reused
shared compiler/build actions            73% reused
branch-specific actions                  27% executed separately
candidate action requests                 4 coalesced
```

The memorable claim, if measured honestly, is:

> "Three agents explored three implementations, but Fern did not perform three
> complete cold builds."

### Architecture

```text
user intent
    |
agent/harness creates branch strategies
    |
Fern Branchpoint
    +-- exact source root and parent lineage
    +-- environment/toolchain identity
    +-- local-state capability declaration
    +-- outward-effect watermark
    |
Candidate Controller
    +-- candidate A
    +-- candidate B
    +-- candidate C
    |
Branchable Action Graph
    +-- source/artifact CAS
    +-- exact action cache
    +-- in-flight action coalescing
    +-- compiler/build adapters
    +-- diagnostic deltas
    +-- branch-wide test allocation
    |
Execution Providers
    +-- local disposable Docker
    +-- remote Linux action worker
    +-- optional Mac/Xcode worker
    +-- optional snapshot backend
    |
live previews and comparison
    |
explicit promotion of one immutable candidate
    |
existing Fern Result -> Verification -> Publication path
```

### Compiler Connection

Fern should treat build computation as a graph of immutable values and actions,
not as undifferentiated shell logs.

An action key approximates:

```text
hash(
  command and declared operation,
  normalized environment,
  platform and toolchain,
  Merkle input root,
  declared outputs,
  execution policy
)
```

Reuse has three honest levels:

| Reuse class | Meaning |
| --- | --- |
| Exact | Hermetic action output can be returned by digest without execution. |
| Validated incremental | A compiler or language server inherits disk state but revalidates dependencies. |
| Advisory | Test selection or diagnostic prioritization may save work but cannot establish a final result. |

Do not serialize arbitrary compiler RAM in the first version. Start with:

- shared Go module and build caches;
- exact action-result caching for selected hermetic commands;
- per-lineage `gopls`/compiler disk state where supported;
- changed-package and reverse-dependency tests;
- coalescing concurrent identical actions;
- one clean final build for the selected candidate.

### Branchpoint Contract

A portable branchpoint should contain:

```text
branchpoint ID and parent
exact Git base/tree plus content artifact
toolchain and environment digest
action-graph root
reusable cache lineage
selected transcript/trajectory summary
captured local services and data declarations
non-portable state: explicit list
outward effects: none, settled, or unresolved
```

It should classify restoration honestly:

- `memory_resumed`: compatible substrate restored process and memory state;
- `checkpoint_restarted`: files and durable state restored, processes recreated;
- `base_replayed`: repository and task recreated without runtime state.

The default portable path is `checkpoint_restarted`. Memory restoration is a
provider-specific optimization.

### Effects During Speculation

Parallel futures are only useful if losing branches do not leak into the world.
Fern needs a small effect boundary, not a general transaction system.

| Effect class | Speculative behavior |
| --- | --- |
| Branch-local reversible | Execute inside branch-local filesystem/database state. |
| Buffered external | Record intent and release only for the promoted branch. |
| Compensable external | Avoid initially; compensation is another fallible effect. |
| Irreversible external | Prohibit until the branch is promoted. |

The first implementation should cover only Fern's existing host-held GitHub App
publication. Candidate workers receive no direct GitHub mutation credential.
Do not begin with arbitrary shell interception or claims that all network effects
are transactional.

### What Fern Reuses

Fern already has useful pieces:

- global task, attempt, receipt, event, result, and publication identities;
- immutable base and result tuples;
- exact Git object collection and manifests;
- actor-bound idempotent receipts;
- cancellation fences;
- host-owned verification;
- write-ahead GitHub publication phases;
- exact remote reconciliation after ambiguous responses;
- constrained Docker provisioning and runtime attestation;
- strict archive extraction and content hashing.

### What Fern Must Add

Do not relax existing single-result constraints in place. Add a separate model:

```text
WorkGroup
  -> Branchpoint
  -> CandidateRun*
  -> Checkpoint*
  -> ActionRequest*
  -> Artifact*
  -> Promotion
```

Only the promoted candidate enters the existing production result pipeline.

New primitives:

- local filesystem content-addressed store;
- verified Git bundle export/import;
- source-root Merkle identity;
- immutable `ActionSpec` and `ActionResult`;
- action cache and in-flight request coalescing;
- branchpoint/candidate lineage;
- serial disposable local-Docker provider;
- typed outward-effect outbox for candidate publication;
- live preview descriptors;
- optional runner capability vectors for Linux/Mac placement.

### Smallest Honest MVP

Use one Go repository first, because Go exposes portable build caching, package
graphs, structured test output, and a native Fern implementation path.

Scope:

- one owner and repository;
- one local host;
- two candidates, initially serial and then concurrency two;
- one pinned batch harness;
- fresh disposable checkout per candidate;
- local filesystem CAS;
- verified Git bundle artifacts;
- named `go.build`, `go.test`, and `go.run` actions;
- shared Go module/build caches with exact lineage;
- action coalescing;
- one branch graph page;
- manual promotion;
- no arbitrary process snapshot;
- no automatic merge;
- no direct external credentials in candidates.

First demo task should have two visibly different correct implementations, not
one correct and one intentionally broken branch.

### Build Slices

#### F0: Portable Exact Artifacts

- Local filesystem CAS.
- `ArtifactDescriptor` with SHA-256, size, and media type.
- Git bundle exporter/importer.
- Re-prove base, result, tree, ancestry, and manifest after import.

This is the prerequisite for every disposable, remote, and branchable path.

#### F1: Typed Go Actions

- `ActionSpec` and `ActionResult`.
- Exact toolchain/environment/platform identity.
- Named Go build/test actions with bounded structured output.
- Exact action cache for selected commands.
- Concurrent identical-action coalescing.

#### F2: Two Semantic Futures

- Branchpoint and candidate records.
- One candidate at a time using a disposable provider.
- Exact checkpoint plus concise continuation input.
- Manual candidate promotion.
- Existing Fern result/publication pipeline for the winner.

#### F3: Shared Build Memory

- Run candidates concurrently.
- Shared read-only module/download cache.
- Lineage-aware Go build cache.
- Changed-package and reverse-dependency test selection.
- Diagnostic deltas against the common parent.
- Final clean build after promotion.

#### F4: Live Futures

- Start one configured application per candidate.
- Authenticated preview routes on separate origins.
- Show live thumbnails and basic runtime logs.
- Allow phone/desktop comparison and promotion.

#### F5: Heterogeneous Actions

- Add one outbound Linux or Mac action worker.
- Route by explicit platform and toolchain capabilities.
- Transfer only content-addressed inputs and typed artifacts.
- Keep the agent session on its original machine.

#### F6: Snapshot Acceleration

- Integrate one provider such as E2B or Morph.
- Snapshot only at declared quiescent branchpoints.
- Give every child fresh attempt, network, entropy, and credentials.
- Preserve semantic restart as the fallback contract.

### Success Metrics

The metrics must describe faster building, not evidence production:

- time from one request to first two runnable alternatives;
- time to switch between alternatives;
- duplicate CPU-seconds avoided across candidates;
- bytes materialized per candidate;
- exact action cache hits;
- coalesced action requests;
- percentage of shared versus divergent build graph;
- time from selecting a future to a normal branch/PR;
- percentage of future-selection tasks where the chosen candidate is actually
  kept rather than rewritten;
- repeat use for tasks with genuine design ambiguity.

### Kill Criteria

Kill or narrow Fern Futures if:

- two good alternatives cost approximately two complete cold runs;
- action wrapping requires converting repositories to Bazel/Nix or writing a
  build system before any value appears;
- users consistently ask for one good solution rather than alternatives;
- comparing branches takes longer than asking one agent for a correction;
- candidate diversity is mostly cosmetic;
- compiler/build reuse saves less than environment snapshots or ordinary cache
  mounts already provide;
- external-effect restrictions make realistic tasks unusable;
- existing Cursor/Replit/Codex candidate workflows absorb the same branchpoint
  and promotion experience;
- Fern cannot demonstrate a visually compelling result within six weeks after
  the artifact and disposable-run prerequisites exist.

## Second Bet: Distributed Migration Compiler

### Thesis

> Compile one high-level software migration into compatibility-ordered changes
> across packages, repositories, schemas, clients, and deployment phases.

Example:

```text
"rename customer_id to account_id without downtime"
  -> add account_id and dual-write
  -> deploy producer
  -> update consumers to dual-read
  -> backfill
  -> flip reads
  -> remove customer_id
```

Each phase has explicit compatibility preconditions and produces an ordered
change. Agents generate local implementations; Fern owns the temporal graph.

### Why It Is Frontier

Strong models increasingly solve one bounded patch. Large migrations remain
difficult because correctness exists across time and across independently
deployed components.

Public signals:

- Stripe AutoJDK computes upgrade eligibility from a live build graph and uses
  agents to repair migration failures at large scale.
- Cursor's swarm work explicitly describes specification lowering and
  compiler-driven propagation through dependencies.
- OpenRewrite, Moderne, Sourcegraph Batch Changes, GitHub modernization, and AWS
  Transform validate demand but focus mostly on codemods, framework upgrades, or
  batches rather than application-specific temporal compatibility phases.

### Fern-Sized Demo

Use three small Go services and one protobuf or exported-API migration:

1. Parse module and dependency information with `go list -deps -json`.
2. Load an explicit `migration.yaml` describing compatibility phases.
3. Generate phase-specific tasks and acceptance conditions.
4. Run each phase serially at first.
5. Produce an ordered local branch stack or draft PR stack.
6. Refuse destructive cleanup until every dependent has crossed the required
   compatibility boundary.

### New Primitives

- task and phase DAG;
- multiple module/repository identities;
- compatibility preconditions;
- target-specific checkpoints and results;
- ordered branch/PR relationships;
- external deployment-state observations;
- migration progress and rollback/forward policy.

### Risks

- migration vocabularies may be too domain-specific;
- existing deterministic codemods may solve most of the work;
- discovering deployment and ownership graphs is expensive;
- agents can produce individually correct patches that violate rollout order;
- multi-repository publication and review are operationally heavy;
- the first useful customer may require enterprise integrations before product
  value is visible.

### Kill Criteria

- three real migrations cannot share a small phase vocabulary;
- authoring the migration manifest takes as long as manual coordination;
- deterministic tools complete more than 90 percent of the target workload;
- users need repository understanding rather than temporal coordination;
- no design partner owns repeated cross-service migrations.

## Third Bet: Heterogeneous Action Linker

### Thesis

> Keep one agent session in place but compile each build, test, device, or
> private-network action for the machine capable of executing it.

Example:

```text
node.test           -> cloud Linux
android.build       -> cloud Linux
ios.build           -> customer Mac mini
ios.simulator_test  -> customer Mac mini
device.install      -> attached iPhone
browser.matrix      -> external browser provider
```

This is not remote shell or whole-session placement. It is a typed action and
artifact fabric.

### Best Initial Niche

Cross-platform mobile development is the most coherent personal wedge:

- cloud agents are usually Linux-centric;
- Xcode, signing, simulators, and Keychain require a Mac;
- Android/JS work can use cheaper Linux capacity;
- a real phone or simulator provides immediate user-visible results;
- the same control surface can be used from a phone.

### Wow Demo

One agent edits an Expo/React Native app, then:

1. Sends JS tests and Android build to Linux.
2. Sends Xcode build to a Mac worker.
3. Installs the iOS artifact in a simulator or attached device.
4. Runs one Maestro/XCTest flow.
5. Returns a live app preview, screenshots, video, crash logs, and typed
   diagnostics into the same agent turn.
6. Repeats after an edit while transferring only changed content-addressed
   chunks.

### Existing Competition

Buildkite, GitHub Actions, Dagger, Depot, Bazel RBE, Xcode Cloud, EAS,
Codemagic, AWS Device Farm, Firebase Test Lab, BrowserStack, Cursor pools,
Claude self-hosted runners, and Coder already cover individual parts.

The narrower opening is per-action placement across several specialist machines
inside one live agent loop. Most systems place a whole job or session on one
runner.

### Kill Criteria

- the workflow is equivalent to invoking GitHub Actions or Dagger through MCP;
- fewer than 30 percent of target tasks need more than one execution locality;
- warm action latency exceeds roughly one minute for an interactive loop;
- typed result maintenance across toolchain versions dominates development;
- Mac/device support becomes a high-cost hosted hardware business rather than a
  customer-owned-worker product.

## Fourth Bet: Agent Continuation Compiler

### Thesis

> Translate unfinished coding work into a portable continuation that another
> model, harness, or machine can consume.

This is semantic continuation, not process migration.

```text
original task and constraints
exact Git checkpoint
accepted and rejected approaches
files inspected and why
tests run and outcomes
unresolved questions
known, settled, and ambiguous external effects
source harness event lineage
target-specific bootstrap prompt and artifacts
```

### Wow Demo

Start in OpenCode on a Mac, interrupt power after several edits, then continue
with pinned Codex on Linux without repeating the user's explanation. Fern shows
that the target resumed from an exact checkpoint and knew which failed approach
not to repeat.

### Why It Is Interesting

Anthropic's managed architecture separates session, harness, and sandbox. ACP,
A2A, and MCP Tasks standardize useful pieces of interaction and long-running
work, but no reviewed protocol defines cross-harness continuation semantics.

### Kill Criteria

Compare against passing only the original prompt and current diff. Stop if a
structured continuation does not materially reduce rediscovery or improve
success on at least three of five real interrupted tasks.

## Fifth Bet: Speculative Effect Runtime

### Thesis

> Let branches mutate local state freely while outward effects remain typed and
> unreleased until one branch is promoted.

Atomix, Cordon, ETAS, AgentRewind, LACUNA, Metis, SIGIL, Sandlock, and TClone
indicate that task-level effects, reversible local state, and transactional agent
execution are active research directions. These are mostly preprints or
prototypes, not settled standards.

Fern should not market this as the product. It is enabling infrastructure for
Futures and migrations.

The Fern-sized implementation is one typed GitHub effect envelope around the
existing App broker:

```text
requested -> staged -> releasing -> committed
                           |
                           +-> ambiguous
```

Losing candidate outboxes are discarded before any GitHub call. The promoted
candidate releases one deterministic operation, and lost responses use Fern's
existing exact-read reconciliation.

Do not begin with arbitrary MCP wrapping, shell interception, distributed ACID,
or claims that closing a PR undoes every observation of it.

## Counterintuitive Bet: Disposable Shift-Work Agents

Instead of preserving an increasingly long context, Fern could deliberately end
agent sessions after one compile-ready milestone.

```text
frontier planner maintains goal tree
  -> fresh bounded worker lands milestone 1
  -> fresh bounded worker lands milestone 2
  -> fresh bounded worker lands milestone 3
  -> final integrated result
```

This follows Anthropic's long-running harness findings and avoids some context
drift. It is cheaper to test than parallel futures and may become a component of
the migration compiler.

Kill it if reorientation consumes more than 25 percent of each worker's turns or
if three real multi-hour tasks are no better than one continuous agent.

## Directions To Reject

| Direction | Reason |
| --- | --- |
| Fern Swarm or Fleet as the product | Parallel runners, leases, worktrees, and dashboards are occupied; demand for owner-operated fleets is unproven. |
| Generic best-of-N | Easy to copy and expensive; selection and shared computation are the real problems. |
| Full MCTS engine | High cost, weak value calibration, and little immediate user value. |
| Generic remote build cache | Bazel, Buck2, Nix, BuildBarn, BuildGrid, BuildBuddy, Depot, and others already own it. |
| Faster VM snapshots | Infrastructure vendors already compete on this dimension. |
| Transparent process migration | Cross-provider, cross-kernel, and Mac/Linux state is not portable; external effects remain unresolved. |
| Semantic/AST merge engine | The user explicitly rejected AST context work, and semantic VCS is a separate deep product. Integrate one if needed. |
| Generic heterogeneous remote shell | Buildkite, Actions, SSH, Coder, and agent pools already solve it. Typed actions are required. |
| Generic agent transaction standard | Research already names the space; one narrow effect adapter is more credible than a paper protocol. |
| Benchmark or evaluation platform | Does not directly help the user build the current software change. |
| Another coding UI | OpenCode, T3, Codex, Claude, Cursor, and GitHub already own richer surfaces. |

## Ranked Decision

| Rank | Direction | Wow | User value | Fern leverage | Technical frontier | Build risk | Decision |
| ---: | --- | ---: | ---: | ---: | ---: | ---: | --- |
| 1 | Fern Futures + Branchable Build Memory | 5 | 4 | 4 | 5 | 4 | **Lead prototype** |
| 2 | Distributed Migration Compiler | 4 | 5 for target teams | 3 | 5 | 5 | **Long-term product candidate** |
| 3 | Heterogeneous Action Linker | 5 for mobile/device work | 4 | 3 | 4 | 4 | **Strong vertical after action IR** |
| 4 | Agent Continuation Compiler | 4 | 3 | 5 | 4 | 3 | **Small enabling experiment** |
| 5 | Speculative Effect Runtime | 3 | Indirect | 5 | 5 | 4 | **Supporting primitive only** |
| 6 | Disposable Shift-Work Agents | 3 | 3 | 3 | 4 | 2 | **Cheap counter-test** |

Scores are strategic judgments, not market measurements.

### Fern Futures Detailed Scorecard

Scores use a 1-10 scale, where 10 is strongest. `Implementation difficulty`
and `scope risk` are inverted risk measures: a high score means difficult or
risky, not favorable.

| Dimension | Score | Reason |
| --- | ---: | --- |
| Visual wow factor | **10** | A running project visibly splitting into several usable futures is immediately understandable. |
| Twitter/X potential | **9** | "Three agents, but only 1.4 builds" is a strong hook if backed by real measurements. |
| Hacker News potential | **8** | Branch lineage, content addressing, build graphs, compiler state, and honest cost results fit the audience. |
| General CV value | **9** | Demonstrates substantially more systems depth than a thin LLM application. |
| Grab role relevance | **7** | Strong capacity, scheduling, identity, and streaming-adjacent work, but less direct than Gateway. |
| Distributed-systems depth | **9** | CAS, request coalescing, lineage, leases, cancellation, artifact transfer, and garbage collection. |
| Compiler/build-system depth | **9** | Action graphs, invalidation, incremental state, toolchain identity, and remote execution. |
| Go learning value | **9** | Requires storage, hashing, schedulers, concurrency, process control, streaming, and APIs. |
| Cloud/Kubernetes relevance | **7** | Strong later extension, but Kubernetes is unnecessary for the first useful prototype. |
| Technical novelty | **8** | The primitives exist separately; the user-facing branchable-build composition remains uncommon. |
| Immediate personal usefulness | **6** | Valuable for ambiguous changes, but excessive for routine fixes and dependency updates. |
| Small-team usefulness | **7** | Potentially useful for UI, API, architecture, migration, and performance decisions. |
| Enterprise usefulness | **6** | Interesting, although migration coordination and governed customer-cloud execution may be clearer enterprise jobs. |
| Six-week prototype feasibility | **5** | Plausible only with one repository, language, action vocabulary, and two candidates. |
| Production feasibility | **3** | Multi-language correctness, cache safety, previews, security, cloud operation, and UX are substantial. |
| Defensibility | **5** | The integration is difficult, but major agent vendors could add a comparable experience. |
| Monetization clarity | **4** | The capability is compelling, but buyer, frequency, and pricing remain unclear. |
| Implementation difficulty | **9** | The complete vision crosses storage, runtimes, builds, previews, agents, and effect boundaries. |
| Scope risk | **9** | It can easily become a weak build system, scheduler, sandbox platform, or parallel-agent dashboard. |
| Immediate roadmap priority | **4** | Current Fern deployment, acceptance, and Gateway work should not be displaced. |
| Overall portfolio value | **9** | It is the strongest standout technical demonstration among the researched directions. |
| Overall product confidence | **6** | Worth a narrow user test, not a full product commitment. |

The social hook must use measured values. `1.4 builds` is an example target, not
a current Fern result.

### Comparative Scorecard

Scores remain strategic judgments. `Useful` considers the likely target user,
not universal demand.

| Direction | Useful | Grab | Wow/viral | Novelty | Feasible | Product | Technical depth |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| **Fern Futures** | 7 | 7 | **10** | **8** | 5 | 6 | **10** |
| **Distributed Migration Compiler** | 8 | 7 | 8 | **9** | 3 | **8** | **10** |
| **Heterogeneous Action Linker** | **8** | 7 | 9 | 7 | 4 | 7 | 9 |
| **Narrow Fern Gateway** | 6 | **10** | 5 | 3 | **8** | 4 | 8 |
| **Exact Change Handoff** | 7 | 6 | 5 | 7 | **9** | 5 | 8 |
| **Attention/Action Plane** | 7 | 7 | 5 | 5 | 6 | 6 | 7 |
| **Outbound Runner Demo** | 5 | 7 | 7 | 3 | 5 | 3 | 8 |
| **Fern Labs** | 4 | 7 | 4 | 5 | 4 | 4 | 8 |

### Ranking By Goal

#### Best For The Grab Application

1. **Narrow Fern Gateway**
2. **Fern Futures**
3. **Outbound runner demonstration**
4. **Distributed Migration Compiler**
5. **Exact Change Handoff**

Gateway wins because the live role directly names Go/Python reverse proxies,
SSE, provider translation, fallback, Redis capacity isolation, PostgreSQL,
usage/cost accounting, and workload identity. Futures is strong secondary
evidence for distributed scheduling, shared capacity, and backend engineering.

#### Best For A Memorable CV

1. **Fern Futures**
2. **Distributed Migration Compiler**
3. **Narrow Fern Gateway**
4. **Heterogeneous Action Linker**
5. **Exact Change Handoff**

Gateway has closer keyword alignment. Futures and the Migration Compiler are
more likely to generate a distinctive systems-design interview.

#### Best For Twitter/X Or A Visual Launch

1. **Fern Futures**
2. **Heterogeneous Action Linker**
3. **Distributed Migration Compiler**
4. **Outbound kill-and-reassign demonstration**
5. **Narrow Gateway**

Futures succeeds only if the demo visibly proves more than three agents in three
worktrees. It should show one warm branchpoint, materially different running
implementations, shared versus divergent computation, coalesced actions, and
one promoted future.

#### Best Long-Term Product Candidate

1. **Distributed Migration Compiler**
2. **Heterogeneous Action Linker**
3. **Fern Futures**
4. **Attention/Action Plane**
5. **Exact Change Handoff**

The Migration Compiler has a clearer enterprise job and budget. Futures has the
strongest demo but uncertain task frequency. The Action Linker has a concrete
mobile, device, embedded, and cross-platform niche.

#### Best For Immediate Personal Utility

1. **Heterogeneous Action Linker**, when building mobile or cross-platform apps
2. **Exact Change Handoff**
3. **Fern Futures**
4. **Narrow Gateway**
5. **Distributed Migration Compiler**

The Action Linker could let a cloud agent invoke a personal Mac for Xcode while
using cheaper Linux compute for other actions. That may recur more often for one
developer than generating alternative implementations.

#### Best Learning Project

1. **Fern Futures**
2. **Distributed Migration Compiler**
3. **Heterogeneous Action Linker**
4. **Narrow Gateway**
5. **Outbound runner demonstration**

Futures spans the widest combination of Go, distributed systems, content-
addressed storage, compiler/build internals, process supervision, and cloud
execution without requiring fabricated production scale.

### Portfolio And Product Interpretation

Fern Futures should be treated differently depending on the goal:

| Goal | Interpretation |
| --- | --- |
| Standout portfolio project | **Strong go** after current baseline acceptance |
| Twitter/X or demo launch | **Strong go** only with a real visual and measured shared-compute result |
| Grab-specific project | **Secondary** to Gateway G0/G1 |
| Immediate personal feature | **Conditional** on tasks with real design ambiguity |
| Committed Fern product | **No-go until the two-future prototype sees repeat use** |
| Full multi-language platform | **No-go** before one Go repository proves action reuse and promotion value |

The recommended dual track is:

```text
Grab track
  deploy current Fern
    -> Gateway G0/G1
    -> one translation
    -> SSE cancellation
    -> provider-attempt ledger

Frontier track
  portable artifacts
    -> disposable local candidate
    -> two semantic futures
    -> typed Go actions
    -> shared build memory
    -> live previews
```

## Recommended Program

### Keep The Existing Gate

First publish, deploy, physically accept, and dogfood current Fern. A frontier
prototype should not erase the incomplete proof that the current product works.

### Foundation Program

Build these primitives because every credible frontier direction uses them:

1. Local content-addressed artifact store.
2. Verified Git bundle import/export.
3. Immutable `ActionSpec` and `ActionResult`.
4. Serial disposable local-Docker provider separate from `workspace.Manager`.
5. One pinned batch harness with exact input, cancellation, and terminal
   semantics.

This is useful infrastructure even if Fern Futures fails.

### Six-Week Frontier Spike

After the foundation exists:

1. Choose one Go repository and one task with two legitimate implementation
   strategies.
2. Create one semantic branchpoint.
3. Run two candidate continuations.
4. Share content-addressed dependencies and selected exact build actions.
5. Coalesce concurrent duplicate actions.
6. Run each candidate as a real application or return a concrete artifact.
7. Present both futures side by side.
8. Promote one and pass it into the existing normal result/publication path.
9. Measure duplicate CPU, bytes, latency, and whether the user keeps the chosen
   future.

Do not add cloud workers, memory snapshots, Mac routing, or automatic selection
until this local demonstration is compelling.

### Expansion Choice

If users value choosing live alternatives, add previews and one snapshot backend.

If shared build work is the strongest result, deepen Branchable Build Memory and
REAPI compatibility.

If cross-platform actions dominate, add the Mac/Linux Action Linker.

If users ask for organization-wide coordinated changes rather than alternatives,
pivot the same action and lineage primitives toward the Distributed Migration
Compiler.

If no branch use case beats one agent plus one correction, stop. Retain the CAS,
portable artifacts, disposable provider, and action contracts for normal Fern
execution.

## How This Changes Fern

Fern does not need to abandon its current primitives. It needs a new layer above
replaceable execution and below the selected result:

```text
current Fern
  Task -> Attempt -> one Result -> Verification -> Publication

frontier layer
  WorkGroup -> Branchpoint -> CandidateRun* -> Promotion
                                               |
                                               v
                                      current Fern Result path
```

Do not remove `UNIQUE(task_id)` result constraints or allow several current
attempts in the existing task model. Candidate generation belongs to a separate
state machine, and promotion is the only bridge into the established result
contract.

The current persistent OpenCode workspace remains useful for interactive work.
Futures uses a separate disposable provider. Snapshot, cloud, Mac, and device
backends are optional execution strategies behind the action graph.

## Final Position

The most ambitious credible Fern is not a fleet, gateway, benchmark, or sandbox.
It is:

> **A branchable software-construction fabric that turns inference-time compute
> into several live futures while sharing the compiler/build work underneath.**

That direction is visually surprising, technically deep, and aligned with where
leading agent products are heading. It combines:

- cloud and disposable execution;
- distributed content-addressed computation;
- compiler and build-system incrementalism;
- inference-time branching;
- heterogeneous toolchains;
- controlled outward effects;
- Fern's existing exact-result handoff.

The long-term enterprise expression may be a Distributed Migration Compiler.
The first proof should remain much smaller: two futures, one Go repository, one
shared action graph, one promoted result.

## Primary And Direct Sources

Agent products and industry direction:

- [OpenAI Codex app and parallel agents](https://openai.com/index/introducing-the-codex-app/)
- [Cursor: Agent swarms and the new model economics](https://cursor.com/blog/agent-swarm-model-economics)
- [Replit: Build in parallel](https://docs.replit.com/learn/build-in-parallel.md)
- [Anthropic: Managed Agents architecture](https://www.anthropic.com/engineering/managed-agents)
- [Anthropic: Effective harnesses for long-running agents](https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents)
- [Anthropic: Multi-agent research system](https://www.anthropic.com/engineering/multi-agent-research-system)
- [Anthropic: Building a C compiler with parallel agents](https://www.anthropic.com/engineering/building-c-compiler)
- [Cognition: Multi-agents working](https://cognition.ai/blog/multi-agents-working)
- [Stripe: AutoJDK](https://stripe.dev/blog/modern-java-at-stripe-language-upgrades-as-a-service)
- [DeepMind: AlphaEvolve](https://deepmind.google/discover/blog/alphaevolve-a-gemini-powered-coding-agent-for-designing-advanced-algorithms/)

Build, compiler, and action infrastructure:

- [Bazel Remote Execution API](https://github.com/bazelbuild/remote-apis)
- [Bazel Skyframe](https://bazel.build/reference/skyframe)
- [Buck2 DICE](https://buck2.build/docs/insights_and_knowledge/modern_dice/)
- [Buck2 incremental actions](https://buck2.build/docs/rule_authors/incremental_actions/)
- [BuildBarn](https://github.com/buildbarn/bb-remote-execution)
- [BuildGrid](https://buildgrid.build/)
- [Nix remote builders](https://nix.dev/manual/nix/latest/command-ref/conf-file.html#conf-builders)
- [gopls implementation](https://go.dev/gopls/design/implementation)
- [Dagger](https://docs.dagger.io/getting-started/introduction)
- [Depot coding-agent CI](https://depot.dev/docs/ci/how-to-guides/coding-agents)

Forkable execution:

- [Morph developer documentation](https://cloud.morph.so/docs/developers)
- [E2B sandbox fork](https://e2b.dev/docs/sandbox/fork)
- [Daytona persistence](https://www.daytona.io/docs/en/persistence/)
- [AgentENV](https://github.com/kvcache-ai/AgentENV)
- [forkd](https://github.com/deeplethe/forkd)
- [Mitos](https://github.com/mitos-run/mitos)
- [Firecracker snapshot support](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md)
- [Kubernetes Agent Sandbox snapshots](https://agent-sandbox.sigs.k8s.io/docs/sandbox/snapshots/)

Research directions, not production claims:

- [Large Language Monkeys](https://arxiv.org/abs/2407.21787)
- [SWE-Search](https://arxiv.org/abs/2410.20285)
- [CodeMonkeys](https://arxiv.org/abs/2501.14723)
- [SWE-Replay](https://arxiv.org/abs/2601.22129)
- [Scaling Test-Time Compute for Agentic Coding](https://arxiv.org/abs/2604.16529)
- [TClone](https://arxiv.org/abs/2605.17320)
- [Atomix](https://arxiv.org/abs/2602.14849)
- [Cordon](https://arxiv.org/abs/2606.17573)
- [AgentRewind](https://arxiv.org/abs/2608.14380)
- [Sandlock](https://arxiv.org/abs/2605.26298)
