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

Direct use requires `FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID` to be the exact
canonical local ID of `fern/opencode-background-source:dev`. Normally run
`integration/background-run-qualification/run.sh`; it builds the candidate,
captures the ID, and passes it to this harness.

The harness proves exactly one session-creation POST despite response loss,
read-only exact session reconciliation after provider/client reconstruction,
one durable exact prompt admission despite independent response loss, positive
active ownership, exact empty `204` interruption, and no session or prompt
replay. Independent bounded cleanup removes and then checks the OpenCode
container, state volume, clone, fake-provider container, and temporary root.
