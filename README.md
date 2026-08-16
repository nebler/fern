# fern

Self-hosted OpenCode workspaces that stop when idle and wake on the next ordinary HTTP request.

`fern` runs natively on the host. It controls a Docker container containing `opencode serve`, watches OpenCode's SSE activity stream, and exposes a stable reverse-proxy address to clients.

## Current Status

The Docker implementation is functional:

- creates a memory-limited, two-CPU, 512-PID OpenCode workspace from `fern/opencode:dev`;
- bind-mounts the selected host repository into `/home/user/workspace`;
- persists OpenCode sessions in protocol-isolated named Docker volumes;
- verifies Fern ownership before every container or volume mutation;
- rejects desired configuration drift instead of resuming stale compute;
- tracks connected epochs and busy, retry, and idle state across all sessions;
- invalidates idle eligibility on watcher loss or a request that may start work;
- stops OpenCode only after zero held requests and an authenticated all-idle snapshot;
- rejects invalid Basic credentials before request admission or workspace wake;
- wakes a stopped workspace when a request reaches the proxy;
- coalesces concurrent wake requests into one Docker operation;
- discards cached endpoints after every attempted runtime pause, including ambiguous failures;
- streams SSE without response buffering;
- prevents concurrent lifecycle writers with a cross-process lease;
- distinguishes failed/OOM compute from an intentional pause;
- rejects remote Docker endpoints because mounts, loopback routing, locks, and intent are host-local.

Kubernetes, setup snapshots, resume hooks, Fern-managed TLS/public ingress, and identity-aware authorization are not implemented yet.

## Documentation

- [docs/DOCUMENTATION.md](./docs/DOCUMENTATION.md): map of current, historical, and research documents.
- [docs/ARCHITECTURE_CURRENT.md](./docs/ARCHITECTURE_CURRENT.md): current end-to-end system model, state, trust boundaries, and limitations.
- [docs/DEPLOYMENT.md](./docs/DEPLOYMENT.md): supervised systemd and private Tailscale Serve deployment runbook.
- [docs/OPENCODE_V2.md](./docs/OPENCODE_V2.md): pinned V2 beta configuration, protocol mapping, state isolation, and verification.
- [integration/lifecycle/README.md](./integration/lifecycle/README.md): repeatable real-Docker lifecycle and timing harness.
- [todo/pre-phone/README.md](./todo/pre-phone/README.md): current delivery and external-evidence tracker.

## Requirements

- Go 1.24+
- A local Docker daemon with at least 8 GiB available; remote `DOCKER_HOST` endpoints are rejected
- an API key supported by OpenCode, such as `ANTHROPIC_API_KEY`

## Quick Start

```bash
docker build -t fern/opencode:dev images/opencode
cp fern.example.yaml fern.yaml
export OPENCODE_SERVER_PASSWORD="$(openssl rand -hex 32)"
go run ./cmd/fern up
# In another terminal:
go run ./cmd/fern attach
```

The command remains in the foreground because it owns the watcher, idle supervisor, lock, and proxy. It prints output similar to:

```text
workspace: demo
proxy: http://127.0.0.1:8080
ready in: 1.4s
```

Connect clients to the stable proxy URL, not the direct Docker port:

```bash
curl -s http://127.0.0.1:8080/global/health | jq
curl -N http://127.0.0.1:8080/event
```

`fern attach` loads the proxy address and OpenCode server credentials from the
same configuration, then starts the official OpenCode TUI with
`opencode attach <proxy-url>`. Credentials are passed through the child
environment rather than command-line arguments.

For a client-visible origin that differs from the listener, such as Tailscale
Serve, pass the explicit root origin:

```bash
fern attach -url https://host.tailnet.ts.net
```

`fern up` forwards these host variables when present. V1 requires
`OPENCODE_SERVER_PASSWORD`; V2 requires `OPENCODE_PASSWORD`; auto requires both:

- `ANTHROPIC_API_KEY`
- `OPENAI_API_KEY`
- `OPENCODE_SERVER_USERNAME`
- `OPENCODE_SERVER_PASSWORD`
- `OPENCODE_PASSWORD`

Additional environment values can be declared under `workspace.env` in `fern.yaml`. Values support required shell-style expansion such as `${SOME_KEY}`; startup fails if a referenced variable is missing. YAML fields are strict, so misspellings fail instead of silently using defaults.

## Commands

```bash
go run ./cmd/fern up
go run ./cmd/fern attach
go run ./cmd/fern status
go run ./cmd/fern logs
go run ./cmd/fern version
go run ./cmd/fern debug events
go run ./cmd/fern down
```

Common flags override file configuration:

```bash
go run ./cmd/fern up \
  -name demo \
  -image fern/opencode:dev \
  -opencode v1 \
  -repo /absolute/path/to/repository \
  -memory 8Gi \
  -idle 10m \
  -listen 127.0.0.1:8080
```

Configuration precedence is flag, then YAML file, then default. `-config` or `--config` selects another YAML file. An explicitly selected missing file is an error.

Changing the image, repository, memory, or environment of an existing container produces a spec-drift error. Run `fern down`, then `fern up`; the named session volume is retained.

OpenCode V1 remains the default. To test the pinned V2 beta, build
`images/opencode-v2`, set `workspace.opencode: v2`, and provide
`OPENCODE_PASSWORD`. V2 uses the isolated `fern-<workspace>-v2-data` volume and
`fern attach` launches `opencode2 --server <proxy-url>`. See
[docs/OPENCODE_V2.md](./docs/OPENCODE_V2.md).

`down` removes the container but deliberately retains the named OpenCode data volume. A later `up` recreates compute around the same durable session data. Volume names are `fern-<workspace>-data` for V1, `fern-<workspace>-v2-data` for V2, and `fern-<workspace>-auto-data` for auto detection. Remove only the volume for the configured protocol and only when its session data is no longer needed:

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
make image-v2
```

CI runs formatting, ordinary and race tests, vet, binary build, both workspace
image builds, and the V1/V2 real-Docker lifecycle matrix. Build versioned Linux
binaries and checksums from a clean working tree with:

```bash
./scripts/build-release.sh v0.1.0
shasum -a 256 -c dist/SHA256SUMS
```

The real-Docker harness is explicit because it creates isolated Docker
resources and retains redacted evidence on failure:

```bash
./scripts/test-lifecycle.sh
FERN_LIFECYCLE_PROTOCOL=v2 ./scripts/test-lifecycle.sh
FERN_V2_IMAGE=fern/opencode-v2:dev ./scripts/test-opencode-v2.sh
```

## Safety Boundary

Fern stops only after a currently connected watcher epoch has reported busy followed by every active session becoming idle. A disconnect or a request that may start work invalidates that boundary. When the timer expires, Fern blocks new held requests and independently confirms the selected protocol's activity snapshots are all idle before stopping. V1 checks `/session/status`; V2 checks foreground sessions, shells, PTYs, permissions, and forms. Retry is busy; unknown state leaves compute running.

This boundary matters because OpenCode reconstructs completed conversation state from SQLite, but active provider streams, tool execution, partial streamed fragments, and permission waiters live in process memory. Stopping mid-turn can silently abandon work. The source and crash-test evidence is in [DAY-1.md](./DAY-1.md).

The guarantee assumes clients use the stable Fern proxy. Docker still publishes a loopback backend port for Fern's host process, but Fern does not advertise it. A same-host principal with Docker inspection access can bypass request admission and is already inside Fern's trusted-host boundary.

## Non-Goals

- OIDC workload identity issuer
- Multiplayer or shared sessions
- Webhook event queue
- Multi-tenant authorization
- A custom UI
- Multi-cloud runtime support
- Firecracker orchestration
- Mid-turn crash recovery

The proxy accepts only numeric loopback listeners. Publish it through a private TLS edge such as Tailscale Serve; OpenCode Basic authentication remains defense in depth and is not a replacement for TLS.
