# Product Direction

This document owns Fern's product direction. [Architecture](./ARCHITECTURE.md)
and code own current implementation behavior.

## Direction

Fern is a self-hosted control plane for durable remote coding tasks, using
OpenCode as its first agent runtime:

> Submit work remotely, disconnect, return to the same task, authorize an exact
> repository result, verify it under host policy, and publish it safely from a
> workspace that can stop, wake, reboot, and recover.

Fern should not become another model loop or rebuild OpenCode's coding UI.
OpenCode remains authoritative for conversations, tools, process-epoch input,
terminals, files, and diffs. Fern owns the durable journey around those
capabilities.

## Product Boundary

| Concern | Authority |
| --- | --- |
| Task receipt, delivery, attempts, cancellation | Fern SQLite |
| Conversation and tool execution | Pinned OpenCode profile |
| Compute lifecycle and recovery | Fern Docker runtime |
| Repository content | Git and the workspace |
| User-authorized result | Exact Fern seal request and Git evidence |
| Automatic execution result | Future authoritative observer; inactivity is insufficient |
| Verification | Host-owned Fern policy and exact commit proof |
| App publication | Fern receipt/effect journal plus GitHub exact reads |
| Direct workspace publication | Explicit user/prompt intent plus workspace `gh` |
| Repository identity | Configured numeric GitHub identity, revalidated live |

The two GitHub modes are explicit and mutually exclusive. `workspace-gh`
matches the Amp-style workflow: trusted workspace code receives an authenticated
`gh` CLI and direct effects are outside Fern's receipts. `github-app-broker`
keeps credentials on the host and permits receipt-backed publication of one
exact sealed and verified result. Fern must not describe its phone action as an
exclusive publication gate in workspace-`gh` mode.

## Current Product Path

The implemented journey is:

1. Pair a device through one canonical private HTTPS origin.
2. Submit one task with a browser-persisted idempotency key and body.
3. Commit task, attempt, receipt, actor, base SHA, and exact OpenCode IDs before
   wake or delivery.
4. Deliver through journaled phases and reconcile exact IDs without mutating a
   prompt after ambiguity.
5. Project only positive `running` or `input_required` evidence; never infer
   generic completion.
6. Preview and authorize one exact clean snapshot under `AcquirePaused`.
7. Seal that snapshot as `user_seal`, mark the attempt `superseded`, and complete
   the task without claiming OpenCode success.
8. Optionally verify the exact commit with host-owned shell-free policy.
9. In App mode, atomically admit one publication receipt and reconcile one exact
   branch and draft PR through the paired API. The embedded task page currently
   displays but does not initiate publication. In workspace-`gh` mode, use
   ordinary explicit `git`/`gh`.
10. Read coherent task, seal, result, verification, publication, and PR status
    from the phone task page.

Lifecycle, offline backup/rollback, encrypted GitHub credential custody,
schema-4 to schema-6 compatibility fixtures, readiness telemetry, and an
attested tag-release workflow support that journey.

## Evidence Boundary

Checked-in automated evidence includes unit/race tests, deterministic browser
and lifecycle harnesses, a zero-cost pinned OpenCode contract harness, release
reproducibility, schema upgrade/byte-restore/upgrade, backup archive and
operational recovery tests, and a synthetic self-test of the physical evidence
recorder.

It does not prove a physical Android or iOS rehearsal occurred. It also does not
prove target Ubuntu/systemd boot, physical reboot, replacement-host restore,
abrupt-power-loss behavior, real private TLS/WSS, physical stream/PTY revocation,
independent tailnet ACL denial, provider-funded execution, or live
organization-specific GitHub policy. Those require operator-controlled evidence
tied to an exact build. See [Phone Field Demo](./FIELD_DEMO.md) and
`integration/production-rehearsal/README.md`.

## Remaining Product Milestones

### Physical Acceptance

Run and retain one complete source-host to replacement-host rehearsal:

- exact release/source/image preflight;
- physical reboot and service recovery;
- verified offline backup;
- old-host service and origin fence;
- replacement-host restore and health;
- real external TLS/WSS browser and terminal traffic;
- physical phone task journey and revocation;
- independent ACL-negative observation;
- redacted final evidence review.

The recorder validates supplied observations but does not perform these steps.

### Generic Completion

The pinned OpenCode profile has no durable generic terminal-success/failure
object. Automatic observer-authorized sealing remains blocked until an exact,
restart-safe primitive can provide two identical success observations inside
`AcquireQuiesced`. Idle, empty inbox, process death, and volatile events remain
invalid evidence.

### Durable Input And Notifications

Fern records `input_required` but has no durable approval table or phone answer
API. It must not recreate vanished form/permission options after an OpenCode
process epoch. A future design needs exact context hashes, actor receipts,
delivery reconciliation, and restart semantics.

Notifications, transactional outbox delivery, PR/CI polling, and review
continuation are not implemented. Build them only after durable event and actor
contracts are fixed; external events are hints and current state must be
reconciled.

The paired App publication API is implemented, but the embedded phone task page
has no publication button or persisted publication command. That UX must retain
the exact idempotency key until acceptance, following the submission pattern.

### GitHub Onboarding And Credential Lifecycle

App discovery primitives exist, but installation/repository selection still
requires operator configuration and first activation requires restart. Complete
onboarding should present and persist an exact numeric selection without
creating a mutable confused-deputy path.

Age-encrypted export/import/rotation and local rollback are implemented. Fern
cannot revoke superseded App keys or workspace OAuth tokens at GitHub. External
revocation and proof remain an operator obligation. Import/rotation also
requires an active prior generation for rollback and cannot bootstrap an empty
credential store; onboarding/login or full host restore owns that case.

### Recovery Atomicity

Offline restore and rollback are staged and retain a durable pre-restore
generation, but filesystem roots and Docker volumes activate sequentially.
Physical abrupt-power-loss characterization and explicit operator recovery from
partially activated domains remain necessary. Online cross-store snapshots are
not a current product claim.

### Operator Browser Boundary

The operator listener stays host-only and is not a supported OpenCode browser
origin. Exact origin and Fetch Metadata checks exist, but operator HTML forms do
not have device-style per-request CSRF tokens. Keep controls CLI/host-local or
add a control-only browser surface before broadening support.

## Sequencing

The critical path is now:

```text
current durable phone-to-verified-App-PR path
  -> one retained physical production rehearsal
  -> generic terminal-success primitive or explicit continued user authority
  -> durable input decisions
  -> notification and PR/CI continuation
  -> smoother GitHub selection and activation
```

Physical evidence work can proceed in parallel with generic completion research,
durable input design, and notification contracts. None may weaken current
receipt, exact-identity, cancellation, result, verification, or publication
fences.

## Later Backends

Docker remains the supported single-owner backend. Before adding another,
separate workspace policy from Docker-specific status, endpoint, storage, logs,
and lifecycle operations. Kubernetes is useful only for a concrete fleet or
multi-node deployment; it does not supply Fern's durable task semantics or
hostile tenant isolation. A hostile shared service additionally needs workload
identity, secret/egress policy, audit, and a sandbox such as gVisor, Kata, or a
microVM boundary.

## Not Now

- A second coding conversation UI.
- Native Fern mobile applications.
- Multiple agent-provider adapters.
- Multi-agent orchestration or a general workflow builder.
- Kubernetes as a requirement for the personal release.
- Hostile multi-tenancy on ordinary shared-kernel Docker.

## Success Criteria

Fern succeeds when the task and its exact evidence survive disconnects and
lifecycle changes, not merely when an HTTP connection or container remains
alive. The next acceptance milestone is one retained physical
phone-to-tested-draft-PR and replacement-host recovery journey, with every
external claim tied to redacted evidence and no inferred success.
