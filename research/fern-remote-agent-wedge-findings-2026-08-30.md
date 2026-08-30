# Fern Remote-Agent Wedge: Findings

**Date:** 2026-08-30

**Fern baseline:** `ab945b5a00db3a310b3fcc30fe8bc99669598b6f`
**Related evidence report:** [Fern's Defensible Remote-Agent Product Wedge](./fern-defensible-remote-agent-wedge-2026-08-30.md)

## Verdict

Fern should test a narrow OpenCode-specific product. It should not expand into a
generic remote-agent platform.

The product hypothesis is:

> Run isolated OpenCode tasks on an always-on private machine, reopen the exact
> native OpenCode environment from any device, and retain an exact Git result
> when the runtime stops or fails.

This is worth two weekends of prototype work and four weeks of dogfood. It is
not yet strong enough to justify a standalone Fern 2.0.

## My Main Findings

### 1. The broad product idea is no longer differentiated

OpenHands Agent Canvas already covers most of the originally imagined product:

- self-hosted, always-on operation;
- remote and browser access;
- multiple conversations and automations;
- local, Docker, VM, cloud, remote, and Kubernetes backends;
- OpenHands and ACP agents;
- files, terminal, conversation, changes, and Git actions;
- pause, resume, persistence, schedules, and GitHub workflows.

Cursor, Claude, Codex, Copilot, Jules, Coder Agents, T3 Code, Orbit, Warren, and
Deputies occupy adjacent versions of the same job. Remote execution, mobile
supervision, task queues, isolated environments, multiple agents, BYOC, and
issue-to-PR automation are now baseline capabilities.

Fern cannot win by implementing fewer versions of those features.

### 2. OpenHands is a strong substitute, but not the same OpenCode experience

OpenHands can launch arbitrary stdio ACP agents. Stable OpenCode documents an
`opencode acp` command, so that combination is plausible. OpenCode V2, which is
Fern's actual target, does not yet have a pinned and tested ACP contract in this
repository.

Even if the ACP path works, Canvas remains the user interface. It does not expose
the native OpenCode server and UI as a first-class task surface. The reviewed
OpenHands ACP bridge also automatically accepts the first permission option,
normally `allow_once`, instead of preserving OpenCode's native interactive
permission flow.

This creates a real product difference, but not automatically a valuable one.
The owner must prefer the native OpenCode handoff in repeated use. If Canvas ACP
is equally effective, Fern should not compete with OpenHands.

### 3. Fern's strongest engineering is real, but mostly invisible

Fern already has an unusually complete correctness chain:

1. Persist task intent and exact OpenCode IDs before delivery.
2. Reconcile a lost response without silently replaying a prompt.
3. Refuse to infer success from idle state or event silence.
4. Fence Fern-controlled work after cancellation.
5. Select one exact clean Git commit and tree.
6. Verify that same selected result under host policy.
7. Record push and PR-create intent before mutation.
8. Reconcile ambiguous GitHub responses through exact reads.

The test suite supports these claims. During this research, the Go tests, race
tests, vet, 13 OpenCode contract scenarios, and 14 lifecycle scenarios passed.

This work is a genuine implementation advantage. It is not yet a product wedge
because most users do not ask for task receipts, fencing epochs, or publication
journals. The value becomes visible only when it prevents lost work, false
success, duplicate execution, wrong-result verification, or stale publication.

### 4. There is repeated pain around handoff and runtime truth

Current issues across Claude, Codex, OpenCode, Copilot, and OpenHands repeatedly
report:

- sessions that remain running after work ended;
- workers that continue after the UI lost control;
- duplicate tasks after reconnect or retry;
- context loss during cross-device continuation;
- wrong worktree or branch displays;
- agents modifying another task's checkout;
- repository deletion or cleanup damage;
- missing or unactionable approval notifications;
- brittle self-hosted restart, proxy, and credential recovery.

This evidence supports a product centered on reliable continuation and retained
results. It does not prove demand for a generic self-hosted agent canvas or a
standalone publication-finalizer business.

### 5. Native handoff is more promising than Session Teleport

Same-host, cross-device continuation is practical: keep the OpenCode server and
state on the remote machine and reconnect clients to the exact session.

Live Session Teleport is not currently supportable. OpenCode session
export/import and move/fork primitives do not capture the complete repository,
processes, terminals, environment variables, credentials, pending interactions,
or uncertain external effects. Copying OpenCode's database or data directory
while active would depend on unsupported internals.

Fern should build stable remote attachment, not claim live host migration.

### 6. Current OpenCode V2 improves the opportunity but does not finish it

Current OpenCode V2 source is materially ahead of Fern's pinned
`0.0.0-next-17444` build. It includes useful primitives such as:

- durable session event logs;
- durable execution claims and bounded orphan recovery;
- session export/import, move, fork, and revert;
- specialized background shell and subagent work;
- experimental same-host PTY handoff.

These can reduce Fern-owned recovery machinery after a carefully characterized
upgrade. They do not provide exactly-once tool execution, generic terminal task
success, durable pending permissions/forms, full environment checkpoints, or
cross-host migration.

Fern still needs its own task, result, artifact, attention, credential, and
publication boundaries.

### 7. Exact Git identity is insufficient without Git object retention

Fern currently records an exact result commit and tree, but it does not yet
retain a portable Git bundle independently from the live repository. A commit
hash is not a durable artifact if the only object database disappears.

The first disposable-task prototype must export reconstructable Git objects
before deleting the task checkout. This is a more important early feature than
Kubernetes, previews, schedules, or multiple agent types.

### 8. Safe publication applies only when the agent lacks write authority

Fern's GitHub App broker can safely publish the selected result and reconcile
ambiguous pushes and PR creation. The guarantee does not apply when the
workspace has unrestricted authenticated `gh` access.

An agent with a GitHub write token can publish from a stale or canceled runtime
outside Fern's journal. Therefore the proposed safe mode must:

- give the task no GitHub write credential;
- use a separate read path for checkout where needed;
- let only Fern's App broker push the selected result;
- describe cancellation as fencing Fern-owned authority, not rolling back every
  external effect.

### 9. Kubernetes would increase work without testing the wedge

Kubernetes Agent Sandbox is a credible future lifecycle substrate. It offers
Sandbox identity, PVCs, suspend/resume, TTLs, and warm-pool primitives. It does
not provide task completion, exact result retention, prompt deduplication,
cancellation safety, or publication authority.

Single-node k3s would add controller, CNI, storage, CRD, upgrade, and diagnostic
failure modes without making the first user demo more compelling. Per-attempt
Docker is sufficient for a trusted owner on one host.

Kubernetes becomes justified only after a real need for multiple nodes,
customer Kubernetes, placement, draining, quotas, network policy, or stronger
runtime classes.

### 10. Fern should preserve OpenCode rather than abstract it

Generic agent support would weaken the only plausible product difference. Fern
should initially use the direct OpenCode V2 server API and native UI. ACP is
useful as a competitive test, not as the product architecture.

Supporting multiple harnesses would introduce incompatible prompt,
cancellation, approval, completion, session, and recovery semantics before the
OpenCode-specific job is validated.

## Recommended Product Boundary

Build only:

- one trusted owner;
- one always-on private Linux host;
- a small configured repository list;
- one fresh full clone per attempt;
- one OpenCode server and state volume per attempt;
- two or three concurrent attempts;
- exact task-to-session routing into the native OpenCode UI;
- conservative `queued`, `working`, `needs you`, `uncertain`, and `result ready`
  states;
- explicit stop and result sealing;
- a retained Git bundle and manifest;
- optional host verification;
- optional GitHub App draft-PR publication;
- bounded retention and cleanup.

Do not build:

- a replacement conversation UI;
- generic ACP or agent support;
- Kubernetes;
- remote runner pools;
- organizations or RBAC;
- public multi-tenancy;
- native mobile applications;
- schedules or workflow automation;
- previews;
- a model Gateway;
- automatic generic completion;
- live Session Teleport.

## Two-Weekend Experiment

### Weekend 1: prove the difference

1. Run OpenHands Agent Canvas `v1.16.0` with one supported ACP preset.
2. Try stable OpenCode through custom ACP and separately determine what V2 would
   require.
3. Compare Canvas with the native OpenCode UI for conversation, permissions,
   files, terminal, diffs, steering, restart, and phone reconnect.
4. Characterize a current source-attributed OpenCode V2 candidate against
   Fern's existing contract suite.
5. Stop if the OpenHands experience is already satisfactory.

### Weekend 2: disposable native proof

1. Start two isolated OpenCode containers against fresh clones from the same
   exact base commit.
2. Route each through a stable authenticated task URL.
3. Open each task in the actual OpenCode UI from phone and desktop.
4. Kill one runtime and report uncertainty rather than success.
5. Explicitly stop and seal both repositories.
6. Export Git bundles, delete both checkouts, and reconstruct each result in a
   clean clone.
7. Measure setup, reconnect, native-UI open, memory, disk, and recovery time.

The prototype does not need a production scheduler, notification system,
Kubernetes, or automatic terminal classification.

## Acceptance Criteria

Continue only if four weeks of dogfood show:

- at least 10 real tasks submitted;
- at least 5 useful retained results without laptop-side repair;
- native OpenCode attachment used at least weekly;
- two same-repository attempts complete without checkout interference;
- every selected result reconstructs after task-environment deletion;
- no duplicate prompt or Fern-owned publication under injected response loss;
- setup and recovery time are lower than the time delegation saves;
- the owner prefers the workflow to OpenHands ACP and one managed cloud agent.

## Kill Criteria

Stop expansion if any of these hold:

- OpenHands custom ACP is equally useful;
- native OpenCode attachment is rarely used;
- most tasks require interactive repair;
- manual result sealing makes the workflow unattractive;
- OpenCode V2 cannot provide a bounded restart and cancellation contract without
  a fork;
- retained Git artifacts do not change review or recovery behavior;
- one persistent workspace remains preferable;
- the first compelling demo requires Kubernetes, Gateway, generic agents, or a
  new frontend.

## Fallback Direction

If the OpenCode product test fails, Fern should remain a personal and portfolio
project. The only component worth testing independently is the Safe Git
Finalizer:

> Agents propose changes; a host-owned finalizer verifies and publishes one
> exact Git result without giving the agent GitHub write authority.

That component should be extracted only after an OpenHands, T3 Code, platform,
or security user demonstrates a repeated need. Technical uniqueness alone is
not enough.

## Final Position

Fern has earned the right to run one narrow experiment, not the right to build a
platform.

The useful experiment is not "self-hosted agents." It is whether the real
OpenCode experience, combined with failure-honest task state and retained exact
results, is materially better than OpenHands ACP and hosted background agents
for repeated personal work.

If the answer is yes, Fern should become the smallest reliable background mode
for OpenCode. If the answer is no, its correctness work remains valuable without
turning into another remote-agent dashboard.
