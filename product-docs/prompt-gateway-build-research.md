# Deep Research Prompt — Designing the fern AI Gateway Skeleton + Its Dogfooding Plan

> Paste the block below into a fresh Claude session with internet access / research mode.
> Notes above this line are for me.

---

Research and produce a **build-ready specification** for the next construction phase of my Go project **fern**: an AI gateway layer sitting between self-hosted coding-agent workspaces and LLM providers — plus a concrete plan for **dogfooding it as my daily driver** from the first working slice. Output must be specific enough to implement across 4–6 weekends without further architectural decisions. This is a design-spec mission ending in decisions, not a survey. You cannot read my repository; Section 2 is the complete codebase picture — treat anything not stated there as unknown, mark it `ASSUMPTION`, and say how I'd verify it locally rather than inventing it.

## 1. Who I am and why this gateway exists

Senior backend/platform engineer (~7 yrs; Kotlin/TS professionally, Go for this project; compiler background; owns CI/CD infra and DX tooling for ~150 engineers). Weekends-only build window, concurrent with interview prep for a specific role: **Senior SWE Backend (AI) — AI Gateway / GrabGPT Gateway, Applied AI team, Petaling Jaya, Malaysia.** Verified live JD language, verbatim: *"distributed rate limiting and tiered capacity isolation on Redis"* · *"design multi-provider LLM routing systems that translate a unified, OpenAI-compatible API surface into provider-native formats, with support for SSE streaming, automatic fallback, and provider-specific capabilities"* · *"token-level usage logging, cost computation, and chargeback pipelines integrated with PostgreSQL and the data lake"* · *"API key and OIDC/workload-identity authentication"* · skills: Go+Python proficiency, *"hands-on experience with LLMs… LangChain, and LangGraph"*. Documented failure mode: over-designing, under-shipping — call out every place ambition exceeds a weekend.

## 2. The codebase you are designing for

**What fern is:** one Go binary (Go 1.24, module `github.com/nebler/fern`) supervising ephemeral Docker workspaces that each run `opencode serve` — the server mode of opencode, an open-source AI coding harness (repo: `anomalyco/opencode`, moved from `sst/opencode`; **v2 beta**, CLI package `@opencode-ai/cli`, binary `opencode2`; fern pins version `0.0.0-next-17444` by npm version AND image digest, enforced in CI by a black-box contract harness).

**Package map (strictly layered, no cycles):**

| Package | Responsibility | Facts the gateway design must respect |
| --- | --- | --- |
| `internal/runtime` | The Docker port. Owns vocabulary: `Spec`, `Observation`, `Endpoint`, five states, `IntentStore`. Classifies ambiguous Docker state via a durable pause-intent journal. Supports two suspend mechanisms (`SuspendStop` graceful stop, `SuspendFreeze` cgroup freezer behind config `idle.mode`). Has `ExecWorkspaceGH` — its only exec capability (pinned `gh` binary inside the attested container). | Container ports bind loopback-only, dynamically assigned; endpoint re-derived every wake, never persisted. Actual image ID (not tag) attested on every resume. |
| `internal/workspace` | Policy kernel for ONE workspace: request admission gate, coalesced wakes, lifecycle serialization token, endpoint attestation with monotonic generations, conservative two-pass idle barrier, quiesced fence for evidence collection. | Deep module, tiny interface (`AcquireRequest`, `Pause`, `AcquirePaused`, `AcquireQuiesced`, …). |
| `internal/watch` | SSE activity watcher. Spec-correct SSE parser; connection epochs reject stale events; single-goroutine actor reduces events to arm/cancel/none timer actions feeding the idle stopper. | OpenCode SSE is volatile BY CONTRACT — disconnections lose events, no Last-Event-ID replay anywhere. |
| `internal/proxy` | Ingress. Two listeners: remote (device-cookie grants via QR pairing over Tailscale Serve) and operator (loopback Basic auth). Classifies requests observe/read/work; work invalidates idle evidence. Reverse proxies to the workspace with `httputil.ReverseProxy{FlushInterval:-1}` (unbuffered SSE proven here). Fern-owned `/fern/*` routes: pairing, device admin, CSRF tokens, task UI + task API, GitHub App onboarding, and an operator-only `POST /fern/api/v1/debug/wake-trace` returning per-phase millisecond waterfalls. | Route registration lives in one dispatcher keyed on exact paths under `/fern/api/v1/`; handlers are mounted via a `Controls` struct that differs per listener (operator gets more than remote). Browser mutations require origin checks + CSRF; Basic-auth mutations are exempt. |
| `internal/taskstore` | SQLite journal (WAL, FKs, **trigger-enforced state machines**, checksummed migrations, CGO-free driver). Tasks/attempts/receipts/events/results/manifests/verifications/publications/seal-requests. Immutable terminal states, monotonic revisions, tamper-rejecting triggers. | This is the house pattern for anything durable. |
| Coordinators (`taskdelivery`, `taskexecution`, `taskresultcoord`, `taskverification`, `taskpublicationcoord`) | Journal-fenced loops: commit phase BEFORE effect; after ambiguity, read-reconcile only, never mutate again. Delivery uses caller-chosen opencode session/message IDs so retries are idempotent by identity. Publication pushes via single-use askpass files + `--force-with-lease` and creates draft PRs through a GitHub App installation token. | Exact-once delivery against a beta upstream is proven by a contract harness pinning the image digest (13 characterized properties). |
| `internal/config` | Strict YAML (unknown fields rejected), canonical-origin/listen validation, environment-forwarding ALLOWLIST: only `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `OPENCODE_PASSWORD` may reach the container; host-only names (`FERN_CONTROL_PASSWORD`, `GH_TOKEN`, `GITHUB_TOKEN`) rejected three ways — by key, by `${...}` alias expansion, and by post-expansion equality. Full-config `Validate()` gauntlet runs in `fern up` after load. | This forwarding machinery is what the gateway REPLACES. |
| `cmd/fern` | Composition root (`up.go` startup order: parse → validate → bind listeners → lease → construct → reconcile → serve) + CLI verbs (`init doctor github up attach down status logs debug events debug wake version`). | New surface area follows existing verb/dispatcher patterns. |

**Integration points the gateway must use (these are the seams):**

1. **Environment injection at container create** — `specEnvironment(spec)` builds the container env from the validated allowlist. Today real provider keys land HERE.
2. **Workspace image** — node:22-slim base, npm-installed `opencode2`, pinned `gh` 2.98.0, runs as UID/GID 1001, entrypoint `opencode2 serve --hostname 0.0.0.0 --port 4096`.
3. **Operator route pattern** — exact-path dispatch under `/fern/api/v1/` with Basic `fern:<control-password>`, JSON helpers, method-not-allowed handling; mounted per-listener via `Controls`.
4. **SQLite migration machinery** — ordered named migrations with checksum verification; add tables there, never beside it.
5. **Config gauntlet** — new options need: yaml node in `fileConfig`, default in `Default()`, merge in `load()`, check in `Validate()`, test coverage, example-yaml comment.

**House conventions:** fail-closed (unknown ⇒ defer/refuse); intent journalled before effect; `%w` wrapping + sentinels + `errors.Is`; no panics on user input; gofmt/vet/-race in CI; conventional commits; table-driven tests; external services faked via httptest servers implementing the Docker REST surface; single-binary ethos — adding a required daemon (Redis, Postgres) is a deployment-philosophy event, not a dependency bump. Current deployment: hardened systemd unit + fail-closed host backup script.

**Explicitly NOT built yet (zero lines today):** any outbound HTTP to LLM providers; any Redis; any Postgres; any Python; any model-pricing data; any token/key issuance. The gateway is fern's first provider-facing component. Provider keys today are plain env vars injected into the workspace — the weak posture this phase replaces ("keys never enter the workspace").

## 3. Hard constraints

- **opencode v2 only.** Anything known only from v1 docs/issues is a gap to flag. Beta means churn; fern pins digests and characterizes behavior with a contract harness before depending on it.
- **Fail-closed constitution extends to the gateway:** metered rows idempotent; mid-stream failures never silently retried once headers are committed; unknown upstream behavior defers rather than guesses.
- **Trusted single-owner posture:** no multi-tenancy theater; but the security story (keys never enter the workspace; scoped revocable tokens; audit trail) must be real because it replaces real forwarding.
- **Deployment-footprint sensitivity:** requiring extra daemons (Redis, Postgres) needs explicit justification vs an embedded/in-process default with optional drivers. SQLite is the established house database.

## 4. The plan under stress-test

Ordered phases from prior research, each to be verified then specified: (1) host-side key custody replacing env forwarding — hashed short-lived fern-scoped tokens issued per workspace; (2) OpenAI-compatible passthrough (`/v1/chat/completions`; decide whether Anthropic-native `/v1/messages` passthrough or full translation is the right v1 cut); (3) SSE passthrough done right (unbuffered flush, `[DONE]` framing, header-committed mid-stream error contract à la OpenRouter, client-disconnect propagation, idle-vs-total timeouts, TTFT stamped on first upstream byte write-once); (4) rate limiting — tier-isolated token buckets, in-process vs Redis-backed (JD says Redis verbatim; house says minimal daemons — resolve this tension explicitly, including a shadow-mode/demo path); (5) usage metering → idempotent cost ledger (streamed usage extraction: OpenAI `stream_options.include_usage` final empty-choices chunk; Anthropic cumulative `message_delta` output tokens — never sum; unit prices stored beside computed costs; pricing-table snapshot strategy — LiteLLM's community JSON license/cadence); (6) a small LangGraph consumer pointed at the gateway (ReAct + streaming tool-call deltas + Postgres-or-SQLite checkpointer + one interrupt + cost reconciliation against fern's ledger) closing the JD's hands-on-LangGraph gap.

Prior calibration to verify rather than repeat: LiteLLM virtual keys hashed in Postgres, budget-reserve-then-reconcile, cooldowns/fallback chains, community pricing JSON; Bifrost (Go, Apache-2.0) virtual-key governance, CEL-layer routing, per-key-vs-transient failure split, "<100µs overhead" vendor claim; Envoy AI Gateway charges usage to Redis counters from response metadata; Higress two-phase Lua limiting (threshold at request, INCRBY real tokens at stream end, TTL repair, fail-open); OpenRouter's canonical contracts (mid-stream error = in-band event + `finish_reason:"error"` once headers committed; rate-limit headers only on rejections; key introspection endpoint). Reference docs: platform.openai.com streaming cookbook; docs.anthropic.com streaming pages; docs.litellm.ai; github.com/maximhq/bifrost; aigateway.envoyproxy.io; higress.ai plugin sources; openrouter.ai/docs.

## 5. Research tasks

### Task 0 — Roadblock census: enumerate every way this phase dies, then resolve or fence each

The provider-redirection question (Task 1) is the famous blocker, but it is not the only one. Conduct a systematic census across the dimensions below. For each: state the failure mode, resolve it with primary-source evidence, or mark `ASSUMPTION` and give me the exact local verification command (usually a one-line curl or docker exec against a throwaway container). Rank all blockers by lethality: **A = kills the design if wrong**, **B = forces rework**, **C = cosmetic**.

1. **Transport path — can a workspace even reach a host-side gateway?** The gateway listens on the host; containers have their own network namespace, so workspace → gateway cannot be `127.0.0.1`. Evaluate concretely: `host-gateway` extra_hosts mapping to the docker bridge IP, publishing the gateway on a second loopback-bound port and targeting the bridge gateway address, a user-defined bridge network, or a Unix socket volume mount. Constraints: the gateway must remain unreachable from anything off-host, and the fix must survive fern's desired-spec fingerprint checks (note any fingerprint implication of changed mounts/settings). Recommend one mechanism.
2. **Credential substitution — what does opencode hold instead of a real key?** Keys-never-enter-the-workspace means the workspace's "provider key" must become a fern-scoped token (or placeholder). Verify: does v2 require a non-empty API key before activating a provider at all? Does it pass the client's `Authorization` header through unmodified to a custom base URL, or rewrite/mangle auth per provider? Decide fern's stance: gateway validates its own scoped tokens and swaps them for real provider keys (Palana-style placeholder swap), vs blind passthrough of whatever the workspace presents.
3. **Request fidelity — paths, bodies, headers.** Does v2 hit `/v1/chat/completions` verbatim on the custom base URL? Any path rewriting, header injection, or body transformation that would confuse gateway classification/metering? Does it probe `/v1/models` at startup or during model selection, and what must the gateway stub for that to succeed?
4. **Translation depth.** Full OpenAI↔Anthropic translation is a project-killer. Determine the minimal honest cut: e.g., OpenAI-schema passthrough + Anthropic-native passthrough (both formats flow through the gateway untranslated, routed by model prefix), deferring true translation. Check whether the JD's "translate… into provider-native formats" can be satisfied by demonstrating ONE translated pair rather than a universal translator, and what that pair should be.
5. **Model catalog resolution.** v2 integrates a model catalog (models.dev). When the base URL is custom, does model listing/validation go remote, local-catalog, or fail? A workspace that cannot resolve `claude-*` model IDs is dead on arrival regardless of the gateway working.
6. **Ledger ↔ task-pipeline identity interaction.** Gateway cost rows are per HTTP request; fern tasks are attempts with their own IDs. Define the join: the gateway receives a fern-scoped token bound to (workspace, task-attempt) — specify how attempt identity propagates (header injected by fern at delivery time? embedded in the token?) so chargeback rolls up to tasks without guesswork.
7. **Infra additions.** Redis and Postgres each violate the single-binary/deployment-footprint ethos. For each: embedded/in-process default vs optional daemon vs dockerized-demo-only — with the honest statement of what the JD demonstration loses in each configuration.
8. **OIDC realism.** Real OIDC issuance (short-lived signed JWTs from fern) vs opaque hashed keys. What does the JD phrase actually demand, what did Palana actually do, and what is the smallest implementation that survives an interviewer probing it?
9. **Contract-harness impact.** Every mechanism above touches the pinned-image world. List which harness scenarios need new cases (e.g., "provider traffic transits the gateway," "placeholder key accepted") and whether the digest pin must move.

Deliverable: a **blocker register** — table of dimension | lethality | resolution | evidence-or-assumption | local verify command. Any A-lethality item left at ASSUMPTION must come with a designed fallback plan.

### Task 1 — THE BLOCKER: how does opencode v2 redirect provider traffic to a custom gateway?

Everything bends around this. Establish precisely, from primary sources (opencode.ai/v2/docs, the v2 OpenAPI spec at opencode.ai/v2/openapi.json, source/issues on github.com/anomalyco/opencode):

- Per-provider `baseURL`/custom-endpoint configuration in v2: config-file shape (`~/.config/opencode/opencode.json` or v2 equivalent), which bundled providers accept overrides (OpenAI, Anthropic, Vertex…), and exact key names.
- Environment-variable redirection: does v2 honor `OPENAI_BASE_URL`, `ANTHROPIC_BASE_URL`, or equivalents, and where are they read?
- Credential storage: what `/connect` writes and WHERE (opencode.db? auth.json?) — and therefore how fern could pre-seed or template provider config/auth into a fresh workspace's data volume before first boot, without breaking the desired-spec fingerprint or the contract harness.
- Given the findings, choose fern's injection mechanism (env-at-create vs generated-config-mount vs volume pre-seed vs hybrid) and specify it exactly: what fern writes, when, and how it verifies the redirection took effect (probe request through the gateway?).
- If v2 cannot redirect for some providers, enumerate which work, which don't, and the honest workaround for the rest.

Also refresh: current v2 release/changelog movement in the last week; status of issues #24874 (Bearer auth) and #38458 (durable replay); confirmation that server-side auth remains Basic-only.

### Task 2 — Dogfooding design: making fern's gateway MY daily driver

This is a first-class deliverable, not garnish. Research how serious builders eat their own cooking, then produce a migration + operation plan:

- **Rollout patterns** from gateway products and internal-platform practice: shadow mode (mirror traffic, compare, no commitment), parallel keys, percentage/canary cuts, kill switches. Which translate to a single-user setup, and what does a safe personal rollout look like day-by-day (e.g., week 1 shadow-log only; week 2 OpenAI via gateway while Anthropic stays direct; week 3 all-in)?
- **Migration mechanics for my real daily opencode install**: I run opencode in tmux daily with credentials already stored via its `/connect` flow. What breaks when the base URL moves — stored OAuth tokens vs static API keys per provider, session continuity, model catalog resolution? Rollback procedure when the gateway misbehaves mid-work-session (the break-glass path must be one command).
- **Failure playbook precedents**: documented failure modes of living behind a gateway (buffering regressions, limit self-throttling, ledger write failures blocking work?) and the mitigations experienced teams chose (fail-open vs fail-closed metering, local bypass flags, circuit-breaker-to-direct).
- **Evidence flywheel**: what to instrument so weeks of real usage become portfolio/interview material — per-task/per-day cost rollups, TTFT distributions, limit-hit telemetry, incident postmortems of my own outages. How do platform teams present such "we ran it ourselves" evidence credibly (dashboards, published numbers, postmortem writeups)?

Deliverable: a dogfooding runbook (rollout stages, rollback commands, instrumentation checklist, exit criteria for "trusted enough to demo"), plus a short list of pitfalls other teams hit that I should preempt.

### Task 3 — Component depth calibration (verdicts, not catalogs)

For each component, pull CURRENT primary-source implementation details and render a verdict: weekend-slice scope vs naive-if-skipped subtleties vs do-not-build.

- **Custody/tokens**: schema proposal for fern (columns, scopes: workspace, tier, budget, model allowlist, expiry), hashing choices, prefix convention, revocation semantics, introspection endpoint worth copying (OpenRouter `GET /api/v1/key`).
- **Routing/failover**: minimal defensible policy (static ordered chains + cooldown windows + allowed-fails counters?), explicitly listing the enterprise features fern must skip (adaptive LB, CEL routing, mirroring, latency-based selection).
- **Limits**: GCRA via `go-redis/redis_rate` current state; Redis-native GCRA command status; reserve-at-request/reconcile-at-stream-end for TPM; tier/model-composed key scheme; header contract (`X-RateLimit-*`, `Retry-After`); fail-open vs fail-closed posture recommendation for a solo self-hosted deployment.
- **Metering**: streamed-usage edge cases per provider (verify against current API docs), idempotency keying (request_id + attempt), DDL sketch (Postgres-compatible, SQLite-runnable), pricing snapshot sourcing/licensing/update cadence (LiteLLM community JSON).
- **SSE correctness matrix**: confirm/refute each hazard — flush semantics (Go's streaming auto-detection vs explicit `FlushInterval:-1`); `X-Accel-Buffering: no`; `[DONE]` sentinel framing; committed-header error frames; disconnect propagation cancelling upstream billing; Caddy's documented non-cancellation quirk; idle-vs-total deadline modeling; TTFB measurement pitfalls (cite LiteLLM's TTFT≡duration bug family if still referenced).

### Task 4 — Landscape deltas since August 23, 2026

Has Grab's **Agent Platform Part 2** (GrabGPT Gateway deep-dive, teased in Part 1) been published? If yes, extract architecture details and remap the plan clause-by-clause. PJ posting still live or edited? Bifrost/LiteLLM/Envoy AI Gateway/agentgateway releases in the last month? Anyone shipping "keys never enter the agent's workspace" for coding agents specifically?

### Task 5 — Scope question: egress enforcement now or later?

Should the gateway double as an egress policy plane (per-workspace domain allowlists evaluated at route time — trivially cheap since all provider traffic already transits it; mirrors Grab Palana's Envoy+OPA posture), or is that a later phase? Recommend with reasoning tied to weekend math and the JD.

### Task 6 — LangGraph consumer blueprint

Current versions Aug 2026; concrete app: ReAct agent, custom `base_url` pointing at the gateway, streaming tool-call deltas, checkpointer, one human-in-the-loop interrupt, per-run cost reconciliation reading fern's ledger. Pin versions; note recent API changes that could bite.

### Task 7 — Portfolio engineering: what this phase must produce as public evidence

The code is necessary but not sufficient; this phase is also the centerpiece of a job campaign. Specify the artifact chain:

1. **Per-slice artifacts**: for each weekend slice, name the concrete public output — README table rows, waterfall/cost-dashboard screenshots, ledger query transcripts, a recorded demo beat, or an essay paragraph. Nothing counts unless it renders on the repo front page or in a 90-second video.
2. **Demo choreography**: refine the target demo into exact beats mapped to JD nouns — stream tokens live → kill/degrade the provider mid-conversation → automatic fallback visible in logs → trigger the rate limiter → show the idempotent cost row landing → `fern debug wake` waterfall. Give shot-by-shot requirements for the recording (what must be visible, durations, captions).
3. **Interview-question map**: for every component in the spec, the likely senior-panel question it pre-answers ("how would you do distributed rate limiting?", "what breaks SSE behind proxies?", "design a chargeback pipeline", "keys never enter the workspace — walk me through it"). Mark any component with no mapped question as decoration and recommend cutting it.
4. **Framing defense**: the known risk is reading as "a platform project wearing a gateway hat." Prior analysis recommended keeping one repo with gateway-forward framing — specify exactly how the README/docs change in this phase (new sections, renamed headings, which diagram gains a gateway box) so the gateway reads as the load-bearing enforcement plane of the whole system, not a bolt-on.
5. **Dogfooding-as-evidence**: connect to Task 2 — define the presentation format for weeks-of-real-usage data (weekly spend chart, incident postmortem template for my own outages, TTFT distribution plots) and the credibility bar it must meet to be shown to interviewers.

### Task 8 — Scope boundaries and stop conditions

My documented failure mode is never knowing where to stop. Produce the fences:

1. **IN / OUT / DEFERRED matrix.** For every component and sub-feature in the spec, assign: `IN` (this phase), `OUT` (rejected, one-sentence reason), or `DEFERRED` (later phase + the trigger condition that would promote it). Candidates to classify explicitly (add others you find): full schema translation · semantic caching · prompt-injection guardrails · admin dashboard · SSO/full OIDC stack · multi-provider breadth beyond two · adaptive load balancing · retry-storm backoff policies · Prometheus/OTel export · data-lake Parquet dumps · per-user multi-key hierarchies · streaming response caching.
2. **Definition of done per slice** — behavioral, testable end-states, not vibes ("slice 3 is done when a streamed response traverses the gateway with correct `[DONE]` framing behind a proxy that buffers by default, verified by an automated test").
3. **Hard cap and cut lines.** Six weekends maximum. For each slice define the Sunday-night checkpoint: if not demonstrable, exactly what ships partial and what is documented as deferred. No slice may silently absorb its successor.
4. **Gold-plating tripwires.** Concrete signals I've left scope — e.g., adding observability exporters before the ledger exists, building an admin UI, supporting a third provider, refactoring the task store "while I'm in there." Name at least five, phrased so I can recognize them mid-flight.
5. **The saturation argument.** State the point of diminishing returns explicitly: once every JD noun has been demonstrated once with real measurements, additional gateway work adds approximately zero hiring probability — estimate where that line sits and force the plan to stop there.

## 6. Deliverables

1. Resolved blocker (Task 1): chosen injection mechanism, specified exactly.
2. Build-ready spec: package layout proposal fitting the seams above, config-schema additions, route list, token + ledger DDL, failover policy, limiter design (in-process/Redis decision included), SSE test matrix, egress verdict.
3. Weekend-by-weekend slicing (≤6), each independently demonstrable and dogfoodable — slice 1 must be usable on my real workload even if later slices slip.
4. Dogfooding runbook (Task 2): rollout stages, break-glass rollback, instrumentation checklist, exit criteria.
5. Corrections register: everything in this prompt your sources contradicted (versions, behaviors, licenses, statuses), with links.
6. Confirmed vs assumed throughout; primary sources cited by URL; content farms discounted; NOT FOUND stated plainly where evidence is absent.
7. Blocker register (Task 0): dimension | lethality | resolution | evidence-or-ASSUMPTION | local verify command.
8. Scope matrix (Task 8): every feature classified IN / OUT / DEFERRED with reasons and promotion triggers, plus the five gold-plating tripwires.
9. Artifact checklist (Task 7): per-slice public outputs, demo beat sheet, interview-question map — formatted so each item is independently checkable.

## 7. Rules

- No implementation — specification only. Sketch interfaces/DDL/config shapes, not full programs.
- Disagree with the plan wherever evidence says so; propose cuts yourself if honest estimates exceed six weekends, and show what was cut.
- Every architectural claim about opencode v2 or a reference gateway carries a URL or an ASSUMPTION flag.
- Don't pad: "trivial" is a valid verdict; say it and move on.
- Every `OUT` classification requires a one-sentence justification; every `IN` component requires the interview question it answers or an explicit `decoration` tag.
- You may not soften scope: if something belongs in OUT, put it in OUT even though it would impress. Impressing is not the goal; finishing is.
- For any blocker left at ASSUMPTION, the fallback plan must be concrete enough that I can start the slice anyway and swap mechanisms without redesign.
