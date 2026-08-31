#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT"

IMAGE=fern/opencode-background-source:dev
SOURCE_CONTRACT=integration/opencode-background-source-contract/contract_harness.py

printf '==> Build and run source contract\n'
(
  unset FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID
  FERN_OPENCODE_BACKGROUND_SOURCE_BUILD=1 \
    FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE="$IMAGE" \
    python3 "$SOURCE_CONTRACT"
)

IMAGE_ID=$(docker image inspect "$IMAGE" --format '{{.Id}}')
case "$IMAGE_ID" in
  sha256:*) ;;
  *) printf 'error: docker returned non-canonical local image ID: %s\n' "$IMAGE_ID" >&2; exit 1 ;;
esac
HEX=${IMAGE_ID#sha256:}
case "$HEX" in
  *[!0-9a-f]*|'') printf 'error: docker returned non-canonical local image ID: %s\n' "$IMAGE_ID" >&2; exit 1 ;;
esac
if [[ ${#HEX} -ne 64 ]]; then
  printf 'error: docker returned non-canonical local image ID: %s\n' "$IMAGE_ID" >&2
  exit 1
fi
printf '==> Captured exact local image ID %s\n' "$IMAGE_ID"

printf '==> Rerun source contract without building\n'
FERN_OPENCODE_BACKGROUND_SOURCE_BUILD=0 \
  FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE="$IMAGE" \
  FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID="$IMAGE_ID" \
  python3 "$SOURCE_CONTRACT"

printf '==> Run disposable provider lifecycle harness\n'
FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID="$IMAGE_ID" \
  integration/background-run-docker/run.sh

printf '==> Run serial OpenCode and coordinator harness\n'
FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID="$IMAGE_ID" \
  integration/background-run-serial/run.sh

printf 'Background Run qualification passed for local image ID %s\n' "$IMAGE_ID"
