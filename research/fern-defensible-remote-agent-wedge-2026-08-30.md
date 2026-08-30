# Fern's Defensible Remote-Agent Product Wedge

**Research date:** 2026-08-30

**Fern baseline:** `ab945b5a00db3a310b3fcc30fe8bc99669598b6f`
**Decision:** build a narrow OpenCode integration as a two-weekend product test;
do not build a generic Fern 2.0 control surface

## Evidence Rules

Code and executed tests are authoritative for Fern. External product claims use
the following labels:

| Label | Meaning |
| --- | --- |
| `S` | Shipped and directly observed or covered by executable Fern evidence. |
| `D` | Documented by a first-party source, but not independently tested here. |
| `SO` | Located in pinned source. |
| `E` | Enterprise or plan-specific. |
| `R` | Roadmap or proposed architecture. |
| `U` | Unknown after targeted review; not evidence of absence. |
| `N` | Explicitly unsupported or outside the product's documented scope. |

Vendor documentation proves an advertised interface, not reliability. `Not
located` in source is not converted into a claim that proprietary or later code
lacks a behavior.

## 1. Executive Verdict

### Decision: build a narrow OpenCode integration

The broad remote-agent category is occupied. Fern should not become a smaller
OpenHands, Cursor, Coder, Orbit, T3 Code, or Warren.

The one product hypothesis worth a bounded build is:

> **Run several real OpenCode sessions on your own always-on machine, reopen the
> exact native OpenCode UI from any device, and retain an exact recoverable Git
> result when a session stops, crashes, or needs you.**

Call the experiment **OpenCode Background Mode**, not a generic agent platform.
Its target is one OpenCode user, one private Linux host, trusted repositories,
and two or three concurrent task environments. It preserves OpenCode's own UI,
configuration, plugins, skills, provider connections, sessions, questions,
terminal, files, and diffs. Fern adds durable admission, environment identity,
conservative status, exact result retention, and optional App-broker
publication.

This is not yet a justified standalone Fern product. It becomes one only if a
two-weekend prototype and four weeks of owner dogfood prove all of these:

1. Native OpenCode attachment is used at least weekly and materially improves
   intervention compared with OpenHands ACP or another generic surface.
2. At least 5 of 10 real delegated tasks produce a useful retained result
   without laptop-side repair.
3. Two concurrent tasks against one repository do not interfere.
4. A killed runtime still yields a reconstructable selected Git result.
5. The owner prefers this workflow to OpenHands, T3 Code, and a managed cloud
   agent for repeated work.

If those tests fail, keep the existing correctness work as a portfolio project
and extract only the Safe Git Finalizer if an external consumer appears. Do not
expand into a generic canvas.

### Why this wedge survives the first screen

| Requirement | Finding |
| --- | --- |
| Repeated problem | Cross-device continuity, false lifecycle state, wrong-worktree execution, and workspace loss recur across Claude, Codex, OpenCode, Copilot, and OpenHands issues. |
| Not adequately solved | Hosted products provide handoff, but current issue evidence shows stale context and status. OpenHands can use generic agents but does not document a first-class native OpenCode task UI. |
| Fern advantage | Exact admission IDs, conservative observation, cancellation fences, exact Git sealing, same-result verification, and publication reconciliation already exist. |
| OpenCode fit | OpenCode supplies the complete UI and runtime; Fern need not fork the agent loop or build an editor. |
| One-developer demo | Two isolated containers, two routes, phone reconnect, native UI, manual seal, Git bundle, runtime deletion, reconstruction. |
| Visible value | The user sees the same OpenCode task and a surviving exact result, not an internal state-machine diagram. |
| Defensibility | Moderate-low. Correct recovery is nontrivial, but OpenHands or OpenCode could absorb the surface. Dogfood must prove the integration quality matters. |

## 2. Repository Reality

The implemented product is one persistent OpenCode workspace, not the proposed
multi-task system. At this baseline Fern has about 65,000 lines of Go and tests.
`go test ./...`, `go test -race ./...`, and `go vet ./...` passed during this
research.

The pinned OpenCode contract harness passed 13/13 scenarios on
`0.0.0-next-17444` at image digest
`sha256:73688cd6f96ce3b236bb1c2d25607b03566a4ee92f0fedabeb06fd1a3e643c6c`.
It reconfirmed exact caller IDs, response-loss reconciliation, restart-stable
inbox rows, non-idempotent `resume`, volatile global events, and process-epoch
forms and permissions. The lifecycle harness passed 14/14 deterministic local
Docker scenarios.

### Shipped strengths

| Capability | Repository evidence |
| --- | --- |
| Durable idempotent admission | `internal/taskstore/admission.go`; admission commits task, attempt, actor, receipt, IDs, and events before wake/delivery. |
| Exact OpenCode delivery | `internal/taskdelivery/coordinator.go`; lost responses reconcile exact session/message IDs without mutation replay. |
| Conservative execution state | `internal/taskexecution/coordinator.go`; only positive `running` and `input_required` evidence is projected. |
| Exact Git result | `internal/taskresult/collector.go` and `internal/taskresultcoord/coordinator.go`; clean commit/tree/manifest under a writer fence. |
| Same-result verification | `internal/taskverification/coordinator.go`; host-owned check is prepared and revalidated against the selected result. |
| Safe App publication | `internal/taskpublicationcoord/coordinator.go`; `push_started` and `pr_create_started` precede effects, followed by exact reads. |
| Lifecycle and ingress | `internal/workspace`, `internal/runtime`, and `internal/proxy`; paired private access, stop/freeze/wake, restart reconciliation. |
| Recovery substrate | Backup/restore, compatibility gates, release evidence, and credential export/import/rotation are implemented. |

### Not shipped

Per-attempt environments, concurrent effecting attempts, automatic generic
completion, portable Git bundles, retention policy, durable answers, a
notification outbox, previews, remote runners, Kubernetes, Agent Sandbox,
Gateway, generic agents, and hosted multi-tenancy are not implemented.

## 3. Correction Register

| Claim | Evidence | Correction | Consequence | Confidence |
| --- | --- | --- | --- | --- |
| Remote agents stop with the laptop or require vendor sandboxes. | `README.md:8-14`; OpenHands, Coder, Orbit, Warren, Deputies, Symphony, and T3 are self-hostable; Cursor, Claude, Devin, Warp, Copilot, and Ona have customer execution options. | This market claim is false in 2026. | Do not lead with self-hosting, remote execution, or BYOC. | High |
| Fern's next personal runtime should be k3s plus Agent Sandbox. | `docs/TARGET_ARCHITECTURE.md:1-12`; Agent Sandbox `v1.0.0` is a young `v1beta1` lifecycle substrate and supplies no task semantics. | Kubernetes is an implementation option, not a wedge. | Use per-attempt Docker for the product test; delete k3s from the critical path. | High |
| The roadmap is executable as written. | `docs/ROADMAP.md:21-35` says serial Docker first; `:223-250` selects k3s first; `docs/REMOTE_PRODUCT.md:228-244` says Docker remains supported and Kubernetes is not required. | Maintained documents conflict. | Resolve only after the wedge test; do not implement either expansion from the current roadmap. | High |
| Fern is already a personal task-computer fleet. | `docs/ARCHITECTURE.md:8-16`; `cmd/fern/up.go`; singleton `workspace.Manager`. | Fern runs one persistent workspace and one effecting attempt. | Concurrent-task claims remain future tense. | High |
| Fern can determine normal unattended completion. | Executed OpenCode contract; `docs/REMOTE_PRODUCT.md:151-157`. | Idle, stream closure, empty inbox, and process exit do not prove success. | Prototype status must include `uncertain`; use explicit result sealing. | High |
| Fern's old pin represents current OpenCode V2. | Fern pins `0.0.0-next-17444`; current V2 source `4a977b2` adds durable session logs, execution claims/recovery, export/import, move/fork, and specialized background work. | Current V2 is materially more capable, but recovery is at-least-once and forms/permissions remain process-epoch. | Qualify a source-attributed upgrade before freezing the new runner contract. | High |
| Exact result identity means the result survives workspace deletion. | Current collector stores commit/tree metadata in the host repository; portable bundles/CAS are roadmap work. | A hash without object bytes is not durable output. | Export a Git bundle before deleting an attempt. | High |
| Fern's safe publication guarantee applies to all modes. | `README.md:125-131`; `workspace-gh` gives the agent direct `gh` authority. | Only App-broker mode can fence Fern-owned publication. | The prototype's safe mode must omit GitHub write credentials from the agent. | High |
| Cancellation prevents stale external writes. | Task cancellation fences Fern coordinators, not provider calls, shell tools, deployments, or direct `gh`. | Cancellation is an authority fence, not rollback. | Market only "stale Fern finalizers cannot publish," not universal cancellation safety. | High |
| The phone-to-PR journey is complete. | `docs/REMOTE_PRODUCT.md:171-173`. | The backend publication API exists; the embedded task page has no publication command. | Add only if the narrow prototype passes; do not count it as current UX. | High |
| OpenHands makes an OpenCode-specific product impossible. | Agent Canvas supports custom ACP agents and broad backends. No reviewed first-party path exposes the native OpenCode server/UI as the task surface. | OpenHands is a strong substitute, but ACP is not native OpenCode continuity. | The bake-off must test whether this distinction changes behavior, not merely appearance. | Medium-high |
| Session Teleport is available from current primitives. | OpenCode exposes persisted sessions, not a live cross-host migration contract for DB, process, repository, terminals, plugins, and in-flight effects. | Cold, quiesced backup/restore may be possible; live teleport is unsupported. | Reject Session Teleport from the first product. | High |
| Fern is already faster or lighter. | Historical README numbers, one local fake-runtime harness, and no controlled OpenHands run. | No comparative performance conclusion is available. | Performance remains a benchmark gate, not positioning. | High |
| Exact finalization is proven user demand. | Competitor source audit finds technical whitespace; issue research finds data loss and repo-state pain, but little explicit demand for publication journals. | Technical uniqueness is not market validation. | Treat Safe Git Finalizer as a fallback component hypothesis. | High |

## 4. OpenHands Gap Audit

OpenHands Agent Canvas is the benchmark. The current stable pins inspected are
Canvas [`v1.16.0` at `64c1269`](https://github.com/OpenHands/OpenHands/releases/tag/v1.16.0),
released 2026-08-27, Agent Server/SDK
[`v1.44.1` at `9d143aa`](https://github.com/OpenHands/software-agent-sdk/releases/tag/v1.44.1),
released 2026-08-28, and Automation `1.9.0` at `f1b3244`. Canvas `v1.16.0`
actually bundles Agent Server `1.44.0`, so unversioned docs and the separately
released `1.44.1` cannot be projected onto the all-in-one image. Answers below
separate documented behavior from source observation and inference.

| # | Question | Finding | Evidence class |
| ---: | --- | --- | --- |
| 1 | Can it run OpenCode? | **Stable OpenCode: plausibly yes by documented composition. Fern's OpenCode V2: unverified.** Canvas accepts custom stdio ACP; stable OpenCode documents `opencode acp`, while the reviewed `opencode2` V2 CLI/API do not establish that command for Fern's target. | D/U |
| 2 | Preset, custom ACP, or unsupported? | Custom ACP only. The source registry contains Claude Code, Codex, and Gemini CLI, not OpenCode. A V2 adapter is required unless a pinned V2 release exposes compatible stdio ACP. | SO |
| 3 | Native OpenCode UI? | No first-party task route or deep link to the native OpenCode UI was found. Canvas renders its own surfaces. Its ACP bridge auto-selects the first permission option, normally `allow_once`, rather than presenting a durable human approval. | SO |
| 4 | Fresh environment per conversation/run? | Backend-dependent. The documented all-in-one Docker command mounts one `/projects` directory and does not promise one fresh container per conversation. Cloud/Kubernetes/custom backends can provide stronger isolation. | D |
| 5 | Retain and reopen completed environment? | Conversations are persistent and pause/resume exists. Filesystem retention depends on backend lifecycle and operator configuration. | D |
| 6 | Survive Canvas restart? | Agent state, events, ACP session ID, and `base_state.json` persist under the conversation directory when the documented state path is mounted. Public restore regressions still require a bake-off. | SO/issues |
| 7 | Survive Agent Server restart? | Persisted state survives, but a persisted `RUNNING` conversation loads as `ERROR` and unmatched tool actions receive an interrupted-tool error. This is conservative, not in-flight effect recovery. | SO |
| 8 | Survive sandbox deletion? | Conversation records can survive; unexported workspace bytes and processes do not. A branch/PR may survive externally. | Inference |
| 9 | Ambiguous prompt deduplication? | The message-events POST has no caller idempotency key. An ambiguous client retry can append the same user task twice; trigger dedupe is separate and only works when a provider delivery ID exists. | SO |
| 10 | Completion classification? | Conversation terminal states are `FINISHED`, `ERROR`, and `STUCK`; automation completion comes from a successful callback or entrypoint exit zero. Neither binds an exact Git result. | SO |
| 11 | Can idle/silence become completion? | `IDLE` is explicitly nonterminal, so source does not equate idle with completion. Cross-adapter behavior still needs runtime testing. | SO/U |
| 12 | Cancellation versus provisioning/execution races? | Interrupt increments a generation, clears rerun intent, cancels the async run, and waits up to five seconds. A legacy thread-pool operation can continue beneath its cancelled wrapper; no arbitrary-effect fence is proven. | SO |
| 13 | Can a stale worker mutate GitHub? | Agents and automations can hold GitHub authority. No orchestrator-owned stale-attempt publication fence was located. | Source inference |
| 14 | Attempt generations/fencing epochs? | Conversation leases have owners, expiry, monotonic generation, and guarded writes. This strongly fences duplicate conversation servers, but not separate conversations sharing a workspace or stale GitHub authority. | SO |
| 15 | Who creates branches/PRs? | Canvas Git controls insert natural-language commit/pull/push/PR instructions into chat. The agent executes them; there is no separately documented exact-result finalizer. | SO |
| 16 | Ambiguous push/PR reconciliation? | No write-ahead exact-ref/PR read reconciliation contract was located. | U |
| 17 | Exact Git objects outside workspace? | A temporary `git-delta`/`tar.gz` archive endpoint exists, but `.git` is excluded and output is deleted after response. It is not a managed exact Git-object artifact. | SO |
| 18 | Reconstruct after deletion? | Only if work was committed/pushed or backend storage was retained. No general exact reconstruction promise was found. | Inference/U |
| 19 | Mobile attention/intervention? | Responsive browser access is documented. First-party durable push was not found, and Mobile Chrome/Safari Playwright projects are commented out in the stable source. | D/SO/U |
| 20 | Personal-machine burden? | Initial npm/Docker setup is simple, but Docker socket access, backend/provider configuration, persistent volumes, proxy/WebSocket behavior, credentials, and upgrades remain operator concerns. Multiple 2025-2026 issues report restart/proxy/settings/startup failures. | D/issues |

### OpenHands conclusion

OpenHands already solves enough of the broad job that feature-count competition
is irrational. The only relevant gap is the combination of native OpenCode
continuity and Fern's exact result/finalization semantics. Both are absorbable;
therefore direct user preference, not architecture, must decide.

Key pinned source evidence: [conversation leases](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-agent-server/openhands/agent_server/conversation_lease.py#L101-L214),
[ACP registry](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-sdk/openhands/sdk/settings/acp_providers.py#L398-L483),
[ACP auto-approval](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-sdk/openhands/sdk/agent/acp_agent.py#L1445-L1462),
[message request without idempotency key](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-sdk/openhands/sdk/conversation/request.py#L64-L75),
[terminal-state definitions](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-sdk/openhands/sdk/conversation/state.py#L48-L79),
[interrupt behavior](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-agent-server/openhands/agent_server/event_service.py#L1620-L1656),
[chat-driven Git actions](https://github.com/OpenHands/OpenHands/blob/64c1269655012698bc66538967989996191beb6c/src/components/features/conversation/conversation-git-actions-menu.tsx#L76-L98),
and [temporary archive export](https://github.com/OpenHands/software-agent-sdk/blob/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a/openhands-agent-server/openhands/agent_server/file_router.py#L894-L1065).

## 5. Capability And Competitive Matrix

### Closest products

| Capability | Fern now | OpenHands | T3 Code | Orbit | Cursor | Claude/Codex remote |
| --- | --- | --- | --- | --- | --- | --- |
| Fully self-hosted authority | `S` | `D` | `D` | `D` | `N` split plane | `N` connected/split |
| OpenCode runtime | `S` pinned | stable custom ACP plausible; V2 `U` | `D` | `D` | `N` | `N` |
| Native OpenCode UI | `S`, one workspace | not first-class | not first-class | not first-class | `N` | `N` |
| Concurrent isolated attempts | `R` | backend-dependent `D` | optional worktrees | `D` | `D` | `D` |
| Cross-device supervision | browser `S` | browser `D` | native/web `D` | native/web `D` | native/web `D` | native/web `D` |
| Durable command/task receipts | `S` | partial/`U` | `SO` | `SO` | `D`, details `U` | details `U` |
| Conservative terminal truth | manual seal `S` | adapter-dependent `U` | provider-dependent | checkpoint/merge state | terminal API `D` | vendor state `D` |
| Exact Git result | identity `S`, bytes `R` | `U` | checkpoint `D` | exact checkpoint `SO` | branch/artifacts `D` | branch/diff `D` |
| Same-result trusted check | `S` | `U` | `U` | `SO` exact checkpoint tests | `U` | `U` |
| Write-ahead publication | App mode `S` | `U` | `U` | merge path `SO` | `U` | `U` |
| Reconcile lost push/PR response | App mode `S` | `U` | `U` | merge path `SO` | `U` | `U` |
| Result after sandbox deletion | only with repo/backup | conversation/remote Git | checkpoint/remote Git | DB checkpoint/remote Git | remote Git/artifacts | remote Git/vendor record |

### Full competitive audit

This table compresses deployment, runtime/UI, handoff, durability, result,
publication authority, price/license, and the main limitation. Cold-start and
idle values are omitted where vendors publish no comparable measurement.

| Product | Deployment/runtime | Handoff, result, and authority | Price/license and limitation |
| --- | --- | --- | --- |
| OpenHands Agent Canvas | Full self-host or managed; local, Docker, VM, remote, cloud, Kubernetes; OpenHands and ACP agents. | Persistent conversations, responsive web, files/terminal/changes/commits. Exact prompt/result/publication recovery is `U`; native OpenCode UI not first-class. | MIT; models/infra extra. Operational regressions appear in public issues. |
| Cursor Cloud Agents | Vendor plane; hosted VMs or BYOM worker pools; Cursor runtime. | Strong mobile takeover, snapshots, branches, PRs, resumable events. Exact external-effect reconciliation is `U`. | $20 individual, $40/user team plus model usage; proprietary. |
| OpenAI Codex | Cloud, local worktrees, SSH, connected hosts; Codex only. | Cloud tasks and mobile remote control; connected host stops when unavailable; diffs/PRs. Exact finalizer `U`. | CLI Apache-2.0, cloud proprietary; current plan price not pinned. |
| Claude Code | Local, Anthropic cloud, or customer runner; Claude only. | Remote control, mobile approvals, requeue after runner loss; commits/pushes use runtime authority. Context/handoff issues recur. | Proprietary and plan-gated. |
| GitHub Copilot coding agent | GitHub or customer Actions runner; GitHub owns session/repo plane. | Best issue/branch/check/PR integration and mobile review; 59-minute session maximum. Lost-effect contract `U`. | Paid Copilot plus Actions/AI credits; proprietary. |
| Google Jules | Google VM; Gemini/Jules. | Immutable activities, snapshots, schedules, pause/resume, PRs and browser notifications. No customer runner or exact finalizer located. | 15/100/300 daily task tiers through Google plans; proprietary. |
| Ona | Ona cloud or Ona-managed customer VPC; Codex/current customer agents. | Prepared environments and closed-loop PR automations. Current self-hosted control-plane source not found. | From $20/month; current platform proprietary. |
| Warp/Factories | Warp-hosted or enterprise customer workers; Warp, Claude, Codex. | Attach/steer, blocked states, typed parent/child runs, mergeable PR workflow. Warp retains plane/transcript/inference authority. | Free, $20 Build, $50 Business, Enterprise; proprietary. |
| Devin | Devin Cloud or Outposts customer worker; Devin. | Typed workflow replay and branch/PR output; planning/inference remain in cloud. Arbitrary tool-effect safety `U`. | Proprietary; current public dollar price `U`. |
| Coder Agents | Full customer Coder deployment; native Coder agent loop. | DB chat survives workspace deletion/rebuild; questions and queued follow-ups. Agent keeps user Git authority; no separate finalizer found. | AGPL Community; Premium custom. |
| T3 Code | User machine plus optional relay; Claude, Codex, Cursor, Grok, OpenCode. | Event-sourced threads, receipts, cursors, checkpoints, native clients. No managed durable compute or exact finalizer. | MIT `v0.0.35`; user supplies runtime/compute. |
| Orbit | Full self-host, PostgreSQL and outbound runners; Claude, Codex, Kimi, OpenCode. | Closest transaction competitor: exact checkpoints/tests, write-ahead merge, exact-tip gates, durable receipts. Concrete PR finalizer/portable record not found. | MIT at `aca9757`, pre-1.0. |
| Warren | Full self-host; local/Docker/Kubernetes; Pi/Claude. | Fresh run workspace, base pin, salvage, branch and idempotent PR lookup. API dedupe is process-local and finalization intent reconstructed. | MIT at `fe10715`. |
| Deputies | Full self-host; several sandbox providers including Agent Sandbox; Pi runner. | Persistent sessions and callbacks; Git/`gh` tools publish directly. PR resource is recorded after mutation. | MIT at `60c7e18`; several service dependencies. |
| OpenAI Symphony | Full self-host reference scheduler; Codex app-server. | Per-issue workspaces, retry/stall/restart recovery. External writes remain workflow/agent tools. | Apache-2.0 at `8001b52`. |
| Amp | Local/customer runner or hosted Orb; Amp. | Durable threads and sleeping retained Orbs; remote host support. Exact Git finalizer not located. | $20/$200 plans; proprietary. |
| Daytona | Hosted or BYOC split plane; sandbox substrate, not an agent UI. | Stop/pause/archive and disk or memory retention. No task/completion/Git authority. | Usage pricing; maintained platform closed after June 2026, legacy AGPL source unmaintained. |
| E2B | Hosted, Enterprise BYOC, or Apache-2.0 infra. | Memory/filesystem pause and roughly one-second documented resume; no task/effect authority. | Free usage tier; Pro $150 plus usage. |
| Runloop | Hosted or Enterprise VPC; proprietary devboxes. | Suspend preserves disk, not RAM; GitHub token can remain agent-held; no finalizer. | Free usage tier; Pro $250 plus usage; MIT SDKs. |
| Kubernetes Agent Sandbox | Customer Kubernetes `v1.0.0`, `v1beta1`; runtime-neutral sandbox lifecycle. | Stable identity, PVC, suspend, TTL, warm pools. Pod/sandbox state is not task truth or Git authority. | Apache-2.0; cluster/runtime/storage cost and complexity. |

## 6. User-Pain Evidence

The strongest evidence is issue frequency, not survey-quality market sizing.

| Problem | Frequency/severity evidence | Workaround/already solved | Fern advantage | Product implication |
| --- | --- | --- | --- | --- |
| Workspace damage/loss | Six reporters across [Codex #32684](https://github.com/openai/codex/issues/32684), [#33557](https://github.com/openai/codex/issues/33557), [#37998](https://github.com/openai/codex/issues/37998), [#40995](https://github.com/openai/codex/issues/40995), [Claude #90146](https://github.com/anthropics/claude-code/issues/90146), and [VS Code #289973](https://github.com/microsoft/vscode/issues/289973). | Commit/stash, clones, VMs, sandbox vendors, GitButler. | Exact result selection and future per-attempt clone/bundle. Current singleton does not prevent deletion. | Strong case for isolated attempts and retained bytes, not for Kubernetes specifically. |
| Untrustworthy lifecycle state | At least eight reporters: [Claude #68992](https://github.com/anthropics/claude-code/issues/68992), [Codex #38364](https://github.com/openai/codex/issues/38364), [OpenCode #36804](https://github.com/anomalyco/opencode/issues/36804), [Copilot #322314](https://github.com/microsoft/vscode/issues/322314), and orphan worker reports. | Restart, kill processes, inspect host manually. | Fern already refuses false success and records uncertainty. | Strong fit, but automatic completion remains blocked. |
| Cross-device continuity failure | Multiple Claude reports ([#84468](https://github.com/anthropics/claude-code/issues/84468), [#33041](https://github.com/anthropics/claude-code/issues/33041), [#87720](https://github.com/anthropics/claude-code/issues/87720)) and Codex reports ([#37403](https://github.com/openai/codex/issues/37403), [#39968](https://github.com/openai/codex/issues/39968), [#40879](https://github.com/openai/codex/issues/40879), [#40124](https://github.com/openai/codex/issues/40124)). | Hosted agents, SSH/tmux/Tailscale, manual resume. | Persistent exact OpenCode IDs/state and native UI route. | Strongest evidence for the narrow wedge. Fern cannot recover context OpenCode itself lost. |
| Wrong repo/branch/worktree | [Codex #28743](https://github.com/openai/codex/issues/28743), [#31572](https://github.com/openai/codex/issues/31572), [#37591](https://github.com/openai/codex/issues/37591), plus Claude collision reports. | Verify `pwd`/SHA; full clones; worktree managers. | Exact base and result identity. | Show immutable repo/base/result in task UI. |
| Brittle self-host operation | At least six OpenHands issues: [#12072](https://github.com/OpenHands/OpenHands/issues/12072), [#12344](https://github.com/OpenHands/OpenHands/issues/12344), [#13477](https://github.com/OpenHands/OpenHands/issues/13477), [#13578](https://github.com/OpenHands/OpenHands/issues/13578), [#13647](https://github.com/OpenHands/OpenHands/issues/13647), [#16356](https://github.com/OpenHands/OpenHands/issues/16356). | Managed cloud, pinned Compose/Helm, backups. | One Go binary, SQLite, existing origin/recovery checks. Fresh-host proof is still missing. | Simplicity is a quality target, not a unique headline. |
| Missing attention/approval | [OpenCode #33878](https://github.com/anomalyco/opencode/issues/33878), [Claude #64839](https://github.com/anthropics/claude-code/issues/64839), [Codex #39346](https://github.com/openai/codex/issues/39346). | Stay attached; vendor push; polling wrappers. | Receipt/actor primitives fit a durable outbox. Nothing user-complete ships. | Build only after the core handoff test. |
| Duplicate dispatch/cancel | [Codex #36072](https://github.com/openai/codex/issues/36072), [#36529](https://github.com/openai/codex/issues/36529), [Claude #77649](https://github.com/anthropics/claude-code/issues/77649), [#89342](https://github.com/anthropics/claude-code/issues/89342). | Inspect before retry; deterministic IDs. | One of Fern's clearest implemented advantages. | Preserve semantics in the new attempt runner. |
| Checkpoint/fork demand | Requests across [Codex #11626](https://github.com/openai/codex/issues/11626), [#28218](https://github.com/openai/codex/issues/28218), [#39735](https://github.com/openai/codex/issues/39735), [Claude #32631](https://github.com/anthropics/claude-code/issues/32631), [#84425](https://github.com/anthropics/claude-code/issues/84425). | Git commits/branches and vendor rewind/fork features. | Exact Git artifact could seed future attempts. | Real pain, but crowded and not first scope. |

Evidence is not strong enough to lead with duplicate PRs, idle sandbox cost,
generic mobile access, generic interactive takeover, unmeasured speed, or exact
publication journals. The latter is currently technical whitespace, not proven
buyer demand.

## 7. Emerging Primitives

| Primitive becoming practical | Opportunity | Effect on Fern |
| --- | --- | --- |
| OpenCode server plus official web UI | Run the real UI remotely without rebuilding an editor. | Enables the narrow wedge. |
| Durable caller-selected OpenCode inbox/message IDs | Admit before delivery and reconcile lost responses. | Already used by Fern; important implementation advantage. |
| Per-session replay/event work in newer OpenCode | Better restart observation than global SSE. | May simplify Fern, but does not yet prove generic terminal success. |
| V2 execution claims and orphan recovery | Current source can resume bounded orphaned execution at least once after process loss. | Useful after a pinned upgrade; external tool effects may repeat, so Fern keeps uncertainty and fences. |
| V2 session export/import, move, fork, and revert | Transcript continuation and local project movement are becoming first-class. | These omit filesystem/process/credential/effect state and are not Session Teleport. |
| V2 durable session log | Ordered cursor replay plus sync watermark can provide a better execution chronology. | Consume after qualification; retain Fern's independent task, Git, verification, and publication journals. |
| V2 background shell/subagent work and experimental PTY handoff | Some same-host work can survive or be recovered explicitly. | Specialized primitives do not make arbitrary task execution or cross-host migration durable. |
| ACP and native-agent adapters | One control surface can run many harnesses. | Makes generic Fern positioning worse; useful only as a bake-off comparator. |
| Cheap container/VM snapshots and suspend-to-zero | Retained task computers are operationally feasible. | Substrate is commodity; result/effect semantics remain above it. |
| Kubernetes Agent Sandbox `v1.0.0` | Standard Sandbox identity, suspend, PVC, TTL, warm pools. | Useful only after a Kubernetes trigger; not differentiation. |
| Customer-owned runner pools | Repositories/tools can stay on user infrastructure. | Broad BYOC is occupied. |
| Git bundles/content-addressed artifacts | Exact result survives worker deletion and can seed a new attempt. | Small, concrete missing piece for the wedge. |
| Capability-held GitHub publication | Agent can work without GitHub write authority; finalizer publishes selected bytes. | Existing App broker is close; safe mode must remove workspace `gh`. |
| Durable attention records | Questions can outlive browsers and notification delivery. | Valuable later, but pinned OpenCode forms remain process-epoch state. |

Current V2 source still keeps environment mutation, pending permissions, and
forms in process memory. `Session.wait` delegates to a process-local idle waiter;
it is neither restart-stable nor a task-success fact. The opportunity is not a
new sandbox. It is now feasible to combine a native agent server, cheap isolated
compute, durable IDs, and exact Git artifacts in a small personal appliance.
OpenHands and OpenCode can both absorb this, so the window is an
integration-quality experiment rather than a durable category monopoly.

## 8. Candidate Wedge Ranking

Scores are 1-5, with 5 favorable. Weights prioritize pain and weak alternatives
(`2x` each), then Fern advantage, OpenCode fit, visibility, and personal use
(`1.5x` each). Implementation size, low operations, defensibility, demo, and
clarity are `1x`; Grab relevance is `0.5x`. Maximum weighted score is 77.5.
This deliberately prevents portfolio relevance or elegant internals from
outvoting user value.

Abbreviations: `P` pain, `A` alternatives, `F` Fern advantage, `O` OpenCode fit,
`V` visibility, `U` personal use, `I` implementation ease, `B` low operating
burden, `D` defensibility, `M` demo, `C` clarity, `G` Grab relevance.

| Rank | Candidate | P/A/F/O/V/U/I/B/D/M/C/G | Raw /60 | Weighted /100 | Decision |
| ---: | --- | --- | ---: | ---: | --- |
| 1 | Native OpenCode background handoff | 4/3/5/5/5/5/3/3/2/5/5/3 | 48 | 81.9 | Prototype now; kill aggressively. |
| 2 | Safe Git Finalizer | 3/4/5/3/3/3/4/4/3/4/4/5 | 45 | 72.9 | Fallback reusable component; demand test first. |
| 3 | Exact change handoff/Git bundle | 3/4/5/3/3/3/3/4/3/4/4/5 | 44 | 71.6 | Required capability, weak standalone product. |
| 4 | Retained task computer | 4/2/3/4/5/5/2/2/2/5/5/3 | 42 | 71.0 | Packaging for #1; category itself is occupied. |
| 5 | Personal OpenCode appliance | 3/2/4/5/4/5/2/3/1/4/5/3 | 41 | 69.0 | Delivery model, not differentiation. |
| 6 | Agent flight recorder | 4/3/4/3/4/4/2/3/2/4/4/4 | 41 | 69.0 | Narrow to Git/state recovery; full process/effect replay is unrealistic. |
| 7 | Durable attention inbox | 4/2/3/3/5/4/3/3/1/4/5/4 | 41 | 67.7 | Later feature; Claude and others already strong. |
| 8 | Private OpenCode cloud | 2/1/4/5/4/5/2/2/1/4/5/3 | 38 | 62.6 | Privacy/self-hosting alone is occupied. |
| 9 | Session Teleport | 3/2/2/2/5/4/1/2/3/5/5/3 | 37 | 60.6 | Reject until OpenCode publishes a migration contract. |
| 10 | Failure-safe generic background agent | 3/3/5/3/2/4/1/2/3/2/3/5 | 36 | 60.0 | Too abstract and too broad to sell or demo. |
| 11 | Instant OpenCode tasks | 2/1/2/4/5/4/2/3/1/5/5/2 | 36 | 58.7 | Reject until measured against OpenHands and hosted agents. |
| 12 | Result forking | 3/2/3/3/4/3/2/3/1/4/4/3 | 35 | 58.1 | Real demand, but crowded and blocked by per-attempt runtime. |

Generic remote canvas, Kubernetes, multi-agent support, another dashboard,
model routing, and vague privacy/simplicity were rejected before scoring because
they fail the premise.

## 9. Top Three Product Concepts

The numerical top five collapse into two coherent offers: native retained task
computers and exact finalization. The attention inbox is the next distinct
concept, not the third-highest standalone score.

### A. OpenCode Background Mode

| Field | Decision |
| --- | --- |
| Promise | Run isolated OpenCode tasks on your private machine and reopen the exact native task from any device without losing the selected Git result. |
| Target user | An existing OpenCode user with an always-on Linux host and two or more delegated coding tasks per week. |
| Repeated job | Start work, leave, inspect/answer in the real UI, return later, and recover a reviewable result after failure. |
| Current workaround | SSH/tmux, one long-lived OpenCode server, OpenHands ACP, T3 Code, or a hosted cloud agent. |
| Why OpenHands is insufficient | Only if the bake-off proves generic ACP loses native OpenCode UI/config/session fidelity or makes intervention materially worse. Otherwise it is sufficient. |
| Why Fern can win | Fern already owns exact admission, conservative state, result fencing, verification, and publication recovery around OpenCode. |
| User journey | Choose configured repo; submit; watch `queued/working/needs you/uncertain/result ready`; open native UI; steer; leave; seal; verify; optionally publish. |
| Minimum architecture | Current Go/SQLite service, separate per-attempt Docker coordinator, fresh clone, separate OpenCode state volume, task-scoped reverse proxy, Git bundle store, App broker. |
| Do not build | Agent UI, generic ACP layer, Kubernetes, previews, organizations, hosted SaaS, Gateway, automatic completion. |
| Two weekends | Two containers from one base, deterministic routes, phone reconnect, native UI, manual stop/seal, bundle export, delete/reconstruct, comparison with OpenHands. |
| Six weekends | Durable queue, attempt journal, restart/cancel reconciliation, two concurrent tasks, bounded retention, one notification, App publication. |
| Twelve weekends | Packaging, backup/restore of artifacts, measured warm path, durable attention only if OpenCode contract supports it, one optional remote Docker runner if demanded. |
| Acceptance | 10 real tasks; at least 5 useful; native attach used weekly; zero checkout collision; exact result reconstructs after deletion; no duplicate Fern publication. |
| Kill | OpenHands custom ACP is equally usable; native attach is rarely used; setup dominates; fewer than half of tasks are useful; manual sealing is unacceptable; owner returns to one persistent workspace. |
| Public demo | Phone submits two tasks; laptop closes; desktop opens exact OpenCode UI; one container is killed; Fern shows uncertainty and reconstructs the sealed result; App broker opens one draft PR. |
| Tweet | "Fern gives every background task a real OpenCode session on your own machine, then keeps the exact Git result even when the runtime disappears." |
| Maintenance risk | OpenCode V2 churn, state-format compatibility, provider auth seeding, proxy compatibility, Docker cleanup, and a competitor adding native OpenCode support. |

### B. Safe Git Finalizer

| Field | Decision |
| --- | --- |
| Promise | Let an agent propose bytes, then verify and publish exactly the selected Git result without giving the agent GitHub write authority. |
| Target user | Platform/security teams operating coding agents against protected repositories. |
| Repeated job | Determine which exact change was authorized and tested, then avoid duplicate or stale publication. |
| Current workaround | Agent-held `gh`, CI checks, protected branches, manual push, GitHub Actions. |
| Why OpenHands is insufficient | OpenHands automations use agent/workflow GitHub authority; no reviewed exact-result finalizer or lost-response journal was found. |
| Why Fern can win | The complete broker path already exists and has lost-response/conflict/stale-fence tests. |
| User journey | Import result commit/bundle; select; run host check; approve; deterministic branch push; create/find one draft PR; inspect receipt. |
| Minimum architecture | Small Go library/service, canonical manifest, Git object materializer, GitHub App token broker, SQLite effect journal, offline fixture. |
| Do not build | Agent runner, chat UI, model gateway, generic workflow engine, merge bot. |
| Two weekends | Extract one library boundary and CLI around an existing Fern fixture; replay lost push and PR responses. |
| Six weekends | GitHub Check projection, portable record, policy config, one external runtime importer. |
| Twelve weekends | Upstream/OpenHands or T3 adapter only after a real consumer; signed host-attested export. |
| Acceptance | One external repository decision changes because the record identifies a mismatch or ambiguity ordinary CI missed. |
| Kill | Five target interviews produce no repeated need; CI/vendor logs answer the question; integration requires giving the agent the same write token; Orbit ships the complete path first. |
| Public demo | Agent result A is verified; stale result B tries to publish; a response is dropped after PR creation; retry returns the same PR and exact SHA. |
| Tweet | "Agents propose changes; the finalizer tests and publishes one exact Git result, and retries never guess after GitHub goes quiet." |
| Maintenance risk | GitHub API behavior, Git transport ambiguity, policy support, and weak buyer urgency. |

### C. Durable OpenCode Attention Inbox

| Field | Decision |
| --- | --- |
| Promise | Put every OpenCode question or blocked task in one restart-safe queue that opens the exact native context. |
| Target user | One person supervising several long-running OpenCode tasks from phone and desktop. |
| Repeated job | Notice a blocker, understand it, answer safely, and resume later. |
| Current workaround | Keep terminals open, poll sessions, or rely on generic push/Slack notifications. |
| Why OpenHands is insufficient | Canvas has status and mobile web, but no reviewed durable push/dedupe/acknowledgment contract. Claude already solves much of the user-facing job. |
| Why Fern can win | Actor, receipt, event, and task primitives exist; OpenCode deep links preserve native context. |
| User journey | Notification opens exact task; user sees question and age; native UI handles the live answer; Fern records acknowledgment and reconciles state. |
| Minimum architecture | Attention table, transactional outbox, current-state reconciler, one ntfy/webhook adapter, native deep link. |
| Do not build | Synthetic recreation of vanished permissions/forms, native mobile app, broad workflow inbox. |
| Two weekends | Observe one live question, create one deduplicated attention item, deep-link to OpenCode, clear after authoritative read. Do not answer through Fern. |
| Six weekends | Restart/outbox tests, quiet hours, expiry, answer only if a newer OpenCode contract exposes stable context-bound replies. |
| Twelve weekends | Cross-agent support only if another real runtime is used weekly. |
| Acceptance | At least three real blockers are caught without duplicate/stale notifications and one prevents a laptop escape. |
| Kill | Questions are rare; native provider notifications suffice; forms remain process-epoch; users always open the full UI anyway. |
| Public demo | Kill Fern after a question is observed, restart it, receive one notification, open the exact OpenCode form, answer, and see the item clear. |
| Tweet | "One quiet inbox for the OpenCode tasks that actually need you, with every alert opening the real session." |
| Maintenance risk | OpenCode's pinned forms and permissions do not survive process epochs; notification platforms add delivery complexity. |

## 10. Build Versus Integrate

| Path | Benefit | Cost/risk | Decision |
| --- | --- | --- | --- |
| Build Fern independently | Preserves native OpenCode UI and reuses current correctness core. | Duplicates broad Canvas features; single-maintainer operations. | **Recommended only for the narrow two-weekend experiment.** |
| Add OpenCode preset to OpenHands | Gains mature multi-backend Canvas and distribution. | ACP does not prove native UI/session fidelity; Fern's state/finalizer semantics do not map automatically. | Run the bake-off and contribute a preset if ACP is sufficient. |
| Use OpenHands as Fern's UI | Avoids frontend work. | Replaces the very native OpenCode experience being tested and couples to Canvas schemas. | Reject. |
| Use OpenHands Agent Server as runtime | Gains backend abstraction and native-agent adapters. | Adds another conversation authority and Python/service stack around an existing OpenCode server. | Reject initially. |
| Fern finalization extension | Reuses the strongest unique code without competing on UI/runtime. | OpenHands extension point and buyer demand are unproven. | Best fallback after a consumer test. |
| Contribute correctness upstream | Improves ecosystem and avoids duplicate platform. | Requires minimal reproductions and agreement on semantics. | Contribute concrete prompt/cancel/publication bugs; do not upstream a speculative framework. |

## 11. Architecture Consequences

| Component | Decision | Reason |
| --- | --- | --- |
| Current persistent Docker workspace | Keep during the experiment. | Stable baseline and fallback; do not rewrite it. |
| Per-attempt Docker | **Use.** | Smallest substrate for isolated tasks on one trusted host. |
| Remote Docker runners | No, until a second host is used weekly. | Requires leases, identity, artifact transfer, draining, and stale fencing. |
| k3s | No. | Adds no user-visible wedge on one host. |
| Kubernetes Agent Sandbox | No. | Young, useful later, and not task authority. |
| OpenHands Agent Server | No. | Duplicate conversation/runtime authority. |
| ACP | Comparator only. | Test OpenHands interoperability; do not make it the product path. |
| Direct OpenCode server API | **Use.** | Required for exact native session IDs, UI, and status reads. |
| GitHub App broker | **Use in safe mode.** | Keeps write credentials outside the attempt and reuses reconciled publication. |
| Model Gateway | No. | Direct provider access is adequate for trusted personal use. |

Attempt layout:

```text
Fern task/attempt row
  -> private full clone at exact base
  -> one OpenCode container and state volume
  -> task-scoped authenticated route to native OpenCode UI/API
  -> explicit stop/writer fence
  -> exact snapshot commit + Git bundle
  -> optional host verification
  -> optional App-broker draft PR
  -> bounded retained state, then cleanup
```

Do not share a writable checkout or OpenCode data volume between attempts.
Seed trusted configuration deliberately; do not copy a live database while it
is running. An attempt receives direct provider credentials only under the
current trusted-owner boundary and receives no GitHub write credential in safe
mode.

## 12. Bake-Off And Benchmarks

### What was actually run

| Check | Result | Limitation |
| --- | --- | --- |
| `go test ./...` | Pass | Local macOS, mostly unit/integration fixtures. |
| `go test -race ./...` | Pass | Same environment. |
| `go vet ./...` | Pass | Static Go checks only. |
| OpenCode contract harness | 13/13 pass | Pinned beta image and zero-cost fake provider. |
| Lifecycle harness | 14/14 pass | Deterministic fake runtime, local Docker Desktop. |
| Warm stopped-to-ready | 10 runs; 0.447s median total, 0.716s nearest-rank p95/max | Warm fake container, not real OpenCode provisioning or remote Ubuntu. |
| Fake active/stopped container | 12.62 MiB active; 0 B stopped | Fake protocol service, not OpenCode. |
| Real long-lived local OpenCode sample | 687.3 MiB and 1.57% CPU at one instant | Uncontrolled 8-day workspace; not an idle or comparative benchmark. |
| Trimmed Fern binary | 24 MiB | Local arm64 build. |
| OpenHands remote bake-off | Not run | No remote Ubuntu host or model-provider credential was available. |

### Operational inventory

| Metric | Fern current | OpenHands Canvas `v1.16.0` | Status |
| --- | --- | --- | --- |
| Clean install time | Unmeasured | Unmeasured | Must be measured on the same clean Ubuntu image. |
| First successful provider task | Unmeasured | Unmeasured | No provider credential was available. |
| Required runtime | One Go binary, Docker, OpenCode image, Tailscale for private ingress | Node 22.12 + `uv` for npm/npx, or one all-in-one Docker image | Documented, not timed. |
| Logical services | Fern supervisor plus OpenCode container; Tailscale/Docker external | Canvas, Agent Server, Automation Server, and ingress packaged together | Architecture fact; process counts unmeasured. |
| Listener ports | Two loopback listeners; only Fern remote listener goes through Tailscale | One documented ingress on port 8000; backend topology depends on deployment | Documented. |
| Persistent stores | Fern SQLite, host repo, OpenCode volume, optional credential store/backups | `~/.openhands` state plus mounted projects; backend/cloud stores vary | Documented. |
| Idle CPU/memory | No controlled Fern-process result; stopped fake container was 0 | Unmeasured | No comparative claim. |
| Active per-task memory | One uncontrolled OpenCode sample at 687.3 MiB | Unmeasured | Must use the same agent/repo/task. |
| Disk growth/backup/restore | Unmeasured for real use | Unmeasured | Four-week dogfood metric. |
| Upgrade steps | Replace signed Fern binary/image, run compatibility checks, preserve OpenCode state/backup | Update npm package or image; Canvas and Agent Server pins can differ | Documented; failure rate unknown. |
| Operator-only failure modes | Docker/image drift, OpenCode state compatibility, origin/Tailscale, disk and backup | Node/uv or image, persisted secrets/settings, proxy/WebSocket, backend versions, workspace mounts | Source/docs/issues; incidence unknown. |

### Reproducible OpenHands bake-off

Run on a clean Ubuntu 24.04 VM with Docker, Tailscale, one disposable GitHub
repository, and a capped provider account. Pin the exact Agent Canvas package or
image and Agent Server version; do not use `latest` in the retained record. The
official quick-start is `npx @openhands/agent-canvas`, while the documented
container mounts `~/.openhands` and `/projects` into
`ghcr.io/openhands/agent-canvas`. Replace both with an exact package version or
image digest for the test, set `OH_AGENT_SERVER_VERSION`, and record
`agent-canvas --info`, `docker inspect`, mounted persistence, ports, process
tree, and all configuration before testing.

```sh
export PROJECTS_PATH="$HOME/openhands-bakeoff/projects"
export STATE_PATH="$HOME/openhands-bakeoff/state"
export SESSION_API_KEY="$(openssl rand -hex 24)"
export OH_SECRET_KEY="$(openssl rand -hex 32)"
export CANVAS_TAG="ghcr.io/openhands/agent-canvas:1.16.0"
mkdir -p "$PROJECTS_PATH" "$STATE_PATH"
docker pull "$CANVAS_TAG"
export CANVAS_IMAGE="$(docker image inspect "$CANVAS_TAG" --format '{{index .RepoDigests 0}}')"
docker run -d --name openhands-bakeoff -p 8000:8000 \
  -e SESSION_API_KEY="$SESSION_API_KEY" -e OH_SECRET_KEY="$OH_SECRET_KEY" \
  -v "$STATE_PATH:/home/openhands/.openhands" \
  -v "$PROJECTS_PATH:/projects" "$CANVAS_IMAGE"
curl --fail http://localhost:8000/alive
```

Test one first-party preset as the control. For a stable-OpenCode exploratory
treatment, select `Settings > Agent > Custom` and set `opencode acp`, with the
pinned executable and credentials installed on the backend. For the actual Fern
V2 decision, first prove that the pinned `opencode2` release speaks ACP JSON-RPC
over stdio or provide an explicit adapter; process startup alone is not a pass:

1. Record VM SKU, kernel, Docker version, OpenHands commit/images, OpenCode
   version, ACP command, model/provider, repo ID/base SHA, and wall clock.
2. Time clean install, first task, warm task, reconnect, UI open, Canvas restart,
   Agent Server restart, Docker restart, backup, and restore.
3. Launch two tasks from the same base that edit disjoint files; close the
   client, reconnect from a phone, inspect conversation/tools/files/terminal/
   changes, steer each, and record whether OpenCode's native UI is reachable.
4. Restart Canvas, then Agent Server, then Docker during separate tasks. Record
   conversation, process, filesystem, Git, approval, and adapter state after
   each fault.
5. Cancel during provisioning and during a marker-bearing long tool operation.
   After cancellation, attempt a delayed write and a GitHub mutation; determine
   what fence, if any, rejects it.
6. Complete, pause, reopen, delete the sandbox, and enumerate exactly what
   remains. Reconstruct the result without relying on an existing checkout.
7. Create a branch and draft PR. Put an HTTP fault proxy or deterministic fake
   GitHub adapter at the mutation boundary so the mutation succeeds but the
   response is dropped. Retry once and inspect refs/PRs for duplication.
8. Repeat prompt submission with the response dropped after admission; inspect
   event IDs/messages/provider turns for duplication.
9. Capture screenshots and timestamped logs, but classify findings as observed
   only for the exact pin/backend tested.

### Decision targets

| Metric | Fern prototype target | Comparative rule |
| --- | --- | --- |
| Clean install to first task | <=20 minutes | Must not require more operator steps than OpenHands for the target profile. |
| Warm task route to native UI | <=5s p50 | User-visible, measured on the same host. |
| Cold task with image present | <=15s p50 | Include clone and OpenCode health; do not compare only container creation. |
| Task-list reconnect | <=2s p95 | Same tailnet and device. |
| Fern restart reconciliation | <=60s | No false success or duplicate mutation. |
| Idle retained attempt | 0 CPU/RAM after stop | Report disk growth separately. |
| Per-task active memory | Measure, no claim yet | Beat OpenHands only if same repo/model/task/backend. |
| Exact result reconstruction | 100% in fault suite | Must work after attempt container/checkout deletion. |
| Duplicate prompt/PR under injected loss | 0 | Any duplicate is a release blocker. |

## 13. Positioning

### Homepage

> Run the real OpenCode on your own always-on machine, return from any device,
> and keep the exact changes even when the session fails.

### README paragraph

Fern is an experimental background mode for OpenCode. Each task runs in its own
checkout and OpenCode server on a private machine you control. You can open the
actual OpenCode UI to inspect or steer it, leave again, and return to a retained
Git result. Fern records uncertain failures honestly and can verify and publish
the selected result without giving the task GitHub write credentials. The
multi-task mode is a product hypothesis until its published acceptance tests
pass.

### For an OpenHands user

Use OpenHands if its Canvas, agent backends, and workspace model already fit.
Fern is only relevant when you specifically need the native OpenCode UI and
settings for each task plus an exact result/finalization boundary. If the
OpenHands ACP bake-off preserves those well enough, Fern should contribute an
OpenCode preset or finalizer rather than compete.

### For an OpenCode user

Fern keeps OpenCode itself as the interface but moves its runtime to an
always-on private machine. A background task can be reopened from phone or
desktop, and its selected Git result can survive the task environment.

### For a recruiter

Fern is a Go/SQLite distributed-systems project around unreliable agent and
GitHub boundaries: durable idempotency, exact identities, lifecycle
reconciliation, cancellation fences, Git object capture, host verification,
fault injection, and write-ahead external effects. The product decision also
shows restraint: Docker serves the one-host wedge; Kubernetes is deferred until
placement or tenant isolation is real.

### Why not just use OpenHands?

For most people, use OpenHands. It already provides a self-hosted multi-agent
Canvas, many execution backends, automations, persistence, and mobile web.
Fern's experiment is narrower: preserve the native OpenCode experience and keep
an exact recoverable Git result with stricter finalization behavior. If that
difference is not noticeable in the bake-off and weekly dogfood, Fern should
stop expanding.

## 14. Final Roadmap

### First weekend: compare before building

1. Run the OpenHands bake-off subset: one control agent, stable OpenCode custom
   ACP as an exploratory check, a V2 ACP-adapter gate, native UI determination,
   two conversations, phone reconnect, Canvas/Server restart, and deletion.
2. Pin one newer OpenCode V2 candidate and rerun Fern's completion, inbox,
   event-log, form/permission, interrupt, and restart contracts.
3. Record 10 owner tasks: frequency, laptop escape, native attach, blocker,
   useful result, setup time, and failure.

Stop immediately if OpenHands exposes a satisfactory native OpenCode task and
retained result or if the owner does not have two candidate tasks per week.

### First two weekends: disposable proof

1. Add an experimental per-attempt Docker path beside `workspace.Manager`, not
   through it.
2. Use one configured repository, a full private clone, one OpenCode state
   volume, one deterministic route, and manual result sealing.
3. Run two attempts from the same base, attach through the native UI, kill one,
   export each selected result as a Git bundle, delete compute, and reconstruct.
4. Compare the journey and timings with OpenHands on the same machine.

This is throwaway product-test code unless all acceptance checks pass.

### First six weekends: productize only after the gate

Add durable environment ownership, bounded queueing, restart and cleanup
reconciliation, two concurrent attempts, artifact retention, one notification
adapter, exact-result verification, and existing App-broker publication. Keep
manual completion where OpenCode lacks terminal truth.

### First twelve weekends: harden, do not broaden

Ship one signed personal-host release, clean-host installer/doctor, backup and
restore for task artifacts, disk-pressure policy, fault suite, phone/desktop
task detail, and measured operations. Add one outbound remote Docker runner only
if tasks repeatedly need a second machine. Do not add another agent.

### Explicit stop conditions

- OpenHands ACP is equivalent for the owner.
- Native OpenCode attachment is used less than weekly over four weeks.
- Fewer than half of 10 real tasks produce useful unattended results.
- Environment setup or repair consumes more time than delegation saves.
- OpenCode cannot provide a bounded restart/cancel contract without a fork.
- Exact retained results do not change recovery or review behavior.
- No external OpenCode user completes a real task after a documented install.
- The product requires Kubernetes, Gateway, or a new UI to make the first demo
  compelling.

### Intentionally excluded

Generic agents, generic ACP UI, native mobile apps, organizations/RBAC,
schedules/webhooks, previews, multi-agent workflows, hostile public repos,
automatic merge, model routing, cost accounting, Postgres/Redis/NATS/Temporal,
Kubernetes, warm pools, and hosted multi-tenancy.

### Infrastructure triggers

| Component | Add only when |
| --- | --- |
| Kubernetes/Agent Sandbox | A second node, customer Kubernetes deployment, placement/draining, quotas/network policy, or stronger runtime class is a measured requirement. |
| Remote runner protocol | A second machine is used weekly and artifact transfer/leases are unavoidable. |
| Gateway | Provider credentials must leave the workspace, or real budget/cost/routing/fallback requirements appear. |

### Product versus portfolio

Product work is the native OpenCode handoff, per-attempt Docker, exact retained
result, restart behavior, and owner workflow. A Kubernetes adapter, distributed
Gateway, generic conformance suite, signed portable evidence format, and
multi-run evaluation are portfolio-only until an external product gate names a
consumer.

## 15. Primary Sources And Limitations

Primary comparison sources include:

- [OpenHands Agent Canvas](https://docs.openhands.dev/openhands/usage/agent-canvas/overview), [setup](https://docs.openhands.dev/openhands/usage/agent-canvas/setup), [ACP agents](https://docs.openhands.dev/openhands/usage/agent-canvas/acp-agents), [Canvas `v1.16.0`](https://github.com/OpenHands/OpenHands/tree/64c1269655012698bc66538967989996191beb6c), and [Agent Server `v1.44.1`](https://github.com/OpenHands/software-agent-sdk/tree/9d143aac35c2dcec9cbb046ff9f35ac5eb072f6a).
- [OpenCode server](https://opencode.ai/docs/server/), [stable ACP](https://opencode.ai/docs/acp/), [V2 API](https://opencode.ai/v2/docs/api), [current V2 source `4a977b2`](https://github.com/anomalyco/opencode/tree/4a977b2b3158adba43daec52fb3a9ab386dad3a8), and [stable `v1.18.25`](https://github.com/anomalyco/opencode/releases/tag/v1.18.25).
- [Cursor Cloud Agents](https://cursor.com/docs/cloud-agent) and [BYOM](https://cursor.com/docs/cloud-agent/bring-your-own-machine).
- [Codex cloud](https://developers.openai.com/codex/cloud/) and [remote connections](https://developers.openai.com/codex/remote-connections).
- [Claude Code Remote Control](https://code.claude.com/docs/en/remote-control) and [self-hosted environments](https://code.claude.com/docs/en/self-hosted-environments).
- [GitHub Copilot cloud agent](https://docs.github.com/en/copilot/concepts/agents/cloud-agent/about-cloud-agent).
- [Jules changelog](https://jules.google/docs/changelog/) and [limits](https://jules.google/docs/usage-limits/).
- [Warp orchestration](https://docs.warp.dev/platform/orchestration/) and [self-hosting](https://docs.warp.dev/platform/self-hosting/).
- [Devin Outposts](https://docs.devin.ai/cloud/outposts/overview) and [Dynamic Workflows](https://docs.devin.ai/work-with-devin/dynamic-workflows).
- [Coder Agents](https://coder.com/docs/ai-coder/agents), [T3 Code `v0.0.35`](https://github.com/pingdotgg/t3code/blob/v0.0.35/README.md), [Orbit `aca9757`](https://github.com/jianghailong-xy/orbit/tree/aca97577e5e57cb2d6d39e503a035115e1cedd9c), [Warren `fe10715`](https://github.com/jayminwest/warren/tree/fe1071562ac957aacba39beba850ef00e10d879a), [Deputies `60c7e18`](https://github.com/sidpalas/deputies/tree/60c7e186187839a52d56c23d57cf9e22fe9cd5b4), and [Symphony `8001b52`](https://github.com/openai/symphony/tree/8001b52e3062495a16e520e4ceaf8f9de868c4d0).
- [Daytona persistence](https://www.daytona.io/docs/en/persistence/), [E2B persistence](https://docs.e2b.dev/sandbox/persistence), [Runloop lifecycle](https://docs.runloop.ai/docs/devboxes/lifecycle), and [Agent Sandbox `v1.0.0`](https://github.com/kubernetes-sigs/agent-sandbox/releases/tag/v1.0.0).
- Repository-local supporting audits: [Homebase category report](./fern-homebase-category-report-2026-08-28.md), [Agent Change Record audit](./agent-change-record-competitor-audit-2026-08-28.md), [strategy audit](./fern-strategy-audit-2026-08-28.md), and [personal task-computer research](./fern-personal-task-computers-2026-08-30.md).

The complete remote OpenHands bake-off, paid vendor trials, target-Ubuntu Fern
deployment, physical-phone run, and controlled cross-product performance tests
were not performed. Public issue counts demonstrate repeated reports, not
market incidence. Proprietary products may implement safeguards that are not
documented publicly. The recommendation is therefore an experiment with kill
criteria, not a claim that Fern has already found product-market fit.
