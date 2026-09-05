# Go naming, maintainability, and local performance review

## Scope and evidence

This review covers the **28 packages in the root Go module** enumerated in the
[package guide](go-packages.md). It follows the removal of host verification,
host publication, historical runtime modes, and duplicate configuration shapes.
Each package README records its own entrypoints, naming observations, performance
costs, and dependency/call diagrams.

This is a source review plus targeted local measurements, not proof that every
failure path is tested or every function name is optimal. The Mermaid graphs
show representative paths; indirect calls through interfaces and callbacks are
identified in prose. Measurements and reproduction details are in
[performance.md](performance.md), separate from static cost observations.

## Naming criteria

Names were evaluated for whether they tell a caller:

1. The domain operation, rather than a numerical/mechanical implementation detail.
2. Whether it observes, validates, persists, mutates an external system, or owns a
   resource that must be closed.
3. What evidence it establishes—and what it cannot establish from local inputs.
4. Whether repeated invocation is safe, reconciles existing state, or can repeat
   an external effect.

Short local helpers do not need elaborate domain names. Conversely, `New`,
`Ensure`, `Verify`, and `Acquire` need explicit contracts when construction cannot
prove external state or a call may mutate resources.

## Changes made during the review/handoff

- Removed host publication APIs and storage, rather than preserving empty
  compatibility adapters or dummy successful outcomes.
- Removed retired result/publication ingress and a `csrfRoute` helper that only
  normalized an obsolete task-cancellation route. Current CSRF input uses the
  exact request path.
- Removed the obsolete `scripts/test-github-publication.sh` stub, which instructed
  operators to use publication APIs and commands that no longer exist.
- Corrected the gh wrapper's auth-subcommand detection so a normal PR title
  containing `auth` is not mistaken for a login command.
- Clarified `RefreshGitHubCredentials`: refresh errors prevent new prompt
  admission and trigger retry, but do not magically stop an already-running
  agent from using a previously delivered token until it expires.
- Kept artifact verification distinct from repository testing and kept runtime
  identity construction distinct from fresh inactivity proof.
- Renamed `pathpkgEscapesWorkspace` to `relativePathEscapesWorkspace` and
  `manifestsEqual` to `changesEqual`, without changing their rules.
- Made `RecordBackgroundRunBundleVerified` the sole bundle-proof recording
  entrypoint, removing the misleading `VerifyBackgroundRunBundle` name and its
  forwarding alias. The store records supplied evidence; the artifact engine
  verifies bytes.
- Removed unused legacy-only task/attempt readers and the result resolver's
  private forwarding helper with a discarded return value.
- Corrected the coordinator's one-external-operation claim: a scan owns one run
  phase, which may require several external calls. Corrected resource-spec,
  Unix-nanosecond, ID-generation, token-delivery, and Git ref-limit comments.
- Removed orphaned GitHub PR/ref-mutation client APIs after confirming their
  host-publisher consumers no longer exist; retained shared onboarding helpers.

## Remaining findings, not silently rewritten

| Area | Finding | Recommendation |
| --- | --- | --- |
| `credentialbundle` | Existence-check then rename is not a race-safe no-clobber primitive; `Lstat` followed by `Open` is not a descriptor-based path-replacement fence. | Treat host/private-directory trust as a prerequisite. If concurrent hostile writers enter the threat model, add descriptor-based tests and hardening rather than relying on names. |
| `credentialbundle` | Plaintext serialization uses buffers; strict decoding does not itself reject duplicate JSON keys. | Do not describe this as streaming or universally canonical parsing. Consider bounded duplicate-key rejection in a separate behavior change. |
| `config` | YAML file reading is unbounded and task parsing repeats marshaling/decoding; `decodeRequiredTaskString` also serves other config sections. | Document the startup-only scope. A file-size bound and a more general helper name are reasonable targeted follow-ups, not reasons for another config layer. |
| `control` / `pluginauth` | Read-sounding authentication/list operations can update/prune persisted state; full-file serialization and fsync hold locks. | Keep those effects explicit in docs. Benchmark write/expiry paths separately from cached reads before changing locking. |
| `githubapp` | `Client` means token minting, while other clients perform discovery. `AppCredentials.PrivateKey` exposes an RSA pointer. | A future focused `TokenClient` rename is clearer; do not claim deep immutability for returned pointers. |
| `taskstore` | Wide row scans serve small list responses; some export/recovery forwarding names remain. | Measure realistic list sizes first. Remove unused aliases when confirmed rather than build mapping objects for every column. |
| `taskenvdocker` | `ProveWriterInactive` may stop the runtime, not just observe it. | Callers must read the mutation contract; a future focused name should reveal stop-and-prove behavior. Do not remove the fresh fence. |
| `backgroundopencode` | Exported `ReadSession` returns private `sessionInfo`. | Narrow it to a private helper if no external use emerges; avoid inventing a public wire model solely to justify export. |
| `runapi` / `runclientapi` | `writeStoreError` maps command errors too; `attachmentReady` establishes durable eligibility, not current live availability. | Prefer intent-specific names at the next touched callsite; keep live route validation separate. |
| `jsoncanon` / `integration/upgrade` | Names suggest canonical serialization and historical migration, respectively, but neither is their current job. | Their READMEs state the actual validation/current-schema scope. Package moves alone would add churn, not improve guarantees. |

These are source-level findings. They are not all exploitable bugs, and no
benchmark result is evidence that the corresponding security contract can be
weakened. The review intentionally does not claim an exhaustive security audit.

## Architectural assessment

The current product is narrower: Fern provides the run, live steering, exact
retention, and repository-scoped credentials; the agent owns tests, pushes, and
PRs. There is no second host publication/test workflow.

The remaining major complexity is concentrated in useful places:

- `taskstore`: transactionally preserving intent, claims, immutable bindings,
  and recovery state across process failure.
- `taskenvdocker`: dealing with actual Docker/Git resources and private token
  installation while refusing replaced identities.
- `backgroundruncoord`: deciding which specific effect is permitted next.
- `backgroundroute` and `proxy`: separate live-session and control-plane trust
  boundaries, not interchangeable reverse proxies.

These packages should not grow generic workflow, credential-provider, or policy
engines without a concrete need. A large storage record is not by itself a reason
to add a constructor/getter/conversion layer for every field.

## Performance interpretation

Local parser, validation, cached-authentication, and SQLite microbenchmarks do
not measure complete agent runs. Git object verification, Docker inspection,
network latency, and model execution are different cost classes.

In particular:

- CAS acquisition intentionally performs fresh Git integrity verification. A
  TTL cache would change the trust contract, not merely optimize it.
- Credential delivery caches expiration and exact-runtime identity, not a token
  string; each scan still checks the current Docker identity. The mint/write
  path is infrequent, while Docker observations remain external I/O.
- SQLite writes include transactional checks. Faster in-memory fake-store
  admission is not evidence of durable write throughput.
- SSE limits bound complete events, but JSON projection can allocate several
  representations within that bound. Profile representative payloads before
  attempting a streaming parser rewrite.
- Capacity one removes the need for worker-pool throughput tuning; it does not
  remove latency or recovery obligations.

## Limits of this pass

- No live GitHub repository was mutated as a test. GitHub token responses in
  credential handoff tests were synthetic; actual container helpers, file
  installation, rotation, and runtime fencing were exercised with Docker.
- No sustained production traffic, large-repository scaling campaign, remote
  model performance test, or deployment-host CPU/I/O profile was performed.
- Passing race tests does not prove absence of every scheduling bug. Direct
  coordinator coverage remains partial; the live harness complements it.
- Host-only App keys do not make agent code untrusted-safe. The agent can read
  its scoped token and misuse it until expiry. Helpers reduce accidental
  persistence; they are not a sandbox against the credential's recipient.

The package READMEs are the detailed review record. Prefer small fixes supported
by an observed maintenance problem or benchmark over another broad restructure.
