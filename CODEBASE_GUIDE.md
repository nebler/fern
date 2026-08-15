# Fern Codebase Guide

This is the detailed map of the current implementation: where each concept lives, who owns mutable state, and how a request moves through the system.

Read [ARCHITECTURE.md](./ARCHITECTURE.md) first for the concise model. Read [CODE_REVIEW.md](./CODE_REVIEW.md) for the foundation review and resolution.

## Repository Map

```text
cmd/fern/main.go                     CLI entrypoint and dispatch
cmd/fern/up.go                       long-running service composition
cmd/fern/commands.go                 lifecycle and diagnostic commands
cmd/fern/attach.go                   OpenCode client attachment
cmd/fern/helpers.go                  shared command plumbing

internal/config/config.go            strict file decoding and normalization
internal/registry/lock.go            host-local single-writer lease
internal/registry/intent.go          persisted intentional-pause identity
internal/runtime/runtime.go          desired and observed runtime values
internal/runtime/docker.go           Docker lifecycle mechanism
internal/runtime/health.go           authenticated OpenCode readiness
internal/workspace/manager.go        single-workspace lifecycle policy
internal/proxy/proxy.go              request admission and reverse proxy
internal/watch/event.go              frame-level SSE transport
internal/watch/controller.go         endpoint generations and typed observations
internal/watch/supervisor.go         single-owner activity model and timer shell
internal/watch/status.go             authoritative all-session-idle query

images/opencode/Dockerfile           pinned workspace image
fern.example.yaml                    user configuration example
Makefile                             development commands
```

## `cmd/fern`: Composition And Lifetime

The `cmd/fern` package is the imperative shell. `main.go` only dispatches; each command file composes the implementations it needs.

### `up`

The startup order deliberately moves validation before effects:

1. Parse flags and select `fern.yaml` or an explicit `--config` path.
2. Strictly decode YAML and merge explicit flag overrides.
3. Expand required environment references and normalize the repository path.
4. Validate name, image, memory, idle duration, repository, auth, and listen address.
5. Bind the TCP proxy listener.
6. Create the root signal context and `errgroup` service context.
7. Acquire the workspace lease.
8. Create Docker, the stream controller, and the workspace manager.
9. Ensure the workspace is healthy and its watcher generation is connected.
10. Create the supervisor and proxy, then serve until signal or component error.
11. Drain HTTP, join manager lifecycle work, stop the watcher, close Docker, and release the lease.

Binding step 5 before Docker means an occupied address cannot leave unexpected compute running.

### Other commands

| Command | Mutation | Lease |
|---|---:|---:|
| `attach` | launch local OpenCode client | not required |
| `down` | remove Fern-owned container | required |
| `resume` | start Fern-owned matching-spec container | required |
| `status` | observe only | not required |
| `logs` | observe only | not required |
| `debug events` | observe running endpoint only | not required |

Lifecycle commands use signal-aware 70-second contexts instead of unbounded backgrounds.

## `internal/config`: Text To Validated Data

`fileConfig` mirrors YAML and remains private. `Config` is the normalized application-facing value.

`Load`:

- allows a missing default `fern.yaml`;
- rejects a missing explicitly selected file;
- uses `yaml.Decoder.KnownFields(true)`;
- resolves relative repository paths against the config file directory;
- expands `$VAR` and `${VAR}`;
- treats `$$` as an escaped literal dollar;
- fails if a referenced environment variable is absent;
- parses the final selected idle duration.

`LoadWorkspace` strictly reads only the workspace section for `resume`, so broken idle or proxy settings cannot block runtime recovery. `LoadClient` reads only name, proxy address, and authentication values for attach and event diagnostics.

`Validate` checks all side-effect prerequisites.

`ParseMemoryBytes` converts once to bytes:

| Input | Meaning |
|---|---:|
| `8Gi` | 8 binary GiB |
| `2GB` | 2 decimal GB |
| `512Mi` | 512 binary MiB |
| bare integer | MiB for CLI compatibility |

Overflow is rejected before Docker receives the value.

## `internal/registry`: Host Writer Ownership

`Acquire` hashes the workspace name into `~/.fern/locks/<hash>.lock` and obtains non-blocking `flock`.

The lease is the open descriptor. Kernel close releases it after normal exit or process death. Host and PID are diagnostics, not ownership itself.

Only `EWOULDBLOCK` and `EAGAIN` mean contention. Filesystem and metadata failures remain distinct errors.

This lease protects Fern processes from each other. Docker labels separately protect foreign containers and volumes from Fern.

`IntentStore` writes an atomic record under `~/.fern/state` before Fern stops a container. The record includes the immutable container ID and a pending/committed phase. An exited container is `paused` only with a committed intent, recoverable `provisioning` with a pending intent, and otherwise `failed`. Resume and destroy clear the record.

## `internal/runtime`: Mechanism Boundary

### Values

`ServerAuth` is one authentication value shared by every OpenCode control request.

`Spec` is desired state:

```go
type Spec struct {
    Name        string
    Image       string
    RepoPath    string
    MemoryBytes int64
    Env         map[string]string
}
```

`Spec.ServerAuth` derives authentication from the two OpenCode server variables in `Env`.

`Observation` is mechanism reality:

```text
phase, container ID, Docker status
running, frozen, OOM, exit code
endpoint and whether it exists
spec fingerprint
```

`Endpoint` is returned after every create/resume. It is never treated as permanent identity.

### Interface

```go
type Runtime interface {
    Create(context.Context, Spec) (Endpoint, error)
    Pause(context.Context, string) error
    Resume(context.Context, Spec) (Endpoint, error)
    Destroy(context.Context, string) error
    Status(context.Context, string) (Observation, error)
}
```

The interface stays lifecycle-only. Hook execution should become a separate capability instead of adding `Exec` here.

## `internal/runtime/docker.go`: Docker Facts And Effects

### Create

1. Validate required spec values.
2. Inspect or create the labeled persistent volume.
3. Reject a same-named foreign volume.
4. Compute a deterministic spec fingerprint from sorted desired values.
5. Create a labeled container with loopback dynamic port, memory bytes, Docker init, repo bind, and data volume.
6. Start it.
7. Re-inspect the endpoint.
8. Wait for authenticated health.

### Observe

`Status` first verifies container labels. It then preserves Docker status, exit, OOM, frozen state, endpoint, and fingerprint in `Observation`.

Unexpected exits are `failed`, not `paused`. Intentional pause is proven by the persisted container-ID record rather than inferred from exit code.

### Resume

Resume rejects:

- absent containers;
- foreign containers;
- failed/OOM containers;
- desired-spec drift.

It also compares actual Docker image, memory, init, restart policy, configured environment, mounts, exposure, and port bindings against desired state so an out-of-band `docker update` cannot hide behind the label fingerprint.

It handles a freezer-paused or stopped owned container, then re-resolves endpoint and health.

### Destroy

Destroy verifies ownership, unfreezes if required, stops running compute, removes the container, and deliberately retains the labeled data volume.

## `internal/runtime/health.go`: Readiness

`WaitHealthy` polls `/global/health` every 200 ms with:

- a two-second timeout per HTTP attempt;
- an outer lifecycle timeout;
- `ServerAuth` Basic authentication.

Health answers only “can OpenCode serve traffic?” It does not answer “is all work idle?”

## `internal/watch/event.go`: SSE Transport

`Stream`:

- opens authenticated `/event`;
- supports 4 MiB frames;
- collects all `data:` lines until the SSE blank-line boundary;
- joins multiline payloads according to SSE rules;
- rejects malformed JSON and missing event types;
- emits generic OpenCode `Event` values.

`StreamForever` exists for `fern debug events`. Lifecycle control uses `StreamController`, because policy also needs connection epochs.

## `internal/watch/controller.go`: Connection Identity

The controller owns one `streamState`:

```text
epoch
base URL
currently connected
cancel function
completion channel
initial readiness channel
```

`Connect` reuses only a currently connected matching endpoint. Otherwise it replaces the generation. `Reconnect` always replaces it.

Each generation:

1. opens SSE;
2. publishes `connected(epoch)`;
3. filters generic events down to typed session statuses;
4. publishes every status with the same epoch;
5. publishes `disconnected(epoch, error)` on loss;
6. reconnects with bounded backoff;
7. treats malformed/unknown status as a failed observation attempt.

Failed initial connection clears the generation, allowing the next request to recover.

Only low-volume lifecycle observations enter the supervisor queue; token and tool events do not.

## `internal/watch/supervisor.go`: Functional Core

The supervisor goroutine exclusively owns the activity model:

```go
activityModel.apply(Observation) -> timerAction
```

The model is copied on each status transition. It contains current epoch, connection truth, busy history in that epoch, and active session IDs.

The reducer knows no Docker, HTTP client, timer object, or logger.

The `Supervisor.Run` shell translates timer actions into a Go timer and calls `OnPause` when it fires.

Important rules:

- disconnect clears all eligibility;
- reconnect starts a fresh epoch model;
- retry is active;
- unknown session idle does not arm;
- duplicate idle does not rearm;
- old epoch status is ignored;
- a request that may start work clears the old idle boundary.

## `internal/watch/status.go`: Final Idle Fact

`AllSessionsIdle` performs an authenticated, two-second-bounded `GET /session/status`.

OpenCode's status map contains active sessions; missing sessions are idle. Any busy/retry entry makes the result false. HTTP or decode failure is an error, which leaves compute running.

This snapshot is intentionally separate from SSE:

- SSE identifies a turn boundary and drives the timer.
- The snapshot confirms current truth at the stop decision.

## `internal/workspace`: Single Workspace Policy

`Manager` owns exactly one `Spec`; its methods do not accept arbitrary workspace names.

### Shared wake

`wakeMu` stores at most one `wakeCall`. Concurrent callers wait on its done channel. The operation is registered synchronously before its goroutine starts, derives from service context, and is joined by `Close`. If every caller leaves, the shared operation is canceled; a new live caller waits for that cleanup and retries with a fresh call.

### Request intent

`RequestIntent` separates three independent facts:

```go
type RequestIntent struct {
    Hold         bool
    MayStartWork bool
    MayWake      bool
}
```

Held requests increment `inFlight` before wake and release after proxy completion. Work-starting requests also emit a policy observation that invalidates a previous idle boundary. Event streams are non-waking: reconnects are rejected while compute is paused rather than immediately undoing the pause.

### Pause gate

`Pause` holds admission while it:

1. checks `inFlight == 0`;
2. serializes against wake;
3. observes current owned runtime;
4. checks the authoritative status snapshot;
5. invokes runtime stop.

No new held request can enter between status snapshot and stop.

### Observer handoff

Every running endpoint is handed to `StreamController`. A lifecycle transition forces a new epoch. If observer attachment fails after wake, the manager rolls the workspace back to stopped.

## `internal/proxy`: Admission And Forwarding

The proxy classifies requests:

| Request | Hold | May start work | May wake |
|---|---:|---:|---:|
| `/event`, `/global/event`, `/api/event` GET | no | no | no |
| `/global/health`, `/session/status` GET/HEAD | yes | no | yes |
| all other requests | yes | yes | yes |

It acquires intent before wake, forwards the untouched method/headers/body, and releases after `ReverseProxy.ServeHTTP` returns.

`FlushInterval: -1` makes SSE and model output immediate.

## Lifecycle Traces

### Running request

```text
proxy -> acquire request intent
manager -> share/perform status + health + watcher check
proxy -> forward
proxy -> release intent after response
```

### Wake

```text
request waits
manager observes owned paused runtime and matching spec
Docker starts and returns new endpoint
controller replaces stream generation
new epoch connects
request forwards
```

### Pause

```text
current epoch observes busy -> all idle
timer fires
manager closes request admission
manager requires no held requests
manager observes owned running runtime
authenticated status snapshot says all idle
Docker stops
stream disconnect invalidates epoch
```

### Shutdown

```text
root signal/component error
service context canceled
HTTP server drains
supervisor exits
manager rejects new wakes and joins current wake
stream stops
Docker closes
lease releases
```

## State Ownership

| State | Owner | Durable |
|---|---|---:|
| repository | host filesystem | yes |
| OpenCode SQLite | labeled Docker volume | yes |
| desired spec | config/flags and process value | source-dependent |
| spec fingerprint | container label | container lifetime |
| runtime reality | Docker daemon | yes across Fern restart |
| manager lease | kernel descriptor | no |
| intentional pause | atomic host state file keyed by workspace and container ID | yes on host |
| wake call | manager | no |
| stream generation | controller | no |
| session activity model | supervisor | no |
| provider/tool execution | OpenCode process | no |

## Invariants

1. Only labeled Fern resources are mutated.
2. One Fern lifecycle writer exists per workspace.
3. Desired spec drift is explicit.
4. A request forwards only after health and current watcher attachment.
5. Concurrent callers share one wake.
6. Endpoint is re-resolved after every transition.
7. Watcher disconnect invalidates pause eligibility.
8. Work-starting request invalidates the previous idle boundary.
9. Pause requires zero held requests and an authenticated all-idle snapshot.
10. Retry is busy.
11. Failed/OOM compute is not silently resumed.
12. Shutdown owns and joins all lifecycle work before releasing the lease.
13. Container removal retains session state.
14. Streaming responses are not buffered.

These invariants hold for traffic through the Fern proxy. Direct backend writes are explicitly outside the admission guarantee.

## Testing Map

| Package | Coverage |
|---|---|
| config | unit semantics, strict projections, missing env, precedence, config-relative paths |
| registry | actual subprocess contention and release on process exit |
| runtime | authenticated health, fingerprints, pause-intent recovery, actual-spec drift, and foreign ownership |
| workspace | shared wake, cancellation, retry, request gate, status gate, observer order and rollback |
| watch | auth, multiline SSE, epochs, disconnect, request invalidation, reconnect recovery, status snapshot |
| proxy | request classification, first-byte SSE flush, and full-lifetime request lease |
| CLI | attach URL/auth environment and explicit-name config bypass |

Real Docker tests are recorded in [CODE_REVIEW.md](./CODE_REVIEW.md) and [IMPLEMENTATION.md](./IMPLEMENTATION.md).

## Extension Rules

- Preserve facts as values until policy deliberately discards them.
- Treat unknown ownership or activity as unsafe, never idle.
- Give every goroutine an owner context and join path.
- Keep desired and observed state separate.
- Do not use elapsed time as a substitute for knowable state.
- Add runtime capabilities as separate interfaces instead of widening lifecycle.
- Keep high-volume data out of coordination queues.
- Test pure transitions without sleeps; use timing only at transport boundaries.
