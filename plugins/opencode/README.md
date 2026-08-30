# @fern/opencode

Repo-local OpenCode TUI plugin for Fern Background Runs.

## Status

This is the first development slice, not a published or end-to-end production release. It targets the local OpenCode TUI plugin contract in `@opencode-ai/plugin` `1.18.16` and OpenTUI `0.4.5`. The referenced `specs/tui-plugins.md` is absent from this repository checkout; implementation follows `opencode/packages/plugin/src/tui.ts`.

Implemented:

- `/fern` opens a native action dialog. Slash arguments are intentionally unsupported by this OpenCode API.
- Native `run`, `runs`, `open`, `stop`, `result`, and `disconnect` command-palette actions.
- `runs`, `open`, `stop`, and `result` remain usable when the TUI connects with explicit `--server`.
- `run` is local-service-only. It refuses explicit `--server` because TUI state paths belong to the server while Git subprocesses execute on the local TUI host. A server-side repository identity API is required before remote submission can be safe.
- Fixed-argument Git subprocesses read the canonical remote, exact `HEAD`, branch, and complete dirty state.
- Run creation rejects missing remotes, unborn repositories, ambiguous fetch URLs, dirty worktrees, and repository changes after confirmation.
- Native confirmation precedes create and stop requests. Pending creates retain only a request digest and caller-generated idempotency key so a response-loss retry reuses the same key.
- Create reports success only for a response containing both a valid `run_id` and `committed: true`.
- The Fern endpoint is resolved only from `FERN_ENDPOINT` when the plugin loads and is never persisted or accepted from plugin configuration.
- Credentials are behind `CredentialStore`; this slice supplies only `InMemoryCredentialStore`.
- The plugin refuses to register commands unless the runtime reports exactly OpenCode `1.18.16`.

Not implemented:

- Fern onboarding, pairing, repository authorization checks, and host compatibility/readiness checks.
- OS credential-store persistence. For development only, seed the process-local store with `FERN_TOKEN`. The plugin does not write this value to OpenCode storage, config, logs, or repository files.
- Server-side credential revocation. `disconnect` only forgets the in-memory credential; an inherited `FERN_TOKEN` remains in the parent process environment and can seed a later plugin load.
- The Fern Background Runs backend routes. The concrete client currently expects the spike contract under `/fern/api/runs`.
- A published npm package or compatibility testing against an installed OpenCode binary.

## Development

```sh
cd plugins/opencode
bun install
bun run format:check
bun run typecheck
bun test
bun run smoke
```

For a local development endpoint, use HTTPS except that `http://localhost` and `http://127.0.0.1` are accepted for a fake backend:

```sh
FERN_ENDPOINT=https://fern-host.example FERN_TOKEN=development-token opencode2 /path/to/repository
```

Load this source checkout through the OpenCode CLI plugin configuration using the package directory or a packed tarball. The intended eventual install command is:

```sh
opencode2 plugin add @fern/opencode@<published-version>
```

That command is not currently usable because `@fern/opencode` has not been published and OS-backed onboarding is not wired.

## HTTP Contract

The concrete development client uses bearer authentication and JSON:

- `POST /fern/api/runs` with `Idempotency-Key`; expects `{ "run_id": "...", "committed": true }`.
- `GET /fern/api/runs` and `GET /fern/api/runs/:id`.
- `POST /fern/api/runs/:id/stop` with `Idempotency-Key`.
- `POST /fern/api/runs/:id/open` with `Idempotency-Key`; the same-host capability URL is resolved fresh, passed only to the browser launcher, and never cached or displayed.
- `GET /fern/api/runs/:id/result`.

This contract is a client-side spike until the Fern backend implements and pins these routes.
