# Background Run Docker Lifecycle

This harness exercises `internal/taskenvdocker` against the already-built
`fern/opencode-background-source:dev` image. It does not build or pull an image,
make a paid model request, run the coordinator, or alter persistent workspace
behavior.

The default operator pin is
`sha256:f493fc1cf2ffb087ef9733eb7f6f14fc0ae0966392fe54ccf695633570c82a82`.
Override it only when intentionally qualifying another local build:

```sh
FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID=sha256:... \
  integration/background-run-docker/run.sh
```

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
