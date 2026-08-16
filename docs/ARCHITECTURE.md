# Fern Architecture

This document describes the implemented first-party system. Code and tests are
authoritative if behavior changes.

## System Boundary

Fern is one native Go process supervising one durable OpenCode workspace in one
local Docker container. OpenCode owns coding sessions, provider calls, tools,
and its client protocol. Docker owns process and network isolation. Fern owns:

- strict desired configuration and ownership checks;
- one lifecycle writer per workspace;
- a stable authenticated loopback proxy;
- wake coalescing and request admission;
- protocol-aware activity observation and conservative idle stop;
- classification of intentional stops versus failed compute.

Fern is not a hostile-tenant sandbox. The service account can access Docker,
the selected repository is writable by the container, and provider credentials
enter that container. The host user, repository, image, and Docker daemon are
inside one trust boundary.

```text
remote client
    |
    | HTTPS and private identity (for example, Tailscale Serve)
    v
host TLS edge
    |
    | loopback HTTP
    v
Fern proxy  <---->  manager  <---->  Docker Engine
    |                    |                 |
    | dynamic loopback   | SSE/status      | container lifecycle
    v                    v                 v
OpenCode server in a constrained container
    |
    +-- bind-mounted repository
    +-- protocol-specific persistent data volume
```

Fern accepts only numeric loopback proxy listeners. OpenCode Basic auth is
checked by Fern before request admission or wake. Remote access requires a
separate private TLS edge; Basic auth alone is not transport security.

## Composition And Startup

`cmd/fern/up.go` is the composition root:

1. Load defaults, strict YAML, and explicit flag overrides.
2. Expand required environment references and validate the repository.
3. Require a numeric loopback proxy address and bind it before Docker effects.
4. Acquire the host-local workspace lease under `~/.fern/locks`.
5. Create a local-Docker runtime backed by pause intents in `~/.fern/state`.
6. Construct the stream controller, workspace manager, supervisor, and proxy.
7. Ensure the container is running and healthy.
8. Connect the activity stream before publishing the endpoint to requests.
9. Start the idle supervisor and HTTP server in one cancellation group.

Only the default Docker endpoint or an absolute Unix-socket endpoint is
accepted. Fern depends on host-local bind mounts, loopback port publication,
file locks, and pause-intent state. This rejects ordinary TCP/SSH Docker
endpoints, but a Unix socket that proxies elsewhere cannot be distinguished
from a local daemon.

## Configuration And Command Paths

Configuration precedence is explicit flags, YAML, then defaults. The YAML
decoder rejects unknown fields for `up`; repository paths in YAML are relative
to the config file, while a relative `-repo` override is relative to the
calling directory. Environment references are required, and the final
environment is part of desired state. Rotating a provider key or OpenCode
password therefore requires `fern down` followed by `fern up`; session storage
is retained.

Full startup validation requires an existing repository, positive memory and
idle duration, numeric loopback listener, supported protocol, and the selected
protocol's password. V1 requires `OPENCODE_SERVER_PASSWORD`, V2 requires
`OPENCODE_PASSWORD`, and auto requires both.

The command surface has different ownership rules:

| Command | Role | Workspace lease |
| --- | --- | --- |
| `up` | Long-running supervisor and proxy | Exclusive writer |
| `down` | Stop/remove compute and clear pause intent | Exclusive writer |
| `status` | Inspect classified Docker state as text or stable JSON | None; read-only |
| `logs` | Stream Docker logs | None; read-only |
| `debug events` | Inspect health and SSE directly on the backend | None; diagnostic bypass |
| `attach` | Launch the protocol-specific official client | None; connects through proxy origin |
| `version` | Print embedded release identity | None |

`down`, `status`, and `logs` intentionally load only the workspace name so
incident cleanup still works when unrelated configuration is broken. `attach`
and `debug events` load only their relevant projections. Explicit remote attach
origins must be HTTPS; plaintext HTTP is accepted only for numeric loopback.
`debug events` is observation-only but bypasses manager admission and endpoint
generation ownership, so it is a diagnostic path rather than normal client
traffic.

The CLI treats malformed invocation as exit status 2 and operational failure as
status 1. Help and version requests write to stdout and succeed. Normal errors
are concise stderr diagnostics rather than structured service logs. A child
launched by `attach` retains its nonzero exit status; interactive SIGINT or
SIGTERM termination is treated as a clean user exit. `status --json` always
emits the workspace, classified state, Docker status, process exit code, and OOM
flag. Runtime states are data, so a successful status query exits zero even when
the observed workspace is absent or failed.

## Desired And Observed State

`runtime.Spec` contains the stable desired workspace name, image, repository,
memory, OpenCode protocol, and environment. The Docker implementation adds
fixed init, port, data-volume, CPU, and PID policy. A fingerprint is stored on
the container. Initial startup and cache-miss wake/reconciliation verify the
fingerprint plus selected actual Docker settings before publishing an endpoint.

Every mutable Docker resource must carry Fern's managed and workspace labels.
Mutations use the inspected immutable container ID, not an unverified reusable
name. Ownership is checked before every mutation. Full desired-state drift
verification applies when creating or reusing compute; `Pause` and `Destroy`
have no desired `Spec` and enforce ownership rather than full drift comparison.
Actual verification covers the settings Fern depends on, not every Docker
field or image-provided environment value.

The observed lifecycle states are:

| State | Meaning |
| --- | --- |
| `absent` | No container exists. |
| `provisioning` | Created/restarting compute or an unresolved pending stop. |
| `running` | The process is running and not externally frozen. |
| `paused` | Fern committed an intentional stop, or Docker reports a frozen process. |
| `failed` | The process exited without a committed Fern pause, died, or was OOM-killed. |

Fern writes a pending pause intent before asking a running container to stop
and commits it only after a successful stop response. A Docker `created`
container is the safe exception: because no process ran, Fern can directly
commit it as paused. A failed stop response remains an unknown outcome; Fern
does not relabel a concurrent crash as a safe pause. Pending intent is scoped
to the exact container ID and is written with atomic rename and filesystem
sync.

## In-Process Ownership

The workspace manager combines three separate synchronization domains:

- `wakeMu` owns the cached endpoint, generation number, and one shared wake;
- a lifecycle token serializes Docker ensure-running and pause operations;
- `admissionMu` owns held-request count and the pause admission barrier.

Concurrent cache-miss requests share one wake operation with a 90-second
service-level deadline. Canceling one caller stops that caller waiting but does
not cancel the shared wake needed by other callers. Endpoint publication gets a
monotonic generation, and failures invalidate only the exact generation they
used.

An endpoint is published only after health succeeds and its activity stream is
connected. If Fern started stopped compute and observer setup then fails, it
attempts to stop it again. Failure while attaching to compute that was already
running does not stop that process.

## Request Path

The proxy classifies requests conservatively:

| Intent | Examples | Wakes? | Holds pause admission? | Invalidates old idle evidence? |
| --- | --- | --- | --- | --- |
| Observe | selected protocol's SSE endpoint | No | No | No |
| Read | selected health/status endpoints | Yes | Yes | No |
| Work | every unknown, mutating, or upgraded request | Yes | Yes | Yes |

Unknown routes default to work because a future OpenCode GET endpoint may start
execution. Basic credentials are validated before this classification reaches
the manager.

For read and work requests, `workspace.Manager` acquires an admission lease and
coalesces concurrent cache-miss wakes into one Docker operation. On a cache
miss, it resolves health and attaches the exact endpoint generation's activity
observer before forwarding. A cached endpoint is reused without per-request
Docker inspection or health probing. External failure, freeze, or replacement
is normally discovered by a transport failure, which invalidates that
generation so the following request reconciles runtime state. Reachable
upstream HTTP error responses do not invalidate the cache. The admission lease
remains held until reverse proxying finishes.

Backend endpoint generations prevent a failed old request from clearing a
newer endpoint. Transport failures and canceled held requests invalidate the
generation so the next request reconciles Docker state. Cancellation of an
observation-only SSE request does not invalidate healthy compute.

Route classification uses exact escaped paths. Trailing slashes, encoded path
variants, unknown GETs, and WebSocket upgrades default to work. Fern validates
Basic credentials before admission and forwards the accepted Authorization
header unchanged to OpenCode; it is an authentication gate, not a credential
translation layer.

## Activity And Pause Protocol

The stream controller owns one monotonically increasing epoch per backend
endpoint. Connected, disconnected, status, malformed-status, and work-request
observations are serialized through the supervisor. Stale epochs cannot make a
new endpoint eligible to stop.

The supervisor requires a connected epoch to report a busy or retry state and
then drain all observed active sessions to idle. Disconnects, malformed or
unknown status, and requests that may start work cancel eligibility. Merely
starting Fern against an already idle process does not arm a stop timer.

One epoch represents one published backend generation, not one TCP connection.
The stream controller reconnects to that endpoint with bounded exponential
backoff after disconnect. A disconnect immediately cancels pause eligibility.
If a pause attempt fails, the supervisor preserves the eligible boundary and
retries after at most five seconds rather than requiring another turn.

When the timer expires, the manager performs a second authoritative barrier:

1. Reject pause if any held HTTP request is active.
2. Block admission of new held requests.
3. Serialize against wake and other lifecycle operations.
4. Reinspect Docker and resolve the current endpoint.
5. Query all protocol-specific activity surfaces with authentication.
6. Stop only if every response is valid and idle.

V1 uses one `/session/status` response. V2 sequentially samples all of
`/api/session/active`, `/api/shell`, `/api/pty`, `/api/permission/request`, and
`/api/form/request`. OpenCode V2 does not expose one atomic aggregate snapshot,
so these reads cannot prove that every surface described the same instant. Fern
blocks new held proxy requests during the sequence and fails closed on any
active, unknown, or unavailable response, but internal state can transition
between reads.

This protects requests using Fern's proxy. A same-host Docker administrator can
discover the backend's loopback port and bypass admission; that principal is
already inside the trusted-host boundary.

## OpenCode Protocols

| Concern | V1 | V2 |
| --- | --- | --- |
| Health | `/global/health` | `/api/health` |
| Lifecycle event stream | `/event` | `/api/event` |
| Auth | configurable Basic username/password | username `opencode`, `OPENCODE_PASSWORD` |
| Client | `opencode attach URL` | `opencode2 --server URL` |
| Data volume | `fern-<name>-data` | `fern-<name>-v2-data` |

`auto` probes both health contracts, requires exactly one to validate, requires
both protocol passwords, and uses `fern-<name>-auto-data`. Explicit protocol
selection is preferred because it catches image/protocol mismatches and keeps
persistence intent clear. Do not reuse an auto workspace with an image tag that
can change between V1 and V2: detection occurs after the shared auto volume is
mounted, so Fern cannot provide explicit-mode protocol isolation in that case.

In auto mode, the watcher and final idle checker use the negotiated endpoint
protocol, but proxy authentication and route classification remain configured
as auto: Fern accepts either credential pair and recognizes both route sets.
The original Authorization header is forwarded, so a credential for the
non-selected protocol can pass Fern's gate and then receive an OpenCode 401.
Explicit mode avoids this split ownership.

## Container And Persistence Model

The workspace container has:

- the configured memory limit;
- a two-CPU quota and 512-process limit;
- Docker init enabled and no restart policy;
- exactly one writable repository bind mount;
- exactly one writable protocol-specific OpenCode data volume;
- one dynamically assigned host port bound exclusively to loopback.

`fern down` removes the container and pause intent but retains the data volume.
The next `fern up` recreates compute around that durable state. A newly created
volume is rolled back if initial container creation never starts; once OpenCode
has started, its data is retained even if later health setup fails. The
workspace lease prevents another Fern writer from racing volume creation, but
the idempotent Docker volume API cannot prove whether an external Docker
administrator won the inspect/create race.

Explicit protocol changes require container recreation and never mount V1 data
into V2 or vice versa. Auto mode has the mutable-image limitation above. Fern
does not migrate OpenCode databases.

## Failure And Recovery Semantics

| Condition | Result |
| --- | --- |
| OpenCode exits without committed intent | `failed`; operator inspects logs and runs `fern down` |
| OOM or dead container | `failed` |
| Successful Fern stop | `paused`; next held request starts it |
| Failed/unknown stop response | Pending intent is preserved and endpoint invalidated |
| Pending intent on an exited container | `provisioning`; wake-to-reconcile is required |
| Externally Docker-paused process | Classified `paused`; a held request unpauses and reconciles it |
| Spec or selected actual-setting drift on reuse | Refused until `fern down` recreates compute |
| Activity/status error | Pause deferred; compute remains running |

The pause safety guarantee applies only to requests using Fern's proxy and to
the OpenCode activity surfaces available for the selected protocol. It does not
recover live provider streams or tool state after process death.

## Shutdown And Supervision

SIGTERM cancels the shared service context, so proxy, supervisor, activity
stream, and wake work begin cancellation concurrently. HTTP shutdown has a
five-second graceful period and then closes tracked connections, including
upgrades. After the service goroutines return, Fern waits for manager-owned
requests, pause, lifecycle, and wake work; then it stops the stream controller
and closes Docker. Manager close is intentionally unbounded inside Fern so the
Docker client is not closed under an owned operation. The systemd unit provides
the outer 120-second stop limit. Pause reconciliation itself uses the caller's
context and cannot silently extend its deadline.

Stopping the Fern process does not stop OpenCode. This permits service restart
to reattach to running compute, but operational backups must stop the service
and run offline `fern down` before archiving repository and volume state.

The checked-in systemd unit supervises `fern up`, runs with Docker-group access,
and therefore does not create a privilege boundary from Docker. It has no
`ExecStop` lifecycle mutation: stopping Fern leaves OpenCode running. Target-host
reboot and Docker-restart behavior remains an explicit deployment acceptance
task.

## Assurance And Limits

Unit, race, vet, formatting, image-build, and V1/V2 Docker lifecycle jobs are
defined in CI. Those lifecycle jobs use a deterministic protocol fixture in
real Docker, not the OpenCode images. CI separately runs the real version-pinned
V2 protocol smoke; there is no equivalent real V1 smoke job. The lifecycle
harness covers creation, authentication
before wake, concurrent wake, request/pause exclusion, endpoint replacement,
persistence, clean-exit and OOM classification, shutdown, and frozen-container
recovery. The real V2 smoke test covers the pinned beta artifact.

The V1 binary is checksum-pinned. V2 pins the base image and top-level npm
package version, but does not check in an npm lockfile or complete transitive
integrity closure; rebuilding it is therefore less reproducible than V1.

Long-lived streaming is favored over aggressive connection limits. The HTTP
server bounds request-header reads but has no Fern-level write or idle timeout,
request-body limit, or connection-count quota. The SSE client bounds connection
setup but has no post-connect heartbeat deadline; a silent open socket remains
connected until the transport reports failure. These choices support SSE and
upgraded connections under the trusted-single-user model, but they are not
denial-of-service protection.

Not yet established by checked-in evidence:

- target-host systemd, reboot, Docker restart, backup, and restore rehearsal;
- remote laptop and phone access through the intended tailnet;
- provider-backed V2 model turns and upstream upgrade compatibility;
- recovery of in-progress provider or tool work after process death;
- hostile multi-user or multi-tenant isolation.
