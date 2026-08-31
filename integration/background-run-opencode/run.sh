#!/bin/sh
set -eu

IMAGE_ID="${FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID:-sha256:f493fc1cf2ffb087ef9733eb7f6f14fc0ae0966392fe54ccf695633570c82a82}"
ACTUAL_ID="$(docker image inspect fern/opencode-background-source:dev --format '{{.Id}}')"
test "$ACTUAL_ID" = "$IMAGE_ID"
FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID="$IMAGE_ID" go run ./integration/background-run-opencode
