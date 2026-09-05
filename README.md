# Fern

Fern runs disposable OpenCode jobs on your Docker host and retains exact Git
results after their compute is removed.

The primary submission interface is the `@fern/opencode` plugin. It submits a
clean local Git revision to Fern, follows the run, stops it, or explicitly seals
its work. Operators use `fern runs` and `fern attach` to connect the normal
OpenCode TUI to the exact live server and session. Fern owns the durable
receipt, runtime identity, artifact, verification record, and GitHub App
publication journal.

Fern does not maintain a persistent coding workspace and does not proxy a
general-purpose OpenCode server.

## Lifecycle

```text
plugin create
    -> durable admission
    -> isolated clone + volume + container
    -> exact OpenCode session + prompt
    -> working / needs_you
    -> user seal
    -> stop exact writer
    -> immutable Git bundle in local CAS
    -> remove route, container, volume, clone
    -> optional verification
    -> optional GitHub App draft PR
```

Every run is bound to an exact repository ID, remote, base commit, image ID,
execution profile, environment digest, session ID, message ID, and runtime
epoch. Capacity is intentionally one run at a time.

## Requirements

- Go 1.27 or newer
- local Docker over a Unix socket
- a qualified `opencode-background-source` image
- a GitHub App installation bound to one repository
- private HTTPS origins for the control plane and live-run route
- OpenCode `1.18.16` for the plugin and attachment

Remote `DOCKER_HOST` endpoints are rejected. Fern expects host-local bind
mounts, loopback routing, and coordination.

The current Docker bridge is not a sandbox for hostile repository code.
Repositories and the host network must be trusted. Background environment
injection, including provider API keys, is rejected until provider credentials
can stay in a host-side broker behind restricted egress; credential-bearing
remote providers are not supported by the current profile.

## Configure

Build the qualified source image:

```sh
make image-background-source
make test-background-qualification
BACKGROUND_IMAGE_ID=$(docker image inspect fern/opencode-background-source:dev --format '{{.Id}}')
```

Create configuration. The private origins must use the same hostname; the live
run origin must have an explicit non-443 port.

```sh
go run ./cmd/fern init \
  --repo /srv/fern/repository \
  --repository owner/repository \
  --repository-id 123456789 \
  --model-provider replace-with-credential-free-provider \
  --model replace-with-model-id \
  --background-image-id "$BACKGROUND_IMAGE_ID" \
  --remote-origin https://fern-host.example.ts.net \
  --background-origin https://fern-host.example.ts.net:8443
```

`fern.example.yaml` contains the complete production shape. The qualified
source profile is
`source-39fb919a054190498f6d5b7985bde231f93ad7a6`. Image IDs are
architecture- and build-specific, so `init` requires the exact ID captured by
qualification on that host.

Publish only the remote listener and live-run listener through a private TLS
edge. Never publish the operator listener. The remote route must exist before
GitHub can return the App Manifest callback.

```sh
tailscale serve --bg http://127.0.0.1:8080
tailscale serve --bg --https=8443 http://127.0.0.1:8443
```

Start Fern in onboarding-only mode:

```sh
go run ./cmd/fern up --config fern.yaml --env-file fern.env
```

When no GitHub App credential is present, the operator control page exposes the
App Manifest onboarding flow. Create the App, install it on only the configured
repository, copy the numeric installation ID from GitHub's installation URL
into `workspace.github.installationId`, and restart Fern. Until that positive
ID and the credentials both exist, readiness is blocked and run/result requests
return `503`.

## Plugin

The repo-local plugin supports native `run`, `runs`, `stop`, `seal`, `result`,
and `disconnect` actions. See `plugins/opencode/README.md` for its
installation, credential, and HTTP contract.

The plugin authorizes against Fern with fixed scopes:

```text
run:create run:read run:stop run:attach run:result
```

Fern stores plugin credentials in the operating-system keyring and stores only
credential digests server-side.

## Attach to a run

List running sessions and attach the OpenCode V2 TUI:

```sh
fern runs
fern runs --json
fern attach                         # selects the only run or opens a picker
fern attach tsk_...
```

Those forms run on the Fern host and read the local configuration and protected
environment file. From another machine, use the private Fern origin:

```sh
fern runs --endpoint https://fern-host.example.ts.net
fern attach --endpoint https://fern-host.example.ts.net tsk_...
```

Remote commands reuse the Fern plugin credential in the operating-system
keyring, or `FERN_TOKEN` when explicitly supplied. An authenticated attach
request returns a random two-hour capability held only in memory and bound to
the current workspace, task, attempt, run generation, single-writer generation,
container process epoch, and OpenCode session. `fern attach` passes it to
`opencode attach <live-origin> --session <session-id> --pure` through the OpenCode
authentication environment. Route removal, process replacement, expiry, or
Fern shutdown revokes access. Fern permits the TUI's read traffic and exact
session interactions while rejecting session creation/deletion, cross-session
mutations, workspace management, credential management, and terminal upgrades.
Attachment never replaces the OpenCode process or volume and never transfers
filesystem ownership.

## Durable State

Fern keeps:

- taskstore schema 1 records, receipts, claims, and actor snapshots;
- paired-device and plugin authorization digests;
- GitHub App credentials;
- retained artifact CAS objects and materialized-result authority;
- verification and publication evidence;
- the host key used to identify disposable Docker resources.

Run clones, artifact work directories, publication checkouts, containers, and
volumes are disposable. This pre-release schema reset does not support older
development taskstore databases; delete and recreate them.

Offline backup and encrypted credential commands remain available:

```sh
fern backup create --output /secure/fern-backup
fern backup restore --backup /secure/fern-backup
fern backup rollback --recovery-dir /var/lib/fern/recovery
fern credentials export --recipient age1... --output credentials.age
fern credentials import --identity identity.txt --input credentials.age
```

The process must be stopped because these commands acquire Fern's repository
lease. Backups include configuration, repository, durable state, retained CAS,
and the disposable-runtime host key; they exclude run clones and scratch work.
Detected credentials are segregated into a separate backup artifact that must
be protected as secret material. The dedicated `fern credentials` export is
age-encrypted.

## Commands

```text
fern init
fern doctor
fern up
fern runs [--json]
fern attach [run-id]
fern backup create|restore|rollback
fern credentials export|import|rotate
fern debug quarantine-publications
fern version
```

`debug quarantine-publications` dispositions unresolved publication records
under explicit operator control.

## Development

```sh
make format
make test
make test-race
make vet
make build
make test-background-qualification
./integration/upgrade/run.sh
./integration/release/run.sh

cd plugins/opencode
bun install --frozen-lockfile
bun run format:check
bun run typecheck
bun test
```

`make lint` additionally requires `golangci-lint`.

## Documentation

`ARCHITECTURE.md` is the detailed system contract: components, state machines,
trust boundaries, evidence, recovery, deployment, attachment, backup, and
release gates.
