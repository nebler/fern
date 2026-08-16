# Fern Architecture

This document separates the implemented lifecycle system from the proposed
production gateway. Code and tests are authoritative when behavior changes.

## System Boundary

Fern is a native Go process supervising one durable OpenCode workspace in one
local Docker container. OpenCode V2 is the only supported server contract.

OpenCode is authoritative for its complete web UI, sessions, configuration,
providers, permissions and forms, terminals, files, diffs, tool execution, and
client protocol. Its official web UI is served at the OpenCode server root and
reaches it through Fern's same-origin reverse proxy. Fern does not build a
coding PWA or consume or fork `@opencode-ai/app`.

Fern currently owns:

- strict desired configuration and Docker resource ownership checks;
- one lifecycle writer per workspace;
- a stable authenticated loopback proxy;
- wake coalescing and request admission;
- V2 activity observation and conservative idle stop;
- classification of intentional stops versus failed compute.

Fern is also the intended owner of future remote delivery, notifications,
publication, and recovery. Those product services are not implied by the
implemented lifecycle component.

```text
browser or official OpenCode client
             |
             | HTTPS at a private edge
             v
       Fern proxy origin
        |             |
        | /fern/*     | all other paths
        | reserved    | wake-aware same-origin proxy
        v             v
 future Fern UI     OpenCode V2 server
 and callbacks        |          |
                      |          +-- official web UI at /
                      +-- APIs, SSE, and upgrades
                              |
                 repository bind mount
                 OpenCode data volume
```

Today `/fern/*` has no implemented pairing or admin surface. The current proxy
uses Basic auth and forwards OpenCode credentials. The production design will
route `/fern/*` to Fern-owned device pairing, administration, and GitHub
callbacks, authenticate a Fern `HttpOnly` device cookie, and inject internal
OpenCode auth for proxied routes. Keep implemented and proposed behavior
distinct.

Fern is not a hostile-tenant sandbox. The service account can access Docker,
the repository is writable by OpenCode, and provider credentials enter the
container. The host user, repository, image, and Docker daemon share one trust
boundary.

## Composition And Startup

`cmd/fern/up.go` is the composition root:

1. Load defaults, strict YAML, environment references, and flag overrides.
2. Validate the repository, limits, credentials, and numeric loopback listener.
3. Bind the listener before Docker effects.
4. Acquire the host-local workspace lease under `~/.fern/locks`.
5. Create the local-Docker runtime backed by pause intents in `~/.fern/state`.
6. Construct the stream controller, manager, supervisor, and reverse proxy.
7. Ensure the OpenCode container is running and healthy.
8. Attach its activity stream before publishing the backend endpoint.
9. Run the idle supervisor and HTTP server in one cancellation group.

Fern accepts only the default Docker endpoint or an absolute Unix socket. It
depends on host-local bind mounts, loopback publication, locks, and pause-intent
state. Ordinary TCP and SSH Docker endpoints are rejected.

## Configuration And Commands

Configuration precedence is flags, YAML, then defaults. YAML is strict;
repository paths in YAML are relative to that file, while `-repo` is relative
to the caller. Expanded environment is part of desired state. Rotating
`OPENCODE_PASSWORD` or a provider key requires `fern down` and `fern up`, while
the OpenCode data volume remains intact.

OpenCode's current Basic username is fixed to `opencode`; its password comes
from `OPENCODE_PASSWORD`. This is the current upstream credential, not the
future Fern device credential.

| Command | Role | Workspace lease |
| --- | --- | --- |
| `up` | Long-running supervisor and proxy | Exclusive writer |
| `down` | Remove compute and clear pause intent | Exclusive writer |
| `status` | Inspect classified Docker state | Read-only |
| `logs` | Stream Docker logs | Read-only |
| `debug events` | Direct backend health/SSE diagnostic | Diagnostic bypass |
| `attach` | Launch the official OpenCode terminal client through Fern | Read-only |
| `version` | Print release identity | None |

`down`, `status`, and `logs` load only the workspace identity so incident
cleanup remains possible when unrelated configuration is invalid. Normal
clients use the proxy. `debug events` bypasses request admission and is not an
application traffic path.

## Desired And Observed State

The runtime specification contains workspace name, image, repository, memory,
and environment. Docker adds fixed init, port, volume, CPU, and PID policies.
Fern records a fingerprint on the container and checks it plus the settings it
depends on before reuse. Drift is rejected rather than silently adopted.

Every mutable Docker resource has Fern-managed and workspace labels. Fern
checks ownership before mutation and operates on inspected immutable container
IDs rather than trusting reusable names.

| State | Meaning |
| --- | --- |
| `absent` | No container exists. |
| `provisioning` | Compute is being created/reconciled or a stop outcome is unresolved. |
| `running` | OpenCode is running and not externally frozen. |
| `paused` | Fern committed an intentional stop, or Docker reports a frozen process. |
| `failed` | OpenCode exited without committed Fern intent, died, or was OOM-killed. |

Fern writes a pending pause intent before stopping a running container and
commits it only after Docker reports success. The record is scoped to the exact
container ID and atomically persisted. An ambiguous stop remains unresolved;
Fern does not relabel a concurrent crash as a safe pause.

## Wake And Request Path

The workspace manager separates endpoint/wake synchronization, lifecycle
serialization, and request admission. Concurrent requests to stopped compute
share one wake operation. Canceling one waiter does not cancel a wake still
needed by others. Every published backend has a monotonically increasing
generation so an old transport failure cannot invalidate a replacement.

An endpoint is published only after authenticated health succeeds and the V2
event stream is connected. A cached endpoint avoids a Docker inspection on
every request. Transport failures invalidate that exact generation; reachable
upstream HTTP errors do not.

The proxy classifies V2 routes conservatively:

| Intent | Examples | Wakes? | Holds pause admission? | Invalidates idle evidence? |
| --- | --- | --- | --- | --- |
| Observe | lifecycle SSE | No | No | No |
| Read | health and activity reads | Yes | Yes | No |
| Work | root UI, unknown, mutating, or upgraded requests | Yes | Yes | Yes |

Treating unknown routes as work preserves compatibility with the full official
OpenCode UI and future upstream routes. Exact escaped paths are used; trailing
slashes, encoded variants, and upgrades fail toward the conservative class.

Current Basic credentials are checked before request admission or wake. The
Authorization header is forwarded unchanged to OpenCode. Fern is presently an
authentication gate, not a credential translation layer.

The future gateway changes only this authentication edge. A Fern device cookie
will be validated before lifecycle admission, Fern will inject a private
OpenCode credential upstream, and `/fern/*` will be handled separately. The
OpenCode origin and UI remain otherwise unchanged.

## Activity And Idle Stop

The stream controller assigns an epoch to each published backend generation.
Connected, disconnected, status, malformed-status, and work-request events are
serialized through the supervisor. Stale epochs cannot make new compute
eligible to stop.

An epoch must report busy or retry and then drain all observed sessions to idle
before the timer starts. Disconnects, unknown states, and requests that may
start work cancel eligibility. Starting Fern against an already idle process
does not itself arm a stop.

When the timer expires, the manager performs an authoritative barrier:

1. Refuse to stop while a held request exists.
2. Block admission of new held requests.
3. Serialize with wake and lifecycle operations.
4. Reinspect Docker and resolve the current endpoint.
5. Authenticate and query V2 sessions, shells, PTYs, permissions, and forms.
6. Repeat the complete activity query while admission remains closed.
7. Stop only when every response in both passes is idle.

The activity reads are sequential, not an atomic OpenCode snapshot. The second
pass catches activity that begins during the first pass. Fern blocks new held
proxy requests throughout and fails closed on active, unknown, or unavailable
results, but OpenCode can still transition internally between reads.

## Container And Persistence

The workspace container has the configured memory limit, a two-CPU quota, a
512-process limit, Docker init, no restart policy, one writable repository bind
mount, one writable OpenCode data volume, and a dynamically assigned backend
port published only on loopback.

The single image is built from `images/opencode/Dockerfile` and tagged
`fern/opencode:dev` for development. The persistent volume is
`fern-<workspace>-v2-data`, mounted at
`/home/user/.local/share/opencode`. The image points `XDG_CONFIG_HOME` into that
mount so OpenCode's global configuration survives with its database. `fern
down` removes compute and pause intent but retains this volume. Fern does not
own OpenCode's data format or perform database migrations.

## Failure And Shutdown Semantics

| Condition | Result |
| --- | --- |
| OpenCode exits without committed intent | `failed`; inspect logs and recreate explicitly |
| Host shutdown stops running compute | `failed` if no Fern pause intent was committed |
| OOM or dead container | `failed` |
| Successful Fern stop | `paused`; the next held request wakes it |
| Failed or unknown stop | Pending intent retained and endpoint invalidated |
| Activity query error | Stop deferred; compute remains running |
| Desired-state drift | Reuse refused until explicit recreation |

SIGTERM cancels proxy, supervisor, event observation, and wake work together.
The HTTP server gets a bounded graceful shutdown, then Fern waits for its owned
manager operations before closing Docker. Stopping Fern itself does not stop
OpenCode, which allows service restart and reattachment but makes quiet backups
an explicit `fern down` operation.

The checked-in systemd unit has no lifecycle-mutating `ExecStop`. If host
shutdown kills running OpenCode before Fern commits pause intent, the next boot
classifies it as failed and requires operator reconciliation. Complete reboot
recovery is not yet implemented.

## Authentication And Network Boundaries

Fern accepts only numeric loopback proxy listeners. A separate private TLS edge
such as Tailscale Serve is required for remote use. Basic auth is not transport
security. Do not expose the current listener publicly.

The planned production chain is:

```text
TLS edge -> Fern HttpOnly device-cookie auth -> lifecycle admission
         -> injected internal OpenCode auth -> OpenCode UI/API
```

Pairing, cookie rotation/revocation, Fern admin handlers, and GitHub callbacks
are future work. Until they exist, Tailscale identity plus current OpenCode
Basic auth is the practical private-deployment boundary.

## Assurance And Limits

CI is expected to run formatting, unit and race tests, vet, binary build, the
single image build, the deterministic real-Docker lifecycle harness, and the
pinned OpenCode smoke test. Relevant local commands are `make image`,
`./scripts/test-lifecycle.sh`, and `./scripts/test-opencode.sh`.

Not established by checked-in evidence:

- production Fern device pairing and cookie authentication;
- target-host reboot, Docker restart, backup, and restore rehearsal;
- remote browser acceptance on intended laptop and phone devices;
- provider-backed model turns across each supported provider;
- recovery of in-progress provider streams or tool work after process death;
- hostile multi-user or multi-tenant isolation.
