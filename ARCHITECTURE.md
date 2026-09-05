# Fern Architecture

## 1. Purpose

Fern is a single-host control plane for disposable Background Runs. A run gets
an isolated Git clone, Docker volume, OpenCode container, authenticated
loopback endpoint, session, and prompt. The run ends by stopping the exact
writer, retaining a reproducible Git result, and deleting all disposable
compute.

The durable product is the control-plane record and retained result, not a
workspace container.

## 2. Product Boundary

Fern owns:

- repository and GitHub App authority;
- actor identity, pairing, plugin grants, and revocation;
- idempotent task, stop, and seal admission;
- exact disposable resource identity and lifecycle;
- run claims, revisions, cancellation epochs, and evidence;
- short-lived exact-session attachment capabilities and connection shutdown;
- writer inactivity proof;
- Git bundle export and local content-addressed storage;
- short-lived, repository-scoped GitHub credentials for the runtime;
- backup, restore, credential rotation, and schema compatibility.

OpenCode owns:

- model and provider execution;
- the session transcript;
- prompts, tools, permissions, questions, and UI;
- file edits made while its disposable writer is active;
- repository checks, Git pushes, and pull-request creation requested by its goal.

Fern does not own a persistent OpenCode home, a persistent repository mount, a
wake-on-request proxy, an idle supervisor, or a browser terminal. Operator
interaction with a live run uses the normal OpenCode TUI.

## 3. Process Topology

```text
                              Fern host

 OpenCode plugin       private TLS edge        local operator
       |                      |                      |
       +---- HTTPS :443 ------+                      |
                              v                      v
                     remote 127.0.0.1:8080   operator 127.0.0.1:8081
                              |                      |
                              +----------+-----------+
                                         v
                      pairing / plugin auth / run APIs
                                         |
                       +-----------------+------------------+
                       |                 |                  |
                   taskstore 3       run coordinator   GitHub App credentials
                       |                 |
                       |                 v
                       |         taskenvdocker provider
                       |                 |
                       |   one exact writer container
                       |                 |
                       |          live loopback port
                       |                 |
                       +------ backgroundroute :8443
                                         |
                                  private TLS :8443

 agent + git/gh -> GitHub branch / draft PR
 stopped writer -> retained artifact CAS (independent of publication)
```

The three listeners are bound before Docker side effects. The remote listener
is the paired-device and plugin surface. The operator listener is loopback-only
and protected by the Fern control password. The Background Run listener has no
default target: `backgroundroute.Manager` binds one exact live runtime and
removes it and waits for admitted forwarding to exit before writer teardown.

## 4. Startup Composition

`fern up` performs this sequence:

1. Strictly load YAML and protected environment values.
2. Apply `config.ValidateBootstrap`.
3. Bind remote, operator, and live-run listeners.
4. Acquire the host-local repository-name lease.
5. Open control and plugin-authorization state.
6. Open taskstore schema 3.
7. If the installation ID is pending, block readiness and expose onboarding
   without composing task services.
8. Otherwise apply strict `config.Validate` and resolve exact GitHub
   App repository authority.
9. Qualify the exact Background Run image through Docker inspection.
10. Open the artifact CAS and inspect every referenced artifact.
11. Build run, route, and HTTP services with the scoped token source.
12. Start all services under one cancellation errgroup.

Fresh-host configuration may omit `workspace.github.installationId` only to
bootstrap onboarding. Fern then exposes onboarding, blocks readiness, and
returns `503` for run operations. After creating and installing the
App on the configured repository, the operator records the numeric installation
ID from GitHub's installation URL and restarts Fern. Missing credentials follow
the same blocked, onboarding-only path. Neither state can compose task services.

## 5. Configuration Authority

Production requires:

- `workspace.name` and an absolute repository path;
- `workspace.github.mode: github-app-broker`;
- exact installation ID, repository ID, and canonical full name;
- explicit agent, model provider, model ID, and timeouts;
- exact Background Run image reference and canonical image ID;
- a loopback live-route listener and private HTTPS live origin;
- remote and operator loopback listeners;
- a control password of at least 32 characters.

Retired persistent-workspace settings (`workspace.image`, `workspace.memory`,
`workspace.env`, `idle`, and `workspace-gh`) are not supported. Arbitrary
`tasks.backgroundEnvironment`, `tasks.budget`, and `tasks.verification` settings
are rejected rather than accepted as unused configuration. `OPENCODE_PASSWORD`
is not forwarded; each disposable runtime receives a Fern-derived credential.

## 6. Authentication And Actors

Fern has three ingress actor classes:

- operator: loopback Basic authentication with the host-only control password;
- paired device: restart-safe secure cookie whose digest is stored by Fern;
- OpenCode plugin: device authorization followed by a fixed-scope bearer.

The plugin scopes are `run:create`, `run:read`, `run:stop`, `run:attach`, and
`run:result`. They are not configuration. Publication admission is reserved for
paired/operator actors and is not a plugin bearer scope.

Ingress installs a validated `task.ActorSnapshot` in request context. Inner API
packages do not derive identity from client-controlled headers or bodies.
Receipts bind actor, command kind, workspace, idempotency key, and canonical
request hash.

## 7. Admission

Run creation accepts only a clean, born Git repository with an unambiguous
canonical remote and exact `HEAD`. The plugin rechecks the repository after
human confirmation. Fern independently verifies the submitted base against its
bound host repository.

Admission atomically writes:

- task and attempt identities;
- actor snapshot;
- repository and base authority;
- instruction hash;
- image, profile, environment, and resource identities;
- OpenCode session and message identities;
- receipt and event records;
- queued Background Run intent.

The coordinator wakes only after commit. A repeated matching idempotency claim
returns the original receipt. A changed hash conflicts. Another actor cannot
probe the original claim.

`runcommand` owns create, stop, and seal application operations: request policy,
canonical idempotency hashing, replay interpretation, base verification, identity
generation, admission coordination, and notification after fresh commits. Its
inputs are separate from HTTP DTOs and its outputs contain accepted-command
facts rather than raw store receipts. The HTTP handler owns routing,
authentication/scopes, bounded decoding, error mapping, and response encoding.

## 8. Run State

The public run state is:

```text
queued -> setting_up -> working <-> needs_you
   |          |             |
   +----------+-------------+-> canceling
                              -> uncertain
                              -> result_ready
                              -> failed
                              -> cleanup_required
```

The durable effect phase is more precise:

```text
absent
 -> provision_intent
 -> clone_observed
 -> volume_observed
 -> container_observed
 -> health_observed
 -> ready
 -> session_observed
 -> prompt_intent
 -> prompt_admitted
 -> seal_intent or stop_intent
 -> writer_inactive
 -> exporting                  (seal only)
 -> artifact_committed         (seal only)
 -> route_removed
 -> container_removed
 -> volume_removed
 -> clone_removed
 -> cleanup_complete
```

External mutations happen only after the corresponding durable intent or
started phase. Reconciliation reads are bounded. An ambiguous mutation is not
blindly retried. Claims bind workspace, task, attempt, generation, revision,
state, phase, cancellation epoch, profile, image, owner, and lease expiry.

`run` owns lifecycle classification independently of persistence: valid
state/phase combinations, attempt-deadline applicability, execution-selection
requirements, and timeout eligibility. The coordinator dispatches concrete
effects; it does not maintain a second list of phase categories. Taskstore keeps
SQL representation and compatibility translation private and continues to
enforce durable transitions transactionally. The existing storage-shaped run
record remains a boundary representation; moving all of its fields into domain
values is not yet complete.

## 9. Disposable Resource Identity

`taskenvdocker.Provider` owns Docker policy. Every clone, volume, container,
endpoint, and runtime gets a deterministic Fern identity derived from immutable
run state and a private host key. Container inspection must match:

- exact container ID and start timestamp;
- runtime epoch and token;
- qualified image ID;
- labels, mounts, resource limits, environment digest, user, and network mode;
- loopback published port;
- repository and run identities.

Replacement or unowned resources are quarantined or rejected. The provider
does not trust names alone.

`run.Resources` owns canonical resource-name derivation and matching;
`run.Runtime` owns exact timestamp and token interpretation. Both have private
representations. These identity values are not inactivity proof: destructive
provider operations still re-observe the exact resource before acting.

Resource-spec version is 10. The image must carry `ai.fern.runtime.spec="10"`
and the rotating GitHub credential helpers. The lane is intentionally serial, with
capacity one. The qualified source and observed-clone envelope is 128 MiB, and
clone work has the same 30-second deadline as its Git operation.

## 10. Live Route

`backgroundroute.Manager` maps short-lived opaque capabilities to one exact
authenticated runtime and OpenCode session. The route is activated only after
container health, endpoint identity, and OpenCode session identity are
committed. The run API can mint a random two-hour capability only while the
complete workspace, task, attempt, run generation, single-writer generation,
container ID, start time, runtime epoch, and session tuple remains active. Fern retains only the
capability digest in process memory.

The attached OpenCode TUI may read the dedicated server and interact with the
exact session. The route rejects cross-session mutation, session creation and
deletion, workspace and credential management, Fern control paths, and HTTP
upgrades. The capability is supplied as OpenCode Basic authentication, never as
a command argument. The underlying provider transport replaces it with the
runtime's derived server credential.

Removal is a fence:

1. Remove target admission.
2. Revoke every attachment capability and cancel admitted HTTP/SSE traffic.
3. Wait for every admitted request-forwarding goroutine to exit.
4. Return route-removal evidence.

No persistent OpenCode path is forwarded by the remote or operator gateway.

The attachment event stream owns its upstream body and filtering worker.
Closing the returned stream closes upstream and waits for worker exit. The
projection limit applies to each complete normalized SSE event, not merely to
individual lines, so an unterminated multiline event cannot grow without bound.

Attachment contract tests explicitly pin the qualified upstream commit. An
upgrade must review route/method/query rejection, response envelopes, and SSE
session filtering. The live serial harness also checks the actual session-list
and active-session envelopes and denies unknown management writes and foreign
session operations. The allow-list is intentionally manual, not generated from
all endpoints exposed by upstream.

## 11. Observation And Stop

The coordinator observes bounded OpenCode session, question, and permission
surfaces plus Docker usage. Positive activity yields `working`; pending human
input yields `needs_you`. Missing or contradictory ownership evidence yields
`uncertain`, not success.

Stop, timeout, and seal all converge on the exact writer fence. Fern stops the
committed container process epoch and then proves that it is non-running. A
replacement container or changed identity invalidates the proof. Cleanup and
artifact export require the same positive inactivity evidence.

## 12. Seal And Retention

Seal is explicit and irreversible. Its receipt commits the seal request,
artifact export ID, materialization ID, retained artifact ID, and result ID
before teardown.

After writer inactivity:

1. Acquire the stopped source clone under its identity lock.
2. Capture committed, staged, unstaged, and untracked changes without mutating
   the source.
3. Build and verify a `git_bundle_v1` object and canonical manifest.
4. Install it under `artifact-cas/sha256:<manifest digest>`.
5. Materialize a detached checkout and prove its base, result commit, and tree.
6. Commit the complete retained-artifact/result tuple in one transaction.
7. Delete route, container, volume, and clone.

The result remains available only when retention is verified and
reconstructable. Every positive plugin API projection comes from a fresh CAS
inspection and complete result/artifact/snapshot tuple check. Artifact locators
and host paths are not returned through the plugin API.

Result consumption uses `taskartifact.Engine.Acquire` to perform one fresh full
verification and return its snapshot together with an owned detached checkout.
The result-source resolver checks that snapshot against the durable result and
artifact binding, closing the checkout on mismatch. Materialization still
checks the copied bundle against the verified digest and size; acquisition does
not cache integrity observations across operations.

## 13. Repository Checks

Fern does not run repository test commands on the host or require a successful
test record before publication. The agent's task or goal may include running
checks in its runtime, and repository CI can assess the published draft PR.
Agent-reported success is not independent verification and is not a publication
credential. Fern's responsibility is exact result retention and safe delivery,
not a second CI system.

Artifact verification remains mandatory: checking bundle bytes, Git objects,
and result identity protects the work being delivered. It does not claim that
the changes pass repository tests. There is no `tasks.verification` policy or
`tasks.budget.maxTurns` configuration; attempt timeouts remain enforced.

## 14. Harness-Owned GitHub Delivery

The runtime image installs Git and GitHub CLI. The agent can run checks, push a
branch, and open a draft PR as part of its task. Fern has no publication API,
publication journal, or host-side Git push/PR coordinator. The harness owns
remote mutation and reconciliation after ambiguous responses.

Fern retains the GitHub App private key on the host and mints short-lived
installation tokens restricted to the bound repository. Before admitting the
prompt and during active execution, it refreshes runtime credentials in the
private disposable OpenCode volume, outside the clone and retained artifact.
Tokens are not written into Docker environment/configuration, taskstore,
labels, command arguments, or logs. Git and gh read the current credential via
image-installed helpers; no personal gh configuration is mounted.

This grants the agent GitHub write authority for the bound repository. Fern
does not treat the agent as unable to read or copy its own token; a malicious
task can exfiltrate that authority until expiry. The helpers prevent accidental
credential persistence, not deliberate misuse by code running as the agent.
Fern
does not guarantee exactly-once PR creation, test success, or equality between
a PR commit and the later sealed artifact. The agent may edit files after a
push; retention preserves that later work independently. Credential refresh
does not authorize execution after cancellation and cannot block teardown.

## 15. Terminal-Native Attachment

`fern runs` queries `/fern/api/v1/runs` through either loopback operator Basic
authentication or the existing remote plugin bearer. It shows running runs by
default and has a stable `--json` projection for automation. Bare `fern attach`
selects the only attachable run or presents an interactive picker; an exact task
ID bypasses selection.

`GET /fern/api/v1/runs/:id/attach` is an operator/client control operation, not a
plugin UI operation and not a browser deep link. It re-reads run ownership and
route readiness, then asks `backgroundroute.Manager` for an in-memory capability
bound to the active runtime and session. `fern attach` starts
`opencode attach <origin> --session <session-id> --pure` with the capability in the
OpenCode authentication environment. The existing OpenCode process remains the
only writer and retains its volume, transcript, tools, permissions, and file
ownership. Attachment adds a client; it does not replace the agent or transfer
the clone to another container.

## 16. Recovery

Fern reconstructs work from taskstore phases after restart. It never treats
process-local memory as authority. Recovery rules include:

- retry an external mutation only when evidence proves it was not attempted;
- reconcile started phases with read-only observations;
- retain `uncertain` when exact outcome cannot be proven;
- stop only the exact committed writer epoch;
- reject result consumption when any artifact tuple field differs;
- preserve cleanup-required state until absence is proven;
- wake coordinators only after durable admission commits.

Taskstore schema is 3 and control-state schema is 2. This pre-release reset has
no supported predecessor: older development state is rejected, never silently
migrated or deleted. Preserve anything needed before explicitly starting with
fresh state. Current-version restart recovery and backup/restore remain
supported; old execution contracts are not resumed.

## 17. Trust Boundaries

Trusted:

- the Fern host administrator and root;
- local Docker daemon administrators;
- the Fern binary and configured host policy;
- the GitHub App private key store;
- the private TLS edge and tailnet policy.
- repository owners and repository code, with respect to host and network
  abuse. The current Docker bridge is a resource boundary, not a security
  sandbox for hostile repositories.

Untrusted or separately constrained:

- remote requests before pairing/plugin authentication;
- all request headers and JSON bodies;
- OpenCode/model output;
- repository contents and Git configuration for result integrity and durable
  authority decisions;
- container names without exact labels and runtime proof;
- ambiguous network and process outcomes.

Secrets are not written to receipts, evidence payloads, container labels,
plugin KV, command arguments, or repository files. Plugin tokens live in the OS
keyring. GitHub App credentials stay host-side. Background environment
injection, including provider credentials, is rejected until a trusted provider
broker and restricted egress network exist. Credential-bearing remote providers
are therefore not supported by this profile.

## 18. Backup And Credentials

Backup is offline under the same repository-name lease used by `fern up`.
Before staging, Fern checkpoints and integrity-checks every SQLite database.
The backup contains non-secret configuration, host repository, control and
plugin auth state, taskstore, compatibility ledgers, retained CAS objects, and
the disposable-resource host key. The archive tool segregates protected
environment and detected credential files into its external credential
artifact instead of the main generation.

It excludes lock/recovery scratch, run clones, clone quarantine and stage
directories, artifact work directories, publication checkouts, containers, and
volumes. A restored startup inspects every taskstore-referenced CAS object before
serving work. Operational restore keeps a durable pre-restore generation for
explicit rollback.

GitHub App credentials can be exported and rotated as bounded age-encrypted
bundles. Binding includes Fern name, mode, host, App ID, installation ID,
repository ID, and canonical full name. Activation validates the candidate
against GitHub before replacing the private store and writes an encrypted prior
generation when one exists.

## 19. Package Map

| Package | Responsibility |
| --- | --- |
| `cmd/fern` | CLI, composition, backup, credentials, process lifecycle |
| `backgroundopencode` | pinned disposable OpenCode client and observations |
| `backgroundroute` | exact live target/session capabilities, request policy, shutdown, and fencing |
| `backgroundruncoord` | serial run effect coordinator and recovery |
| `config` | strict compatibility loader and production validator |
| `control` | devices and pairing |
| `credentialbundle` | age-encrypted GitHub credential bundles |
| `githubapp` | onboarding, installation tokens, repository authority |
| `pluginauth` | fixed-scope plugin device authorization and revocation |
| `proxy` | remote/operator ingress and browser security |
| `run` | persistence-independent lifecycle policy and immutable resource/runtime identities |
| `runapi` | plugin-authenticated run contract |
| `runcommand` | transport-independent create/stop/seal policy, admission, replay, and post-commit notification |
| `runclientapi` | operator/client discovery and attachment admission |
| `task` | identifiers, actor snapshots, idempotency vocabulary |
| `taskartifact` | deterministic Git bundle creation, CAS, materialization |
| `taskenvdocker` | disposable Docker resources and writer proof |
| `taskresultsource` | CAS-only result binding and verified checkout acquisition |
| `taskstore` | schema 3 run/result authority and state machines |
| `observability` | health, readiness, status, metrics, retry |
| `hostlease` | exclusive host-local repository-binding lease |
| `compatibility` | fresh-schema and release-manifest alignment |

`gitref` and `jsoncanon` are narrow shared validation utilities.
Integration packages qualify Docker, OpenCode, upgrades, and releases.

### Boundary decisions

Package extraction follows ownership, not a generic layered template:

For this one-host, capacity-one system, new packages and adapters are not the
default way to improve a boundary. GitHub delivery belongs to the harness,
not a separate host workflow. Ordinary store
records may remain plain structs; introduce domain values only when they remove
duplicated decisions or protect authority-bearing combinations. Keep the
explicit coordinator and route allow-list rather than building workflow or
policy frameworks.

- `run` cannot import taskstore, Docker, HTTP, or coordinators. Storage keeps
  compatibility names for its callers, but current run enum values and lifecycle
  interpretation come from this domain owner.
- `runcommand` owns create/stop/seal policy and idempotent acceptance. Private
  hash projections preserve historical request bytes without making the HTTP
  DTO or its JSON field order the public application interface.
- Artifact integrity and retained-result authority remain separate. The engine
  verifies bytes and materializes a checkout; the resolver establishes which
  durable result those bytes belong to. Neither consumer supplies an unverified
  snapshot to bypass fresh verification.

SQL constraints, current-resource reinspection, and domain validation are
independent protections at different boundaries. Consolidating meaning does
not remove those protections or make historical observations permanently valid.

## 20. Deployment

The supported host layout is:

```text
/usr/local/bin/fern
/etc/fern/fern.yaml
/etc/fern/fern.env
/var/lib/fern/                 HOME and Fern state
/srv/fern/repository/          bound source repository
/opt/fern/src/ARCHITECTURE.md  local operator reference
```

Use `deploy/systemd/fern.service` with user `fern`, group `fern`, supplementary
group `docker`, `UMask=0077`, and the included hardening directives. Publish
only `127.0.0.1:8080` and `127.0.0.1:8443` through private TLS. Keep
`127.0.0.1:8081` host-only.

Readiness fails for corrupt durable state, missing GitHub App credentials, or
failed background components.
Liveness reports only process availability.

## 21. Qualification And Release

Required local gates are:

```sh
test -z "$(gofmt -l .)"
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 go test ./internal/taskstore ./internal/compatibility
make build

cd plugins/opencode
bun run format:check
bun run typecheck
bun test
```

Docker/release gates qualify the source-pinned Background Run image, serial run
lifecycle, exact-session attachment fencing, artifact retention, fresh-schema
initialization, deployment files, reproducibility, SBOM,
signatures, and provenance. Release publication must bind the image by registry
digest; a local image ID is not portable registry authority. Each architecture
is built as a candidate, the exact pushed digest is qualified under its target
architecture, and only those candidate digests are promoted into the release
manifest.

No synthetic harness may claim a physical phone test, host reboot,
replacement-host restore, independent tailnet ACL denial, release, or signed
tag. Those facts require operator-supplied evidence.

## 22. Current Scale

The repository reports final package and line counts after implementation in
the change summary rather than hard-coding a number that will immediately
drift. Use:

```sh
go list ./... | wc -l
find cmd internal integration scripts -name '*.go' ! -name '*_test.go' -print0 | xargs -0 cat | wc -l
find cmd internal integration scripts -name '*_test.go' -print0 | xargs -0 cat | wc -l
find plugins/opencode/src -name '*.ts' -print0 | xargs -0 cat | wc -l
find plugins/opencode/test -name '*.ts' -print0 | xargs -0 cat | wc -l
```
