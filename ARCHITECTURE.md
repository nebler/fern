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
- idempotent run create, stop, and seal admission;
- exact disposable resource identity and lifecycle;
- run revisions, stop receipts, and evidence;
- short-lived exact-session attachment capabilities and connection shutdown;
- writer inactivity proof;
- Git bundle export and local content-addressed storage;
- short-lived, repository-scoped GitHub credentials for the runtime;
- backup, restore, and validated GitHub App credential storage.

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
               SQLite state db       run coordinator   GitHub App credentials
                       |                 |
                       |                 v
                       |         taskenvdocker provider
                       |                 |
                       |   one exact writer container
                       |                 |
                       |          live loopback port
                       |                 |
                       +------ opencode.Router :8443
                                         |
                                  private TLS :8443

 agent + git/gh -> GitHub branch / draft PR
 stopped writer -> retained artifact CAS (independent of publication)
```

The three listeners are bound before Docker side effects. The remote listener
is the paired-device and plugin surface. The operator listener is loopback-only
and protected by the Fern control password. The Background Run listener has no
default target: `opencode.Router` binds one exact live runtime and
removes it and waits for admitted forwarding to exit before writer teardown.

## 4. Startup Composition

`fern up` performs this sequence:

1. Strictly load YAML and protected environment values.
2. Apply `config.ValidateBootstrap`.
3. Bind remote, operator, and live-run listeners.
4. Acquire the host-local repository-name lease.
5. Open the workspace SQLite database (store schema 12), which also holds
   devices and plugin authorizations.
6. Compose control and plugin-authorization stores over it.
7. If the installation ID is pending, block readiness without composing task
   services.
8. Otherwise apply strict `config.Validate` and resolve exact GitHub
   App repository authority.
9. Qualify the exact Background Run image through Docker inspection.
10. Open the artifact CAS and inspect every referenced artifact.
11. Build run, route, and HTTP services with the scoped token source.
12. Start all services under one cancellation errgroup.

The operator creates the GitHub App by hand (metadata read, contents and pull
requests write, no webhook), installs it on only the bound repository, and runs
`fern credentials set --app-id N --private-key app.pem` while Fern is stopped.
That command parses the RSA key and stores it only after proving it live: the App
must see the configured installation, and the installation must expose the
configured repository (matching ID, name, and owner, not archived or disabled)
with the required permissions. Fresh-host configuration may omit
`workspace.github.installationId`; Fern then serves devices and plugin
authorization, blocks readiness, and returns `503` for run operations. Missing
credentials take the same blocked path. Neither state can compose task
services; fixing either requires a restart.

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
`run:result`. They are not configuration.

Each control listener has one route table in `internal/proxy/routes.go`, the
complete access policy for that listener. A request authenticates as the realm
of the credential it carries (bearer, device cookie, or Basic), never by path; a
route that does not admit that realm answers 404. Unlisted paths are 404 and
other methods 405, before authentication. Percent-encoded paths are rejected.
Authenticated mutations require the same origin, and device mutations also
require a CSRF token bound to the method and exact path.

| Listener | Routes | Realm |
| --- | --- | --- |
| remote | `POST /fern/api/plugin-auth/{start,poll}`, `GET\|POST /fern/pair` | public |
| remote | `GET /fern/`, `GET /fern/api/v1/csrf`, `GET /fern/plugin-auth/authorize`, `POST /fern/api/plugin-auth/requests/:id/{approve,deny}` | device |
| remote | `/fern/api/runs` (runapi, including `GET /fern/api/runs/:id/attach`), `POST /fern/api/plugin-auth/self/revoke` | plugin |
| operator | `GET /fern/live`, `GET /fern/ready` | public |
| operator | landing, control page, `POST /fern/pair/new`, device and plugin credential administration, plugin approval, `GET /fern/api/runs[/:id[/attach]]` (runapi, workspace-wide, read and attach only) | operator |

The live route listener (`opencode.Router`, `:8443`) is separate: it admits only
attachment capabilities and applies the OpenCode allow-list in `policy.go`.

Ingress installs a validated `domain.ActorSnapshot` in request context. Inner API
packages do not derive identity from client-controlled headers or bodies.
Receipts bind actor, command kind, workspace, idempotency key, and canonical
request hash.

## 7. Admission

Run creation accepts only a clean, born Git repository with an unambiguous
canonical remote and exact `HEAD`. The plugin rechecks the repository after
human confirmation. Fern independently verifies the submitted base against its
bound host repository.

Admission atomically writes one queued `runs` row and its create receipt. The
task store has four tables:

- `workspaces`: the bound repository, GitHub App installation, and image;
- `runs`: one row per run, keyed by its `run_` UUIDv7. Its immutable intent is
  the prompt, repository and base, branch, agent and model, deadline, image,
  profile, environment digest, OpenCode session and message, and creator actor.
  Its lifecycle is state, phase, revision, and the authority recorded once
  along the way (runtime identity, prompt fence, stop/timeout/seal admission,
  writer fence), plus last evidence, last error, and the cleanup proof;
- `receipts`: one per accepted create, stop, or seal, bound to actor, command
  kind, workspace, idempotency key, and request hash, pointing at its run;
- `results`: a sealed run's retained snapshot, selected and then sealed.

Clone, volume, container, and endpoint names derive from the run ID and are not
stored.

The coordinator wakes only after commit. A repeated matching idempotency claim
returns the original receipt. A changed hash conflicts. Another actor cannot
probe the original claim.

`runapi` owns create, stop, and seal application operations (`service.go`):
request policy, canonical idempotency hashing, replay interpretation, base
verification, identity generation, admission coordination, and notification
after fresh commits. Command inputs are a private intent separate from HTTP
DTOs, and outputs contain accepted-command facts rather than raw store
receipts. The HTTP layer (`runapi.go`) owns routing, authentication/scopes,
bounded decoding, error mapping, and response encoding.

## 8. Run State

The public run state is:

```text
queued -> setting_up -> working <-> needs_you
   |          |             |
   +----------+-------------+-> canceling (stopping or sealing)
                              -> uncertain
                              -> result_ready
                              -> failed
                              -> cleanup_required
```

The coordinator is an observe-and-act loop. Clones, volumes, containers,
routes, and sessions have deterministic identities, so each pass re-inspects
them and acts only where they differ from the run's intent; the durable phase
records only what inspection cannot re-derive:

```text
absent            queued; no effect started
 -> provisioning  clone, volume, container, start, health, route, session
 -> prompt_pending  one-way prompt fence set; dispatched at most once
 -> admitted      prompt confirmed in the session; work observed
 -> sealing       (seal) writer fence, then export; resources retained
 -> cleaning      (stop, timeout, failure, or committed result) teardown
 -> cleanup_complete
```

A pass reconciles its phase until it is blocked, stable, or the operation
deadline expires: provisioning reaches the prompt fence in one pass, and
cleaning drains the route, stops the exact writer, and removes container,
volume, and clone in one pass. A pass cut short is simply repeated. The durable
records inside a phase are the started runtime identity (container ID, start
time, epoch, port), the prompt-request fence, stop/timeout/seal admission, the
writer fence, the selected result, and the terminal cleanup proof. There are no
per-run claims or leases: `fern up`'s host lease admits one coordinator per
workspace, and every transition is a compare-and-swap on the run's workspace,
ID, revision, state, and phase, so a write prepared before a concurrent stop or
seal fails.

`run` owns lifecycle classification independently of persistence: valid
state/phase combinations, whether a phase is executing (bound by the run
deadline and the configured execution identity), and timeout eligibility. The
coordinator dispatches concrete effects; it does not maintain a second list of
phase categories. Taskstore keeps SQL representation private and enforces
durable transitions transactionally.

## 9. Disposable Resource Identity

Execution requires native Linux and a local Docker daemon; Docker Desktop is
unsupported. `tasks.runtimeStorageRoot` names an operator-provisioned XFS
project-quota root enclosing disposable clone and state-volume directories,
separate from durable SQLite/CAS storage. Both byte and inode hard limits must
be enforced, with project inheritance covering new files. Runtime storage is
mandatory for execution but may be omitted during control-plane bootstrap.
Configuration validation checks only absolute, cleaned, non-root path shape;
the provider owns filesystem and quota enforcement checks and fails closed.
Neither `init` nor `doctor` provisions quotas, changes host mounts, or certifies
live exhaustion behavior. Operators must qualify byte/inode exhaustion and
recovery on the target Linux host and maintain free-space/inode reserve for
Docker images/logs and durable data outside the runtime quota. macOS tests do
not establish Linux quota qualification.

`taskenvdocker.Provider` owns Docker policy. Every clone, volume, container,
endpoint, and runtime gets a deterministic Fern identity derived from immutable
run state and a private host key, and every provider step inspects before it
creates or removes. A container is this run's only if it has the canonical
name, the qualified image ID, and Fern's ownership and spec-digest labels (the
digest binds the run, repository, image, and environment identities); its
other settings are Fern's own create request and are not re-checked. Health,
route dials, credential writes, stop, and removal additionally require the
exact committed runtime (container ID and start timestamp, hence epoch and
token) and, where used, the loopback published port.

Replacement or unowned resources are quarantined or rejected. The provider
does not trust names alone. Host Git inspection of a clone happens only while
no run container exists, because the agent can write the clone.

`domain.Resources` owns canonical resource-name derivation and matching;
`domain.Runtime` owns exact timestamp and token interpretation. Both have private
representations. These identity values are not inactivity proof: destructive
provider operations still re-observe the exact resource before acting.

Resource-spec version is 10. The image must carry `ai.fern.runtime.spec="10"`
and the rotating GitHub credential helpers. The lane is intentionally serial, with
capacity one. The qualified source and observed-clone envelope is 128 MiB, and
clone work has the same 30-second deadline as its Git operation.

## 10. Live Route

`opencode.Router` maps short-lived opaque capabilities to one exact
authenticated runtime and OpenCode session. The route is activated only for a
committed runtime that has just passed authenticated health; attachment is
offered once provisioning has reconciled the session and fenced the prompt.
The run API can mint a random two-hour capability only while the complete
workspace, run, container ID, start time, runtime epoch, and session tuple
remains active. Fern retains only the capability digest in process memory.

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

Stop, timeout, and seal all converge on the exact writer. Fern stops the
committed container process epoch and then proves that it is non-running; a
container that started but whose runtime was never recorded is adopted for
cleanup only after it attests as this run's. A replacement container or changed
identity invalidates the proof. Cleanup re-proves inactivity on every pass;
seal records it once as a durable writer fence, and export reads only under it.

Stop and seal race atomically: each is a compare-and-swap on the admitted
run's revision, so exactly one wins. A stopped run moves to `cleaning`; a
sealed run moves to `sealing` (state `canceling`, not `cleanup_required`) and
can no longer be stopped or timed out.

## 12. Seal And Retention

Seal is explicit and irreversible. Its receipt and the run's seal columns
(receipt, result ID, request time, policy version) commit together before any
teardown. Each sealing pass then:

1. Unless the run already records one, proves the exact writer stopped and
   records the writer fence on the run.
2. Unless CAS already holds exactly the selected result, acquires the stopped
   source clone under its identity lock, captures committed, staged, unstaged,
   and untracked changes without mutating the source, and builds a
   `git_bundle_v1` object and canonical manifest bound to the run and result
   ID.
3. On the first pass, inserts the `results` row in state `selected` (commit,
   tree, change digest, the canonical manifest stored whole, bundle digest); on
   later passes, requires the deterministic re-snapshot to equal it.
4. Installs it under `artifact-cas/sha256:<manifest digest>` and re-inspects it.
5. Materializes a detached checkout and proves its base, result commit, and
   tree.
6. Seals the result row with the materialization proof and moves the run to
   `result_ready`/`cleaning` in one transaction.

A failed pass records `last_error` on the sealing run and is retried. A
trigger keeps a sealed run, and so its resources, in `sealing` until its
result row is sealed; a selected row's tuple is immutable and a sealed row is
immutable and undeletable. Cleaning then deletes route, container, volume, and
clone.

The result remains available only when retention is verified and
reconstructable. Every positive plugin API projection comes from a fresh CAS
inspection checked against the complete run and result tuple. Artifact
locators and host paths are not returned through the plugin API.

Result consumption uses `artifact.Engine.Acquire` to perform one fresh full
verification and return its snapshot together with an owned detached checkout.
The result-source resolver checks that snapshot against the durable run and
result, closing the checkout on mismatch. Materialization still checks the
copied bundle against the verified digest and size; acquisition does not cache
integrity observations across operations.

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
`tasks.budget.maxTurns` configuration; run timeouts remain enforced.

## 14. Harness-Owned GitHub Delivery

The runtime image installs Git and GitHub CLI. The agent can run checks, push a
branch, and open a draft PR as part of its task. Fern has no publication API,
publication journal, or host-side Git push/PR coordinator. The harness owns
remote mutation and reconciliation after ambiguous responses.

Fern retains the GitHub App private key on the host and mints short-lived
installation tokens restricted to the bound repository. Before admitting the
prompt and during active execution, it refreshes runtime credentials in the
private disposable OpenCode volume, outside the clone and retained artifact.
Tokens are not written into Docker environment/configuration, store,
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

`fern runs` queries `/fern/api/runs` through either loopback operator Basic
authentication or the existing remote plugin bearer. It shows running runs by
default and has a stable `--json` projection for automation. Bare `fern attach`
selects the only attachable run or presents an interactive picker; an exact run
ID bypasses selection.

`GET /fern/api/runs/:id/attach` is an operator/client control operation, not a
plugin UI operation and not a browser deep link. It re-reads run ownership and
route readiness, then asks `opencode.Router` for an in-memory capability
bound to the active runtime and session. `fern attach` starts
`opencode attach <origin> --session <session-id> --pure` with the capability in the
OpenCode authentication environment. The existing OpenCode process remains the
only writer and retains its volume, transcript, tools, permissions, and file
ownership. Attachment adds a client; it does not replace the agent or transfer
the clone to another container.

## 16. Recovery

Recovery is the ordinary reconcile pass: after a restart the coordinator
resumes the stored phase and re-inspects derived resources. It never treats
process-local memory as authority. Recovery rules include:

- repeat an effect only when it is idempotent by inspection (deterministic
  names, spec labels, content addressing); never repeat a prompt POST once its
  fence is durable, and reconcile it against bounded history instead;
- retain `uncertain` when exact outcome cannot be proven;
- stop only the exact committed writer epoch, or an unrecorded one that
  attests as this run's;
- export only under the recorded writer fence, and only content equal to the
  selected result;
- reject result consumption when any artifact tuple field differs;
- keep resources until absence is proven, and a sealed run's until its result
  commits;
- wake coordinators only after durable admission commits.

Taskstore schema is 9. This pre-release reset has
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

## 18. Durable State, Backup, And Credentials

Fern's durable state is the workspace's one SQLite database
(`~/.fern/tasks/<name>.db`), the artifact CAS beside it, the disposable-resource
host key, and the private GitHub App credential file. The database schema is
owned by `store` and also defines the tables of `auth` (paired devices,
operator credential ID, plugin authorizations and
credentials); that package issues their own SQL through the shared handle, one
transaction per operation. Only digests of device tokens and plugin device and
user codes are stored. In-flight request registries that revocation cancels
stay in memory. `safeio` private-file writes remain only for the credential file and host key.

Backup is offline: `fern backup` takes the workspace lease used by `fern up`
and every other lease file in the state directory. `fern backup create` writes
one age-encrypted gzip tar containing the state directory, the configuration,
and the protected environment file, plus a manifest with a sha256 for every
file. SQLite databases are captured with `VACUUM INTO` and integrity-checked.
Locks, SQLite sidecars, run clones and markers, and artifact work directories
are excluded; of the runtime root only the disposable-resource host key is kept.
Containers and volumes are never part of a backup.

`fern backup restore` decrypts into a staging directory next to the state
directory, rejects anything but regular files under `state/` and `config/`,
requires every checksum to match the manifest, and integrity-checks restored
databases before activating anything. Restore never replaces anything: it
refuses when the state directory holds more than locks or when the
configuration or environment file exists, so restoring over a host means moving
the old state aside first. Activation is a short sequence of renames with undo:
the held lease directory moves into the staged state, and the staged state and
configuration files take their places. A restored startup inspects every
store-referenced CAS object before serving work.

The GitHub App credential file (`~/.fern/github-app/app-credentials.json`, mode
0600) holds only the App ID and private key; it is not encrypted at rest, and
backups carry it age-encrypted. Rotating the key means generating a new one on
GitHub, running `fern credentials set` again, restarting, and then revoking the
old key on GitHub.

## 19. Package Map

Packages are listed in suggested reading order: lifecycle first, then the
command path, effects, and composition, then the boundaries they rely on. Each
package's doc comment (`go doc ./internal/<name>`) records its invariants and
ownership boundaries.

| Package | Responsibility |
| --- | --- |
| `internal/domain` | identifiers, actor snapshots, idempotency vocabulary, persistence-independent lifecycle policy, immutable resource/runtime identities, and shared Git ref, GitHub name/remote, and path validation |
| `internal/runapi` | run HTTP contract (plugin bearer; operator for list/get/attach), attachment admission, plus create/stop/seal policy, admission, replay, and post-commit notification |
| `internal/backgroundruncoord` | serial run effect coordinator and recovery |
| `cmd/fern` | CLI, composition, backup, credentials, process lifecycle |
| `internal/store` | the SQLite database: schema 12, run/result authority and state machines |
| `internal/artifact` | deterministic Git bundle creation, CAS, materialization, and CAS-only result binding with verified checkout acquisition |
| `internal/taskenvdocker` | disposable Docker resources, writer proof, container GitHub credential delivery |
| `internal/opencode` | pinned disposable OpenCode client and observations; the Router's exact live target/session capabilities, request policy, shutdown, and fencing |
| `internal/proxy` | remote/operator ingress, pairing, and browser security |
| `internal/auth` | device identities, operator credential ID, and fixed-scope plugin device authorization and revocation (SQLite tables) |
| `internal/githubapp` | credential file, installation tokens, repository discovery and authority |
| `internal/config` | strict configuration loader and bootstrap/execution validation |
| `internal/observability` | in-memory component readiness behind the liveness and readiness probes |
| `internal/safeio` | atomic replace and bounded read of private files (App credentials, host key), trust-neutral filesystem helpers, the exclusive host-local repository-binding lease, and strict JSON validation before typed decoding |

Integration packages under `integration/` qualify Docker and OpenCode; each has
its own README.

### Boundary decisions

Package extraction follows ownership, not a generic layered template:

For this one-host, capacity-one system, new packages and adapters are not the
default way to improve a boundary. GitHub delivery belongs to the harness,
not a separate host workflow. Ordinary store
records may remain plain structs; introduce domain values only when they remove
duplicated decisions or protect authority-bearing combinations. Keep the
explicit coordinator and route allow-list rather than building workflow or
policy frameworks.

- `domain` cannot import store, Docker, HTTP, or coordinators. Run enum
  values and lifecycle interpretation come from this domain owner.
- `runapi` owns create/stop/seal policy and idempotent acceptance. Private
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
CGO_ENABLED=0 go test ./internal/store
./scripts/test-critical-coverage.sh
./scripts/test-deployment.sh
make release VERSION=v0.0.0-dev

cd plugins/opencode
bun run format:check
bun run typecheck
bun test
```

The Docker qualification job exercises the source-pinned Background Run image,
serial run lifecycle, exact-session attachment fencing, and artifact retention.
A release is only the `make release` binaries and `SHA256SUMS`, uploaded by the
tag workflow; the Background Run image is built and qualified locally and bound
by local image ID, so it is not published or signed.

No synthetic harness may claim a physical phone test, host reboot,
replacement-host restore, or independent tailnet ACL denial. Those facts
require operator-supplied evidence.

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
