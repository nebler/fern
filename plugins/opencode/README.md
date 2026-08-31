# @fern/opencode

Repo-local OpenCode TUI plugin for Fern Background Runs.

## Status

This is an unpublished development integration targeting the OpenCode TUI host contract in `@opencode-ai/plugin` `1.18.16` and OpenTUI `0.4.5`. That host compatibility pin is separate from Fern's remote execution profile.

Implemented:

- `/fern` opens a native action dialog. Slash arguments are intentionally unsupported by this OpenCode API.
- Native `run`, `runs`, `open`, `stop`, `result`, and `disconnect` command-palette actions.
- `runs`, `open`, `stop`, and `result` remain usable when the TUI connects with explicit `--server`.
- `run` is local-service-only. It refuses explicit `--server` because TUI state paths belong to the server while Git subprocesses execute on the local TUI host. A server-side repository identity API is required before remote submission can be safe.
- Fixed-argument Git subprocesses read the canonical remote, exact `HEAD`, branch, and complete dirty state.
- Run creation rejects missing remotes, unborn repositories, ambiguous fetch URLs, dirty worktrees, and repository changes after confirmation.
- Run creation confirms and submits Fern's qualified remote execution profile `source-39fb919a054190498f6d5b7985bde231f93ad7a6`; it is not derived from the local OpenCode TUI version.
- Native confirmation precedes create and stop requests. Pending creates retain only a request digest and caller-generated idempotency key so a response-loss retry reuses the same key.
- Create reports success only for a response containing both a valid `run_id` and `committed: true`.
- On the first explicit Fern action, the plugin asks for and validates a root HTTPS Fern origin. That non-secret canonical origin is stored in OpenCode KV storage. `FERN_ENDPOINT` remains an optional development override.
- Device authorization uses Fern's fixed scopes, displays the one-time user code, opens the same-origin verification URL with a sanitized child environment, respects server polling intervals, and stops on denial, expiry, cancellation, or lifecycle shutdown.
- Credentials are stored by canonical Fern origin in macOS Keychain (`security`) or Linux Secret Service (`secret-tool`). Unsupported, unavailable, locked, or failing keyrings refuse durable onboarding rather than writing plaintext. A persistence failure after approval triggers immediate best-effort self-revocation and reports any uncertain cleanup.
- Public authorization start and poll requests never send a bearer. Auth responses, identities, token type, expiry, scopes, URLs, retry intervals, and body sizes are validated before use.
- A 401 removes the invalid local credential only when `WWW-Authenticate` identifies Fern's plugin bearer realm, and never replays the operation. A later explicit Fern action starts onboarding.
- Disconnect requests server-side self-revocation before one local deletion and distinguishes confirmed revocation, an already-ineffective grant, a definitive server failure, and ambiguous transport loss.
- Keyring and browser subprocesses use fixed arguments, sanitized environments, bounded streamed output, lifecycle cancellation, and hard timeouts.
- The plugin refuses to register commands unless the runtime reports exactly OpenCode `1.18.16`.

Not implemented:

- Repository authorization checks and host compatibility/readiness checks.
- Windows durable credential storage.
- Retained result artifacts. Create, list, get, stop, and open routes are implemented; result requests remain `409 Conflict` until artifact retention lands.
- A published npm package. Registry installation cannot succeed until `@fern/opencode` is published.

## Development

```sh
cd plugins/opencode
bun install
bun run format:check
bun run typecheck
bun test
bun run build
bun run pack
bun run smoke
bun run smoke:cli
```

`smoke` installs the packed archive into an isolated package tree and imports its compiled exports. `smoke:cli` additionally requires an installed `opencode` `1.18.16` binary and verifies that its plugin installer detects the extracted packed artifact as a TUI target without contacting a model provider.

For development only, `FERN_ENDPOINT` can override the persisted origin and `FERN_TOKEN` can seed a process-local `InMemoryCredentialStore`. The token remains inherited by the OpenCode parent process; the plugin never writes it to KV storage, configuration, logs, repository files, process arguments, or child environments. Localhost HTTP is accepted only through this development path:

```sh
FERN_ENDPOINT=https://fern-host.example FERN_TOKEN=development-token opencode2 /path/to/repository
```

The pinned OpenCode CLI installs a published version with:

```sh
opencode2 plugin @fern/opencode@<version>
```

That command is not currently usable because `@fern/opencode` has not been published.

## HTTP Contract

The client uses JSON and a Fern plugin bearer after device authorization:

- `POST /fern/api/plugin-auth/start` and `POST /fern/api/plugin-auth/poll` are public and never receive the bearer.
- `POST /fern/api/plugin-auth/self/revoke` revokes the calling credential.

- `POST /fern/api/runs` with `Idempotency-Key` and profile `source-39fb919a054190498f6d5b7985bde231f93ad7a6`; expects `{ "run_id": "...", "committed": true }`.
- `GET /fern/api/runs` and `GET /fern/api/runs/:id`.
- `POST /fern/api/runs/:id/stop` with `Idempotency-Key`.
- `POST /fern/api/runs/:id/open` with `Idempotency-Key`; the same-host capability URL is resolved fresh, passed only to the browser launcher, and never cached or displayed.
- `GET /fern/api/runs/:id/result`.

The authentication routes and create, list, get, stop, and open run routes are implemented by Fern. The result route returns `409 Conflict` until retained result artifacts are available.
