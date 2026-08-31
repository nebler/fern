# OpenCode Integration

Fern's persistent workspace supports one current OpenCode V2 server contract.
There is no automatic protocol mode or cross-version state migration. A
separate Background Run qualifications exist for the published OpenCode
`1.18.16` package and for source commit
`39fb919a054190498f6d5b7985bde231f93ad7a6`; neither is a second persistent
workspace image. Fern can now qualify and durably admit the source profile, but
this commit does not perform disposable Docker or prompt effects.

## Product Boundary

`opencode2 serve` provides the official complete OpenCode web UI at `/` together
with the APIs, SSE stream, and upgraded connections that power it. Fern proxies
that origin without replacing its frontend. In particular, Fern does not build
a custom coding PWA and does not consume, bundle, or fork `@opencode-ai/app`.

OpenCode owns:

- the browser and terminal UI;
- sessions and OpenCode configuration;
- provider setup and model interaction;
- permission requests and questions;
- terminals, files, and diffs.

Fern owns the container lifecycle, wake and pause admission, and the stable
same-origin proxy. Fern-owned `/fern/*` routes provide the phone control page,
remote-ingress readiness, restart-safe device administration, workflow/session
correlations, durable task admission/delivery/cancellation, conservative
execution observation, explicit snapshot sealing, verification, and
receipt-backed App publication without copying the coding interface. Generic
terminal-result classification, durable approval answers, and notifications
remain future services because this pinned OpenCode profile exposes no closed
durable terminal-success or restart-stable approval object.

## Image And Configuration

Build the single pinned image:

```bash
make image
```

The image is `fern/opencode:dev` by default and is defined by
`images/opencode/Dockerfile`. It runs `opencode2 serve` on container port 4096 as
UID/GID 1001 and stores OpenCode state below
`/home/user/.local/share/opencode`, which Fern mounts from
`fern-<workspace>-v2-data`. The image also points `XDG_CONFIG_HOME` inside that
volume so global OpenCode agents, commands, skills, plugins, models, and
permissions survive container recreation.

The isolated Background Run image is `fern/opencode-background:dev`, defined by
`images/opencode-background/Dockerfile` and built with `make image-background`.
It installs official `opencode-ai@1.18.16` and is qualified only by
`integration/opencode-background-contract`. It does not replace
`fern/opencode:dev`, alter `fern-<workspace>-v2-data`, or make Background Run
execution available.

The distinct source candidate is `fern/opencode-background-source:dev`, defined
by `images/opencode-background-source/Dockerfile` and built with
`make image-background-source`. Its profile is
`source-39fb919a054190498f6d5b7985bde231f93ad7a6`: the build fetches and verifies
that exact full commit, installs with Bun `1.3.14` and the frozen repository
lockfile, builds one native Linux binary with the official UI embedded, and
labels the final image with the source URL, revision, and commit-derived
version. Although the checkout's package metadata says `1.18.16`, this profile
does not claim equivalence to the published package. It is qualified separately
by `integration/opencode-background-source-contract` and does not alter the
persistent workspace profile.

A minimal workspace configuration is:

```yaml
workspace:
  name: demo
  image: fern/opencode:dev
  repo: .
  memory: 8Gi
  env: {}
idle:
  after: 10m
proxy:
  listen: 127.0.0.1:8080
  operatorListen: 127.0.0.1:8081
```

This is valid local-only configuration. Remote publication is unsupported until
`proxy.remoteOrigin` is set to the exact canonical private HTTPS root, such as
`https://fern-host.example.ts.net` without a trailing slash. Fern then pins the
upstream `Host`, `X-Forwarded-Host`, `X-Forwarded-Proto`, and effective port to
that origin while removing all client forwarding headers.

Set `OPENCODE_PASSWORD` in the host or protected service environment. OpenCode's
Basic-auth username is `opencode`. Do not put the password in YAML or command
arguments. Run `fern attach` and use the official client's `/connect` flow to
connect an OpenCode account or another supported provider. The resulting
OpenCode-managed state persists in the workspace volume and is exposed in the
web UI. Provider environment credentials can also be forwarded through
`workspace.env` when desired.

## Proxy Contract

Paired clients use Fern's stable remote origin for every OpenCode route,
including `/`. The official local CLI uses the operator origin. Fern
wakes stopped compute before forwarding ordinary UI and API requests, holds
pause admission while relevant requests are active, streams responses, and
preserves the origin expected by the official UI.

The local fake tests cover this metadata and unchanged SSE flushing, but the
exact pinned image still needs acceptance through real TLS for absolute links
and redirects and through WSS for terminal/upgraded traffic.

Fern owns the complete `/fern/*` namespace for landing, liveness/readiness,
telemetry, pairing, device, retired compatibility, task, and control routes.
These routes do not acquire workspace
admission or wake OpenCode. Paths outside that namespace, including similar
names such as `/fern-smoke`, are proxied unchanged.

The following plugin-authorization routes are implemented independently of run
execution:

| Purpose | Method and route | Authority |
| --- | --- | --- |
| Begin setup | `POST /fern/api/plugin-auth/start` | Public remote route |
| Poll setup | `POST /fern/api/plugin-auth/poll` | Public remote route with the device code |
| Review setup | `GET /fern/plugin-auth/authorize?id=...&code=...` | Paired device only |
| Approve | `POST /fern/api/plugin-auth/requests/:id/approve` | Paired device plus CSRF, or loopback operator |
| Deny | `POST /fern/api/plugin-auth/requests/:id/deny` | Paired device plus CSRF, or loopback operator |
| List grants | `GET /fern/api/plugin-auth/credentials` | Loopback operator only |
| Revoke grant | `DELETE /fern/api/plugin-auth/credentials/:id` | Loopback operator only |
| Revoke self | `POST /fern/api/plugin-auth/self/revoke` | That plugin bearer |

All plugin-authorization JSON requests are strict and bounded. Setup expires
after 10 minutes, starts are limited to one per second, polls to one per five
seconds, and durable state caps authorization records, credentials, and invalid
polls.
Successful repeated polls return the same device code presented by the caller;
Fern never persists plaintext device codes, user codes, or bearers. The fixed
grant is valid for 90 days and has only `run:create`, `run:read`, `run:stop`,
`run:open`, and `run:result`.

Start returns `verification_uri` and `verification_uri_complete` rooted only in
the configured trusted remote origin. The complete link opens a paired-device
page showing the fixed OpenCode client and scopes. Its approve and deny buttons
fetch an existing route-bound CSRF token and submit the strict JSON decision API.
The page never receives or renders the device code or bearer.

`POST/GET /fern/api/runs`, `GET /fern/api/runs/:id`, and the `stop`, `open`, and
`result` descendants are reserved for plugin bearers. Admission commits an
immutable task/attempt generation and disposable-environment intent before any
effect. This build intentionally has no disposable Docker provider: accepted
runs remain `queued`; stopping a queued run atomically records it, its task, and
its attempt as `failed` with the honest `background_run_stopped_before_start`
reason; and open/result return `not_ready`. `tasks.backgroundImage` and
`tasks.backgroundImageID` are an optional pair with no default. The latter is a
canonical `sha256:` local image ID pinned by the operator. At startup Fern uses
read-only Docker inspection and requires that exact ID; exact OCI source,
revision `39fb919a054190498f6d5b7985bde231f93ad7a6`, version, and Fern profile
labels; user `1001:1001`; exact server argv; only exposed port `4096/tcp`; and no
baked `OPENCODE_SERVER_PASSWORD`. Only then does observability report the
profile as `qualified`. Both the Background Run and its attempt commit that
qualified image ID and source profile as their execution identity; normal
persistent attempts remain unchanged. The local ID pin is only the current
single-host dogfood boundary. Externally distributed images still require
promotion to a registry digest and independent verification. The run API
accepts only the source-commit profile and returns `profile_unavailable` when
the qualified image is absent. The plugin still uses the preceding profile
until its following compatibility commit and is not changed here.
Plugin bearers receive `404` on routes outside their allowlist and never fall
through to the persistent OpenCode workspace.

The lifecycle integration uses these V2 surfaces:

| Purpose | Route |
| --- | --- |
| Health | `/api/health` |
| Events | `/api/event` |
| Foreground sessions | `/api/session/active` |
| Shells | `/api/shell` |
| PTYs | `/api/pty` |
| Permission requests | `/api/permission/request` |
| Forms | `/api/form/request` |
| Questions | `/api/question/request` |

The durable-task contract harness additionally pins these observed
`0.0.0-next-17444` behaviors for image digest
`sha256:73688cd6f96ce3b236bb1c2d25607b03566a4ee92f0fedabeb06fd1a3e643c6c`:

- caller-selected session and top-level-text prompt IDs are preserved;
- session creation and exact reads project the complete configured title, agent,
  model provider/model ID, and working-directory tuple;
- a response-lost prompt appears in the inbox, exact retry is stable, and a
  conflicting retry returns `409`;
- `/api/session/{id}/message` is finite, ordered, cursor-paginated, and
  duplicate-free;
- exact promoted caller-message reads preserve the caller ID, `user` type,
  exact text, and positive creation time; `resume` remains an inbox-only field;
- model questions use session forms and an answered form resumes once;
- pending synthetic permissions disappear after both same-container process
  restart and container replacement while their durable session persists;
- an undelivered `resume:false` prompt survives process restart, can be deleted
  without a message/provider projection, remains absent across replacement, and
  permits exact-ID reuse with changed text after deletion;
- interrupt before admission and after completed provider work is a `204` idle
  no-op with no durable cancel latch or new interruption evidence;
- an exact 65,536-byte `resume:false` prompt is admitted once without provider
  execution, matching Fern's task-admission text limit;
- `resume` is not persisted or idempotency-bound: replaying one pending ID/text
  from `false` to `true` returns the same admission object and starts execution,
  so Fern must never retry prompt mutation and cannot claim upstream resume
  proof during reconciliation;
- interrupt during exactly one marker-bearing fake-provider turn closes the
  stream, empties the inbox, projects one caller message, and records the
  interruption; a second idle interrupt changes neither state nor disconnects;
- direct pending and answered forms are process-epoch state, and a lost
  caller-selected form ID can be recreated with changed metadata and options;
- exact prompt retry after container replacement does not duplicate the inbox,
  message, or fake-provider turn, and interruption evidence survives;
- global events are volatile, session history/event routes are absent, and the
  experimental log is not a durable replay source.

Pending and answered forms disappear across process epochs, and a model question
may remain represented as a running but inactive tool after replacement. When
Fern positively detects that epoch loss, it must move the attempt to
`recovery_required`, not reconstruct or auto-answer it. The current observer
cannot prove every epoch change, so inconclusive cases remain running or require
operator recovery rather than fabricating loss. Direct permission approval and
pending-permission epoch loss are proven by the pinned harness.
An actual model tool-generated permission is intentionally omitted; the harness
does not execute a command to manufacture approval state. Permission decision
and interruption races remain open. Run the zero-cost harness with:

```bash
python3 integration/opencode-contract/contract_harness.py
```

`internal/opencodeapi` implements this pinned loopback contract with required
deadlines, bounded bodies, strict envelopes, redacted errors, exact identity and
ownership checks, finite message-scan anomaly detection, and no automatic
mutation retries. It is an adapter only; task transitions and form epoch-loss
policy remain Fern coordinator responsibilities.

Delivery reconciliation uses exact read-only projections. A session must match
caller ID, title, agent, model provider/model ID, and working directory. A
pending prompt must match ID, session owner, `user` type, `steer` delivery,
payload text, and positive creation time. A promoted prompt must match ID,
`user` type, text, and positive creation time. The adapter returns only closed
states, reports `resume` as unobservable, never returns prompt text or raw
objects, and rejects simultaneous inbox/history presence.

Unknown routes are treated as potential work because OpenCode may add routes
whose GET requests start execution. This also ensures root UI assets and
upgraded connections pass through the same wake-aware path.

Immediately before an idle stop, Fern blocks new held proxy requests and
samples all activity surfaces. The reads are conservative but are not one
atomic OpenCode snapshot. Any active item, malformed response, unknown state,
authentication failure, or unavailable endpoint leaves compute running.

## Authentication Boundary

The remote listener accepts only pairing capabilities and durable device
cookies; it rejects both Basic credentials before wake. The loopback-only
operator listener accepts `opencode:$OPENCODE_PASSWORD` for the official CLI and
the distinct `fern:$FERN_CONTROL_PASSWORD` for administration and pairing
issuance. Every admitted OpenCode request receives a newly generated backend
Authorization header.

Pairing GET renders a confirmation page without consuming the short-lived code;
the confirmation POST issues a Fern device credential in an `HttpOnly`,
`Secure` cookie. The remote ingress authenticates that cookie, removes it before
proxying, and injects the internal OpenCode credential. Device cookies authorize
OpenCode and Fern task access only; they cannot access `/fern/control`, device
administration, retired host-publication routes, or pairing issuance. An
eligible App publication request is a task API command with its own receipt,
verification checks, and CSRF protection. OpenCode continues serving every
non-`/fern/*` UI and API route.

Only the remote listener may be published through a private TLS edge. The
operator listener must remain host-local and must never be a Serve target or be
exposed to a LAN or the internet.

Plugin device-flow start and poll are the only additional unauthenticated remote
routes. Approval reuses an already trusted device or operator boundary; it does
not add OAuth clients, dynamic scopes, refresh tokens, or configuration fields.
Each admitted plugin request receives a server-owned `ActorOpenCode` snapshot,
fixed scope context, revocation registration, and credential-expiry deadline.

Restoring or rolling back Fern control state can restore a previously revoked
device or plugin credential. Treat backups as credential-bearing: after any
rollback, repeat revocation against the restored Fern state and rotate any
client-held bearer that may have survived. Revocation performed only after the
restored snapshot cannot survive restoration.

## Persistence And Upgrades

`fern down` removes the workspace container but retains
`fern-<workspace>-v2-data`. A later `fern up` recreates compute around the same
OpenCode sessions and global configuration. Back up the volume before changing the
pinned OpenCode version. Fern does not transform or repair OpenCode's database;
OpenCode remains the data-format authority.

Fern's offline backup commands export and restore the managed OpenCode volume,
but they do not transform its contents. The host task-store compatibility
harness does not imply OpenCode database downgrade compatibility. Back up before
changing the pinned image and restore the complete pre-change volume if rollback
is required.

Changing the image or environment changes Fern's desired-state fingerprint and
requires container recreation. Use immutable image tags or recorded image
digests for deployment.

## Verification

Run the deterministic lifecycle harness and real pinned-OpenCode smoke test
with local Docker:

```bash
make image
./scripts/test-lifecycle.sh
FERN_OPENCODE_IMAGE=fern/opencode:dev ./scripts/test-opencode.sh
```

The lifecycle harness verifies creation, auth-before-wake, concurrent wake,
request/pause exclusion, endpoint replacement, persistence, failure and OOM
classification, shutdown, and frozen-container recovery. The OpenCode smoke
test verifies the actual image, authenticated V2 routes, official server
surface, activity, recreation, sessions, and global configuration. Provider-backed model turns
and target-browser acceptance remain credentialed external checks.

Qualify the separate zero-cost Background Run image contract with:

```bash
python3 integration/opencode-background-contract/contract_harness.py
python3 integration/opencode-background-source-contract/contract_harness.py
```

That harness reports its captured local image ID and all blocked properties. In
particular, official 1.18.16 ignores caller-selected Session IDs, has no durable
SSE replay, and does not provide conflict-safe prompt retries; these limitations
must not be confused with the stronger persistent `0.0.0-next-17444` contract.

The source-commit harness captures the exact local image ID and passes that ID,
not its mutable tag, to every container. For commit
`39fb919a054190498f6d5b7985bde231f93ad7a6`, it proves caller-selected Session
and prompt IDs, exact agent/model/location reconciliation, finite durable
prompt-admission history, side-effect-free exact retry before and after
container replacement, and HTTP `409` for conflicting reuse. Active execution
and pending questions/permissions remain process-local. A hanging provider turn
has durable admission and promotion but no durable step-start or settlement
event before replacement, so its outcome is uncertain rather than complete.
Deep-link checks cover embedded-UI HTTP fallback for official directory and
server-scoped routes; browser navigation, private TLS, external origins, and WSS
remain unqualified.
