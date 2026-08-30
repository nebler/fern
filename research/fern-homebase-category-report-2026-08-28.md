# Fern Homebase Category Report

Research date: 2026-08-28

Fern repository baseline: `harden/production-readiness` at
`ab945b5a00db3a310b3fcc30fe8bc99669598b6f`

Status: point-in-time strategy research. In this report, **Fern Homebase** is
shorthand for Fern's proposed small-team, self-hosted product shape; it is not
the name of an implemented package in the repository. Code and maintained
architecture documents remain authoritative for Fern's current behavior.

## Decision

The broad category is occupied. A self-hosted or BYOC coding-agent control
plane can no longer differentiate on remote task dispatch, isolated execution,
issue-to-PR automation, schedules, mobile access, multiple harnesses, or runner
pools alone.

The most consequential 2026 changes are:

- OpenHands Agent Canvas is an MIT, self-hostable control surface with
  OpenHands, Claude Code, Codex, Gemini, and ACP backends, local/remote/cloud
  execution, schedules, GitHub events, custom webhooks, phone browser access,
  and conversation pause/resume primitives.[S17][S18]
- Coder Agents is an AGPL self-hosted control-plane agent loop, not merely a
  customer-hosted workspace under a vendor SaaS control plane. Its chat, agent
  loop, model credentials, and tool authority run in the customer's Coder
  deployment.[S19]
- Warren, Orbit, Deputies, Symphony, and T3 Code provide several credible OSS
  paths across isolated jobs, durable control, multi-harness supervision,
  runners, triggers, and Git delivery.[S20][S21][S22][S23][S24]
- Warp, Claude Code, Cursor, Devin, GitHub Copilot, and Ona all offer forms of
  customer-hosted execution. In each reviewed offering, the vendor still owns
  material control-plane, session, planning, or inference authority.[S02][S08]
  [S12][S14][S16][S26]

The defensible recommendation is therefore:

> Build Fern Homebase around **verified change handoff**: one admitted intent,
> one exact Git result, host-owned verification, explicit human authority, and
> conservative publication recovery. Add a reliable human-escalation inbox and
> simple installation as product requirements, not as standalone category
> claims.

Do not position Fern as the first self-hosted control plane, a generic
multi-harness manager, or a unique way to run agents from a phone. The strongest
OSS opportunity is a small, composable correctness layer that can eventually
consume T3 Code or another control surface rather than replacing it.

## Method And Labels

The matrices report public documentation, not hands-on reliability. A vendor's
documented capability is not an independent claim that the feature works well.

| Label | Meaning |
| --- | --- |
| `D` | Documented in the cited first-party source or pinned source tree. |
| `U` | Not publicly documented in the reviewed primary sources. This is not proof that an internal capability does not exist. |
| `NA` | Not applicable to the product's documented scope. |

Additional rules:

- A documented negative is still `D`, for example "recurring schedules are not
  yet built."
- "Mobile" distinguishes a native app from a usable mobile browser.
- "Resume" distinguishes continuing a persisted conversation from safely
  reconciling an interrupted external effect.
- "Self-hosted" is classified by control-plane placement below; customer-owned
  compute by itself is not full self-hosting.
- Prices are included only when the live primary source exposed them. A blank
  or inaccessible price is `U`, not an estimate.

## Capability Matrix: Workflow And Supervision

Sources in each row apply across the row unless a cell cites a narrower source.

| Product | Triggers | Issue or request to PR | Mobile supervision | Human escalation and resume | Notifications |
| --- | --- | --- | --- | --- | --- |
| **Fern today** | `D` Paired browser/API submission; no schedules or webhook automation. | `D` Task to exact user-sealed result, optional exact-result verification, and receipt-backed draft PR; issue assignment is not the trigger. | `D` Responsive private web/OpenCode surface; no native Fern app. | `D` User can inspect, cancel, preview, seal, and return to durable task state. Durable Fern approval answers and generic authoritative completion are not implemented. | `D` No notification outbox or review digest. |
| **Warp / Factories** | `D` API/CLI, schedules, Slack, Linear, GitHub, GitLab, Jira, direct runs, and factory work intake.[S01][S03] | `D` Early Access Factories explicitly move work items through triage, spec, implementation, and review to a mergeable PR.[S03] | `D` Web run surface. `U` Native mobile client in reviewed docs. | `D` `BLOCKED` state, user approvals, live attach/steer, durable parent-child messages, and follow-up wake of terminal children.[S01] | `D` In-app parent-run notifications; child detail requires drill-down.[S01] `U` Public push-notification contract. |
| **OpenAI Codex** | `D` Web, CLI, GitHub/GitLab, Linear, Slack, and scheduled/automation surfaces.[S05] | `D` Cloud task runs in an isolated environment, returns a diff, and can open a PR.[S05] | `D` ChatGPT iOS/Android Remote can start, steer, approve, answer, inspect diffs/tests/screenshots, and switch hosts. Connected-host execution stops when the host is unavailable.[S06] | `D` Remote approvals/questions and cross-device continuation; host handoff transfers chat and Git state. `U` Fern-equivalent ambiguous publication recovery. | `D` Remote completion and attention notifications.[S06] |
| **OpenAI Symphony** | `D` Long-running issue-tracker poller with bounded dispatch and tracker-driven reconciliation.[S07] | `D` Isolated issue runs can end at a workflow-defined human-review state; PR/ticket writes are performed through agent tools, not a built-in publication transaction.[S07] | `U` No prescribed rich UI or mobile client. | `D` Retries, stall handling, restart reconciliation, preserved per-issue workspaces, and human-review handoff. `U` Interactive mobile question inbox.[S07] | `D` Structured logs and optional status surface. `U` Push notifications.[S07] |
| **Claude Code** | `D` Cloud routines, desktop schedules, session loops, GitHub Actions/events, and direct cloud dispatch.[S09] | `D` Hosted and self-hosted cloud sessions clone GitHub repositories and can push changes; cloud coding workflows are documented. | `D` Native Claude iOS/Android and browser Remote Control; can supervise subagents/workflows and answer tool prompts.[S08] | `D` Permission questions stay open, local conversations reconnect, server sessions can be resumed for a bounded period, and self-hosted sessions requeue after runner loss.[S08] | `D` Configurable "Claude decides" and "actions required" mobile push, with terminal-presence suppression.[S08] |
| **GitHub Copilot cloud agent** | `D` Issue assignment, PR comments, chat, VS Code, schedules, and event-driven automations.[S10] | `D` Research/plan/change on a branch and optionally create exactly one PR per assigned task.[S10] | `D` GitHub Mobile can assign issues to Copilot and review issues, PRs, and notifications.[S27] | `D` Users can iterate in the agent session and PR comments. `U` A documented paused-question queue or exact post-crash resume contract. | `D` GitHub Mobile repository/mention notifications. `U` A separate agent-attention notification taxonomy.[S27] |
| **Devin** | `D` Cloud, Slack, API/MCP, automations, and reusable Dynamic Workflows.[S13][S14] | `D` Sessions and workflows can push branches/PRs; Outposts executes the same tool calls on customer machines.[S13] | `D` Browser/cloud supervision. `U` A first-party native mobile coding client in reviewed sources. | `D` Workflow runs replay completed typed stages and resume only unfinished calls; users can stop and later resume the recorded run.[S14] | `U` Exact public completion/input-required notification contract in reviewed sources. |
| **Google Jules** | `D` Web/API/CLI, `jules` issue label, schedules, suggested tasks, PR comments, and CI-failure continuation.[S15] | `D` Fresh VM to tests, branch, and direct PR; schedules can open PRs unattended.[S15] | `D` Mobile-capable web product and task controls; `U` native Jules app. | `D` Plan questions, pause/resume task controls, PR-feedback response, and immutable session activities.[S15] | `D` Browser notifications when complete or when input is needed.[S15] |
| **OpenHands Agent Canvas** | `D` Manual conversations, cron schedules, GitHub events, custom signed webhooks, and prebuilt automations.[S17] | `D` GitHub issue/PR automations and agent GitHub workflows are documented. `U` A Fern-style write-ahead PR publication transaction.[S17] | `D` Responsive browser access through Tailscale or authenticated ngrok. `U` Native mobile app.[S18] | `D` SDK persistence, pause/resume, send-message-while-running, and resumable goals; Agent Canvas conversations are backend-persisted.[S17] | `U` A documented first-party mobile push channel in the reviewed Canvas docs. Slack/GitHub automation outputs are documented. |
| **Coder Agents** | `D` Web chat and API; community GitHub Action can start a chat from an issue/PR. `U` Built-in general cron/webhook automation in reviewed Agents docs.[S19] | `D` Agent tools can use Git and open PRs under the submitting user's authority; `U` a first-party issue-assignment-to-PR workflow. | `D` Web UI; `U` native mobile client and mobile-specific layout contract. | `D` Durable DB-backed chat survives workspace stop/delete/rebuild, queued follow-ups, structured questions, plan approval, and interruptible subagents.[S19] | `D` General Coder web notifications are listed; `U` agent completion/input-required push contract.[S25] |
| **Ona** | `D` Manual, Linear, schedules, PR events, and webhooks; automations run closed-loop workflows.[S16] | `D` Clone, branch, build, test, iterate, commit, push, and open a PR across configured repositories.[S16] | `D` Review automation results from phone or iPad browser. `U` native mobile app.[S16] | `D` Codex goal mode and retained automation history. `U` Public contract for a durable human-question inbox and interrupted-effect recovery.[S28] | `U` Exact completion/input-required push behavior in reviewed current docs. |
| **Cursor Cloud Agents** | `D` iOS/web/desktop, Slack, GitHub/Bitbucket comments, Linear, API, and automations.[S11] | `D` Agents work on branches and open PRs, including coordinated multi-repo PRs.[S11] | `D` Native iOS; Android PWA; run management from any device.[S11] | `D` Follow-ups, takeover of remote desktop, terminal states, and resumable run event APIs. `U` exact ambiguous-effect recovery.[S11] | `D` Slack integration includes notifications. `U` Reviewed source did not specify a complete mobile attention taxonomy. |
| **Warren** | `D` API/CLI and repeated schedules; exact external trigger list is not fully specified in the pinned README.[S20] | `D` Fresh workspace to pushed branch, with optional configured PR creation.[S20] | `D` Responsive web UI. `U` native mobile client.[S20] | `D` Live steering/cancellation, watchdog recovery, and salvage-before-teardown. Source audit found finalize intent reconstructed rather than durably journaled.[S20][S29] | `U` User-facing push channel in pinned source. |
| **Orbit** | `D` Durable task queue/dependency graph and manual dispatch. Recurring schedules and inbound sources are explicitly not built.[S21] | `D` Exact Git/test checkpoints and a durable merge path; no concrete PR-create implementation was located in the source audit.[S21][S29] | `D` Native iPhone/iPad, native macOS, and responsive web clients.[S21] | `D` Live approvals, resumable sessions, runner recovery, and durable task/project state.[S21] | `U` Push notification channel in pinned source. |
| **Deputies** | `D` Slack, GitHub, generic webhook, scheduled automation, and per-session scheduled follow-ups.[S22] | `D` Concrete Git and GitHub CLI tools can push and create PRs; source audit found no write-ahead PR effect journal.[S22][S29] | `D` Web UI. `U` native mobile app or mobile-specific contract.[S22] | `D` Persistent sessions, queued steering, archive/resume, nested sessions, and durable callback delivery tracking.[S22] | `D` Slack/callback integrations. `U` native push notifications.[S22] |
| **T3 Code** | `D` Direct interactive dispatch from local, web, desktop, and mobile clients. `U` schedules or event/webhook automations in `v0.0.35`.[S24] | `D` Source-control actions include commit/push/PR. `U` autonomous issue-assignment trigger.[S24] | `D` Native iOS and Android plus web and Electron clients.[S24] | `D` Durable event-sourced threads, command receipts, provider resume cursors, and Git checkpoints. Server updates can still interrupt active work.[S30] | `U` A documented push-notification policy in `v0.0.35`. |

## Capability Matrix: Execution, Deployment, And Price

| Product | Environment model | Harness strategy | Runner and control-plane placement | OSS deployment | Public price signal |
| --- | --- | --- | --- | --- | --- |
| **Fern today** | One persistent, constrained Docker workspace over one host repository; stop/freeze/wake and backup/restore. | One digest-pinned OpenCode V2 beta profile. | `D` Full control plane and execution on the owner's host; hosted model inference remains external when used. | `D` Source repository exists, but no verified public release or one-command external deployment is claimed. | `NA` No Fern service price; operator pays host and model costs. |
| **Warp / Factories** | Reusable configured environments on Warp-hosted workers or customer Docker/Kubernetes/direct workers.[S02] | Warp Agent, Claude Code, and Codex; children may use a different harness from the parent.[S01] | **Split plane:** execution can be customer-hosted; orchestration, session management, transcripts, and inference route through Warp.[S02] | `U` No OSS control-plane deployment. Self-hosted workers are Enterprise-only.[S02] | Free; Build $20/month; Business $50/user/month; Enterprise custom. Annual prices differ. Factory PAYG adds 20% where documented.[S04] |
| **OpenAI Codex** | Isolated cloud environments, local worktrees, connected desktop hosts, and SSH hosts.[S05][S06] | Codex only as the execution harness; app-server/SDK are integration surfaces. | **Vendor or connected-host:** OpenAI-hosted cloud, or owner host controlled through ChatGPT Remote. No Codex customer runner pool was documented. | `D` CLI is open source; `U` self-hosted ChatGPT/Codex cloud control plane. | `U` Exact current plan amounts were not captured from a live primary pricing page in this review. |
| **OpenAI Symphony** | Deterministic per-issue filesystem workspace, preserved across retries and normally removed at terminal tracker state.[S07] | Codex app-server in the reference contract. | **Full self-host:** scheduler, tracker adapter, workspace manager, and agent process run under the operator.[S07] | `D` Apache-2.0 reference implementation/spec at `8001b52`.[S07] | `D` No license fee; operator pays model, tracker, and compute costs. |
| **Claude Code** | Local machine, Anthropic-hosted cloud session, or customer runner image/network.[S08] | Claude Code only. | **Split plane for self-hosted environments:** customer runs tools/repos; Anthropic retains queue, transcript, session control, and inference.[S08] | `U` No OSS Claude control plane. | `D` Plan-gated; self-hosted environments use normal organization Claude Code usage. Exact dollar amounts not captured.[S08] |
| **GitHub Copilot cloud agent** | Ephemeral GitHub Actions environment with setup workflow, larger runner, or recommended ephemeral single-use customer runner; 59-minute maximum.[S10][S26] | Copilot cloud agent, with some third-party agent delegation through GitHub. | **Split plane:** GitHub owns task/session/repository authority; execution can use customer Actions runners.[S26] | `U` No OSS Copilot control-plane deployment. | `D` All paid Copilot plans; consumes AI credits and Actions minutes. Exact seat prices are outside the reviewed source.[S10] |
| **Devin** | Devin VMs, per-child workflow VMs, shared session VM, or Outposts worker on customer VM/container/Kubernetes/Mac.[S13][S14] | Devin only, with Lite mode choices inside workflows. | **Split plane:** Outposts keeps commands/files/repos on customer machines; inference and planning remain in Devin Cloud.[S13] | `D` Outposts Kubernetes operator is open source; `U` self-hosted Devin control plane/agent loop. | `U` Live Devin pricing page returned HTTP 429; no price inferred. Outposts is documented for Pro, Max, and Teams.[S13] |
| **Google Jules** | Short-lived Google VM, repository setup scripts, reusable environment snapshots, 20 GB disk.[S15] | Jules/Gemini only. | **Vendor hosted:** no customer runner or self-hosted Jules control plane documented. | `U` No OSS Jules control plane. | Free 15 tasks/day and 3 concurrent; Pro 100/15; Ultra 300/60, bundled through Google AI plans.[S31] |
| **OpenHands Agent Canvas** | Local process, Docker, VM, Kubernetes, Modal, OpenHands Cloud, or remote Agent Server.[S17][S32] | OpenHands plus Claude Code, Codex, Gemini, and any ACP-compatible agent.[S17] | **Full self-host or managed:** UI, agent server, automation server, state, and execution can all run under the operator. Model APIs may remain external.[S17][S32] | `D` MIT at `f26d734`; npm/npx or one Docker command, plus VM/Helm paths.[S17][S32] | `D` No OSS license fee; operator pays model/infra. Managed Cloud/Enterprise pricing not evaluated. |
| **Coder Agents** | Terraform-defined Docker, VM, Kubernetes, and other Coder workspaces, provisioned on demand.[S19] | Native Coder Go agent loop; explicitly not a wrapper for Claude/Codex. Multiple LLM providers supported.[S19] | **Full self-host:** agent loop, chat, credentials, tool routing, database, and workspaces remain in the Coder deployment.[S19] | `D` AGPL control plane at `8bf271c`; Community install is free.[S19][S25] | Community free; Premium annual per-user amount is not public on the pricing page.[S25] |
| **Ona** | Dev Container environments on Ona Cloud or customer AWS/GCP VPC runners; automations use full prepared environments.[S16] | Current Codex Agent; customer-installed Claude Code; legacy Ona Agent deprecated.[S28] | **Split/managed plane:** Enterprise execution is in a customer VPC but Ona manages the product/control plane; current docs call this self-hosted, Ona-managed VPC.[S33] | `D` Legacy Gitpod Classic code is AGPL but no longer recommended; `U` current Ona control-plane deployment source. | Core from $20/month, 80-2,200 OCUs depending tier selection; Enterprise custom.[S33] |
| **Cursor Cloud Agents** | Isolated Cursor VMs with Builds/snapshots, or BYOM personal machines and enterprise pools.[S11][S12] | Cursor agent loop with curated models. | **Split plane:** BYOM moves tool execution only; Cursor retains agent loop, inference, and planning, and receives needed file/tool output.[S12] | `U` No OSS Cursor control plane. AWS worker examples are reference deployments only. | Individual $20/month; Teams $40/user/month; Enterprise custom; Cloud Agents also use model API pricing.[S34] |
| **Warren** | Fresh worktree/clone per run on local sandbox, sibling Docker container, or Kubernetes pod.[S20] | Pi and Claude Code adapters in the pinned distribution.[S20] | **Full self-host:** control plane, run history, credentials, and runtime on operator infrastructure.[S20] | `D` MIT, two-command local install and Compose/Kubernetes deployment at `fe10715`.[S20] | `D` No license fee; operator pays model/compute. |
| **Orbit** | Registered machines, optional per-session worktree, durable project/task state in PostgreSQL.[S21] | Claude Code, Codex, Kimi, and OpenCode; BYO model providers.[S21] | **Full self-host:** control plane, PostgreSQL, clients, and outbound-polling runners.[S21] | `D` MIT Docker Compose deployment at `aca9757`; pre-1.0.[S21] | `D` No license fee; operator pays model/compute. |
| **Deputies** | Daytona, Superserve, Docker, Tensorlake, Kubernetes Agent Sandbox, Lambda microVM, fake, or trusted-local providers.[S22] | Pi-based agent runner plus provider/subscription options.[S22] | **Full self-host:** portable Node/Caddy/PostgreSQL/object-store control plane and selectable sandbox providers.[S22] | `D` MIT at `60c7e18`; Compose, Helm, Railway, and AWS Terraform paths.[S22] | `D` No source license fee; operator pays model, database, object storage, and sandbox costs. |
| **T3 Code** | Existing local/remote machine and optional Git worktrees; it does not provide managed durable compute.[S24][S30] | Claude Code, Codex, Cursor, Grok Build, and OpenCode through installed authenticated CLIs.[S24] | **Full self-host for control surface/execution:** backend runs on the user's machine; optional relay is a connectivity aid, not managed compute.[S24][S30] | `D` MIT at tag `v0.0.35`; `npx t3@latest` starts backend and web UI.[S24] | `D` Free software; user supplies harness subscriptions, model access, and compute.[S24] |

## Deployment Taxonomy

The phrase "BYOC" should not appear in Fern positioning without one of these
qualifiers.

| Class | Customer controls | Vendor still controls | Examples |
| --- | --- | --- | --- |
| **Connected host / remote window** | Existing machine, files, tools, credentials, and process uptime | Relay, account, synchronized transcript, and client surface | Codex Remote, Claude Remote Control, T3 relay when used |
| **Customer-hosted execution** | Tool execution, checkout, build artifacts, network adjacency, worker capacity | Session queue, planning/agent loop, transcript, policy surface, and often inference | Warp self-hosted workers, Claude self-hosted environments, Cursor BYOM, Devin Outposts, Copilot on self-hosted Actions runners |
| **Managed customer VPC** | Cloud account/network boundary and some IAM/network policy | Vendor-operated deployment, upgrades, product control plane, and support authority | Ona Enterprise runners/VPC |
| **Fully self-hosted control plane** | Task/session authority, database, agent loop or harness process, execution, credentials, upgrades, and network | Only explicitly chosen external services such as model APIs or source control | Fern, Coder Agents, OpenHands Agent Canvas, Warren, Orbit, Deputies, Symphony, T3 Code |
| **Hosted execution** | Repository authorization and configuration | Control plane and worker infrastructure | Codex cloud, Jules, default Copilot, Cursor Cloud, Devin Cloud, Warp-hosted, Claude cloud |

Two additional terms are orthogonal:

- **BYO model/provider** means the customer supplies an API key, endpoint, cloud
  account, or subscription. It says nothing about where the control plane runs.
- **Customer data plane** means repository clones and tool execution stay on
  customer infrastructure. It does not imply transcripts, prompts, planning,
  or inference stay there.

Fern's honest class is fully self-hosted control plane with externally hosted
inference when configured. Its current trusted Docker workspace is not hostile
multi-tenant isolation.

## Commodity Conclusions

The following are now table stakes rather than wedges:

| Capability | Evidence of crowding |
| --- | --- |
| Remote/background work | All major hosted products plus every reviewed OSS control plane |
| Issue/request to PR | Warp Factories, Codex, Claude, Copilot, Devin, Jules, Ona, Warren, Deputies, Symphony workflows |
| Phone supervision | Codex, Claude, Copilot, Cursor, T3 native clients; OpenHands, Ona, Orbit, Fern web/native combinations |
| Schedules and webhooks | Warp, Claude, Copilot, Jules, OpenHands, Ona, Deputies; Warren schedules |
| Isolated execution | Hosted VMs, Actions runners, worktrees, containers, customer VPC workers, and OSS sandbox adapters |
| Multiple harnesses | Warp, OpenHands, Orbit, T3 Code; Warren has two adapters |
| Runner pools / BYOC | Warp, Claude, Copilot, Devin, Cursor, Ona, Orbit, and self-hosted OSS deployments |
| Resume after disconnect | Codex, Claude, Cursor, Devin workflows, Jules, OpenHands, Coder, Orbit, Deputies, T3 |
| Simple local self-host start | OpenHands npm/Docker, Warren two-command install, T3 `npx`, Coder single binary, Orbit Compose |

Three caveats prevent a false "everything is solved" conclusion:

1. Documented resume usually means conversation or workflow continuation, not
   proof that a timed-out push, PR creation, provider call, or tool effect can
   safely be repeated.
2. Notifications are uneven. Claude has the clearest documented
   input-required/completion push policy. Many products expose status but do not
   publicly specify durable delivery, deduplication, quiet hours, escalation,
   acknowledgment, or recovery from a lost notification.
3. Full control-plane self-hosting is available, but current OSS products trade
   off installation simplicity, multi-user operations, exact authority, native
   clients, and publication correctness. This creates product opportunities,
   but not an empty category.

## Five-Wedge Scorecard

Scores are 1-5, where 5 is favorable. **Crowding** scores whitespace, so 5 means
few direct implementations. **Fern distance** scores how much of the hard
backend already exists, so 5 means close. **T3 leverage** scores the ability to
use T3 Code as a producer/client or distribution complement instead of
rebuilding it. Totals are unweighted out of 30.

| Candidate wedge | User clarity | Crowding | Fern distance | T3 leverage | Distribution | Monetization | Total |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| **A. Verified Change Record and safe publication** | 4 | 4 | 5 | 4 | 4 | 3 | **24** |
| **B. Reliable human escalation and resumption inbox** | 5 | 3 | 3 | 4 | 3 | 4 | **22** |
| **C. Cross-harness failure-semantics conformance kit** | 3 | 5 | 4 | 5 | 4 | 1 | **22** |
| **D. One-command small-team Homebase appliance** | 4 | 2 | 4 | 2 | 4 | 2 | **18** |
| **E. Mobile verified-handoff queue** | 5 | 2 | 3 | 5 | 2 | 1 | **18** |

### A. Verified Change Record And Safe Publication

**Job:** let a maintainer or policy gate answer, "Which exact change was
authorized, verified, and published, and what remains ambiguous?"

- Fern advantage: exact base/result identities, user seal, same-commit host
  verification, write-ahead push/PR phases, and exact read reconciliation
  already exist.[S29]
- Market opening: competitor records usually center on transcript, run state,
  checkpoints, CI, or authorship. The pinned source audit did not locate the
  complete intent-to-exact-result-to-verification-to-publication chain in any
  reviewed OSS system.[S29]
- T3 leverage: ingest T3 thread/checkpoint identity as typed producer claims;
  do not ask Fern to replace T3's clients.
- Distribution: offline verifier, GitHub Check, canonical JSON schema, and a
  runtime conformance corpus are natural OSS entry points.
- Monetization: modest. A hosted policy/check service or support could sell to
  security/platform teams, but external demand is unproven.

**Decision:** primary strategic proof. Build only after Fern's current physical
phone-to-verified-PR journey is accepted. Validate it against one real external
merge, rollout, audit, or incident decision.

### B. Reliable Human Escalation And Resumption Inbox

**Job:** ensure an agent question, approval, blocked state, or uncertain effect
becomes one durable decision item that can be answered later without losing the
run or repeating unsafe work.

- Market opening: many products show questions or send notifications; fewer
  publicly specify durable dedupe, acknowledgment, expiry, authority binding,
  restart recovery, and safe resume as one contract.
- Fern advantage: durable receipts, cancellation fences, actor snapshots, and
  conservative ambiguity handling are relevant, but a durable approval-answer
  table/API and notification outbox do not exist.
- T3 leverage: T3 supplies broad harness events and clients; Fern could provide
  the narrower durable decision object and resume policy.
- Monetization: clearer than evidence export for teams losing hours to blocked
  agents, but integration and support costs are high.

**Decision:** second product capability, not a standalone company. First prove
one end-to-end question/approval flow with restart, duplicate delivery, expired
authority, and answer-after-harness-reconnect tests.

### C. Cross-Harness Failure-Semantics Conformance Kit

**Job:** tell maintainers exactly what a harness does on accepted input,
disconnect, cancellation, process death, replay, malformed events, and ambiguous
effects.

- Market opening: multi-harness UIs normalize happy paths; public compatibility
  suites for failure semantics are uncommon.
- Fern advantage: its black-box pinned OpenCode contract harness and refusal to
  infer success are directly reusable.
- T3 leverage: highest of all candidates because T3 already drives the named
  harnesses and could expose or consume adapter profiles.
- Distribution: upstream bug reports, reproducible fixtures, version badges,
  and CI actions can build credibility.
- Monetization: weak by itself. Treat it as OSS distribution and adapter
  quality infrastructure for wedge A or B.

**Decision:** pursue as a narrow credibility project only when a second runtime
adapter is required. Do not build a universal runner.

### D. One-Command Small-Team Homebase Appliance

**Job:** run a complete trusted-team control plane on one machine without
Kubernetes, vendor SaaS, or assembling five services.

- Market opening: installation and operations remain painful in parts of the
  OSS market.
- Crowding correction: OpenHands, Warren, T3, Coder, and Compose-based Orbit
  already provide simple starting paths. "Easy self-hosting" is not empty.
- Fern advantage: one Go binary and SQLite are operationally attractive, but
  Fern supports one owner/workspace/harness and lacks the visible workflow
  breadth of competitors.
- Monetization: likely support/sponsorship, not meaningful per-seat SaaS revenue.

**Decision:** required packaging for Fern, not the lead wedge. Optimize for a
signed binary, one config, one Docker dependency, backup/restore, and a clear
trusted-team boundary. Do not add PostgreSQL or Kubernetes to appear complete.

### E. Mobile Verified-Handoff Queue

**Job:** inspect small agent results, answer bounded questions, seal an exact
result, request publication, and defer large review to a desk.

- User clarity is excellent, but native mobile and mobile web supervision are
  heavily occupied.
- Fern's distinction must be the exact evidence and authority shown at the
  decision point, not "agents from your phone."
- T3 leverage is high: a T3 client integration is more rational than another
  native mobile application.
- Distribution and monetization are weak as an independent product.

**Decision:** keep Fern's responsive web surface. Add the missing verified
publication action and chronology. Do not build native mobile before a real
consumer proves browser/T3 integration insufficient.

## Recommended Product Boundary

The smallest coherent Homebase offer is:

```text
existing agent surface (OpenCode first; T3/import later)
  -> durable admitted task
  -> durable escalation/decision items
  -> one user-authorized exact Git result
  -> host-owned verification of that result
  -> receipt-backed publication with ambiguity reconciliation
  -> portable host-attested change record
```

It deliberately does not own:

- a new chat UI or native mobile client;
- a generic multi-harness scheduler;
- a broad issue-tracker automation framework;
- a hosted sandbox fleet;
- model-provider breadth or a general LLM gateway;
- automatic merge;
- hostile multi-tenant execution;
- enterprise identity or distributed infrastructure before a real user
  boundary requires it.

The first integration stance should be **producer, not replacement**:

- Fern-wrapped OpenCode can make strong admitted-input and effect claims.
- T3 Code, Codex, Claude, or another runtime can initially contribute typed,
  imported producer claims and Git objects.
- Imported claims remain explicitly `unavailable` where Fern did not own input,
  terminal, cancellation, or effect authority.
- The offline verifier checks integrity, exact Git identity, verification, and
  policy without upgrading vendor assertions into Fern facts.

## Execution Gates

1. Finish and physically accept current Fern: signed release, Ubuntu/systemd,
   phone TLS/SSE/WSS, reboot, revocation, provider-funded task, exact seal,
   verification, draft PR, backup, and replacement-host restore.
2. Add the already-supported publication action and an evidence chronology to
   the phone web UI.
3. Dogfood for two weeks. Record every question, blocked state, laptop escape,
   duplicate action, and recovery.
4. Prototype one portable record and offline verifier from an existing task.
5. Show that record to at least five platform/security/developer-productivity
   engineers and observe one real decision. Interest alone does not pass.
6. Build one durable escalation item only if dogfood produces a recurrent lost
   question, approval, or unsafe-resume problem.
7. Add one T3 or structured batch adapter only when a real consumer needs
   cross-harness evidence. Preserve unavailable fields rather than inventing a
   lowest-common-denominator success state.

Kill the direction if ordinary CI plus vendor logs answer the consumer's
question, if no external installation reaches a real repository, or if
portability requires weakening Fern's exact result, cancellation, verification,
or publication fences.

## Explicit Unknowns

The following remained `U` after targeted primary-source review:

- a public native mobile Warp client and a complete Warp push-notification
  delivery contract;
- a customer-operated Codex cloud runner pool distinct from connected-host
  Remote and the separate Symphony project;
- a Fern-equivalent ambiguous GitHub-effect reconciliation contract in any
  reviewed vendor product;
- a first-party native Devin mobile coding client and current Devin dollar
  pricing, because `https://devin.ai/pricing` returned HTTP 429;
- native Jules mobile applications;
- first-party mobile push for OpenHands Agent Canvas;
- agent-specific push/attention semantics for Coder Agents, Ona, Warren, Orbit,
  Deputies, and T3 Code;
- a current open-source deployment of Ona's control plane rather than the
  deprecated/not-recommended Gitpod Classic codebase;
- public dollar pricing for Coder Premium and several plan-gated vendor runner
  offerings.

These unknowns should not be converted into competitive claims without a fresh
source or direct product test.

## Primary Sources

Live documentation without a visible publication date is labeled "accessed
2026-08-28." Source-tree claims are pinned to the revision inspected.

- **[S01] Warp**, [Multi-agent orchestration](https://docs.warp.dev/platform/orchestration/), last updated 2026-08-27.
- **[S02] Warp**, [Self-hosting overview](https://docs.warp.dev/platform/self-hosting/), last updated 2026-08-27.
- **[S03] Warp**, [Factories overview](https://docs.warp.dev/factories/), last updated 2026-08-28.
- **[S04] Warp**, [Pricing](https://www.warp.dev/pricing), accessed 2026-08-28.
- **[S05] OpenAI**, [Codex cloud](https://developers.openai.com/codex/cloud/), accessed 2026-08-28.
- **[S06] OpenAI**, [Codex remote connections](https://developers.openai.com/codex/remote-connections), accessed 2026-08-28.
- **[S07] OpenAI Symphony**, [repository and Apache-2.0 reference implementation](https://github.com/openai/symphony/tree/8001b52e3062495a16e520e4ceaf8f9de868c4d0) and [service specification](https://github.com/openai/symphony/blob/8001b52e3062495a16e520e4ceaf8f9de868c4d0/SPEC.md), pinned 2026-08-28.
- **[S08] Anthropic**, [Claude Code Remote Control](https://code.claude.com/docs/en/remote-control) and [self-hosted environments](https://code.claude.com/docs/en/self-hosted-environments), accessed 2026-08-28.
- **[S09] Anthropic**, [Claude Code scheduled tasks](https://code.claude.com/docs/en/scheduled-tasks), accessed 2026-08-28.
- **[S10] GitHub**, [About Copilot cloud agent](https://docs.github.com/en/copilot/concepts/agents/cloud-agent/about-cloud-agent), accessed 2026-08-28.
- **[S11] Cursor**, [Cloud Agents](https://cursor.com/docs/cloud-agent), accessed 2026-08-28.
- **[S12] Cursor**, [Bring your own machine](https://cursor.com/docs/cloud-agent/bring-your-own-machine), accessed 2026-08-28.
- **[S13] Cognition**, [Devin Outposts](https://docs.devin.ai/cloud/outposts/overview), accessed 2026-08-28.
- **[S14] Cognition**, [Devin Dynamic Workflows](https://docs.devin.ai/work-with-devin/dynamic-workflows), accessed 2026-08-28.
- **[S15] Google**, [Jules changelog](https://jules.google/docs/changelog/), entries dated 2025-06-26 through 2026-03-09, accessed 2026-08-28.
- **[S16] Ona**, [Background automations](https://ona.com/docs/ona/automations/overview), accessed 2026-08-28.
- **[S17] OpenHands**, [Agent Canvas repository](https://github.com/OpenHands/OpenHands/tree/f26d734a848297d8dcf460b0bb739174e76511f0) and [Agent Canvas overview](https://docs.openhands.dev/openhands/usage/agent-canvas/overview), pinned/accessed 2026-08-28.
- **[S18] OpenHands**, [Phone and tablet access](https://docs.openhands.dev/openhands/usage/agent-canvas/mobile-access) and [event-based automations](https://docs.openhands.dev/openhands/usage/automations/event-automations), accessed 2026-08-28.
- **[S19] Coder**, [Coder Agents](https://coder.com/docs/ai-coder/agents) and [AGPL repository at `8bf271c`](https://github.com/coder/coder/tree/8bf271c5030fdc0cd06ab1d3c0f5714712a1c8d5), accessed/pinned 2026-08-28.
- **[S20] Warren**, [README and MIT source at `fe10715`](https://github.com/jayminwest/warren/tree/fe1071562ac957aacba39beba850ef00e10d879a), pinned 2026-08-28 source audit.
- **[S21] Orbit**, [README and MIT source at `aca9757`](https://github.com/jianghailong-xy/orbit/tree/aca97577e5e57cb2d6d39e503a035115e1cedd9c), pinned 2026-08-28.
- **[S22] Deputies**, [README and MIT source at `60c7e18`](https://github.com/sidpalas/deputies/tree/60c7e186187839a52d56c23d57cf9e22fe9cd5b4), pinned 2026-08-28.
- **[S23] OpenAI Symphony**, [README](https://github.com/openai/symphony/blob/8001b52e3062495a16e520e4ceaf8f9de868c4d0/README.md), pinned 2026-08-28.
- **[S24] T3 Code**, [README and MIT source at `v0.0.35`](https://github.com/pingdotgg/t3code/blob/v0.0.35/README.md), released/inspected 2026-08-27/28.
- **[S25] Coder**, [Pricing](https://coder.com/pricing), accessed 2026-08-28.
- **[S26] GitHub**, [Configure the Copilot cloud-agent development environment](https://docs.github.com/en/copilot/customizing-copilot/customizing-the-development-environment-for-copilot-coding-agent), including self-hosted Actions runners, accessed 2026-08-28.
- **[S27] GitHub**, [GitHub Mobile](https://docs.github.com/en/get-started/using-github/github-mobile), accessed 2026-08-28.
- **[S28] Ona**, [Codex Agent](https://ona.com/docs/ona/agents/codex) and [deprecated Ona Agent](https://ona.com/docs/ona/agents/overview), accessed 2026-08-28.
- **[S29] Fern research**, [pinned Agent Change Record competitor source audit](./agent-change-record-competitor-audit-2026-08-28.md), 2026-08-28.
- **[S30] T3 Code**, [architecture](https://github.com/pingdotgg/t3code/blob/v0.0.35/docs/internals/overview.md) and [remote model](https://github.com/pingdotgg/t3code/blob/v0.0.35/docs/internals/remote.md), pinned 2026-08-28.
- **[S31] Google**, [Jules limits and plans](https://jules.google/docs/usage-limits/), accessed 2026-08-28.
- **[S32] OpenHands**, [Install Agent Canvas](https://docs.openhands.dev/openhands/usage/agent-canvas/setup), accessed 2026-08-28.
- **[S33] Ona**, [Pricing](https://ona.com/pricing) and [AWS runners](https://ona.com/docs/ona/runners/aws/overview), accessed 2026-08-28.
- **[S34] Cursor**, [Pricing](https://cursor.com/pricing), accessed 2026-08-28.

## Limitations

- This is a public-documentation and pinned-source comparison, not a
  reliability benchmark or paid-product trial.
- Product docs changed during the research window. Recheck contracts before an
  integration or purchasing decision.
- Vendor pages establish advertised/current interfaces, not independent uptime,
  correctness, security, or productivity outcomes.
- The source audit's `not located` results are scoped to the pinned revisions
  and search performed; they are not universal absence claims.
- Fern Homebase, the portable record, offline verifier, durable escalation
  inbox, T3 integration, Gateway, Labs, second runtime adapter, and native
  mobile application are not implemented.
