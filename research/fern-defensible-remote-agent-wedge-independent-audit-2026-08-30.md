# Independent Audit: Fern's Defensible Remote-Agent Wedge

**Research date:** 2026-08-30

**Repository baseline:** `ab945b5a00db3a310b3fcc30fe8bc99669598b6f`,
plus the uncommitted working-tree documents present during the audit

**Supporting protocol:**
[`fern-defensible-remote-agent-wedge-appendix-2026-08-30.md`](./fern-defensible-remote-agent-wedge-appendix-2026-08-30.md)

This is an independent product audit. Code and implemented tests are
authoritative over roadmap and target-architecture documents.

## Evidence Labels

| Label | Meaning |
| --- | --- |
| `T` | Tested in the Fern repository during this audit |
| `R` | Implemented in Fern code or asserted by checked-in tests |
| `S` | Observed in pinned external source |
| `D` | Current first-party documentation |
| `M` | Vendor marketing or vendor-reported result |
| `I` | Inference from named evidence |
| `U` | Unknown; not established by reviewed material |

`D` is not reliability evidence. `U` is not proof of absence. Conversation
persistence is not exact result retention, process state is not task outcome,
and a visible Stop button is not proof of external-effect fencing.

Local verification completed with Go 1.24.2 on macOS arm64:

```text
go test ./...
go test -race ./...
```

The OpenHands remote-VM bake-off was not run. The audit therefore does not
claim hands-on results for OpenHands, phone access, provider execution, or live
GitHub fault injection.

## 1. Executive Verdict

**Decision: Keep Fern as a personal appliance.**

The evidence does not yet justify a standalone Fern 2.0 product. OpenHands,
T3 Code, Orbit, Coder Agents, Warren, Deputies, and hosted cloud agents already
cover enough of the desired experience that remote execution, owner hardware,
mobile supervision, concurrency, isolation, persistence, and issue-to-PR cannot
be the wedge.

Fern's correctness is unusually strong, but the public evidence does not show
that users choose products based on exact prompt admission, attempt generations,
Git-object retention, or reconciled PR creation. Those properties matter when
failure occurs; they are not yet demonstrated acquisition or weekly-use drivers.

The best remaining product experiment is:

> **Run the real OpenCode on your own always-on machine, take over from any
> device, and retain an exact Git result after the task environment disappears.**

This combines two visible properties:

1. Native OpenCode continuity rather than a normalized agent conversation.
2. A reviewable result independent of the live environment, with publication
   authority kept outside the agent.

It is not yet a product verdict. OpenHands can plausibly run `opencode acp`, T3
already has direct OpenCode V2 support and native clients, and no repeated
evidence yet shows that users require the official OpenCode UI rather than a
competent generic surface.

Run the OpenHands comparison first. If it passes, build only a two-weekend
per-attempt Docker prototype. Do not start k3s, Kubernetes Agent Sandbox, a
remote runner protocol, a Gateway, or a new UI before that gate.

Graduate to **Build narrow OpenCode integration** only if real usage proves the
native handoff is materially better and used weekly. Otherwise finish the
current appliance as a portfolio project and stop expansion.

## 2. Repository Reality

### Shipped

- `R` One single-owner, single-host persistent OpenCode workspace in Docker.
- `R` Paired-device private access, wake/pause/restart reconciliation, bounded
  API adapters, backup/restore, release compatibility, and provenance gates.
- `R` Atomic durable task admission with immutable task, attempt, exact prompt,
  actor, OpenCode session, and message identities before external I/O.
- `R` Ambiguous prompt delivery is reconciled through exact observations rather
  than replayed under a new identity.
- `R` Cancellation fences later Fern-controlled effects; missing information is
  not converted into success.
- `R` Execution observation projects positive live states, while inactivity
  remains open.
- `R` Explicit user result sealing, exact base/result/tree collection, changed
  path manifest, optional host checks, and same-result verification.
- `R` GitHub App credential custody and receipt-backed branch/draft-PR
  publication with read reconciliation after ambiguous responses.
- `T` Normal and race-enabled Go test suites pass.

### Not Shipped

- Multiple independent workspaces or concurrent effecting attempts.
- One checkout, OpenCode state volume, and server per attempt.
- Generic authoritative automatic OpenCode completion.
- Restart-stable answers to every OpenCode question or permission.
- Retained Git object bytes independent of the configured live repository.
- Notification outbox, retained-environment cleanup, or disk-pressure policy.
- Remote runners, Kubernetes, Agent Sandbox, hostile multi-tenancy, or Gateway.
- Physical acceptance on the intended Ubuntu/Tailscale/phone path.

### Strategic Consequence

Fern has an implementation advantage for safe handoff and publication. It does
not yet have the user journey that makes the advantage visible. The smallest
correct next change is not a platform migration; it is a comparison and a
disposable Docker experiment beside the current manager.

## 3. Correction Register

| Claim | Evidence | Correction | Consequence | Confidence |
| --- | --- | --- | --- | --- |
| Self-hosted background coding is whitespace. | OpenHands, Coder, T3, Orbit, Warren, Deputies, and Symphony have owner-operated paths. | It is occupied. | Do not build a generic remote canvas. | High |
| OpenHands cannot run OpenCode. | Canvas accepts custom ACP commands; current OpenCode ships `opencode acp`. | It can plausibly run OpenCode as a custom, uncurated ACP integration. | Test it before claiming a native gap. | Medium |
| OpenHands preserves the native OpenCode experience. | Canvas renders normalized Agent Server/ACP events; no supported route to the official OpenCode UI was found. | Native UI access is unverified and likely outside the supported path. | This is a hypothesis, not established demand. | Medium |
| OSS Canvas gives every conversation an isolated sandbox. | The pinned Helm profile uses one shared pod/PVC; isolation depends on backend, and Enterprise adds per-run containers. | Name the backend and its policy. | Compare actual deployment profiles, not product names. | High |
| Persistent conversation means retained exact result. | OpenHands persists events/workspaces, but no mandatory post-fence Git bundle contract was found. | Transcript, workspace, and Git-object retention are distinct. | Delete the runtime and reconstruct the result in the bake-off. | High |
| OpenHands treats idle as complete. | Pinned status source makes `IDLE` nonterminal. | OpenHands is more conservative than that claim. | Fern must compare exact boundaries, not a straw man. | High |
| ACP supplies durability. | ACP covers session/update/permission/cancel vocabulary, not daemon persistence or effect recovery. | ACP is an adapter protocol. | Do not infer crash safety from protocol support. | High |
| OpenCode V2 wait is authoritative. | The endpoint is documented; source has returned `503` for unfinished V2 mutations in relevant builds. | Pin and black-box exact behavior. | Keep manual sealing until a stronger terminal contract passes faults. | High |
| Fern already preserves results after runtime deletion. | It records identities/manifests but not all required Git object bytes. | A commit ID is not an artifact. | Add a bundle/CAS only in the narrow experiment. | High |
| Fern prevents every stale GitHub write. | Fern fences its App path; agent-held tokens and arbitrary network effects remain outside it. | Scope the claim to Fern-controlled credentials/effects. | Remove or disclose alternate write credentials. | High |
| Safe publication is a validated product. | Strong implementation, weak repeated demand evidence. | It is a component hypothesis. | Test with external maintainers before extraction. | High |
| Personal remote hardware differentiates Fern. | T3, OpenHands, Coder, Orbit, SSH/Tailscale, Codex, and Claude cover owner hardware or connected hosts. | Hardware ownership alone is table stakes. | Lead only with a proven native handoff. | High |
| Fern is faster or lighter. | No same-host benchmark exists. | Performance is unknown. | Use the benchmark protocol before making claims. | High |
| Kubernetes creates the wedge. | Agent Sandbox is infrastructure and Docker can run several tasks on one host. | Kubernetes changes the substrate, not the job. | Remove it from the first twelve-weekend path. | High |
| Session teleport is incremental. | No supported contract was found for atomic session, workspace, process, and credential migration. | Teleport is a separate research project. | Reject it from the initial product. | High |
| Strong internals guarantee adoption. | Most guarantees appear only under rare faults. | Engineering advantage is not demand. | Apply explicit usage and kill gates. | High |

## 4. OpenHands Gap Audit

Pinned review: Agent Canvas `v1.16.0` at
[`64c1269`](https://github.com/OpenHands/OpenHands/commit/64c1269655012698bc66538967989996191beb6c)
and Agent Server/SDK `v1.44.1` at
[`9d143aa`](https://github.com/OpenHands/software-agent-sdk/commit/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a).

| # | Question | Finding | Evidence class |
| ---: | --- | --- | --- |
| 1 | Can OpenHands run OpenCode? | Plausibly through custom ACP; not hands-on verified. | `D/S` |
| 2 | Preset or custom? | Custom command, not a built-in OpenCode preset at the pin. | `S` |
| 3 | Native OpenCode UI? | No supported native route found; Canvas renders its own ACP surface. | `D/S/I` |
| 4 | Fresh isolated OSS environment per conversation/run? | Backend-dependent; the OSS Helm profile commingles runs in one pod/PVC. | `D` |
| 5 | Completed environment retained/reopened? | Possible with persistent backend storage; no universal retention contract. | `D/U` |
| 6 | What survives Canvas restart? | Backend-persisted conversation/settings survive; frontend reconnects to history. | `S` |
| 7 | What survives Agent Server restart? | Persisted history survives; source recovery changes interrupted `RUNNING` work to `ERROR`, not transparent continuation. | `S` |
| 8 | What survives sandbox deletion? | Backend-specific conversation data may survive; no mandatory exact Git artifact was found. | `U` |
| 9 | Prompt dedupe after ambiguous response? | Caller conversation UUID helps creation dedupe, but message requests have no operation ID/digest contract. | `S/I` |
| 10 | Completion classification? | `FINISHED`, `ERROR`, and `STUCK` terminal; `IDLE` nonterminal. `FINISHED` is not trusted Git verification. | `S` |
| 11 | Can idle/silence complete? | Not in the pinned status model. | `S` |
| 12 | Cancellation/provisioning races? | Pause/interrupt exist; source paths do not establish a complete provisioning and external-effect fence. | `S/U` |
| 13 | Can stale worker mutate GitHub? | Conversation lease generations fence persistence writes, not every shell/network/GitHub effect. | `S/I` |
| 14 | Attempt generations/fencing epochs? | Conversation lease generations exist; no end-to-end task/effect epoch was found. | `S/U` |
| 15 | Agent or finalizer creates branch/PR? | Canvas controls produce natural-language agent prompts for commit/push/PR. | `S` |
| 16 | Ambiguous push/PR reconciliation? | No general prepared-effect/read-reconciliation contract found. | `U` |
| 17 | Exact Git objects retained independently? | No mandatory bundle/object artifact found; archives have different guarantees. | `U` |
| 18 | Reconstruct after deletion? | Unknown and backend-dependent. | `U` |
| 19 | Mobile attention/intervention? | Responsive browser and Tailscale/ngrok guidance; browser interactions exist; no durable OS push/outbox contract found. | `D/U` |
| 20 | Personal-machine operations? | Installation can be compact, but strict per-task isolation, auth, recovery, and diagnosis are not default in the OSS Helm profile. | `D/I` |

### OpenHands Conclusion

OpenHands already wins the broad feature comparison: multiple conversations,
several agents, automations, files, terminal, changes, responsive access, and
multiple backends. It also has real correctness machinery: persisted events,
nonterminal idle, restart error recovery, and conversation lease generations.

The source-observed or still-unknown gaps are narrower: OpenCode-native UI,
strict message-operation idempotency, default per-attempt OSS isolation,
stop-proven cancellation, generation-bound external effects, exact Git artifact
retention, and host-owned publication reconciliation. None has standalone demand
proof.

## 5. Competitive Capability Matrix

Performance cells are `U` unless a comparable hands-on benchmark exists. Vendor
cold-start claims are not treated as measurements.

| Product | Hosting/control and runtime | Handoff/mobile | Durability/result/publication | Price, license, and limitation |
| --- | --- | --- | --- | --- |
| Fern today | `R` Owner-hosted, one Docker/OpenCode workspace | `R` Official OpenCode UI and phone web task page | `R` Strong exact IDs, seal, checks, App finalizer; no independent Git bytes | Unreleased; one owner/workspace; performance `U` |
| OpenHands Agent Canvas | `D/S` MIT self-hosted UI/server; local, Docker, VM, remote, cloud, K8s; custom ACP | `D` Responsive Canvas, files/terminal/changes | `S/U` Persisted events; backend retention; no mandatory exact finalizer found | MIT; OSS Helm shared pod/PVC; Enterprise isolation |
| Cursor Cloud Agents | `D` Vendor loop with managed VMs or customer workers | `D` Web/desktop/iOS/PWA and remote desktop | `D/U` Durable runs/artifacts/builds; exact fencing `U` | Proprietary, from $20/month plus usage |
| OpenAI Codex | `D` OpenAI cloud or connected local/SSH host | `D` CLI/IDE/desktop/web/mobile remote | `D/U` Cloud diffs/apply; exact worker-loss/finalizer contract `U` | Proprietary cloud; open CLI; plan limits |
| Claude Code | `D` Local, Anthropic cloud, or customer runner under Anthropic service | `D` Web/mobile Remote Control and teleport | `D/U` Requeue/resume documented; exact artifact/finalizer `U` | Proprietary, plan and usage priced |
| GitHub Copilot agent | `D` Actions environment or customer runner | `D` GitHub web/mobile issue-to-PR | `D` GitHub-owned branch/PR; 59-minute task limit; restart checkpoint `U` | Proprietary, paid Copilot plus credits/minutes |
| Google Jules | `D` Hosted task VMs | `D` Mobile web, plans, activities, notifications | `D/U` Git patches/branches/PRs; crash fencing `U` | Proprietary; daily/concurrency plan limits |
| Ona | `D` Managed cloud or Ona-managed customer VPC | `D` Web/mobile-browser workflows | `D/U` Managed lifecycle; environments expire; exact finalizer `U` | Proprietary, from $20/month plus usage |
| Warp | `D` Vendor or customer-hosted workers, vendor session layer | `D` Terminal/web, attach and steer | `D/U` Run records; exact cancel/result/publication `U` | Proprietary, plan priced |
| Devin | `D` Devin VMs or Outposts customer workers | `D` Browser IDE/shell and takeover | `D/U` Workflow stage resume; exact Git custody `U` | Proprietary, plan priced |
| Coder Agents | `D/S` AGPL self-hosted control and Terraform workspaces | `D` Web/API, questions, follow-ups | `D/U` DB chat survives workspace lifecycle; publication recovery `U` | Community AGPL; enterprise plans; high setup |
| T3 Code | `S` MIT owner-hosted server; OpenCode and other CLIs | `D/S` Native mobile, web, desktop, relay/Tailscale/SSH | `S/U` Event-sourced threads, receipts, Git checkpoints; finalizer `U` | MIT/free; early and changing |
| Orbit | `S` MIT self-hosted Postgres plus outbound runners; OpenCode supported | `S` Web and Apple clients, approvals | `S/U` Durable tasks/checkpoints; exact PR finalizer not located | MIT/free; pre-1.0 and multi-service |
| Warren | `S` MIT self-hosted; local/Docker/K8s jobs | `D/S` Responsive web, steer/cancel | `S/U` Watchdog/salvage and host push; complete effect fence `U` | MIT/free; young project |
| Deputies | `S` MIT self-hosted; many sandbox providers | `D/S` Web, integrations, queued steering | `S/U` Postgres queues/leases/callbacks; exact publication chain `U` | MIT; Postgres/object-store operations |
| OpenAI Symphony | `S` Apache-2.0 self-hosted Codex issue scheduler | `S` Work-tracker/status oriented | `S/U` Retry/reconciliation spec; publication workflow-defined | Experimental Apache-2.0 reference |
| Daytona | `D` Managed/BYOC sandbox substrate | API/SDK, not native agent handoff | Caller owns task, Git, and publication semantics | Usage priced; no complete agent product |
| E2B | `D` Managed VM substrate; enterprise self-host paths | API/SDK | Caller owns durability and publication | Apache-2.0 infra/SDK; plan and session limits |
| Runloop | `D` Managed microVM devboxes | API/browser/shell substrate | Snapshots/suspend; caller owns task and Git policy | Proprietary, usage and plan priced |
| Kubernetes Agent Sandbox | `D/S` Customer-operated K8s CRD/controller | No agent UI | PVC/pause/claim primitives; no task outcome or Git authority | Apache-2.0, `v1beta1`, cluster operations required |

### Matrix Conclusion

OpenHands and T3 undermine generic OpenCode remote-control positioning. Orbit,
Warren, Deputies, Coder, and Symphony undermine generic self-hosted durability
positioning. Hosted products are ahead on distribution, prepared environments,
notifications, and polished takeover. Sandbox vendors remove any reason for Fern
to build a sandbox abstraction as the wedge.

The narrow composition not clearly shipped as one contract is:

```text
native OpenCode task
  -> isolated retained environment
  -> exact reconstructable Git result
  -> trusted checks
  -> separate reconciled publication
```

The market gap is source-observed; demand remains unproved.

## 6. User-Pain Evidence

| Problem | Evidence of recurrence/severity | Workaround and existing solvers | Fern fit | Product conclusion |
| --- | --- | --- | --- | --- |
| Remote/mobile state becomes stale or disconnected | OpenCode [#17769](https://github.com/anomalyco/opencode/issues/17769), Claude [#29726](https://github.com/anthropics/claude-code/issues/29726), and Codex [#23011](https://github.com/openai/codex/issues/23011) show cross-client divergence/reconnect failures. | Refresh/reconnect, SSH/tmux; vendors continue improving native clients. | Exact IDs and conservative projections help; Fern has no outbox. | Real pain, but mobile control is occupied. |
| Server restart leaves uncertain or stuck work | OpenCode [#19023](https://github.com/anomalyco/opencode/issues/19023) links a cluster of orphaned-message/tool symptoms. | Restart, send a continuation, inspect Git/processes manually. | Strong match to Fern's no-false-success policy. | Supports a flight-recorder feature, not proven acquisition. |
| Background agents disappear without useful completion | Claude [#63023](https://github.com/anthropics/claude-code/issues/63023) and OpenCode [#11865](https://github.com/anomalyco/opencode/issues/11865) show silent death, hangs, and missing callbacks. | Poll, hooks, WIP pushes, watchdogs. | Fern can retain task/result facts outside the agent process. | Strong repeated pain; exact Fern solution still untested. |
| Cancellation does not prove process death | Remote-agent issue clusters describe orphan processes and stale status; OpenHands interrupt source has bounded waiting. | SSH kill, runtime restart, VM TTL. | Fern already distinguishes cancellation intent and acknowledgment. | High-severity systems problem, weak purchasing evidence. |
| Parallel checkouts interfere | Worktree, port, service, and state collisions recur across agent products. | One clone/container per task; cloud agents already solve much of it. | Per-attempt Docker is natural but not implemented. | Required product quality, not differentiation. |
| Permission friction or broad credentials | Repeated Claude, Codex, Cursor, and OpenCode discussions trade repeated prompts against unsafe broad authority. | YOLO mode, allowlists, VMs, short-lived tokens. | App finalizer removes GitHub publication credential from agent. | Credible finalizer hypothesis; broad capability security is out of scope. |
| Cost/quota surprises | Large issue/forum threads recur across Claude, Codex, and Cursor. | Provider dashboards, cheaper models, gateways, manual budgets. | Weak current fit; Gateway absent. | Do not pivot to metering before core use. |
| Environment setup/self-hosting is difficult | OpenHands issues and Docker-in-Docker discussions repeatedly show networking, mount, and version failures. | Managed agents, Coder, Daytona, prebuilt images. | One binary/SQLite could help, but has no benchmark. | Packaging requirement, not defensibility. |
| Review throughput becomes bottleneck | Industry reports and user discussions repeatedly emphasize plausible changes, CI, and review load. | Smaller PRs, CI, second-agent review. | Same-result checks prevent evidence drift but not semantic errors. | Exact handoff improves hygiene; never claim proof of correctness. |

### Pain Not Yet Established

The audit did not find repeated evidence strong enough to establish demand for:

- the official OpenCode UI rather than Canvas/T3;
- live OpenCode session migration between hosts;
- Git-object reconstruction after deletion rather than an early WIP push;
- duplicate-PR protection as a frequent end-user problem;
- attempt epochs as a buying criterion;
- a native Fern mobile application.

These are the central unknowns behind the personal-appliance verdict.

## 7. Emerging Primitives

| Primitive | Recent feasibility change | Opportunity | Constraint |
| --- | --- | --- | --- |
| Durable OpenCode input/history | Current V2 work adds durable prompt inbox and session-scoped replay. | Bind Fern task to exact native IDs without terminal scraping. | Beta/version-specific; wait and restart semantics remain incomplete. |
| ACP | Multiple agents and clients now share a common interaction protocol. | Cheap OpenHands baseline and future adapters. | No persistence or effect guarantee. |
| Agent client/server modes | OpenCode, Codex, Claude, and Pi expose structured service/headless surfaces. | Reattach without replacing agent loop. | Native semantics differ materially. |
| Connected-host mobile control | Codex, Claude, T3, and others validate cross-device supervision. | Confirms the job. | Removes generic remote control as whitespace. |
| Prepared disposable environments | Cloud builds, snapshots, Dev Containers, and sandbox APIs are practical. | Isolated task computers are feasible for one developer. | Environment setup remains product work. |
| Suspend/retain/fork | Devbox and K8s substrates expose lifecycle primitives. | Retained task environment and result branching. | Infrastructure state is not exact result custody. |
| Git bundles and content addressing | Mature Git primitives can package exact reachable objects. | Preserve a result after deleting compute. | Old primitive; agent-specific value must be demonstrated. |
| Scoped workload credentials | GitHub Apps, OIDC, and brokered credentials are common. | Keep final publication authority outside agent. | Fern only controls its broker path today. |
| Durable attention state | Products expose blocked/approval states and notifications. | Leave and re-enter safely. | Fern lacks durable answer/outbox contracts. |
| Typed external actions | Durable workflows increasingly separate intent from mutation. | Reconcile lost GitHub responses. | Arbitrary agent shell/network effects remain outside the boundary. |

The new capability is the composition, not any individual primitive. It became
practical to preserve the real agent session while independently retaining and
finalizing its Git result. Whether that composition is valuable is the test.

## 8. Candidate Wedge Ranking

Scores are favorable from 1 to 5. For alternatives, 5 means poorly solved. For
size and operations, 5 means small/easy.

Weights prioritize pain (15%), alternatives (12%), Fern advantage (13%),
OpenCode fit (10%), visibility (10%), personal use (10%), size (8%), operations
(5%), defensibility (7%), demonstration (5%), clarity (4%), and Grab relevance
(1%). Product demand dominates portfolio relevance deliberately.

| Rank | Candidate | Pain | Alt | Fern | OC | Vis | Use | Size | Ops | Def | Demo | Clear | Grab | Raw/60 | Weighted/5 | Decision |
| ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 1 | Native OpenCode background-to-interactive handoff | 3 | 4 | 4 | 5 | 5 | 5 | 3 | 4 | 3 | 5 | 5 | 2 | 48 | 4.07 | Prototype only |
| 2 | Failure-safe exact change handoff | 3 | 4 | 5 | 4 | 4 | 4 | 3 | 4 | 4 | 4 | 4 | 4 | 47 | 3.90 | Compose with rank 1 |
| 3 | Safe Git finalizer | 3 | 4 | 5 | 4 | 3 | 4 | 4 | 4 | 4 | 4 | 4 | 4 | 47 | 3.88 | External demand test |
| 4 | Durable personal attention inbox | 4 | 3 | 3 | 4 | 5 | 5 | 3 | 3 | 2 | 5 | 5 | 3 | 45 | 3.76 | Feature, not wedge |
| 5 | Retained task computer/forking | 4 | 2 | 3 | 5 | 5 | 5 | 2 | 2 | 2 | 5 | 5 | 2 | 42 | 3.60 | Too broad initially |
| 6 | Instant OpenCode tasks | 4 | 2 | 2 | 4 | 5 | 5 | 2 | 2 | 1 | 5 | 5 | 2 | 39 | 3.30 | Benchmark only |
| 7 | Session teleport | 3 | 4 | 2 | 2 | 5 | 4 | 1 | 3 | 4 | 5 | 5 | 2 | 40 | 3.27 | Reject: no contract |
| 8 | Private OpenCode cloud | 3 | 1 | 3 | 5 | 4 | 5 | 2 | 3 | 1 | 5 | 5 | 2 | 39 | 3.21 | Reject: occupied |
| 9 | Runtime conformance kit | 2 | 4 | 4 | 3 | 2 | 2 | 4 | 4 | 3 | 4 | 3 | 5 | 40 | 3.10 | Portfolio only |
| 10 | OpenHands finalizer extension | 3 | 4 | 4 | 3 | 3 | 2 | 3 | 3 | 2 | 3 | 3 | 4 | 37 | 3.09 | Conditional integration |
| 11 | Generic remote-agent canvas | 4 | 1 | 2 | 2 | 4 | 4 | 1 | 1 | 1 | 4 | 3 | 3 | 30 | 2.53 | Reject |

The top score ranks experiments, not validated products. Native handoff still
fails the final decision rule because weekly use and superiority to OpenHands
have not been observed.

## 9. Top Three Concepts

### A. Native OpenCode Handoff

**Promise:** Run the real OpenCode on an always-on machine, leave it working,
and re-enter the exact native session from any device.

**Target user:** OpenCode power users with an always-on Linux machine and several
substantial repository tasks per week.

**Repeated job:** Start a task remotely, leave, inspect or answer it later in
OpenCode, then retain one reviewable result.

**Current workaround:** SSH/tmux, `opencode serve` over Tailscale, T3 Code,
OpenHands custom ACP, or a hosted agent.

**Why OpenHands may be insufficient:** Canvas presents a normalized ACP surface;
no supported official OpenCode UI route was found. This must be tested.

**Why Fern can win:** It already proxies the official origin, binds exact native
IDs to a durable task, and separates snapshot authority from agent status.

**Journey:** Choose repository and base, submit, close client, receive attention,
open the exact OpenCode session, answer or steer, leave, return to a reviewable
result, run trusted checks, and optionally publish through Fern.

**Minimum architecture:** Existing Go/SQLite/Tailscale boundaries, per-attempt
Docker checkout/server/state, direct pinned OpenCode V2 API, task-ID routing,
explicit seal, Git bundle, and optional existing GitHub App broker.

**Do not build:** Kubernetes, a replacement editor/chat, generic ACP runtime,
remote runners, organizations, Gateway, previews, schedules, or native mobile.

**Two weekends:** First run the OpenHands ACP comparison. Then launch two
disposable OpenCode containers, route both official UIs, restart Fern, stop one
writer, and materialize one exact bundle after deleting its runtime.

**Six weekends:** Serial durable lifecycle, then concurrency two, local artifact
store, restart/cleanup reconciliation, one notification adapter, and adapted
verification/publication.

**Twelve weekends:** Install/doctor, backup/restore, fault suite, mobile task
detail, measured performance, and one external installation.

**Acceptance:** Six real tasks in two weeks, two concurrent pairs, at least 60%
useful unattended yield, native UI used meaningfully in at least 25%, p95 native
reconnect under three seconds after readiness, no prompt loss/replay on Fern
restart, and clean reconstruction after runtime deletion.

**Kill:** OpenHands ACP or T3 is equally good; native UI is rarely opened; setup
dominates; fewer than half of tasks are useful; or the prototype expands into a
platform to work.

**Demo:** Submit two phone tasks, restart Fern, enter one official OpenCode UI,
answer it, delete a completed container, reconstruct its exact result, and open
one draft PR through Fern.

**Tweet:** Fern keeps the real OpenCode running on your server. Start remotely,
take over in the native UI, and keep the exact Git result after the task computer
is gone.

**Maintenance risk:** High OpenCode beta/API/UI churn and container/provider
compatibility work.

### B. Failure-Safe Exact Change Handoff

**Promise:** A background task can fail or disappear without taking its exact
reviewable Git result with it.

**Target user:** Maintainers delegating long tasks who do not want a transcript
or live sandbox to be the only copy of useful work.

**Repeated job:** Recover exact partial or complete changes, verify that state,
and preserve it before cleanup.

**Current workaround:** Keep workspaces indefinitely, push WIP early, or download
patches.

**Why OpenHands may be insufficient:** No mandatory independent Git-object
artifact bound to stopped-writer verification was found.

**Why Fern can win:** Exact base/result/tree, manifest, manual authority,
same-result checks, uncertainty, and publication reconciliation largely exist.

**Journey:** See `Interrupted, work preserved`, materialize the exact result,
inspect/check it, continue from it or publish only the selected object.

**Minimum architecture:** Local content-addressed artifact directory, atomic Git
bundle ingestion, manifest, clean materializer, and existing result/check code.

**Do not build:** Process checkpointing, VM snapshots, universal adapters, video
replay, or claims that a host record proves model behavior.

**Two weekends:** Export one sealed result, hide the source repository,
materialize in a clean clone, and reject missing/tampered/interrupted artifacts.

**Six weekends:** Atomic artifact store, retention/cleanup reconciliation,
bounded logs/checks, and export fault tests.

**Twelve weekends:** One external imported producer and GitHub Check only if a
real maintainer requests them.

**Acceptance:** Reconstruction after deleting runtime/checkout, no valid-looking
partial exports, exact-commit verification, and one real changed decision.

**Kill:** Early WIP branches plus CI are sufficient or maintainers never use the
artifact.

**Demo:** Kill the task, delete its sandbox, recover exact work, reject tampering,
verify, and continue.

**Tweet:** The agent and sandbox are gone; the exact Git change is still here to
inspect, verify, and continue.

**Maintenance risk:** Git formats, partial clones, submodules, LFS, artifact
growth, and pressure to overclaim attestation.

### C. Safe Git Finalizer

**Promise:** Agents propose exact changes; a separate restart-safe finalizer
publishes only the result selected by the user or policy.

**Target user:** Small platform/security teams unwilling to place GitHub write
credentials and retry policy inside agent sandboxes.

**Repeated job:** Verify one result, push a deterministic branch, and converge on
one draft PR after timeout or restart.

**Current workaround:** Agent-held `gh`, CI push jobs, or vendor Git integrations.

**Why OpenHands may be insufficient:** Canvas publication controls prompt the
agent; no equivalent result-bound write-ahead publication transaction was found.

**Why Fern can win:** The App broker, exact branch/ref identity, started phases,
read reconciliation, and stale-state checks are implemented.

**Journey:** Runtime emits bundle, finalizer shows exact base/result/checks, user
selects, finalizer records intent, pushes, creates a draft PR, and adopts the
existing effect after a lost response.

**Minimum architecture:** Small CLI/daemon around current publication packages,
SQLite receipts, local bundle input, GitHub App identity, and no agent runtime.

**Do not build:** Chat, model routing, CI replacement, merge, broad SCM, hosted
secrets, or policy language.

**Two weekends:** Fake GitHub lost-response injection plus one real disposable
draft PR from an uncredentialed agent result.

**Six weekends:** Narrow App onboarding, signed immutable request, verification
hook, diagnostics, and adversarial tests.

**Twelve weekends:** OpenHands/GitHub Check integration only after three real
external users.

**Acceptance:** Three maintainers use it, one lost response converges without
duplicate PR, no agent credential, stale generation denied, and one adopter
changes credential/retry policy.

**Kill:** No external install, GitHub Actions is enough, users prefer agent-held
tokens, or generic support weakens exact constraints.

**Demo:** Drop push/PR responses, restart the finalizer, and show one correct
branch and one draft PR.

**Tweet:** The agent never gets your GitHub write token. Fern verifies its exact
change and safely converges on one draft PR after a lost response.

**Maintenance risk:** App onboarding, GitHub API/rules, Git transport ambiguity,
forks, LFS/submodules, and low perceived value for individuals.

## 10. OpenHands Build Versus Integrate

| Path | Benefit | Cost/mismatch | Decision |
| --- | --- | --- | --- |
| Independent Fern | Preserves official OpenCode UI and current exact-result/finalizer code | Must build environments, routing, retention, notifications; tiny audience | Conditional after bake-off |
| Add OpenCode preset to OpenHands | Cheapest way to test generic Canvas adequacy and help existing users | Does not itself add native UI or Fern finalizer | Configure/test first; upstream concrete fixes |
| Use OpenHands as Fern UI | Gains broad files/terminal/automation UI | Duplicates authority and erases native hypothesis | No |
| Use Agent Server as Fern runtime | Gains ACP/backend adapters | Adds Python and another persistence boundary before validation | No initially |
| Fern finalizer extension | Reuses strongest code and complements OpenHands | Demand and extension contract unproved | Second experiment only |
| Correctness contributions upstream | Benefits more users from observed faults | Not a Fern product | Yes for reproducible bake-off defects |

**Recommendation:** Configure the smallest OpenCode ACP baseline in OpenHands.
If it is good enough, contribute concrete defects and keep Fern personal. If the
native experience wins clearly, keep Fern direct-to-OpenCode. Do not embed
OpenHands or normalize OpenCode through Agent Server in the first product.

## 11. Architecture Consequences

| Component | Decision | Product reason or trigger |
| --- | --- | --- |
| Current persistent Docker workspace | Keep | Working appliance and control lane |
| Per-attempt Docker | Prototype conditionally | Smallest two-task/native/result test |
| Remote Docker runners | No | Trigger only after a second physical host is used weekly |
| k3s | No | No placement, CNI, HA, or cluster requirement |
| Kubernetes Agent Sandbox | No | Consume only after Kubernetes is independently required |
| OpenHands Agent Server | Baseline only | Useful comparison, not native implementation dependency |
| ACP | Baseline only | OpenHands comparison; not a durability boundary |
| Direct OpenCode V2 API | Yes, pinned | Exact native IDs and official UI; retain manual seal |
| GitHub App broker | Yes, optional | Visible least-authority publication benefit |
| Model Gateway | No | Trigger on credential custody, budgets, cost, routing, or separate portfolio work |
| SQLite | Yes | One process/host does not need distributed state |
| Local artifact CAS | After prototype | Required only if exact deletion-independent result matters |
| Notification outbox | After native gate | Required utility, not differentiation |

The first profile remains one trusted owner with selected repositories. Docker
limits reduce accidents but are not hostile multi-tenant isolation. Stop the
exact writer before trusted collection. Keep GitHub App credentials outside the
agent. Direct provider credentials and open egress remain disclosed limitations.

## 12. Product Positioning

These words are conditional on passing the experiment.

**Homepage:**

> Run the real OpenCode on your always-on machine, take over from any device,
> and keep the exact Git result when the task is done.

**README:**

> Fern is a personal remote appliance for OpenCode. It runs each task in its own
> retained environment on hardware you control, opens the task's actual
> OpenCode UI when you need to inspect or steer it, and preserves an exact Git
> result for trusted checks and optional draft-PR publication. Fern does not
> replace OpenCode's agent loop or editor, and it is not a hosted multi-agent
> platform.

**For an OpenHands user:**

> Use OpenHands for a broad self-hosted canvas, several agents, automations, and
> multiple backends. Fern is only interesting if every task must remain a native
> OpenCode environment and a separate process must retain, verify, and publish
> one selected Git result.

**For an OpenCode user:**

> Fern leaves OpenCode intact and adds an always-on private task inbox,
> per-task environments, restart recovery, retained Git results, and optional
> safe draft-PR delivery.

**For a recruiter:**

> Fern is a Go distributed-systems project around coding-agent effects:
> immutable identities, write-ahead intent, idempotent admission, cancellation
> fences, conservative crash recovery, exact Git snapshots, host verification,
> and reconciled GitHub publication around a pinned runtime.

**Why not OpenHands?**

> You probably should use OpenHands if its OpenCode ACP experience and workspace
> lifecycle meet your needs. Fern is narrower: the official OpenCode UI remains
> the task surface, while Fern retains and safely publishes one exact selected
> result. If that difference is not visible in a bake-off, Fern should not
> become a competing product.

Do not lead with control plane, evidence plane, provenance, BYOC, or
Kubernetes-native. Those are implementation categories, not the user job.

## 13. Final Roadmap

### First Weekend: Compare Before Building

1. Pin Canvas, Agent Server, OpenCode, images, and OpenAPI hashes.
2. Configure OpenCode custom ACP in the OSS single-owner profile.
3. Run a real task, laptop disconnect, phone reconnect, steering, process
   restarts, retention/reopen, workspace deletion, and result inspection.
4. Record every native capability lost, preserved, or equally good.
5. Measure install, first task, cold/warm start, reconnect, UI open, memory, CPU,
   disk, and recovery using the supporting protocol.

**Stop:** OpenHands is acceptable and official OpenCode adds no repeated value.

### First Two Weekends: Falsify Native Handoff

1. Keep the current workspace untouched.
2. Start two exact-base Docker/OpenCode attempts with distinct state.
3. Route immutable task IDs to official UIs.
4. Persist exact identities before execution and never replay on silence.
5. Restart Fern while both run.
6. Stop one writer, export Git bundle/manifest, delete runtime, and reconstruct.

**Stop:** Fewer than two useful tasks, no clear native advantage, identity loss
on restart, or scope expands into Kubernetes/new UI/generic runtime.

### First Six Weekends: One Reliable Appliance

1. Add serial durable isolated lifecycle, then concurrency two after faults.
2. Add atomic local artifact ingestion and materialization.
3. Adapt trusted verification/publication to per-attempt results.
4. Add one transactional notification destination.
5. Add retention, cleanup, disk, timeout, OOM, and image-failure states.
6. Dogfood at least six real tasks over two weeks and repeat the comparison.

### First Twelve Weekends: Decide Product Or Portfolio

1. Ship init/doctor, signed release, Ubuntu/systemd, private TLS/WSS, backup, and
   replacement-host restore.
2. Complete provisioning/model/tool cancellation, Fern/Docker restart, lost
   GitHub response, stale attempt, and interrupted cleanup faults.
3. Make result, checks, retention, attention, and publication clear on phone and
   desktop.
4. Obtain one external OpenCode-user installation.

Graduate only if the owner uses it weekly, native handoff beats OpenHands/T3/SSH,
most tasks yield useful unattended results, exact retention matters, and one
external user completes the flow. Otherwise publish the engineering artifact
and stop.

### Explicit Exclusions

- Multiple harnesses and generic agent adapters.
- New chat/editor/terminal UI.
- Kubernetes, Agent Sandbox, warm pools, and Fern operator.
- Remote runners, customer clusters, multi-tenancy, RBAC, SSO, or billing.
- Native mobile applications, schedules, issue automation, or workflow builder.
- General previews, browser-computer infrastructure, agent swarms, or auto-merge.
- General Gateway, routing/fallback, or cost chargeback.
- Session teleport, process checkpointing, or memory snapshots.

### Infrastructure Triggers

| Component | Trigger |
| --- | --- |
| Kubernetes | Four weeks of sustained multi-node placement, customer K8s demand, cross-trust NetworkPolicy/workload identity, or autoscaling Docker cannot meet |
| Remote runners | A second physical execution host used weekly with a measured source/network locality need |
| Gateway | Required provider-key custody, per-task budgets/cost, routing/fallback, or explicitly separate portfolio objective |
| PostgreSQL/Redis/OIDC | Multiple Fern replicas or distinct principals with measured contention/failure requirements |

### Product Versus Portfolio

| Product work | Portfolio-only unless demand appears |
| --- | --- |
| OpenHands bake-off, native handoff, per-attempt Docker, retained bundle, task UI, notification, verification/publication adaptation, install/restore | Kubernetes, remote runner protocol, Gateway translation/fallback, Redis/Postgres/OIDC, multi-replica deployment, cross-runtime conformance, generalized signed record |

## 14. Research Limits And Sources

The supporting appendix contains the exact 20-step bake-off, fault matrix,
performance definitions, claim thresholds, evidence capture format, primary
source index, and remaining unknowns.

Principal evidence includes:

- [OpenHands Agent Canvas architecture at `64c1269`](https://github.com/OpenHands/OpenHands/blob/64c1269655012698bc66538967989996191beb6c/docs/architecture.md)
- [OpenHands custom ACP documentation](https://docs.openhands.dev/openhands/usage/agent-canvas/acp-agents#custom-acp-servers)
- [OpenHands OSS Helm isolation warning](https://github.com/OpenHands/OpenHands/blob/64c1269655012698bc66538967989996191beb6c/helm/agent-canvas/README.md)
- [OpenHands Agent Server status and lease source at `9d143aa`](https://github.com/OpenHands/software-agent-sdk/tree/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a)
- [OpenCode V2 API](https://opencode.ai/v2/docs/api/)
- [OpenCode source evidence for unavailable V2 operations](https://github.com/anomalyco/opencode/commit/f5d20c580b605c638d417dd00d74110f08dcfbf2)
- Current first-party documentation and pinned sources for the competitive rows,
  all linked in the supporting appendix and existing dated category audits.

This research establishes category crowding and technical feasibility. It does
not establish willingness to install, weekly retention, willingness to pay, or
comparative reliability. Those are exactly the next experiment's job.
