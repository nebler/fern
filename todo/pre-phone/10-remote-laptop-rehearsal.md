# Task 10: Rehearse Remotely From A Laptop

## Commit

```text
docs: record pre-phone remote rehearsal
```

## Purpose

Remove deployment, Tailscale, authentication, URL and lifecycle variables before introducing phone-specific behavior.

## Dependencies

Task `09` complete, CI green, harness passing, and supervised service installed.

## Owned Files

Create only:

```text
evidence/pre-phone/laptop-rehearsal.md
evidence/pre-phone/results/**
```

Commit only sanitized, bounded evidence. Exclude credentials, provider payloads, sensitive tailnet identifiers and unbounded logs.

## Procedure

1. Confirm systemd is active after reboot.
2. Confirm Fern listens only on the intended interface.
3. Confirm the private Tailscale origin is reachable from another network.
4. Let the workspace stop intentionally.
5. Prove missing and wrong credentials do not wake it.
6. Open authenticated OpenCode web and record wake stages.
7. Run `fern attach -url <private-origin>` and resume the same session.
8. Submit deterministic work and observe busy-to-idle behavior.
9. Disconnect during controlled long work and record actual behavior.
10. Reconnect and inspect persisted state.
11. Let idle stop and record resource state.
12. Repeat authenticated wake at least ten times.
13. Back up data, recreate the container around the volume, and verify state.
14. Record every manual recovery step.

## Evidence

Record Fern version/commit, image identity, host context, reboot outcome, listener/origin model, unauthorized results, ten raw wake samples, median/range/failures, memory observations, persistence, disconnect behavior, idle timing, and comparison with plain authenticated OpenCode over the same Tailscale path.

## Acceptance

The evidence commit contains a completed result for every procedure step, preserves raw timestamps for at least ten wakes, contains no secrets, and clearly marks every failure or unrun experiment.

## Phone Entry Gate

- Unauthorized requests never wake compute.
- All authorized wakes succeed or every failure is understood and accepted.
- Origin and credentials need no laptop-specific workaround.
- Reboot recovery and session persistence work.
- Idle stop is correctly classified.
- Recovery needs no undocumented Docker commands.
- The same timestamp procedure is ready for cellular use.

If this rehearsal fails, fix that concrete failure before phone testing. Do not classify generic remote deployment failures as mobile failures.
