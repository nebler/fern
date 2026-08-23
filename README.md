# fern

> Self-hosted OpenCode workspaces that stop when idle and wake on the next HTTP
> request — your Docker host, your tailnet, your keys.

## Why

Coding agents are chained to whichever machine started them: close the laptop
and the work stops, and the "remote" alternatives mean renting someone else's
sandbox. fern is a single Go binary that runs agent workspaces on hardware you
own, puts an authenticated wake-aware reverse proxy in front of them, and makes
the phone a first-class control surface — dispatch a task over cellular, get
interrupted only when input is required, review and publish the result as a
draft PR.

## Demo

> **Demo (recording pending):** 60–90s phone-on-cellular flow — scan pairing QR,
> submit a durable task, watch the wake land, open the resulting draft PR. Will
> be embedded here as an animated GIF rather than a linked video.

<!-- Replace this block with ![fern phone demo](docs/demo.gif) once recorded.
     Reviewers do not click through to videos; an inline GIF at screen one is
     the unit that works. -->

## Measured numbers

| Metric | Value | Source |
| --- | --- | --- |
| Cold wake (stopped → healthy, proxied first byte) | ~2.8–3.1 s | 10-run lifecycle harness, `curl` `time_starttransfer` (`integration/lifecycle/artifacts/*/wake-timings.tsv`) |
| Warm wake (pre-thawed freezer pool) | planned: < 100 ms | roadmap — `todo/next-actions.md` (not yet implemented; current pause is graceful stop) |
| Traffic while idle | none — compute stopped | two-pass all-idle barrier (`internal/workspace`) |

Every wake above ~100 ms is logged with its duration; the harness measures real
Docker transitions against a deterministic fake OpenCode.

## Architecture

```text
      phone / laptop (tailnet)                 local operator CLI
               │  HTTPS via Tailscale Serve          │ loopback :8081
               ▼                                     ▼
    ┌─────────────────────────┐          ┌─────────────────────────┐
    │  remote listener :8080  │          │ operator listener :8081 │
    │  paired-device grants   │          │  Basic-auth policy      │
    └───────────┬─────────────┘          └───────────┬─────────────┘
                └──────────────┬─────────────────────┘
                               ▼
              request admission (observe / read / work)
                               ▼
       coalesced wake ──► docker resume or create ──► health + auth probes
                               ▼
             attested endpoint (loopback port, generation-stamped)
                               ▼
           opencode v2 serve — pinned image digest, unprivileged UID
                               ▼
      unbuffered SSE / streaming reverse proxy back to the client
```

The same ingress carries a durable task pipeline:

```text
phone submit ──► durable admission (idempotency receipt, caller-chosen IDs)
                      │ wakes compute only after commit
                      ▼
        exact-once delivery ──► fail-closed execution observation
                      ▼
   user-authorized seal ──► fenced verification runner ──► journaled draft PR
```

| fern owns | OpenCode owns |
| --- | --- |
| workspace lifecycle, wake/pause safety, admission | sessions, prompts, tools, terminals |
| durable task intent, receipts, cancellation fences | permissions, forms, provider calls |
| result sealing, verification policy, publication journals | the official UI, files, diffs |

## Scope

Fern is V2-only. It does not build a coding PWA, fork or consume
`@opencode-ai/app`, or reimplement OpenCode screens. OpenCode remains the
authority for the UI, sessions, configuration, providers, permissions and
forms, terminals, files, and diffs. Fern owns workspace lifecycle, paired-device
task admission, exact-once-attempted OpenCode delivery, cancellation,
conservative recovery projections, result provenance, verification machinery,
and repository-scoped GitHub App boundaries. Generic OpenCode terminal
classification and notifications remain incomplete.

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

Fern exposes two distinct loopback listeners. The remote listener accepts only
pairing capabilities and durable device cookies and is the sole Tailscale Serve
target. The operator listener accepts local `opencode:$OPENCODE_PASSWORD` for
the official CLI and host-only `fern:$FERN_CONTROL_PASSWORD` for administration.
Both Basic credentials are rejected remotely before wake, and Fern regenerates
backend auth only after admission. The control password never enters the
workspace container.

Fern reserves `/fern/*` for its phone landing and control plane. A paired
browser receives a restart-safe `HttpOnly` cookie; Fern stores only its digest
and injects the internal OpenCode credential only while proxying upstream. A
paired device can use OpenCode but cannot issue pairings, administer devices,
mutate workflows, or publish. Those operations require the operator listener's
`/fern/control` and the host-only Fern control credential.

Fern is not yet a complete remote coding product. Durable prompt delivery,
cancellation, App Manifest credential onboarding, and conservative execution
observation are implemented. The pinned OpenCode API still cannot prove generic
terminal success, so automatic result capture, verification, task publication,
notification delivery, installation/repository selection, complete fresh-host
restore, and abrupt-power-loss recovery remain incomplete. The old control-page
draft-PR flow remains a separate constrained host-credential prototype.

Fern's chosen GitHub direction is Amp-style unrestricted authenticated `gh`
inside the trusted workspace. An explicit request in an OpenCode prompt may
authorize the agent to push and create a draft PR; Fern does not claim that a
separate phone button can exclusively gate publication when OpenCode has the
same credential. The image and persistent workspace authentication needed for
that workflow are not implemented yet; the GitHub App sections below describe
the current brokered implementation.

## Documentation

- [Architecture](./docs/ARCHITECTURE.md): implemented authentication, lifecycle, proxy, publication, and persistence boundaries.
- [OpenCode](./docs/OPENCODE.md): the V2 server contract, official web UI, persistence, and verification.
- [Deployment](./docs/DEPLOYMENT.md): private systemd and Tailscale Serve runbook.
- [Phone field demo](./docs/FIELD_DEMO.md): exact preflight, phone sequence, evidence, abort, and cleanup checklist.
- [Product direction](./docs/REMOTE_PRODUCT.md): durable-task direction, boundaries, and sequencing.
- [Durable task model](./docs/TASK_MODEL.md): normative IDs, implemented persistence, state machines, reconciliation, and remaining fault gates.
- [GitHub integration](./docs/GITHUB_INTEGRATION.md): legacy host prototype plus repository-scoped App onboarding and transport boundaries.
- [Security boundary](./docs/SECURITY.md): current controls, release blockers, and parallel hardening tracks.
- [Lifecycle harness](./integration/lifecycle/README.md): real-Docker lifecycle test details.
- [OpenCode contract harness](./integration/opencode-contract/README.md): zero-cost exact-ID, restart, approval, and cancellation observations.
- [Task UI prototype](./integration/task-ui/README.md): read-only mobile inbox/detail contract fixtures.
- [Release harness](./integration/release/README.md): reproducibility, integrity, and deployment safety checks.

The files under [`product-docs/`](./product-docs/) are historical audits, not
current contracts.

## Requirements

- Go 1.24 or newer
- a local Docker daemon with at least 8 GiB available; remote `DOCKER_HOST` endpoints are rejected
- an account or provider connection supported by the pinned OpenCode release
- Tailscale on the host and phone for the private phone demo
- GitHub CLI authentication when the optional draft-PR prototype is configured

## Quick Start

```bash
make image
go run ./cmd/fern init --repo .
go run ./cmd/fern up --config fern.yaml --env-file fern.env
```

In another terminal, open the official OpenCode client and use its `/connect`
flow to connect an OpenCode account or any other provider supported by the
pinned release:

```bash
go run ./cmd/fern attach --config fern.yaml --env-file fern.env
```

This default is local-only. Before remote publication, set
`proxy.remoteOrigin` in `fern.yaml` to this host's exact canonical Tailscale
Serve HTTPS root, for example `https://fern-host.example.ts.net` with no slash
or explicit `:443`, then restart Fern. Alternatively pass that value to
`fern init --remote-origin`. Fern stays in the foreground because it owns both proxy listeners, the watcher,
idle supervisor, and workspace lease. It prints the remote/device origin,
typically `http://127.0.0.1:8080`, and operator origin, typically
`http://127.0.0.1:8081`.

In another terminal, publish Fern privately and create a one-time phone pairing
QR:

```bash
tailscale serve --bg http://127.0.0.1:8080
go run ./cmd/fern doctor --config fern.yaml --env-file fern.env --phone
```

Phone and field-demo diagnostics fail unless the configured origin exactly
equals both the root origin reported by `tailscale serve status` and this host's
tailnet HTTPS origin. `doctor --url` is only an exact assertion; it is not an
origin override. Only `proxy.listen` may be published.

Scan the QR within five minutes in the browser that should retain access. The
GET renders a confirmation page so scanner previews cannot consume the code;
tap **Pair this phone** to exchange it for a secure `HttpOnly` device cookie.
Fern opens the landing page; tap **Open OpenCode** to enter the official UI
without typing the generated Basic password. Pairing sessions survive Fern
restarts for up to 30 days and can be listed or revoked by an operator at
`http://127.0.0.1:8081/fern/control`. Active requests are canceled on revoke or
grant expiry.
Clients must use the Fern origin rather than Docker's dynamic backend port so
requests can wake compute and participate in pause admission.

For diagnostics:

```bash
curl --user "opencode:$OPENCODE_PASSWORD" http://127.0.0.1:8081/api/health
curl --no-buffer --user "opencode:$OPENCODE_PASSWORD" http://127.0.0.1:8081/api/event
```

`fern attach --env-file fern.env` remains available for the official OpenCode
terminal client. It loads the configured proxy origin and OpenCode credential
rather than connecting to the container directly. The browser UI needs no
Fern-built frontend.

Connect accounts and providers through the official OpenCode `/connect` flow;
OpenCode stores the connection in its persistent volume. The web UI exposes
the same OpenCode-managed provider state. Fern also implicitly forwards
`OPENCODE_PASSWORD` and optional provider variables such as
`ANTHROPIC_API_KEY` and `OPENAI_API_KEY`. Additional values
must be declared under `workspace.env` in `fern.yaml`; `${NAME}` references
resolve from the protected env file. `FERN_CONTROL_PASSWORD` and GitHub
credentials are host-only and rejected from workspace configuration.

## Commands

```bash
go run ./cmd/fern init --repo .
go run ./cmd/fern doctor --phone
go run ./cmd/fern up
go run ./cmd/fern github publish --dry-run --title "Describe the change"
go run ./cmd/fern attach
go run ./cmd/fern status
go run ./cmd/fern logs
go run ./cmd/fern version
go run ./cmd/fern debug events
go run ./cmd/fern down
```

`up` is the long-running supervisor. Starting it preserves absent or
intentionally paused compute; the first authenticated OpenCode request wakes or
creates the workspace. `down` removes compute but deliberately
retains OpenCode state. `status --json` emits stable machine-readable state;
inspect its `state` field rather than treating a stopped or failed workspace as
a command invocation failure.

Workspace GitHub access is disabled by default. To enable Amp-style `gh`, bind
the workspace explicitly; the ID is GitHub's positive numeric repository ID and
the full name is exact-case canonical `owner/repository`:

```yaml
workspace:
  github:
    mode: workspace-gh
    hostname: github.com
    repository:
      id: 123456789
      fullName: owner/repository
```

Authenticate and publish directly from the OpenCode terminal:

```bash
gh auth login --hostname github.com
gh auth setup-git --hostname github.com
git push --set-upstream origin HEAD
gh pr create --draft --base main --fill
```

Fern persists only this workspace's `gh` config in its dedicated Docker volume.
Run `gh auth status --hostname github.com` to inspect it and `gh auth logout
--hostname github.com` to remove it. Fern does not wrap ordinary push or PR
creation with a second publication API.

Durable tasks remain disabled unless a complete execution policy and explicit
GitHub authority are configured. Provider and model IDs are always explicit:

```yaml
workspace:
  env:
    OPENCODE_PASSWORD: ${OPENCODE_PASSWORD}
  github:
    mode: workspace-gh
    hostname: github.com
    repository:
      id: 123456789
      fullName: owner/repository
tasks:
  agent: build
  model:
    provider: replace-with-provider-id
    id: replace-with-model-id
  attemptTimeout: 30m
  leaseDuration: 2m
  budget:
    maxTurns: 100
  # Optional; no check is inferred when this block is absent.
  verification:
    checkName: repository-tests
    argv: [/usr/bin/make, test]
    workingDirectory: ""
    timeout: 15m
    environment:
      CI: "true"
    outputBytes: 65536
```

With `proxy.remoteOrigin` configured and no App credentials present, the
operator-only `/fern/control` page starts GitHub's App Manifest flow. Fern
validates configured installation/repository authority at task-service startup.
The verification command is shell-free, host-owned policy and runs only after a
sealed immutable result; the pinned OpenCode profile currently prevents Fern
from classifying generic terminal success automatically. Fern includes an
explicit quiesced result-sealing coordinator for a future authoritative success
observer, but does not activate it from idle or inactive session state.

When `installationId` is omitted, the separate operator-control prototype uses
the host's existing `gh` credential. It verifies the configured numeric
repository route, persists the exact base SHA/result commit/branch before
mutation, pushes without force, and persists an exact re-read of one open draft
pull request. Checkout `origin` is only a consistency diagnostic, never
publication authority. `fern github publish --dry-run` retains preparation
diagnostics, but standalone non-dry-run publication is rejected because it would
bypass the durable coordinator. This broad host-token transport is
prototype-only and is never initialized in GitHub App task mode.

Common overrides are:

```bash
go run ./cmd/fern up \
  -name demo \
  -image fern/opencode:dev \
  -repo /absolute/path/to/repository \
  -memory 8Gi \
  -idle 10m \
  -listen 127.0.0.1:8080 \
  -operator-listen 127.0.0.1:8081
```

Configuration precedence is flags, YAML, then defaults. Values explicitly set
in `workspace.env` override `--env-file`; selected process variables fill keys
that remain absent. Changing an existing container's image, repository, memory,
or environment produces a spec-drift error. Run `fern down` and then `fern up`;
`fern-<workspace>-v2-data` is retained.
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
make lint
make build
make image
./scripts/test-lifecycle.sh
FERN_OPENCODE_IMAGE=fern/opencode:dev ./scripts/test-opencode.sh
PYTHONDONTWRITEBYTECODE=1 python3 integration/opencode-contract/contract_harness.py
./integration/release/run.sh
go test ./integration/task-ui
```

CI runs formatting, unit and race tests, vet, the binary and image builds, the
real-Docker lifecycle harness, pinned OpenCode smoke test, and reproducible
release harness. Build release binaries, deployment assets, metadata, and
checksums from a clean tree with:

```bash
./scripts/build-release.sh v0.1.0
shasum -a 256 -c dist/SHA256SUMS
```

## Safety Boundary

Fern arms an idle timer only after a connected OpenCode event epoch reports
work and all observed activity drains. Disconnects, unknown states, and requests
that may start work invalidate that evidence. Before stopping, Fern blocks new
held requests and checks the V2 activity surfaces for sessions, PTYs,
permissions, and questions. Any active, malformed, unauthorized, or unavailable
response leaves compute running.

This protects traffic using Fern's proxy. A host or Docker administrator can
discover the loopback backend and is already inside the trusted-host boundary.
Fern cannot recover process-local provider streams or tool execution after a
crash and does not claim mid-turn crash recovery.

Publish only the remote/device listener through a private TLS edge such as
Tailscale Serve. Never publish the operator listener. The remote listener rejects
both Basic credentials and relies on selective Fern device grants; Basic auth is
not transport security.
