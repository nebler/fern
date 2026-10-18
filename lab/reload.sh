#!/usr/bin/env bash
# Rebuilds the images and restarts the daemon in the running lab.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
cd "$LAB_DIR/.."
lab/build.sh
docker save --platform "linux/$(docker version -f '{{.Server.Arch}}')" fern:dev fern-box:dev \
  | docker exec -i "$LAB" ctr -n k8s.io images import - >/dev/null
k apply -f - < deploy/fern.yaml >/dev/null
k -n fern-system rollout restart ds/fern >/dev/null
k -n fern-system rollout status ds/fern --timeout=300s
