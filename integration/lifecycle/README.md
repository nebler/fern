# Lifecycle harness

`../../scripts/test-lifecycle.sh` runs the production Fern binary as a host
process and exercises its Docker runtime through the stable HTTP proxy. The
container image in this directory is a deterministic implementation of the
small OpenCode protocol surface Fern consumes. In V1 mode that is
`/global/health`, `/event`, and `/session/status`; in V2 mode it is
`/api/health`, `/api/event`, `/api/session/active`, and the shell, PTY,
permission, and form activity snapshots. The modes are deliberately exclusive
so a wrong protocol path fails. It is not a provider emulator and does not
claim to test model output.

The deterministic scenarios are mandatory. In particular, stopped-workspace
authentication is capability-detected by behavior: missing or invalid
credentials must return `401` without a Docker start event. Older Fern builds
without pre-wake proxy authentication fail that scenario; it is never skipped.
No unstable auth/version/origin command-line flag is assumed.

Useful environment variables:

- `FERN_BIN=/absolute/path/to/fern` uses an existing binary instead of building.
- `FERN_LIFECYCLE_IMAGE=image:tag` uses an existing simulator-compatible image.
- `FERN_LIFECYCLE_ARTIFACTS=/path` selects the evidence directory.
- `FERN_LIFECYCLE_KEEP_RESOURCES=1` retains the isolated HOME, container, volume,
  image, and fixture after success or failure. Fern and Docker event-monitor
  host processes are still terminated.
- `FERN_LIFECYCLE_WAKE_COUNT=10` controls measured wake repetitions; values below
  ten are rejected because ten is part of the harness contract.
- `FERN_LIFECYCLE_PROTOCOL=v1|v2` selects the strict protocol fixture. V1 is the
  default.

Provider-backed scenarios are intentionally reported as not run. They require a
real OpenCode image, credentials, provider availability, and model billing, and
therefore cannot be deterministic or mandatory in this black-box suite. Run
normal OpenCode smoke tests separately when those dependencies are explicitly
available. The report distinguishes this from a mandatory skip.

The pinned real V2 artifact has a separate smoke test:

```bash
make image-v2
FERN_V2_IMAGE=fern/opencode-v2:dev ../../scripts/test-opencode-v2.sh
```

Evidence includes a scenario transcript, redacted Fern and container logs,
Docker events/inspection/stats, host memory context, and wake timing TSV. Secrets
are passed through the isolated process environment and are replaced in captured
output. Successful runs clean only exact, uniquely named resources. Failed runs
retain evidence by default but still clean Docker resources unless keep mode is
explicitly enabled.
