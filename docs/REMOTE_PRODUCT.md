# Remote Product Roadmap

This document records the product capabilities Fern needs after its current
remote-phone claim is validated. It is a roadmap, not an implemented-system
contract. [ARCHITECTURE.md](./ARCHITECTURE.md) remains authoritative for current
behavior.

## Product Position

Fern is currently a careful lifecycle wrapper around one self-hosted OpenCode
workspace. Its strongest implemented behavior is not remote chat or agent
orchestration. It is the ability to authenticate an ordinary request, wake
stopped compute behind a stable endpoint, and stop it only after protocol-aware
evidence says doing so is safe.

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

## Current Validation Gate

Before expanding scope, Fern must prove this exact flow with a real provider:

1. Install the merged release on the target Linux host.
2. Reach it from a laptop and phone through Tailscale Serve.
3. Start and steer a provider-backed OpenCode turn.
4. Disconnect while work continues.
5. Observe a safe idle stop and ordinary-request wake.
6. Reconnect to the same completed session state.
7. Reboot the host and recover the same workspace.
8. Back up and restore the repository, Fern state, and OpenCode data.

This is a product validation gate because it tests the complete remote path,
client compatibility, provider behavior, lifecycle policy, and recovery claim.
Failure may change the foundation or priority of every later feature.

## Missing Capabilities

The priorities below target a serious single-user or small trusted-team product,
not a hostile multi-tenant platform.

| Priority | Capability | Product requirement |
| --- | --- | --- |
| P0 | Supported phone/browser control | Show status, submit and steer work, cancel, approve, and reconnect reliably. |
| P0 | Appliance installation | Provide guided initialization, preflight checks, system service setup, updates, and rollback. |
| P0 | Durable remote commands | Give every instruction an explicit queued, delivered, applied, completed, failed, or expired state. |
| P0 | Notifications and approvals | Deliver questions, permission requests, completion, and failure to the user's phone. |
| P0 | Automated backup and restore | Back up, checksum, verify, restore into a fresh workspace, and rehearse recovery automatically. |
| P1 | Setup and resume hooks | Prepare repository dependencies and restart services without rebuilding the base image. |
| P1 | Mobile-safe artifacts | View logs, screenshots, reports, videos, and generated files through authenticated links. |
| P1 | Private preview ports | Route declared application ports with authentication, wake, readiness, and expiry behavior. |
| P1 | Terminal observation and takeover | Follow agent activity, transfer one exclusive write lease to a human, and hand control back safely. |
| P1 | Git onboarding and delivery | Clone repositories, isolate worktrees, inspect diffs, commit, push, and create pull requests. |
| P1 | Multiple workspaces | Create, list, route, stop, recover, and remove more than one repository workspace. |
| P1 | Device identity | Replace one shared Basic password with device pairing, revocation, and short-lived sessions. |
| P1 | Lifecycle observability | Explain wake latency, active blockers, failure/OOM state, runtime versions, and resource use. |
| P2 | Secret and egress controls | Keep provider/Git credentials out of ordinary container environment variables and restrict destinations. |
| P2 | Coordinated upgrades | Safely stop, back up, upgrade Fern/OpenCode, verify compatibility, and roll back. |

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

OpenCode is the first adapter. Other harnesses should be added only when they
offer authoritative activity and quiescence APIs; process or CPU heuristics are
not equivalent.

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

1. A supported headless Linux installation with `init`, `doctor`, update, and
   explicit workspace state.
2. A supported phone/browser control surface with reliable reconnect behavior.
3. Durable commands, push notifications, and remote approvals.
4. Authenticated artifacts and one declared private preview port.
5. Automated backup, verification, and restore.
6. Visible protocol-aware sleep blockers and measured wake behavior.
7. A timeboxed whole-host-wake experiment.

This release should still use OpenCode as the agent. It does not require Fern to
own a model loop, add Temporal, host Git, or build multi-agent reasoning.

## Work After The Phone Demo

The successful phone demo removes the largest product-risk gate: it proves that
the current architecture can deliver its basic remote promise. It does not make
all roadmap work independent.

### Safe Parallel Tracks

These tracks have limited overlap and can proceed concurrently in separate
worktrees with explicit contracts:

| Track | Initial deliverable | Main ownership |
| --- | --- | --- |
| Appliance | `init`, `doctor`, installer, systemd automation, update checks | CLI, packaging, deployment |
| Recovery | `backup`, `verify`, restore-to-new-workspace, automated rehearsal | volume/repository tooling |
| Environment | setup/resume hooks and dedicated cache mounts | configuration and Docker runtime |
| Product evidence | provider-backed end-to-end tests, wake SLO, lifecycle diagnostics | integration tests and observability |

### Work That Needs A Shared Design First

The following features all need stable workspace identity, persisted product
state, and an authenticated control API. Building them independently would
create conflicting state models and duplicate routing logic:

- durable command inbox, notifications, and approvals;
- multiple workspaces and repository onboarding;
- device pairing and authorization;
- artifact and preview routing;
- browser terminal and human/agent write ownership;
- event-triggered or whole-host wake.

Define the smallest control-plane state and API for one workspace first. Then
these can split into server, client, notification, and preview workstreams.

### Practical Parallelism Limit

Immediately after the phone demo, four independent implementation tracks are
reasonable. More parallel work would mostly collide in `config`, the Docker
runtime, proxy routing, and the one-workspace composition root. Once a small
multi-workspace control API exists, the work can fan out further.

The next architectural decision after the demo is therefore not Temporal or
multi-agent execution. It is whether Fern remains one supervised workspace with
a better appliance experience, or becomes one daemon that owns a registry of
workspaces and a remote control API.

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
