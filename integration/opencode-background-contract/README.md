# OpenCode Background Contract

This directory is an isolated black-box, real-Docker qualification harness for
the official `opencode-ai@1.18.16` package. It does not exercise or change the
persistent `fern/opencode:dev` image. It uses a local OpenAI-compatible fake
provider with zero token cost and makes no external model request. The fake
model never invokes a shell tool.

Run from the repository root:

```sh
python3 integration/opencode-background-contract/contract_harness.py
```

The default command builds `fern/opencode-background:dev`, resolves its exact
local Docker image ID after the build, prints that ID, and tests only that
resolved image. Every run uses random container/network names, host ports, and
server password; a temporary full Git repository and OpenCode data directory
are bind-mounted and removed with all containers, anonymous volumes, and the
network on success, failure, or interruption.

To consume an existing image and require the ID recorded by CI or an operator:

```sh
FERN_OPENCODE_BACKGROUND_BUILD=0 \
FERN_OPENCODE_BACKGROUND_IMAGE=fern/opencode-background:dev \
FERN_OPENCODE_BACKGROUND_IMAGE_ID=sha256:<locally-inspected-id> \
python3 integration/opencode-background-contract/contract_harness.py
```

Do not copy a developer-machine image ID into source. A local Docker image ID is
captured evidence for that build, not a portable registry identity. Production
admission must resolve the published tag to an immutable registry manifest
digest, compare it with the release-approved digest, pull it, and run this
harness with the resulting exact local ID gate before enabling that digest.

## Proven Contract

The harness proves:

- the binary is exactly `1.18.16`, the process is UID/GID 1001, and no server
  password is baked into image environment metadata;
- missing and wrong Basic credentials return an exact bodyless `401` with the
  Basic challenge, while correct credentials work;
- `GET /global/health` returns exactly
  `{"healthy":true,"version":"1.18.16"}`;
- the embedded official OpenCode web UI is served locally at `/`;
- caller-selected prompt `messageID` is preserved in exact and list history;
- exact and conflicting duplicate message IDs both return `200` and append a
  text part to the existing message, so retries are observably unsafe and no
  conflict is detected;
- finite history plus exact-message reads can reconcile initial prompt
  admission;
- a generated session and its complete history survive container replacement
  with the same data mount;
- `/session/status` is process-local: a busy entry disappears on replacement
  while an incomplete durable assistant record remains, so inactivity cannot
  authorize completion;
- active `/abort` disconnects the fake provider and preserves history; active
  and idle abort calls both return `true`;
- questions can be listed and answered, while an unanswered question disappears
  on process replacement.

## Blocked Or Unproven

- Caller-selected Session ID is blocked: `POST /session` ignores `id` and
  generates a different ID.
- Durable SSE replay is blocked: `/event` ignores `Last-Event-ID` and starts a
  new stream at `server.connected`. Reconciliation must use finite history.
- Permission volatility is unproven. The public surface only lists/replies to
  requests, and this zero-side-effect harness does not ask a model to invoke a
  shell tool merely to manufacture permission state.
- OpenCode completion is not proven or inferred. Only an external explicit seal
  can select a disposable attempt result.
- Private TLS, external-origin redirects, WSS, browser acceptance, artifact
  export, and coordinator lifecycle are outside this harness.
