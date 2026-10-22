#!/usr/bin/env bash
# Builds fern-t3:dev, T3 Code's server with Fern's checkpoint hook, from T3's
# source on GitHub at the commit pinned in images/t3.Dockerfile, and imports it
# into the lab if it is up. Needs the lab images (lab/build.sh).
set -euo pipefail
source "$(dirname "$0")/lib.sh"
cd "$LAB_DIR/.."
docker build -q --label "$LABEL" \
  --build-context fern=docker-image://fern:dev \
  --build-context fern-box=docker-image://fern-box:dev \
  -f images/t3.Dockerfile -t fern-t3:dev . >/dev/null
echo "built fern-t3:dev"
if docker inspect "$LAB" >/dev/null 2>&1; then
  docker save --platform "linux/$(docker version -f '{{.Server.Arch}}')" fern-t3:dev \
    | docker exec -i "$LAB" ctr -n k8s.io images import - >/dev/null
  echo "imported fern-t3:dev into $LAB"
  # The tag stays the same, so a running server only picks the image up when restarted.
  if k -n fern-system get deploy/t3 >/dev/null 2>&1; then
    k -n fern-system rollout restart deploy/t3 >/dev/null
    k -n fern-system rollout status deploy/t3 --timeout=180s >/dev/null
    echo "restarted T3's server"
  fi
fi
