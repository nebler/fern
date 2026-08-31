# Background Run Docker Lifecycle

This harness exercises `internal/taskenvdocker` against the already-built
`fern/opencode-background-source:dev` image. It does not build or pull an image,
make a paid model request, run the coordinator, or alter persistent workspace
behavior.

Direct use requires the operator or orchestrator to provide the exact canonical
local image ID:

```sh
FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID=sha256:... \
  integration/background-run-docker/run.sh
```

Normally run `integration/background-run-qualification/run.sh`; it builds the
candidate, captures the ID, and passes it to this harness.

The harness proves clone isolation and quarantine, exact volume/container
labels, runtime security and resource limits, authenticated V2 health,
credential recovery after provider reconstruction, positive stopped-writer
evidence, rejection of a manually restarted process epoch, and separate
complete cleanup of the container, volume, and clone. Cleanup uses independent
bounded contexts plus exact-name fallbacks and fails if any test resource
remains.

Clone byte limits are admission and observation checks, not filesystem quotas.
Docker Desktop bind mounts and `local` volumes do not provide a portable hard
per-attempt byte quota; only the configured Docker log rotation is hard-bounded.
