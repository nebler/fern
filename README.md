# fern

Self-hosted OpenCode workspaces that stop when idle and wake on the next HTTP
request.

Fern runs on the host, supervises the pinned OpenCode server in Docker, observes OpenCode
activity, and exposes a stable wake-aware reverse proxy. OpenCode serves its
official, complete web UI at the server root, so opening Fern's proxy origin in
a browser is the primary client experience.

Fern is V2-only. It does not build a coding PWA, fork or consume
`@opencode-ai/app`, or reimplement OpenCode screens. OpenCode remains the
authority for the UI, sessions, configuration, providers, permissions and
forms, terminals, files, and diffs. Fern owns workspace lifecycle and is the
future home of remote delivery, notifications, publication, and recovery.

## Current Status

The Docker lifecycle and reverse proxy are functional:

- one constrained `fern/opencode:dev` workspace container;
- a writable repository mount at `/home/user/workspace`;
- durable OpenCode state in `fern-<workspace>-v2-data`;
- ownership and desired-state drift checks before lifecycle mutations;
- authenticated activity observation and conservative idle stopping;
- request admission, concurrent wake coalescing, and same-origin proxying;
- streaming SSE and upgraded connections without response buffering;
- intentional-stop, failure, and OOM classification;
- a host-local lease preventing concurrent lifecycle writers.

The current proxy uses OpenCode Basic authentication. It validates the request
before waking compute and forwards the accepted credentials upstream. Use it
only behind a private TLS edge.

The production gateway design is not implemented yet. It will reserve
`/fern/*` for Fern-owned device pairing, administration, and GitHub callbacks,
authenticate a Fern-issued `HttpOnly` device cookie, and inject internal
OpenCode credentials when proxying all other routes. Pairing and that cookie
exchange do not exist today; `/fern/*` is a reserved namespace, not a current
user interface.

Fern is not yet a complete remote coding product. It does not currently provide
device pairing, durable task submission, notification delivery, repository
authorization, Git credential brokerage, branch or PR publication, complete
fresh-host restore, or automatic recovery after every host-reboot state.

## Documentation

- [Architecture](./docs/ARCHITECTURE.md): implemented boundaries, lifecycle, proxy, and future gateway boundary.
- [OpenCode](./docs/OPENCODE.md): the V2 server contract, official web UI, persistence, and verification.
- [Deployment](./docs/DEPLOYMENT.md): private systemd and Tailscale Serve runbook.
- [Remote product](./docs/REMOTE_PRODUCT.md): end-to-end acceptance gaps and roadmap.
- [GitHub integration](./docs/GITHUB_INTEGRATION.md): proposed host-side GitHub and publication boundary.
- [Lifecycle harness](./integration/lifecycle/README.md): real-Docker lifecycle test details.

## Requirements

- Go 1.24 or newer
- a local Docker daemon with at least 8 GiB available; remote `DOCKER_HOST` endpoints are rejected
- an API key supported by OpenCode, such as `ANTHROPIC_API_KEY`
- a non-empty `OPENCODE_PASSWORD`

## Quick Start

```bash
make image
cp fern.example.yaml fern.yaml
export OPENCODE_PASSWORD="$(openssl rand -hex 32)"
go run ./cmd/fern up
```

Fern stays in the foreground because it owns the proxy, watcher, idle
supervisor, and workspace lease. It prints the stable proxy origin, typically
`http://127.0.0.1:8080`.

Open that origin in a browser to use the official OpenCode web UI. Use the
OpenCode Basic username `opencode` and the value of `OPENCODE_PASSWORD`.
Clients must use the Fern origin rather than Docker's dynamic backend port so
requests can wake compute and participate in pause admission.

For diagnostics:

```bash
curl --user "opencode:$OPENCODE_PASSWORD" http://127.0.0.1:8080/api/health
curl --no-buffer --user "opencode:$OPENCODE_PASSWORD" http://127.0.0.1:8080/api/event
```

`fern attach` remains available for the official OpenCode terminal client. It
loads the configured proxy origin and credentials rather than connecting to the
container directly. The browser UI needs no Fern-built frontend.

Fern forwards supported provider variables from the host when present,
including `ANTHROPIC_API_KEY` and `OPENAI_API_KEY`. Additional values can be
declared under `workspace.env` in `fern.yaml`; `${NAME}` references are required
and startup fails if a referenced host variable is absent. Keep secrets out of
YAML and command arguments.

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

`up` is the long-running supervisor. `down` removes compute but deliberately
retains OpenCode state. `status --json` emits stable machine-readable state;
inspect its `state` field rather than treating a stopped or failed workspace as
a command invocation failure.

Common overrides are:

```bash
go run ./cmd/fern up \
  -name demo \
  -image fern/opencode:dev \
  -repo /absolute/path/to/repository \
  -memory 8Gi \
  -idle 10m \
  -listen 127.0.0.1:8080
```

Configuration precedence is flags, YAML, then defaults. Changing an existing
container's image, repository, memory, or environment produces a spec-drift
error. Run `fern down` and then `fern up`; `fern-<workspace>-v2-data` is retained.
Delete it only when its sessions and configuration are no longer needed:

```bash
docker volume rm fern-demo-v2-data
```

## Development

```bash
make format
make test
make test-race
make vet
make build
make image
./scripts/test-lifecycle.sh
FERN_OPENCODE_IMAGE=fern/opencode:dev ./scripts/test-opencode.sh
```

CI runs formatting, unit and race tests, vet, the binary and image builds, the
real-Docker lifecycle harness, and the pinned OpenCode smoke test. Build release
binaries and checksums from a clean tree with:

```bash
./scripts/build-release.sh v0.1.0
shasum -a 256 -c dist/SHA256SUMS
```

## Safety Boundary

Fern arms an idle timer only after a connected OpenCode event epoch reports
work and all observed activity drains. Disconnects, unknown states, and requests
that may start work invalidate that evidence. Before stopping, Fern blocks new
held requests and checks the V2 activity surfaces for sessions, shells, PTYs,
permissions, and forms. Any active, malformed, unauthorized, or unavailable
response leaves compute running.

This protects traffic using Fern's proxy. A host or Docker administrator can
discover the loopback backend and is already inside the trusted-host boundary.
Fern cannot recover process-local provider streams or tool execution after a
crash and does not claim mid-turn crash recovery.

Publish the loopback proxy only through a private TLS edge such as Tailscale
Serve. Current Basic auth is defense in depth, not transport security or the
future Fern device identity model.
