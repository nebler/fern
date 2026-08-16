# OpenCode V2 Compatibility

Fern supports OpenCode V1 and the pinned V2 beta as separate workspace
protocols. V1 remains the default and the recommended phone-test path. V2 is
opt-in because its upstream API and package are still marked beta.

## Configuration

Select the protocol explicitly:

```yaml
workspace:
  opencode: v2
  image: fern/opencode-v2:dev
  env: {}
```

Set `OPENCODE_PASSWORD` in the host environment or a protected service
environment file. V2 fixes the Basic-auth username to `opencode`. Do not put
the password in YAML or a command argument.

`workspace.opencode` accepts `v1`, `v2`, or `auto`. Auto requires both V1 and V2
passwords and probes authenticated
`/api/health` and `/global/health`; exactly one contract must validate. It
fails if neither validates or both validate. The detected protocol is scoped
to the current backend endpoint generation. Explicit selection is preferred
for deployment because it catches an image/protocol mismatch before clients
connect.

When no image is configured, `v2` selects `fern/opencode-v2:dev`; V1 and auto
retain the V1 default image. `fern up` also accepts `-opencode v1|v2|auto`.

## Pinned Artifact

`images/opencode-v2/Dockerfile` installs
`@opencode-ai/cli@0.0.0-next-17444`. The package was exercised on Debian
`linux/arm64` and `linux/amd64`. It starts with:

```text
opencode2 serve --hostname 0.0.0.0 --port 4096
```

The image fixes `OPENCODE_DB` to
`/home/user/.local/share/opencode/opencode-v2.db`, inside Fern's persistent
volume. An upstream pin change requires rerunning the V2 smoke and lifecycle
tests before deployment.

All Fern workspace containers, including V2, are constrained by the configured
memory limit plus fixed limits of two CPUs and 512 processes. Changing those
runtime limits on an existing container is treated as specification drift.

## Protocol Mapping

| Capability | V1 | V2 |
| --- | --- | --- |
| Health | `/global/health` | `/api/health` |
| Events | `/event`, `properties` envelope | `/api/event`, `data` envelope |
| Foreground activity | `/session/status` | `/api/session/active` |
| Client | `opencode attach URL` | `opencode2 --server URL` |
| Password | `OPENCODE_SERVER_PASSWORD` | `OPENCODE_PASSWORD` |
| Volume | `fern-<name>-data` | `fern-<name>-v2-data` |

Fern consumes V2 `session.status` events with the same conservative
busy/retry-to-idle policy used for V1. Immediately before stopping, while new
work requests are blocked, Fern checks all process-local V2 work surfaces:

- foreground session drains from `/api/session/active`;
- running non-interactive commands from `/api/shell`;
- running PTYs from `/api/pty`;
- pending permission requests from `/api/permission/request`;
- pending forms from `/api/form/request`.

Any active item, malformed response, unknown state, authentication failure, or
unavailable endpoint leaves compute running. The extra shell check is required
because a V2 shell command does not appear in `/api/session/active`.

These five reads are a conservative sequence, not one atomic upstream snapshot.
Fern blocks new held proxy requests while sampling and fails closed, but
OpenCode internal state can transition between reads.

## State Isolation

Fern never mounts the V1 data volume into V2. Changing protocol changes the
desired-spec fingerprint and requires `fern down` before recreation. Both
volumes are retained, so reverting to V1 does not overwrite or migrate V1
state. There is no automatic V1-to-V2 database migration.

Back up the relevant named volume before any manual migration or upstream V2
upgrade. An `auto` workspace uses `fern-<name>-auto-data`, also isolated from
both explicit protocol volumes. Do not reuse an auto workspace with a mutable
image tag that can change between V1 and V2: detection occurs only after the
shared auto volume is mounted. Explicit mode is required for strict V1/V2
persistence isolation.

## Verification

Run the protocol-specific checks with local Docker:

```bash
make image-v2
FERN_V2_IMAGE=fern/opencode-v2:dev ./scripts/test-opencode-v2.sh
FERN_LIFECYCLE_PROTOCOL=v2 ./scripts/test-lifecycle.sh
```

The real V2 smoke test covers authenticated health, SSE connection, shell
activity, the official `opencode2 api --server` client, container recreation,
and SQLite persistence. The deterministic lifecycle harness covers Fern's full
13-scenario lifecycle policy and ten stopped-to-ready wakes. Provider-backed
model turns remain an external, credentialed acceptance test.
