# Lifecycle harness

`../../scripts/test-lifecycle.sh` runs the production Fern binary as a host
process and exercises its Docker runtime through the stable HTTP proxy. The
container image in this directory is a deterministic implementation of the
small OpenCode V2 protocol surface Fern consumes: `/api/health`, `/api/event`,
`/api/session/active`, and the shell, PTY, permission, and form activity
snapshots. It is not a provider emulator and does not claim to test model
output.

The deterministic scenarios are mandatory. In particular, stopped-workspace
authentication is capability-detected by behavior: missing or invalid
credentials must return `401` without a Docker start event. Older Fern builds
without pre-wake proxy authentication fail that scenario; it is never skipped.
No unstable auth/version/origin command-line flag is assumed.

The phone lifecycle regression is also fully isolated. The simulator emits an
authoritative busy-to-idle transition, then a paired-device browser request is
made partway through the idle grace period. The harness requires compute to
remain running past the original deadline, pause after the complete restarted
grace period, and wake with the same container, repository, and persisted
session marker.

Startup rollback scenarios distinguish a committed user/shutdown pause intent
from a committed failed-start intent. A container Fern started but could not
make healthy remains classified `failed`; it cannot later appear intentionally
dormant merely because rollback stopped it.

The fixture configures a synthetic canonical HTTPS `remoteOrigin` while requests
still travel over local HTTP. A paired request supplies a malicious `Host`,
`Forwarded`, and `X-Forwarded-*` set; the fake backend requires Fern's configured
remote host/HTTPS/default port and checks absolute `Location`/`Link` generation.
The paired operator assertion requires its canonical loopback HTTP tuple. This
proves proxy metadata policy, not real TLS, WSS, or pinned-image behavior.

Useful environment variables:

- `FERN_BIN=/absolute/path/to/fern` uses an existing binary instead of building.
- `FERN_LIFECYCLE_IMAGE=image:tag` uses an existing simulator-compatible image.
- `FERN_LIFECYCLE_ARTIFACTS=/path` selects the evidence directory.
- `FERN_LIFECYCLE_KEEP_RESOURCES=1` retains the isolated HOME, container, volume,
  image, and fixture after success or failure. Fern and Docker event-monitor
  host processes are still terminated.
- `FERN_LIFECYCLE_WAKE_COUNT=10` controls measured wake repetitions; values below
  ten are rejected because ten is part of the harness contract.

Provider-backed scenarios are intentionally reported as not run. They require a
real OpenCode image, credentials, provider availability, and model billing, and
therefore cannot be deterministic or mandatory in this black-box suite. Run
normal OpenCode smoke tests separately when those dependencies are explicitly
available. The report distinguishes this from a mandatory skip.

The pinned real OpenCode artifact has a separate smoke test:

```bash
make image
FERN_OPENCODE_IMAGE=fern/opencode:dev ../../scripts/test-opencode.sh
```

Evidence includes a scenario transcript, redacted Fern and container logs,
Docker events/inspection/stats, host memory context, and wake timing TSV. Secrets
are passed through the isolated process environment and are replaced in captured
output. Successful runs clean only exact, uniquely named resources. Failed runs
retain evidence by default but still clean Docker resources unless keep mode is
explicitly enabled.

This local Docker harness is not evidence of a physical host reboot, real
private-edge TLS/WSS, replacement-host restore, or phone rehearsal.
