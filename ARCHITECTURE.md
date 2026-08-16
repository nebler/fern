# Fern Architecture

> **Document status (2026-08-15):** This concise overview is retained for
> historical context but contains implementation details that are no longer
> exact and predates V2/auto support. Read
> [docs/ARCHITECTURE_CURRENT.md](./docs/ARCHITECTURE_CURRENT.md) for the
> current source-verified architecture, call stacks, diagrams, trust boundaries,
> failure paths, persistence model, and known limitations.

Fern is a native Go control process for one Docker-hosted OpenCode workspace. It keeps repository and session data durable, stops compute only after a safe idle boundary, and wakes compute before forwarding the next request.

For a file-by-file map, read [CODEBASE_GUIDE.md](./CODEBASE_GUIDE.md). For the foundation review and its resolution, read [CODE_REVIEW.md](./CODE_REVIEW.md).

## Boundary

```text
host
  fern
    strict config -> runtime.Spec
    exclusive workspace lease
    wake-before-forward proxy
    single-workspace lifecycle manager
    epoch-aware OpenCode watcher
    single-owner activity model + timer shell
    Docker runtime adapter
         |
         | Docker API
         v
  Fern-owned Docker container
    opencode serve :4096
    /home/user/workspace          host bind mount
    ~/.local/share/opencode      labeled named volume
```

Fern is not containerized. The container is disposable compute; the host repository and named OpenCode volume are durable state.

## Core Values

`runtime.Spec` is normalized desired state:

```text
workspace identity
image
absolute repository path
memory bytes
environment
server authentication
```

`runtime.Observation` preserves runtime facts instead of collapsing them:

```text
Fern phase
container identity and Docker status
running/frozen/OOM state
exit code
resolved endpoint
desired-spec fingerprint
```

`watch.Observation` preserves activity facts with time identity:

```text
connection epoch
connected/disconnected
session busy/retry/idle
request may start work
```

## Ownership

Fern uses two independent ownership checks:

1. `flock` under `~/.fern/locks` permits one Fern lifecycle writer for a workspace on the host.
2. Docker labels prove that a container or volume belongs to Fern and to the configured workspace.

Every Docker mutation verifies:

```text
dev.fern.managed=true
dev.fern.workspace=<name>
```

Containers also carry `dev.fern.spec=<sha256>`. A changed image, repository, memory limit, or environment produces a spec-drift error rather than silently resuming stale compute.

## Runtime States

| State | Meaning |
|---|---|
| `absent` | no container exists |
| `provisioning` | created or restarting |
| `running` | process is running and not freezer-paused |
| `paused` | intentionally stopped or freezer-paused |
| `failed` | dead, OOM-killed, or unexpectedly exited |

Failed compute is not automatically restarted. The user inspects logs and recreates it with `fern down` followed by `fern up`.

## Wake Path

```text
client       proxy       manager       watcher       Docker       OpenCode
  |            |            |             |             |             |
  | request    |            |             |             |             |
  |----------->| acquire request lease    |             |             |
  |            |----------->|             |             |             |
  |            |            | status/ownership/spec     |             |
  |            |            |-------------------------->|             |
  |            |            | start if stopped          |             |
  |            |            |-------------------------->|             |
  |            |            | authenticated health ----------------->|
  |            |            |<---------------------------------- 200 -|
  |            |            | resolve current endpoint   |             |
  |            |            |------------ reconnect ---->| /event      |
  |            |            |<----------- connected epoch|             |
  |            | endpoint   |             |             |             |
  |            |<-----------|             |             |             |
  |            | forward original request --------------------------->|
  |            |<-----------------------------------------------------|
  | response   | release request lease |             |             |
  |<-----------|            |             |             |             |
```

Concurrent requests share one manager-owned wake operation. Individual callers may stop waiting, but the wake remains owned by the Fern service context. Process shutdown cancels and joins it before Docker and the workspace lease are released.

## Request Admission

The proxy distinguishes two facts:

- whether a request must hold the workspace for its complete proxied lifetime;
- whether it may start asynchronous work.

OpenCode event endpoints do neither. Ordinary requests hold a lease. Non-read methods and WebSocket upgrades also invalidate the previous idle boundary before wake begins.

This is deliberately not a time-based grace period. A request that may admit work requires a fresh observed `busy -> all idle` sequence, even if its HTTP response returns before provider execution starts.

## Activity Model

`internal/watch/supervisor.go` keeps policy state under one goroutine owner:

```go
model.apply(observation) -> timer action
```

The model contains:

```text
current stream epoch
whether that epoch is connected
whether busy was observed in that epoch
active session IDs
```

Rules:

```text
connected(epoch)    -> select epoch, clear prior activity
disconnected(epoch) -> invalidate all pause eligibility
request             -> invalidate prior idle boundary
busy/retry(session) -> mark active and cancel timer
idle(session)       -> remove only if actually active
all active -> idle  -> arm timer
old epoch event     -> ignore
```

The timer is mechanism, not state. It may request pause; it cannot prove pause is safe.

## Pause Authorization

When the timer fires, the manager performs a second safety gate:

1. Lock request admission so no new held request can enter.
2. Require zero in-flight held requests.
3. Serialize against create/resume.
4. Re-inspect the Fern-owned runtime and current endpoint.
5. Query authenticated `/session/status`.
6. Require every reported session to be idle.
7. Stop with Docker `SIGTERM`.

If the watcher disconnects, a request starts, status is busy, status cannot be queried, or ownership is uncertain, Fern fails safe and leaves compute running.

All agent traffic must use the Fern proxy. Direct writes to Docker's loopback backend port bypass request admission and are outside this safety guarantee.

## Stream Generations

Docker may republish a stopped container on a new host port. `StreamController` assigns a monotonically increasing epoch to each endpoint generation.

- Wake never returns an endpoint until that exact generation's `/event` connection is live.
- Disconnect is published as policy input immediately.
- Reconnect publishes a fresh connected observation.
- Status events carry their epoch.
- Old-epoch events cannot arm pause.
- Malformed or unknown status events invalidate the stream attempt rather than being interpreted as idle.

## Persistence

| Data | Storage | Stop/start | `fern down` |
|---|---|---:|---:|
| repository | host bind mount | survives | survives |
| OpenCode SQLite | labeled named volume | survives | survives |
| desired spec fingerprint | container label | survives | removed with container |
| provider/tool execution | OpenCode memory | lost | lost |
| watcher/session model | Fern memory | rebuilt | rebuilt |
| workspace lease | kernel lock | process-local | process-local |

Fern stops only after durable turn completion. It does not claim mid-turn recovery or machine-power-loss durability for SQLite's newest WAL tail.

## Authentication

`runtime.ServerAuth` is one explicit value shared by:

- health polling;
- SSE connection;
- authoritative session-status checks.

Provider and OpenCode credentials can enter the container through environment values. Fern listens on loopback by default. Remote exposure still requires Tailscale or an identity-aware proxy.

## Shutdown

The proxy server and supervisor run under one `errgroup` service context.

```text
signal or component error
  -> cancel service context
  -> stop accepting requests
  -> await HTTP Shutdown
  -> await manager lifecycle operation
  -> stop stream controller
  -> close Docker client
  -> release workspace lease
```

No shared wake operation uses an unowned `context.Background()` lifetime.

## Image Reproducibility

The workspace image pins:

- the official Node 22.23.2 Bookworm image by digest;
- OpenCode 1.18.16;
- architecture-specific OpenCode archive SHA-256 values.

The image does not execute the mutable OpenCode or NodeSource installer scripts. Debian package repository contents can still change between builds; fully hermetic apt snapshots are future work.

## Kubernetes Mapping

The policy values survive a Kubernetes backend:

| Docker | Kubernetes |
|---|---|
| owned container labels | owner references and labels |
| named volume | PersistentVolumeClaim |
| dynamic host port | Service endpoint |
| stop/start | replicas 0/1 |
| file lease | leader election and keyed reconcile ownership |
| runtime observation | Deployment/Pod/PVC observation |

The controller should preserve rich observations and the same fail-safe activity epochs rather than reducing Kubernetes reality directly to a phase string.
