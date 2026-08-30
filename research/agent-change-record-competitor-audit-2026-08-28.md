# Agent Change Record Competitor Source Audit

Research date: 2026-08-28

Status: point-in-time source audit. `NL` means not located after targeted source
search, not proof that no implementation exists.

## Verdict

None of the seven audited systems implements the complete repository-change
evidence chain:

```text
admitted authority
  -> exact base/result/tree
  -> verification of that same result
  -> write-ahead publication
  -> ambiguity reconciliation
  -> signed portable export
  -> offline policy verification
```

Orbit is the closest transaction competitor. It has exact Git/test checkpoints,
durable write-ahead merge operations, exact-tip gates, at-least-once delivery,
race-safe push behavior, and durable merge receipts. No concrete PR creation,
GitHub Check/required-merge integration, or portable signed offline-verifiable
result export was located.

`agentdiff` materially narrows the positioning because signed Git-native
attribution, offline signature verification, and CI policy checks already exist.
Fern should describe a **host-attested change transaction**, not generic AI
authorship provenance.

## Classification

- `Y`: concrete production wiring satisfies the criterion.
- `N`: source explicitly contradicts or excludes the criterion.
- `A`: an adjacent or generic primitive exists, but exact-change binding is
  unproven.
- `NL`: not located after targeted source search.

| Capability | Warren | Orbit | Deputies | AgentRouter | agentserver | Codex Router | agentdiff |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Durable idempotency receipt | N | Y | A | Y | A | Y | N |
| Durable pre-effect intent | A | Y | NL | Y | A | A | A |
| Immutable base/result/tree Git binding | A | Y | NL | NL | NL | A | A |
| Verification of exact resulting state | NL | Y | NL | NL | NL | N | N |
| Actor/approval authority binding | A | Y | A | Y | Y | A | A |
| Write-ahead Git publication | N | Y | NL | NL | NL | NL | N |
| Publication/effect reconciliation | A | Y | A | NL | A | A | NL |
| Explicit uncertain effect state | A | Y | NL | NL | Y | Y | N |
| Signed portable result export | NL | NL | NL | NL | A | NL | Y |
| Offline result verifier | NL | NL | NL | NL | A | NL | Y |
| GitHub Check or required-merge wiring | A | NL | NL | NL | NL | NL | A |

## Audit Pins

- [Warren `fe10715`](https://github.com/jayminwest/warren/tree/fe1071562ac957aacba39beba850ef00e10d879a)
- [Orbit `aca9757`](https://github.com/jianghailong-xy/orbit/tree/aca97577e5e57cb2d6d39e503a035115e1cedd9c)
- [Deputies `60c7e18`](https://github.com/sidpalas/deputies/tree/60c7e186187839a52d56c23d57cf9e22fe9cd5b4)
- [AgentRouter `8c7e339`](https://github.com/perixtar/AgentRouter/tree/8c7e339f36593d4daf03003a7ca24f7e380e8ed6)
- [agentserver `3411a15`](https://github.com/agentserver/agentserver/tree/3411a155375dfe8a1843b7f702ae8f5eaed3438a)
- [Codex Router `56356ec`](https://github.com/rixzkiye/codex-router/tree/56356ec55e36d3360e6e13ea75634e5124b28d78)
- [`agentdiff` `f9ffbd2`](https://github.com/codeprakhar25/agentdiff/tree/f9ffbd2b742826b27de7584e104da455d1635f64)

## Warren

`POST /runs` deduplication is explicitly process-local, TTL-bound, reopened by
restart, and excludes durable cross-restart dedupe:
[source](https://github.com/jayminwest/warren/blob/fe1071562ac957aacba39beba850ef00e10d879a/src/server/idempotency.ts#L1-L23).

Warren persists a full base commit pin
([source](https://github.com/jayminwest/warren/blob/fe1071562ac957aacba39beba850ef00e10d879a/src/runs/base-commit.ts#L1-L67)),
refuses default-branch direct pushes
([source](https://github.com/jayminwest/warren/blob/fe1071562ac957aacba39beba850ef00e10d879a/src/runs/target-branch.ts#L62-L100)),
and finds or creates a PR idempotently by head/base
([source](https://github.com/jayminwest/warren/blob/fe1071562ac957aacba39beba850ef00e10d879a/src/runs/reap/pr-open.ts#L129-L163)).

Finalize intent is in memory and reconstructed after restart
([source](https://github.com/jayminwest/warren/blob/fe1071562ac957aacba39beba850ef00e10d879a/src/runs/finalize-recovery.ts#L1-L23)).
Checks are read but not created
([source](https://github.com/jayminwest/warren/blob/fe1071562ac957aacba39beba850ef00e10d879a/src/forge/github/provider.ts#L297-L313)).

## Orbit

Checkpoints bind branch, commit, tree, base, test counts, and the exact tested
tree:
[source](https://github.com/jianghailong-xy/orbit/blob/aca97577e5e57cb2d6d39e503a035115e1cedd9c/src/apiserver/src/projects/task-checkpoint.ts#L55-L115).
Recording rejects evidence measured against another tree and derives accepted or
red status instead of trusting the caller:
[source](https://github.com/jianghailong-xy/orbit/blob/aca97577e5e57cb2d6d39e503a035115e1cedd9c/src/apiserver/src/projects/task-checkpoint.ts#L166-L240).

Merge requests persist `pending`, an operation ID, and the authorized checkpoint
before runner delivery:
[source](https://github.com/jianghailong-xy/orbit/blob/aca97577e5e57cb2d6d39e503a035115e1cedd9c/src/apiserver/src/sessions/sessions.service.ts#L3900-L3980).
Delivery is at least once until a result changes durable state:
[source](https://github.com/jianghailong-xy/orbit/blob/aca97577e5e57cb2d6d39e503a035115e1cedd9c/src/apiserver/src/realtime/realtime.service.ts#L812-L889).

Push races are retried, while push success followed by local fast-forward
failure becomes a partial outcome:
[source](https://github.com/jianghailong-xy/orbit/blob/aca97577e5e57cb2d6d39e503a035115e1cedd9c/src/runner-go/worktree.go#L1228-L1349).
Receipts are durable, exact-SHA-bound, and replayed by deterministic identity:
[source](https://github.com/jianghailong-xy/orbit/blob/aca97577e5e57cb2d6d39e503a035115e1cedd9c/src/apiserver/src/sessions/merge-receipt.service.ts#L45-L78),
[source](https://github.com/jianghailong-xy/orbit/blob/aca97577e5e57cb2d6d39e503a035115e1cedd9c/src/apiserver/src/sessions/merge-receipt.service.ts#L121-L180).

## Deputies

PostgreSQL deduplicates inbound integration deliveries by `(source, dedupe_key)`:
[source](https://github.com/sidpalas/deputies/blob/60c7e186187839a52d56c23d57cf9e22fe9cd5b4/apps/control-plane/src/store/postgres.ts#L4867-L4922).
This is ingress delivery dedupe, not a mutating-command receipt bound to Git.

The authenticated Git tool permits generic agent-invoked pushes
([source](https://github.com/sidpalas/deputies/blob/60c7e186187839a52d56c23d57cf9e22fe9cd5b4/apps/control-plane/src/repositories/git-tool.ts#L27-L88)),
and the `gh` tool posts PR creation before recording an external resource
([source](https://github.com/sidpalas/deputies/blob/60c7e186187839a52d56c23d57cf9e22fe9cd5b4/apps/control-plane/src/repositories/github-cli-tool.ts#L199-L252)).

## AgentRouter

PostgreSQL stores TTL idempotency claims and resulting run IDs transactionally:
[source](https://github.com/perixtar/AgentRouter/blob/8c7e339f36593d4daf03003a7ca24f7e380e8ed6/apps/api/src/server.ts#L202-L284).
Actions and arguments receive canonical SHA-256 bindings:
[source](https://github.com/perixtar/AgentRouter/blob/8c7e339f36593d4daf03003a7ca24f7e380e8ed6/packages/core/src/index.ts#L53-L83).
Proposal, policy, approval, and execution events are ordered, and approval must
match the action digest:
[source](https://github.com/perixtar/AgentRouter/blob/8c7e339f36593d4daf03003a7ca24f7e380e8ed6/packages/worker/src/index.ts#L965-L1067).

Repository output is collected as `git diff --binary HEAD` plus workspace status,
not as an exact commit/tree seal:
[source](https://github.com/perixtar/AgentRouter/blob/8c7e339f36593d4daf03003a7ca24f7e380e8ed6/packages/worker/src/index.ts#L1423-L1525).

## agentserver

Run authority is an RFC 8785 canonical Ed25519-signed immutable manifest:
[source](https://github.com/agentserver/agentserver/blob/3411a155375dfe8a1843b7f702ae8f5eaed3438a/v2/internal/runmanifest/manifest.go#L315-L438).
Approval events bind attempt scope, execution, approver, expiry, and a context
digest:
[source](https://github.com/agentserver/agentserver/blob/3411a155375dfe8a1843b7f702ae8f5eaed3438a/v2/internal/runevent/payload.go#L395-L455).

Checkpoint finalization begins before immutable object publication, verifies
exact returned pointers, and commits afterward:
[source](https://github.com/agentserver/agentserver/blob/3411a155375dfe8a1843b7f702ae8f5eaed3438a/v2/internal/harnesspool/checkpoint_finalizer.go#L117-L143),
[source](https://github.com/agentserver/agentserver/blob/3411a155375dfe8a1843b7f702ae8f5eaed3438a/v2/internal/harnesspool/checkpoint_finalizer.go#L226-L270).
Ambiguous object or checkpoint commits retain original runtime bytes for recovery:
[source](https://github.com/agentserver/agentserver/blob/3411a155375dfe8a1843b7f702ae8f5eaed3438a/v2/internal/harnesspool/checkpoint_finalizer.go#L291-L333).

These records attest runtime/checkpoint state, not repository commit/tree
publication. No v2 Git push, PR, or Check chain was located.

## Codex Router

SQLite persists idempotency claims, request hashes, operation state, results, and
expiration:
[source](https://github.com/rixzkiye/codex-router/blob/56356ec55e36d3360e6e13ea75634e5124b28d78/src/store/database.ts#L96-L118),
[source](https://github.com/rixzkiye/codex-router/blob/56356ec55e36d3360e6e13ea75634e5124b28d78/src/store/registry.ts#L216-L279).
Startup reconciles runtime state and moves unrecoverable missing-thread cases to
`needs_attention`:
[source](https://github.com/rixzkiye/codex-router/blob/56356ec55e36d3360e6e13ea75634e5124b28d78/src/router.ts#L625-L693).

Result distillation records current head/base and command-shaped test
observations, but does not bind tests to that Git state and labels missing
observed evidence unverified:
[source](https://github.com/rixzkiye/codex-router/blob/56356ec55e36d3360e6e13ea75634e5124b28d78/src/result.ts#L11-L53).
Push/merge authority is inferred from approval text using regular expressions:
[source](https://github.com/rixzkiye/codex-router/blob/56356ec55e36d3360e6e13ea75634e5124b28d78/src/router.ts#L1054-L1073).

## agentdiff

The trace format contains an informational VCS revision, line ranges/content
hashes, contributor data, and optional Ed25519 signature. The Git SHA is
explicitly not record identity:
[source](https://github.com/codeprakhar25/agentdiff/blob/f9ffbd2b742826b27de7584e104da455d1635f64/src/data.rs#L5-L41),
[source](https://github.com/codeprakhar25/agentdiff/blob/f9ffbd2b742826b27de7584e104da455d1635f64/src/data.rs#L84-L95).

The CLI verifies signatures using local, Git-registry, or archived public keys
and fails on invalid signatures:
[source](https://github.com/codeprakhar25/agentdiff/blob/f9ffbd2b742826b27de7584e104da455d1635f64/src/commands/verify.rs#L11-L175).
It installs a pull-request policy workflow but does not configure that workflow
as a required branch-protection check:
[source](https://github.com/codeprakhar25/agentdiff/blob/f9ffbd2b742826b27de7584e104da455d1635f64/src/commands/install_ci.rs#L56-L98).
Its documentation scopes the product as source-line authorship rather than
execution quality or supply-chain transaction proof:
[source](https://github.com/codeprakhar25/agentdiff/blob/f9ffbd2b742826b27de7584e104da455d1635f64/README.md#L38-L50).

## Fern Baseline

Fern's current implementation already covers the integrated transaction core:

- exact result commit and tree sealing
  ([source](https://github.com/nebler/fern/blob/ab945b5a00db3a310b3fcc30fe8bc99669598b6f/internal/taskresultcoord/coordinator.go#L305-L318));
- verification prepared and revalidated against that sealed result
  ([source](https://github.com/nebler/fern/blob/ab945b5a00db3a310b3fcc30fe8bc99669598b6f/internal/taskverification/coordinator.go#L181-L247));
- `push_started` and `pr_create_started` committed before effects, followed by
  exact read reconciliation
  ([source](https://github.com/nebler/fern/blob/ab945b5a00db3a310b3fcc30fe8bc99669598b6f/internal/taskpublicationcoord/coordinator.go#L146-L243));
- user-authorized seal recollection against the exact expected tuple
  ([source](https://github.com/nebler/fern/blob/ab945b5a00db3a310b3fcc30fe8bc99669598b6f/internal/taskresultcoord/coordinator.go#L323-L399)).

Fern does not have the portable-product layer: signed self-contained export,
offline verifier, or GitHub Check projection.

## Strategic Conclusion

The integrated transaction contract remains unusual and plausible technical
whitespace. Attribution, signatures, approvals, transaction journals, offline
verification, and CI policy are individually occupied. The product hypothesis
passes only if their composition changes a real merge, rollout, compliance, or
incident decision.
