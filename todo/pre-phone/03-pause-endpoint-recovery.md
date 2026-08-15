# Task 03: Invalidate Endpoint After Ambiguous Pause

## Commit

```text
fix(workspace): discard endpoint after attempted runtime pause
```

## Purpose

Prevent the manager from trusting a cached endpoint after Docker stop may have taken effect but runtime pause returned an error.

```text
Docker stop succeeds -> intent commit fails -> manager retains endpoint -> avoidable 502
```

## Dependencies

Start from task `00`. Independent of all other Wave 1 tasks.

## Owned Files

May modify `internal/workspace/manager.go` and create `internal/workspace/manager_pause_test.go`.

Do not edit `internal/runtime/docker.go`.

## Required Invariant

Once `Manager.Pause` invokes runtime pause for a running or provisioning observation, the previous cached endpoint must not be returned without runtime reconciliation, whether pause reports success, failure, or ambiguity.

The manager must reopen admission, preserve the original error, avoid clearing a newer endpoint generation, and allow the next request to invoke `EnsureRunning`.

## Tests

1. Successful pause clears the endpoint.
2. Running-state pause error clears the endpoint.
3. Provisioning-state pause error clears the endpoint.
4. The next request performs fresh `EnsureRunning`.
5. Admission reopens after every outcome.
6. A stale invalidation cannot erase a newer generation.

## Acceptance

```bash
GOTOOLCHAIN=local go test ./internal/workspace
GOTOOLCHAIN=local go test -race ./internal/workspace
```
