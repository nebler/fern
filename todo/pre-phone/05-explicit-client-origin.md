# Task 05: Add An Explicit Attach Origin

## Commit

```text
feat(attach): accept an explicit client origin
```

## Purpose

Separate where Fern listens from the URL a remote client uses:

```text
listener:      127.0.0.1:8080
client origin: https://host.tailnet.ts.net
```

## Dependencies

Start from task `00`. Independent of all other Wave 1 tasks.

## Owned Files

May modify `cmd/fern/attach.go` and create `cmd/fern/attach_origin_test.go`.

Do not modify config, root example, or shared documentation files. Task `09` decides whether the proven origin belongs in configuration.

## Minimal Contract

Add:

```bash
fern attach -url https://host.tailnet.ts.net
```

- Explicit URL takes precedence over listener-derived URL.
- Accept only absolute HTTP or HTTPS URLs with a host.
- Reject embedded user information, fragments, and unsupported schemes.
- Do not place OpenCode credentials in arguments or URLs.
- Preserve listener-derived behavior when `-url` is absent.
- Continue passing credentials through the child environment.
- Prefer rejecting non-root path prefixes unless verified against OpenCode attach.

## Tests

Cover valid local HTTP, valid tailnet HTTPS, precedence, missing scheme/host, unsupported scheme, userinfo, fragments, path policy, and unchanged credential handling.

## Acceptance

```bash
GOTOOLCHAIN=local go test ./cmd/fern
```

Task `10` must verify the command against the actual Tailscale origin before documentation calls it supported.
