# Product Direction

This document is authoritative for Fern's product direction. It describes
planned boundaries, not implemented behavior. [ARCHITECTURE.md](./ARCHITECTURE.md)
and the code remain authoritative for the current system.

## Direction

Fern should become a self-hosted control plane for durable remote coding tasks,
using OpenCode as its first agent runtime.

> Submit work remotely, disconnect, return to the same task, inspect an
> attributable tested result, and publish it safely from a workspace that can
> stop, wake, reboot, and recover.

Fern should not become another model loop or rebuild OpenCode's coding UI.
OpenCode remains authoritative for conversations, tools, permissions,
questions, terminals, files, and diffs. Fern owns the durable journey around
those capabilities.

## Product Boundary

| Concern | Authority |
| --- | --- |
| Task receipt, delivery state, attempts, and cancellation intent | Fern |
| Conversation and tool execution | OpenCode |
| Compute lifecycle and recovery | Fern runtime backend |
| Repository content | Git and the workspace |
| Verification and result provenance | Fern |
| Push and pull request effects | Fern publication broker |
| Repository authorization | GitHub App broker |
| Infrastructure scheduling | Docker now; other backends later |

The central product model should become:

```text
Workspace
  contains Tasks
    contain Attempts
      map to OpenCode Sessions
      produce Results
      may produce Publications
```

A Fern task is not a duplicate transcript. It is the durable record proving
that Fern accepted an instruction, whether OpenCode received it, what attempt
ran, whether cancellation was requested, and which exact result was verified
or published.

## First Product Outcome

From a supported phone browser, a user can:

1. Pair one device through a private TLS route.
2. Submit one durable, idempotently accepted task and disconnect.
3. Return to its current status and OpenCode session.
4. Answer an approval or question.
5. Inspect the changed files and verification tied to an exact commit.
6. Publish one draft pull request through a narrow host-side credential.
7. Stop, wake, restart, and restore without losing completed work.

The current field demo validates only a constrained portion of this outcome.
See [FIELD_DEMO.md](./FIELD_DEMO.md) for its exact claim.

## Roadmap

### Before The Phone Demo

Do not add a new client, runtime, scheduler, or persistence model. Freeze scope,
review and commit the hardening changes, pass every local gate, rehearse with a
disposable repository and spend-limited credentials, then run the real-phone
sequence and retain redacted evidence.

### Next: Durable Remote Tasks

1. Replace coarse workflow records with transactional task, attempt, receipt,
   event, approval, result, and publication records.
2. Persist a task before waking OpenCode or submitting its prompt.
3. Add idempotent delivery, explicit ambiguous states, durable cancellation
   intent, and startup reconciliation.
4. Add a small Fern task inbox and reconnectable event cursor under `/fern/*`.
5. Deep-link into the official OpenCode session instead of rebuilding its UI.
6. Notify on input required, completion, failure, and publication readiness.
7. Bind verification and publication to the same exact Git commit.

SQLite is sufficient for the first single-host task store. A distributed event
platform is not required.

### Then: Product And Security Completion

1. Add GitHub App repository onboarding and short-lived scoped credentials.
2. Add versioned setup and resume hooks with bounded logs and failure states.
3. Move provider and Git capabilities behind enforceable brokers where
   practical, and add attributable egress controls.
4. Automate backup, fresh-host restore, upgrade, rollback, and old-host fencing.
5. Add private previews, mobile-safe artifacts, and bounded CI/review follow-up.
6. Add a workspace registry only after one durable task journey is complete.

### Later: Execution Backends

Docker remains the supported backend for the single-owner, trusted-host
product. Before adding another backend, separate the workspace controller from
Docker-specific status, endpoints, locks, storage, logs, and CLI operations.

Kubernetes becomes useful for a workspace fleet, multi-node scheduling,
distributed reconciliation, or a concrete enterprise deployment. It does not
provide durable task semantics or strong tenant isolation by itself. A shared,
hostile multi-tenant service also needs a sandbox runtime such as gVisor, Kata,
or a microVM boundary, plus external identity, secrets, egress, and audit.

For Grab, Fern should integrate its OpenCode-aware lifecycle and durable task
coordination with a Palana-style Kubernetes platform. Grab's platform should
continue to own pod scheduling, storage, ingress, workload identity, Vault,
egress policy, and audit. Fern should not run its Docker-daemon model inside an
agent pod.

## T3 Code Decision

T3 Code is a useful benchmark for mobile clients, durable command receipts,
event replay, terminals, files, Git views, and multi-provider orchestration. It
is not a drop-in frontend for Fern. Adopting its server would make T3 the thread
and application authority while Fern became its lifecycle supervisor.

Fern may run a time-boxed, version-pinned T3 experiment against a Fern-managed
OpenCode server. The experiment must prove OpenCode V2 compatibility, joint
T3/OpenCode quiescence, crash recovery, Git checkpoint ordering, publication
safety, and persistence. Do not fork T3, reproduce its private RPC contract, or
make it a release dependency before those results exist.

Fern can adopt T3-like interaction contracts without adopting T3's runtime:

- stable task identity;
- durable command receipts;
- monotonic reconnect cursors;
- explicit connection and execution states;
- mobile task and result views;
- deep links into the authoritative coding session.

## Not Now

- A second coding conversation UI.
- Native Fern mobile applications.
- Multiple agent-provider adapters.
- Multi-agent orchestration or a general workflow builder.
- Kubernetes as a requirement for the personal release.
- Direct Firecracker fleet management.
- Hostile multi-tenancy on ordinary shared-kernel Docker containers.

## Success Criteria

Fern is closer to Amp when the task, rather than the HTTP connection or
container, survives disconnects and lifecycle changes. The next release should
be judged by one complete phone-to-tested-PR journey, not by the number of
runtimes, schedulers, or infrastructure features it supports.

## Research Sources

- [Amp Orbs](https://ampcode.com/manual/orbs)
- [T3 Code architecture](https://github.com/pingdotgg/t3code/blob/main/docs/internals/overview.md)
- [T3 Code remote architecture](https://github.com/pingdotgg/t3code/blob/main/docs/internals/remote.md)
- [Kubernetes multi-tenancy](https://kubernetes.io/docs/concepts/security/multi-tenancy/)
- [Kubernetes RuntimeClass](https://kubernetes.io/docs/concepts/containers/runtime-class/)
- [Grab Palana architecture](https://engineering.grab.com/part-2-palana-architecture)
- [Firecracker](https://firecracker-microvm.github.io/)
