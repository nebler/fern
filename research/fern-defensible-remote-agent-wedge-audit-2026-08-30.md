# Audited Research: Fern's Defensible Remote-Agent Wedge

**Research date:** 2026-08-30
**Fern baseline:** `harden/production-readiness` at
`ab945b5a00db3a310b3fcc30fe8bc99669598b6f`, plus uncommitted working-tree
documents present during the audit
**Status:** independent audit; the separately edited
`fern-defensible-remote-agent-wedge-2026-08-30.md` was left untouched

## Evidence Labels

| Label | Meaning |
| --- | --- |
| `T` | Executed in this research session. |
| `R` | Implemented or tested in the Fern repository. |
| `S` | Observed in pinned external source. |
| `D` | Current first-party documentation, not independently tested. |
| `M` | Vendor claim or vendor-reported outcome. |
| `I` | Inference from named evidence. |
| `U` | Unknown; no guarantee was established. |

Code and implemented tests are authoritative over strategy documents. Process
status is not durable task outcome. Workspace persistence is not exact
Git-object retention. Customer-hosted execution is not a fully self-hosted
control surface. Failure to locate source is not proof that a feature is absent.

## 1. Executive Verdict

### Decision: Keep Fern as a personal appliance

The broad remote-agent category is occupied. This audit found repeated pain in
background attention, ambiguous lifecycle state, self-hosting failures,
parallel-workspace interference, cost, permissions and retained work. It did
**not** establish repeated demand for the specific combination that could make
Fern a standalone product: native OpenCode UI continuity, reconstructable Git
objects after runtime deletion, and a separately fenced GitHub finalizer.

The only product hypothesis worth a bounded experiment is:

> **Run the real OpenCode on your own always-on machine, take over from any
> device, and return to an exact Git result even after the task environment is
> gone.**

That is not a go decision for Fern 2.0. It is a two-weekend falsification test
inside a personal appliance. Change the decision to **Build narrow OpenCode
integration** only if:

1. OpenHands custom ACP is materially worse for the owner's real work.
2. Native OpenCode attachment is used at least weekly for four weeks.
3. At least 5 of 10 real tasks produce a useful result without laptop repair.
4. Two same-repository tasks run without writable sharing.
5. A deleted runtime leaves a result reconstructable on a clean clone.
6. The owner prefers the complete journey to OpenHands, T3 and one managed agent.

Otherwise finish the existing release as a portfolio appliance and stop. Extract
the Safe Git Finalizer only after an external maintainer demonstrates demand.

### One-Sentence Explanation

Fern's possible wedge is **native OpenCode handoff with failure-safe exact Git
results**, not self-hosting, remote execution, mobile access, Docker,
Kubernetes, agent choice, or another dashboard.

## 2. Repository Reality

### Executed Here

- `T` `go test ./...` passed on Go 1.24.2, macOS arm64.
- `T` `go test -race ./...` passed.
- `T` Docker Desktop 4.39.0 / Engine 28.0.1 was available.
- `T` The local OpenCode CLI reported `opencode2 v0.0.0-beta-18684`.
- `T` The characterized `fern/opencode:dev` image was not present, so the pinned
  OpenCode contract harness was **not** rerun.
- `T` No remote Ubuntu, phone, provider-funded, GitHub fault-injection or
  OpenHands bake-off was executed.

No comparative speed, memory, installation, restore or reliability result is
claimed in this audit.

### Shipped Strengths

| Capability | Repository evidence |
| --- | --- |
| One persistent workspace | `R` `workspace.Manager` explicitly owns exactly one workspace (`internal/workspace/manager.go:95-164`). |
| Durable exact admission | `R` Task, attempt, actor, receipt, prompt hash, exact base, image, OpenCode IDs, model and budget commit before delivery (`internal/taskstore/admission.go`; schema at `migrations.go:182-263`). |
| One effecting attempt | `R` Unique partial index at `internal/taskstore/migrations.go:253-254`. |
| Exact delivery recovery | `R` Delivery persists phases before session/prompt mutation and reconciles exact IDs after response loss (`internal/taskdelivery/coordinator.go`). |
| Conservative observation | `R` Positive `running`/`input_required` only; inactivity remains inconclusive (`internal/taskexecution/coordinator.go:143-219`). |
| Native OpenCode | `R` Fern proxies the official V2 server/UI unchanged; OpenCode owns sessions, forms, permissions, terminal, files and diffs (`docs/OPENCODE.md:6-29`). |
| Exact user-selected result | `R` Production uses authorized preview/recollection and marks the attempt `superseded`; it does not call the agent successful (`cmd/fern/tasks.go:332-355`). |
| Trusted exact-result verification | `R` Optional host executable checks the selected commit under repository-integrity constraints. |
| Safe App publication | `R` `push_started` and `pr_create_started` precede one mutation; recovery uses exact reads (`internal/taskpublicationcoord/`). |
| Operational substrate | `R` Backup/restore, compatibility, release, credential replacement, lifecycle and ingress tests exist. Physical target-host acceptance remains incomplete. |

### Not Shipped

- Multiple independent workspaces, per-attempt checkout/server/state, or
  concurrent effecting attempts.
- Generic automatic OpenCode terminal success.
- Restart-stable Fern answers to forms or permissions.
- Portable Git object bytes or bundle independent of the checkout.
- Notification outbox, retention/cleanup coordinator or disk-pressure policy.
- Remote runner, k3s, Agent Sandbox, Gateway, generic agents or hosted tenancy.
- Embedded phone action to request the already implemented App publication.

### Repository Documentation Defects

- `TASK_MODEL.md` says unlisted transitions are invalid, but schema-6 user sealing
  permits `input_required -> completed` and active attempts to `superseded`.
- The document describes `collecting/uncertain/failed/recovery_required` Result
  rows; schema 6 permits only immutable `sealed` Result rows.
- Approval-shadow prose conflicts with the implemented boundary: no approval,
  question or form table exists.
- `docs/OPENCODE.md` says epoch-lost forms move to recovery, but the execution
  observer has no process-epoch input and treats disappearance as inconclusive.
- Production attests the initially selected image ID against later drift but
  does not prove through health that the selected image is the characterized
  OpenCode build. Release/deployment policy must enforce the digest/profile.

## 3. Correction Register

| Claim | Evidence | Correction | Consequence | Confidence |
| --- | --- | --- | --- | --- |
| Self-hosted background agents are Fern's wedge. | OpenHands, Coder, T3, Orbit, Warren, Deputies and Symphony are self-hosted; many vendors support customer execution. | Table stakes. | Do not lead with self-hosting, remote work or BYOC. | High |
| OpenHands cannot run OpenCode. | Canvas supports custom ACP; OpenCode has ACP. OpenCode is absent from the built-in registry. | Experimental custom command, not first-class preset. | Run the exact bake-off first. | High |
| OpenHands preserves native OpenCode. | Agent Server normalizes ACP JSON-RPC into OpenHands events rendered by Canvas. | No supported native OpenCode UI path was found. | Visible hypothesis, not market proof. | High |
| OSS Canvas creates a fresh environment per conversation. | Backend-dependent; local mode is shared, Helm commingles agents in one Pod/PVC, ordinary requests default `worktree=false`. | Isolation is optional, not invariant. | Test the exact profile and same-repo writes. | High |
| Canvas restart resumes the same turn. | Agent Server reloads persisted conversations but marks interrupted running work `ERROR`. | History survives; transparent in-flight continuation is not general. | Compare explicit recovery, not transcript presence. | High |
| OpenHands uses idle as completion. | Source status model makes `IDLE` nonterminal. | False. `FINISHED` is primarily agent-declared but idle is not finish. | Differentiate on exact result/checks, not this strawman. | High |
| ACP guarantees durability. | ACP standardizes messages, updates, permissions, cancel and stop reasons. | Process supervision, replay and effect fencing remain implementation-specific. | Comparator/adapter, not authority. | High |
| OpenHands cancellation proves stop. | Conversation cancel and automation cleanup have races; cancellation does not generation-bind shell/network/GitHub effects. | Best-effort control, not stop acknowledgement. | Test marker-bearing delayed effects. | High |
| Current OpenCode cannot recover server-interrupted execution. | Current V2 source has durable execution claims, restart sweep and bounded resume budget; background subagents can recover. | This is false for current beta, though true of older/pinned behavior in important paths. | Test an upgrade adapter before building duplicate recovery machinery. | High |
| OpenCode `/wait` is simply unavailable. | Current V2 implements it; it only waits on process-local execution and never schedules work. | Version-stale blanket claim. It is still not durable completion authority. | Pin source/build and prefer durable execution log. | High |
| OpenCode global events are the durable replay path. | Current global SSE is live-only and can drop an overflowing subscriber; per-session experimental log is durable/cursored. | Use session log for recovery and global stream for freshness. | Persist sequence after processing and test overflow/restart. | High |
| Current OpenCode terminal event proves a Fern task succeeded. | It proves OpenCode execution terminal for that session; it does not select/retain/verify/publish exact Git state. | Preserve separate outcome boundaries. | Automation may reduce manual execution classification without weakening result authority. | High |
| Fern preserves reconstructable results after deletion. | Current records retain IDs/manifests, not all object bytes. | Commit ID is not artifact retention. | Bundle before cleanup. | High |
| Fern prevents all stale GitHub writes. | App-broker path is fenced; workspace `gh` or another token is outside it. | Scope claim to Fern-owned authority. | Safe prototype omits agent write credentials. | High |
| Safe finalization is validated demand. | Technical source gap exists; public demand for publication journals is weak. | Component hypothesis only. | Interview/test before extraction. | High |
| Personal hardware is unique. | OpenHands, T3, Coder, Orbit and connected-host products cover it. | Hardware ownership is delivery, not differentiation. | Lead with native handoff plus retained exact result. | High |
| Fern is faster/lighter. | No controlled same-host comparison. | Unmeasured. | Benchmark before wording. | High |
| Kubernetes Agent Sandbox differentiates Fern. | KAS is a young `v1beta1` workload lifecycle substrate with no task/result/publication semantics. | Infrastructure option only. | Remove from first twelve weekends. | High |
| Session Teleport is incremental. | No supported OpenCode live state migration contract for DB, processes, terminals, repository, plugins and effects. | Research project, not feature. | Reject initially. | High |
| Exact correctness work guarantees product value. | Strong implementation reuse; invisible until failure/ambiguity and no adoption proof. | Engineering advantage is not demand. | Make it visible and apply kill criteria. | High |

## 4. OpenHands Gap Audit

Pinned: OpenHands/Canvas `1.16.0` at
[`64c1269`](https://github.com/OpenHands/OpenHands/commit/64c1269655012698bc66538967989996191beb6c),
Agent Server/SDK `v1.44.1` at
[`9d143aa`](https://github.com/OpenHands/software-agent-sdk/commit/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a),
Automation `1.9.1` at
[`e4535c8`](https://github.com/OpenHands/automation/commit/e4535c85ea158068f554255c44c2bfcf616aa566),
and docs at
[`5a75b32`](https://github.com/OpenHands/docs/commit/5a75b32c7f1e93811e8ccf440ad577307cc35bd6).

| # | Question | Finding | Evidence |
| ---: | --- | --- | --- |
| 1 | Can OpenHands run OpenCode? | Custom ACP can attempt `opencode acp`; not hands-on verified here. | `D/I` [Custom ACP](https://docs.openhands.dev/openhands/usage/agent-canvas/acp-agents#custom-acp-servers) |
| 2 | Preset or custom? | Custom. Built-ins are Claude Code, Codex and Gemini. Open issue #4639 tracks OpenCode packaging/pinning/auth/isolation/testing. | `S` [registry](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-sdk/openhands/sdk/settings/acp_providers.py#L398-L484), [#4639](https://github.com/OpenHands/software-agent-sdk/issues/4639) |
| 3 | Native OpenCode UI? | No supported route found. Canvas renders normalized ACP events. | `D/S` [ACP architecture](https://docs.openhands.dev/openhands/usage/agent-canvas/acp-agents#what-is-an-acp-agent) |
| 4 | Fresh isolated environment each conversation? | No universal guarantee; backend-dependent, and Helm commingles agents. | `D` [isolation](https://docs.openhands.dev/openhands/usage/agent-canvas/architecture#execution-and-isolation) |
| 5 | Isolated checkouts? | Optional `worktree`; ordinary requests default false and child fallback may share parent. | `S` [request model](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-sdk/openhands/sdk/conversation/request.py#L105-L123) |
| 6 | Isolated OpenCode state? | Not automatic; `acp_isolate_data_dir` defaults false and OpenCode paths are unregistered. | `S` issue #4639 |
| 7 | Retain/reopen completed environment? | Conversation/workspace can persist with backend storage; policy is backend-specific. | `D/S` |
| 8 | Canvas/Agent Server restart? | History/settings persist; interrupted `RUNNING` work becomes `ERROR`, not transparent continuation. | `S` [recovery](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-agent-server/openhands/agent_server/conversation_service.py#L2069-L2108) |
| 9 | Sandbox deletion? | Conversation deletion preserves workspace; later workspace/PVC deletion has no portable-result guarantee. | `S/U` [deletion](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-agent-server/openhands/agent_server/conversation_service.py#L1799-L1856) |
| 10 | Prompt/conversation dedupe? | Caller UUID returns existing conversation without validating new request digest. GitHub events dedupe only with delivery ID. | `S` [start path](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-agent-server/openhands/agent_server/conversation_service.py#L1403-L1485) |
| 11 | Completion? | `FINISHED`, `ERROR`, `STUCK` terminal; `IDLE` explicitly nonterminal. Normal finish is agent-declared. | `S` [state model](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-sdk/openhands/sdk/conversation/state.py#L48-L79) |
| 12 | Silence as completion? | No; idle is nonterminal. `/goal` adds an LLM judge, not deterministic exact-result proof. | `S/D` |
| 13 | Cancellation races? | Conversation interrupt cancels active task; automation can commit cancel before sandbox ID exists and dispatcher can still start. | `S` [cancel](https://github.com/OpenHands/automation/blob/e4535c85ea158068f554255c44c2bfcf616aa566/openhands/automation/router.py#L745-L843) |
| 14 | Can stale worker mutate? | Conversation disk writes use lease generations; arbitrary subprocess/network/GitHub effects are not fenced end to end. | `S/I` |
| 15 | Attempt generations? | Conversation lease has monotonic generation; no general task/effect generation guarantee. | `S` [lease](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-agent-server/openhands/agent_server/conversation_lease.py#L18-L26) |
| 16 | Who creates branch/PR? | Canvas controls send natural-language prompts to the agent. | `S` [prompts](https://github.com/OpenHands/OpenHands/blob/64c1269655012698bc66538967989996191beb6c/src/utils/utils.ts#L389-L425) |
| 17 | Ambiguous push/PR recovery? | No general prepared-effect/read-reconciliation contract located. | `U` |
| 18 | Exact Git object retention? | No mandatory portable bundle/object manifest located. | `U` |
| 19 | Mobile attention? | Responsive web over Tailscale/ngrok; no native app/push/outbox guarantee found. | `D/U` [mobile](https://docs.openhands.dev/openhands/usage/agent-canvas/mobile-access) |
| 20 | Personal-machine burden? | npm/npx or one Docker image is compact, but strict per-task isolation/recovery is not default and issue clusters show runtime diagnosis costs. | `D/issues` |

**Conclusion:** OpenHands already beats current Fern on broad user experience. It
is not a drop-in replacement for Fern's exact task/result/finalizer semantics,
but those narrower gaps have no standalone demand proof. Fern must beat custom
ACP in a real user journey, not in an architecture table.

## 4A. Current OpenCode V2 Contract Audit

This is distinct from Fern's pinned `0.0.0-next-17444` profile. Current V2 source
was inspected at
[`4a977b2`](https://github.com/anomalyco/opencode/commit/4a977b2b3158adba43daec52fb3a9ab386dad3a8),
2026-08-30. Current published beta `0.0.0-beta-18684` maps through successful
[workflow run 18684](https://github.com/anomalyco/opencode/actions/runs/33234821926)
to source
[`106629a`](https://github.com/anomalyco/opencode/commit/106629aa118086be7def6123241a9bf056ba77b6),
2026-08-29. API/client contracts remain beta.

| Capability | Current source finding | Fern consequence |
| --- | --- | --- |
| Durable prompt inbox | `S` `steer`, `queue` and cancellation rows persist in SQLite and promote after restart. | Exact stable message IDs remain useful; current OpenCode removes some custom recovery work. |
| Client disconnect | `S` Server owns execution after HTTP admission; submitting client need not remain. | Generic "close laptop" is increasingly OpenCode itself, not Fern. |
| Server crash/restart | `S` Durable execution claims and restart sweep resume with a ten-resume crash-loop budget. | Characterize/adopt rather than assume server loss is terminal; preserve Fern attempt/environment generation. |
| Durable terminal events | `S` Per-session log contains `session.execution.succeeded`, `failed` and `interrupted`. | A new adapter may classify OpenCode execution terminal without manual seal; it still must not call the Git result verified/published. |
| Per-session event replay | `S` Experimental durable log supports exclusive `after` cursor, follow and `log.synced`. | Persist Fern cursor after processing each event; promising upgrade from current pin. |
| Global SSE | `S` Live-only queue, no replay ID; subscriber drops after 4,096-item overflow. | Liveness/UI hint only, never recovery authority. |
| `/wait` | `S` Implemented now, but waits only for process-local execution to become idle and never starts work. | Not scheduler or durable completion authority; prior blanket "503 unavailable" claim is version-stale. |
| Permissions/forms | `S` Pending deferreds/cache remain process memory and are not reconstructed. | Preconfigure unattended allow/deny; durable `ask`/answer still needs a stronger upstream contract. |
| Background subagents | `S` Running child can resume and notify parent after restart. | Useful bounded parallel reasoning without Fern building agent swarms. |
| Background shell | `S` Running shell is reported canceled on server restart, not continued. | Durable shell/build execution still requires external supervision or honest interruption. |
| Fork | `S` Explicit before/through boundaries over settled history. | Result/session forking is more feasible, but not first wedge. |
| Export/import | `S` Settled transcript transfer exists. | Backup/settled transfer, not migration of running job/process/workspace. |
| Snapshots | `D/S` Best-effort filesystem rollback. | Cannot reverse processes, ignored files, network/GitHub effects or replace exact retained Git artifact. |
| ACP | `S` `load` replays, `resume` attaches without replay, `fork` creates native fork; ACP starts a private standalone server tied to stdio lifetime. | OpenHands ACP does not attach to Fern's shared native server or preserve its native web UI; direct HTTP is the correct Fern path. |
| Remote service | `D/S` Defaults loopback; Basic auth and `OPENCODE_PASSWORD`; hostname/port/CORS but no native TLS. | Keep Tailscale/HTTPS/auth proxy, strict version rejection and secret management. |

Recommended direct integration:

1. Pin CLI and client to the exact published build and OpenAPI hash.
2. Use HTTP, not ACP, for Fern. Persist session ID, caller message IDs and last
   processed durable sequence.
3. Consume `/api/experimental/session/{id}/log?after=<seq>&follow=true`; commit
   cursor only after processing.
4. Use global events only as hints.
5. Separate OpenCode execution terminal from selected Git result, trusted checks
   and publication.
6. Do not leave unattended work waiting on process-memory `ask` interactions.
7. Treat transcript export/import as settled backup, not live Session Teleport.
8. Run external process supervision for shell work that must survive restart.

This makes the narrow prototype more feasible but weakens Fern's standalone
claim. OpenCode increasingly supplies its own durable background execution,
restart recovery, forks and clients. Fern must prove value above that substrate:
isolated task computers, durable attention policy, exact Git-object retention and
safe publication. It must not market OpenCode's new features as Fern novelty.

## 5. Competitive Capability Matrix

| Product | Hosting/runtime | Native handoff/mobile | Durability/result/publication | Price/license/limit |
| --- | --- | --- | --- | --- |
| Fern today | `R` Owner-hosted, one Docker/OpenCode workspace. | `R` Official OpenCode web UI, phone task page. | Strong task/effect records and manual exact seal; no portable object bytes/concurrency. App finalizer only in broker mode. | Unreleased; one owner/workspace/profile. |
| OpenHands | `D/S` MIT self-host; local/Docker/VM/remote/cloud/Kubernetes; OpenHands and ACP. | Responsive Canvas, files/terminal/changes; no supported native OpenCode UI/push contract. | Persistent conversation; backend-dependent workspace; no mandatory Git bundle/finalizer. | MIT; model/infra cost. |
| Cursor | Managed VMs or customer worker pools; Cursor retains agent loop. | Web/desktop/iOS/Android PWA, remote desktop/terminal takeover. | Durable runs/events/artifacts/Builds; exact crash/cancel/finalizer contract `U`. | Proprietary; Individual $20, Teams $40/user plus usage. |
| Codex | Cloud containers or connected owner computer. | CLI/IDE/desktop/web/mobile, Remote steering/approval/handoff. | Cached cloud state up to 12h; connected host must stay online; exact retained result `U`. | Proprietary cloud; open CLI; plans/usage apply. |
| Claude Code | Local/connected host, Anthropic cloud or customer runner with Anthropic control. | Web/iOS/Android Remote Control, queued messages, `--teleport`. | Session resume window/requeue documented; exact Git/finalizer `U`. | Proprietary; Pro $20, Max from $100. |
| Copilot coding agent | Ephemeral Actions environment or customer runner. | GitHub issue/PR/session and GitHub Mobile. | GitHub retains logs/commits; 59-minute hard limit; at most one PR per assigned task documented. | Paid Copilot plus Actions/AI credits. |
| Jules | Hosted per-task VM. | Mobile web, plan/activity/diff, pause/delete, browser notifications. | Async history; crash/cancel/retention contract `U`; branch/PR flow. | Proprietary; 15/100/300 daily, 3/15/60 concurrent tiers. |
| Ona | Cloud or Ona-managed customer VPC. | Web/mobile browser; prepared environments/automation. | Core env deletes after seven inactive days; formal run recovery/finalizer `U`. | Proprietary; from $20/month plus usage. |
| Warp | Vendor or customer workers; vendor retains session/control authority. | Terminal/web attach/steer and integrations. | Run records; checkpoint/cancel/exact finalizer `U`. | Proprietary; plan/usage pricing. |
| Devin/Outposts | Vendor VM or customer VM/container/Kubernetes/Mac; cloud loop. | Browser IDE/shell and steering. | Queue/workflow stage resume; task fencing/finalizer `U`. | Proprietary; current plans from free/Pro/Max. |
| Coder Agents | AGPL self-hosted Coder loop and Terraform workspaces. | Web/API, queued follow-ups/questions. | DB chat survives workspace lifecycle; Git authority from workspace; finalizer `U`. | AGPL Community; Premium commercial. |
| T3 Code | MIT server on user's local/remote machine; supports OpenCode and other CLIs. | Native iOS/Android, web, desktop, relay/Tailscale/SSH. | Event-sourced threads, receipts, cursors, Git checkpoints; no managed compute/finalizer. | MIT/free. |
| Orbit (`jianghailong-xy/orbit`) | MIT self-hosted PostgreSQL control surface/outbound runners; supports OpenCode. | Apple/web clients and approvals. | Durable tasks/sessions/checkpoints, exact Git/test/merge path; concrete PR finalizer not located. | MIT at `aca9757`, pre-1.0. |
| Warren | MIT self-host; fresh `bwrap` or Kubernetes run, Claude/Pi. | Responsive web/steering/cancel. | Durable history; local restart loses active runs; trusted host reaper pushes without sandbox GitHub auth. | MIT, young project. |
| Deputies | MIT self-hosted Pi system; many sandbox providers. | Web/integrations/queued steering. | Postgres queues/leases/replay/two-phase cancel; constrained trusted Git tool; no general PR gate. | MIT; several services, young. |
| Symphony | Apache-2.0 self-hosted Codex issue scheduler. | Logs/status, no prescribed rich mobile UI. | Retry/stall/tracker recovery; scheduler authority in memory; Git workflow-defined. | Experimental Apache-2.0 reference. |
| Daytona | Managed arbitrary-agent sandbox. | SDK substrate, not agent UI. | Stateful/snapshots; caller owns task/Git semantics. | Usage priced; sub-90ms start is vendor claim. |
| E2B | Managed sandbox, Enterprise BYOC. | SDK substrate. | Session/template persistence; Hobby 1h, Pro 24h max. | Pro $150/month plus usage; platform proprietary. |
| Runloop | Managed microVM Devboxes. | Browser/shell APIs. | Snapshot/fork/suspend; no agent/finalizer semantics. | Pro $250/month plus usage. |
| Kubernetes Agent Sandbox | Apache-2.0 Kubernetes CRD/controller; isolation delegated to RuntimeClass. | Infrastructure API only. | PVC, pause/resume, claims/warm pools; no task/result/publication authority. | Young `v1beta1`; cluster cost. |

**Competitive conclusion:** OpenHands/T3 invalidate generic OpenCode remote
control. Deputies invalidates broad durable-coordinator novelty. Warren invalidates
"agent lacks GitHub auth" as uniqueness. Sandbox vendors invalidate substrate as
a wedge. Fern's remaining technical distinction is the composition of native
OpenCode, exact selected-result verification and conservative publication
recovery. It is absorbable and demand is unproved.

## 6. User-Pain Evidence

Counts were observed on 2026-08-30 and are comments/posts/reactions, not unique
users or incidence.

| Problem | Affected user | Evidence/frequency/severity | Workaround and solvers | Fern advantage | Decision |
| --- | --- | --- | --- | --- | --- |
| Durable background attention | Users leaving several local/remote agents unattended. | OpenCode [#5887](https://github.com/anomalyco/opencode/issues/5887); Claude [#13024](https://github.com/anthropics/claude-code/issues/13024) 26 comments/81 reactions; Codex [#3962](https://github.com/openai/codex/issues/3962) 53 comments/192 +1; repeated Cursor notification requests. **Very strong/high.** | Bells, hooks, polling, tmux, Telegram/ntfy; vendors increasingly ship notifications. | Durable task projection exists; outbox/answers do not. | Required feature after wedge test, low defensibility. |
| Brittle self-host startup/network/versioning | Home-server, WSL, private/regulated and complex integration-test users. | OpenHands [#12528](https://github.com/OpenHands/OpenHands/issues/12528) 61 comments, [#5968](https://github.com/OpenHands/OpenHands/issues/5968) 45, [#8705](https://github.com/All-Hands-AI/OpenHands/issues/8705) reports 1.5-2 lost days; recurring compatibility issues; Cursor Docker-in-Docker 22 posts/39 likes. **Very strong/blocker.** | Pin images, networking/firewall fixes, privileged Docker or managed agents. | Small Go/SQLite service, image/drift/lifecycle checks. | Potential known-good appliance claim; must benchmark. |
| Ambiguous completion/debug evidence | Background users/operators deciding whether to trust or retry work. | Claude [#21151](https://github.com/anthropics/claude-code/issues/21151) 133 comments; Codex [#24287](https://github.com/openai/codex/issues/24287); OpenHands [#13479](https://github.com/OpenHands/OpenHands/issues/13479). **Strong/high.** | Inspect process/Docker/Git/provider logs and restart. | Exact IDs, phases, uncertainty, seal/check/publication receipts. | Strongest implementation fit; no install-demand proof. |
| Parallel interference/merge debt | Power users, monorepo and multi-repo teams running several tasks. | Claude [#24798](https://github.com/anthropics/claude-code/issues/24798) 84 comments; OpenCode [#17994](https://github.com/anomalyco/opencode/issues/17994), [#4278](https://github.com/anomalyco/opencode/issues/4278); Cursor multi-repo thread 30 votes. **Strong/high.** | Worktrees/clones/containers, ports, manual ownership/order, cloud agents. | Exact attempt/base/result foundation, but no per-attempt runtime today. | Isolation is table stakes, not wedge. |
| Context compaction loses constraints/state | Long feature/debug sessions and safety-constraint users. | OpenCode [#4102](https://github.com/anomalyco/opencode/issues/4102) aggregates >=8 issues; Claude [#34556](https://github.com/anthropics/claude-code/issues/34556) 113 comments/59 compactions; Codex [#29356](https://github.com/openai/codex/issues/29356). **Very strong/high.** | `AGENTS.md`, progress files, commits, fresh sessions, manual handoff. | Task/base/result/effects survive outside model context; reasoning does not. | Consider typed handoff later; no generic memory product. |
| Permission fatigue vs unsafe broad authority | Enterprise, autonomous and untrusted-repository users. | Claude [#11380](https://github.com/anthropics/claude-code/issues/11380) 82; Codex [#2860](https://github.com/openai/codex/issues/2860) 77 and [#14936](https://github.com/openai/codex/issues/14936) 56; Cursor thread 87 posts; OpenCode [#3808](https://github.com/anomalyco/opencode/issues/3808). **Strong/high-critical.** | Blanket allow, VMs, short-lived credentials, egress proxy. | App finalizer separates one exact authority; provider/general egress remain open. | Safe finalizer credible; full capability envelope later only. |
| Runaway/opaque cost | Heavy individuals/teams using parallel agents and premium models. | Claude [#16157](https://github.com/anthropics/claude-code/issues/16157) 1,491 and [#38335](https://github.com/anthropics/claude-code/issues/38335) 837; Codex [#14593](https://github.com/openai/codex/issues/14593) 630; Cursor pricing thread 415. **Very strong/high.** | Dashboards/log parsers/LiteLLM/Helicone/Langfuse. | Could bind budget to exact attempt only after Gateway. | Strong pain, wrong immediate wedge. |
| Resource leaks on laptops/servers | Laptop users and operators retaining many sessions. | OpenCode [#20695](https://github.com/anomalyco/opencode/issues/20695) 138; Codex [#28224](https://github.com/openai/codex/issues/28224) 154 and [#20214](https://github.com/openai/codex/issues/20214) 110. **Repeated/medium-high.** | Restart/archive/remote VM/resource limits. | Remote bounded stop/freeze. | Supports appliance; remote compute already common. |
| Review overload | Maintainers and teams adopting parallel agent output. | Claude [#42796](https://github.com/anthropics/claude-code/issues/42796) 583; Cursor discussion 62 posts; recurring HN practitioner reports. **Strong/high.** | Smaller PRs, CI, plan approval, second-model reviewers. | Checks bind to exact result but do not prove semantic correctness. | Improve handoff; avoid proof-of-correctness claims. |

### Missing Demand Evidence

No repeated cross-source evidence established that users specifically require:

- official OpenCode web UI instead of competent ACP/T3;
- live OpenCode session migration between hosts;
- Git-object reconstruction after deletion instead of a pushed WIP branch;
- duplicate-PR recovery or attempt generations as a purchasing criterion;
- native Fern mobile software.

This missing evidence controls the verdict.

## 7. Emerging Primitives

| Primitive | Opportunity | Caveat |
| --- | --- | --- |
| Durable OpenCode inbox/messages and evolving session replay | Bind exact external task IDs without terminal scraping. | Version-specific; Fern's pin has volatile global events/epoch-local forms. |
| ACP | Cheap OpenHands baseline and future adapters. | Vocabulary, not durability. |
| Native agent client/server modes | Background-to-interactive without replacing loop/UI. | Persistence/terminal semantics differ. |
| Connected-host mobile control | Validates cross-device job. | Removes generic remote control as whitespace. |
| Prepared disposable environments/snapshots | Makes concurrent task computers practical. | Fern has no setup pipeline; substrate is commodity. |
| Git bundles/content-addressed objects | Result survives compute deletion. | Old primitive; composition is hypothesis. |
| GitHub Apps/scoped credentials | Agent can edit without final publication authority. | Fern scopes only broker path, not general egress. |
| Durable attention state | Questions can outlive clients. | Fern lacks outbox/stable answer contract. |
| Typed external actions | Lost mutation response can be reconciled. | Arbitrary agent shell/network effects remain outside. |

The newly practical composition is **native session, disposable task environment,
retained Git artifact and separate finalizer**. It is not a new sandbox category.

## 8. Candidate Wedge Ranking

Scores are favorable 1-5. For alternatives, 5 means poorly solved. For size and
operations, 5 means easy/small. Weights: pain 15%, alternatives 12%, Fern
advantage 13%, OpenCode fit 10%, visibility 10%, personal use 10%, implementation
8%, operations 5%, defensibility 7%, demo 5%, clarity 4%, Grab relevance 1%.

| Rank | Candidate | P/A/F/O/V/U/I/Ops/D/Demo/C/G | Raw /60 | Weighted /5 | Decision |
| ---: | --- | --- | ---: | ---: | --- |
| 1 | Native OpenCode handoff | 3/4/4/5/5/5/3/4/3/5/5/2 | 48 | **4.07** | Prototype; demand unproved. |
| 2 | Failure-safe exact handoff | 3/4/5/4/4/4/3/4/4/4/4/4 | 47 | **3.90** | Compose with #1. |
| 3 | Safe Git Finalizer | 3/4/5/4/3/4/4/4/4/4/4/4 | 47 | **3.88** | External demand test. |
| 4 | Durable attention inbox | 4/3/3/4/5/5/3/3/2/5/5/3 | 45 | **3.76** | Feature, not wedge. |
| 5 | Retained task computer/fork | 4/2/3/5/5/5/2/2/2/5/5/2 | 42 | **3.60** | Too broad initially. |
| 6 | Instant OpenCode tasks | 4/2/2/4/5/5/2/2/1/5/5/2 | 39 | 3.30 | Benchmark only. |
| 7 | Session Teleport | 3/4/2/2/5/4/1/3/4/5/5/2 | 40 | 3.27 | Reject: missing contract. |
| 8 | Private OpenCode cloud | 3/1/3/5/4/5/2/3/1/5/5/2 | 39 | 3.21 | Reject: occupied. |
| 9 | Runtime conformance kit | 2/4/4/3/2/2/4/4/3/4/3/5 | 40 | 3.10 | Portfolio/distribution. |
| 10 | OpenHands finalizer extension | 3/4/4/3/3/2/3/3/2/3/3/4 | 37 | 3.09 | Conditional integration. |
| 11 | Generic remote canvas | 4/1/2/2/4/4/1/1/1/4/3/3 | 30 | 2.53 | Reject. |

The highest score still fails the final decision rule because pain frequency for
the native distinction, weekly use and the hands-on comparison are unknown.

## 9. Top Three Concepts

### A. Native OpenCode Handoff

| Field | Decision |
| --- | --- |
| Promise | Run the real OpenCode on your own server, leave it working, and re-enter the exact native task from any device. |
| Target | OpenCode power user with an always-on Linux host and several substantial tasks/week. |
| Job | Start remotely, leave, inspect/answer later, then retain one reviewable result. |
| Workaround | SSH/tmux, expose `opencode2 serve`, T3, OpenHands custom ACP, or hosted agent. |
| Why OpenHands may fail | Generic ACP surface does not support the official OpenCode UI or automatic OpenCode state isolation. Must be tested. |
| Why Fern | Already proxies native UI, binds exact session/message/task IDs, seals exact snapshots and reconciles publication. |
| Journey | Choose repo/base; submit; close client; receive `Needs you`; open exact OpenCode; answer/steer; leave; inspect result/checks; seal; optional draft PR. |
| Minimum architecture | Go/SQLite, per-attempt Docker checkout/state/server, task route, direct V2 API, Tailscale, Git bundle, optional App broker. |
| Do not build | Kubernetes, new editor, generic agents, remote runners, orgs, Gateway, native mobile, schedules, previews. |
| Two weekends | OpenHands ACP baseline; then two disposable OpenCode containers/routes, Fern restart, manual writer stop and reconstructable Git bundle. |
| Six weekends | Durable serial lifecycle then queue of two, artifact store, restart/cleanup, one notification, verification/publication adaptation. |
| Twelve weekends | `init/doctor`, Ubuntu release, backup/restore, phone detail, measured performance, one external install. |
| Acceptance | Six owner tasks/two weeks, two concurrent pairs, >=60% useful without laptop repair, native UI opened >=25%, materially better than ACP, no prompt loss, clean reconstruction after deletion. |
| Kill | ACP/T3/SSH equally good; native UI rare; setup dominates; <50% useful; prototype requires Kubernetes/new UI/generic runtime. |
| Demo | Phone submits two tasks; restart Fern; laptop opens real OpenCode; answer; delete completed runtime; reconstruct and publish exact result. |
| Tweet | "Fern keeps the real OpenCode running on your server. Start from your phone, take over in the native UI, and keep the exact Git result after the task computer is gone." |
| Risk | Beta OpenCode API/UI/state/auth churn and high pin/upgrade labor. |

### B. Failure-Safe Exact Change Handoff

| Field | Decision |
| --- | --- |
| Promise | A failed or deleted task cannot take its exact reviewable Git result with it. |
| Target | Maintainer delegating long tasks who does not trust a live sandbox/transcript as the only copy. |
| Job | Determine exactly what exists, run trusted checks and preserve partial work before cleanup. |
| Workaround | Keep workspaces, push WIP branches, download patches. |
| Why OpenHands may fail | Conversation/workspace persistence does not mandate independent Git objects bound to verification/publication. |
| Why Fern | Exact base/result/tree/manifest, user authority, same-result checks and conservative uncertainty already exist. |
| Journey | See `Interrupted, work preserved`; materialize exact result; check; authorize/fork; publish selected object. |
| Minimum architecture | Existing result/check/publication code plus atomic local CAS, Git bundle, manifest and clean materializer. |
| Do not build | Process/video replay, VM memory snapshots, universal adapters, proof-of-model claims. |
| Two weekends | Export one sealed result; hide source repo; materialize clean; reject missing/tampered/interrupted artifacts. |
| Six weekends | Atomic ingestion, retention/cleanup, logs/check artifacts and crash tests. |
| Twelve weekends | One imported producer/GitHub Check only after consumer request. |
| Acceptance | Reconstruct after deletion; interrupted export never validates; exact checks; one real decision changes. |
| Kill | Pushed WIP+CI is enough; environments are retained anyway; no one uses/trusts record. |
| Demo | Kill task after edits, remove workspace, recover partial result, reject tamper, verify and continue. |
| Tweet | "Killed the agent and deleted its sandbox. The exact Git change is still inspectable, verifiable, and resumable." |
| Risk | Git formats, LFS/submodules/large repos, artifact growth and signing overclaims. |

### C. Safe Git Finalizer

| Field | Decision |
| --- | --- |
| Promise | Agents propose exact Git changes; a restart-safe finalizer publishes only the selected result. |
| Target | Small platform/security team keeping forge write credentials outside sandboxes. |
| Job | Check exact result, push deterministic branch, create/recover one draft PR without duplicates. |
| Workaround | Agent token/`gh`, CI push, platform integration. |
| Why OpenHands may fail | Agent-prompt GitHub path; no reviewed exact-result prepared/reconciled finalizer. |
| Why Fern | Exact repo/result/check, deterministic branch, durable phases, one PR POST and authoritative reads exist. |
| Journey | Runtime emits bundle; user/policy selects; finalizer writes intent, verifies, pushes, rereads, creates/finds exact draft PR. |
| Minimum architecture | Small daemon/CLI around GitHub App/publication/store, local bundle input and receipts. |
| Do not build | Agent runtime, chat, CI replacement, merge, broad SCM/policy language. |
| Two weekends | Local bundle + fake GitHub lost push/PR response + restart convergence + one test-repo draft PR. |
| Six weekends | App install, narrow request schema/check hook, OpenCode and one imported example. |
| Twelve weekends | OpenHands/GitHub Check only after external use. |
| Acceptance | Three external maintainers; one lost response converges; no agent write credential; stale generation denied; changes credential/retry policy. |
| Kill | No external install; Actions/logs enough; users keep agent tokens; generic support weakens constraints. |
| Demo | Uncredentialed agent bundle, dropped response, restart, exactly one branch/PR. |
| Tweet | "The agent never gets your GitHub write token. Fern verifies and publishes one exact change, even when GitHub's response is lost." |
| Risk | App/API/rules/forks/LFS complexity and weak urgency. |

## 10. Build Versus Integrate

| Path | Benefit | Cost | Decision |
| --- | --- | --- | --- |
| Independent Fern | Native UI and maximum reuse of exact semantics. | Must build environments/routing/retention/notifications; tiny market. | Conditional after bake-off. |
| OpenCode preset in OpenHands | Cheapest category test and distribution. | Still generic UI; no Fern finalizer. | Configure/contribute baseline first. |
| OpenHands as Fern UI | Rich features immediately. | Duplicates authority and erases native hypothesis. | No. |
| Agent Server runtime | Broad adapters/backends. | Python/service/conversation authority before job validation. | No initially. |
| Fern finalizer extension | Complements OpenHands and reuses strongest code. | Extension/demand unproved. | Conditional second test. |
| Correctness upstream | Helps more users and yields reproductions. | Not a Fern product. | Yes for concrete bake-off defects. |

Recommendation: run/configure the custom ACP baseline, contribute a preset or
bug only if concrete, and build independent native Fern only if the experience
gap is material. Never embed OpenHands merely to claim breadth.

## 11. Architecture Consequences

| Component | Decision | Trigger/reason |
| --- | --- | --- |
| Current persistent Docker | Keep unchanged. | Working appliance/comparison lane. |
| Per-attempt Docker | Prototype conditionally. | Smallest same-host isolation test. |
| Remote Docker runners | No. | Trigger after second physical host is used weekly. |
| k3s | No. | One host does not need scheduler/CNI/CRDs. |
| Agent Sandbox | No. | Only after Kubernetes is independently justified. |
| Agent Server | Baseline only. | Does not serve native path. |
| ACP | Baseline only. | Direct V2 preserves richer OpenCode semantics. |
| Direct OpenCode V2 | Yes; pin and black-box test the selected upgrade. | Exact IDs/UI; keep separate result authority. |
| GitHub App broker | Yes, optional. | Credential separation/safe publication. |
| Gateway | No. | Trigger for credential custody, budgets, accounting/routing or separate portfolio work. |
| SQLite | Yes. | One process/host. |
| Local artifact CAS | After prototype. | Needed for runtime-independent result. |
| Notification outbox | After native test passes. | Needed for unattended use, not wedge validation. |

The first profile remains one trusted owner and approved repositories. Use
unprivileged containers, dropped capabilities, `no-new-privileges`, default
seccomp, no Docker socket/host namespaces/devices, resource/PID/time/output
limits and task-only mounts. This reduces accidents, not hostile multi-tenancy.
Stop the writer before collection. Keep App credentials in Fern. Direct provider
credentials and open egress remain an explicit limitation.

## 12. Reproducible Bake-Off And Benchmarks

### Status

Specified, not run. Available Docker on macOS does not substitute for the
requested remote Ubuntu/provider/GitHub/phone test.

### Pin And Start

On disposable Ubuntu 24.04 with Node >=22.12, `uv`, Docker, Tailscale, exact
OpenCode V2, a capped provider account and disposable GitHub repository:

```bash
mkdir -p ~/fern-bakeoff/{logs,screenshots,projects,openhands-state}
cd ~/fern-bakeoff

git clone https://github.com/OpenHands/OpenHands.git source
git -C source checkout 64c1269655012698bc66538967989996191beb6c

date --iso-8601=seconds | tee logs/start.txt
uname -a | tee logs/uname.txt
cat /etc/os-release | tee logs/os-release.txt
docker version | tee logs/docker-version.txt
node --version | tee logs/node-version.txt
uv --version | tee logs/uv-version.txt
opencode2 --version | tee logs/opencode-version.txt

export OH_AGENT_SERVER_VERSION=1.44.1
export LOCAL_BACKEND_API_KEY="$(openssl rand -hex 32)"
export OH_SECRET_KEY="$(openssl rand -hex 32)"

npx --yes @openhands/agent-canvas@1.16.0 --public \
  >logs/openhands.log 2>&1
```

The documented container baseline mounts `~/.openhands` and `/projects`. Resolve
and retain the immutable digest; do not silently use `latest`:

```bash
docker pull ghcr.io/openhands/agent-canvas:1.16.0
docker image inspect ghcr.io/openhands/agent-canvas:1.16.0 \
  --format '{{json .RepoDigests}}' | tee logs/image-digest.txt

docker run --name agent-canvas --rm -p 8000:8000 \
  -e LOCAL_BACKEND_API_KEY="$LOCAL_BACKEND_API_KEY" \
  -e OH_SECRET_KEY="$OH_SECRET_KEY" \
  -v ~/fern-bakeoff/openhands-state:/home/openhands/.openhands \
  -v ~/fern-bakeoff/projects:/projects \
  ghcr.io/openhands/agent-canvas:1.16.0
```

If that exact image tag is unavailable, record it and build the pinned source.
Do not substitute a moving tag. In Canvas choose `Settings -> Agent -> ACP ->
Custom`, use the exact OpenCode ACP command exposed by the pinned build, and
record all path/credential/image work needed. Friction is a result.

### Cases

1. Run one supported ACP agent control and OpenCode treatment on the same exact
   repository/base/model-equivalent task.
2. Launch two conversations with default `worktree=false`, then enabled
   worktrees; prove whether writes share.
3. Close the client for 15 minutes and reconnect from phone over Tailscale.
4. Inspect conversation, tools, files, terminal and changes; steer/answer.
5. Compare Canvas ACP with the official OpenCode UI for forms, permissions,
   plugin/skill behavior, terminal, files, diffs and exact session continuity.
6. Restart Canvas/ingress, Agent Server during model stream, Agent Server during
   marker-bearing tool, Docker and host in separate runs.
7. Cancel during conversation start, workspace setup, model stream and delayed
   tool; prove process stop and attempt stale file/network effect.
8. Finish, retain/reopen, delete conversation only, then delete workspace/PVC in
   another case. Reconstruct result without old checkout or remote branch.
9. Redeliver same conversation UUID with different prompt; redeliver GitHub
   webhook with and without delivery ID.
10. Trigger branch/draft PR and identify which process owns credentials/effect.
11. Using a disposable repository and trusted `mitmproxy` CA, forward one
    GitHub mutation, then close downstream after upstream success. Restart and
    count exact matching refs/PRs. If interception is impossible, record `U`.
12. Run equivalent Fern fake-GitHub lost-response tests and one authorized real
    rehearsal only after the prototype exists.

For each case retain UTC times, versions/digests, host/backend, repo ID/base,
conversation/task/attempt/container IDs, process/container/Git/GitHub state,
terminal classification, reconstructability, screenshots/logs and uncertainty.

### Benchmark Matrix

| Metric | Command/method | OpenHands | Fern | Gate |
| --- | --- | --- | --- | --- |
| Clean install | Screen/timestamps OS-to-healthy UI. | `U` | `U` | Fern simplicity claim only if materially lower and no manual DB/container diagnosis. |
| First exact result | Clean host to reconstructable result. | `U` | `U` | <=30 min for intended owner. |
| Cached cold server | Stop/start to health and usable UI. | `U` | `U` | Fern p50 <=10s, p95 <=30s or no "instant" claim. |
| Warm reconnect | Phone request to usable running UI. | `U` | `U` | p95 <=3s on same tailnet. |
| Fresh task ready | Separate clone/setup/server/model phases. | `U` | `U` | No target until three real repos measured. |
| Idle CPU/RSS | `docker stats --no-stream`, cgroup/process RSS after 10 min. | `U` | `U` | Must materially beat chosen Canvas profile before "lighter". |
| Active CPU/RSS | Same marker task, provider excluded. | `U` | `U` | Two tasks fit target host without swap/OOM. |
| Disk growth | `du -x` before/after 10 tasks; separate cache/image/result. | `U` | `U` | Bounded retention and logs. |
| Control restart recovery | UI/service restart while runtime active. | `U` | `U` | p95 <=15s, no prompt replay. |
| Host restart recovery | Reboot with two tasks. | `U` | `U` | Classified/adopted <=60s after service ready; no false success. |
| Native UI open | Task click to exact session. | Unsupported path | `U` | p95 <=3s after health. |
| Backup/restore | Bytes/time after 10 tasks; clean host. | `U` | `U` | Exact result restored; <=30 min before reliability claim. |
| Services/ports/stores | `ps`, `ss`, mounts, DB/file inventory. | `D`: packaged Canvas/ingress/Agent/Automation plus backend/storage. | `R`: Fern, Docker/OpenCode, external Tailscale, SQLite/state/repo. | Compare operator interventions, not component count alone. |
| Upgrade | Active/completed tasks across pinned upgrade/rollback. | `U` | Repository tests only; physical `U`. | Explicit incompatible failure, no silent schema/profile migration. |

Publish raw runs. Three repetitions are a smoke comparison, not a market-wide
benchmark.

## 13. Positioning

### Homepage

> Run the real OpenCode on your own always-on machine, take over from any device,
> and keep the exact Git result when the task is done.

### README

> Fern is a personal remote appliance for OpenCode. It runs each coding task in
> its own retained environment on hardware you control, lets you open the task's
> actual OpenCode UI whenever it needs you, and preserves an exact Git result for
> trusted checks and draft-PR publication. Fern does not replace OpenCode's agent
> loop or editor, and it is not a hosted multi-agent platform.

### For An OpenHands User

Use OpenHands for a broad self-hosted canvas, agents, automations and backends.
Fern matters only if native OpenCode and an exact retained/finalized result are
materially better in the bake-off.

### For An OpenCode User

Fern leaves OpenCode intact and adds an always-on private task inbox, per-task
environments, lifecycle recovery, retained Git results and optional safe draft
PR delivery.

### For A Recruiter

Fern is a Go systems project around unreliable external effects: immutable
identities, write-ahead intent, idempotent admission, cancellation fences,
conservative recovery, exact Git snapshots, host checks and reconciled GitHub
publication around a pinned OpenCode runtime.

### Why Not OpenHands?

You probably should use OpenHands if custom ACP and its workspace lifecycle meet
your needs. Fern is narrower: the real OpenCode UI plus strict selected-result
and Fern-owned publication semantics. If users cannot see the difference, stop.

Do not headline "control plane", "evidence plane", "provenance", "BYOC" or
"Kubernetes-native".

## 14. Final Roadmap

### First Weekend

1. Run the pinned OpenHands control/OpenCode ACP subset: two conversations,
   phone reconnect, native UI determination, Agent Server restart and deletion.
2. Pin a current OpenCode V2 candidate and rerun prompt/inbox/event/form/
   permission/interrupt/restart black-box contracts.
3. Start an owner task log. Stop if there are not two candidate tasks/week.

### First Two Weekends

1. Keep current persistent workspace unchanged.
2. Build a separate thin Docker experiment with two full clones, state volumes,
   pinned servers and task-ID routes.
3. Persist environment/OpenCode IDs, prompt digest, base and container identity
   before work; never replay based on silence.
4. Restart Fern, stop one exact writer, export bundle/manifest, delete runtime
   and reconstruct cleanly.
5. Compare journey/timings with OpenHands.

Stop if native experience is not material, fewer than two tasks are useful,
exact restart identity fails, or scope requires Kubernetes/new UI/generic API.

### First Six Weekends

Only after the gate: serial durable lifecycle then concurrency two; artifact CAS;
verification/publication materialization; one notification outbox adapter;
retention/cleanup/disk/OOM/timeout states; calm phone/desktop task projections;
six real tasks over two weeks.

### First Twelve Weekends

Only after six-weekend acceptance: signed release, `init/doctor`, Ubuntu/systemd,
private TLS/WSS, backup/clean-host restore, fault suite, clear task/result/check/
attention/publication UI and one external installation. Test finalizer import or
portable record only when that user asks.

### Stop Conditions

- OpenHands ACP/T3/SSH is equivalent.
- Native attach is used less than weekly over four weeks.
- Fewer than half of 10 tasks produce useful unattended work.
- Setup/repair costs more than delegation saves.
- OpenCode needs a fork for bounded restart/cancel behavior.
- Retained exact results do not change recovery/review.
- No external OpenCode user completes an install/task.
- Kubernetes, Gateway or new editor is needed to make the demo compelling.

### Intentionally Excluded

Generic agents/UI, native mobile, org/RBAC/SSO/billing, schedules/webhooks,
previews, swarms, hostile public repos, auto-merge, broad SCM, model routing/
fallback/accounting, Kubernetes/warm pools, remote runners and hosted tenancy.

### Infrastructure Triggers

| Component | Add only when |
| --- | --- |
| Kubernetes/KAS | Multi-node placement, customer Kubernetes, cross-trust NetworkPolicy/workload identity or autoscaling is measured. Same-host concurrency is not a trigger. |
| Remote runners | Second physical host is used weekly and locality justifies leases/heartbeats/generation/artifact transfer. |
| Gateway | Provider-key custody, enforced budgets, cost accounting, routing/fallback or separately labeled portfolio goal is concrete. |
| Postgres/Redis/OIDC | Multiple replicas or distinct human/workload principals create measured requirements. |

Product work is native handoff, per-attempt Docker, exact retained result,
attention, verification/publication and install/restore. Kubernetes, distributed
Gateway, runner protocol, cross-harness kit, signed portable format and Labs are
portfolio-only until an external consumer requires them.

## 15. Primary Sources And Limitations

### Primary Sources

OpenHands and OpenCode:

- [Agent Canvas architecture](https://docs.openhands.dev/openhands/usage/agent-canvas/architecture), [setup](https://docs.openhands.dev/openhands/usage/agent-canvas/setup), [ACP agents](https://docs.openhands.dev/openhands/usage/agent-canvas/acp-agents), and [mobile access](https://docs.openhands.dev/openhands/usage/agent-canvas/mobile-access), docs pin `5a75b32`, accessed 2026-08-30.
- [OpenHands `1.16.0` source pin `64c1269`](https://github.com/OpenHands/OpenHands/tree/64c1269655012698bc66538967989996191beb6c).
- [OpenHands Agent Server/SDK `v1.44.1` pin `9d143aa`](https://github.com/OpenHands/software-agent-sdk/tree/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a).
- [OpenHands Automation `1.9.1` pin `e4535c8`](https://github.com/OpenHands/automation/tree/e4535c85ea158068f554255c44c2bfcf616aa566).
- [OpenCode V2 documentation](https://opencode.ai/v2/docs/), [V2 API](https://opencode.ai/v2/docs/api), [current source `4a977b2`](https://github.com/anomalyco/opencode/tree/4a977b2b3158adba43daec52fb3a9ab386dad3a8), published beta source [`106629a`](https://github.com/anomalyco/opencode/tree/106629aa118086be7def6123241a9bf056ba77b6), and [publish workflow 18684](https://github.com/anomalyco/opencode/actions/runs/33234821926).

Agent products:

- [Cursor Cloud Agents](https://cursor.com/docs/cloud-agent), [BYOM](https://cursor.com/docs/cloud-agent/bring-your-own-machine), and [pricing](https://cursor.com/pricing).
- [OpenAI Codex cloud](https://developers.openai.com/codex/cloud/), [Remote](https://developers.openai.com/codex/remote-connections), and [pricing](https://developers.openai.com/codex/pricing).
- [Claude Code Remote Control](https://code.claude.com/docs/en/remote-control), [self-hosted environments](https://code.claude.com/docs/en/self-hosted-environments), and [pricing](https://claude.com/pricing).
- [GitHub Copilot coding agent](https://docs.github.com/en/copilot/concepts/agents/coding-agent/about-coding-agent).
- [Google Jules changelog](https://jules.google/docs/changelog/) and [limits](https://jules.google/docs/usage-limits/).
- [Ona automations](https://ona.com/docs/ona/automations/overview) and [pricing](https://www.ona.com/pricing).
- [Warp documentation index](https://docs.warp.dev/llms.txt), [orchestration](https://docs.warp.dev/platform/orchestration/), and [self-hosting](https://docs.warp.dev/platform/self-hosting/).
- [Devin Outposts](https://docs.devin.ai/cloud/outposts/overview) and [Dynamic Workflows](https://docs.devin.ai/work-with-devin/dynamic-workflows).
- [Coder Agents](https://coder.com/docs/ai-coder/agents) and [pricing](https://coder.com/pricing).
- [T3 Code](https://github.com/pingdotgg/t3code), [remote access](https://github.com/pingdotgg/t3code/blob/main/docs/user/remote-access.md), and prior pinned `v0.0.35` architecture audit.
- [Orbit `aca9757`](https://github.com/jianghailong-xy/orbit/tree/aca97577e5e57cb2d6d39e503a035115e1cedd9c).
- [Warren](https://github.com/jayminwest/warren) and [runtime/supervisor contract](https://github.com/jayminwest/warren/blob/main/docs/design/runtime-and-supervisor.md); prior pin `fe10715`.
- [Deputies](https://github.com/sidpalas/deputies) and [architecture](https://github.com/sidpalas/deputies/blob/main/docs/architecture.md); prior pin `60c7e18`.
- [OpenAI Symphony specification](https://github.com/openai/symphony/blob/main/SPEC.md) and implementation; prior pin `8001b52`.

Runtime substrates:

- [Daytona pricing](https://www.daytona.io/pricing).
- [E2B pricing](https://e2b.dev/pricing).
- [Runloop Devboxes](https://docs.runloop.ai/devboxes/overview), [snapshots](https://docs.runloop.ai/devboxes/snapshots), and [pricing](https://www.runloop.ai/pricing).
- [Kubernetes Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) and [quickstart](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/examples/quickstart).

Repository-local source audits:

- [Homebase category report](./fern-homebase-category-report-2026-08-28.md).
- [Agent Change Record audit](./agent-change-record-competitor-audit-2026-08-28.md).
- [Strategy audit](./fern-strategy-audit-2026-08-28.md).
- [Agentic coding frontier](./agentic-coding-frontier-2026-08-28.md).
- [Personal task computers](./fern-personal-task-computers-2026-08-30.md).

### Limitations

- No complete remote OpenHands bake-off, paid vendor trial, target-Ubuntu Fern
  deployment, physical-phone run or controlled cross-product benchmark occurred.
- Public issue engagement establishes repeated reports, not incidence,
  willingness to pay or causal product preference.
- Proprietary systems may implement undocumented safeguards. `U` is not absence.
- Several 2026 products and OpenCode V2 surfaces are beta, preview or rapidly
  changing. Re-pin immediately before implementation.
- Fern's current test success does not prove physical Tailscale/TLS/WSS, provider,
  GitHub organization policy, reboot or replacement-host operation.
- The recommendation is intentionally reversible. It does not justify Fern by
  sunk cost.

## Final Decision Rule

No standalone Fern 2.0 should proceed today. The only route to a go decision is
the native OpenCode two-weekend experiment plus repeated owner use and one
external installation. Failure means **keep Fern as a personal/portfolio
appliance and stop expanding it**, not switch to Kubernetes, a generic canvas,
Gateway or multi-agent breadth.
