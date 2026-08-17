# OpenCode Integration

Fern supports one current OpenCode V2 server contract. There is no automatic
protocol mode, cross-version state migration, or second workspace image.

## Product Boundary

`opencode2 serve` provides the official complete OpenCode web UI at `/` together
with the APIs, SSE stream, and upgraded connections that power it. Fern proxies
that origin without replacing its frontend. In particular, Fern does not build
a custom coding PWA and does not consume, bundle, or fork `@opencode-ai/app`.

OpenCode owns:

- the browser and terminal UI;
- sessions and OpenCode configuration;
- provider setup and model interaction;
- permission requests and questions;
- terminals, files, and diffs.

Fern owns the container lifecycle, wake and pause admission, and the stable
same-origin proxy. Fern-owned `/fern/*` routes provide the phone control page,
gateway readiness, restart-safe device administration, workflow/session
correlations, and constrained publication without copying the coding interface.
Notifications and full delivery/recovery remain future services around
OpenCode.

## Image And Configuration

Build the single pinned image:

```bash
make image
```

The image is `fern/opencode:dev` by default and is defined by
`images/opencode/Dockerfile`. It runs `opencode2 serve` on container port 4096 as
UID/GID 1001 and stores OpenCode state below
`/home/user/.local/share/opencode`, which Fern mounts from
`fern-<workspace>-v2-data`. The image also points `XDG_CONFIG_HOME` inside that
volume so global OpenCode agents, commands, skills, plugins, models, and
permissions survive container recreation.

A minimal workspace configuration is:

```yaml
workspace:
  name: demo
  image: fern/opencode:dev
  repo: .
  memory: 8Gi
  env: {}
idle:
  after: 10m
proxy:
  listen: 127.0.0.1:8080
```

Set `OPENCODE_PASSWORD` in the host or protected service environment. OpenCode's
Basic-auth username is `opencode`. Do not put the password in YAML or command
arguments. Provider credentials can be forwarded through `workspace.env`.

## Proxy Contract

Clients use Fern's stable origin for every OpenCode route, including `/`. Fern
wakes stopped compute before forwarding ordinary UI and API requests, holds
pause admission while relevant requests are active, streams responses, and
preserves the origin expected by the official UI.

`/fern/`, `/fern/ready`, `/fern/pair`, and `/fern/pair/new` are the only current
Fern-owned HTTP routes. They do not acquire workspace admission or wake
OpenCode. All other paths, including similar names such as `/fern-smoke`, are
proxied unchanged.

The lifecycle integration uses these V2 surfaces:

| Purpose | Route |
| --- | --- |
| Health | `/api/health` |
| Events | `/api/event` |
| Foreground sessions | `/api/session/active` |
| Shells | `/api/shell` |
| PTYs | `/api/pty` |
| Permission requests | `/api/permission/request` |
| Forms | `/api/form/request` |

Unknown routes are treated as potential work because OpenCode may add routes
whose GET requests start execution. This also ensures root UI assets and
upgraded connections pass through the same wake-aware path.

Immediately before an idle stop, Fern blocks new held proxy requests and
samples all activity surfaces. The reads are conservative but are not one
atomic OpenCode snapshot. Any active item, malformed response, unknown state,
authentication failure, or unavailable endpoint leaves compute running.

## Authentication Roadmap

The implementation currently requires Basic auth at Fern and OpenCode. Fern
rejects invalid credentials before wake, then forwards the accepted
Authorization header upstream.

The intended production gateway is different:

- Fern-owned handlers live only under reserved `/fern/*` routes;
- pairing issues a Fern device credential in an `HttpOnly`, `Secure` cookie;
- the gateway authenticates that cookie before waking a workspace;
- Fern injects internal OpenCode authentication on proxied OpenCode requests;
- OpenCode continues serving every non-`/fern/*` UI and API route.

That gateway, device pairing, cookie issuance, and admin UI are proposed, not
implemented. Do not document current Basic auth as if it were the final device
identity boundary, and do not expose the current listener directly to the
internet.

## Persistence And Upgrades

`fern down` removes the workspace container but retains
`fern-<workspace>-v2-data`. A later `fern up` recreates compute around the same
OpenCode sessions and global configuration. Back up the volume before changing the
pinned OpenCode version. Fern does not transform or repair OpenCode's database;
OpenCode remains the data-format authority.

Changing the image or environment changes Fern's desired-state fingerprint and
requires container recreation. Use immutable image tags or recorded image
digests for deployment.

## Verification

Run the deterministic lifecycle harness and real pinned-OpenCode smoke test
with local Docker:

```bash
make image
./scripts/test-lifecycle.sh
FERN_OPENCODE_IMAGE=fern/opencode:dev ./scripts/test-opencode.sh
```

The lifecycle harness verifies creation, auth-before-wake, concurrent wake,
request/pause exclusion, endpoint replacement, persistence, failure and OOM
classification, shutdown, and frozen-container recovery. The OpenCode smoke
test verifies the actual image, authenticated V2 routes, official server
surface, activity, recreation, sessions, and global configuration. Provider-backed model turns
and target-browser acceptance remain credentialed external checks.
