# Phone Field Demo

This runbook is an operator checklist for a physical Fern demonstration. The
repository contains deterministic harnesses and an evidence recorder, but no
checked-in evidence establishes that a physical phone, reboot, replacement-host
restore, TLS/WSS, or ACL-negative rehearsal occurred.

Do not infer a prior rehearsal from this document. A completed run exists only
when an operator retains a redacted evidence bundle tied to the exact source,
binary, image, host, phone, and timestamps.

## Claim Boundary

The demo may exercise private Tailscale access, explicit pairing, the unchanged
OpenCode UI, durable task submission, user snapshot sealing, optional
verification, App publication or direct workspace `gh`, idle pause/wake,
restart continuity, and device revocation.

It does not prove hostile-host isolation, generic OpenCode terminal success,
provider budget enforcement, notification delivery, cross-domain backup
atomicity, fresh-host split-brain prevention, or organization-specific GitHub
policy.

## Before Demo Day

1. Freeze scope and review the exact source commit.
2. Use distinct `OPENCODE_PASSWORD` and `FERN_CONTROL_PASSWORD` values, a bounded
   provider account, and a disposable or reviewed private repository.
3. Select one bounded prompt with a known expected change and host verification
   policy.
4. Decide the GitHub mode. In App mode, verify installation/repository binding
   and use receipt-backed publication. In workspace-`gh` mode, explicitly accept
   that direct mutations are outside Fern's journal.
5. Prepare encrypted evidence storage and cleanup commands. Never retry an
   ambiguous external mutation manually.
6. Connect the provider through `fern attach` and OpenCode's `/connect` flow
   before final readiness checks.

## Abort Conditions

Abort if any mandatory local gate fails, the repository or GitHub identity is
wrong, Funnel is enabled, the image differs from the recorded digest/ID, the
provider account is unbounded, readiness is false, or evidence storage is not
ready.

## Record Identity

```bash
git status --short
git rev-parse HEAD
go run ./cmd/fern version
docker image inspect fern/opencode:dev --format '{{.Id}}'
tailscale status --json
tailscale serve status
```

For workspace-`gh`, also record the redacted result of `gh auth status`. For App
mode, record only App/installation/repository numeric identity, never private
keys or tokens.

## Local Gates

```bash
test -z "$(gofmt -l .)"
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
./scripts/test-critical-coverage.sh
./scripts/test-deployment.sh
./scripts/test-browser.sh
./integration/release/run.sh
./integration/upgrade/run.sh
./integration/production-rehearsal/run.sh self-test
./scripts/test-lifecycle.sh
FERN_OPENCODE_IMAGE=fern/opencode:dev ./scripts/test-opencode.sh
```

The production rehearsal self-test uses synthetic facts. It validates the
recorder and schema only. The lifecycle harness uses a deterministic fake
OpenCode surface, and the OpenCode contract harness uses a local fake provider;
neither is evidence of a paid provider turn or physical phone behavior.

## Start

```bash
make image
go run ./cmd/fern up --config fern.yaml --env-file fern.env
```

In another terminal:

```bash
tailscale funnel status
tailscale serve --bg http://127.0.0.1:8080
go run ./cmd/fern doctor --config fern.yaml --env-file fern.env \
  --field-demo --json
go run ./cmd/fern doctor --config fern.yaml --env-file fern.env \
  --field-demo
```

`--field-demo` checks local gateway behavior, exact Tailscale root mapping,
disabled Funnel, configured GitHub authority, an active OpenCode provider, and
remote rejection of backend Basic auth. It does not spend provider funds,
mutate GitHub, or establish real browser/WSS acceptance.

## Phone Sequence

1. Scan the five-minute QR in the intended browser. Confirm a scanner GET preview
   does not consume it, then tap **Pair this phone**.
2. Verify `/fern/` opens without Basic auth and the operator control page lists
   the new device. Its display name is self-asserted, not Tailscale identity.
3. Open the official OpenCode UI and exercise navigation, assets, SSE, and a
   terminal WSS connection through the exact private HTTPS origin.
4. Open the Fern task queue. Submit one bounded task and confirm it remains
   represented after reload. The browser persists one pending body and
   idempotency key until acceptance; if durable browser storage is unavailable,
   it must refuse to send.
5. Confirm the exact OpenCode session and resulting repository change. Exercise
   one permission/form only if the prepared prompt naturally requests it; do not
   treat vanished process-epoch input as accepted.
6. Review the clean committed snapshot and invoke seal preview/authorization.
   Record that the attempt becomes `superseded`, not `succeeded`.
7. If verification is configured, wait for the exact result verification and
   record only its state and digests, not output secrets.
8. In App mode, use an API-capable paired client to request publication for the
   exact successful verification and retain its idempotency key until acceptance;
   the embedded task page currently displays status but has no publication
   button. Verify one receipt-backed publication reaches one draft PR whose head
   equals the sealed result. In workspace-`gh` mode, use explicit ordinary
   `git`/`gh` commands and record that this effect is outside Fern receipts.
9. Wait for configured idle pause, then reopen and record wake latency and state
   continuity. Do not substitute deterministic harness timings for this physical
   observation.
10. Restart Fern and verify the paired grant, task/result/verification/
    publication snapshot, and OpenCode state remain available as applicable.
11. Revoke the phone from the loopback operator surface while a stream or
    terminal is active. Verify closure, later `401`, denied reconnect, and an
    unaffected authorized control device.

## Evidence

Record timestamps, source commit/version, binary checksum, image ID/digest,
service boot ID, phone OS/browser, pseudonymous tailnet identity, OpenCode
session ID, Fern task/result/verification/publication IDs, local commit, remote
branch SHA, draft PR identity, pause/wake timing, revocation timing, and redacted
screenshots/log digests. Never store prompts containing secrets, credentials,
cookies, tokens, private URLs with credentials, or raw command output.

For a full reboot/restore/TLS/WSS/phone/ACL exercise, use
`integration/production-rehearsal/run.sh` to record operator-supplied phase JSON.
The recorder does not perform those operations and its finalization means only
that the supplied observations passed schema and cross-phase validation.

## Cleanup

```bash
tailscale serve reset
go run ./cmd/fern down --config fern.yaml
```

Delete disposable branches/repositories and revoke temporary provider or GitHub
credentials. Retain only redacted evidence. List any resource that cannot be
removed as unresolved cleanup.

## Remaining External Gates

Real iOS Safari and Android Chrome, cellular/Wi-Fi transitions, Ubuntu systemd
boot, physical reboot, replacement-host restore, abrupt power loss, private-edge
TLS/WSS, provider billing/interruption, external credential revocation, and
organization-specific GitHub rules remain external acceptance obligations until
an operator records them against an exact build.
