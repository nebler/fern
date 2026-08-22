# Phone Field Demo

This runbook validates the constrained Fern field-demo claim. It does not claim
GitHub App onboarding, notification delivery, durable prompt/cancel semantics,
hard provider budgets, hostile-host isolation, or fresh-host split-brain safety.

## Latest Rehearsal Status

The 2026-08-22 physical Android Chrome rehearsal is a successful product
observation, not complete release acceptance. It observed private Tailscale
access, explicit phone pairing, the unchanged OpenCode UI, provider-backed file
changes, tests and local commits, lock/reopen continuity, Wi-Fi-to-cellular
continuity, and Fern restart continuity.

It also found or left open the following gates:

- QR scanners may preview a GET before opening Chrome. Pairing now requires a
  confirmation POST, and the browser and lifecycle harnesses cover preview,
  confirmation, replay rejection, and restart survival. A final recorded gate
  run is still required from the integrated field-demo commit.
- Automatic idle pause did not occur even though OpenCode's activity surfaces
  later reported no active session, PTY, permission, or question. A reducer fix
  now restarts the full grace timer after a late browser request while retaining
  admission fencing and two authoritative activity snapshots. The isolated
  lifecycle harness now proves the restarted grace period, pause, wake, and
  preserved state; physical pause/wake confirmation remains open.
- Permission/question handling, operator publication, exact remote PR proof,
  and device revocation were not exercised.
- The complete evidence bundle required below was not retained, so the run
  cannot be promoted to release acceptance retroactively.

## Before Demo Day

1. Freeze product scope; do not add T3, Kubernetes, a new UI, or a new state
   model before this rehearsal.
2. Review and commit the complete hardening diff so the demonstrated source,
   binary, and image have one recorded identity.
3. Use distinct `OPENCODE_PASSWORD` and `FERN_CONTROL_PASSWORD` values, a
   bounded OpenCode provider account, and the intended GitHub account.
4. Prepare one bounded prompt and a disposable or reviewed private repository
   whose expected change and verification command are known in advance.
5. Run the local gates and one complete dry run before using the presentation
   phone or repository.
6. Prepare evidence storage and cleanup commands; never improvise a repeated
   publication after an ambiguous external result.
7. Start Fern, run `fern attach`, and use OpenCode's `/connect` flow to connect
   the bounded account or provider before the final field-demo readiness check.

## Abort Conditions

Do not begin phone testing if any local gate fails, the repository is not a
disposable or reviewed target, Tailscale Funnel is enabled, the OpenCode
provider account is not bounded, the GitHub credential belongs to the wrong
account, or the Fern/OpenCode image identity differs from the recorded build.

## Record Identity

```bash
git status --short
git rev-parse HEAD
go run ./cmd/fern version
docker image inspect fern/opencode:dev --format '{{.Id}}'
gh auth status --hostname github.com
```

Store this output with the demo evidence. Never store provider or GitHub tokens.

## Local Gates

Run all non-mutating and isolated gates before exposing the phone route:

```bash
test -z "$(gofmt -l cmd internal)"
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go build ./cmd/fern
./scripts/test-deployment.sh
./scripts/test-browser.sh
./scripts/test-lifecycle.sh
FERN_OPENCODE_IMAGE=fern/opencode:dev ./scripts/test-opencode.sh
```

The current pairing behavior is GET-confirm/POST-consume. The browser and
lifecycle scripts must continue to prove that scanner-style GET previews do not
consume the code, confirmation POST issues the cookie, replay is rejected, and
the device grant survives restart.

The lifecycle gate performs a checksum-verified destructive restore of isolated
repository, OpenCode volume, Fern control state, and configuration. It also
simulates an orderly Fern shutdown followed by a Docker stop and verifies
automatic recovery.

The former standalone real-GitHub gate is disabled because standalone mutation
cannot bypass durable coordinator persistence. Publication acceptance currently
uses deterministic host `git`/`gh api` fakes only. A future disposable live gate
must drive the configured running control coordinator and retain exact durable
tuple assertions before it can be documented here.

## Start The Demo

```bash
make image
go run ./cmd/fern up --config fern.yaml --env-file fern.env
```

In a second terminal:

```bash
tailscale status --json
tailscale serve status
tailscale funnel status
tailscale serve --bg http://127.0.0.1:8080
go run ./cmd/fern doctor --config fern.yaml --env-file fern.env --field-demo --json
go run ./cmd/fern doctor --config fern.yaml --env-file fern.env --field-demo
```

`--field-demo` requires the local gateway, exact Tailscale root route, disabled
Funnel, host GitHub authentication, and at least one active provider reported
by OpenCode. Use `fern attach` and OpenCode's `/connect` flow before running
this check. It also requires the remote listener to reject the backend Basic
credential and rejects any Serve mapping to `proxy.operatorListen`. It does not
spend provider funds or mutate GitHub.

It also requires nonempty `proxy.remoteOrigin` to exactly equal both the root
Serve origin and this host's local tailnet HTTPS origin. Case, a trailing slash,
and explicit default port are mismatches, not aliases. `--url`, when supplied,
is an assertion of that exact configured value and never overrides it.

## Phone Sequence

1. Scan the five-minute QR in the intended browser, open the confirmation page,
   tap **Pair this phone**, and confirm `/fern/` opens without a Basic-auth
   prompt. A scanner preview must not consume the code.
2. From operator-authenticated
   `http://127.0.0.1:8081/fern/control`, confirm the paired device
   appears with the bounded display name entered on the confirmation page. The
   name is self-asserted and is not a Tailscale identity. On the paired phone,
   confirm **Open OpenCode** reaches the unchanged official UI.
3. Create one bounded OpenCode session and submit the prepared low-cost task.
4. Lock the phone or close the browser after OpenCode durably admits the prompt.
5. Reopen from Tailscale and confirm the same session and resulting repository
   change.
6. Exercise one permission or question if the prepared task requests it.
7. Review the OpenCode diff and verification output on the phone.
8. Wait for Fern to pause compute, then reopen OpenCode and record wake latency.
9. From the loopback operator `/fern/control`, track the OpenCode session and
   publish its committed clean change. A paired-device cookie alone cannot
   administer Fern or publish.
10. Verify the draft PR is unique and its head SHA exactly equals the inspected
    local commit.
11. Restart Fern and confirm the same cookie, workflow, and publication status
    remain available.
12. Revoke the phone from the loopback operator `/fern/control`. Confirm the operator receives the
    revocation success page, an active OpenCode stream or terminal closes, and
    the revoked phone receives `401 Unauthorized` on its next request and cannot
    authenticate again. Automated request/SSE/upload/upgrade-shaped coverage
    exists, but this physical transport check remains required.

## Evidence

Capture timestamps, Fern commit/version, image ID, phone/browser versions,
Tailscale hostname, OpenCode session ID, workflow ID, local commit, remote branch
SHA, draft PR URL, pause/wake timestamps, redacted Fern logs, and screenshots of
the permission/diff/result. Record failures rather than repeating ambiguous
external effects manually.

## Cleanup

```bash
tailscale serve reset
go run ./cmd/fern down --config fern.yaml
```

Delete disposable branches and repositories, revoke temporary provider or
GitHub credentials, and retain only redacted evidence. A repository that cannot
be deleted must be made private and archived, then listed as unresolved cleanup.

## Remaining External Gates

Real iOS Safari, Android Chrome, network transitions, Ubuntu systemd boot, an
actual host reboot, provider billing behavior, and organization-specific GitHub
rules cannot be proven by the local harnesses. Fresh-host recovery also still
requires credential reauthorization and split-brain fencing beyond the isolated
same-host restore rehearsal.
