# Fern Product Direction: Independent Research Review

**Research date: 2026-08-15**

> **Document status:** Dated report pinned to Fern `7c470d6`. Later commits
> added CI, release and deployment material, explicit client origins, the
> real-Docker lifecycle matrix, V2 compatibility, and additional hardening.
> Present-tense repository gaps below are historical; current authorities are
> indexed in [docs/DOCUMENTATION.md](./docs/DOCUMENTATION.md).

This report evaluates repository HEAD `7c470d6faad733e2d57848e77b66b3ae35c3169f`,
OpenCode V1 `1.18.16` at upstream commit
[`a3647eb`](https://github.com/anomalyco/opencode/commit/a3647eb025c7615159d417dcc49fc39fdaeba65b),
and OpenCode V2 at upstream commit
[`3d782ef`](https://github.com/anomalyco/opencode/commit/3d782efee4c7724024deda362ca22390794dacf3).
Product and pricing claims are snapshots on the research date.

## 1. Executive Decision

- Fern today is a well-tested Go lifecycle controller and stable reverse proxy
  for one durable OpenCode V1 Docker workspace. It conservatively stops the
  container after an observed busy-to-all-idle boundary and wakes it on a work
  request (`internal/workspace/manager.go:93-299`,
  `internal/watch/supervisor.go:86-188`).
- It solves a real but narrow resource-management problem on an existing shared
  or home Docker host. It has not yet proved that this problem is frequent or
  painful enough for users to adopt another daemon.
- It is not yet meaningfully better for basic remote access than authenticated
  `opencode serve` plus Tailscale. Its extra value is safe automatic stop/wake,
  ownership/drift checks, and failure classification, not remote coding itself.
- Container stop returns CPU and memory to the host; it does not stop the host
  VM or necessarily reduce the infrastructure bill. Cost saving is therefore
  not a defensible primary claim.
- Fern should remain a polished OSS portfolio project until repeated external
  use proves a community project. There is no evidence for pursuing a hosted
  business, enterprise open core, or managed cloud.
- Recommended concept: **a wake-ready, inspectable, private OpenCode
  workspace**, not an agent platform. One command should wake and attach to the
  native OpenCode interface; one status path should explain the exact lifecycle
  generation, private endpoint, and failure boundary.
- The end-to-end experience worth earning is: install on an existing Docker
  host, name one repository, receive one private URL, open it from a phone or
  run `fern attach`, transparently wake the persisted workspace, resume the
  OpenCode session, disconnect safely, and later observe an explained idle stop.
- Fern is the correct layer only for this experience because it uniquely owns
  request admission, Docker lifecycle, endpoint generations, and the safe-stop
  boundary. OpenCode should own chat and sessions; Tailscale identity and
  networking; Docker isolation; Git source state.
- The next two weeks should deliver CI, a reproducible install/deployment path,
  direct plain-OpenCode comparison, ten authenticated wakes, phone dogfooding,
  failure measurements, concise generation-aware status, and a short demo.
- Do not build hooks, previews, receipts, webhooks, multi-workspace scheduling,
  Kubernetes, Redis, OIDC issuance, an LLM gateway, or a fern UI until the basic
  workflow earns continued investment. The most important thing not to build
  is a company-sized hosted control plane.

## Research Passes

### Pass One: Amp From First-Party Sources

**Method.** Reviewed Amp's current manual, Orbs manual, plugin and SDK
references, pricing, security reference, announcements, and team notes. Current
manual behavior was preferred over launch posts. Private implementation was not
inferred from product language.

**Result.** Amp's unit is a thread placed at creation on local execution, a
fresh managed Orb, or a named live runner. Public documentation reviewed does
not describe moving an existing thread between executors. Amp provides a much
larger task loop than fern: prepared per-thread compute, setup/resume, portals,
artifacts, mobile review, durable webhooks, schedules, secrets, OIDC workload
identity, multiplayer, and Puck. It does not publicly document exact-state,
freshness-invalidating verification receipts. See [Amp manual](https://ampcode.com/manual),
[Orbs](https://ampcode.com/manual/orbs), [plugin API](https://ampcode.com/manual/plugin-api),
[TypeScript SDK](https://ampcode.com/manual/sdk/typescript), and
[security](https://ampcode.com/security).

### Pass Two: OpenCode Docs And Pinned Source

**Method.** Separated V1 `1.18.16` from V2 beta. Cross-checked public docs with
the two pinned commits above, including server, auth, event, plugin, location,
session-move, workspace-driver, and service-discovery source. Issues were used
only as instability or direction signals.

**Result.** Fern's V1 HTTP/attach boundary is supported. Contrary to the old
research's categorical wording, V1 contains an explicitly experimental plugin
workspace adaptor. V2 has a public beta client/server/location model and an
internal `WorkspaceDriver` with create/connect/suspend/destroy, but no reviewed
public third-party provider registration contract. V2 materially overlaps
fern's direction but does not yet supersede its independently conservative
Docker stop policy. See [V1 server docs](https://opencode.ai/docs/server/),
[V2 docs](https://opencode.ai/v2/docs/),
[migration guide](https://opencode.ai/v2/docs/migrate-v1), and
[V2 client docs](https://opencode.ai/v2/docs/build/client).

### Pass Three: Market And Economics

**Method.** Reviewed current first-party product, architecture, security,
self-hosting/BYOC, and pricing material across managed coding agents,
enterprise workspace control planes, and sandbox infrastructure. Prices were
treated as dated, non-comparable list prices; vendor adoption and performance
claims were treated as self-reported.

**Result.** Managed agents bundle compute with a complete task/review loop;
sandbox vendors sell programmable isolation and lifecycle; Coder/Ona sell
governance; open-source projects leave operations to users. A private,
single-tenant, existing-host OpenCode workspace is a credible OSS niche but not
a validated business segment. Netclode demonstrates technical overlap and
raises the bar with microVMs, mobile clients, previews, secret brokering, and
multiple harnesses ([Netclode](https://github.com/angristan/netclode)).

### Pass Four: Product Journey And Alternatives

**Method.** Walked discovery through removal, assigning user intent, required
knowledge, hidden state, failures, security boundaries, smallest improvement,
and delegated owner to each step. Evaluated the proposed features only where
fern has unique state or authority. Compared three coherent product concepts,
not feature bundles.

**Result.** The primary product defect is not missing hooks or receipts. It is
the absence of a demonstrated, reproducible first success. `fern attach` also
derives a local URL from `proxy.listen` and has no explicit external URL,
making local, Serve, and tailnet attachment less seamless than claimed
(`cmd/fern/attach.go:17-84`). Installation, supervision, release, private
deployment, phone use, and recovery remain undocumented or unproven.

### Pass Five: Product-Taste References

**Method.** Studied concrete current interactions in Tailscale, Vercel,
Cloudflare, Convex, and T3. Reputation and visual style were excluded as
evidence. Each lesson was tested against fern's ownership boundary.

**Result.** The transferable pattern is one named object, one direct operation,
secure defaults, and inspectable failure, not a dashboard or broader platform.
Fern should make a durable workspace legible while delegating adjacent systems.

### Grab Cross-Check

**Method.** Used the live first-party Grab role and current Grab Engineering
material. Gateway-specific evidence was separated from Grab-wide practice and
architectural inference.

**Result.** The live [Senior Software Engineer, Backend (AI)](https://www.grab.careers/en/jobs/744000137791699/senior-software-engineer-backend-ai/)
role, released 2026-07-15, is specifically for the GrabGPT Gateway. Fern
directly demonstrates transferable Go, streaming proxy, cancellation,
concurrency, health, and failure-state skills. It does not demonstrate the
role's defining multi-provider routing, Redis quotas, token/cost attribution,
PostgreSQL pipelines, Kubernetes, Python, or LangGraph requirements.

## 2. Corrections To Existing Research

### `AMP_PRODUCT_RESEARCH.md`

| Lines | Correction |
|---|---|
| 41-55 | The conclusion is ahead of validation. “Wake ready and ready to prove” is a concept, not the smallest evidenced direction. CI, deployment, measurement, and first-use UX should precede hooks, previews, artifacts, or receipts. |
| 64-81 | “Access from another device: yes” needs a caveat: fern provides no networking or tested remote deployment. External networking and an externally usable URL are prerequisites. |
| 101-103 | Too categorical. OpenCode V1 `1.18.16` has `experimental_workspace.register(type, adapter)` and experimental local/remote workspace routing ([source](https://github.com/anomalyco/opencode/blob/a3647eb025c7615159d417dcc49fc39fdaeba65b/packages/plugin/src/index.ts#L26-L63)). The correct claim is that it is not a dependable contract. |
| 114-117 | `fern attach` is only seamless for the configured listen-derived URL. It has no explicit external URL and wildcard addresses are rewritten to loopback (`cmd/fern/attach.go:47-65`). Tailnet and Tailscale Serve behavior is unproven. |
| 137-155 | Missing material V2 source evidence. V2 now has an internal `WorkspaceDriver` with `create`, `connect`, `suspendForIdle`, and `destroy`, plus private SDK injection ([driver](https://github.com/anomalyco/opencode/blob/3d782efee4c7724024deda362ca22390794dacf3/packages/core/src/workspace/driver.ts#L26-L68)). This strengthens redundancy risk but remains internal. |
| 159-184 | The document reports only two research passes and does not constitute a market, UX, reference-product, or source-pinned OpenCode review. Its verification search is useful but insufficient to select the product. |
| 190 | An Orb is created for an **Orb-backed** thread, not every Amp thread. Threads may execute locally or on runners. Executor selection is documented at creation; later relocation was not found in reviewed public docs. |
| 232-242 | `.amp/dev-ports.json`, `/__dev/preflight`, development-login patterns, and a known artifact directory are examples from Amp guidance/team notes, not all general platform guarantees. Separate recommended repository conventions from Amp-managed behavior. |
| 252-258 | Add current portal constraints: access follows thread visibility; authenticated requests wake the Orb; same-Orb hairpin bypasses portal auth; cross-portal browser CORS is unsupported. See [Portal access](https://ampcode.com/manual/orbs#portal-access). |
| 279-295 | Directionally correct but incomplete. Delivery is at least once; 202 means persisted/queued, not completed; handlers have 30 seconds; burst is 10 with 10/min refill; queue cap is 100; body cap is 1 MB; sender idempotency and plugin signature verification are caller/plugin responsibilities. See [webhooks](https://ampcode.com/manual/orbs#webhooks). |
| 304-306 | Amp OIDC additionally has RS256, issuer and audience validation requirements and a default ten-minute life. It binds Amp identities, not source, environment, or artifact digests. See [OIDC](https://ampcode.com/manual/orbs/oidc). |
| 348-369 | The comparison obscures operating models. Amp's workspace is fresh per Orb thread; fern's is one shared durable checkout. “State-bound evidence freshness: no” for remote OpenCode is a reviewed-doc finding, not proof of absence in extensions or private systems. |
| 434-533 | The five-part “lowest-cost coherent feature set” is not low cost: it adds arbitrary command execution, persistent hook state, process supervision, routing, file serving, authentication, and content-safety concerns. It should not precede proof that stop/wake is valued. |
| 558-620 | Fern does not yet have a durable, externally meaningful “request generation.” Endpoint generations are in-memory routing correctness, and lifecycle intent is persisted against a container ID. The proposed receipt identity and independent verifier require new design and execution authority. |
| 635-639 | “Safe scale-to-zero” overstates behavior. Fern stops a container, not the host VM. The narrower possible novelty is freshness invalidation tied to fern-observed container/spec/source state. No user need is validated. |
| 683-696 | The ranking mixes product value, portfolio signal, and Grab keyword overlap. CI/release/dogfooding are evidence work; hooks/previews are product bets; gateway features are another product. They should not share one score without weights. |
| 748, 771-774 | Cancellation is directly demonstrated in internal Go composition and proxy request contexts, but there is no measured provider-token saving or remote production operation. Frame it as transferable systems skill. |
| 816-818 | “8.5/10 CV project” is unsupported precision. Hooks and previews do not close the role's defining routing, quota, accounting, SQL, or Kubernetes gaps. |
| 830-835 | The repository has no checked-in real-Docker integration suite. The 2.8-3.1 second measurements are summaries without raw ten-sample evidence. Do not claim “Docker integration tests” or a representative latency distribution. |
| 839-841 | Explicitly future-only; none of these features is implemented. |
| 968-971 | This product statement claims future behavior (“known-good,” evidence, previews) as if current. Current fern wakes a conservatively managed OpenCode process; environment readiness and proof are absent. |

### `ROADMAP.md`

| Lines | Correction |
|---|---|
| 7-17, 53-71 | The definition of done combines repository credibility, deployment, comparative research, hooks, diagnostics, phone use, measurement, installation, and demo. This is not a defensible part-time two-week commitment without prior deployment infrastructure. |
| 44-49 | Setup/resume and `doctor` are ranked before the baseline has proved repeated pain. They should be P2/conditional; CI, deployment, measurement, demo, and install are P0. |
| 55-56 | No CI exists today. The Docker image build could not be locally revalidated because the Docker daemon was unavailable. |
| 58-60 | A phone cannot “attach” with the terminal TUI unless a phone terminal is assumed. Distinguish opening OpenCode web from terminal `fern attach`. |
| 75-96 | The clean Go baseline passes at HEAD, but Docker and image versions remain unrecorded in this environment. The image build is a separate unresolved gate. |
| 117-156 | Correctly makes Tailscale external, but assumes deployment and URL representation can be resolved in one session. Current attach URL derivation is a known product gap, not only an optional dogfood discovery. |
| 177-225 | This is a new remote-code-execution surface with ownership races, output retention, timeout/kill semantics, and real-Docker tests. One part-time session is not credible. |
| 229-286 | Hook generation semantics require persistent state, typed runtime changes, rollback, diagnostics, and concurrency validation. Four sessions are optimistic and introduce substantial attack surface before demand. |
| 288-324 | `doctor` depends on daemon/watch state that standalone CLI invocations cannot necessarily observe. The ownership and transport for live watcher state must be designed first. |
| 326-345 | “Docker suites” do not currently exist as checked-in daemon-backed tests. The failure matrix is valuable future work, not an available final verification step. |
| 347-382 | Installation, release, demo, measurement publication, and cleanup are compressed after all feature work. These are the actual P0 product evidence and need most of the second week. |
| 403-405 | Preview and verification pivots need observed repeated problems, not one dogfood impression or generic distrust. Setup-cache work should require measured setup latency across repositories. |

## 3. Amp Cross-Check

### Current Capability Map

| Area | Current documented behavior | Fern lesson |
|---|---|---|
| Placement | Executor is selected when a thread is created: local, Orb, or named runner. No reviewed public relocation operation was found. | Do not add placement abstractions to one workspace. |
| Orbs | Fresh Debian machine per Orb-backed thread, repository clone, 40 GB disk, tools, authenticated `gh`/`amp`; E2B supplies ephemeral compute. | Fresh task isolation is a core Amp advantage fern does not have. |
| Projects/workspaces/defaults | Projects are optional and hold repository, Orb, secret, and changes settings; “No Project” Orb threads are allowed. Local Git remotes can infer a project. Projects have a default Orb size; a workspace can default new projects. Personal Megawatt projects default to `a1.small`; a universal non-Megawatt default was not found. Workspaces are collaboration/billing scopes, not executors. SDK visibility defaults to `workspace`, interpreted in account context. | Keep fern's one workspace identity distinct from execution placement and avoid hidden defaults. |
| Sizes/pricing | `a1.tiny` 1 CPU/2 GB $0.08/h; small 2/4 $0.17; medium 4/8 $0.33; large 8/16 $0.66; xxlarge 16/32 $1.32. Enterprise rates are 50% higher. Minute billing; paused is free. [Pricing](https://ampcode.com/manual/orbs#pricing), accessed 2026-08-15. | Never compare stopped-container resource release with Amp's zero Orb runtime billing. |
| Subscriptions | Megawatt is $20/month with 750 `a1.small` hours and $20 agent use; Gigawatt is $200/month with 1,000 `a1.xxlarge` hours and $200 agent use. PAYG can continue usage. Public docs reviewed do not explain cross-size quota normalization. | Bundled economics make raw infrastructure-price competition unattractive. |
| Pause/wake | Auto-pause after five idle minutes; thread work, authenticated portal requests, webhooks, and schedules wake. | Wake is one part of a larger task loop. |
| Setup/resume | `.agents/setup` prepares fresh state. A first-party note says the result may be snapshotted/reused up to 24 hours, but current manual does not make caching a stable contract. `.agents/resume` blocks for at most ten seconds; late execution continues in background; early non-zero failure is surfaced. | If hooks are later built, specify stricter timeout and failure semantics rather than copying ambiguity. |
| Portals | `.amp/services.yaml`, supervised services, sticky ports, readiness, authenticated thread-scoped HTTPS, wake, same-Orb hairpin, private routing. | A preview is valuable only with supervision, access, health, wake, and routing, making it non-trivial. |
| Artifacts/review | Inputs, uploaded files, `.amp/in/artifacts`, screenshots/video, diffs, live portals, annotations, terminal and mobile review. | “Artifact inbox” without review continuity is a weak copy. |
| Webhooks | Durable capability URL, persistence before 202, at-least-once, event IDs, sender idempotency, retry, limits, 30-second handlers. Plugin validates sender signatures and untrusted payload. | Signed GitHub ingestion requires a durable queue and idempotency, not merely an HTTP handler. |
| Identity/secrets | Scoped secrets and short-lived Orb OIDC with workspace/project/user/thread claims. | Generic OIDC is unjustified for one user; tailnet identity is proportional. |
| Collaboration | Web/mobile, remote control, workspace threads, time-bounded multiplayer, Slack, and Puck coordination. | Preserve OpenCode's clients; a fern meta-agent has nothing useful to route. |
| Verification | Rich audit context and agent-produced tests, screenshots, video, and live previews. No public exact source/environment digest or automatic stale-evidence invalidation found. | A narrow research gap exists, not validated differentiation. |

Amp's product is a task, environment, evidence, and review system. Fern should
borrow direct outcomes and legible state, not recreate Amp's scheduler, Orb
fleet, portal edge, queue, identity issuer, collaboration model, or Puck.

## 4. OpenCode Integration Assessment

### V1/V2 Matrix

| Surface | V1 `1.18.16` | V2 `3d782ef` | Classification |
|---|---|---|---|
| `serve`, attach, Basic auth | Public docs and pinned implementation | Explicit server/client connection, beta | V1 Supported; V2 Beta |
| Events | `/event` SSE, heartbeat and location filtering | `/api/event` async iterable and changed schemas | V1 Supported; V2 Beta |
| Persistence | Global SQLite/WAL in data directory | SQLite plus V1 migration and bounded startup recovery | Behavior Supported/Beta; storage internals Internal |
| Web/mobile | Public web server | Beta client/server product | V1 Supported; V2 Beta |
| Plugin context | Project/directory/worktree/client/shell/events/tools | In-process capability subset | V1 Supported; V2 Beta |
| Plugin session placement | Experimental adaptor can return local directory or remote URL | Plugin may select a location on creation but cannot register provisioning or call normal `move` | V1 Experimental; V2 unsupported for providers |
| Location/worktree/PTY/shell/filesystem | Mixed supported and experimental control-plane surfaces | Public beta location-scoped APIs | V2 Beta |
| Session move | Experimental workspace warp | Public beta move at an execution boundary | V1 Experimental; V2 Beta |
| Workspace provider | `experimental_workspace` and experimental routes | `WorkspaceDriver` registry; private SDK injection | V1 Experimental; V2 Internal |
| Third-party provider API | No stable contract | Not found in reviewed public docs/API/plugins | Not Supported; future availability Planned/Unknown |
| Distributed placement/fencing | Not provided | Explicitly absent from recovery semantics | Planned/Internal direction at most |
| General published embedded SDK | V1 SDK exists | Regular SDK “coming soon”; Effect SDK private/beta | V1 Supported; V2 Planned/Internal |

Relevant pinned source:

- V1 [`serve`](https://github.com/anomalyco/opencode/blob/a3647eb025c7615159d417dcc49fc39fdaeba65b/packages/opencode/src/cli/cmd/serve.ts#L6-L22),
  [`attach`](https://github.com/anomalyco/opencode/blob/a3647eb025c7615159d417dcc49fc39fdaeba65b/packages/opencode/src/cli/cmd/attach.ts#L7-L61),
  [auth](https://github.com/anomalyco/opencode/blob/a3647eb025c7615159d417dcc49fc39fdaeba65b/packages/opencode/src/server/auth.ts#L17-L47),
  and [SSE](https://github.com/anomalyco/opencode/blob/a3647eb025c7615159d417dcc49fc39fdaeba65b/packages/opencode/src/server/routes/instance/httpapi/handlers/event.ts#L25-L85).
- V2 [service discovery](https://github.com/anomalyco/opencode/blob/3d782efee4c7724024deda362ca22390794dacf3/packages/client/src/promise/service.ts#L17-L127),
  [locations](https://github.com/anomalyco/opencode/blob/3d782efee4c7724024deda362ca22390794dacf3/packages/core/src/location-services.ts#L57-L105),
  [session move](https://github.com/anomalyco/opencode/blob/3d782efee4c7724024deda362ca22390794dacf3/packages/core/src/session.ts#L745-L794),
  and [workspace driver](https://github.com/anomalyco/opencode/blob/3d782efee4c7724024deda362ca22390794dacf3/packages/core/src/workspace/driver.ts#L26-L70).
- Open issues such as [V2 service restart #37239](https://github.com/anomalyco/opencode/issues/37239)
  are instability evidence, not universal behavior or a contract.

**Recommendation:** keep the V1 attach wrapper and network boundary now. Build
only a disposable V2 compatibility harness against documented HTTP/client APIs.
Do not use the V1 experimental adaptor, V2 private SDK/driver, or a fork. Migrate
when V2 is stable and a conservative all-session idle boundary, authenticated
attach, and V1 data migration pass reproducible tests. Contribute an upstream
adaptor only after OpenCode publishes a provider contract and fern has users
requiring it.

## 5. Market Map

### Compact Product Comparison

This table records the operating facts that affect fern's decision. “Proof”
means the review evidence a product exposes, not a cryptographic guarantee.

| Product | Unit/isolation and lifecycle | Clients, setup, previews/proof | Hosting, dated price, security/extensibility | Fern overlap |
|---|---|---|---|---|
| Amp Orbs | Thread; fresh managed machine; five-minute pause, wake on work/portal/event; minute billing | CLI/web/mobile/Slack; setup/resume; portals, diffs, screenshots/video | Managed only; $0.08-$1.32/h list plus $20/$200 bundles; secrets/OIDC/plugins | Complete managed version of the broad roadmap |
| OpenCode V2 | Session/location in a background service; worktrees and bounded recovery; beta | CLI/TUI/web/client/API; plugins, PTY/shell/filesystem; no managed proof layer | Open source/self-host; provider-neutral; public provider registration not found | Substrate and largest redundancy risk |
| Cursor | Agent run in dedicated Firecracker VM; snapshots and long tasks | IDE/web/API/GitHub/Slack; setup; desktop/browser, screenshots/video, PR | Managed plus enterprise self-hosted pools; $20-$200 individual, $40/$120 team; OIDC/private networking | Customer-controlled compute plus much better review UX |
| Copilot coding agent | One repo/branch session in ephemeral Actions; 59-minute cap | GitHub/web/IDE/mobile/integrations; setup workflow; commits/logs/checks/PR | Managed or self-hosted ephemeral Actions runners; $10-$39 individual, $19/$39 business/enterprise plus Actions | GitHub-native bounded task alternative |
| OpenAI Codex | Cloud task/container/branch; cached state up to 12 hours | Web/CLI/IDE/desktop/mobile/integrations; setup/maintenance; logs/diff/PR | Managed cloud; included plans plus credits; OpenAI estimates $100-$200/developer/month average | Complete cloud-agent substitute; no BYOC found |
| Google Jules | GitHub repo task in short-lived Ubuntu VM; setup snapshot | Web/API/CLI; plan approval, diff and PR | Managed; free 15 tasks/day, AI Pro 100, Ultra 300; no BYOC found | Low-friction managed task alternative |
| Devin | Long-running session with IDE/shell/browser | Web/desktop/terminal/API/integrations; takeover, browser and PR evidence | Managed; dedicated/VPC enterprise; free/$20/$200, teams from $80 + seats | Broad autonomous-work product |
| Factory | Session on persistent auto-pausing Droid Computer or BYOM host | Desktop/CLI/web/mobile/API; SSH, VS Code, port relay, PR/diff | Managed/BYOM/on-prem/air-gap; $20/$100/$200, enterprise custom; rich extensions | Closest polished customer-host alternative |
| Claude Code cloud | Persistent isolated VM session; re-provision from conversation history | Terminal/IDE/desktop/web/mobile/Slack/GitHub; diffs/PR and teleport | Managed plus enterprise self-hosted environment; $17-$200 consumer plans | Harness/client alternative on customer compute |
| Augment | Cloud session/automation; up to 50 concurrent on current Cosmos plan | CLI/chat/review/automations; context engine and MCP | Managed; $100/month pooled base plus usage; enterprise controls | Large-repo managed alternative |
| Replit Agent | Project task/checkpoint on integrated workspace/runtime | Browser IDE, app preview, databases, rollback and deploy | Managed only; $20/$100 with credits | Strong greenfield preview/deploy substitute |
| Coder | Persistent Terraform-defined workspace on customer VM/K8s/container/host; auto-stop | Any IDE/SSH/web/API; templates; human/agent shared environment | Fully self-hosted; AGPL free, premium quote; RBAC/audit/air-gap/model choice | Enterprise-grade superset of infrastructure ownership |
| Ona/Gitpod | Ephemeral CDE/session or agent fleet; prebuilds; inactive cleanup | IDE/browser/API; Dev Containers, previews and automations | Managed or customer VPC; from $20/month/OCU; SSO/audit/policy | Team control plane, far broader than fern |
| E2B | Isolated sandbox VM; 1-hour Hobby/24-hour Pro; per-second active | SDK/files/commands/desktop; templates, no finished PR UX | OSS architecture, self-host/BYOC enterprise; Pro $150/month + use | Infrastructure fern could use but should not recreate |
| Daytona | Dedicated-kernel sandbox/container/VM; archive or memory-preserving pause | SDK/API/CLI, Git/LSP/PTY/SSH/VNC/previews, snapshots/volumes | PAYG; enterprise BYOC/Kubernetes; exact rendered rate not verified | Rich open sandbox/control primitive |
| Modal | Container sandbox; default five minutes, up to 24 hours; snapshots | Python/JS/Go, files, tunnels, GPU; official OpenCode example | Managed only; $30 free credit, Team $250 + use | Straightforward hosted OpenCode substrate |
| Cloudflare Sandbox | Per-ID container on Workers/DO; scale-to-zero timeout | TypeScript SDK, terminal/files/processes/preview URLs/storage | Managed; $5 Workers Paid + resource billing; programmable egress | Serverless sandbox alternative with platform coupling |
| Vercel Sandbox | Firecracker microVM; persistent save/resume, snapshots, up to 24h | JS/Python/CLI, Docker, drives, multi-agent users, live previews | Managed only; CPU $0.128/h + memory/storage | Polished persistence/preview primitive |
| Runloop | MicroVM Devbox; snapshot and suspend to storage-only state | SDK/API/CLI, browser, event streams, evaluations | Managed/VPC enterprise; Pro $250 + use | Agent sandbox with stronger evaluation story |
| Fly Sprites | Persistent microVM/computer; hibernate and object-store restore | CLI, checkpoints, services, URL, connectors | Managed; per-second price documented but numeric rate not retrieved | Very simple persistent-computer competitor |
| Northflank | General service/job/container and preview environment | UI/API/CLI, GitOps, services, DB/GPU, logs/metrics | Managed or BYOC; no PAYG minimum, 2 CPU/4 GB $0.0667/h | Broader app environment, less agent-specific |
| Netclode | Persistent session on Kata/Cloud Hypervisor microVM; warm pool; compute deletion with JuiceFS state | iOS/macOS/CLI, terminal, Git diff, bot, Tailscale previews; multiple harnesses | OSS self-host; own k3s/S3/Redis/Tailscale/model costs; secret proxy | Direct open-source overlap with stronger mobile/isolation scope |

Google's August 2026 material also names **Antigravity** as a multi-agent
development platform bundled with AI plans. Reviewed public detail was
insufficient to classify its isolation, lifecycle, BYOC, and unit economics, so
it is a watch item rather than evidence used in the decision.

### Managed Coding-Agent Products

- **Amp, Cursor Cloud Agents, GitHub Copilot coding agent, Codex cloud, Jules,
  Devin, Factory, Claude Code cloud, Augment, and Replit** sell a task/thread or
  PR outcome. They generally provide fresh or dedicated managed environments,
  Git integration, review evidence, and bundled model/compute economics.
- Cursor provides dedicated Firecracker VMs, setup/snapshots, browser/computer
  use, screenshots/video, PRs, and an enterprise self-hosted pool
  ([Cloud Agents](https://cursor.com/docs/cloud-agent)).
- Copilot uses an ephemeral GitHub Actions environment, one repository/branch,
  a 59-minute session cap, setup workflows, and PR/check evidence
  ([cloud agent](https://docs.github.com/copilot/concepts/agents/cloud-agent/about-cloud-agent)).
- Codex uses isolated managed containers and task branches, with setup network
  access and an offline-by-default agent phase; it spans web, CLI, IDE,
  desktop/mobile, and integrations. OpenAI reports an approximate average of
  $100-$200/developer/month, a vendor estimate, not independent economics
  ([rate card](https://help.openai.com/en/articles/20001106-codex-rate-card)).
- Jules uses short-lived GitHub-repository VMs, setup snapshots, plan approval,
  code review, and PRs; free and consumer AI plan quotas are its visible pricing
  model ([limits](https://jules.google/docs/usage-limits/)).
- Factory is especially overlapping through persistent auto-pausing Droid
  Computers and BYOM hosts, plus desktop, CLI, web/mobile, SSH, previews, and
  enterprise air-gap options ([pricing](https://www.factory.ai/pricing)).

Fern cannot compete with these products on complete task UX, parallelism,
review, or bundled economics. Its advantage can only be private ownership,
minimal infrastructure, and OpenCode interoperability.

### Enterprise Workspace And Agent Control Planes

- **Coder** is self-hosted, Terraform-defined workspace infrastructure spanning
  VMs, Kubernetes, containers, SSH/IDE/desktop, policy, audit, model choice, and
  agent controls. Its AGPL community edition is free
  ([Coder docs](https://coder.com/docs)).
- **Ona/Gitpod** combines managed or customer-VPC development environments,
  agents, prebuilds, Dev Containers, automations, identity, governance, and
  enterprise controls ([pricing](https://ona.com/pricing)).

These products target platform teams and governance budgets. Fern should not
move toward enterprise control-plane scope without organizational adopters.

### Sandbox Infrastructure

- **E2B, Daytona, Modal, Cloudflare Sandbox, Vercel Sandbox, Runloop, Fly
  Sprites, and Northflank** sell sandboxes, VMs, or containers through APIs.
  Common primitives are snapshots, pause/hibernate, volumes, previews,
  terminals, secret handling, egress policy, and observability.
- Dated public list examples: E2B Pro has a $150/month platform fee plus usage
  ([pricing](https://e2b.dev/pricing)); Modal Team is $250/month plus compute
  ([pricing](https://modal.com/pricing)); Cloudflare Sandbox requires Workers
  Paid and bills container resources
  ([pricing](https://developers.cloudflare.com/containers/pricing/)); Vercel
  bills active CPU at $0.128/hour and memory at $0.0212/GB-hour
  ([pricing](https://vercel.com/docs/sandbox/pricing)); Runloop Pro is
  $250/month plus usage ([pricing](https://www.runloop.ai/pricing)); Northflank
  advertises no PAYG minimum and a 2-vCPU/4-GB plan at $0.0667/hour
  ([pricing](https://northflank.com/pricing)).
- These figures are not comparable: isolation, active-CPU accounting, storage,
  subscription minimums, credits, regions, and agent features differ.

Fern should not become a sandbox API. Existing vendors and Docker already
provide the execution primitive; fern's possible value is the OpenCode-specific
lifecycle experience above it.

### Segment And Business Assessment

| Outcome | Assessment | Evidence threshold |
|---|---|---|
| Polished OSS portfolio | Credible now after CI, deployment, measurements, and demo | One reproducible release and honest operational record |
| Useful community project | Possible, unvalidated | At least 5 unrelated active installations and recurring issues/PRs |
| Support/sponsorship | Possible but unlikely near term | Repeated users asking for installation/upgrade support |
| Small sustainable business | Unsupported | Paying users with a pain not solved by Factory BYOM, Coder, Netclode, or plain OpenCode |
| Enterprise open core | Contradicted by current scope/capacity | Organizational deployment, governance needs, and willingness to fund operations |
| Managed multi-tenant cloud | Do not pursue | Requires isolation, identity, billing, abuse control, support, regions, SRE, and a complete task UX |

The underserved-segment statement is plausible but unvalidated: a user who
already owns an intermittently constrained Docker host, wants one private
durable OpenCode workspace, values automatic safe stop more than simplicity,
and does not want Factory/Coder/Netclode. The conjunction may be too narrow.

## 6. User Journey And Product Critique

### Journey

| Step and intent | Current friction, ambiguity, risk | Smallest improvement | Delegate |
|---|---|---|---|
| Discover: decide whether fern is needed | Future features obscure today's narrow value; no comparison demo | State “use plain OpenCode if always-on is acceptable”; show one measured stop/wake workflow | OpenCode docs for coding features |
| Install/remove | No release artifact, installer, service unit, upgrade/removal or data-retention guide | One versioned binary release and one tested host runbook, including retained volume removal | Package manager/service manager |
| Connect repository/configure credentials | Manual image build/YAML/env knowledge; secret ownership is unclear | Validate one minimal config, print missing variable names only, document host/container/provider boundaries | Git, environment/secret manager |
| Start and attach | `up` is foreground; `attach` assumes local listen-derived URL; daemon/network state is hidden | One supervised deployment path; explicit external URL only if required; bounded wake/ready phases | Tailscale networking; OpenCode TUI/web |
| Send/disconnect | OpenCode owns the turn; user cannot easily know whether disconnect affected work | Document observed V1 behavior and show fern request/activity state without claiming mid-turn recovery | OpenCode session semantics |
| Return from phone | No tested phone route, concise status, notification, or proof view | Open the native private OpenCode web URL; show latest fern state in logs/CLI, not a new UI | OpenCode web, Tailscale Serve |
| Recover | Strong internal classification is not yet translated into install-level actions | Preserve failure category and provide one safe remediation command/runbook section | Docker for low-level inspection |
| Sleep | Strongest implemented moment; conservative all-session idle stop | Measure it and expose observed-at time/generation | Fern owns this |
| Upgrade/remove | Pinned V1 and volume migration risks are not operationalized | Backup, upgrade, rollback, and retained-data instructions | Docker volumes, OpenCode migration |

### Five Highest-Friction Moments

1. Getting a reproducible installed, supervised process from the repository.
2. Knowing which local, tailnet, or Tailscale Serve URL `attach` and web should use.
3. Understanding whether authentication and secrets are correctly applied without leakage.
4. Returning after a disconnect and distinguishing running work, idle, paused, and failed state.
5. Recovering or upgrading without losing the retained OpenCode volume or misclassifying an external exit.

The strongest current moment is the pause admission sequence: fern blocks new
requests, serializes lifecycle work, re-inspects Docker, checks authoritative
session status, and refuses to stop unless every session is idle
(`internal/workspace/manager.go:251-299`, `:389-419`).

### Proposed Feature Evaluation

| Feature | Problem and unique authority | Minimum version | Cost/attack surface | Decision |
|---|---|---|---|---|
| `fern attach` + remote URL | Current local/external URL ambiguity; fern owns ingress address but Tailscale owns reachability | Preserve native client; add explicit URL only after deployment reproduces need; never put secrets in argv | URL validation, auth forwarding, confused endpoint | P0 dogfood/fix only if reproduced |
| `.fern/setup` | Fresh container may lack dependencies; Docker can execute, repository can script | One bounded unprivileged script tied to immutable container ID | Arbitrary code, timeout/kill, logs/secrets, rollback, generation state | P2, not in two weeks |
| `.fern/resume` | Services may not survive stop; fern knows actual resume transition | One fast bounded idempotent script, no background continuation | Same as setup plus every-wake latency | P2 after measured need |
| `doctor --json` | Failures are hidden in Docker/logs; fern knows ownership and runtime observations | Static config/runtime checks first; versioned JSON | Live-daemon state transport and misleading stale green state | P1 only after deployment; do not expose to model by default |
| Private preview | App results are hard to inspect from phone; fern can wake-route but not provide identity | One localhost service routed through Tailscale Serve | Process supervision, routing, health, auth, path/CORS | Conditional pivot after repeated preview friction |
| Artifact inbox | Agent outputs are scattered; fern has no unique need to store files | Bounded local manifest/download only | Traversal, symlink, MIME, retention, sensitive data | Do not build before preview users request it |
| Freshness receipt | Old checks can be mistaken for current; fern observes runtime lifecycle but not yet exact source execution | Manual declared command, source/spec hashes, EXACT/STALE/UNKNOWN | Command execution, TOCTOU, trust claims, persistent schema | Research only after observed distrust |
| Signed GitHub ingestion | Events should wake sleeping work | Verify signature, durable queue, idempotency, untrusted payload separation | Public ingress, replay, SSRF/prompt injection, queue operations | Do not build without a real recurring workflow |

Explicit exclusions apply across all versions: no public anonymous sharing,
generic supervisor, dependency graph, secret vault, natural-language control
action, arbitrary repository browser, policy engine, trust score, auto-merge, or
hosted queue.

### Product-Taste Reference Table

| Reference | Concrete design choice | User problem solved | Borrow | Avoid | Evidence |
|---|---|---|---|---|---|
| Tailscale | Identity login, named devices/MagicDNS, private Serve, `status`/`netcheck`/`bugreport` | Removes network topology from first success while preserving diagnostics | One named workspace/private URL; hide routing until failure | Building VPN, identity provider, tunnel, or claiming tailnet policy | [Quickstart](https://tailscale.com/docs/how-to/quickstart), [Serve](https://tailscale.com/docs/features/tailscale-serve) |
| Vercel | Git/framework inference, every deployment gets a URL, immutable commit versus moving branch URLs, retained failure logs | Turns source action into inspectable result and cheap recovery | Return one private endpoint; distinguish stable URL from generation-bound evidence | Public-by-default URLs, hosting platform, hidden platform coupling | [Git](https://vercel.com/docs/git), [URLs](https://vercel.com/docs/deployments/generated-urls) |
| Cloudflare | Wrangler local/remote continuity, capability bindings, explicit secrets, structured logs, separate public tunnel | Composes infrastructure while preserving target and authority | Structured lifecycle operation IDs/generations and explicit exposure | Primitive sprawl, Workers/DO/Sandbox stack, fern secret store | [Local development](https://developers.cloudflare.com/workers/local-development/), [bindings](https://developers.cloudflare.com/workers/runtime-apis/bindings/) |
| Convex | One `dev` flow, coherent deployment identity, generated contracts, reactive state, classified errors/request IDs | Makes durable state and transitions one model | One workspace noun; errors identify layer and next action | Dashboard, database, reactive framework, generated app SDK, proprietary control plane | [Quickstart](https://docs.convex.dev/quickstart/react), [errors](https://docs.convex.dev/functions/error-handling/) |
| T3/Theo | Explicit audience, bounded scaffold choices, “solve problems,” reversible bleeding edge, stated exclusions | Reduces first-run choice and roadmap sprawl | Say who should use plain OpenCode; add config only after reproduced failure | Trend/personality-led scope and anecdotal validation | [Introduction](https://create.t3.gg/en/introduction), [Why](https://create.t3.gg/en/why) |

### Product-Taste Scorecard

Scores are 1-5 and cover only observed product behavior. Fern is scored today,
not against planned features. T3 is a design/communication lens, so operational
dimensions are not scored.

| Product/lens | First success | State legibility | Secure default | Recovery | Direct result | Scope discipline |
|---|---:|---:|---:|---:|---:|---:|
| Fern today | 1 | 2 | 4 | 3 | 2 | 5 |
| Amp | 4 | 4 | 4 | 4 | 5 | 3 |
| Tailscale | 5 | 5 | 5 | 5 | 4 | 5 |
| Vercel | 5 | 4 | 2 | 5 | 5 | 3 |
| Cloudflare | 3 | 4 | 4 | 4 | 4 | 2 |
| Convex | 5 | 5 | 4 | 5 | 4 | 4 |
| T3 lens | 5 | N/A | N/A | 3 | 4 | 5 |

Fern's secure-default score reflects loopback default and required password on
non-loopback binds (`internal/config/config.go:423-442`). Its low state score
reflects that strong internal state is not yet a cohesive user-facing product.

## 7. Product Concepts

Weights reflect the current constraint: one part-time maintainer and no market
validation. Usefulness 25%, clarity 20%, resilience to OpenCode V2 20%,
implementation cost 15%, distinctiveness 10%, operational burden 10%. Scores
are 1-5; cost and burden are reverse-scored, where 5 is cheap/light.

| Concept | Clarity 20 | Usefulness 25 | V2 resilience 20 | Low cost 15 | Distinctive 10 | Low burden 10 | Weighted / 5 |
|---|---:|---:|---:|---:|---:|---:|---:|
| A. Lifecycle + attach wrapper only | 5 | 3 | 1 | 5 | 2 | 5 | 3.55 |
| B. Wake-ready, inspectable private workspace | 5 | 4 | 3 | 4 | 3 | 4 | **3.90** |
| C. Evidence-bound verification workspace | 3 | 3 | 4 | 2 | 4 | 3 | 3.20 |

**A: deliberately minimal.** Finish CI, release, deployment, measurements, and
docs; freeze features. Clear and cheap, but V2 service lifecycle or plain
OpenCode may erase most value.

**B: recommended.** Make one private durable workspace start, wake, attach,
explain, recover, and sleep exceptionally well. It remains useful if OpenCode
changes clients because fern's external host authority and private deployment
experience remain separable. Only add setup/doctor where dogfooding proves a
gap.

**C: evidence-bound workspace.** Independently run declared checks and mark
evidence stale when source/spec/generation changes. More resilient and novel,
but it is a new verification product with unclear demand and significant
correctness burden. It should be a later pivot, not the present roadmap.

## 8. Differentiation Test

### Claim Falsification

| # | Claim | Verdict | Reason |
|---:|---|---|---|
| 1 | Fern is meaningfully better than `opencode serve` + Tailscale | **Unknown** | Safe stop/wake is real; no comparative dogfood proves meaningful user value. |
| 2 | Container stop/wake creates enough value without host stop | **Partly supported** | It reclaims RAM/CPU and reduces process exposure, but not host cost; value depends on host contention. |
| 3 | `fern attach` is seamless locally, tailnet, and phone | **Contradicted** | It derives a local listen URL; remote URL and phone paths are untested. |
| 4 | Hooks are more valuable than previews/notifications | **Unknown** | No observed user friction or comparative evidence. |
| 5 | State-bound verification is not already fully solved | **Partly supported** | No reviewed Amp public exact-state freshness contract; adjacent receipt/proof systems exist, and private systems are unknown. |
| 6 | Fern lifecycle state makes freshness more trustworthy | **Partly supported** | Fern independently owns container/spec/admission state, but not yet atomic source snapshots or independent check execution. |
| 7 | OpenCode roadmap does not make fern redundant | **Unknown** | V2 internal suspendable workspace drivers and service lifecycle create material redundancy risk. |
| 8 | One workspace is useful focus, not fatal | **Partly supported** | It enables correctness and low burden but prevents parallel isolated tasks, a central remote-agent use case. |
| 9 | Exact privacy-oriented segment exists | **Unknown** | Plausible substitutes and Netclode exist; no fern user/adoption evidence validates the pain. |
| 10 | Existing two-week roadmap is feasible part-time | **Unsupported** | It combines deployment, research, RCE hooks, persistence, diagnostics, integration tests, release, and demo. |
| 11 | Hooks/diagnostics beat CI/deployment/measurement/demo | **Contradicted** | Current deficiency is product evidence and reproducibility, not another planned capability. |
| 12 | Fern is stronger Grab evidence than a small gateway | **Partly supported** | Fern is stronger general systems work; a focused gateway more directly proves routing, quotas, accounting, and SQL. |

### Defensible Novelty

The strongest claim fern **could** defend after implementation is:

> Fern invalidates locally recorded deterministic verification evidence when
> its independently observed source identity, desired runtime fingerprint, or
> workspace/container generation no longer matches the state that was checked.

It cannot claim to invent receipts, provenance, attestations, proof-carrying
work, screenshot/video evidence, or source-bound command records. It cannot
claim cryptographic integrity, reproducibility, absence of hidden state,
correctness of the tests, trustworthy agent-authored commands, or freshness
across unobserved external mutations. See [AI Integrity Receipts](https://github.com/invariant-systems-ai/aiir),
[ProofShot](https://github.com/AmElmo/proofshot), and
[agent-receipts](https://github.com/0xelitesystem/agent-receipts).

The strongest evidence against the recommended concept is that OpenCode V2 is
already implementing managed service discovery, explicit locations, session
move, worktrees, bounded recovery, and an internal idle-suspend workspace
driver. Plain OpenCode plus Tailscale is simpler, and Factory BYOM, Coder, and
Netclode provide broader customer-owned alternatives. If users do not value
fern's conservative external stop boundary, the project has no durable product
advantage.

## 9. Grab Evidence Matrix

### Live Role And Requirements

As of 2026-08-15, Grab's first-party careers site and posting API both report the
role as active. It is **Senior Software Engineer, Backend (AI)**, reference
`REF5628X`, in the Applied AI team, Petaling Jaya, Malaysia, onsite. The posting
was released 2026-07-15. Exact sources:

- https://www.grab.careers/en/jobs/744000137791699/senior-software-engineer-backend-ai/
- https://api.smartrecruiters.com/v1/companies/Grab/postings/744000137791699

This is not a generic agent role. Its stated ownership is the **AI Gateway
(GrabGPT Gateway)**, the unified LLM integration point in the critical path of
Grab's LLM traffic. The posting says it provides access to 60+ models and names
OpenAI, Anthropic, and Google Vertex AI. Its requirements divide into four
testable groups:

| Group | Explicit role requirements |
|---|---|
| Request path | Low-latency reverse proxy; unified OpenAI-compatible API to provider-native request/response formats; provider-specific capabilities; SSE streaming; routing and automatic fallback; model/provider onboarding |
| Control and accounting | API keys; OIDC/workload identity; Redis distributed rate limits; tiered capacity isolation; token-level usage; cost computation; chargeback through PostgreSQL and the data lake; observability and governance |
| Platform and operation | Go and Python; AWS/GCP/Azure resources; Kubernetes; CI/CD; database/config migrations; migration to Grab's EKS stack; Tier-critical on-call, capacity/quota incidents, RCA, and post-mortems |
| Baseline qualifications | 3+ years; Go and Python; hands-on LLM, LangChain, and LangGraph; AWS and Kubernetes; distributed systems and multithreading; unit/integration testing and concise documentation; a relational database |

The job ad is authoritative evidence of the current mandate and hiring bar. It
is not independent proof that every named mechanism is already deployed exactly
as described. Public engineering posts provide narrower implementation evidence.

### Current First-Party Architecture Evidence

Evidence labels below are deliberately strict: **gateway** means a public source
describes Grab AI/GrabGPT Gateway itself; **consumer** means a current Grab agent
system describes how it consumes the gateway; **adjacent** means another Grab
platform demonstrates an engineering pattern but not the gateway implementation;
**role-only** means the live job specifies it but no reviewed public engineering
article proves the current implementation.

| Area | Current first-party evidence | Level | What is not publicly established |
|---|---|---|---|
| Gateway scale and role | The 2026 agent-platform post says one gateway fronts every company model call and handles billions of tokens monthly. The live role says 60+ models. The 2025 gateway post reported 50+ models and 300+ onboarded use cases. | Gateway/consumer; self-reported | Independent traffic, latency, availability, or model-count verification |
| Agents | More than 500 services use the internal agent framework and more than 50 remote MCP servers are registered. Current LLM-Kit scaffolds FastAPI, LangGraph ReAct agents, evals, PostgreSQL/pgvector, OIDC, Vault, health, metrics, gRPC, and OTel. | Consumer; 2026 current | These are not necessarily owned by the gateway role; no public source maps team boundaries |
| Palana | Kubernetes-native security substrate used for hundreds of agents: per-agent namespace/service account/PVC/RBAC/quota/network policy/Vault scope, controlled ingress/egress, external kill switch, and idle reaper. | Adjacent; 2026 current | Palana was built by Grab CyberSecurity and is not evidence that the GrabGPT team owns agent runtime orchestration |
| Routing and translation | The gateway exposes provider-specific reverse proxies plus an OpenAI-schema unified API that translates requests and responses. It dynamically routes equivalent models and balances regions to use reserved capacity and avoid regional quota constraints. Current LLM-Kit resolves endpoint by environment/data tier and permits central provider changes and fallback configuration. | Gateway/consumer | Routing algorithm, health model, retry budget, fallback eligibility, consistency, and config propagation are not public |
| Streaming | The role explicitly requires SSE. The reviewed gateway article says responses receive no-to-minimal processing and describes response translation, but does not specify SSE framing, buffering, backpressure, or stream error semantics. | Role-only for SSE | No current first-party engineering proof of implementation details or measurements |
| Cancellation and deadlines | Grab's Go guidance explains propagated contexts, upstream cancellation, child deadlines, retries, and avoiding wasted work. Fern uses request contexts and consumes SSE with context cancellation. | Adjacent/Fern | No reviewed Grab source proves GrabGPT propagates client disconnect through translators to providers, or that cancellation stops provider billing |
| Authentication and identity | The 2025 gateway uses exploration and production API keys, path-based authorization, and upstream credential replacement. Exploration keys are short-lived, staging-only, and more restricted. The role adds API-key and OIDC/workload-identity ownership. Palana derives agent identity from Kubernetes context, retrieves per-agent GrabGPT credentials from Vault, and does not trust client identity headers. | Gateway; role-only for gateway OIDC; adjacent for Palana workload identity | Gateway key storage/rotation/hash design and current OIDC token validation/authorization flow are not public |
| Quotas and capacity | The 2025 gateway enforced request rate per key over provider limits and identified batch interference with online SLOs; advanced token/cost/model/provider limits were future work. The 2026 role calls for Redis distributed limiting and tiered capacity isolation. Grab's separate Quotas service demonstrates Kafka-distributed decisions, local caches, Redis aggregation, sliding windows, Datadog, load tests, and fail-safe architecture. | Gateway for basic historical limit; role-only for current Redis tiers; adjacent for Quotas design | Do not claim the gateway uses the published Quotas architecture. Exact Redis algorithm, atomicity, fail-open/closed policy, tiers, and deployed status are not public |
| Observability | The gateway centralizes monitoring/alerts and writes request/response bodies plus token/path/model metadata to the data lake. Current LLM-Kit auto-instruments FastAPI, outbound HTTP, LangChain, and MCP with OTel; adds Kubernetes attributes; correlates structured logs with traces in Grafana/Kibana. Palana logs proxy, Git, LLM, lifecycle, and idle decisions. | Gateway plus consumer/adjacent | Gateway metric names, trace topology, SLOs, sampling, redaction, dashboards, and alert thresholds are not public |
| Cost and chargeback | Gateway calculates synchronous request cost after the provider response, handles asynchronous jobs with a daily process, archives cost/audit data, aggregates by service for dashboards/showback, and alerts on thresholds. The role adds token-level logging and PostgreSQL/data-lake chargeback pipelines. | Gateway; role-only for PostgreSQL/current details | Price versioning, late/missing usage reconciliation, currency, streaming partials, schema, and financial accuracy controls are not public |
| Governance | New production use cases pass a mini-RFC/checklist and possibly AI Governance review; exploration is separated through short-lived staging keys. Calls, bodies, and metadata are archived for audit and security/policy inspection. GrabGPT itself is private and auditable. Palana adds default-deny egress, OPA decisions, proxy-only secrets, and revocable external controls. | Gateway plus adjacent | Current retention, redaction, access controls, customer-data policy, built-in guardrail deployment, and deletion process are not public. In 2025 built-in guardrails were future work |
| Reliability | Gateway uses regional routing to reduce quota failures and runs SDK integration tests to catch missing paths/configurations. The role identifies it as Tier-critical and names on-call, provider quota incidents, RCA/post-mortems, and EKS migration. Grab-wide guidance covers deadline budgets, cancellation, idempotent retries, jitter, circuit breakers, bulkheads, and rate limiting. | Gateway; role and adjacent | No public GrabGPT SLO/error budget, failover matrix, load result, incident report, or multi-region recovery objective |

Exact first-party engineering URLs, all accessed 2026-08-15:

- https://engineering.grab.com/grab-ai-gateway
- https://engineering.grab.com/how-grab-builds-and-runs-ai-agents-at-scale
- https://engineering.grab.com/palana-part-1-secure-platform-for-ai-agents
- https://engineering.grab.com/part-2-palana-architecture
- https://engineering.grab.com/the-birth-of-grab-gpt
- https://engineering.grab.com/supercharging-llm-application-development-with-llm-kit
- https://engineering.grab.com/grab-bench-evaluating-ai
- https://engineering.grab.com/context-deadlines-and-how-to-set-them
- https://engineering.grab.com/beyond-retries-part-1
- https://engineering.grab.com/quotas-service
- https://engineering.grab.com/designing-resilient-systems-part-2

The 2026 agent-platform article announces that a future Part 2 will cover the
gateway in more depth. No published Part 2 was found by the research cutoff, so
it must not be cited as available evidence.

### Honest Candidate Evidence Matrix

| Requirement | Fern evidence today | Confidence | Missing evidence | Honest framing |
|---|---|---:|---|---|
| Go backend and concurrency | Manager admission, wake coalescing, watcher epochs, locks, bounded lifecycle operations, race tests | High | Production throughput and on-call | “I can show the invariants and race coverage; this is not production-scale operation.” |
| Reverse proxy | Generation-aware `httputil.ReverseProxy` around a changing local backend | High | Provider adapters and protocol conformance | “It proves proxy lifecycle correctness, not an LLM gateway.” |
| SSE | Strict OpenCode SSE consumer with content-type/status validation and reconnect generations | High | Producing OpenAI-compatible streams, translating provider events, backpressure | “I consume SSE safely; I have not yet implemented a multi-provider streaming API.” |
| Cancellation | Context-bound proxy requests, SSE teardown, deadlines, and lifecycle cancellation | Medium | Measured client-disconnect-to-provider cancellation and billing outcome | “Cancellation is propagated locally; provider compute/cost cancellation is unproven.” |
| Routing/fallback | Backend endpoint generations prevent stale routing | Low for role | Model aliases, capabilities, provider-native adapters, pre-header fallback | “The concurrency pattern transfers, but model routing is missing.” |
| Auth and governance | Basic auth forwarding, required password on non-loopback bind, loopback backend | Low for role | API-key lifecycle, OIDC/JWT validation, workload identity, tenant authorization, audit policy | “This is a narrow single-user boundary, not multi-tenant IAM.” |
| Distributed quotas | In-process request admission only | Low | Redis atomic limits, multiple replicas, capacity tiers, fairness and degraded mode | “I will not call local admission distributed rate limiting.” |
| Usage/cost/SQL | No owned token accounting or relational schema | None | Usage normalization, immutable ledger, versioned prices, PostgreSQL migration/reconciliation | “Mounting OpenCode's SQLite volume is not database experience.” |
| Observability | Structured logs, explicit state/failure classification | Medium | OTel traces/metrics, cardinality policy, SLO dashboard and alert exercise | “Useful local diagnostics, not fleet observability.” |
| Reliability | Conservative unknown handling, ownership/spec drift checks, rollback, stale-generation rejection | High transferable | Provider conformance, overload/fallback tests, incident response | “Strong systems design evidence; no Tier-critical operating history.” |
| Kubernetes/cloud | Docker API and container image | Low | Kubernetes manifests/Helm, EKS/AWS resources, rollout and migration | “Docker is not Kubernetes or cloud-platform evidence.” |
| Python/LangGraph/agents | OpenCode is an external workload | None | Python service/agent, LangGraph graph and evaluation | “Agent consumption is not agent-framework implementation.” |
| CI/CD and documentation | Local tests and docs; no checked-in CI/release at audit time | Medium-low | Green public pipeline and deployment exercise | “The code is tested locally; delivery evidence is unfinished.” |

### Highest-Signal Two Weeks For This Role

If the goal is **Fern product evidence**, follow section 10. If the goal is the
**current Grab application**, do not spend the same two weeks adding Fern hooks,
previews, Kubernetes lifecycle, or a credential proxy. Build a separate,
deliberately small **gateway proof**. Do not merge it into Fern or describe it as
production-ready.

Assumption: 24-30 focused hours. The coherent vertical slice is one Go service,
two deterministic provider adapters (one OpenAI-shaped, one differently shaped),
Redis, PostgreSQL, OTel, Docker Compose, and a minimal Kubernetes deployment.
Use local fake providers for repeatable failure and cancellation tests; optional
real-provider smoke tests must not be the acceptance test.

| Order | Deliverable | Acceptance evidence | Hours |
|---:|---|---|---:|
| 1 | Freeze contracts and failure policy | Short design note defines normalized chat/usage errors, provider capability table, timeout budget, idempotency boundary, “fallback only before downstream headers,” and explicit Redis/SQL degraded modes | 2 |
| 2 | Implement unified request path and two adapters | `POST /v1/chat/completions`; model alias selects adapters; request and non-stream response translation; unknown capability rejected; golden conformance tests against fake providers | 5 |
| 3 | Implement SSE and cancellation correctly | Incremental flush without whole-body buffering; normalized OpenAI chunks and `[DONE]`; malformed/mid-stream failures classified; client disconnect closes provider request; test records cancellation at fake provider; never claim provider billing cancellation | 5 |
| 4 | Add deterministic routing and bounded fallback | Tenant/tier/model policy selects route; health/capacity signal; fallback only for eligible pre-stream transient errors; no retry of auth/validation errors; retry budget and attempt fields visible in trace | 3 |
| 5 | Add Redis quotas and capacity isolation | Atomic tenant request/token reservation plus in-flight concurrency; separate online/batch tiers; two gateway replicas in a test share limits; TTL/refund semantics and Redis outage policy tested | 4 |
| 6 | Add usage and cost ledger | PostgreSQL migration; append-only request/attempt/usage records; input/output/cached token normalization; immutable pricing version; streaming partial/unknown usage represented honestly; idempotent finalization and aggregate query tested | 4 |
| 7 | Add auth and observability | Hashed scoped API keys for the demo; request/tenant identity separated from logs; OTel spans across gateway/provider/Redis/SQL, low-cardinality metrics, structured errors; redact prompts, keys, and auth headers | 3 |
| 8 | Package and prove failure behavior | CI for format/unit/integration/race; Compose demo; minimal Kubernetes Deployment/Service/Secret references, probes and graceful shutdown; load/disconnect/quota/provider-failure report with raw commands/results | 3-4 |
| 9 | Optional only after all gates | Tiny Python LangGraph client using the OpenAI-compatible endpoint plus one deterministic eval, demonstrating consumer compatibility without pretending two weeks establishes Python depth | 1-2 |

The demo should show five failures, not merely a successful prompt: provider 429
before headers falls back; provider 401 does not; disconnect cancels upstream;
online capacity remains available while batch is limited; unknown usage remains
unknown rather than becoming zero cost. Publish raw test output and a one-page
tradeoff note. Do not add a UI, generic plugin system, model catalogue, admin
portal, full OIDC issuer, Helm chart, data lake, or real chargeback.

This project would move routing, streaming, Redis, SQL/cost, OTel, and Kubernetes
from “missing” to “portfolio implementation.” It would **not** create evidence of
60-model scale, production traffic, AWS/EKS operation, real provider billing
semantics, on-call/RCA, mature Python, or LangGraph production experience. Those
gaps should remain visible in the application and interview.

## 10. Revised Two-Week Roadmap

Assumption: 24-30 focused hours around a full-time job. Stop when P0 evidence is
complete; do not fill unused time with speculative features.

| Priority | Work and user outcome | Boundary and verification | Hours | Dependency | Stop condition | Deferred |
|---|---|---|---:|---|---|---|
| P0 | Add CI so every claim is independently checkable | GitHub Actions: format, tests, race, vet, build; separate image build. Verify green run from clean checkout. | 3-4 | GitHub Actions | One green run; no matrices/coverage service | Release publishing |
| P0 | Ship one install and supervised private-host runbook | Versioned binary or exact `go install`; one launchd/systemd path; loopback + Tailscale Serve; secrets and removal. Verify from a fresh host/user context. | 5-6 | Target host, Docker, tailnet | Reboot restores service; another engineer can follow steps | Package repositories, installer framework |
| P0 | Compare plain OpenCode and fern end to end | Same repo/provider/host; local and phone cellular; record steps, errors, reconnects, idle stop. | 3-4 | Working provider and phone | Honest conclusion recorded even if fern loses | Feature fixes unrelated to blockers |
| P0 | Measure ten authenticated wakes and resource release | Raw timestamps; median/range; before/after container/host memory; persisted session; disconnect during/after turn; external exit/OOM classification. | 4-5 | Docker host, image, credentials | Reproducible raw data and limitations checked in | Benchmark dashboard |
| P0 | Make attach URL behavior explicit | Document local versus private URL; add a narrow `-url` only if deployment cannot use current config safely. Test credentials absent from argv/logs. | 2-3 | Deployment result | One documented local and remote terminal command works | URL discovery, identity proxy |
| P0 | Record 60-90 second demo and release notes | Phone web wake, persisted session, terminal attach, disconnect, idle stop, failure/status. Verify demo against tagged code. | 3-4 | All earlier P0 | Another engineer can state value and limits after watching | Marketing site |
| P1 | Concise generation-aware status/failure output | Extend existing status only with state fern can observe reliably: container ID/generation, runtime state, endpoint, observed time, last failure. Unit tests plus demo. | 3-4 | Dogfood identifies ambiguity | No stale state is shown as current/green | General `doctor` framework/dashboard |
| P1 | Reconcile architecture/code guide | Correct manager interface and request-invalidation ordering; link measurements. Verify review against HEAD. | 1-2 | Final behavior | No planning behavior described as shipped | Broad rewrite |
| P2 | Disposable V2 compatibility harness | Outside production volume; auth, event schema, active-session boundary, attach, clean/SIGKILL recovery, copied V1 DB. Decision record only. | 5-8, only if P0 done | Installable V2 beta | Stop on unsupported idle boundary or migration risk | Product migration/private SDK |
| Do not build | Setup/resume hooks | No validated user problem; introduces arbitrary code execution and rollback state. | 0 | Repeated measured setup/resume pain | At least 3 real repos/users report the same issue | Caching and service supervisor |
| Do not build | Preview/artifacts/receipts/webhooks | Each is a separate product/security surface. | 0 | Decision triggers below | Validated repeated pain and concept pivot | Public sharing, queues, policy |
| Do not build | Multi-workspace/cloud/Kubernetes/Redis/OIDC/gateway | Requires a different product or company. | 0 | Organizational adopter or separate gateway project | Explicit new decision | All platform scope |

### Required Experiments

The clean-checkout command below passed on 2026-08-15 for formatting, unit
tests, race tests, vet, and build. There is no checked-in daemon-backed Docker
test suite or CI workflow. Docker experiments could not run because the local
daemon was unavailable. Tailscale was installed but did not return status
within 30 seconds. No result below is fabricated.

| Experiment | Exact procedure | Decision-changing outcomes |
|---|---|---|
| Clean checkout CI | Detached worktree at `7c470d6`; run `test -z "$(gofmt -l .)"`, `GOTOOLCHAIN=local go test ./...`, `go test -race ./...`, `go vet ./...`, `go build ./cmd/fern`; separately `docker build -t fern/opencode:dev images/opencode`. | Go checks passed. Image failure blocks release; success establishes only buildability. |
| Plain remote versus fern | Same host/repo/config. Run authenticated `opencode serve` behind Tailscale Serve for three sessions, then fern for three. Count setup commands, reconnect steps, failures, idle resources, and time-to-prompt from laptop/phone. | If fern does not remove repeated work or contention, freeze it as portfolio-only. |
| Authenticated attach | Inspect process list/logs while `fern attach`; test correct and incorrect password locally and through explicit private URL; resume same session. | Credential leak is release-blocking. External URL friction justifies `-url`; V1 auth incompatibility may block product viability. |
| Phone wake and ten samples | Let fern reach stopped state; from cellular request authenticated web URL with timestamp; record first byte and OpenCode-ready time; repeat 10 times, preserving raw timestamps and failures. | Median >5 seconds or unreliable wake weakens transparent-wake positioning; scanners must not cause unauthenticated wake. |
| Memory | Record `docker stats --no-stream`, host free/used memory, and container inspection while active; stop through fern; repeat after 30 seconds across 10 cycles. | Material reclaim supports shared-host value; negligible host change weakens it. Never infer cost saving. |
| Session persistence | Record session ID/messages; allow fern stop; wake and reopen; then `fern down`, recreate around retained volume, and reopen. Back up volume first. | Loss/corruption is release-blocking; recreation failure narrows claim to stop/start only. |
| Disconnect | Submit deterministic long fake-provider turn; close client during stream, then after completion; observe OpenCode status/events, provider socket, persisted messages, fern idle timer and reconnect. | If disconnect cancels execution, phone/background positioning changes; if provider continues, document cost and state semantics. |
| External exit/OOM | While idle and busy, `docker kill`; separately exceed a deliberately low memory limit; inspect Docker OOM/exit state and `fern status`, then request wake. | Misclassification as intentional stop or automatic destructive recovery is release-blocking. |
| V2 replacement | Disposable V2 server and copied backup volume; test Basic auth, official client, `/api/event`, session active/move, permission/form wait, queued input, interrupt, clean stop, SIGKILL and recovery. | Migrate only if public APIs reproduce conservative safe-idle behavior and data recovery; otherwise remain V1. |

## 11. Decision Triggers

| Decision | Measurable trigger |
|---|---|
| Continue current direction | Maintainer uses it at least weekly for 6 weeks; at least 5 unrelated installations; wake success >=99% over 100 attempts; users cite safe stop/recovery as the reason, not generic remote access. |
| Pivot to previews | At least 3 active users repeatedly start app services and report private result inspection as their top friction; Tailscale Serve plus documented manual service is insufficient. |
| Pivot to verification | At least 3 users can show stale or untrustworthy completion evidence causing rework, and they prefer deterministic source-bound checks over CI links or existing receipt tools. |
| Integrate V2 | V2 exits beta for required APIs; official remote client/auth works; migration backup passes; event/status contract proves all-session idle conservatively; no private SDK/provider dependency. |
| Maintenance-only | Fewer than 3 unrelated monthly active installations after 3 months, or OpenCode ships equivalent stable self-hosted stop/wake and diagnostics. |
| Abandon | Maintainer does not use it for 8 consecutive weeks, V1 becomes insecure/unsupported without a viable V2 seam, or safe lifecycle invariants cannot be preserved. |
| Commercialize | At least 10 organizations actively use it, 3 pay for support/pilots, a repeated unsolved organizational need exists, and the maintainer explicitly accepts support/security/release operations. |

Stars, social interest, theoretical willingness to pay, and interview relevance
are not commercialization triggers.

## 12. Source Appendix

All web sources were accessed 2026-08-15.

### First-Party Documentation

- Amp: [manual](https://ampcode.com/manual), [Orbs](https://ampcode.com/manual/orbs),
  [plugin API](https://ampcode.com/manual/plugin-api), [SDK](https://ampcode.com/manual/sdk/typescript),
  [pricing](https://ampcode.com/pricing), [security](https://ampcode.com/security),
  [OIDC](https://ampcode.com/manual/orbs/oidc), [setup note](https://ampcode.com/notes/putting-an-agent-in-an-orb),
  [portals](https://ampcode.com/news/portals), and [webhooks](https://ampcode.com/news/event-driven-orbs).
- OpenCode: [V1 server](https://opencode.ai/docs/server/), [V1 plugins](https://opencode.ai/docs/plugins/),
  [V2 overview](https://opencode.ai/v2/docs/), [V2 API](https://opencode.ai/v2/docs/api),
  [client](https://opencode.ai/v2/docs/build/client), [plugins](https://opencode.ai/v2/docs/build/plugins),
  [SDK](https://opencode.ai/v2/docs/build/sdk), and [migration](https://opencode.ai/v2/docs/migrate-v1).
- Product references: [Tailscale quickstart](https://tailscale.com/docs/how-to/quickstart),
  [MagicDNS](https://tailscale.com/docs/features/magicdns), [Serve](https://tailscale.com/docs/features/tailscale-serve),
  [Vercel Git](https://vercel.com/docs/git), [deployment URLs](https://vercel.com/docs/deployments/generated-urls),
  [Cloudflare local development](https://developers.cloudflare.com/workers/local-development/),
  [bindings](https://developers.cloudflare.com/workers/runtime-apis/bindings/),
  [Convex workflow](https://docs.convex.dev/understanding/workflow),
  [errors](https://docs.convex.dev/functions/error-handling/), and
  [T3 introduction](https://create.t3.gg/en/introduction).

### Pinned Source

- Fern [`7c470d6`](https://github.com/nebler/fern/commit/7c470d6faad733e2d57848e77b66b3ae35c3169f).
- OpenCode V1 [`a3647eb`](https://github.com/anomalyco/opencode/commit/a3647eb025c7615159d417dcc49fc39fdaeba65b).
- OpenCode V2 [`3d782ef`](https://github.com/anomalyco/opencode/commit/3d782efee4c7724024deda362ca22390794dacf3).
- Particularly relevant files are linked in section 4.

### Issues And Pull Requests

- [OpenCode V2 intermittent service restart #37239](https://github.com/anomalyco/opencode/issues/37239).
- [OpenCode historical storage issue #8538](https://github.com/anomalyco/opencode/issues/8538),
  which concerns an older V1 JSON layout and is not evidence against V1
  `1.18.16` SQLite.

### Market And Pricing

- [Cursor Cloud Agents](https://cursor.com/docs/cloud-agent),
  [Copilot cloud agent](https://docs.github.com/copilot/concepts/agents/cloud-agent/about-cloud-agent),
  [Copilot plans](https://docs.github.com/en/copilot/get-started/plans-for-github-copilot),
  [Codex rate card](https://help.openai.com/en/articles/20001106-codex-rate-card),
  [Jules limits](https://jules.google/docs/usage-limits/),
  [Devin pricing](https://devin.ai/pricing), [Factory pricing](https://www.factory.ai/pricing),
  [Coder docs](https://coder.com/docs), [Ona pricing](https://ona.com/pricing),
  [E2B pricing](https://e2b.dev/pricing), [Daytona BYOC](https://www.daytona.io/docs/en/bring-your-own-compute.md),
  [Modal OpenCode example](https://modal.com/docs/examples/opencode_server),
  [Cloudflare container pricing](https://developers.cloudflare.com/containers/pricing/),
  [Vercel Sandbox pricing](https://vercel.com/docs/sandbox/pricing),
  [Runloop pricing](https://www.runloop.ai/pricing),
  [Northflank pricing](https://northflank.com/pricing), and
  [Netclode](https://github.com/angristan/netclode).

### Job-Role Material

- [Grab role](https://www.grab.careers/en/jobs/744000137791699/senior-software-engineer-backend-ai/)
  and [first-party posting API](https://api.smartrecruiters.com/v1/companies/Grab/postings/744000137791699).
- [Grab AI Gateway](https://engineering.grab.com/grab-ai-gateway),
  [GrabGPT origin](https://engineering.grab.com/the-birth-of-grab-gpt),
  [LLM-Kit](https://engineering.grab.com/supercharging-llm-application-development-with-llm-kit),
  [current agent platform](https://engineering.grab.com/how-grab-builds-and-runs-ai-agents-at-scale),
  [Grab Bench](https://engineering.grab.com/grab-bench-evaluating-ai),
  [Palana part 1](https://engineering.grab.com/palana-part-1-secure-platform-for-ai-agents),
  [Palana part 2](https://engineering.grab.com/part-2-palana-architecture), and
  [Grab cancellation guidance](https://engineering.grab.com/context-deadlines-and-how-to-set-them).
- Grab-wide reliability context: [rate limiting](https://engineering.grab.com/beyond-retries-part-1),
  [Quotas service](https://engineering.grab.com/quotas-service), and
  [retries](https://engineering.grab.com/designing-resilient-systems-part-2).

### Third-Party Analysis

- [Cursor agent-factory interview/analysis](https://arize.com/blog/inside-cursors-agent-factory-how-it-verifies-ai-written-code/).
- Adjacent evidence projects linked in section 8. Their claims demonstrate
  category activity, not adoption or independent proof of quality.

## 13. Fern Product Principles

| Principle | Roadmap choice supported | Feature ruled out |
|---|---|---|
| **Every command returns a named object or terminal state.** A mutating command reports workspace/operation identity, phase, and inspection path; partial completion is not success. | Make attach report waking, ready, or a classified failure. | Fire-and-forget wake that prints only “started.” |
| **Hide orchestration; preserve the failure owner and evidence.** Routine output uses workspace states, while failures identify Docker, OpenCode, proxy, auth, or network ownership and a safe action. | Concise status and tested recovery runbook. | Generic “offline” or a dashboard mirroring raw Docker events. |
| **Private identity-scoped access is default; broader exposure is separate.** | Loopback backend plus Tailscale Serve deployment. | Anonymous previews, Funnel automation, public listener defaults. |
| **Fern owns lifecycle continuity, not coding or adjacent platforms.** Build only where fern has unique host/lifecycle authority. | Preserve native OpenCode TUI/web. | Fern chat UI, editor, identity provider, tunnel, secret vault, agent framework. |
| **Every green state names its target, generation, and observation time.** State changes stale prior evidence. | Generation-aware status now; evaluate receipts later. | Evergreen “healthy” or “verified” badges. |
| **First success uses user outcomes, not control-plane vocabulary.** | One install, named workspace, private URL, attach, sleep. | First-run questions for proxy/SSE/container internals or every timeout. |
| **Configuration and abstractions require reproduced leverage and a reversible boundary.** | Add `-url` only if deployment proves it; retain Git/OpenCode data independently. | Generic providers, Kubernetes, Redis, multi-cloud adaptors, speculative plugin systems. |

## Final Answer

Fern earns the right to exist only through this experience:

> On infrastructure I already own, I run one documented install for one
> repository and receive one private OpenCode address. From a laptop terminal or
> phone browser, I use OpenCode's native interface. If the workspace is asleep,
> the ordinary authenticated request wakes it; my persisted session returns;
> fern states exactly what generation became ready. I disconnect without
> managing Docker. After an observed safe idle boundary, fern stops the
> container, returns its resources to the host, and leaves a legible state and
> recovery path.

Fern is the correct layer because only an external host process can admit the
request that wakes stopped compute, verify Docker ownership/specification,
observe OpenCode activity without asking the model, and stop conservatively
under uncertainty. Evidence that users value it is not stars or a polished
roadmap: it is repeated use, materially reclaimed host resources, reliable wake
measurements, fewer manual lifecycle actions than plain OpenCode, and unrelated
users explicitly choosing fern for that boundary. Until that evidence exists,
the disciplined product decision is to finish this loop and build nothing
beyond it.
