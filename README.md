# fern

Self-hosted OpenCode workspaces that stop when idle and wake on the next HTTP request.

`fern` runs natively on the host. It controls a Docker container containing `opencode serve`, watches OpenCode's SSE activity stream, and exposes a stable reverse-proxy address to clients.

## Current Status

The Docker implementation is functional:

- creates an 8 GiB-limited OpenCode workspace from `fern/opencode:dev`;
- bind-mounts the selected host repository into `/home/user/workspace`;
- persists OpenCode sessions in the `fern-<workspace>-data` Docker volume;
- verifies Fern ownership before every container or volume mutation;
- rejects desired configuration drift instead of resuming stale compute;
- tracks connected epochs and busy, retry, and idle state across all sessions;
- invalidates idle eligibility on watcher loss or a request that may start work;
- stops OpenCode only after zero held requests and an authenticated all-idle snapshot;
- wakes a stopped workspace when a request reaches the proxy;
- coalesces concurrent wake requests into one Docker operation;
- streams SSE without response buffering;
- prevents concurrent lifecycle writers with a cross-process lease;
- distinguishes failed/OOM compute from an intentional pause.

Kubernetes, setup snapshots, resume hooks, ingress, and the credential proxy are not implemented yet.

## Documentation

- [CODEBASE_GUIDE.md](./CODEBASE_GUIDE.md): detailed package map, lifecycle traces, state ownership, invariants, and the current-versus-simpler architecture.
- [CODE_REVIEW.md](./CODE_REVIEW.md): prioritized Go and Rich Hickey-style review findings.
- [ARCHITECTURE.md](./ARCHITECTURE.md): concise system design and future Kubernetes mapping.
- [IMPLEMENTATION.md](./IMPLEMENTATION.md): exact completion and verification record.
- [DAY-1.md](./DAY-1.md): OpenCode persistence and turn-boundary research.

## Requirements

- Go 1.24+
- Docker with at least 8 GiB available
- an API key supported by OpenCode, such as `ANTHROPIC_API_KEY`

## Quick Start

```bash
docker build -t fern/opencode:dev images/opencode
cp fern.example.yaml fern.yaml
go run ./cmd/fern up
```

The command remains in the foreground because it owns the watcher, idle supervisor, lock, and proxy. It prints output similar to:

```text
workspace: demo
direct: http://127.0.0.1:49153
proxy: http://127.0.0.1:8080
ready in: 1.4s
```

Connect clients to the stable proxy URL, not the direct Docker port:

```bash
curl -s http://127.0.0.1:8080/global/health | jq
curl -N http://127.0.0.1:8080/event
```

`fern up` forwards these host variables when present:

- `ANTHROPIC_API_KEY`
- `OPENAI_API_KEY`
- `OPENCODE_SERVER_USERNAME`
- `OPENCODE_SERVER_PASSWORD`

Additional environment values can be declared under `workspace.env` in `fern.yaml`. Values support required shell-style expansion such as `${SOME_KEY}`; startup fails if a referenced variable is missing. YAML fields are strict, so misspellings fail instead of silently using defaults.

## Commands

```bash
go run ./cmd/fern up
go run ./cmd/fern status
go run ./cmd/fern resume
go run ./cmd/fern logs
go run ./cmd/fern debug events
go run ./cmd/fern down
```

Common flags override file configuration:

```bash
go run ./cmd/fern up \
  -name demo \
  -repo /absolute/path/to/repository \
  -memory 8Gi \
  -idle 10m \
  -listen 127.0.0.1:8080
```

Configuration precedence is flag, then YAML file, then default. `-config` or `--config` selects another YAML file. An explicitly selected missing file is an error.

Changing the image, repository, memory, or environment of an existing container produces a spec-drift error. Run `fern down`, then `fern up`; the named session volume is retained.

`down` removes the container but deliberately retains the named OpenCode data volume. A later `up` recreates compute around the same durable session data. Remove that volume manually only when the session data is no longer needed:

```bash
docker volume rm fern-demo-data
```

## Development

```bash
make format
make test
make test-race
make vet
make build
make image
```

## Safety Boundary

Fern stops only after a currently connected watcher epoch has reported busy followed by every active session becoming idle. A disconnect or a request that may start work invalidates that boundary. When the timer expires, Fern blocks new held requests and independently confirms `/session/status` is all idle before stopping. Retry is busy; unknown state leaves compute running.

This boundary matters because OpenCode reconstructs completed conversation state from SQLite, but active provider streams, tool execution, partial streamed fragments, and permission waiters live in process memory. Stopping mid-turn can silently abandon work. The source and crash-test evidence is in [DAY-1.md](./DAY-1.md).

The guarantee assumes clients use the stable Fern proxy. Direct writes to Docker's loopback backend port bypass request admission and are for diagnostics only.

## Non-Goals

- OIDC workload identity issuer
- Multiplayer or shared sessions
- Webhook event queue
- Multi-tenant authorization
- A custom UI
- Multi-cloud runtime support
- Firecracker orchestration
- Mid-turn crash recovery

The proxy listens on loopback by default. Do not expose it beyond localhost without an external authentication layer such as Tailscale or an identity-aware proxy.
