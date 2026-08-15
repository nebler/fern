# Task 07: Add A Real-Docker Lifecycle Harness

## Commit

```text
test: add repeatable Docker lifecycle and timing harness
```

## Purpose

Turn historical manual Docker claims into a repeatable black-box experiment that runs the real binary and preserves raw evidence.

## Dependencies

Author from task `00`. Final acceptance runs after tasks `02`, `03`, `04`, `05`, and `06` merge.

## Owned Files

Create only:

```text
integration/**
scripts/test-lifecycle.sh
```

Do not edit production Go, `Makefile`, CI, shared docs, or example config.

## Harness Contract

- Use unique workspace, container, volume, port and temporary-state names.
- Build or accept an explicit Fern binary and image.
- Never use the developer's normal config or `~/.fern` state.
- Use a temporary repository fixture and deterministic local/fake provider behavior where possible.
- Record commands, timestamps, exit statuses, Docker inspection and Fern logs.
- Redact credentials and clean up only resources created by the harness.
- Retain diagnostics on failure and support explicit keep-resources debugging.
- Fail clearly when Docker is unavailable.

## Required Scenarios

1. Create and become healthy.
2. Authorized request reaches OpenCode.
3. Missing/wrong credentials while stopped return `401` without starting compute.
4. Concurrent authorized requests coalesce into one wake.
5. Busy-to-idle activity leads to stop.
6. A held request prevents stop.
7. Wake handles a changed dynamic backend endpoint.
8. Repository and OpenCode data survive stop/start.
9. Data volume survives destroy/recreate.
10. External clean exit is classified failed.
11. OOM is classified failed when reproducible.
12. SIGTERM shuts Fern down without leaks.
13. Ambiguous pause/stale endpoint follows the corrected recovery path.

Separate mandatory deterministic scenarios from optional provider-backed scenarios. Never silently skip a mandatory scenario.

## Measurements

For at least ten wakes, preserve request time, container-start completion, health-ready time, watcher-connected time when observable, first upstream byte, total duration, container ID, endpoint, and failure classification.

Capture active/stopped `docker stats --no-stream` and host-memory context. Do not infer cost savings.

## Acceptance

```bash
./scripts/test-lifecycle.sh
```

The command returns non-zero on mandatory failure and leaves no test process or Docker resource after successful cleanup.
