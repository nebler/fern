#!/bin/sh
set -eu

IMAGE_ID="${FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID:-}"
if [ -z "$IMAGE_ID" ]; then
  printf '%s\n' 'error: FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID is required; run integration/background-run-qualification/run.sh or export the exact ID from docker image inspect fern/opencode-background-source:dev --format {{.Id}}' >&2
  exit 2
fi
case "$IMAGE_ID" in
  sha256:*) ;;
  *) printf '%s\n' 'error: FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID must be canonical sha256:<64 lowercase hex>' >&2; exit 2 ;;
esac
HEX=${IMAGE_ID#sha256:}
case "$HEX" in
  *[!0-9a-f]*|'') printf '%s\n' 'error: FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID must be canonical sha256:<64 lowercase hex>' >&2; exit 2 ;;
esac
if [ "${#HEX}" -ne 64 ]; then
  printf '%s\n' 'error: FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID must be canonical sha256:<64 lowercase hex>' >&2
  exit 2
fi
ACTUAL_ID="$(docker image inspect fern/opencode-background-source:dev --format '{{.Id}}')"
if [ "$ACTUAL_ID" != "$IMAGE_ID" ]; then
  printf 'error: operator-pinned image ID %s does not match local tag ID %s\n' "$IMAGE_ID" "$ACTUAL_ID" >&2
  exit 1
fi
FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID="$IMAGE_ID" go run ./integration/background-run-opencode
