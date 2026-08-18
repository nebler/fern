# Phone Field Demo

This runbook validates the constrained Fern field-demo claim. It does not claim
GitHub App onboarding, notification delivery, durable prompt/cancel semantics,
hard provider budgets, hostile-host isolation, or fresh-host split-brain safety.

## Before Demo Day

1. Freeze product scope; do not add T3, Kubernetes, a new UI, or a new state
   model before this rehearsal.
2. Review and commit the complete hardening diff so the demonstrated source,
   binary, and image have one recorded identity.
3. Use distinct `OPENCODE_PASSWORD` and `FERN_CONTROL_PASSWORD` values, a
   spend-limited provider key, and the intended GitHub account.
4. Prepare one bounded prompt and a disposable or reviewed private repository
   whose expected change and verification command are known in advance.
5. Run the local gates and one complete dry run before using the presentation
   phone or repository.
6. Prepare evidence storage and cleanup commands; never improvise a repeated
   publication after an ambiguous external result.

## Abort Conditions

Do not begin phone testing if any local gate fails, the repository is not a
disposable or reviewed target, Tailscale Funnel is enabled, the provider key is
not spend-limited, the GitHub credential belongs to the wrong account, or the
Fern/OpenCode image identity differs from the recorded build.

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

The lifecycle gate performs a checksum-verified destructive restore of isolated
repository, OpenCode volume, Fern control state, and configuration. It also
simulates an orderly Fern shutdown followed by a Docker stop and verifies
automatic recovery.

The optional real GitHub gate creates and deletes a private disposable
repository. It refuses to run without cleanup scope and explicit confirmation:

```bash
gh auth refresh -h github.com -s delete_repo
FERN_GITHUB_TEST_CONFIRM_MUTATION=1 ./scripts/test-github-publication.sh
```

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
Funnel, host GitHub authentication, and a supported provider credential. It does
not spend provider funds or mutate GitHub.

## Phone Sequence

1. Scan the five-minute QR and confirm `/fern/` opens without a Basic-auth prompt.
2. Confirm the paired device appears by name and **Open OpenCode** reaches the unchanged official UI.
3. Create one bounded OpenCode session and submit the prepared low-cost task.
4. Lock the phone or close the browser after OpenCode durably admits the prompt.
5. Reopen from Tailscale and confirm the same session and resulting repository change.
6. Exercise one permission or question if the prepared task requests it.
7. Review the OpenCode diff and verification output on the phone.
8. Wait for Fern to pause compute, then reopen OpenCode and record wake latency.
9. Track the OpenCode session from `/fern/` and publish its committed clean change.
10. Verify the draft PR is unique and its head SHA exactly equals the inspected local commit.
11. Restart Fern and confirm the same cookie, workflow, and publication status remain available.
12. Revoke the phone device and confirm the browser receives the revocation page and cannot authenticate again.

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
