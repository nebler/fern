# Task 02: Authenticate Before Wake

## Commit

```text
fix(proxy): reject invalid credentials before workspace wake
```

## Purpose

Prevent missing or incorrect credentials from starting compute. Desired ordering:

```text
request -> Fern validates Basic credentials -> manager admission/wake -> OpenCode
```

## Dependencies

Start from task `00`. Independent of all other Wave 1 tasks.

## Owned Files

May modify:

```text
internal/proxy/proxy.go
cmd/fern/up.go
```

May create:

```text
internal/proxy/auth.go
internal/proxy/auth_test.go
```

Do not edit runtime, workspace, config, or shared documentation files.

## Contract

- Preserve loopback unauthenticated behavior when no password is configured.
- Require Basic authentication before calling `Waker` when a password exists.
- Use `opencode` when configured username is empty, matching `runtime.ServerAuth`.
- Return `401` with a Basic challenge for missing or wrong credentials.
- Preserve valid credentials on the forwarded request for OpenCode.
- Never log credentials or consume the request body.
- Use constant-time credential comparison or a standard equivalent.

## Tests

With a counting fake `Waker`, prove missing username/password, wrong username, and wrong password call the waker zero times. Prove valid credentials reach upstream, remain forwarded, and work for event, health, ordinary and WebSocket-shaped requests. Prove no-password behavior remains unchanged and concurrent unauthorized requests produce zero wakes.

## Acceptance

```bash
GOTOOLCHAIN=local go test ./internal/proxy ./cmd/fern
GOTOOLCHAIN=local go test -race ./internal/proxy ./cmd/fern
```

Task `07` must later confirm that unauthorized traffic leaves a real stopped container stopped.

## Out Of Scope

TLS, Tailscale identity, OIDC, API-key issuance, per-user authorization, and credential rotation.
