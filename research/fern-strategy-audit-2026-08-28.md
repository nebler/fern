# Fern Strategy Audit: Product Utility And Grab Fit

Research date: 2026-08-28

Repository reviewed: `harden/production-readiness` at
`ab945b5a00db3a310b3fcc30fe8bc99669598b6f`

Status: research input, not an implementation claim. The supplied strategy
report was treated as an untrusted draft. Statements below distinguish official
public sources, repository evidence, vendor claims, and conclusions drawn from
them.

## Verdict

| Question | Verdict | Confidence |
| --- | --- | --- |
| Is Fern useful? | Yes, as a narrow self-hosted control and evidence plane for one owner's coding-agent work, if deployment dogfooding proves the workflow recurs. | Medium |
| Is Fern Labs a useful feature? | Conditionally. The strongest job is qualifying model, agent, prompt-policy, or runtime upgrades against repository-specific regression cases. | Medium |
| Is Fern Labs a validated standalone product or business? | No. Demand and willingness to pay are unproven, case authoring is expensive, and direct alternatives already cover much of the workflow. | High |
| Is Fern Gateway useful? | Yes as a credential-custody and measurement boundary. It is not differentiated as a general gateway. | High |
| Must Gateway G1 precede the first evaluation? | No. A no-build quality pilot can omit cost claims or label provider-reported usage as descriptive. G1 is required before authoritative per-request route and cost claims. | High |
| Does Fern currently match Grab AI Gateway or Palana? | No. Its engineering is relevant, but the implemented system has no model gateway, provider translation, model-use ledger, workload identity, default-deny egress, or hostile-agent isolation. | High |
| Is the proposed work useful for the Grab role? | Yes. G0/G1 and provider-stream fault testing are directly relevant. Redis, PostgreSQL, OIDC, and EKS are relevant only as measured scale work, not as default Fern dependencies. | High |

The product definition should be:

> Fern is a self-hosted control and evidence plane for durable coding-agent
> work. Its evaluation mode qualifies changes to the agent stack against
> versioned repository cases. Its Gateway keeps provider credentials outside
> the workspace and records provider-attempt facts when those facts are needed.

"Fern Labs" may remain a name for the evaluation mode. It should not replace
Fern as the product identity unless repeated external use proves that evaluation,
rather than durable operation, is the recurring job.

## What The Supplied Report Got Right

The following conclusions survive cross-checking:

- Deploying and dogfooding the current release is more important than adding a
  new subsystem. No checked-in evidence proves the physical Ubuntu, reboot,
  private phone TLS/WSS, revocation, or replacement-host journeys.
- Fern must not convert OpenCode idle, an empty inbox, process exit, or stream
  disconnection into agent success.
- Public coding benchmarks do not answer which complete agent configuration is
  appropriate for a particular repository and policy.
- Grab Bench is close methodological prior art for versioned cases, hidden
  certification checks, weak baselines, anti-gaming gates, row-level records,
  and explicit limitations.
- A provider boundary can reduce credential exposure and make route, latency,
  token, and cost facts observable.
- A universal gateway, public leaderboard, managed sandbox platform, native
  mobile application, or default Kubernetes deployment would be a distraction.
- Fern's current trusted, single-owner Docker boundary must not be described as
  hostile multi-tenant isolation.
- Redis, PostgreSQL, and Kubernetes belong only in a measured multi-replica
  profile or in separate portfolio work.

## Coding-Agent Market Corrections

The supplied report underestimates how much durable control-plane behavior
competitors now expose. This narrows Fern's claim but does not erase its exact
result and external-effect work.

| Product | Primary-source correction | Consequence for Fern |
| --- | --- | --- |
| Amp | Orbs are persistent, sleep/wake managed machines with remote control, production fleet support, and OIDC. Public material does not guarantee replay of an exact interrupted tool effect. | Generic durable remote compute is not differentiation; ambiguous-effect semantics may be. |
| Claude Code | Remote Control reconnects to local execution; web sessions are hosted; self-hosted execution is public beta; UltraReview independently attempts to reproduce findings. | Remote supervision and reviewer agents are occupied categories. Bind verification to exact Git evidence instead. |
| Codex | Official surfaces now include mobile remote control, worktrees, schedules, subagents, SDKs, machine-readable `codex exec --json`, review, workload identity, and security scans. Remote executes on the connected owner host; it is not cloud handoff. | Do not compete on surface breadth. A pinned `codex exec` contract is a credible second evaluation adapter. |
| Cursor | Cloud Agents expose durable per-prompt runs, terminal states, resumable SSE, usage, artifacts, OIDC, event subscriptions, environment provenance, iOS, and customer worker pools. | Cursor is a direct control-plane competitor, not merely an editor. |
| Copilot coding agent | GitHub retains sessions, logs, branches, checks, and PRs; execution may use ephemeral GitHub-hosted or customer self-hosted Actions runners. | Issue-to-PR and mobile dispatch are baseline. Fern must justify its separate journal through exact result authority and recovery. |
| Devin | Dynamic Workflows have typed stages and replay; Outposts runs tools/repos on customer infrastructure while planning/inference stay in Devin cloud; OIDC is available. | General workflows, resumable stages, BYOC workers, and workload identity are occupied. |
| Jules | Google announced general availability in 2025 even though some current wording still says experimental; its API remains alpha. It now has schedules, immutable activities, parallel attempts, snapshots, critics, and CI autofix. | Task-to-PR, event logs, schedules, and critics are not a Fern wedge. |
| T3 Code | Current tagged architecture is event-sourced, has idempotent command receipts, durable threads, Git checkpoints, and provider resume cursors. Server updates can still interrupt active work. | Do not claim T3 lacks durability. The narrower observed gap is separate correctness verification, cost custody, and portable evidence. |

### OpenCode Version Correction

The market report mixed latest OpenCode behavior with Fern's pinned
`0.0.0-next-17444` contract.

- Latest stable on the research date was `v1.18.25`, not a stable 2.x release.
- Its generated `session.wait` endpoint still returns
  `OperationUnavailableError`; API presence is not implementation.
- Stable `v1.18.25` does have durable prompt admission, a persisted input inbox,
  paginated session history, and per-session replay after an aggregate sequence.
  Its generic event stream remains live-only.
- The inspected current `opencode run` code sets a nonzero exit status on loop or
  provider errors. The supplied report's unqualified "exits 0 on failure" claim
  is stale for current stable, even if older issues documented that behavior.
- Fern's checked-in pinned profile remains different: its experimental log was
  observed to return only `log.synced`, so the current production adapter is
  correct to use finite projections and to reject generic success.

Before building Labs, test two distinct upgrade candidates rather than assuming
the current blocker is permanent: a newer pinned OpenCode server adapter using
per-session replay, and a pinned batch adapter such as `opencode run` or
`codex exec --json`. Process exit is still not enough by itself; the harness must
bind exact input/message identity, cancellation, result Git object, and evaluator
outcome.

## Corrections To The Supplied Report

### Product And Market Claims

1. **"Fern Labs is the product" is not supported.** Repository-specific agent
   evaluation is a real job, but the public evidence does not establish demand
   for another standalone platform. Stet directly markets private-repository
   agent bakeoffs, Sigmabench offers private-code evaluation, RepoAgentBench
   mines historical pull requests locally, and Harbor and Inspect provide
   reusable runner/evaluator infrastructure.

2. **The strongest recurring Labs job is regression qualification, not a model
   leaderboard.** The useful question is "did this model, runtime, or policy
   upgrade regress the tasks we care about?" This naturally reuses cases over
   time and fits Fern's exact-version discipline. An occasional "which model is
   best?" bakeoff may not repay case maintenance for one engineer.

3. **"Only Fern" is not a defensible claim.** No finite market review can prove
   uniqueness, and self-hosted evaluation systems already retain detailed local
   run evidence. Fern's credible distinction is the combination it has actually
   implemented: exact task/result identity, conservative ambiguity handling,
   host verification, and receipt-backed publication.

4. **"Independently verifiable" overstates a future evidence bundle.** A third
   party can verify hashes, signatures, Git objects, evaluator identity, and
   recorded outputs, but still trusts the Fern host and its signing key for the
   truth of model execution unless a provider or independently attested runtime
   supplies evidence. Use "portable, tamper-evident, host-attested evidence."

5. **"Without sending your code to anyone" is false with hosted models.** A
   workspace can send prompts, source excerpts, tool output, or other repository
   context to the provider. A defensible claim is that provider credentials stay
   out of the workspace and Fern's evidence remains under host custody.

6. **Wake-time comparisons with sandbox vendors are not decision evidence.** A
   persistent OpenCode workspace waking from stop or freeze is not the same
   operation as restoring a vendor sandbox. Re-measure Fern for operations, but
   do not use an apples-to-oranges ranking as the product thesis.

7. **"T3 has no durable task semantics" is false.** Its current tagged source
   documents event sourcing, durable threads, command receipts, Git checkpoints,
   and resume cursors. Fern can compare stronger correctness and recovery
   semantics; it cannot use absence of durability as the contrast.

8. **Current OpenCode behavior must not be projected onto Fern's old pin.** The
   latest stable release has useful per-session replay and corrected CLI exit
   handling, while the pinned Fern profile does not. Upgrade research is a
   prerequisite to choosing the Labs adapter, not a reason to weaken current
   production claims.

### Evaluation Design

1. **Gateway G1 is not needed to test whether cases are useful.** The first
   pilot can compare quality and intervention records while omitting cost or
   labeling usage as provider-reported and descriptive. Building the Gateway
   first risks solving metering before proving that the evaluation changes a
   decision.

2. **A manual-seal pilot is research, not yet a useful automated product.** It
   can validate case yield, evaluator quality, reporting, and whether the result
   changes a rollout decision. It cannot validate unattended throughput or
   time-to-completion.

3. **An agent-authored completion fact is not the only possible batch contract.**
   A later runner can define a fixed time or token horizon, stop the environment,
   and evaluate the exact resulting snapshot. That supports quality-under-budget
   evaluation without claiming the agent reported success. It does not provide
   completion latency and must be labeled as a harness-terminated run.

4. **Fresh runs cannot reuse Fern's current production lifecycle unchanged.**
   The production runtime deliberately preserves the OpenCode data volume when
   compute is destroyed. Labs needs a separate disposable-run lifecycle or a
   borrowed runner such as Harbor.

5. **The existing verification runner is a useful primitive, not a complete
   Labs evaluator.** It accepts a host-owned static native executable, requires
   an exact clean commit before and after, and treats repository mutation as an
   integrity failure. Per-case hidden artifacts, dynamic manifests, alternative
   valid patches, and disposable evaluator environments need an additional
   contract.

6. **A Labs database is premature for the pilot.** Versioned manifests,
   append-only row JSON, artifacts by digest, and Markdown are enough to test the
   job. If Labs is promoted and a run atomically creates a task, attempt, scoped
   token, and budget reservation, add dedicated tables to the existing
   workspace database. A second SQLite database would introduce a dual-write
   recovery problem at exactly that authority boundary.

### Gateway Design

1. **The endpoint protocol is not decided.** G0 must observe the exact pinned
   OpenCode requests before selecting Chat Completions, Responses, Anthropic
   Messages, or another shape. Choosing an OpenAI-compatible Chat Completions
   endpoint from market popularity would invert the contract-first sequence.

2. **A normal Docker container cannot reach a service bound only to the host's
   loopback interface.** The personal Gateway should remain in the Fern process
   to preserve the current one-service deployment, but G0 must prove a private
   Linux container-to-host transport and its exposure boundary. The likely
   shape is a Fern-managed bridge and a listener bound only to its host-gateway
   address, with network identity added to runtime attestation. "Loopback-only
   Gateway" is not an implementable topology for the current bridge container.

3. **Per-run attribution is not currently established.** Fern sends the model
   in the OpenCode session request, but provider authentication is process-level.
   Unless G0 proves that an exact per-session token or header can be supplied and
   preserved, G1 can attribute requests only to a workspace credential. It must
   not infer a task/run binding from timestamps.

4. **A scoped Gateway token is still a credential visible to workspace code.**
   It reduces blast radius because it can expire, enforce a model allowlist and
   budget, and be revoked. This is narrower than Palana's general proxy-only
   secret substitution and policy-mediated egress.

5. **Open egress does not by itself defeat provider-key custody.** If the
   workspace has no provider key or alternate provider authentication, it cannot
   directly make an authenticated provider call merely because egress is open.
   Default-deny egress is required for a stronger claim that all relevant
   outbound traffic is policy-mediated, not for the narrower claim that the
   provider key stays host-side.

6. **At-rest age encryption is not a complete key-custody design.** Encrypting a
   provider key with another key available to the same unattended host may add
   little. Start with the existing protected service-secret boundary or systemd
   credentials and document root/Docker trust; add external key management only
   for a concrete threat.

7. **Idempotent ledger writes do not make a provider request exactly once.**
   Persist request and provider-attempt intent before the upstream call. If the
   response is lost, record an ambiguous attempt and do not replay unless the
   exact upstream contract proves replay safe. A fallback can still duplicate
   provider computation or cost even when no output reached the caller.

8. **One row is insufficient once fallback exists.** Use one logical request
   with one or more provider-attempt records. Each attempt needs route, start
   fence, status, latency, nullable usage, nullable cost, and upstream request
   identity. Aggregates belong above those facts.

9. **"Safe fallback" means output-safe, not side-effect- or cost-safe.** It is
   valid only before downstream response commitment. The first provider may
   still have accepted and billed ambiguous work, so both attempts must be
   retained and charged against the budget conservatively.

10. **Current Fern SSE work is transferable but is not a model Gateway.** Fern
    already proxies long-lived traffic and parses OpenCode lifecycle events. It
    does not yet implement provider event translation, usage trailers, `[DONE]`,
    provider error normalization, TTFT, or post-commit fallback rules.

11. **Removing provider-key environment variables is not enough for migration.**
    OpenCode `/connect` credentials can remain in the persistent data volume and
    bypass Gateway accounting. G1 needs an audit/migration procedure for the
    interactive volume, while disposable evaluation runs must start with a new
    Gateway-only OpenCode volume.

12. **Very short-lived environment tokens conflict with the current runtime
    contract.** Fern attests environment values and changing them requires
    container recreation. Immediate revocation can remain a Gateway database
    decision, but renewal for the durable workspace needs either a proven
    OpenCode configuration-update path or planned recreation. Fresh evaluation
    containers do not have this problem.

## Repository Feasibility Check

The proposed Labs design does not compose over the current task path without new
boundaries:

| Constraint | Repository evidence | Consequence |
| --- | --- | --- |
| One supervised workspace and one repository | `docs/ARCHITECTURE.md:8-16`; `internal/config/config.go:68-77` | Labs needs a disposable run provider rather than treating production workspaces as a fleet. |
| One effecting attempt per workspace | `internal/taskstore/migrations.go:253-254` | Serial execution fits; parallel experiments do not. |
| Model and agent are fixed by startup task configuration | `cmd/fern/tasks.go:215-229` | The current task API cannot select arbitrary arms. |
| Task service requires explicit GitHub authority | `cmd/fern/tasks.go:91-98` | A local synthetic benchmark is not a drop-in task-service workload. |
| Production destroy retains OpenCode state | `internal/runtime/lifecycle.go:298-329` | Fresh per-run state requires another cleanup contract. |
| Runtime mounts one host repository and persistent data volume | `internal/runtime/spec.go:92-100` | Fresh checkout, hidden evaluator, and run artifacts need new mount rules. |
| Default container networking has no Fern egress policy | `internal/runtime/provision.go:51-75` | Fern cannot claim Palana-style default-deny or forced Gateway routing. |
| Verification records exact clean-commit checks | `internal/verification/verification.go:367-484` | Reusable foundation, but hidden test injection must preserve the exact-result integrity contract. |
| OpenTelemetry packages are transitive Docker dependencies | `go.mod:34-51` | Fern has no composed OTel tracing pipeline today. |

The current result record retains Git identities and manifests, not all Git
object bytes. A disposable runner must export a durable patch, Git bundle, or
content-addressed result object before deleting its checkout and volume.

This argues for a no-build pilot first and, if it passes, a separate `fern eval`
command or package with disposable-run semantics. Reuse IDs, digest conventions,
Git collection, bounded process handling, and failure vocabulary where they fit.
Keep experiments, cases, arms, runs, and evaluations in dedicated tables, while
allowing one atomic store transaction to bind a run to its task, attempt, scoped
token, and budget. Do not redefine the operational task states as experiment
states.

## Is The Product Useful?

### Evidence That The Job Is Real

- Grab Bench publicly describes versioned task definitions, deterministic and
  judge scoring, hidden certification cases, baselines, canaries, anti-gaming,
  usage facts, and row-level failure analysis.
- Stet markets local private-repository comparisons across agents, models,
  instructions, and harnesses by mining merged pull requests.
- Sigmabench markets historical-change evaluation with frozen tools and
  containers for private codebases.
- RepoAgentBench is an MIT local-first implementation that mines pull requests
  into run manifests and records patches, logs, and verification results.
- Harbor and Inspect provide established runner, sandbox, scorer, intervention,
  and logging primitives that can be extended rather than rebuilt.
- METR's work on maintainer disagreement and reward hacking supports hidden,
  adversarial, and human-reviewed evaluation. It does not establish demand for
  Fern specifically.

### Evidence That The Product Is Not Yet Validated

- Public sources do not establish that individuals will pay for a self-hosted
  repository-evaluation appliance.
- Competitor paid-pilot offers establish an offer, not retained customers or
  willingness to pay.
- Case selection, historical dependency reconstruction, prompt reconstruction,
  hidden-check design, alternative-valid-solution handling, and maintenance are
  the expensive work. Fern currently automates none of them.
- The target risks sitting between markets: more operational ceremony than a
  casual individual wants, but no SSO, RBAC, hostile-workload isolation, broad
  harness support, or managed operations for an enterprise buyer.
- Fern has not yet been deployed and used through the physical journey on which
  its current product promise depends.

### Product Decision

- **Fern core:** continue and dogfood.
- **Evaluation mode:** run a falsifiable pilot before implementation.
- **Standalone Labs business:** no-go until repeated external use and payment.
- **Gateway:** build for normal Fern credential reduction and measurement, or as
  directly relevant portfolio work; do not justify it solely as a prerequisite
  for an unvalidated Labs platform.

## Smallest Falsifiable Pilot

Do not build a Labs service for this pilot.

1. Trial Stet on the target private repository if its local/authentication
   boundary is acceptable. Use RepoAgentBench or Harbor on two representative
   cases as an open-source control.
2. Inspect 20 recent behavior-changing pull requests and attempt to produce 8
   valid cases within four engineer-hours. If no suitable private repository is
   available, use Fern cases only to test mechanics, not market demand.
3. Compare two arms that differ in one variable. Use two randomized,
   interleaved repetitions per case and arm, for 32 runs if the initial case
   yield passes.
4. Keep hidden checks outside the agent workspace. Include a no-op or weak
   baseline and audit every arm disagreement manually.
5. Record exact base/result commit, runtime/environment identity, terminal
   reason, evaluator identity, hard failures, interventions, recovery events,
   changed paths, duration, and patch digest.
6. Treat pre-Gateway usage and cost only as estimated/descriptive, with its
   source named. Do not use it for chargeback or routing claims.
7. Inject one runner crash and one provider timeout. Determine whether Fern's
   recovery and ambiguity fields answer a decision-relevant question the control
   products do not answer.

Proceed to a minimal `fern eval` implementation only if:

- at least 8 of 20 candidate changes become valid cases within four hours;
- setup and gold evaluation replay reliably after 30 days;
- the evaluator has no audited false acceptance in the pilot;
- the report changes or prevents one real rollout decision;
- Fern's provenance, intervention, or recovery record answers a question not
  answered cheaply by Stet plus ordinary CI;
- the owner intends to maintain the cases for the next two upgrades.

If Stet or RepoAgentBench plus CI answers the question within one engineer-day,
integrate or document that workflow instead of building Fern Labs.

## Grab Stack Comparison

Grab's public systems map to three different Fern concerns and must not be
collapsed into one comparison:

| Grab system | Purpose | Nearest Fern concern | Honest comparison |
| --- | --- | --- | --- |
| Grab AI Gateway / GrabGPT Gateway | Organization-wide unified model API, provider translation, authorization, routing, usage, cost, and shared capacity | Future Fern Gateway | Same class of boundary, radically smaller scope. Fern has no implementation today. |
| Palana | Kubernetes-native secure execution substrate for agents, with namespace isolation, identity, default-deny and policy-mediated egress, Vault, proxy-only secrets, audit, and external lifecycle controls | Fern runtime and future credential boundary | Adjacent control placement, different threat model. Fern is trusted single-owner Docker and is not Palana-like containment. |
| Agent Platform | Shared production scaffolding, model indirection, tracing, SDKs, MCP integration, deployment conventions, and evaluation around hundreds of services | Fern composition and future integrations | Fern demonstrates rigorous local composition, not platform scale or breadth. |
| Grab Bench | Versioned production-shaped evaluation with hidden cases, baselines, anti-gaming, and row-level records | Future Fern evaluation mode | Strong methodological prior art. Grab Bench is publicly described methodology, not a reusable Fern dependency. |

### Verified Role Fit

Grab's current `Senior Software Engineer, Backend (AI)` posting, requisition
`744000137791699` / `REF5628X`, was open with an active application form on
2026-08-28. It names the AI Gateway as the unified integration point for more
than 60 models and explicitly calls for:

- OpenAI-compatible to provider-native translation;
- SSE streaming and automatic fallback;
- Go and Python reverse-proxy engineering;
- Redis distributed rate and capacity isolation;
- API-key and OIDC/workload-identity authentication;
- token usage, cost, PostgreSQL, data-lake, and chargeback work;
- Kubernetes/EKS, migrations, testing, on-call response, RCA, and post-mortems;
- LangChain and LangGraph experience.

The current posting proves these are role responsibilities. It does not prove
that every item is already implemented in Grab's production architecture.

The 2025 Grab AI Gateway article publicly establishes provider-native reverse
proxies, an OpenAI-compatible interface, API-key authorization and provider-key
substitution, dynamic region/model routing, request/response audit in the data
lake, cost calculation/showback, and per-key request-rate limits. Redis,
PostgreSQL, OIDC/workload identity, SSE, and fallback are exact current job
requirements, but their detailed production implementation is not public. In
particular, the 2025 article described token- and cost-based limits as future
work; it did not publish a Redis algorithm or PostgreSQL schema.

Palana's human OIDC, Kubernetes-derived agent identity, Vault integration, and
policy-mediated egress must not be projected onto GrabGPT Gateway as though they
were one implementation. They are related systems with different authorities.

### Fern Fit By Capability

| Capability | Fern today | Smallest honest evidence |
| --- | --- | --- |
| Reverse proxy and streaming | Long-lived OpenCode HTTP/SSE/WSS forwarding and lifecycle SSE parsing | Transferable experience only; do not call it provider SSE. |
| Effect correctness | Durable intent, exact identity, ambiguity reconciliation, cancellation fences, publication receipts | Strong existing backend signal. |
| Model API gateway | None | G0 fixture plus G1 one-provider passthrough. |
| Provider translation | None | One translation target after the observed G0 protocol. |
| Fallback | None | Output-safe pre-commit fallback with separate provider-attempt rows and fault tests. |
| API keys / workload identity | Basic/device/GitHub credentials, not model API keys or OIDC | Hashed scoped Gateway token first; OIDC only for a real multi-user/fleet need. |
| Rate limiting | Pairing abuse controls only | In-process model limits first; Redis only with two replicas and failure tests. |
| Usage and cost accounting | None | SQLite request and provider-attempt journal with nullable usage and versioned prices. |
| PostgreSQL / data lake | None | Optional measured scale profile, not a fake production dependency. |
| Kubernetes / EKS | None; deliberately single-host Docker | Optional two-replica deployment only if it tests behavior unavailable locally. |
| Palana security | Trusted workspace, open egress, provider keys may enter | Narrow host-held provider key; no general proxy-only-secret or hostile-agent claim. |
| Evaluation | Exact result and verification primitives, no experiments | No-build pilot, then serial `fern eval` only if the pilot passes. |
| Testing and operations | Extensive unit/race, lifecycle, browser, upgrade, release, backup/recovery work; physical acceptance incomplete | Deploy, rehearse, retain an incident log, then add fake-provider fault tests. |

Fern's best interview signal is not pretending to have Grab's scale. It is the
ability to explain which single-host invariants remain valid at scale, which
ones require Redis/PostgreSQL/workload identity, and why those dependencies do
not belong in the personal product before measured need.

Do not delay an application until the full roadmap is complete. The role is open
now, and the current correctness, recovery, proxy, and Go work already supports
a credible systems narrative. G0/G1 is the highest-value additional artifact for
that role.

## Palana Boundary

A future defensible statement is:

> Fern Gateway keeps model-provider credentials outside the trusted coding
> workspace and attributes requests to the narrowest credential the pinned
> OpenCode contract supports. Fern does not mediate general egress or provide
> Palana-style hostile-agent containment.

Do not claim any of the following without new implementation and acceptance
evidence:

- Palana-like isolation;
- general proxy-only secrets;
- default-deny or identity-mediated egress;
- workload identity;
- namespace-per-agent or hostile multi-tenancy;
- Kubernetes-native operation;
- equivalence to Grab's Agent Platform.

## Corrected Sequence

There are two legitimate priorities and they should be named separately.

### Product Sequence

1. Publish the signed baseline.
2. Deploy it to Ubuntu and complete physical acceptance.
3. Dogfood the core for two weeks and retain failures.
4. Run the no-build private-repository evaluation pilot against Stet and an
   open-source control.
5. Complete Gateway G0/G1 if normal Fern credential custody, the pilot's
   measurement needs, or the Grab portfolio goal justifies it. The checked-in
   OpenCode contract harness already has fake `/v1/chat/completions` and
   `/v1/responses` paths, but does not yet prove production transport, token
   preservation, every request path, or cancellation.
6. Build `fern eval` only if the pilot's case-yield, decision-utility, and Fern-
   delta gates pass.
7. Add translation, budgets, fallback, distributed state, or evidence export
   only in response to measured need.

### Grab Portfolio Sequence

1. Apply while the role is open; do not wait for the roadmap.
2. Complete deployment and one honest incident/recovery account.
3. Build G0 and G1 with provider-stream cancellation and fault fixtures.
4. Add one translation/fallback path only if time remains.
5. Demonstrate Redis/PostgreSQL/two replicas only as a clearly separate scale
   experiment with load and replica-loss evidence.

This separation prevents job-description keywords from distorting the daily
product while still producing relevant engineering evidence.

## Decision Register

| Decision | Result |
| --- | --- |
| Preserve Fern core as product identity | Go |
| Deploy and dogfood before subsystem expansion | Go |
| Run a no-build private-repository bakeoff | Go |
| Build a broad Fern Labs platform now | No-go |
| Treat Labs as an optional `fern eval` mode | Conditional go |
| Build Gateway G1 before validating cases | No-go for product sequencing |
| Build Gateway G0/G1 for credential security and Grab evidence | Go after baseline deployment, with scope fixed by G0 |
| Build a universal gateway | No-go |
| Add Redis/PostgreSQL/Kubernetes to normal Fern | No-go |
| Add a measured two-replica portfolio profile | Conditional go |
| Claim Palana parity or hostile-agent isolation | No-go |
| Export third-party-verifiable model-execution proof | No-go without a stronger attestation authority |

## Primary And Direct Sources

Grab:

- [Senior Software Engineer, Backend (AI)](https://www.grab.careers/en/jobs/744000137791699/senior-software-engineer-backend-ai/)
- [Grab AI Gateway](https://engineering.grab.com/grab-ai-gateway)
- [Palana Part 1](https://engineering.grab.com/palana-part-1-secure-platform-for-ai-agents)
- [Palana Part 2](https://engineering.grab.com/part-2-palana-architecture)
- [Agent Platform Part 1](https://engineering.grab.com/how-grab-builds-and-runs-ai-agents-at-scale)
- [Grab Bench](https://engineering.grab.com/grab-bench-evaluating-ai)
- [LLM-Kit](https://engineering.grab.com/supercharging-llm-application-development-with-llm-kit)

Evaluation systems and methodology:

- [Stet private-repository evaluation](https://www.stet.sh/private) (vendor claim)
- [Stet methodology](https://www.stet.sh/methodology) (vendor claim)
- [Sigmabench methodology](https://sigmabench.com/methodology/) (vendor claim)
- [RepoAgentBench](https://github.com/HumphreySun98/repoagentbench)
- [Harbor](https://github.com/harbor-framework/harbor)
- [Inspect AI](https://inspect.aisi.org.uk/)
- [METR: SWE-bench passing PRs and maintainer decisions](https://metr.org/notes/2026-03-10-many-swe-bench-passing-prs-would-not-be-merged-into-main/)
- [METR: reward hacking](https://metr.org/blog/2025-06-05-recent-reward-hacking/)

Coding agents and runtimes:

- [Amp: Agents Everywhere](https://ampcode.com/news/agents-everywhere)
- [Amp Orbs](https://ampcode.com/docs/markdown/orbs)
- [Claude Code Remote Control](https://code.claude.com/docs/en/remote-control)
- [Claude Code self-hosted environments](https://code.claude.com/docs/en/self-hosted-environments)
- [Codex documentation index](https://learn.chatgpt.com/llms.txt)
- [Cursor Cloud Agents](https://cursor.com/docs/cloud-agent)
- [Copilot cloud agent](https://docs.github.com/en/copilot/concepts/agents/cloud-agent/about-cloud-agent)
- [Devin Dynamic Workflows](https://docs.devin.ai/work-with-devin/dynamic-workflows)
- [Jules changelog](https://jules.google/docs/changelog/)
- [T3 Code architecture at `v0.0.35`](https://github.com/pingdotgg/t3code/blob/v0.0.35/docs/internals/overview.md)
- [OpenCode `v1.18.25`](https://github.com/anomalyco/opencode/releases/tag/v1.18.25)

The Grab engineering site had published Agent Platform Part 1 but no discoverable
Part 2 on 2026-08-28. Do not cite an unpublished Part 2 as architecture evidence.
