# Fern Product Direction: Critical Reassessment and Inventor's Review

> **Document status:** Supplemental dated audit pinned to Fern `7c470d6`, not
> current implementation or operating guidance. See
> [docs/DOCUMENTATION.md](./docs/DOCUMENTATION.md).

**Research date: 2026-08-15**

This report evaluates Fern repository HEAD
[`7c470d6`](https://github.com/nebler/fern/commit/7c470d6faad733e2d57848e77b66b3ae35c3169f),
OpenCode V1 `1.18.16` at
[`a3647eb`](https://github.com/anomalyco/opencode/commit/a3647eb025c7615159d417dcc49fc39fdaeba65b),
and OpenCode V2 at
[`3d782ef`](https://github.com/anomalyco/opencode/commit/3d782efee4c7724024deda362ca22390794dacf3).
Product and pricing claims are snapshots on the research date.

This is an independent audit of `DEEP_RESEARCH_REPORT.md`, not a rewrite that
assumes its conclusions. The first report is directionally strong, especially
in rejecting a hosted platform, but it is too conservative about Fern's most
interesting invention surface and not conservative enough about several stale
market and OpenCode claims.

## 1. Executive Decision

- Fern today is a carefully engineered host-side controller for one durable
  OpenCode V1 Docker workspace. Its strongest implementation is the admission
  gate between an all-session-idle observation and Docker stop
  (`internal/workspace/manager.go:251-299`), not remote access.
- It solves a real technical failure mode: stopping an OpenCode process during
  a turn can abandon process-local provider streams, tool work, and partial
  output while preserving only committed session state (`DAY-1.md:337-373`).
  It has not proved that enough users experience this as a product problem.
- Fern is not meaningfully better for basic access than authenticated
  `opencode serve` plus Tailscale. It adds conservative stop/wake, ownership,
  drift detection, and failure classification. Those benefits have not yet
  been measured against the simpler alternative.
- Keep Fern as a polished OSS portfolio project. Promote it to a community tool
  only after unrelated weekly users choose it specifically for safe lifecycle
  behavior. There is no evidence for a business, open-core company, or hosted
  cloud.
- Recommended concept: **Fern One, an explainable private workspace
  appliance**. One durable private origin opens the native OpenCode interface,
  wakes transparently, reports a precise lifecycle generation, and later
  explains why stopping was safe.
- The first report's “wake-ready, inspectable workspace” is correct but vague.
  The minimum inspectable state is `stopped`, `waking`, `ready`, `busy`,
  `idle-countdown`, `stopping`, `failed`, or `unknown`, each with observation
  time, concrete generation, and failure owner.
- The inventor's wedge is a **quiescence seal**, not a generic verification
  receipt: a local record of the exact Fern-observed facts that authorized a
  stop. This is novel enough to investigate because Fern uniquely controls
  admission and observes watcher epoch, session status, container identity,
  spec, and stop outcome. It proves no code correctness.
- The next two weeks should deliver CI, one supervised install, one canonical
  private origin, direct plain-OpenCode comparison, ten authenticated wakes,
  resource and failure measurements, a bounded transition ledger, and a demo.
- For the specific Grab role, a separate small gateway project is stronger
  evidence than adding gateway-shaped features to Fern. Finish Fern honestly;
  do not distort it into Redis, provider routing, or token accounting.
- Do not build setup/resume hooks, previews, artifacts, source verification,
  webhooks, multi-workspace scheduling, Kubernetes, or a custom UI until the
  appliance loop records a repeated user failure that one of those features
  removes.

### Comparison With `DEEP_RESEARCH_REPORT.md`

| Topic | Existing report | This report | Judgment |
|---|---|---|---|
| Core direction | Wake-ready, inspectable private workspace | Same direction, specified as an appliance plus lifecycle ledger | Existing conclusion supported, product contract tightened |
| Differentiation | Conservative lifecycle; possible source-bound receipts | Conservative lifecycle plus quiescence evidence | Existing report looked for novelty in the wrong evidence layer |
| `fern attach` | Explicit URL only if dogfood proves need | Canonical external origin is required now | Existing report is too conditional; listen and public/private origin are different values |
| Concepts | Wrapper, polished wrapper, verification | Appliance, flight recorder, quiescence seal | Existing A/B concepts are polish levels, not distinct concepts |
| Market | Stop/wake is common | Stop/wake is now substantially commoditized | Existing report is stale on E2B, Vercel, Daytona, Sprites, and Claude self-hosting |
| OpenCode V2 | Broadly beta | Beta product with many explicitly experimental APIs and an internal provider lifecycle | Existing classifications are too coarse |
| Product scorecard | Numeric 1-5 scores | Observable mechanisms and anti-lessons | Existing scores imply unsupported precision |
| Two-week roadmap | Evidence first | Evidence first plus a tiny transition ledger | Supported, with explicit origin moved to P0 |
| Grab | Fern is transferable, gateway project more direct | Same | Supported |

## Research Passes

### Pass One: Amp From First-Party Sources

**Method.** Reviewed Amp's current manual, Orbs and OIDC manuals, pricing,
security, plugin and SDK references, notes, and announcements. Manual contracts
were separated from team conventions and private architecture was not inferred.

**Result.** Execution is selected when a thread is created: local, a fresh
Orb, or a live runner. No reviewed public operation moves an existing thread
between executors. A runner need not be named; `--runner-id` only gives it a
stable identity. Orbs bundle fresh compute, setup/resume, supervised portals,
artifacts and review, durable webhooks, schedules, secrets, workload identity,
mobile, multiplayer, and experimental Puck. Fern overlaps only a narrow
lifecycle fragment. [Amp manual](https://ampcode.com/manual),
[Orbs](https://ampcode.com/manual/orbs),
[SDK](https://ampcode.com/manual/sdk/typescript), and
[plugin API](https://ampcode.com/manual/plugin-api).

### Pass Two: OpenCode Docs and Pinned Source

**Method.** Treated V1 `1.18.16` and V2 separately. Used the official V2 docs
as primary authority, inspected both pinned commits, and reviewed workspace
provider issues and the V2 workspace PR sequence. Internal source was not
promoted into a public contract.

**Result.** V1 `serve`, attach, Basic auth, SSE, persistence, web, and plugins
are supported; its workspace adaptor is explicitly experimental. V2 is a beta
client/background-service architecture. Its HTTP API describes event, session,
filesystem, PTY, and shell groups as experimental, while the internal
`WorkspaceDriver` has real create/connect/idle-suspend/destroy behavior and
private SDK injection. No public third-party workspace-provider registration
contract was found. [V2 overview](https://opencode.ai/v2/docs/),
[API](https://opencode.ai/v2/docs/api), and
[client](https://opencode.ai/v2/docs/build/client).

### Pass Three: Market and Economics

**Method.** Reviewed first-party product, lifecycle, BYOC/self-hosting,
security, and pricing material for managed agents, customer-hosted runners,
workspace platforms, and sandbox infrastructure. Dated list prices are not
treated as comparable total costs.

**Result.** Customer-controlled execution and persistent stop/wake are no
longer unusual. Factory BYOM, Cursor self-hosted pools, GitHub self-hosted
runners, Claude self-hosted environments, Coder, Ona, Daytona BYOC, and E2B BYOC
cover customer infrastructure. E2B now preserves memory and files indefinitely
while paused; Vercel persistence is default with automatic stop snapshots and
resume; Fly Sprites warm-pause in roughly 30 seconds and publish 100-500 ms warm
wake; Daytona has disk-preserving container stop and memory-preserving VM pause.
[E2B persistence](https://docs.e2b.dev/sandbox/persistence),
[Vercel persistence](https://vercel.com/docs/sandbox/concepts/persistent-sandboxes),
[Sprites lifecycle](https://docs.sprites.dev/concepts/lifecycle/), and
[Claude self-hosted environments](https://docs.anthropic.com/en/docs/claude-code/self-hosted-environments).

### Pass Four: Product Journey and Alternatives

**Method.** Walked discovery, installation, configuration, start, local and
remote attach, task execution, disconnect, phone return, inspection, recovery,
sleep, upgrade, and removal. Proposed features were admitted only where Fern
owns necessary state or authority.

**Result.** The product failure is first success and re-entry, not absent
hooks. `fern attach` converts `proxy.listen` into an HTTP URL and rewrites
wildcard binds to loopback (`cmd/fern/attach.go:47-65`); it cannot represent a
Tailscale Serve origin or reverse-proxy hostname. The strongest current moment
is invisible: Fern's stop decision. Turning that into a small legible record is
more coherent than adding `doctor`, hooks, or receipts.

### Pass Five: Product-Taste References

**Method.** Studied current concrete interactions in Tailscale, Vercel,
Cloudflare, Convex, and T3. The review excluded visual imitation and brand
reputation.

**Result.** The useful transfer is stable identity plus explicit generation,
one direct outcome, delegated identity, classified retry/failure semantics,
and risky technology behind reversible boundaries. Fern should not copy their
dashboards, hosting platforms, primitive breadth, databases, or scaffolds.

### Grab Cross-Check

**Method.** Verified the live first-party role and current Grab engineering
posts. Role requirements, public Grab system behavior, and inference were kept
separate.

**Result.** The live Senior Software Engineer, Backend (AI) role is explicitly
the GrabGPT Gateway role: 60+ models, provider translation, SSE, fallback,
Redis capacity controls, API key/OIDC identity, token/cost attribution,
PostgreSQL/data lake, Kubernetes, CI/CD, and operation of a critical service.
Fern demonstrates transferable Go proxy and reliability skills, not this
gateway. [Grab role](https://www.grab.careers/en/jobs/744000137791699/senior-software-engineer-backend-ai/)
and [AI Gateway](https://engineering.grab.com/grab-ai-gateway).

## 2. Corrections to Existing Research

### `AMP_PRODUCT_RESEARCH.md`

| Lines | Correction |
|---|---|
| 41-56 | The recommendation outruns evidence. “Ready to prove” assumes hooks, previews, artifacts, and receipts before basic deployment proves value. |
| 64-81 | Remote access is conditional on networking and a usable external origin. Fern provides neither identity nor remote publication. |
| 101-103 | V1 has an experimental `experimental_workspace` adaptor. The stable-contract conclusion is right; the categorical absence claim is wrong. [Pinned source](https://github.com/anomalyco/opencode/blob/a3647eb025c7615159d417dcc49fc39fdaeba65b/packages/plugin/src/index.ts#L26-L63). |
| 114-117 | “Seamless” is unsupported. Attach derives only a listen-based local URL (`cmd/fern/attach.go:47-65`). |
| 137-155 | Missing V2 internal provider lifecycle, idle janitor, persistence, and private provider injection. [Driver](https://github.com/anomalyco/opencode/blob/3d782efee4c7724024deda362ca22390794dacf3/packages/core/src/workspace/driver.ts), [workspace service](https://github.com/anomalyco/opencode/blob/3d782efee4c7724024deda362ca22390794dacf3/packages/core/src/workspace.ts). |
| 159-184 | Two passes are insufficient for the product decision. Market, journey, source-pinned V2, taste, and job evidence were absent. |
| 190 | Only Orb-backed threads receive Orbs; local and runner threads do not. |
| 232-242 | `.amp/dev-ports.json`, preflight conventions, development login, and known artifact paths are team guidance, not all platform guarantees. |
| 252-258 | Add access following thread visibility, authorized-request wake, same-Orb hairpin, and unsupported cross-portal browser CORS. [Portals](https://ampcode.com/manual/orbs#portals). |
| 279-295 | Add 202-as-queued, at-least-once delivery, 30-second handler, burst 10, refill 10/minute, queue 100, body 1 MB, sender idempotency, and plugin-owned signature verification. [Webhooks](https://ampcode.com/manual/orbs#webhooks). |
| 304-306 | OIDC binds Amp workspace/project/user/thread identity, not source or environment state. Default TTL is ten minutes and relying parties verify issuer, audience, signature, and expiry. [OIDC](https://ampcode.com/manual/orbs/oidc). |
| 434-533 | The “lowest-cost” feature set is a process supervisor, router, file server, command runner, and security boundary. It is not low cost. |
| 558-620 | Fern has an in-memory endpoint generation and durable pause intent, not yet a durable source/request generation suitable for receipts. |
| 635-639 | “Safe scale-to-zero” overstates container stop. The host remains running and billed. |
| 683-696 | Ranking mixes user value, interview signal, and category expansion without weights. |
| 778-818 | Numeric CV scores imply false precision. Hooks and previews do not close the role's main routing, quota, accounting, database, or Kubernetes gaps. |
| 830-835 | No checked-in daemon-backed Docker test suite or raw ten-sample wake distribution supports “Docker integration tests” as a reproducible repository claim. |
| 968-971 | “Known-good,” previews, and evidence are future behavior stated as current product positioning. |

### `ROADMAP.md`

| Lines | Correction |
|---|---|
| 7-17, 53-71 | The definition of done is not a credible 24-30 hour part-time slice. It combines product proof with a new arbitrary-code execution subsystem. |
| 44-49 | CI, install, deployment, measurement, and demo are P0. Hooks and broad doctor output are conditional P2 work. |
| 55-56 | There is no CI workflow. On 2026-08-15 Go checks passed, but Docker could not run because the local daemon was unavailable. |
| 58-60 | A phone opens OpenCode web; it does not use terminal `fern attach` unless a phone terminal is explicitly part of the test. |
| 117-156 | The canonical external origin is a product value, not an optional discovery. A listen socket cannot safely model Serve, TLS, and tailnet DNS. |
| 177-286 | Hooks add arbitrary execution, timeout/kill semantics, output retention, persistent generation state, rollback, and container replacement races. The estimates are optimistic. |
| 288-324 | A standalone `doctor` cannot reliably report in-memory watcher state without a daemon API. A bounded `status --json` over durable observations is smaller and more honest. |
| 326-345 | “Docker suites” are not checked in. Record manual experiments as manual evidence until automated. |
| 347-382 | Install, release, measurement, and demo are compressed after speculative work; they are the actual product slice. |
| 403-405 | One anecdote should not trigger previews or verification. Require repeated use and a reproduced failure. |

### `DEEP_RESEARCH_REPORT.md`

| Area | Material correction |
|---|---|
| Amp runners | “Named live runner” is too narrow; stable runner IDs are optional. |
| Amp details | Mobile review, attachment types, schedule semantics, multiplayer security scope, and Puck's experimental status are compressed or omitted. |
| OpenCode classifications | V2 event, session-message, filesystem, PTY, and shell routes are explicitly experimental inside the beta product, not simply Beta. [API](https://opencode.ai/v2/docs/api). |
| OpenCode web/mobile | Current V2 docs reviewed do not establish a supported V2 mobile workflow; source existence is internal evidence. |
| OpenCode source history | Add PRs [#41187](https://github.com/anomalyco/opencode/pull/41187), [#42142](https://github.com/anomalyco/opencode/pull/42142), and [#42227](https://github.com/anomalyco/opencode/pull/42227), which show a real internal provider lifecycle and later private injection. |
| Market | E2B persistence, Vercel default persistence, Daytona lifecycle, Fly Sprites, and Claude self-hosted environments materially changed the landscape. |
| Cursor | Self-hosted workers still use Cursor's cloud agent loop/inference and send model-visible data outside the customer network. |
| Factory | Managed Droid Computers auto-pause; BYOM does not stop the customer host. |
| Concepts | Lifecycle wrapper and polished lifecycle wrapper are not distinct product concepts. |
| Product scorecard | Unexplained 1-5 scores are decorative precision. Observable interaction choices are stronger evidence. |
| External URL | Treating `-url` as conditional understates a known model error between bind address and client-visible origin. |
| Differentiation | Source-bound receipts are speculative. A stop-authorization/quiescence record uses authority Fern already has. |

## 3. Amp Cross-Check

| Area | Current documented behavior | Fern implication |
|---|---|---|
| Placement | Thread creation selects local, Orb, or runner; no reviewed relocation operation | Do not add placement abstractions for one workspace |
| Compute | Fresh Debian 12 Orb per Orb-backed thread; E2B named as compute provider, private architecture undisclosed | Fern's shared checkout is fundamentally different |
| Pricing | Tiny through xxlarge: $0.08, $0.17, $0.33, $0.66, $1.32/hour; minute billing; paused free; enterprise 50% higher | Never equate stopped-container resources with zero infrastructure billing |
| Pause | Five idle minutes; archive also pauses; thread work, portal traffic, webhooks, and schedules can wake | Wake is only one component of Amp's task loop |
| Setup/resume | `.agents/setup`; first-party note says snapshot reuse up to 24 hours. `.agents/resume` blocks ten seconds, surfaces early non-zero, then continues in background | Do not copy hooks without stricter semantics and demand |
| Portals | Declared supervised services, sticky ports, health, thread-scoped auth, wake, hairpin routing | One preview is already a non-trivial product |
| Webhooks | Persist-before-202, at least once, event IDs, sender idempotency, retries, limits; plugin verifies source signatures | A Fern GitHub handler would need a queue, not just HMAC |
| Identity | Layered secrets and short-lived OIDC bound to Amp identities | Generic OIDC is disproportionate for Fern |
| Review | Diffs, files, terminal, screenshots/video, live portals, comments, mobile | An artifact directory alone is not a review workflow |
| Multiplayer/Puck | Time-bounded shared Orb access; Puck is experimental coordination over many objects | Fern has no routing leverage for a meta-agent |
| Exact-state proof | No public source/environment digest and automatic stale-evidence invalidation found | A research gap, not validation |

Amp should teach Fern direct outcomes and honest state. It should not set Fern's
feature list. Amp operates a company-sized fleet, identity boundary, queue,
portal edge, collaboration system, and review product.

## 4. OpenCode Integration Assessment

| Surface | V1 `1.18.16` | V2 at `3d782ef` | Classification |
|---|---|---|---|
| Server/attach/auth | Documented `serve`, `attach`, Basic auth | Background service, explicit server and generated client | V1 Supported; V2 Beta |
| Events | `/event` SSE with heartbeat/filtering | `/api/event` async iterable | V1 Supported; V2 Experimental |
| Persistence | SQLite/WAL global data directory | SQLite, V1 migration, bounded recovery | V1 Supported behavior; V2 Beta behavior; internals Internal |
| Web | Documented V1 browser interface | Not established as a public mobile contract in reviewed V2 docs | V1 Supported; V2 Unknown/Internal |
| Plugins | Public context, hooks, client, shell, events | Public beta capability subset | V1 Supported; V2 Beta |
| Session placement | Experimental adaptor may return local path or remote URL | Plugin may choose existing location on create | V1 Experimental; V2 Beta placement only |
| Session move | Experimental workspace warp | Public route under experimental session routes | Experimental |
| Filesystem/PTY/shell | Mixed server surfaces | Explicitly experimental location-scoped routes | V2 Experimental |
| Worktrees/location | Mixed experimental concepts | Public location and worktree APIs | V2 Beta, with experimental adjacent routes |
| Provider registration | Experimental V1 adaptor only | Internal `WorkspaceDriver`; private SDK injection | V1 Experimental; V2 Internal |
| Public third-party provider | No stable contract | Not found in docs, HTTP API, client, or plugins | Not Supported |

V2 source contains more than an interface: a default 20-minute idle threshold,
one-minute polling, process activity leases, opaque persisted bindings, and
create/connect/suspend/destroy behavior
([workspace service](https://github.com/anomalyco/opencode/blob/3d782efee4c7724024deda362ca22390794dacf3/packages/core/src/workspace.ts)).
That is serious redundancy risk. It is not Fern's invariant: an environment
process lease is not the same as independently confirming all OpenCode sessions
idle while request admission is closed.

**Recommendation:** keep the V1 attach/proxy boundary for the shipped product.
Build a disposable V2 harness only against documented client/HTTP APIs. Do not
depend on V1's experimental adaptor, V2's private SDK, or a fork. Migrate when
the required V2 APIs stabilize and the harness proves authentication, attach,
all-session quiescence, V1 data migration, and crash recovery.

## 5. Market Map

### Managed Task and Review Products

Amp, Cursor, Copilot coding agent, Codex cloud, Jules, Devin, Factory, Claude
Code cloud, Augment, and Replit sell a task/thread/PR outcome. Their value is
fresh or dedicated execution plus client continuity, review, artifacts, Git
integration, and bundled model economics. Fern cannot match this loop with
lifecycle alone.

The closest threat is not Amp. It is **Factory BYOM** or **Claude self-hosted
environments** for customers who want managed clients with customer execution.
Factory's managed computers auto-pause, but BYOM leaves host lifecycle to the
user. Claude's public beta runs web/mobile/desktop/terminal/scheduled cloud
sessions on customer runners, while Anthropic retains queue, transcript,
orchestration, and inference. These products weaken a paid Fern thesis but
leave a fully OSS/no-hosted-control-plane preference unserved.

### Workspace and Enterprise Control Planes

Coder, Ona/Gitpod, Daytona BYOC, and Northflank provide identity, templates,
prebuilds, quotas, audit, lifecycle, previews, observability, and customer
infrastructure. Coder Community is already free and self-hosted. Moving Fern
upmarket would enter a mature category and require organization-level support.

### Lifecycle Substrates

| Product | Current relevant lifecycle | Decision impact |
|---|---|---|
| E2B | Filesystem and memory pause, indefinite retention, connect-to-resume, auto-pause/resume; Pro $150/month plus use | Stop/wake and memory preservation are commodity API primitives |
| Vercel Sandbox | Persistence is default; stop snapshots filesystem; next SDK operation resumes; `onCreate`/`onResume`; Pro platform minimum/credit plus use | Implements much of the proposed Fern workspace contract |
| Fly Sprites | Persistent disk, about 30-second idle, 100-500 ms warm and 1-2 s cold wake, services and Tasks API | A simpler persistent-computer mental model than Fern |
| Daytona | Container disk-preserving stop/archive; VM memory-preserving pause and hot snapshots; BYOC | Generic sandbox lifecycle is not a Fern opportunity |
| Cloudflare Sandbox | Request-driven container wake and scale-to-zero on Workers/Durable Objects | Powerful but platform-coupled substitute |
| Modal | Managed sandboxes and an official OpenCode example | Hosted OpenCode substrate already exists |
| Runloop | Disk-preserving suspend and storage-only billing | Another agent-focused infrastructure substitute |
| Northflank | General services/jobs/previews with managed or BYOC operation | Broader application environment, not Fern's niche |

### Direct OSS Overlap

[Netclode](https://github.com/angristan/netclode) demonstrates that a private
self-hosted agent environment is a real project category, but it also raises the
bar with microVMs, multiple harnesses, iOS/macOS clients, previews, Git review,
secret brokering, warm pools, k3s, Redis, S3/JuiceFS, and Tailscale. Fern should
win only by being radically smaller: one ordinary Docker host, one OpenCode
workspace, no hosted control plane.

### Segment and Economics

The possible user is unusually specific: they already operate a constrained
Docker host, want one durable OpenCode checkout, reject a hosted control plane,
accept Docker rather than per-task isolation, and value conservative OpenCode-
aware shutdown enough to run another daemon. That is a credible OSS niche and
an unvalidated market.

| Outcome | Assessment |
|---|---|
| Polished portfolio project | Supported after CI, deployment, measurements, and demo |
| Useful community project | Possible after repeated unrelated weekly use |
| Sponsorship/support | Possible but weak; requires recurring install/upgrade requests |
| Small business | Unsupported without paying users rejecting mature substitutes |
| Enterprise open core | Unsupported and operationally mismatched |
| Managed cloud | Do not pursue |

## 6. User Journey and Product Critique

### Five Highest-Friction Moments

1. Installing a versioned artifact and supervising `fern up` after reboot.
2. Distinguishing bind address, local attach URL, tailnet origin, and Serve URL.
3. Knowing which layer owns OpenCode credentials, provider keys, TLS, and user identity.
4. Returning after disconnect and understanding busy, idle, stopped, failed, and stale observations.
5. Upgrading or removing compute without losing the retained OpenCode volume.

The strongest current moment is the stop gate: close admission, require no held
requests, serialize lifecycle, re-inspect runtime, query authenticated session
status, require all idle, then stop (`internal/workspace/manager.go:251-299`).
The product currently hides this moment in code and logs.

### Smallest Coherent Experience

```text
install one versioned binary and image
  -> configure one repository and one private origin
  -> open native OpenCode web or run fern attach
  -> ordinary authenticated traffic reports waking -> ready
  -> resume the retained OpenCode session
  -> disconnect without Docker work
  -> inspect the latest lifecycle transition
  -> Fern records why the workspace became safe to stop
  -> resources return to the existing host
```

Fern owns lifecycle, admission, generation, and failure classification.
OpenCode owns coding and sessions; Tailscale owns network identity and private
publication; Docker owns isolation; Git owns source history.

### Proposed Features

| Feature | Smallest useful form | Verdict |
|---|---|---|
| `fern attach` and origin | Separate bind address from canonical client origin; keep credentials in environment | P0 |
| Setup/resume | Bounded unprivileged scripts tied to container ID | P2 only after measured repeated need |
| `doctor --json` | Prefer narrow `status --json`; never claim live watcher health from stale host state | Defer |
| Private preview | One supervised, healthy, authorized service through existing private publication | Product pivot, not incremental feature |
| Artifact inbox | Bounded manifest/download inside a validated review workflow | Do not build independently |
| Source verification | Independently run declared checks and bind exact source/spec/artifacts | Research-only future product |
| Quiescence seal | Local unsigned record of stop authorization facts | P1 experiment after deployment |
| Signed GitHub events | Verified signature plus durable idempotent queue | Do not build without a recurring workflow |

### Product-Taste Reference Table

| Reference | Concrete design choice | Problem solved | Borrow | Avoid | Evidence |
|---|---|---|---|---|---|
| Tailscale | Named devices, MagicDNS, identity grants, private Serve, `status`/`netcheck` | Removes topology from first success without hiding diagnostics | One named private workspace and delegated identity | VPN, identity provider, tunnel implementation | [Quickstart](https://tailscale.com/docs/how-to/quickstart), [Serve](https://tailscale.com/docs/features/tailscale-serve), [grants](https://tailscale.com/docs/features/access-control/grants) |
| Vercel | Every deployment has an origin; immutable deployment identity differs from moving aliases; skew protection preserves generation continuity | Makes results direct and generations legible | Stable origin plus exact backend generation | Hosting platform and public-by-default result sharing | [URLs](https://vercel.com/docs/deployments/generated-urls), [skew protection](https://vercel.com/docs/skew-protection) |
| Cloudflare | Wrangler local/remote continuity, capability bindings, explicit secrets, Local Explorer | Makes broad infrastructure composable and diagnosable | Structured read-only lifecycle inspection | Primitive sprawl and agent-visible mutation tools | [Local development](https://developers.cloudflare.com/workers/development-testing/), [bindings](https://developers.cloudflare.com/workers/runtime-apis/bindings/) |
| Convex | One deployment identity, coherent dev flow, classified retry/error semantics | Removes synchronization and teaches failure ownership | Separate safe observations, idempotent reconciliation, and ambiguous effects | Dashboard, database, reactive framework, proprietary control plane | [Workflow](https://docs.convex.dev/understanding/workflow), [errors](https://docs.convex.dev/functions/error-handling/) |
| T3/Theo | Named audience, fewer initial choices, “solve problems,” risky technology at reversible boundaries | Reduces scope and makes tradeoffs teachable | State who should use plain OpenCode; isolate unstable V2 work | Trend-driven scope and anecdotal validation | [Introduction](https://create.t3.gg/en/introduction), [why](https://create.t3.gg/en/why) |

The first report's numeric taste scorecard is intentionally not repeated. The
observable interaction mechanics above are auditable; a 1-5 brand score is not.

## 7. Product Concepts

Weights reflect one part-time maintainer with no external validation:
usefulness 25%, clarity 20%, resilience to OpenCode V2 20%, implementation cost
15%, distinctiveness 10%, and operating burden 10%. Cost and burden are reverse
scored, where 5 is best.

| Concept | Clarity | Usefulness | V2 resilience | Low cost | Distinctive | Low burden | Weighted / 5 |
|---|---:|---:|---:|---:|---:|---:|---:|
| A. Fern One: private workspace appliance | 5 | 4 | 2 | 4 | 2 | 4 | **3.65** |
| B. Fern Flight Recorder: explainable lifecycle ledger | 4 | 4 | 4 | 4 | 4 | 4 | **4.00** |
| C. Fern Quiescence Seal: safe-to-stop evidence | 4 | 3 | 5 | 3 | 5 | 4 | **3.95** |
| D. Evidence-bound source verification workspace | 3 | 3 | 4 | 2 | 4 | 3 | 3.20 |

**A is the deliberately minimal product.** Finish installation, private origin,
wake, attach, recovery, stop, and removal. It is useful but vulnerable to V2.

**B is the recommended product depth.** Keep the appliance UX while recording
the last bounded set of transitions: operation ID, stable workspace, concrete
generation, old/new state, trigger, observation time, failure owner, and safe
next action. This remains useful even if OpenCode owns more lifecycle.

**C is the inventor experiment.** Record watcher epoch, last busy observation,
all-session-idle snapshot, held-request count, container/spec identity, stop
admission, and stop result. It answers “why did Fern believe stopping was safe?”
without asserting that code or the model output is correct.

**D is a different verification product.** It needs source snapshot semantics,
independent command execution, TOCTOU handling, persistence, artifact security,
and carefully limited trust claims. Do not smuggle it into the appliance.

The concepts are strategically distinct: A provides an outcome, B explains
operations, C records authorization evidence, and D verifies source state.

## 8. Differentiation Test

| # | Claim | Verdict | Strongest reason |
|---:|---|---|---|
| 1 | Better than plain OpenCode + Tailscale | **Unknown** | Real extra invariants, no comparative usage evidence |
| 2 | Container stop/wake creates enough value | **Partly supported** | Reclaims host resources, not host cost; value depends on contention |
| 3 | Attach is seamless locally/tailnet/phone | **Contradicted** | Listen-derived HTTP target cannot represent remote TLS origin; phone uses web |
| 4 | Hooks beat previews/notifications | **Unknown** | No repeated observed problem |
| 5 | State-bound verification is unsolved | **Partly supported** | No reviewed Amp exact-state contract; adjacent receipt/proof systems exist |
| 6 | Fern lifecycle makes freshness more trustworthy | **Partly supported** | True for lifecycle facts, not source/test correctness |
| 7 | OpenCode will not make Fern redundant | **Unknown** | V2 has internal workspace lifecycle and public service/client direction |
| 8 | One workspace is useful focus | **Partly supported** | Strong simplicity, but no parallel task isolation |
| 9 | Exact privacy segment exists | **Unknown** | Plausible, but no active-user evidence and many substitutes |
| 10 | Existing roadmap is feasible part-time | **Unsupported** | Too many new effects, state, tests, deployment, and publication tasks |
| 11 | Hooks/doctor beat CI/deployment/demo | **Contradicted** | Reproducible first success is currently absent |
| 12 | Fern beats a gateway project for Grab | **Partly supported** | Better general systems depth; weaker direct role evidence |

### Defensible Novelty

Fern can defend this claim after a small implementation:

> Fern records the independently observed admission and activity facts that
> authorized stopping a specific OpenCode container generation, and records
> whether the stop completed or remained unknown.

Fern cannot claim to prove code correctness, model correctness, absence of
unobserved direct-backend traffic, cryptographic integrity, reproducibility, or
safe mid-turn recovery. It should not call the record an attestation unless it
is signed by a defined trust root. “Quiescence seal” should remain descriptive,
local, and unsigned in version one.

The strongest evidence against this recommendation is V2's internal provider
lifecycle plus commodity persistent sandboxes. If OpenCode exposes a stable
provider API with equivalent all-session quiescence, or if users accept generic
timeout suspension, Fern's remaining value may be only educational.

## 9. Grab Evidence Matrix

| Job requirement | Fern evidence today | After recommended roadmap | Still missing | Honest framing |
|---|---|---|---|---|
| Go/concurrency | Manager admission, wake coalescing, epochs, locks, race tests | Public CI and measured operation | Production scale/on-call | Direct portfolio evidence, not production experience |
| Reverse proxy/SSE | Unbuffered proxy and authenticated SSE watcher | Remote demo and disconnect measurements | Provider protocol conformance | Direct transferable transport skill |
| Cancellation | Context-owned requests and lifecycle operations | Recorded disconnect outcomes | Provider token/billing cancellation | Direct composition skill; savings unproven |
| Auth/security | Basic auth forwarding, loopback backend, ownership checks | Tested private deployment and secret checks | Tenant authz, API-key lifecycle, OIDC | Transferable boundary design |
| Routing/fallback | Endpoint generation only | No change | Provider adapters, capability routing, fallback | Missing |
| Distributed quotas | In-process admission only | No change | Redis, tiers, fleet coordination | Missing |
| Usage/cost | Lifecycle timing only | Local resource ledger | Tokens, prices, chargeback, data lake | Missing; do not equate host metrics with LLM cost |
| Observability | Failure classes and structured values | Transition ledger and raw measurements | OTel, SLOs, fleet dashboards | Strong local design, no operations evidence |
| SQL/data | Preserves OpenCode SQLite volume | Recovery measurement | Owned schemas, PostgreSQL, migrations | Missing |
| Kubernetes/cloud | Docker API | Supervised single-host deployment | EKS, Helm, Terraform, cloud operations | Missing |
| Python/LangGraph | None | None | Direct implementation/evaluation | Missing |
| CI/CD | Local commands only | GitHub Actions, versioned release, runbook | Production pipeline | Direct portfolio evidence after roadmap |

For this exact role, the highest-signal separate two-week project is a small Go
gateway with two provider adapters, unified/OpenAI-compatible translation,
correct SSE and cancellation, Redis tenant/concurrency limits, PostgreSQL usage
and price-version events, OTel, Docker Compose, and failure tests. It should not
be added to Fern.

## 10. Revised Two-Week Roadmap

Assumption: 24-30 focused hours. Stop after P0 if time is exhausted.

| Priority | User outcome and boundary | Verification | Hours | Dependency | Stop condition | Deferred |
|---|---|---|---:|---|---|---|
| P0 | Independently checkable build: format, test, race, vet, binary and separate image jobs | Green GitHub Actions run from clean checkout | 3-4 | GitHub Actions | One green run | Matrix, coverage SaaS |
| P0 | One installed, supervised private workspace survives reboot and has removal/retention instructions | Fresh host/user runbook; reboot test | 5-6 | Docker host, tailnet | Reboot restores service | Installer framework, package repos |
| P0 | One canonical origin works for native web and terminal attach; bind and origin remain separate | Local and tailnet tests; secrets absent from argv/logs | 2-3 | Deployment | One documented command per client | Discovery, custom identity |
| P0 | Honest plain-OpenCode comparison | Same host/repo/provider; three sessions each from laptop and phone | 3-4 | Provider, phone | Record result even if Fern loses | Unrelated fixes |
| P0 | Ten authenticated wakes and resource/failure evidence | Raw ready times, median/range, memory before/after, persistence, disconnect, external exit/OOM | 4-5 | Running Docker daemon | Raw artifacts checked in | Benchmark service |
| P0 | 60-90 second tagged demo | Phone web wake, retained session, terminal attach, stop explanation | 3-4 | Earlier P0 | Viewer understands value and limit | Marketing site |
| P1 | Bounded lifecycle ledger exposed through `status --json` | Unit tests for transition ordering, stale/unknown behavior, and bounded retention | 3-4 | Deployment vocabulary | No stale observation is green/current | General doctor/dashboard |
| P1 | Experimental quiescence seal for the latest stop only | Test exact generation, epoch, all-idle result, held requests, spec/container, stop outcome | 2-3 | Ledger | Record never claims code correctness | Signatures, transparency log |
| P1 | Reconcile docs with measured behavior | Review README/architecture against tagged code and raw results | 1-2 | Final behavior | No future claim reads as shipped | Broad rewrite |
| P2 | Disposable V2 compatibility harness | Auth, client, event schema, active sessions, migration copy, clean/crash restart | 5-8 only after P0 | Installable V2 | Stop on unsupported quiescence or migration | Private SDK/provider |
| Do not build | Setup/resume hooks | Require repeated measured wake-readiness failure in 3 real repos/users | 0 | Validation | Explicit new decision | Cache/supervisor |
| Do not build | Preview/artifacts/source receipts/webhooks | Require repeated top-ranked friction and concept pivot | 0 | Validation | Explicit new decision | Public sharing, queue, policy |
| Do not build | Multi-workspace/cloud/Kubernetes/Redis/OIDC/gateway | Different customer and operating model | 0 | Organizational adopter | Explicit new product decision | Entire platform scope |

### Experiment Status and Procedures

On 2026-08-15 these commands passed at HEAD:

```bash
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local go test -race ./...
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go build ./cmd/fern
```

There is no `.github` CI, release tag, or checked-in daemon-backed Docker suite.
Docker CLI `28.0.1` is installed, but the daemon was unavailable, so image,
wake, memory, phone, OOM, and recreation experiments were not rerun. Use these
procedures; do not reuse the `2.8-3.1s` summary as a distribution:

| Experiment | Procedure | Decision change |
|---|---|---|
| Plain vs Fern | Same host/repo/provider, authenticated Tailscale Serve, three complete sessions each; count setup/reconnect/manual lifecycle steps and errors | If safe stop removes no recurring burden, freeze Fern |
| Attach auth | Test correct/wrong credentials locally and at canonical origin; inspect process list and logs; resume same session | Any secret leak blocks release; origin mismatch justifies model change |
| Phone wake x10 | Reach stopped state; from cellular request authenticated native web origin; record request, first byte, OpenCode-ready, and failures | Reliability below 99% or median above 5s weakens transparency claim |
| Memory | Record host memory and `docker stats --no-stream` active and 30s after stop for ten cycles | Negligible reclaim weakens shared-host value; never infer cost savings |
| Session recovery | Record session ID/messages; stop/wake; then `down`, recreate around backed-up volume, and reopen | Loss or corruption blocks release |
| Disconnect | Use deterministic long fake-provider turn; disconnect during and after; observe provider socket, OpenCode events/status, persistence, timer, reconnect | Determines whether “background” is supportable |
| Exit/OOM | Kill while idle/busy; separately use low memory; inspect Docker and `fern status`, then request wake | Misclassification or destructive auto-recovery blocks release |
| V2 replacement | Disposable V2 server and copied volume; test official auth/client/event/active/move/interrupt and clean/SIGKILL recovery | Migrate only if public APIs preserve conservative stop policy |

## 11. Decision Triggers

| Decision | Measurable trigger |
|---|---|
| Continue appliance direction | Weekly maintainer use for 6 weeks, 100 wakes at >=99%, material host resource reclaim, and fewer manual lifecycle actions than plain OpenCode |
| Continue as community tool | At least 5 unrelated weekly-active installs choosing Fern for safe lifecycle, not generic remote access |
| Pivot to previews | 3 active users repeatedly run app services and rank private result inspection first; manual Serve is insufficient |
| Pivot to source verification | 3 users show stale completion evidence causing rework and prefer deterministic source-bound checks over CI/receipt tools |
| Integrate V2 | Required APIs leave beta/experimental status, official auth/client and migration pass, and all-session quiescence is reproducible without private SDKs |
| Maintenance-only | Fewer than 3 unrelated monthly-active installs after 3 months, or OpenCode ships equivalent stable lifecycle |
| Abandon | No maintainer use for 8 weeks, unsupported/insecure V1 with no safe V2 seam, or the stop invariant cannot be maintained |
| Commercialize | 10 organizations active, 3 paying pilots/support customers, repeated unsolved organizational need, and explicit acceptance of security/support operations |

Stars, novelty, social interest, and interview relevance are not product
validation.

## 12. Source Appendix

All web sources accessed 2026-08-15.

### First-Party Documentation

- Amp: [manual](https://ampcode.com/manual), [Orbs](https://ampcode.com/manual/orbs),
  [OIDC](https://ampcode.com/manual/orbs/oidc),
  [plugin API](https://ampcode.com/manual/plugin-api),
  [SDK](https://ampcode.com/manual/sdk/typescript),
  [pricing](https://ampcode.com/pricing), [security](https://ampcode.com/security),
  [setup note](https://ampcode.com/notes/putting-an-agent-in-an-orb),
  [portals](https://ampcode.com/news/portals),
  [events](https://ampcode.com/news/event-driven-orbs),
  [mobile](https://ampcode.com/news/agents-everywhere),
  [multiplayer](https://ampcode.com/news/multiplayer), and
  [Puck](https://ampcode.com/news/meet-puck).
- OpenCode: [V1 server](https://opencode.ai/docs/server/),
  [V1 plugins](https://opencode.ai/docs/plugins/),
  [V2 overview](https://opencode.ai/v2/docs/),
  [V2 API](https://opencode.ai/v2/docs/api),
  [client](https://opencode.ai/v2/docs/build/client),
  [plugins](https://opencode.ai/v2/docs/build/plugins), and
  [migration](https://opencode.ai/v2/docs/migrate-v1).
- Taste references: [Tailscale](https://tailscale.com/docs/how-to/quickstart),
  [Vercel URLs](https://vercel.com/docs/deployments/generated-urls),
  [Cloudflare local development](https://developers.cloudflare.com/workers/development-testing/),
  [Convex workflow](https://docs.convex.dev/understanding/workflow), and
  [T3](https://create.t3.gg/en/introduction).

### Pinned Source

- Fern [`7c470d6`](https://github.com/nebler/fern/commit/7c470d6faad733e2d57848e77b66b3ae35c3169f).
- OpenCode V1 [`a3647eb`](https://github.com/anomalyco/opencode/commit/a3647eb025c7615159d417dcc49fc39fdaeba65b).
- OpenCode V2 [`3d782ef`](https://github.com/anomalyco/opencode/commit/3d782efee4c7724024deda362ca22390794dacf3).

### Issues and Pull Requests

- OpenCode workspace implementation [#41187](https://github.com/anomalyco/opencode/pull/41187),
  external/private provider injection [#42142](https://github.com/anomalyco/opencode/pull/42142),
  registry hardening [#42227](https://github.com/anomalyco/opencode/pull/42227),
  earlier provider proposal [#37437](https://github.com/anomalyco/opencode/pull/37437),
  provider request [#15752](https://github.com/anomalyco/opencode/issues/15752),
  runner request [#17434](https://github.com/anomalyco/opencode/issues/17434),
  and V2 service restart [#37239](https://github.com/anomalyco/opencode/issues/37239).

### Market and Pricing

- [Factory BYOM](https://docs.factory.ai/droid-computers/byom),
  [Cursor runtimes](https://cursor.com/docs/cloud-agent/choose-runtime),
  [Copilot cloud agent](https://docs.github.com/copilot/concepts/agents/cloud-agent/about-cloud-agent),
  [Codex environments](https://developers.openai.com/codex/cloud/environments),
  [Jules limits](https://jules.google/docs/usage-limits/),
  [Devin pricing](https://devin.ai/pricing),
  [Claude self-hosted](https://docs.anthropic.com/en/docs/claude-code/self-hosted-environments),
  [Coder](https://coder.com/pricing), [Ona](https://ona.com/pricing),
  [E2B](https://docs.e2b.dev/sandbox/persistence),
  [Daytona](https://www.daytona.io/docs/en/persistence/),
  [Vercel Sandbox](https://vercel.com/docs/sandbox/concepts/persistent-sandboxes),
  [Fly Sprites](https://docs.sprites.dev/concepts/lifecycle/),
  [Cloudflare Sandbox](https://developers.cloudflare.com/sandbox/),
  [Modal OpenCode](https://modal.com/docs/examples/opencode_server),
  [Runloop](https://docs.runloop.ai/devboxes/lifecycle),
  [Northflank](https://northflank.com/pricing), and
  [Netclode](https://github.com/angristan/netclode).

### Job-Role Material

- [Grab role](https://www.grab.careers/en/jobs/744000137791699/senior-software-engineer-backend-ai/),
  [AI Gateway](https://engineering.grab.com/grab-ai-gateway),
  [agent platform](https://engineering.grab.com/how-grab-builds-and-runs-ai-agents-at-scale),
  [Palana part 1](https://engineering.grab.com/palana-part-1-secure-platform-for-ai-agents),
  [Palana part 2](https://engineering.grab.com/part-2-palana-architecture), and
  [context cancellation](https://engineering.grab.com/context-deadlines-and-how-to-set-them).

### Third-Party and Adjacent Evidence

- [AI Integrity Receipts](https://github.com/invariant-systems-ai/aiir),
  [ProofShot](https://github.com/AmElmo/proofshot),
  [agent-receipts](https://github.com/0xelitesystem/agent-receipts), and
  [Cursor verification analysis](https://arize.com/blog/inside-cursors-agent-factory-how-it-verifies-ai-written-code/).
  These establish category activity, not adoption or correctness.

## 13. Fern Product Principles

| Principle | Roadmap choice supported | Feature ruled out |
|---|---|---|
| **Build only from facts Fern uniquely observes or transitions it controls.** | Lifecycle ledger and quiescence seal | Chat UI, source review, identity issuer |
| **Every mutation ends in a named terminal state or durable unknown.** | Record wake/stop operation outcome | Fire-and-forget wake and generic “offline” |
| **Stable workspace identity and concrete generation are separate.** | One private origin plus exact generation in status | Treating a moving endpoint as permanent identity |
| **Only authenticated intent wakes compute; observation does not mutate.** | Private origin and non-waking status/event inspection | Anonymous probes and public previews |
| **Retry policy follows side effects.** Observations may retry; reconciliation must be idempotent; ambiguous stop/start requires reinspection. | Preserve current stop reconciliation | Blind lifecycle retries |
| **Risk stays behind reversible boundaries and cannot own retained data.** | Disposable V2 harness | Fork/private provider API controlling the only session volume |
| **A feature requires a recorded repeated failure in the current journey.** | Evidence-first roadmap | Speculative hooks, previews, receipts, queues, providers |

## Final Decision

Fern earns the right to exist through one excellent experience:

> On a Docker host I already own, I install one supervised service for one
> repository and receive one private OpenCode origin. The native OpenCode web
> and terminal clients use that origin. An authenticated request wakes the
> retained workspace and reports the concrete generation that became ready. I
> disconnect without managing Docker. After Fern observes a fresh busy-to-all-
> idle boundary, closes admission, independently confirms every session idle,
> and stops the exact owned container, it leaves a small record explaining why
> that stop was authorized and whether it completed.

Fern is the correct layer because OpenCode cannot admit traffic while its own
process is stopped, Tailscale does not understand OpenCode activity, Docker does
not know session quiescence, and Git does not own runtime admission. Fern sits
at the only boundary that sees all four facts without asking the model.

The evidence that users value it is repeated successful wake/stop use, material
host resource reclamation, fewer manual lifecycle actions than plain OpenCode,
and users explicitly citing conservative shutdown as the reason Fern remains
installed. If that evidence does not appear, the correct outcome is a finished,
well-documented portfolio artifact, not a broader platform.
