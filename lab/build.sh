#!/usr/bin/env bash
# Builds the lab images: fern:dev (daemon) and fern-box:dev (box).
set -euo pipefail
cd "$(dirname "$0")/.."
for image in fern fern-box; do
  docker build -q --label app.kubernetes.io/part-of=fern \
    -f "images/$image.Dockerfile" -t "$image:dev" . >/dev/null
  echo "built $image:dev"
done
