# Fern

Fern runs disposable OpenCode jobs on your Docker host and retains exact Git
results after their compute is removed.

The primary interface is the `@fern/opencode` plugin. It submits a clean local
Git revision to Fern, follows the run, opens the exact read-only live OpenCode
session, stops it, or explicitly seals its work. Paired devices and the local
operator can also inspect, interrupt, steer, or perform a cold writable
takeover and handback through Fern's run-control deck. Fern owns the durable
receipt, runtime identity, writer fence, artifact, verification record, and
GitHub App publication journal.

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
- OpenCode `1.18.16` for the plugin

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

The repo-local plugin supports native `run`, `runs`, `open`, `stop`, `seal`,
`result`, and `disconnect` actions. See `plugins/opencode/README.md` for its
installation, credential, and HTTP contract.

The plugin authorizes against Fern with fixed scopes:

```text
run:create run:read run:stop run:open run:result
```

Fern stores plugin credentials in the operating-system keyring and stores only
credential digests server-side.

## Human Access

The live OpenCode route is read-only: Fern permits `GET` and `HEAD` and rejects
mutations and upgrades before they reach OpenCode. Paired devices and the local
operator use `/fern/runs` for durable warm interrupt/steer controls and a
networkless, credential-free read-only inspector terminal.

Writable access is a cold ownership transfer. Fern first closes and drains the
agent route and inspector, stops and removes the exact agent process epoch, and
deletes its disposable OpenCode volume. Only then does it start a networkless,
credential-free PID 1 Bash container with the run clone mounted writable. On
handback Fern drains and removes that human writer, captures a Git boundary,
starts a fresh OpenCode volume/container/session, submits a re-read/resume
prompt, and restores the read-only agent route under a higher writer generation.
Ambiguous evidence leaves all routes closed.

## Durable State

Fern keeps:

- taskstore schema 1 records, receipts, claims, ownership transfers, control
  journals, and actor snapshots;
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
trust boundaries, evidence, recovery, deployment, backup, release gates, and
the cold writer-ownership transfer.
