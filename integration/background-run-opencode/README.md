# Background Run OpenCode Client

This real-Docker harness uses `internal/taskenvdocker` to provision the already
built, operator-pinned source-profile image and exercises
`internal/backgroundopencode` against its loopback endpoint. A local provider
container returns one hanging, zero-cost stream and never asks OpenCode to run a
shell or another tool.

Run from the repository root:

```sh
integration/background-run-opencode/run.sh
```

The default local image gate is
`sha256:f493fc1cf2ffb087ef9733eb7f6f14fc0ae0966392fe54ccf695633570c82a82`.
Set `FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID` only when intentionally testing a
new qualified local build.

The harness proves exact session creation/reconciliation, one durable exact
prompt admission, positive active ownership, exact empty `204` interruption,
provider and HTTP-client reconstruction, and no prompt replay. Independent
bounded cleanup removes and then checks the OpenCode container, state volume,
clone, fake-provider container, and temporary root.
