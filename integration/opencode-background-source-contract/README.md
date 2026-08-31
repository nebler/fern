# OpenCode Background Source Contract

This isolated black-box harness qualifies the binary built from exact OpenCode
source commit `39fb919a054190498f6d5b7985bde231f93ad7a6`. It is separate from the
published `opencode-ai@1.18.16` negative qualification and from Fern's
persistent workspace profile.

Run from the repository root:

```sh
python3 integration/opencode-background-source-contract/contract_harness.py
```

The harness builds `fern/opencode-background-source:dev`, resolves its exact
local image ID, and passes that ID rather than the tag to every test container.
It uses a zero-cost local fake provider and never requests shell execution.
Temporary containers, network, repository, and data are removed on success or
failure.

To test a previously built image with an exact local-ID gate:

```sh
FERN_OPENCODE_BACKGROUND_SOURCE_BUILD=0 \
FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE=fern/opencode-background-source:dev \
FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID=sha256:<local-image-id> \
python3 integration/opencode-background-source-contract/contract_harness.py
```

A local image ID is build-local evidence, not a portable registry digest.
Promotion would require publishing an immutable manifest, recording its registry
digest, and rerunning this contract against the pulled image ID.

## Proven Contract

- Image labels and the commit-derived binary version identify the full source
  commit without claiming npm-package equivalence.
- Missing/wrong/correct Basic auth, both health routes, and embedded UI routes
  have exact black-box assertions.
- Caller-selected Session and prompt IDs survive replacement with exact
  agent/model/location and prompt text/delivery projections.
- Finite durable history records one exact admission. Exact retries add no
  history or provider calls, including after replacement; conflicting reuse is
  HTTP `409`.
- Active execution loss leaves durable admission/promotion but no settlement,
  so it is uncertain. Exact interrupt disconnects the local provider stream.
- Synthetic pending permission and fake-provider question state disappear on
  process replacement. The fake model never requests shell execution.

## Remaining Limits

- A hanging provider turn has no durable step-start event before settlement.
- Pending questions cannot be recovered after process replacement.
- HTTP SPA fallback proves the official directory and server-scoped route
  shapes, not browser navigation, private TLS, external origins, SSE, or WSS.
- No coordinator, disposable provider, result boundary, or Fern production/run
  profile consumes this candidate.
