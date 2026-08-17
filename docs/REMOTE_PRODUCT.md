# Remote Product Roadmap

This document defines the end-to-end outcome Fern must support and records the
gap between that outcome and the implemented lifecycle component. It is a
roadmap, not an implemented-system contract. [ARCHITECTURE.md](./ARCHITECTURE.md)
remains authoritative for current behavior.

## Product Position

Fern is currently a lifecycle and same-origin delivery wrapper around one
self-hosted OpenCode workspace. The official OpenCode web UI is already served
unchanged through Fern's stable origin. Fern's strongest implemented behavior
is authenticating an ordinary request, waking stopped compute, and stopping it
only after V2 activity evidence says doing so is safe.

The remote-agent market already provides managed sandboxes, phone control,
worktrees, schedules, webhooks, previews, collaboration, and hibernation. Cursor,
Amp, T3 Code, Claude Code, Codex, Devin, Copilot, Replit, OpenHands, Coder, and
Daytona each cover substantial parts of that surface.

Fern should therefore aim at a narrower product:

> A self-owned Linux coding-agent computer that can safely sleep, wake remotely,
> survive failures, and remain fully recoverable.

Self-hosting, persistence, or hibernation alone are not differentiators. The
potential differentiation is their combination with protocol-aware lifecycle
safety, owner-controlled data, reliable remote delivery, and simple operations.

## Acceptance Outcome

The first credible product outcome is:

> From the official OpenCode UI on a supported phone browser, authorize one
> repository, submit work, answer any approval, and return to a tested draft
> pull request. Fern can then stop, wake, reboot, continue through CI or review
> feedback, and restore onto a fresh host without losing completed work.

A pushed branch is an intermediate result. A correctly proxied HTTP response is
not a durable task, and surviving files are not proof that the workflow can be
recovered.

## Acceptance Matrix

The field rehearsal validates the existing lifecycle component. Product
acceptance validates the user outcome. Neither has been completed.

| ID | Requirement | Current status |
| --- | --- | --- |
| `FIELD-INSTALL` | Install exact Fern/OpenCode artifacts on Ubuntu with systemd and Tailscale | Partially implemented; `fern init` and `fern doctor --phone` cover local demo configuration and transport checks, while systemd appliance installation remains manual |
| `FIELD-PHONE` | Use the official OpenCode UI on supported phone browsers to connect, submit, steer, cancel, approve, and reconnect | Ready for rehearsal through a one-time pairing QR and restart-safe device cookie; real-phone interaction is not yet accepted |
| `FIELD-PROVIDER` | Complete a real provider-backed turn after client disconnect | Not run; continuation semantics are upstream-dependent |
| `FIELD-SLEEP` | Stop only after completed work and wake through the actual client sequence | Deterministic fixture passes; phone/provider path unproven |
| `FIELD-REBOOT` | Reboot while running or paused and recover automatically | Blocked; a running container stopped by host shutdown is classified as failed |
| `FIELD-RESTORE` | Restore all durable state onto a fresh host and resume | Blocked; backup recipe exists, complete restore does not |
| `E2E-REPO` | Install a GitHub App on one selected repository and clone an exact base SHA | Blocked; host path must already exist |
| `E2E-SETUP` | Run versioned project setup with credentials, logs, failure state, and caches | Blocked; no setup/resume contract |
| `E2E-TASK` | Submit one idempotent phone task with durable status and cancellation | Partially implemented; Fern durably associates workflows with OpenCode sessions, but prompt delivery and cancellation remain live OpenCode operations |
| `E2E-PR` | Commit with deterministic identity, push one Fern branch, and create one draft PR | Host-credential prototype supports durable in-process publication and the standalone CLI; GitHub App credentials and deterministic commit identity remain blocked |
| `E2E-APPROVAL` | Notify, inspect, answer, expire, and audit an OpenCode approval from the phone | Blocked; approvals only influence idle detection |
| `E2E-OUTPUT` | Inspect diff, tests, logs, and selected artifacts from the phone | Blocked; no artifact surface |
| `E2E-CI` | Correlate CI/review events to the PR head and run a bounded follow-up | Blocked; no GitHub event or polling integration |
| `E2E-RECOVERY` | Preserve task, Git, PR, approval, and artifact state across process/host failure | Partially implemented for devices, workflow/session correlations, and publication operations; delivery, approval, and artifact recovery remain blocked |

Anything required by this matrix is P0 for the declared product. A narrow field
rehearsal may still be useful before all P0 work exists, but passing it must not
be described as product validation.

## Missing Capabilities

The priorities below target a serious single-user or small trusted-team product,
not a hostile multi-tenant platform.

| Priority | Capability | Product requirement |
| --- | --- | --- |
| P0 | Reboot-safe lifecycle | Recover running and intentionally paused workspaces after Docker and host restart without misclassifying normal shutdown as failure. |
| P0 | Official UI phone acceptance | Validate the unchanged OpenCode UI through Fern for submit, steer, cancel, approve, diff review, and reconnect. Build no second coding client. |
| P0 | Appliance installation | Provide guided initialization, preflight checks, system service setup, updates, and rollback. |
| P0 | GitHub App and repository onboarding | Authorize selected repositories, clone exact source state, and refresh narrow credentials without placing them in the container. |
| P0 | Git publication | Configure identity, allocate one branch, commit, push through a host broker, and create or update one draft PR. |
| P0 | Project setup | Run deterministic setup/resume hooks with private dependencies, logs, timeouts, failure state, and persistent caches. |
| P0 | Durable remote commands | Give every instruction an explicit queued, delivered, applied, completed, failed, or expired state. |
| P0 | Notifications and approvals | Deliver questions, permission requests, completion, and failure to the user's phone. |
| P0 | CI and review continuation | Reconcile exact PR head, checks, review comments, human edits, conflicts, and bounded follow-up attempts. |
| P0 | Model and budget preflight | Validate provider access and enforce task runtime/token/cost limits before and during work. |
| P0 | Automated backup and restore | Back up, checksum, verify, restore into a fresh workspace, and rehearse recovery automatically. |
| P0 | Coordinated upgrades | Quiesce, back up, upgrade Fern/OpenCode, verify compatibility, and roll back. |
| P0 | Mobile-safe results | View the diff, verification, logs, and selected generated artifacts through authenticated links. |
| P0 | Device identity | Extend implemented restart-safe device identity, listing, revocation, and expiry with production enrollment and recovery policy. |
| P0 | Credential boundaries | Broker Git credentials and separate setup, agent, publication, registry, and provider credentials. |
| P1 | Private preview ports | Route declared application ports with authentication, wake, readiness, and expiry behavior. |
| P1 | Terminal observation and takeover | Follow agent activity, transfer one exclusive write lease to a human, and hand control back safely. |
| P1 | Multiple workspaces | Create, list, route, stop, recover, and remove more than one repository workspace. |
| P1 | Lifecycle observability | Explain wake latency, active blockers, failure/OOM state, runtime versions, and resource use. |
| P1 | Egress controls | Restrict and audit package, Git, model, preview, and arbitrary network destinations. |

Enterprise SSO, SCIM, Kubernetes, hostile multi-tenancy, multiplayer editing,
multi-agent orchestration, and a general workflow builder are not required for a
credible personal product.

## Credible White Space

No exhaustive market search can prove that no implementation exists. The
following contracts were not found as complete, public capabilities in the major
products reviewed.

### Whole-Host Wake

Connected-machine products generally require the user's machine to remain awake.
Fern could place a small wake coordinator outside the agent host, start a
physical machine or cloud VM through Wake-on-LAN or a provider API, hold the
original request, and forward it after Fern and OpenCode become ready.

This extends scale-to-zero from one container to the complete agent computer. It
requires an external always-on relay or LAN helper because Fern cannot receive a
request while its own host is asleep.

### Explainable Agent-Safe Sleep

Fern can expose why a workspace is not safe to stop: an active turn, shell, PTY,
permission request, form, held request, watcher loss, ambiguous status, or failed
final check. This makes safe hibernation an inspectable contract rather than a
generic inactivity timer.

OpenCode V2 is the sole supported execution and UI primitive. Adding other
agent harnesses is outside this roadmap; process or CPU heuristics are not an
acceptable substitute for authoritative activity and quiescence APIs.

### Exportable Agent Workstation

Fern can produce an owner-controlled bundle containing repository and
uncommitted state, OpenCode data, environment definition, artifacts, checksums,
and exact Fern/image versions. A restore command should recreate it on another
host and verify the result.

Git patches alone do not preserve a remote workstation. The export must also
state what it cannot preserve, including in-progress provider streams, tool
processes, and permission continuations.

### Reliable Delivery To A Sleeping Host

A remote instruction should retain one identity and visible state while the
host is sleeping, rebooting, or temporarily unreachable. Submission must be
idempotent, cancellation explicit, and the distinction between delivered and
applied observable.

This is directly a remote-agent problem: an ordinary live WebSocket cannot prove
that a phone command reached an intermittently available agent computer.

### Private Wakeable Outputs

Authenticated artifact and preview URLs can wake the same private workspace and
serve output without uploading it to a managed agent vendor or public artifact
host. The distinctive scope is private, owner-operated, and able to use local
dependencies; generic previews already exist in Amp, Cursor, Replit, and other
products.

## Recommended First Release

A coherent first remote-product release should include:

1. A supported headless Linux installation with `init`, `doctor`, safe update,
   rollback, and reboot recovery.
2. The official OpenCode web UI accepted on supported phone browsers, with
   Fern-owned device pairing and durable shadow delivery records.
3. GitHub App onboarding for one selected repository.
4. Versioned project setup with persistent caches and explicit failure state.
5. Host-brokered publication to one Fern-owned branch and one draft PR.
6. Phone notifications, remote approvals, diff/test/log/artifact review, and
   bounded CI or review follow-up.
7. Automated backup, fresh-host restore, and visible protocol-aware sleep
   blockers.

This release should still use OpenCode as the agent. It does not require Fern to
own a model loop, add Temporal, host Git, or build multi-agent reasoning.

## Work Sequencing

The field phone rehearsal remains valuable, but it is not the single blocker
after which all product work becomes safe. Before provisioning the host, fix or
explicitly isolate the running-state reboot failure and define the official-UI
phone acceptance matrix. The GitHub, task, and setup boundaries can be designed
in parallel with that field work.

### Safe Parallel Tracks

These tracks have limited overlap and can proceed concurrently in separate
worktrees with explicit contracts:

| Track | Initial deliverable | Main ownership |
| --- | --- | --- |
| Lifecycle | Reboot-safe Docker semantics, provider-backed field test, wake SLO | runtime and integration tests |
| GitHub | App onboarding, host broker, branch push, draft PR | host control API and GitHub client |
| Appliance/recovery | `init`, `doctor`, installer, backup, fresh-host restore, update rollback | CLI, packaging, deployment |
| Environment | Setup/resume hooks, private dependency contract, cache mounts | configuration and Docker runtime |

### Work That Needs A Shared Design First

The following features all need stable workspace identity, persisted product
state, and an authenticated control API. Building them independently would
create conflicting state models and duplicate routing logic:

- durable command inbox, notifications, and approvals;
- multiple workspaces and repository registry;
- device pairing and authorization;
- artifact and preview routing;
- official-UI deep links and human/agent write ownership;
- event-triggered or whole-host wake.

Define the smallest control-plane state and API for one workspace first. Keep
Fern-owned browser handlers under `/fern/*`; every coding-session route remains
owned by the official OpenCode UI. Then the work can split into server,
notification, publication, and preview workstreams.

### Practical Parallelism Limit

Four implementation tracks are reasonable after their boundaries are written.
More parallel work would mostly collide in `config`, the Docker runtime, proxy
routing, and the one-workspace composition root. Once a small control API and
workspace registry exist, the work can fan out further.

The next architectural decisions are therefore not Temporal or multi-agent
execution. They are the durable task record, the host-side GitHub capability
broker, and whether Fern becomes one daemon that owns a registry of workspaces
and a remote control API.

## Sources

- [Cursor Cloud Agents](https://cursor.com/docs/cloud-agent)
- [Amp Orbs](https://ampcode.com/manual/orbs)
- [T3 Code](https://github.com/pingdotgg/t3code)
- [Claude Code Remote Control](https://docs.anthropic.com/en/docs/claude-code/remote-control)
- [OpenAI Codex Remote](https://developers.openai.com/codex/remote)
- [Devin session tools](https://docs.devin.ai/work-with-devin/devin-session-tools)
- [Coder Agents](https://coder.com/docs/ai-coder/agents)
- [OpenHands Enterprise](https://docs.openhands.dev/enterprise)
- [Daytona persistence](https://www.daytona.io/docs/en/persistence/)
- [Grab Palana architecture](https://engineering.grab.com/part-2-palana-architecture)
