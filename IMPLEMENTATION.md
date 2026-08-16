# Implementation Record

Date: 2026-08-12

> **Document status:** Dated V1 foundation completion record. It predates the
> current protocol abstraction, V2 support, release/deployment work, and later
> lifecycle hardening. See [README.md](./README.md),
> [docs/ARCHITECTURE_CURRENT.md](./docs/ARCHITECTURE_CURRENT.md), and
> [docs/OPENCODE_V2.md](./docs/OPENCODE_V2.md).

## V1 Foundation Snapshot

Fern implements a safety-first Docker lifecycle for one OpenCode workspace.

### Desired and observed values

- Strict YAML and flag normalization occurs before external effects.
- Memory is represented as checked bytes.
- OpenCode auth is one explicit value shared by all control clients.
- Docker observation preserves identity, status, endpoint, exit code, frozen state, OOM state, and spec fingerprint.
- Runtime failure is distinct from an intentional Fern pause.

### Ownership

- Host-local `flock` enforces one lifecycle writer.
- Containers and volumes carry Fern managed/workspace labels.
- Every lifecycle mutation verifies those labels.
- Mutations use the verified immutable container ID, not the reusable name.
- Existing foreign resources are refused.
- Containers carry a deterministic desired-spec fingerprint.
- Actual Docker image, memory, init, environment, mounts, and port configuration are also inspected for drift.
- Image, repository, memory, environment, or implementation drift requires explicit recreation while retaining session storage.
- Intentional pauses are persisted by container ID under `~/.fern/state`; external clean exits are failures.

### Activity safety

- SSE is parsed by complete frames, including multiline data.
- Lifecycle receives only typed connection and session-status observations.
- Every endpoint generation has a monotonically increasing epoch.
- Disconnect invalidates all pause eligibility.
- Stale generations cannot overwrite a newer connection model.
- Old-epoch and duplicate-idle events cannot arm pause.
- Retry is busy.
- Requests that may admit work invalidate the previous idle boundary.
- Held requests remain counted through the full reverse-proxy response.
- Timer expiry drains already-queued observations before considering pause.
- Timer expiry triggers an authenticated `/session/status` snapshot under the request-admission gate.
- `null`, malformed, disconnected, unknown, or active state leaves compute running.

### Wake and shutdown

- Concurrent callers share one manager-owned wake call.
- Wake derives from the Fern service context, not a request or unowned background.
- Wake resolves health, current endpoint, and watcher attachment before forwarding.
- Partial create/resume and observer failure roll back with an independent bounded cleanup context.
- Proxy and supervisor run under one `errgroup`.
- Active request contexts inherit the service context.
- Shutdown drains HTTP and force-closes remaining or hijacked connections after the deadline.
- Shutdown closes request admission, awaits lifecycle work, stops the stream, closes Docker, then releases the lease.

### Docker image

- Node 22.23.2 Bookworm base is pinned by digest.
- OpenCode is pinned to 1.18.16.
- amd64 and arm64 OpenCode archives have pinned SHA-256 values.
- Mutable OpenCode and NodeSource installer scripts are not executed.
- Docker init ensures stop signals reach OpenCode as a normal child process.

## Commands

```text
fern up
fern attach
fern down
fern resume
fern status
fern logs
fern debug events
```

`up`, `down`, and `resume` participate in lifecycle ownership. `status`, `logs`, and `debug events` are observational. Emergency `down`, `status`, and `logs` can use an explicit `-name` even if unrelated full configuration is invalid.

## Automated Verification

```bash
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local go test -race ./...
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go build ./cmd/fern
docker build -t fern/opencode:dev images/opencode
```

Coverage includes:

- strict YAML, trailing-document rejection, and missing environment references;
- escaped literal dollar values;
- config-relative repository paths;
- decimal/binary memory units and overflow boundaries;
- authenticated health, status, and SSE;
- multiline SSE frames;
- connection epochs, stale epoch refusal, and disconnect invalidation;
- request invalidation of idle boundaries;
- endpoint replacement and reconnect recovery;
- authoritative busy and `null` refusal;
- full request lease lifetime;
- one shared concurrent wake;
- observer-before-forward and rollback on observer failure;
- stable and drift-sensitive spec fingerprints;
- persistent pause intent bound to container identity;
- foreign container refusal across create, pause, resume, and destroy;
- foreign existing-volume refusal and volume inspect/create race handling;
- unbuffered SSE proxy delivery;
- real cross-process lock ownership and release.

## Docker Integration Results

The hardened foundation was tested against Docker Desktop 28.0.1:

```text
authenticated health/create: pass
Fern container ownership labels: pass
Fern volume ownership labels: pass
foreign same-name container status/down refusal: pass
8 GiB byte-exact memory limit: pass
session persisted after stop/wake: pass
session persisted after container removal/recreation: pass
dynamic port produced a new watcher epoch: pass
5 concurrent wake requests: 5/5 HTTP 200 through one wake
spec drift (8 GiB -> 7 GiB) refused: pass
external SIGTERM exit classified failed and wake refused: pass
occupied proxy address created no container: pass
long-lived proxied SSE closed during Fern shutdown: pass
graceful Fern process shutdown: pass
test container and volume cleanup: pass
```

Observed authenticated request-driven wake was approximately 2.8-3.1 seconds on the test machine.

## Design Decisions

### Functional core, imperative shell

The supervisor goroutine exclusively owns the activity model and applies observations directly. SSE, timers, HTTP, Docker, and logging remain effects outside that policy transition.

### Fail safe

Unknown owner, disconnected watcher, malformed status, failed status query, in-flight request, active session, OOM, external exit, and unexpected exit all prevent automatic stop or restart.

### Stop instead of memory snapshot

Fern acts only at a durable turn boundary. Process memory is redundant there, and stop matches future Kubernetes replicas-zero semantics. Mid-turn recovery remains explicitly out of scope.

### State volume survives compute

`fern down` removes the container but retains `fern-<workspace>-data`. Recreate applies a new desired spec around the same OpenCode SQLite state.

## Remaining Work

- Provider-backed empirical permission-wait boundary test
- Warm setup snapshot and invalidation
- `.fern/setup` and `.fern/resume` hooks
- Kubernetes Workspace CRD and controller
- PVC, Service, ingress, and scale-to-zero
- In-cluster credential proxy
- Remote identity layer and phone test
- Full apt repository snapshot pinning
- Mid-turn crash recovery remains a non-goal
