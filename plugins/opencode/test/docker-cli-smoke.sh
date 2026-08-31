#!/usr/bin/env bash
set -Eeuo pipefail

IMAGE_TAG=${1:?usage: docker-cli-smoke.sh IMAGE}
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
TEMP=$(mktemp -d "${TMPDIR:-/tmp}/fern-opencode-plugin-cli.XXXXXX")
trap 'rm -rf "$TEMP"' EXIT

IMAGE=$(docker image inspect "$IMAGE_TAG" --format '{{.Id}}')
[[ "$IMAGE" =~ ^sha256:[0-9a-f]{64}$ ]]
test "$(docker run --rm --entrypoint opencode "$IMAGE" --version)" = "1.18.16"
npm pack --ignore-scripts --pack-destination "$TEMP" "$ROOT" >/dev/null
ARCHIVE=$(find "$TEMP" -maxdepth 1 -type f -name '*.tgz' -print)
test -n "$ARCHIVE"
test "$(printf '%s\n' "$ARCHIVE" | wc -l | tr -d ' ')" = "1"

mkdir "$TEMP/package" "$TEMP/home" "$TEMP/project"
tar -xzf "$ARCHIVE" --strip-components=1 -C "$TEMP/package"
chmod 0755 "$TEMP" "$TEMP/package"
chmod 0777 "$TEMP/home" "$TEMP/project"
OUTPUT=$(docker run --rm \
  --user 1001:1001 \
  --env HOME=/tmp/home \
  --env XDG_CACHE_HOME=/tmp/home/cache \
  --env XDG_CONFIG_HOME=/tmp/home/config \
  --env XDG_DATA_HOME=/tmp/home/data \
  --env XDG_STATE_HOME=/tmp/home/state \
  --mount "type=bind,src=$TEMP/package,dst=/tmp/package,readonly" \
  --mount "type=bind,src=$TEMP/home,dst=/tmp/home" \
  --mount "type=bind,src=$TEMP/project,dst=/tmp/project" \
  --workdir /tmp/project \
  --entrypoint opencode \
  "$IMAGE" plugin /tmp/package)

grep -q 'Detected tui target' <<<"$OUTPUT"
grep -q 'Installed' <<<"$OUTPUT"
printf 'Packed plugin loaded by OpenCode 1.18.16\n'
