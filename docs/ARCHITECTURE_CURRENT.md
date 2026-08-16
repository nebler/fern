# Current Architecture

**Authority:** current first-party code and tests as of 2026-08-16. Historical
design and research documents are indexed in [DOCUMENTATION.md](./DOCUMENTATION.md).

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

Only local Unix-socket Docker endpoints are supported. Fern depends on
host-local bind mounts, loopback port publication, file locks, and pause-intent
state, so a remote Docker daemon is rejected before lifecycle mutation.

## Desired And Observed State

`runtime.Spec` contains the stable desired workspace name, image, repository,
memory, OpenCode protocol, and environment. The Docker implementation adds
fixed init, port, data-volume, CPU, and PID policy. A fingerprint is stored on
the container, and every wake/reuse verifies both the fingerprint and actual Docker
configuration.

Every mutable Docker resource must carry Fern's managed and workspace labels.
Mutations use the inspected immutable container ID, not an unverified reusable
name. Foreign resources and configuration drift are refused.

The observed lifecycle states are:

| State | Meaning |
| --- | --- |
| `absent` | No container exists. |
| `provisioning` | Created/restarting compute or an unresolved pending stop. |
| `running` | The process is running and not externally frozen. |
| `paused` | Fern committed an intentional stop, or Docker reports a frozen process. |
| `failed` | The process exited without a committed Fern pause, died, or was OOM-killed. |

Fern writes a pending pause intent before asking Docker to stop and commits it
only after a successful stop response. A failed stop response remains an
unknown outcome; Fern does not relabel a concurrent crash as a safe pause.

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
coalesces concurrent wakes into one Docker operation. It resolves health and
attaches the exact endpoint generation's activity observer before forwarding.
The lease remains held until reverse proxying finishes.

Backend endpoint generations prevent a failed old request from clearing a
newer endpoint. Transport failures and canceled held requests invalidate the
generation so the next request reconciles Docker state. Cancellation of an
observation-only SSE request does not invalidate healthy compute.

## Activity And Pause Protocol

The stream controller owns one monotonically increasing epoch per backend
endpoint. Connected, disconnected, status, malformed-status, and work-request
observations are serialized through the supervisor. Stale epochs cannot make a
new endpoint eligible to stop.

The supervisor requires a connected epoch to report a busy or retry state and
then drain all observed active sessions to idle. Disconnects, malformed or
unknown status, and requests that may start work cancel eligibility. Merely
starting Fern against an already idle process does not arm a stop timer.

When the timer expires, the manager performs a second authoritative barrier:

1. Reject pause if any held HTTP request is active.
2. Block admission of new held requests.
3. Serialize against wake and other lifecycle operations.
4. Reinspect Docker and resolve the current endpoint.
5. Query all protocol-specific activity surfaces with authentication.
6. Stop only if every response is valid and idle.

V1 uses `/session/status`. V2 requires all of `/api/session/active`,
`/api/shell`, `/api/pty`, `/api/permission/request`, and `/api/form/request` to
be idle. Any error or unknown state leaves compute running.

This protects requests using Fern's proxy. A same-host Docker administrator can
discover the backend's loopback port and bypass admission; that principal is
already inside the trusted-host boundary.

## OpenCode Protocols

| Concern | V1 | V2 |
| --- | --- | --- |
| Health | `/global/health` | `/api/health` |
| Events | `/event` or `/global/event` | `/api/event` |
| Auth | configurable Basic username/password | username `opencode`, `OPENCODE_PASSWORD` |
| Client | `opencode attach URL` | `opencode2 --server URL` |
| Data volume | `fern-<name>-data` | `fern-<name>-v2-data` |

`auto` probes both health contracts, requires exactly one to validate, and uses
`fern-<name>-auto-data`. Explicit protocol selection is preferred because it
catches image/protocol mismatches and keeps persistence intent clear.

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
has started, its data is retained even if later health setup fails.

Protocol changes require container recreation and never mount one protocol's
data into another. Fern does not migrate OpenCode databases.

## Shutdown And Supervision

SIGTERM cancels the proxy, supervisor, activity stream, and outstanding wake
ownership in a defined order. HTTP shutdown has a bounded graceful period and
then closes tracked connections. Pause reconciliation uses the caller's
context and cannot silently extend its deadline.

Stopping the Fern process does not stop OpenCode. This permits service restart
to reattach to running compute, but operational backups must stop the service
and run offline `fern down` before archiving repository and volume state.

The checked-in systemd unit supervises `fern up`; target-host reboot and Docker
restart behavior remains an explicit deployment acceptance task.

## Assurance And Limits

Unit, race, vet, formatting, image-build, and V1/V2 real-Docker lifecycle jobs
are defined in CI. The local lifecycle harness covers creation, authentication
before wake, concurrent wake, request/pause exclusion, endpoint replacement,
persistence, clean-exit and OOM classification, shutdown, and frozen-container
recovery. The real V2 smoke test covers the pinned beta artifact.

Not yet established by checked-in evidence:

- target-host systemd, reboot, Docker restart, backup, and restore rehearsal;
- remote laptop and phone access through the intended tailnet;
- provider-backed V2 model turns and upstream upgrade compatibility;
- recovery of in-progress provider or tool work after process death;
- hostile multi-user or multi-tenant isolation.
