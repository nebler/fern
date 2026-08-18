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

Fern now serves an authenticated landing page and readiness endpoint under
`/fern/*`. A Basic-authenticated local request can mint a five-minute one-time
pairing link; consuming it over private HTTPS creates a secure `HttpOnly` cookie
and Fern injects internal OpenCode auth for proxied routes. Fern persists only a
digest of the 30-day device token under `~/.fern/control`; device listing,
expiry, revocation, workflow/session correlations, and publication operations
survive process restart. Five-minute pairing codes remain intentionally
process-local and single-use.

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
5. Open the versioned Fern control snapshot under `~/.fern/control`.
6. Create the local-Docker runtime backed by pause intents in `~/.fern/state`.
7. Construct the stream controller, manager, supervisor, and reverse proxy.
8. Reconcile startup without creating or resuming absent/paused compute.
9. Adopt running compute and attach its activity stream before publishing the
   backend endpoint; otherwise wait for the first admitted OpenCode request.
10. Run the idle supervisor and HTTP server in one cancellation group.

Fern accepts only the default Docker endpoint or an absolute Unix socket. It
depends on host-local bind mounts, loopback publication, locks, and pause-intent
state. Ordinary TCP and SSH Docker endpoints are rejected.

## Configuration And Commands

Configuration precedence is flags, YAML, then defaults. YAML is strict;
repository paths in YAML are relative to that file, while `-repo` is relative
to the caller. Expanded environment is part of desired state. Rotating
`OPENCODE_PASSWORD` or a provider key requires `fern down` and `fern up`, while
the OpenCode data volume remains intact.

OpenCode's Basic username is `opencode`; its password comes from
`OPENCODE_PASSWORD` and enters the container. Fern control routes use the
distinct username `fern` and host-only `FERN_CONTROL_PASSWORD`. Paired device
cookies grant OpenCode access only, not administration or publication.

| Command | Role | Workspace lease |
| --- | --- | --- |
| `init` | Generate local demo config and protected secrets | None |
| `doctor` | Check Docker, gateway, GitHub, Tailscale, and phone route | None |
| `up` | Long-running supervisor and proxy | Exclusive writer |
| `github publish` | Push exact committed `HEAD` and create/reuse one draft PR | Exclusive; service must be stopped and compute absent |
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

All `/fern/*` routes are handled before workspace admission and do not wake
compute. `/fern/` is the paired-device landing page; `/fern/control` and control
APIs require Fern control authentication. Every coding route remains owned by
OpenCode. The lifecycle-owned publication coordinator
holds request admission and lifecycle wake serialization closed after stopping
compute, records the exact commit and branch before push, obtains an existing
`gh` token only in host memory, and permits only a validated `github.com` origin
and Fern-owned branch. The standalone command instead acquires the workspace
lease and requires absent compute. The intended GitHub App broker remains
unimplemented.

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

Credentials are checked before request admission or wake. OpenCode Basic auth
is forwarded to OpenCode. A valid device cookie is stripped and translated to
the internal OpenCode credential. Fern control credentials are stripped and
are never accepted on OpenCode routes.

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
5. Authenticate and query V2 active sessions, PTYs, permissions, and questions.
6. Repeat the complete activity query while admission remains closed.
7. Stop only when every response in both passes is idle.

The activity reads are sequential, not an atomic OpenCode snapshot. The second
pass catches activity that begins during the first pass. Fern blocks new held
proxy requests throughout and fails closed on active, unknown, or unavailable
results, but OpenCode can still transition internally between reads.

## Container And Persistence

The workspace container is forced to UID/GID 1001 with all Linux capabilities
dropped and `no-new-privileges`. It has the configured memory limit, a two-CPU
quota, a 512-process limit, Docker init, no restart policy, one writable
repository bind mount, one writable OpenCode data volume, and a dynamically
assigned backend port published only on loopback. The Docker-visible spec
fingerprint contains environment key names, not secret values; full container
inspection still verifies exact values before reuse.

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
| Orderly Fern/service shutdown followed by Docker stop | Recovery intent makes the exited container resumable |
| Docker stops running compute without orderly Fern shutdown | `failed` |
| OOM or dead container | `failed` |
| Fern restart while compute remains running | Running container is adopted and the shutdown recovery intent is cleared |
| Failed or unknown stop | Pending intent retained and endpoint invalidated |
| Activity query error | Stop deferred; compute remains running |
| Desired-state drift | Reuse refused until explicit recreation |

SIGINT or SIGTERM cancels proxy, supervisor, event observation, and wake work
together, then records a container-specific recovery intent before releasing
Docker. Docker must stop the container within that five-minute intent window,
but the resulting stopped container remains recoverable after a longer offline
period. Ordinary committed idle-pause intents remain durable until resume or
explicit cleanup.
The HTTP server gets a bounded graceful shutdown, then Fern waits for its owned
manager operations before closing Docker. Stopping Fern itself does not stop
OpenCode, which allows service restart and reattachment but makes quiet backups
an explicit `fern down` operation.

The checked-in systemd unit has no lifecycle-mutating `ExecStop`. Its normal
ordering stops Fern before Docker, allowing Fern to record the short-lived
recovery intent.
The deterministic lifecycle harness simulates this order and resumes the
container. Abrupt power loss, forced Fern termination, Docker stopping first,
and a real target-host reboot remain separate failure/acceptance cases.

## Authentication And Network Boundaries

Fern accepts only numeric loopback proxy listeners. A separate private TLS edge
such as Tailscale Serve is required for remote use. Basic auth is not transport
security. Do not expose the current listener publicly.

The planned production chain is:

```text
TLS edge -> Fern HttpOnly device-cookie auth -> lifecycle admission
         -> injected internal OpenCode auth -> OpenCode UI/API
```

Pairing, durable cookie digests, expiry, device revocation, and Fern control
handlers are implemented. Pairing codes remain process-local and single-use.
GitHub App credentials and callbacks remain future work.

## Assurance And Limits

CI is expected to run formatting, unit and race tests, vet, binary build, the
single image build, the deterministic real-Docker lifecycle harness, and the
pinned OpenCode smoke test. Relevant local commands are `make image`,
`./scripts/test-lifecycle.sh`, and `./scripts/test-opencode.sh`.

Not established by checked-in evidence:

- production enrollment and account-recovery policy beyond current private
  single-user pairing;
- target-host reboot, Docker restart, backup, and restore rehearsal;
- remote browser acceptance on intended laptop and phone devices;
- provider-backed model turns across each supported provider;
- recovery of in-progress provider streams or tool work after process death;
- hostile multi-user or multi-tenant isolation.
