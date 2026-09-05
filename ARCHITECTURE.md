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
- idempotent task, stop, open, seal, intervention, ownership-transfer, and
  publication admission;
- exact disposable resource identity and lifecycle;
- run claims, revisions, cancellation epochs, and evidence;
- read-only live-route capabilities and draining;
- exact writer generations and cold human takeover/handback;
- writer inactivity proof;
- Git bundle export and local content-addressed storage;
- verification and GitHub publication journals;
- backup, restore, credential rotation, and schema compatibility.

OpenCode owns:

- model and provider execution;
- the session transcript;
- prompts, tools, permissions, questions, and UI;
- file edits made while its disposable writer is active.

Fern does not own a persistent OpenCode home, a persistent repository mount, a
wake-on-request proxy, or an idle supervisor. Its browser surface is a bounded
paired/operator run-control deck, not a general OpenCode or task-management UI.

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
                    pairing / plugin auth / run-control APIs
                                         |
                       +-----------------+------------------+
                       |                 |                  |
                   taskstore 11      run coordinator   GitHub App broker
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

 retained artifact CAS -> verifier -> publication journal -> draft PR
```

The three listeners are bound before Docker side effects. The remote listener
is the paired-device and plugin surface. The operator listener is loopback-only
and protected by the Fern control password. The Background Run listener has no
default target: `backgroundroute.Manager` binds one exact live runtime and
removes and drains it before writer teardown.

## 4. Startup Composition

`fern up` performs this sequence:

1. Strictly load YAML and protected environment values.
2. Apply `config.ValidateBackgroundBootstrap`.
3. Bind remote, operator, and live-run listeners.
4. Acquire the host-local repository-name lease.
5. Open control and plugin-authorization state.
6. Open taskstore schema 11.
7. If the installation ID is pending, block readiness and expose onboarding
   without composing task services.
8. Otherwise apply strict `config.ValidateBackground` and resolve exact GitHub
   App repository authority.
9. Qualify the exact Background Run image through Docker inspection.
10. Open the artifact CAS and inspect every referenced artifact.
11. Build run, verification, publication, route, and HTTP coordinators.
12. Start all services under one cancellation errgroup.

Fresh-host configuration may omit `workspace.github.installationId` only to
bootstrap onboarding. Fern then exposes onboarding, blocks readiness, and
returns `503` for run and result operations. After creating and installing the
App on the configured repository, the operator records the numeric installation
ID from GitHub's installation URL and restarts Fern. Missing credentials follow
the same blocked, onboarding-only path. Neither state can compose task services.

## 5. Configuration Authority

Production requires:

- `workspace.name` and an absolute repository path;
- `workspace.github.mode: github-app-broker`;
- exact installation ID, repository ID, and canonical full name;
- explicit agent, model provider, model ID, timeouts, and turn budget;
- exact Background Run image reference and canonical image ID;
- explicit Background Run environment;
- a loopback live-route listener and private HTTPS live origin;
- remote and operator loopback listeners;
- a control password of at least 32 characters.

`workspace.image`, `workspace.memory`, `workspace.env`, `idle`, and
`workspace-gh` remain parser and schema compatibility vocabulary. Production
composition does not consume them. `OPENCODE_PASSWORD` is not forwarded; each
disposable runtime receives a Fern-derived server credential.

## 6. Authentication And Actors

Fern has three ingress actor classes:

- operator: loopback Basic authentication with the host-only control password;
- paired device: restart-safe secure cookie whose digest is stored by Fern;
- OpenCode plugin: device authorization followed by a fixed-scope bearer.

The plugin scopes are `run:create`, `run:read`, `run:stop`, `run:open`, and
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

Resource-spec version is 9. The current lane is intentionally serial, with
capacity one. The qualified source and observed-clone envelope is 128 MiB, and
clone work has the same 30-second deadline as its Git operation.

## 10. Live Route

`backgroundroute.Manager` maps one opaque capability to one exact authenticated
runtime. The route is activated only after container health, endpoint identity,
and OpenCode session identity are committed. `open` returns a fresh URL only
while that exact route remains active.

The agent route admits only `GET` and `HEAD`; it rejects all mutations and all
HTTP upgrades before OpenCode. Its identity includes workspace, task, attempt,
run generation, writer generation, role, container ID, and runtime epoch. Human
write access never reuses this route.

Removal is a fence:

1. Remove target admission.
2. Cancel and close admitted HTTP, SSE, and WebSocket traffic.
3. Wait for target users to drain.
4. Return route-removal evidence.

No persistent OpenCode path is forwarded by the remote or operator gateway.

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

## 13. Verification

Verification is optional explicit host policy. Fern never infers a command from
the repository. The command is an argument vector, not a shell string, and runs
against a newly materialized retained artifact.

Only `retained_artifact` results can become new verification work. Historical
`persistent_workspace` results remain readable but are excluded by the taskstore
selection predicate. The verifier re-reads result ownership, acquires and
inspects the CAS artifact, commits a started journal phase, runs the exact
policy, accounts for bounded stdout/stderr, and records terminal evidence.

## 14. Publication

Publication requires a successful verification for the exact result commit.
The paired/operator publication API accepts an idempotency key and expected
verification ID; repository, base, result, installation, branch, and draft-PR
tuple are derived from durable state.

The publication coordinator acquires and re-inspects a fresh CAS checkout on
every pass, re-reads the selected revisions, and then advances one journal
phase. Push and pull-request mutations are each preceded by durable started
phases and followed by exact GitHub observations. Lost responses become
read-only reconciliation, never blind duplicate mutation.

## 15. Intervention And Writer Ownership

Paired devices and the loopback operator can use `/fern/api/v1/runs` and
`/fern/runs`. The plugin-only `/fern/api/runs` contract remains separate. Warm
inspection uses a credential-free, networkless container with the clone mounted
read-only. Interrupt and steer are durable control rows with idempotency receipts,
claims, CAS revisions, and the exact agent writer generation/runtime tuple.
Interrupt records its irreversible attempted boundary and becomes `uncertain`
after a crash there; steering reuses one durable OpenCode message identity and
reconciles history before dispatch. Stop, seal, and takeover cannot race an
active warm command.

Writable takeover is a cold writer-ownership transfer, not an OpenCode interrupt
or route toggle:

```text
agent_owned W1
 -> takeover_requested (route removal, inspector removal, agent stop/removal,
                         OpenCode volume removal, Git boundary)
 -> human_owned W2
 -> handback_requested (terminal drain, human stop/removal, Git boundary,
                         fresh volume/container/session/resume prompt)
 -> agent_owned W3
 -> ...
```

`background_run_ownerships` is independent of the public run lifecycle. It
stores mode, effect phase, current and target resource tuples, monotonically
increasing writer generation, request receipt/actor, bounded evidence, claim,
lease generation, and revision. Transitional modes publish no writer route.
Every Docker mutation attests the exact labeled object and runtime epoch; a
replacement or contradictory object fails closed.

The human writer is the PID 1 Bash process in a dedicated container. It has no
network, credentials, ports, or OpenCode state volume; it has a read-only root,
bounded tmpfs state, dropped capabilities, `no-new-privileges`, and only the
clone bind-mounted writable. Fern uses Docker attach, never Docker exec. The
browser WebSocket is same-origin, tied to paired-device cancellation, and is
drained before handback. An expired attempt automatically begins handback so
normal timeout processing can resume under an agent-owned generation.

At each cold boundary Fern proves that all writer containers are absent, then
records `HEAD`, status metadata, tracked diff bytes, and untracked-content object
identities in bounded Git evidence. Handback creates a fresh OpenCode volume,
container process, session, and deterministic re-read/resume prompt; no previous
OpenCode process state is reused. Only after exact prompt reconciliation does
Fern reactivate the read-only agent route and commit the new odd-numbered writer
generation. Ambiguous identity or external outcomes move ownership to
`uncertain` and leave routes closed for explicit repair.

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

Schema version is 11. Legacy phase names and columns remain decodable so
the first supported baseline upgrades without byte or semantic reinterpretation.
Legacy tasks are not silently converted into Background Runs.

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
| `backgroundroute` | exact live target capability, drain, and fencing |
| `backgroundruncoord` | serial run effect coordinator and recovery |
| `config` | strict compatibility loader and production validator |
| `control` | devices, pairing, legacy disposition, route state |
| `credentialbundle` | age-encrypted GitHub credential bundles |
| `githubapp` | onboarding, installation tokens, repository authority |
| `pluginauth` | fixed-scope plugin device authorization and revocation |
| `proxy` | remote/operator ingress and browser security |
| `runapi` | plugin-authenticated run contract |
| `runcontrol` | paired/operator control API and bounded HTML deck |
| `runterminal` | same-origin WebSocket to exact Docker terminal attach |
| `resultapi` | paired/operator retained result and publication admission |
| `task` | identifiers, actor snapshots, idempotency vocabulary |
| `taskartifact` | deterministic Git bundle creation, CAS, materialization |
| `taskenvdocker` | disposable Docker resources and writer proof |
| `taskpublication` | stateless GitHub push and draft-PR effects |
| `taskpublicationcoord` | durable publication effect journal |
| `taskresultsource` | CAS-only result checkout authority |
| `taskstore` | schema 11 durable authority and state machines |
| `taskverification` | durable verification coordination |
| `verification` | shell-free bounded host check runner |
| `observability` | health, readiness, status, metrics, retry |
| `hostlease` | exclusive host-local repository-binding lease |
| `compatibility` | first-supported-baseline upgrade contract |

`gitref`, `jsoncanon`, and `evidence` are narrow shared validation utilities.
Integration packages qualify Docker, OpenCode, upgrades, and releases.

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

Readiness fails for corrupt durable state, unresolved legacy publication
authority, missing GitHub App credentials, or failed background components.
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
lifecycle, read-only inspector, cold human writer, fresh-runtime handback,
artifact retention, upgrade baseline, deployment files, reproducibility, SBOM,
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
